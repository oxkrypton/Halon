package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oxkrypton/Halon/internal/incident"
	"github.com/redis/go-redis/v9"
)

const (
	incidentWinKeyPrefix     = "halon:incident:win:"
	incidentAggKeyPrefix     = "halon:incident:agg:"
	incidentActiveIndexKey   = "halon:incident:active"
	incidentActiveIndexLimit = 200
	maxCoalesceSamples       = 5
	aggKeyTTLBufferMS        = 60000
	memberSep                = "|"
)

// WindowStatus represents the lifecycle phase of a coalescing window.
type WindowStatus string

const (
	WindowPending WindowStatus = "pending"
	WindowActive  WindowStatus = "active"
)

// IncidentPolicy configures how one EventType is detected, settled, and cooled
// down across gateway replicas.
type IncidentPolicy struct {
	Enabled        bool
	MinOccurrences int
	DetectWindow   time.Duration
	SettleDelay    time.Duration
	Cooldown       time.Duration
	RetryBackoff   time.Duration
}

// IncidentPolicySet maps incident event types to policies, with a Default fallback
// so newly introduced event types work automatically.
type IncidentPolicySet struct {
	Default  IncidentPolicy
	Policies map[incident.EventType]IncidentPolicy
}

// PolicyFor returns the configured policy for eventType, falling back to Default.
func (s IncidentPolicySet) PolicyFor(eventType incident.EventType) IncidentPolicy {
	if s.Policies != nil {
		if policy, ok := s.Policies[eventType]; ok {
			return policy
		}
	}
	if s.Default.MinOccurrences > 0 || s.Default.Cooldown > 0 || s.Default.SettleDelay > 0 || s.Default.DetectWindow > 0 || s.Default.Enabled {
		return s.Default
	}
	return IncidentPolicy{
		Enabled:        true,
		MinOccurrences: 1,
		SettleDelay:    15 * time.Second,
		Cooldown:       5 * time.Minute,
		RetryBackoff:   time.Minute,
	}
}

// Enabled reports whether eventType is configured to emit incidents.
func (s IncidentPolicySet) Enabled(eventType incident.EventType) bool {
	return s.PolicyFor(eventType).Enabled
}

// CoalesceRecordResult describes the atomic outcome of recording one raw anomaly.
type CoalesceRecordResult struct {
	Triggered    bool
	OwnerEventID string
	Occurrences  int
	Status       WindowStatus
}

// IncidentWindowSnapshot summarizes one coalescing or cooldown window.
type IncidentWindowSnapshot struct {
	EventType       incident.EventType `json:"event_type"`
	ScopeKey        string             `json:"scope_key"`
	Route           string             `json:"route,omitempty"`
	EventID         string             `json:"event_id"`
	Status          WindowStatus       `json:"status"`
	Occurrences     int                `json:"occurrences"`
	FirstAt         time.Time          `json:"first_at"`
	LastAt          time.Time          `json:"last_at"`
	TTLMS           int64              `json:"ttl_ms"`
	SamplePaths     []string           `json:"sample_paths,omitempty"`
	SampleClientIPs []string           `json:"sample_client_ips,omitempty"`
}

// IncidentCoalescer arbitrates cross-replica incident deduplication, threshold
// burst detection, settle aggregation, and cooldown windows across gateways.
type IncidentCoalescer interface {
	Record(ctx context.Context, event incident.Event, policy IncidentPolicy) CoalesceRecordResult
	Snapshot(ctx context.Context, eventType incident.EventType, scopeKey, eventID string) (IncidentWindowSnapshot, bool)
	UpdateCooldown(ctx context.Context, eventType incident.EventType, scopeKey, ownerEventID string, ttl time.Duration)
	ListWindows(ctx context.Context, filterType incident.EventType, filterScope string) ([]IncidentWindowSnapshot, error)
}

var recordIncidentScript = redis.NewScript(`
local winKey = KEYS[1]
local activeIndexKey = KEYS[2]
local owner = ARGV[1]
local eventType = ARGV[2]
local scopeKey = ARGV[3]
local route = ARGV[4]
local eventMs = tonumber(ARGV[5])
local path = ARGV[6]
local ip = ARGV[7]
local aggPrefix = ARGV[8]
local maxSamples = tonumber(ARGV[9])
local aggBuffer = tonumber(ARGV[10])
local minOcc = tonumber(ARGV[11])
local detectMs = tonumber(ARGV[12])
local activeMs = tonumber(ARGV[13])
local member = ARGV[14]

local t = redis.call('TIME')
local nowMs = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)

local function appendSample(aggKey, field, val)
  if val == '' then
    return
  end
  local cur = redis.call('HGET', aggKey, field)
  if not cur or cur == '' then
    redis.call('HSET', aggKey, field, val)
    return
  end
  local count = 1
  for item in string.gmatch(cur, '[^\n]+') do
    if item == val then
      return
    end
    count = count + 1
  end
  if count <= maxSamples then
    redis.call('HSET', aggKey, field, cur .. '\n' .. val)
  end
end

local function initAgg(aggKey, status, ttlMs)
  redis.call('HSET', aggKey,
    'event_type', eventType,
    'scope_key', scopeKey,
    'route', route,
    'status', status,
    'occurrences', 1,
    'first_at_ms', eventMs,
    'last_at_ms', eventMs,
    'paths', path,
    'ips', ip
  )
  redis.call('PEXPIRE', aggKey, ttlMs + aggBuffer)
end

local cur = redis.call('GET', winKey)
if not cur or cur == '' then
  local aggKey = aggPrefix .. owner
  if minOcc <= 1 then
    redis.call('SET', winKey, 'active:' .. owner, 'PX', activeMs)
    initAgg(aggKey, 'active', activeMs)
    redis.call('ZADD', activeIndexKey, nowMs + activeMs, member)
    redis.call('ZREMRANGEBYSCORE', activeIndexKey, '-inf', nowMs)
    return {1, owner, 1, 'active'}
  end
  redis.call('SET', winKey, 'pending:' .. owner, 'PX', detectMs)
  initAgg(aggKey, 'pending', detectMs)
  return {0, owner, 1, 'pending'}
end

local existingOwner = ''
local status = 'active'
if string.sub(cur, 1, 8) == 'pending:' then
  status = 'pending'
  existingOwner = string.sub(cur, 9)
elseif string.sub(cur, 1, 7) == 'active:' then
  status = 'active'
  existingOwner = string.sub(cur, 8)
else
  existingOwner = cur
end

if existingOwner == '' then
  return {0, '', 0, status}
end

local aggKey = aggPrefix .. existingOwner
local count = 1
if redis.call('EXISTS', aggKey) == 0 then
  redis.call('HSET', aggKey,
    'event_type', eventType,
    'scope_key', scopeKey,
    'route', route,
    'status', status,
    'occurrences', 1,
    'first_at_ms', eventMs,
    'last_at_ms', eventMs,
    'paths', path,
    'ips', ip
  )
else
  count = redis.call('HINCRBY', aggKey, 'occurrences', 1)
  redis.call('HSET', aggKey, 'last_at_ms', eventMs)
  appendSample(aggKey, 'paths', path)
  appendSample(aggKey, 'ips', ip)
end

if status == 'pending' and count >= minOcc then
  redis.call('SET', winKey, 'active:' .. existingOwner, 'PX', activeMs)
  redis.call('HSET', aggKey, 'status', 'active')
  redis.call('PEXPIRE', aggKey, activeMs + aggBuffer)
  redis.call('ZADD', activeIndexKey, nowMs + activeMs, member)
  redis.call('ZREMRANGEBYSCORE', activeIndexKey, '-inf', nowMs)
  return {1, existingOwner, count, 'active'}
end

local winTTL = redis.call('PTTL', winKey)
if winTTL and winTTL > 0 then
  redis.call('PEXPIRE', aggKey, winTTL + aggBuffer)
end
return {0, existingOwner, count, status}
`)

var setIncidentTTLIfOwnerScript = redis.NewScript(`
local winKey = KEYS[1]
local aggKey = KEYS[2]
local activeIndexKey = KEYS[3]
local owner = ARGV[1]
local ttlMs = tonumber(ARGV[2])
local aggBuffer = tonumber(ARGV[3])
local member = ARGV[4]

if redis.call('GET', winKey) ~= ('active:' .. owner) then
  return 0
end
if ttlMs <= 0 then
  redis.call('DEL', winKey, aggKey)
  redis.call('ZREM', activeIndexKey, member)
  return 1
end
local t = redis.call('TIME')
local nowMs = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
redis.call('PEXPIRE', winKey, ttlMs)
redis.call('PEXPIRE', aggKey, ttlMs + aggBuffer)
redis.call('ZADD', activeIndexKey, nowMs + ttlMs, member)
redis.call('ZREMRANGEBYSCORE', activeIndexKey, '-inf', nowMs)
return 1
`)

// RedisIncidentCoalescer backs IncidentCoalescer in Redis and falls back to an
// in-process window table when Redis is unreachable so anomaly storms remain
// throttled during transient Redis outages.
type RedisIncidentCoalescer struct {
	client    *redis.Client
	opTimeout time.Duration
	logger    *slog.Logger
	onHealth  func(bool)
	fallback  *memoryIncidentCoalescer
}

var _ IncidentCoalescer = (*RedisIncidentCoalescer)(nil)

func NewRedisIncidentCoalescer(client *redis.Client, opTimeout time.Duration, logger *slog.Logger, onHealth func(bool)) *RedisIncidentCoalescer {
	if opTimeout <= 0 {
		opTimeout = 500 * time.Millisecond
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &RedisIncidentCoalescer{
		client:    client,
		opTimeout: opTimeout,
		logger:    logger,
		onHealth:  onHealth,
		fallback:  newMemoryIncidentCoalescer(),
	}
}

func incidentWinKey(eventType incident.EventType, scopeKey string) string {
	return incidentWinKeyPrefix + string(eventType) + ":" + scopeKey
}

func incidentIndexMember(eventType incident.EventType, scopeKey string) string {
	return string(eventType) + memberSep + scopeKey
}

func parseIncidentIndexMember(member string) (incident.EventType, string, bool) {
	rawType, scopeKey, ok := strings.Cut(member, memberSep)
	if !ok || rawType == "" || scopeKey == "" {
		return "", "", false
	}
	return incident.EventType(rawType), scopeKey, true
}

func normalizeWindowDurations(policy IncidentPolicy) (minOcc int, detectWindow, activeWindow time.Duration) {
	minOcc = max(1, policy.MinOccurrences)
	detectWindow = policy.DetectWindow
	if minOcc > 1 && detectWindow <= 0 {
		detectWindow = 30 * time.Second
	}
	activeWindow = max(time.Duration(0), policy.SettleDelay) + max(time.Duration(0), policy.Cooldown)
	if activeWindow <= 0 {
		activeWindow = time.Millisecond
	}
	return minOcc, detectWindow, activeWindow
}

func (c *RedisIncidentCoalescer) Record(ctx context.Context, event incident.Event, policy IncidentPolicy) CoalesceRecordResult {
	scopeKey := event.EffectiveScopeKey()
	if c.client == nil || scopeKey == "" || event.Type == "" {
		return CoalesceRecordResult{Triggered: true, OwnerEventID: event.EventID, Occurrences: max(1, event.Occurrences), Status: WindowActive}
	}
	minOcc, detectWindow, activeWindow := normalizeWindowDurations(policy)
	if minOcc <= 1 && policy.Cooldown <= 0 && policy.SettleDelay <= 0 {
		return CoalesceRecordResult{Triggered: true, OwnerEventID: event.EventID, Occurrences: 1, Status: WindowActive}
	}

	opCtx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()

	eventMs := event.OccurredAt.UnixMilli()
	if eventMs <= 0 {
		eventMs = time.Now().UTC().UnixMilli()
	}
	raw, err := recordIncidentScript.Run(
		opCtx,
		c.client,
		[]string{incidentWinKey(event.Type, scopeKey), incidentActiveIndexKey},
		event.EventID,
		string(event.Type),
		scopeKey,
		event.Route,
		eventMs,
		sanitizeSample(event.Path),
		sanitizeSample(event.ClientIP),
		incidentAggKeyPrefix,
		maxCoalesceSamples,
		aggKeyTTLBufferMS,
		minOcc,
		ttlMillis(detectWindow),
		ttlMillis(activeWindow),
		incidentIndexMember(event.Type, scopeKey),
	).Slice()
	if err != nil {
		c.reportHealth(false)
		c.logger.Warn("redis incident coalescer record failed; falling back to local window",
			"event_type", event.Type, "scope_key", scopeKey, "error", err)
		return c.fallback.Record(event, policy)
	}
	parsed, parseErr := parseCoalesceRecordResult(raw)
	if parseErr != nil {
		c.logger.Warn("unexpected redis incident coalescer response; falling back to local window",
			"event_type", event.Type, "scope_key", scopeKey, "error", parseErr)
		return c.fallback.Record(event, policy)
	}
	c.reportHealth(true)
	return parsed
}

func (c *RedisIncidentCoalescer) Snapshot(ctx context.Context, eventType incident.EventType, scopeKey, eventID string) (IncidentWindowSnapshot, bool) {
	if snap, ok := c.fallback.Snapshot(eventType, scopeKey, eventID); ok {
		return snap, true
	}
	if c.client == nil || eventID == "" || scopeKey == "" || eventType == "" {
		return IncidentWindowSnapshot{}, false
	}
	opCtx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()

	pipe := c.client.Pipeline()
	aggCmd := pipe.HGetAll(opCtx, incidentAggKeyPrefix+eventID)
	ttlCmd := pipe.PTTL(opCtx, incidentWinKey(eventType, scopeKey))
	if _, err := pipe.Exec(opCtx); err != nil {
		c.reportHealth(false)
		return IncidentWindowSnapshot{}, false
	}
	c.reportHealth(true)
	fields, err := aggCmd.Result()
	if err != nil || len(fields) == 0 {
		return IncidentWindowSnapshot{}, false
	}
	ttlMS := int64(0)
	if ttl, ttlErr := ttlCmd.Result(); ttlErr == nil && ttl > 0 {
		ttlMS = ttl.Milliseconds()
	}
	return snapshotFromFields(eventType, scopeKey, eventID, fields, ttlMS), true
}

func (c *RedisIncidentCoalescer) UpdateCooldown(ctx context.Context, eventType incident.EventType, scopeKey, ownerEventID string, ttl time.Duration) {
	c.fallback.UpdateCooldown(eventType, scopeKey, ownerEventID, ttl)
	if c.client == nil || ownerEventID == "" || scopeKey == "" || eventType == "" {
		return
	}
	opCtx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()

	var ttlMs int64
	if ttl > 0 {
		ttlMs = ttlMillis(ttl)
	}
	if err := setIncidentTTLIfOwnerScript.Run(
		opCtx,
		c.client,
		[]string{incidentWinKey(eventType, scopeKey), incidentAggKeyPrefix + ownerEventID, incidentActiveIndexKey},
		ownerEventID,
		ttlMs,
		aggKeyTTLBufferMS,
		incidentIndexMember(eventType, scopeKey),
	).Err(); err != nil {
		c.reportHealth(false)
		c.logger.Warn("redis incident coalescer ttl update failed",
			"event_type", eventType, "scope_key", scopeKey, "event_id", ownerEventID, "error", err)
		return
	}
	c.reportHealth(true)
}

func (c *RedisIncidentCoalescer) ListWindows(ctx context.Context, filterType incident.EventType, filterScope string) ([]IncidentWindowSnapshot, error) {
	filterType = incident.EventType(strings.TrimSpace(string(filterType)))
	filterScope = strings.TrimSpace(filterScope)

	if c.client == nil {
		return c.fallback.ListWindows(filterType, filterScope), nil
	}
	opCtx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()

	// Exact point lookup when both dimensions are provided (surfaces pending windows too).
	if filterType != "" && filterScope != "" {
		winKey := incidentWinKey(filterType, filterScope)
		pipe := c.client.Pipeline()
		valCmd := pipe.Get(opCtx, winKey)
		ttlCmd := pipe.PTTL(opCtx, winKey)
		if _, err := pipe.Exec(opCtx); err != nil && err != redis.Nil {
			c.reportHealth(false)
			return c.fallback.ListWindows(filterType, filterScope), err
		}
		c.reportHealth(true)
		rawVal, err := valCmd.Result()
		if err != nil || rawVal == "" {
			return []IncidentWindowSnapshot{}, nil
		}
		status, owner := parseWindowValue(rawVal)
		ttlMS := int64(0)
		if ttl, ttlErr := ttlCmd.Result(); ttlErr == nil && ttl > 0 {
			ttlMS = ttl.Milliseconds()
		}
		fields, hErr := c.client.HGetAll(opCtx, incidentAggKeyPrefix+owner).Result()
		if hErr != nil || len(fields) == 0 {
			return []IncidentWindowSnapshot{{
				EventType:   filterType,
				ScopeKey:    filterScope,
				EventID:     owner,
				Status:      status,
				Occurrences: 1,
				TTLMS:       ttlMS,
			}}, nil
		}
		snap := snapshotFromFields(filterType, filterScope, owner, fields, ttlMS)
		snap.Status = status
		return []IncidentWindowSnapshot{snap}, nil
	}

	nowMs := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	pruneAndRead := c.client.Pipeline()
	pruneAndRead.ZRemRangeByScore(opCtx, incidentActiveIndexKey, "-inf", nowMs)
	membersCmd := pruneAndRead.ZRange(opCtx, incidentActiveIndexKey, 0, incidentActiveIndexLimit-1)
	if _, err := pruneAndRead.Exec(opCtx); err != nil && err != redis.Nil {
		c.reportHealth(false)
		return c.fallback.ListWindows(filterType, filterScope), err
	}
	c.reportHealth(true)

	members, err := membersCmd.Result()
	if err != nil || len(members) == 0 {
		return []IncidentWindowSnapshot{}, nil
	}

	type target struct {
		eventType incident.EventType
		scopeKey  string
		member    string
		valCmd    *redis.StringCmd
		ttlCmd    *redis.DurationCmd
	}
	targets := make([]target, 0, len(members))
	pipe := c.client.Pipeline()
	for _, member := range members {
		et, scope, ok := parseIncidentIndexMember(member)
		if !ok {
			continue
		}
		if filterType != "" && et != filterType {
			continue
		}
		if filterScope != "" && scope != filterScope {
			continue
		}
		key := incidentWinKey(et, scope)
		targets = append(targets, target{
			eventType: et,
			scopeKey:  scope,
			member:    member,
			valCmd:    pipe.Get(opCtx, key),
			ttlCmd:    pipe.PTTL(opCtx, key),
		})
	}
	if len(targets) == 0 {
		return []IncidentWindowSnapshot{}, nil
	}
	if _, err := pipe.Exec(opCtx); err != nil && err != redis.Nil {
		c.reportHealth(false)
		return c.fallback.ListWindows(filterType, filterScope), err
	}

	type activeEntry struct {
		eventType incident.EventType
		scopeKey  string
		eventID   string
		status    WindowStatus
		ttlMS     int64
	}
	actives := make([]activeEntry, 0, len(targets))
	staleMembers := make([]any, 0)
	aggPipe := c.client.Pipeline()
	aggCmds := make([]*redis.MapStringStringCmd, 0, len(targets))
	for _, tgt := range targets {
		rawVal, err := tgt.valCmd.Result()
		if err != nil || rawVal == "" {
			staleMembers = append(staleMembers, tgt.member)
			continue
		}
		status, owner := parseWindowValue(rawVal)
		ttlMS := int64(0)
		if ttl, ttlErr := tgt.ttlCmd.Result(); ttlErr == nil && ttl > 0 {
			ttlMS = ttl.Milliseconds()
		}
		actives = append(actives, activeEntry{
			eventType: tgt.eventType,
			scopeKey:  tgt.scopeKey,
			eventID:   owner,
			status:    status,
			ttlMS:     ttlMS,
		})
		aggCmds = append(aggCmds, aggPipe.HGetAll(opCtx, incidentAggKeyPrefix+owner))
	}
	if len(staleMembers) > 0 {
		_ = c.client.ZRem(opCtx, incidentActiveIndexKey, staleMembers...).Err()
	}
	if len(actives) == 0 {
		return []IncidentWindowSnapshot{}, nil
	}
	if _, err := aggPipe.Exec(opCtx); err != nil && err != redis.Nil {
		c.reportHealth(false)
		return c.fallback.ListWindows(filterType, filterScope), err
	}

	windows := make([]IncidentWindowSnapshot, 0, len(actives))
	for i, active := range actives {
		fields, err := aggCmds[i].Result()
		if err != nil || len(fields) == 0 {
			windows = append(windows, IncidentWindowSnapshot{
				EventType:   active.eventType,
				ScopeKey:    active.scopeKey,
				EventID:     active.eventID,
				Status:      active.status,
				Occurrences: 1,
				TTLMS:       active.ttlMS,
			})
			continue
		}
		snap := snapshotFromFields(active.eventType, active.scopeKey, active.eventID, fields, active.ttlMS)
		snap.Status = active.status
		windows = append(windows, snap)
	}
	sortWindowSnapshots(windows)
	return windows, nil
}

func (c *RedisIncidentCoalescer) reportHealth(up bool) {
	if c.onHealth != nil {
		c.onHealth(up)
	}
}

func parseWindowValue(raw string) (WindowStatus, string) {
	if strings.HasPrefix(raw, "pending:") {
		return WindowPending, strings.TrimPrefix(raw, "pending:")
	}
	if strings.HasPrefix(raw, "active:") {
		return WindowActive, strings.TrimPrefix(raw, "active:")
	}
	return WindowActive, raw
}

func snapshotFromFields(eventType incident.EventType, scopeKey, eventID string, fields map[string]string, ttlMS int64) IncidentWindowSnapshot {
	occurrences, _ := strconv.Atoi(fields["occurrences"])
	if occurrences <= 0 {
		occurrences = 1
	}
	firstMS, _ := strconv.ParseInt(fields["first_at_ms"], 10, 64)
	lastMS, _ := strconv.ParseInt(fields["last_at_ms"], 10, 64)
	var firstAt, lastAt time.Time
	if firstMS > 0 {
		firstAt = time.UnixMilli(firstMS).UTC()
	}
	if lastMS > 0 {
		lastAt = time.UnixMilli(lastMS).UTC()
	}
	status := WindowStatus(fields["status"])
	if status != WindowPending {
		status = WindowActive
	}
	return IncidentWindowSnapshot{
		EventType:       eventType,
		ScopeKey:        scopeKey,
		Route:           fields["route"],
		EventID:         eventID,
		Status:          status,
		Occurrences:     occurrences,
		FirstAt:         firstAt,
		LastAt:          lastAt,
		TTLMS:           ttlMS,
		SamplePaths:     splitNonEmptyLines(fields["paths"]),
		SampleClientIPs: splitNonEmptyLines(fields["ips"]),
	}
}

func splitNonEmptyLines(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, "\n")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func sanitizeSample(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\n", "")
	value = strings.ReplaceAll(value, "\r", "")
	return value
}

func ttlMillis(d time.Duration) int64 {
	ms := d.Milliseconds()
	if ms < 1 {
		return 1
	}
	return ms
}

func parseCoalesceRecordResult(raw []any) (CoalesceRecordResult, error) {
	if len(raw) != 4 {
		return CoalesceRecordResult{}, fmt.Errorf("expected 4 elements, got %d", len(raw))
	}
	flag, ok := raw[0].(int64)
	if !ok {
		return CoalesceRecordResult{}, fmt.Errorf("unexpected triggered flag type %T", raw[0])
	}
	owner, ok := raw[1].(string)
	if !ok {
		return CoalesceRecordResult{}, fmt.Errorf("unexpected owner type %T", raw[1])
	}
	count, ok := raw[2].(int64)
	if !ok {
		return CoalesceRecordResult{}, fmt.Errorf("unexpected count type %T", raw[2])
	}
	statusRaw, ok := raw[3].(string)
	if !ok {
		return CoalesceRecordResult{}, fmt.Errorf("unexpected status type %T", raw[3])
	}
	status := WindowActive
	if statusRaw == string(WindowPending) {
		status = WindowPending
	}
	return CoalesceRecordResult{
		Triggered:    flag == 1,
		OwnerEventID: owner,
		Occurrences:  int(count),
		Status:       status,
	}, nil
}

func sortWindowSnapshots(windows []IncidentWindowSnapshot) {
	sort.Slice(windows, func(i, j int) bool {
		if windows[i].EventType != windows[j].EventType {
			return windows[i].EventType < windows[j].EventType
		}
		return windows[i].ScopeKey < windows[j].ScopeKey
	})
}

type windowMapKey struct {
	eventType incident.EventType
	scopeKey  string
}

type memoryWindow struct {
	owner     string
	expiresAt time.Time
	snapshot  IncidentWindowSnapshot
}

type memoryIncidentCoalescer struct {
	mu      sync.Mutex
	windows map[windowMapKey]*memoryWindow
	byEvent map[string]*memoryWindow
}

func newMemoryIncidentCoalescer() *memoryIncidentCoalescer {
	return &memoryIncidentCoalescer{
		windows: make(map[windowMapKey]*memoryWindow),
		byEvent: make(map[string]*memoryWindow),
	}
}

func (m *memoryIncidentCoalescer) Record(event incident.Event, policy IncidentPolicy) CoalesceRecordResult {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	m.evictExpiredLocked(now)

	scopeKey := event.EffectiveScopeKey()
	key := windowMapKey{eventType: event.Type, scopeKey: scopeKey}
	minOcc, detectWindow, activeWindow := normalizeWindowDurations(policy)
	ts := event.OccurredAt
	if ts.IsZero() {
		ts = now
	}

	if existing, ok := m.windows[key]; ok {
		existing.snapshot.Occurrences++
		existing.snapshot.LastAt = ts
		existing.snapshot.SamplePaths = appendUniqueSample(existing.snapshot.SamplePaths, sanitizeSample(event.Path))
		existing.snapshot.SampleClientIPs = appendUniqueSample(existing.snapshot.SampleClientIPs, sanitizeSample(event.ClientIP))

		if existing.snapshot.Status == WindowPending && existing.snapshot.Occurrences >= minOcc {
			existing.snapshot.Status = WindowActive
			existing.expiresAt = now.Add(activeWindow)
			return CoalesceRecordResult{
				Triggered:    true,
				OwnerEventID: existing.owner,
				Occurrences:  existing.snapshot.Occurrences,
				Status:       WindowActive,
			}
		}
		return CoalesceRecordResult{
			Triggered:    false,
			OwnerEventID: existing.owner,
			Occurrences:  existing.snapshot.Occurrences,
			Status:       existing.snapshot.Status,
		}
	}

	status := WindowActive
	ttl := activeWindow
	triggered := true
	if minOcc > 1 {
		status = WindowPending
		ttl = detectWindow
		triggered = false
	}
	win := &memoryWindow{
		owner:     event.EventID,
		expiresAt: now.Add(ttl),
		snapshot: IncidentWindowSnapshot{
			EventType:       event.Type,
			ScopeKey:        scopeKey,
			Route:           event.Route,
			EventID:         event.EventID,
			Status:          status,
			Occurrences:     1,
			FirstAt:         ts,
			LastAt:          ts,
			SamplePaths:     appendUniqueSample(nil, sanitizeSample(event.Path)),
			SampleClientIPs: appendUniqueSample(nil, sanitizeSample(event.ClientIP)),
		},
	}
	m.windows[key] = win
	m.byEvent[event.EventID] = win
	return CoalesceRecordResult{
		Triggered:    triggered,
		OwnerEventID: event.EventID,
		Occurrences:  1,
		Status:       status,
	}
}

func (m *memoryIncidentCoalescer) Snapshot(eventType incident.EventType, scopeKey, eventID string) (IncidentWindowSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	win, ok := m.byEvent[eventID]
	if !ok {
		return IncidentWindowSnapshot{}, false
	}
	if win.snapshot.EventType != eventType || win.snapshot.ScopeKey != scopeKey {
		return IncidentWindowSnapshot{}, false
	}
	snap := win.snapshot
	snap.TTLMS = max(int64(0), time.Until(win.expiresAt).Milliseconds())
	snap.SamplePaths = append([]string(nil), win.snapshot.SamplePaths...)
	snap.SampleClientIPs = append([]string(nil), win.snapshot.SampleClientIPs...)
	return snap, true
}

func (m *memoryIncidentCoalescer) UpdateCooldown(eventType incident.EventType, scopeKey, ownerEventID string, ttl time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := windowMapKey{eventType: eventType, scopeKey: scopeKey}
	win, ok := m.windows[key]
	if !ok || win.owner != ownerEventID || win.snapshot.Status != WindowActive {
		return
	}
	if ttl <= 0 {
		delete(m.windows, key)
		delete(m.byEvent, ownerEventID)
		return
	}
	win.expiresAt = time.Now().UTC().Add(ttl)
}

func (m *memoryIncidentCoalescer) ListWindows(filterType incident.EventType, filterScope string) []IncidentWindowSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now().UTC()
	m.evictExpiredLocked(now)

	exactPoint := filterType != "" && filterScope != ""
	out := make([]IncidentWindowSnapshot, 0)
	for _, win := range m.windows {
		if filterType != "" && win.snapshot.EventType != filterType {
			continue
		}
		if filterScope != "" && win.snapshot.ScopeKey != filterScope {
			continue
		}
		if !exactPoint && win.snapshot.Status != WindowActive {
			continue
		}
		snap := win.snapshot
		snap.TTLMS = max(int64(0), win.expiresAt.Sub(now).Milliseconds())
		snap.SamplePaths = append([]string(nil), win.snapshot.SamplePaths...)
		snap.SampleClientIPs = append([]string(nil), win.snapshot.SampleClientIPs...)
		out = append(out, snap)
	}
	sortWindowSnapshots(out)
	return out
}

func (m *memoryIncidentCoalescer) evictExpiredLocked(now time.Time) {
	for key, win := range m.windows {
		if !now.Before(win.expiresAt) {
			delete(m.windows, key)
			if cur := m.byEvent[win.owner]; cur == win {
				delete(m.byEvent, win.owner)
			}
		}
	}
}

func appendUniqueSample(items []string, candidate string) []string {
	if candidate == "" {
		return items
	}
	for _, item := range items {
		if item == candidate {
			return items
		}
	}
	if len(items) >= maxCoalesceSamples {
		return items
	}
	return append(items, candidate)
}

package trafficlog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	queryTimeout    = 10 * time.Second
	maxQueryWindow  = 7 * 24 * time.Hour
	defaultLogLimit = 50
	maxLogLimit     = 200
)

// ErrNotFound is returned when a requested traffic log record does not exist.
var ErrNotFound = errors.New("traffic log not found")

// OverviewStats aggregates traffic volume, error rates, and latency percentiles.
type OverviewStats struct {
	TotalRequests int64   `json:"total_requests"`
	QPS           float64 `json:"qps"`
	Status2xx     int64   `json:"status_2xx"`
	Status4xx     int64   `json:"status_4xx"`
	Status5xx     int64   `json:"status_5xx"`
	ErrorRate     float64 `json:"error_rate"`
	AvgMS         float64 `json:"avg_ms"`
	P50MS         float64 `json:"p50_ms"`
	P95MS         float64 `json:"p95_ms"`
	P99MS         float64 `json:"p99_ms"`
	MaxMS         float64 `json:"max_ms"`
	UniqueClients int64   `json:"unique_clients"`
}

// TimeSeriesPoint represents one time-bucketed aggregation point.
type TimeSeriesPoint struct {
	TS        time.Time `json:"ts"`
	Requests  int64     `json:"requests"`
	Status2xx int64     `json:"status_2xx"`
	Status4xx int64     `json:"status_4xx"`
	Status5xx int64     `json:"status_5xx"`
	AvgMS     float64   `json:"avg_ms"`
	P50MS     float64   `json:"p50_ms"`
	P95MS     float64   `json:"p95_ms"`
	P99MS     float64   `json:"p99_ms"`
}

// RouteStat aggregates metrics grouped by gateway route or exact path.
type RouteStat struct {
	Target    string  `json:"target"`
	Requests  int64   `json:"requests"`
	Status4xx int64   `json:"status_4xx"`
	Status5xx int64   `json:"status_5xx"`
	ErrorRate float64 `json:"error_rate"`
	AvgMS     float64 `json:"avg_ms"`
	P50MS     float64 `json:"p50_ms"`
	P95MS     float64 `json:"p95_ms"`
	P99MS     float64 `json:"p99_ms"`
	MaxMS     float64 `json:"max_ms"`
}

// LogFilter narrows queries against base_logs.
type LogFilter struct {
	Window       time.Duration
	Route        string
	Path         string
	RequestID    string
	TraceID      string
	ClientIP     string
	StatusMin    int
	StatusMax    int
	MinLatencyMS float64
	OnlyErrors   bool
	Limit        int
	Offset       int
}

// LogItem is the summary projection returned by ListLogs.
type LogItem struct {
	ID                int64     `json:"id"`
	RequestID         string    `json:"request_id"`
	TraceID           string    `json:"trace_id"`
	ClientIP          string    `json:"client_ip"`
	Method            string    `json:"method"`
	Path              string    `json:"path"`
	Route             string    `json:"route"`
	StatusCode        int       `json:"status_code"`
	LatencyMS         float64   `json:"latency_ms"`
	UpstreamLatencyMS float64   `json:"upstream_latency_ms"`
	UpstreamResult    string    `json:"upstream_result"`
	Retries           int       `json:"retries"`
	CreatedAt         time.Time `json:"created_at"`
	HasBody           bool      `json:"has_body"`
}

// LogDetail extends LogItem with captured request and response payloads.
type LogDetail struct {
	LogItem
	RequestBody  string `json:"request_body"`
	ResponseBody string `json:"response_body"`
}

// TopClientStat summarizes request and error volume per client IP.
type TopClientStat struct {
	ClientIP   string  `json:"client_ip"`
	Requests   int64   `json:"requests"`
	ErrorCount int64   `json:"error_count"`
	Status5xx  int64   `json:"status_5xx"`
	AvgMS      float64 `json:"avg_ms"`
	P95MS      float64 `json:"p95_ms"`
}

func validateWindow(window time.Duration) error {
	if window <= 0 || window > maxQueryWindow {
		return fmt.Errorf("window must be > 0 and <= %s", maxQueryWindow)
	}
	return nil
}

func queryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, queryTimeout)
}

func (r *Repository) sqlDB() (*sql.DB, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("trafficlog repository is not initialized")
	}
	return r.db.DB()
}

// OverviewStats aggregates request volume, errors, and latency percentiles over window.
func (r *Repository) OverviewStats(ctx context.Context, window time.Duration, route string) (OverviewStats, error) {
	if err := validateWindow(window); err != nil {
		return OverviewStats{}, err
	}
	db, err := r.sqlDB()
	if err != nil {
		return OverviewStats{}, err
	}
	seconds := int64(window.Seconds())
	args := []any{seconds}
	conditions := []string{"created_at >= NOW() - make_interval(secs => $1)"}
	if route != "" {
		args = append(args, route)
		conditions = append(conditions, fmt.Sprintf("route = $%d", len(args)))
	}
	// #nosec G201 -- fixed filter clauses only; all user values stay parameterized.
	query := fmt.Sprintf(`SELECT
	COUNT(*),
	COUNT(*) FILTER (WHERE status_code BETWEEN 200 AND 299),
	COUNT(*) FILTER (WHERE status_code BETWEEN 400 AND 499),
	COUNT(*) FILTER (WHERE status_code >= 500),
	COALESCE(AVG(latency_ms), 0),
	COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms), 0),
	COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0),
	COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms), 0),
	COALESCE(MAX(latency_ms), 0),
	COUNT(DISTINCT client_ip)
FROM base_logs
WHERE %s`, strings.Join(conditions, " AND "))

	qctx, cancel := queryContext(ctx)
	defer cancel()
	var stats OverviewStats
	if err := db.QueryRowContext(qctx, query, args...).Scan(
		&stats.TotalRequests,
		&stats.Status2xx,
		&stats.Status4xx,
		&stats.Status5xx,
		&stats.AvgMS,
		&stats.P50MS,
		&stats.P95MS,
		&stats.P99MS,
		&stats.MaxMS,
		&stats.UniqueClients,
	); err != nil {
		return OverviewStats{}, err
	}
	if stats.TotalRequests > 0 {
		stats.QPS = float64(stats.TotalRequests) / float64(seconds)
		stats.ErrorRate = float64(stats.Status5xx) / float64(stats.TotalRequests)
	}
	return stats, nil
}

// TimeSeries aggregates request counts and latency percentiles into fixed time buckets.
func (r *Repository) TimeSeries(ctx context.Context, window, step time.Duration, route string) ([]TimeSeriesPoint, error) {
	if err := validateWindow(window); err != nil {
		return nil, err
	}
	if step <= 0 || step > window {
		return nil, fmt.Errorf("step must be > 0 and <= window")
	}
	db, err := r.sqlDB()
	if err != nil {
		return nil, err
	}
	stepSeconds := int64(step.Seconds())
	windowSeconds := int64(window.Seconds())
	if stepSeconds <= 0 {
		stepSeconds = 1
	}
	args := []any{stepSeconds, windowSeconds}
	conditions := []string{"created_at >= NOW() - make_interval(secs => $2)"}
	if route != "" {
		args = append(args, route)
		conditions = append(conditions, fmt.Sprintf("route = $%d", len(args)))
	}
	// #nosec G201 -- fixed filter clauses only; all user values stay parameterized.
	query := fmt.Sprintf(`SELECT
	date_bin(make_interval(secs => $1), created_at, TIMESTAMP WITH TIME ZONE '1970-01-01 00:00:00+00') AS bucket,
	COUNT(*),
	COUNT(*) FILTER (WHERE status_code BETWEEN 200 AND 299),
	COUNT(*) FILTER (WHERE status_code BETWEEN 400 AND 499),
	COUNT(*) FILTER (WHERE status_code >= 500),
	COALESCE(AVG(latency_ms), 0),
	COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms), 0),
	COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0),
	COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms), 0)
FROM base_logs
WHERE %s
GROUP BY bucket
ORDER BY bucket ASC`, strings.Join(conditions, " AND "))

	qctx, cancel := queryContext(ctx)
	defer cancel()
	rows, err := db.QueryContext(qctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	points := make([]TimeSeriesPoint, 0)
	for rows.Next() {
		var point TimeSeriesPoint
		if err := rows.Scan(
			&point.TS,
			&point.Requests,
			&point.Status2xx,
			&point.Status4xx,
			&point.Status5xx,
			&point.AvgMS,
			&point.P50MS,
			&point.P95MS,
			&point.P99MS,
		); err != nil {
			return nil, err
		}
		points = append(points, point)
	}
	return points, rows.Err()
}

var routeSortClauses = map[string]string{
	"requests":   "COUNT(*) DESC",
	"p99_ms":     "p99_ms DESC, COUNT(*) DESC",
	"p95_ms":     "p95_ms DESC, COUNT(*) DESC",
	"avg_ms":     "avg_ms DESC, COUNT(*) DESC",
	"status_5xx": "status_5xx DESC, COUNT(*) DESC",
	"error_rate": "CASE WHEN COUNT(*) = 0 THEN 0 ELSE (COUNT(*) FILTER (WHERE status_code >= 500))::double precision / COUNT(*) END DESC",
}

// RouteStats aggregates traffic and latency percentiles grouped by route or path.
func (r *Repository) RouteStats(ctx context.Context, window time.Duration, groupBy, sortBy string, limit int) ([]RouteStat, error) {
	if err := validateWindow(window); err != nil {
		return nil, err
	}
	targetExpr := ""
	switch groupBy {
	case "", "route":
		targetExpr = "CASE WHEN route <> '' THEN route ELSE path END"
	case "path":
		targetExpr = "path"
	default:
		return nil, fmt.Errorf("invalid group_by %q", groupBy)
	}
	if sortBy == "" {
		sortBy = "requests"
	}
	orderClause, ok := routeSortClauses[sortBy]
	if !ok {
		return nil, fmt.Errorf("invalid sort_by %q", sortBy)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	db, err := r.sqlDB()
	if err != nil {
		return nil, err
	}
	// #nosec G201 -- targetExpr and orderClause come strictly from fixed whitelists above.
	query := fmt.Sprintf(`SELECT
	%s AS target,
	COUNT(*),
	COUNT(*) FILTER (WHERE status_code BETWEEN 400 AND 499),
	COUNT(*) FILTER (WHERE status_code >= 500) AS status_5xx,
	COALESCE(AVG(latency_ms), 0) AS avg_ms,
	COALESCE(percentile_cont(0.50) WITHIN GROUP (ORDER BY latency_ms), 0) AS p50_ms,
	COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0) AS p95_ms,
	COALESCE(percentile_cont(0.99) WITHIN GROUP (ORDER BY latency_ms), 0) AS p99_ms,
	COALESCE(MAX(latency_ms), 0) AS max_ms
FROM base_logs
WHERE created_at >= NOW() - make_interval(secs => $1)
GROUP BY target
ORDER BY %s
LIMIT $2`, targetExpr, orderClause)

	qctx, cancel := queryContext(ctx)
	defer cancel()
	rows, err := db.QueryContext(qctx, query, int64(window.Seconds()), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make([]RouteStat, 0)
	for rows.Next() {
		var item RouteStat
		if err := rows.Scan(
			&item.Target,
			&item.Requests,
			&item.Status4xx,
			&item.Status5xx,
			&item.AvgMS,
			&item.P50MS,
			&item.P95MS,
			&item.P99MS,
			&item.MaxMS,
		); err != nil {
			return nil, err
		}
		if item.Requests > 0 {
			item.ErrorRate = float64(item.Status5xx) / float64(item.Requests)
		}
		stats = append(stats, item)
	}
	return stats, rows.Err()
}

// ListLogs returns paginated request log summaries matching filter plus the total count.
func (r *Repository) ListLogs(ctx context.Context, filter LogFilter) ([]LogItem, int64, error) {
	if err := validateWindow(filter.Window); err != nil {
		return nil, 0, err
	}
	if filter.Limit <= 0 {
		filter.Limit = defaultLogLimit
	}
	if filter.Limit > maxLogLimit {
		filter.Limit = maxLogLimit
	}
	if filter.Offset < 0 {
		return nil, 0, errors.New("offset must be non-negative")
	}
	if filter.StatusMin != 0 && (filter.StatusMin < 100 || filter.StatusMin > 599) {
		return nil, 0, errors.New("status_min must be 100..599")
	}
	if filter.StatusMax != 0 && (filter.StatusMax < 100 || filter.StatusMax > 599) {
		return nil, 0, errors.New("status_max must be 100..599")
	}
	if filter.StatusMin > 0 && filter.StatusMax > 0 && filter.StatusMin > filter.StatusMax {
		return nil, 0, errors.New("status_min must not exceed status_max")
	}
	if filter.MinLatencyMS < 0 {
		return nil, 0, errors.New("min_latency_ms must be non-negative")
	}
	db, err := r.sqlDB()
	if err != nil {
		return nil, 0, err
	}

	args := []any{int64(filter.Window.Seconds())}
	conditions := []string{"b.created_at >= NOW() - make_interval(secs => $1)"}
	addCondition := func(expr string, val any) {
		args = append(args, val)
		conditions = append(conditions, fmt.Sprintf(expr, len(args)))
	}
	if filter.Route != "" {
		addCondition("b.route = $%d", filter.Route)
	}
	if filter.Path != "" {
		addCondition("b.path = $%d", filter.Path)
	}
	if filter.RequestID != "" {
		addCondition("b.request_id = $%d", filter.RequestID)
	}
	if filter.TraceID != "" {
		addCondition("b.trace_id = $%d", filter.TraceID)
	}
	if filter.ClientIP != "" {
		addCondition("b.client_ip = $%d", filter.ClientIP)
	}
	if filter.OnlyErrors {
		conditions = append(conditions, "b.status_code >= 400")
	}
	if filter.StatusMin > 0 {
		addCondition("b.status_code >= $%d", filter.StatusMin)
	}
	if filter.StatusMax > 0 {
		addCondition("b.status_code <= $%d", filter.StatusMax)
	}
	if filter.MinLatencyMS > 0 {
		addCondition("b.latency_ms >= $%d", filter.MinLatencyMS)
	}
	whereClause := strings.Join(conditions, " AND ")

	qctx, cancel := queryContext(ctx)
	defer cancel()

	// #nosec G201 -- fixed filter expressions only; all values stay parameterized.
	countQuery := fmt.Sprintf(`SELECT COUNT(*) FROM base_logs b WHERE %s`, whereClause)
	var total int64
	if err := db.QueryRowContext(qctx, countQuery, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, filter.Limit, filter.Offset)
	// #nosec G201 -- fixed filter expressions only; all values stay parameterized.
	listQuery := fmt.Sprintf(`SELECT
	b.id, b.request_id, b.trace_id, b.client_ip, b.method, b.path, b.route,
	b.status_code, b.latency_ms, b.upstream_latency_ms, b.upstream_result,
	b.retries, b.created_at, (e.id IS NOT NULL) AS has_body
FROM base_logs b
LEFT JOIN error_body e ON e.base_log_id = b.id
WHERE %s
ORDER BY b.created_at DESC, b.id DESC
LIMIT $%d OFFSET $%d`, whereClause, len(args)-1, len(args))

	rows, err := db.QueryContext(qctx, listQuery, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]LogItem, 0)
	for rows.Next() {
		var item LogItem
		if err := rows.Scan(
			&item.ID,
			&item.RequestID,
			&item.TraceID,
			&item.ClientIP,
			&item.Method,
			&item.Path,
			&item.Route,
			&item.StatusCode,
			&item.LatencyMS,
			&item.UpstreamLatencyMS,
			&item.UpstreamResult,
			&item.Retries,
			&item.CreatedAt,
			&item.HasBody,
		); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

// GetLogByID fetches one log row and any captured request/response bodies.
func (r *Repository) GetLogByID(ctx context.Context, id int64) (*LogDetail, error) {
	if id <= 0 {
		return nil, errors.New("id must be positive")
	}
	return r.getSingleLog(ctx, "b.id = $1", id)
}

// GetLogByRequestID fetches the most recent log row matching requestID.
func (r *Repository) GetLogByRequestID(ctx context.Context, requestID string) (*LogDetail, error) {
	if strings.TrimSpace(requestID) == "" {
		return nil, errors.New("request_id is required")
	}
	return r.getSingleLog(ctx, "b.request_id = $1", requestID)
}

func (r *Repository) getSingleLog(ctx context.Context, predicate string, arg any) (*LogDetail, error) {
	db, err := r.sqlDB()
	if err != nil {
		return nil, err
	}
	// #nosec G201 -- predicate is a constant column match with $1 placeholder.
	query := fmt.Sprintf(`SELECT
	b.id, b.request_id, b.trace_id, b.client_ip, b.method, b.path, b.route,
	b.status_code, b.latency_ms, b.upstream_latency_ms, b.upstream_result,
	b.retries, b.created_at, (e.id IS NOT NULL) AS has_body,
	COALESCE(e.request_body, ''), COALESCE(e.response_body, '')
FROM base_logs b
LEFT JOIN error_body e ON e.base_log_id = b.id
WHERE %s
ORDER BY b.created_at DESC, b.id DESC
LIMIT 1`, predicate)

	qctx, cancel := queryContext(ctx)
	defer cancel()
	var detail LogDetail
	err = db.QueryRowContext(qctx, query, arg).Scan(
		&detail.ID,
		&detail.RequestID,
		&detail.TraceID,
		&detail.ClientIP,
		&detail.Method,
		&detail.Path,
		&detail.Route,
		&detail.StatusCode,
		&detail.LatencyMS,
		&detail.UpstreamLatencyMS,
		&detail.UpstreamResult,
		&detail.Retries,
		&detail.CreatedAt,
		&detail.HasBody,
		&detail.RequestBody,
		&detail.ResponseBody,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &detail, nil
}

// GetLogsByTraceID fetches log entries associated with traceID along with any error bodies.
func (r *Repository) GetLogsByTraceID(ctx context.Context, traceID string, limit int) ([]LogDetail, error) {
	if strings.TrimSpace(traceID) == "" {
		return nil, errors.New("trace_id is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	db, err := r.sqlDB()
	if err != nil {
		return nil, err
	}
	query := `SELECT
	b.id, b.request_id, b.trace_id, b.client_ip, b.method, b.path, b.route,
	b.status_code, b.latency_ms, b.upstream_latency_ms, b.upstream_result,
	b.retries, b.created_at, (e.id IS NOT NULL) AS has_body,
	COALESCE(e.request_body, ''), COALESCE(e.response_body, '')
FROM base_logs b
LEFT JOIN error_body e ON e.base_log_id = b.id
WHERE b.trace_id = $1
ORDER BY b.created_at ASC, b.id ASC
LIMIT $2`

	qctx, cancel := queryContext(ctx)
	defer cancel()
	rows, err := db.QueryContext(qctx, query, traceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	details := make([]LogDetail, 0)
	for rows.Next() {
		var detail LogDetail
		if err := rows.Scan(
			&detail.ID,
			&detail.RequestID,
			&detail.TraceID,
			&detail.ClientIP,
			&detail.Method,
			&detail.Path,
			&detail.Route,
			&detail.StatusCode,
			&detail.LatencyMS,
			&detail.UpstreamLatencyMS,
			&detail.UpstreamResult,
			&detail.Retries,
			&detail.CreatedAt,
			&detail.HasBody,
			&detail.RequestBody,
			&detail.ResponseBody,
		); err != nil {
			return nil, err
		}
		details = append(details, detail)
	}
	return details, rows.Err()
}

// TopClients returns the top client IPs ordered by request volume over window.
func (r *Repository) TopClients(ctx context.Context, window time.Duration, route string, limit int) ([]TopClientStat, error) {
	if err := validateWindow(window); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	db, err := r.sqlDB()
	if err != nil {
		return nil, err
	}
	args := []any{int64(window.Seconds())}
	conditions := []string{"created_at >= NOW() - make_interval(secs => $1)"}
	if route != "" {
		args = append(args, route)
		conditions = append(conditions, fmt.Sprintf("route = $%d", len(args)))
	}
	args = append(args, limit)
	// #nosec G201 -- fixed filter expressions only; all values stay parameterized.
	query := fmt.Sprintf(`SELECT
	client_ip,
	COUNT(*),
	COUNT(*) FILTER (WHERE status_code >= 400),
	COUNT(*) FILTER (WHERE status_code >= 500),
	COALESCE(AVG(latency_ms), 0),
	COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)
FROM base_logs
WHERE %s
GROUP BY client_ip
ORDER BY COUNT(*) DESC
LIMIT $%d`, strings.Join(conditions, " AND "), len(args))

	qctx, cancel := queryContext(ctx)
	defer cancel()
	rows, err := db.QueryContext(qctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	clients := make([]TopClientStat, 0)
	for rows.Next() {
		var item TopClientStat
		if err := rows.Scan(
			&item.ClientIP,
			&item.Requests,
			&item.ErrorCount,
			&item.Status5xx,
			&item.AvgMS,
			&item.P95MS,
		); err != nil {
			return nil, err
		}
		clients = append(clients, item)
	}
	return clients, rows.Err()
}

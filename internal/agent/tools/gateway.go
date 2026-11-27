package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/oxkrypton/Halon/internal/agent"
	"github.com/oxkrypton/Halon/internal/authz"
)

// NewGatewayTools returns the read-only gateway state inspection tool and the
// mutating blacklist tool when both adminBaseURL and adminToken are configured.
func NewGatewayTools(kubernetes *KubernetesToolset, adminBaseURL, adminToken string, client *http.Client) []agent.Tool {
	if strings.TrimSpace(adminBaseURL) == "" || strings.TrimSpace(adminToken) == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: queryTimeout}
	}
	return []agent.Tool{
		{
			Name:       "read_gateway",
			Permission: authz.PermToolObserve,
			Description: "Inspect live gateway runtime state stored in Redis and memory across one or more sources: " +
				"state (Redis health, blocked IP list, rate-limit parameters, per-route circuit breaker statuses), " +
				"ip_status (client IP blacklist membership and Redis token-bucket state without consuming tokens), " +
				"incident_windows (active per-(type, scope_key) incident coalescing windows, sample paths/IPs, and remaining cooldown TTL). " +
				"Supports batching multiple queries in one call.",
			Parameters: batchQuerySchema(map[string]any{
				"source": enumStringProperty("Gateway runtime data source to inspect.", "state", "ip_status", "incident_windows"),
				"ip":     stringProperty("[ip_status] Required IPv4 or IPv6 address to inspect."),
				"type":   stringProperty("[incident_windows] Optional incident type filter, for example upstream_timeout, upstream_error, breaker_open, upstream_5xx, concurrency_limit, rate_limit_abuse."),
				"scope":  stringProperty("[incident_windows] Optional exact scope key filter (route prefix or client IP)."),
				"route":  stringProperty("[incident_windows] Optional route prefix alias for scope."),
			}, "One or more gateway state queries to run sequentially."),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				return runBatchQueries(raw, func(source string, item json.RawMessage) (any, error) {
					switch source {
					case "state":
						if _, err := decodeSourceQuery[struct {
							Source string `json:"source"`
						}](item, source, "(none)"); err != nil {
							return nil, err
						}
						return doGatewayGet(ctx, client, adminBaseURL, adminToken, "/admin/state/gateway")
					case "ip_status":
						input, err := decodeSourceQuery[struct {
							Source string `json:"source"`
							IP     string `json:"ip"`
						}](item, source, "ip (required)")
						if err != nil {
							return nil, err
						}
						parsed := net.ParseIP(strings.TrimSpace(input.IP))
						if parsed == nil {
							return nil, fmt.Errorf("valid ip is required")
						}
						return doGatewayGet(ctx, client, adminBaseURL, adminToken, "/admin/state/rate-limit/"+url.PathEscape(parsed.String()))
					case "incident_windows":
						input, err := decodeSourceQuery[struct {
							Source string `json:"source"`
							Type   string `json:"type"`
							Scope  string `json:"scope"`
							Route  string `json:"route"`
						}](item, source, "type, scope, route")
						if err != nil {
							return nil, err
						}
						q := url.Values{}
						if eventType := strings.TrimSpace(input.Type); eventType != "" {
							q.Set("type", eventType)
						}
						if scope := strings.TrimSpace(input.Scope); scope != "" {
							q.Set("scope", scope)
						} else if route := strings.TrimSpace(input.Route); route != "" {
							q.Set("scope", route)
						}
						path := "/admin/state/incident-windows"
						if encoded := q.Encode(); encoded != "" {
							path += "?" + encoded
						}
						return doGatewayGet(ctx, client, adminBaseURL, adminToken, path)
					default:
						return nil, fmt.Errorf("unsupported source %q; allowed sources: state, ip_status, incident_windows", source)
					}
				})
			},
		},
		NewGatewayActionTool(kubernetes, adminBaseURL, adminToken, client),
	}
}

func NewGatewayActionTool(kubernetes *KubernetesToolset, adminBaseURL, adminToken string, client *http.Client) agent.Tool {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	return agent.Tool{
		Name: "gateway_block_ip", Permission: authz.PermGatewayBlacklistWrite,
		Description: "Add one IP to the live gateway blacklist. Use only when read_observability (top_ips or error_logs) and read_gateway (ip_status) show clear abuse; this does not fix upstream outages or timeouts.",
		Parameters:  objectSchema(map[string]any{"ip": stringProperty("IPv4 or IPv6 address supported by request evidence."), "reason": stringProperty("Evidence-based blocking reason.")}, "ip", "reason"),
		Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
			input, err := decodeArguments[struct {
				IP     string `json:"ip"`
				Reason string `json:"reason"`
			}](raw)
			if err != nil {
				return nil, err
			}
			parsed := net.ParseIP(input.IP)
			if parsed == nil || strings.TrimSpace(input.Reason) == "" {
				return nil, fmt.Errorf("valid ip and reason are required")
			}
			if strings.TrimSpace(adminBaseURL) == "" {
				return nil, fmt.Errorf("GATEWAY_ADMIN_URL is not configured")
			}
			ip := parsed.String()
			err = kubernetes.withResourceAction(ctx, gatewayIPCooldownKey(ip), func() error {
				request, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(adminBaseURL, "/")+"/admin/rules/blacklist/"+url.PathEscape(ip), nil)
				if err != nil {
					return err
				}
				request.Header.Set("Authorization", "Bearer "+adminToken)
				response, err := client.Do(request)
				if err != nil {
					return err
				}
				defer func() { _ = response.Body.Close() }()
				body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
				if response.StatusCode < 200 || response.StatusCode >= 300 {
					return fmt.Errorf("gateway returned %s: %s", response.Status, strings.TrimSpace(string(body)))
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			return map[string]any{"status": "blocked", "ip": ip, "reason": input.Reason}, nil
		},
	}
}

func doGatewayGet(ctx context.Context, client *http.Client, adminBaseURL, adminToken, path string) (any, error) {
	if strings.TrimSpace(adminBaseURL) == "" {
		return nil, fmt.Errorf("GATEWAY_ADMIN_URL is not configured")
	}
	if strings.TrimSpace(adminToken) == "" {
		return nil, fmt.Errorf("GATEWAY_ADMIN_TOKEN is not configured")
	}
	queryCtx, cancel := queryContext(ctx)
	defer cancel()

	request, err := http.NewRequestWithContext(queryCtx, http.MethodGet, strings.TrimRight(adminBaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+adminToken)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("gateway returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode gateway response: %w", err)
	}
	return payload, nil
}

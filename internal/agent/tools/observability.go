package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/oxkrypton/Halon/internal/agent"
)

const queryTimeout = 15 * time.Second

type ErrorLogRow struct {
	ID           int64     `json:"id"`
	RequestID    string    `json:"request_id"`
	ClientIP     string    `json:"client_ip"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	StatusCode   int       `json:"status_code"`
	LatencyMS    float64   `json:"latency_ms"`
	CreatedAt    time.Time `json:"created_at"`
	RequestBody  string    `json:"request_body"`
	ResponseBody string    `json:"response_body"`
}

type TrafficStatRow struct {
	Path         string  `json:"path"`
	StatusCode   int     `json:"status_code"`
	Requests     int64   `json:"requests"`
	AverageMS    float64 `json:"average_ms"`
	MaximumMS    float64 `json:"maximum_ms"`
	P95MS        float64 `json:"p95_ms"`
	ServerErrors int64   `json:"server_errors"`
}

type TopIPRow struct {
	ClientIP   string  `json:"client_ip"`
	Requests   int64   `json:"requests"`
	ErrorCount int64   `json:"error_count"`
	AverageMS  float64 `json:"average_ms"`
}

func ObservabilityTools(database *sql.DB, prometheusBaseURL string, httpClient *http.Client) []agent.Tool {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: queryTimeout}
	}
	return []agent.Tool{
		{
			Name: "query_error_logs", Level: agent.LevelReadOnly,
			Description: "Query PostgreSQL error and slow request details. Use the incident request_id first, then path and time window to find related failures.",
			Parameters: objectSchema(map[string]any{
				"request_id": stringProperty("Optional exact gateway X-Request-ID."),
				"path":       stringProperty("Optional exact request path, for example /api/v1/users/1."),
				"minutes":    integerProperty("Lookback window in minutes, default 30.", 1, 1440),
				"limit":      integerProperty("Maximum rows, default 20.", 1, 100),
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					RequestID string `json:"request_id"`
					Path      string `json:"path"`
					Minutes   int    `json:"minutes"`
					Limit     int    `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Minutes == 0 {
					input.Minutes = 30
				}
				if input.Limit == 0 {
					input.Limit = 20
				}
				if input.Minutes < 1 || input.Minutes > 1440 || input.Limit < 1 || input.Limit > 100 {
					return nil, fmt.Errorf("minutes must be 1..1440 and limit must be 1..100")
				}
				return queryErrorLogs(ctx, database, input.RequestID, input.Path, input.Minutes, input.Limit)
			},
		},
		{
			Name: "query_traffic_stats", Level: agent.LevelReadOnly,
			Description: "Aggregate PostgreSQL request counts, latency and server errors by exact path and status code.",
			Parameters: objectSchema(map[string]any{
				"path":    stringProperty("Optional exact request path."),
				"minutes": integerProperty("Lookback window in minutes, default 30.", 1, 1440),
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Path    string `json:"path"`
					Minutes int    `json:"minutes"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Minutes == 0 {
					input.Minutes = 30
				}
				if input.Minutes < 1 || input.Minutes > 1440 {
					return nil, fmt.Errorf("minutes must be 1..1440")
				}
				return queryTrafficStats(ctx, database, input.Path, input.Minutes)
			},
		},
		{
			Name: "query_top_ips", Level: agent.LevelReadOnly,
			Description: "Find clients with unusually high request or error counts. Use this as evidence before calling gateway_block_ip.",
			Parameters: objectSchema(map[string]any{
				"path":    stringProperty("Optional exact request path."),
				"minutes": integerProperty("Lookback window in minutes, default 30.", 1, 1440),
				"limit":   integerProperty("Maximum clients, default 10.", 1, 50),
			}),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Path    string `json:"path"`
					Minutes int    `json:"minutes"`
					Limit   int    `json:"limit"`
				}](raw)
				if err != nil {
					return nil, err
				}
				if input.Minutes == 0 {
					input.Minutes = 30
				}
				if input.Limit == 0 {
					input.Limit = 10
				}
				if input.Minutes < 1 || input.Minutes > 1440 || input.Limit < 1 || input.Limit > 50 {
					return nil, fmt.Errorf("minutes must be 1..1440 and limit must be 1..50")
				}
				return queryTopIPs(ctx, database, input.Path, input.Minutes, input.Limit)
			},
		},
		{
			Name: "query_prometheus", Level: agent.LevelReadOnly,
			Description: "Run a read-only Prometheus instant query. Useful queries include rate(halon_gateway_http_requests_total[5m]), histogram_quantile(0.95, sum by (le, route) (rate(halon_gateway_http_request_duration_seconds_bucket[5m]))), and kube_pod_container_status_restarts_total.",
			Parameters: objectSchema(map[string]any{
				"query": stringProperty("PromQL instant query."),
				"time":  stringProperty("Optional RFC3339 evaluation time; defaults to now."),
			}, "query"),
			Handler: func(ctx context.Context, raw json.RawMessage) (any, error) {
				input, err := decodeArguments[struct {
					Query string `json:"query"`
					Time  string `json:"time"`
				}](raw)
				if err != nil {
					return nil, err
				}
				input.Query = strings.TrimSpace(input.Query)
				if input.Query == "" || len(input.Query) > 4096 {
					return nil, fmt.Errorf("query must contain 1..4096 characters")
				}
				if input.Time != "" {
					if _, err := time.Parse(time.RFC3339, input.Time); err != nil {
						return nil, fmt.Errorf("time must be RFC3339")
					}
				}
				return queryPrometheus(ctx, httpClient, prometheusBaseURL, input.Query, input.Time)
			},
		},
	}
}

func queryErrorLogs(ctx context.Context, database *sql.DB, requestID, path string, minutes, limit int) ([]ErrorLogRow, error) {
	conditions := []string{"b.created_at >= NOW() - make_interval(mins => $1)"}
	arguments := []any{minutes}
	if requestID != "" {
		arguments = append(arguments, requestID)
		conditions = append(conditions, fmt.Sprintf("b.request_id = $%d", len(arguments)))
	}
	if path != "" {
		arguments = append(arguments, path)
		conditions = append(conditions, fmt.Sprintf("b.path = $%d", len(arguments)))
	}
	arguments = append(arguments, limit)
	query := fmt.Sprintf(`SELECT b.id, b.request_id, b.client_ip, b.method, b.path, b.status_code, b.latency_ms, b.created_at, e.request_body, e.response_body
FROM base_logs b JOIN error_body e ON e.base_log_id = b.id
WHERE %s ORDER BY b.created_at DESC LIMIT $%d`, strings.Join(conditions, " AND "), len(arguments))
	ctx, cancel := queryContext(ctx)
	defer cancel()
	rows, err := database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]ErrorLogRow, 0)
	for rows.Next() {
		var row ErrorLogRow
		if err := rows.Scan(&row.ID, &row.RequestID, &row.ClientIP, &row.Method, &row.Path, &row.StatusCode, &row.LatencyMS, &row.CreatedAt, &row.RequestBody, &row.ResponseBody); err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

func queryTrafficStats(ctx context.Context, database *sql.DB, path string, minutes int) ([]TrafficStatRow, error) {
	arguments := []any{minutes}
	filter := ""
	if path != "" {
		arguments = append(arguments, path)
		filter = " AND path = $2"
	}
	query := `SELECT path, status_code, COUNT(*), AVG(latency_ms), MAX(latency_ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY latency_ms), COUNT(*) FILTER (WHERE status_code >= 500)
FROM base_logs WHERE created_at >= NOW() - make_interval(mins => $1)` + filter + ` GROUP BY path, status_code ORDER BY COUNT(*) DESC LIMIT 50`
	ctx, cancel := queryContext(ctx)
	defer cancel()
	rows, err := database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]TrafficStatRow, 0)
	for rows.Next() {
		var row TrafficStatRow
		if err := rows.Scan(&row.Path, &row.StatusCode, &row.Requests, &row.AverageMS, &row.MaximumMS, &row.P95MS, &row.ServerErrors); err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

func queryTopIPs(ctx context.Context, database *sql.DB, path string, minutes, limit int) ([]TopIPRow, error) {
	arguments := []any{minutes}
	filter := ""
	if path != "" {
		arguments = append(arguments, path)
		filter = " AND path = $2"
	}
	arguments = append(arguments, limit)
	query := fmt.Sprintf(`SELECT client_ip, COUNT(*), COUNT(*) FILTER (WHERE status_code >= 400), AVG(latency_ms)
FROM base_logs WHERE created_at >= NOW() - make_interval(mins => $1)%s GROUP BY client_ip ORDER BY COUNT(*) DESC LIMIT $%d`, filter, len(arguments))
	ctx, cancel := queryContext(ctx)
	defer cancel()
	rows, err := database.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]TopIPRow, 0)
	for rows.Next() {
		var row TopIPRow
		if err := rows.Scan(&row.ClientIP, &row.Requests, &row.ErrorCount, &row.AverageMS); err != nil {
			return nil, err
		}
		results = append(results, row)
	}
	return results, rows.Err()
}

func queryPrometheus(ctx context.Context, client *http.Client, baseURL, query, evaluationTime string) (map[string]any, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, fmt.Errorf("PROMETHEUS_URL is not configured")
	}
	form := url.Values{"query": {query}}
	if evaluationTime != "" {
		form.Set("time", evaluationTime)
	}
	ctx, cancel := queryContext(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(baseURL, "/")+"/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	var envelope struct {
		Status string          `json:"status"`
		Data   json.RawMessage `json:"data"`
		Error  string          `json:"error"`
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 8<<20))
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode Prometheus response: %w", err)
	}
	if response.StatusCode != http.StatusOK || envelope.Status != "success" {
		return nil, fmt.Errorf("Prometheus query failed with %s: %s", response.Status, envelope.Error)
	}
	return map[string]any{"status": envelope.Status, "data": envelope.Data, "evaluation_time": evaluationTime, "query": query, "unix_time": strconv.FormatInt(time.Now().Unix(), 10)}, nil
}

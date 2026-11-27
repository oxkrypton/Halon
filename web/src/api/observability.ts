import { apiFetch } from './client';

export type TimeWindow = '15m' | '1h' | '6h' | '24h' | '7d';

export interface OverviewStats {
  total_requests: number;
  qps: number;
  status_2xx: number;
  status_4xx: number;
  status_5xx: number;
  error_rate: number;
  avg_ms: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  max_ms: number;
  unique_clients: number;
}

export interface RejectedCounts {
  total: number;
  blacklist: number;
  rate_limit: number;
  concurrency_limit: number;
  body_too_large: number;
}

export interface RuntimeMetricsSnapshot {
  in_flight_requests: number;
  upstream_in_flight: number;
  rejected: RejectedCounts;
  retries_total: number;
  retry_exhausted_total: number;
  breaker_trips_total: number;
  trafficlog_dropped: number;
  incident_events_dropped: number;
  webhook_notify_failures: number;
}

export interface TimeSeriesPoint {
  ts: string;
  requests: number;
  status_2xx: number;
  status_4xx: number;
  status_5xx: number;
  avg_ms: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
}

export interface RouteStat {
  target: string;
  requests: number;
  status_4xx: number;
  status_5xx: number;
  error_rate: number;
  avg_ms: number;
  p50_ms: number;
  p95_ms: number;
  p99_ms: number;
  max_ms: number;
}

export interface LogItem {
  id: number;
  request_id: string;
  trace_id: string;
  client_ip: string;
  method: string;
  path: string;
  route: string;
  status_code: number;
  latency_ms: number;
  upstream_latency_ms: number;
  upstream_result: string;
  retries: number;
  created_at: string;
  has_body: boolean;
}

export interface LogDetail extends LogItem {
  request_body: string;
  response_body: string;
}

export interface TopClientStat {
  client_ip: string;
  requests: number;
  error_count: number;
  status_5xx: number;
  avg_ms: number;
  p95_ms: number;
}

export function fetchOverview(window: TimeWindow, route = '') {
  const params = new URLSearchParams({ window });
  if (route) params.set('route', route);
  return apiFetch<{ overview: OverviewStats; runtime?: RuntimeMetricsSnapshot; window: string }>(
    `/api/gateway/admin/observability/overview?${params}`,
  );
}

export function fetchTimeSeries(window: TimeWindow, route = '') {
  const params = new URLSearchParams({ window });
  if (route) params.set('route', route);
  return apiFetch<{ points: TimeSeriesPoint[]; window: string; step_seconds: number }>(
    `/api/gateway/admin/observability/timeseries?${params}`,
  );
}

export function fetchRouteStats(
  window: TimeWindow,
  groupBy: 'route' | 'path' = 'route',
  sortBy = 'requests',
  limit = 50,
) {
  const params = new URLSearchParams({
    window,
    group_by: groupBy,
    sort_by: sortBy,
    limit: String(limit),
  });
  return apiFetch<{ routes: RouteStat[]; group_by: string; sort_by: string }>(
    `/api/gateway/admin/observability/routes?${params}`,
  );
}

export interface LogsQuery {
  window: TimeWindow;
  route?: string;
  path?: string;
  request_id?: string;
  trace_id?: string;
  client_ip?: string;
  status_min?: number;
  status_max?: number;
  min_latency_ms?: number;
  only_errors?: boolean;
  limit?: number;
  offset?: number;
}

export function fetchLogs(query: LogsQuery) {
  const params = new URLSearchParams({ window: query.window });
  if (query.route) params.set('route', query.route);
  if (query.path) params.set('path', query.path);
  if (query.request_id) params.set('request_id', query.request_id);
  if (query.trace_id) params.set('trace_id', query.trace_id);
  if (query.client_ip) params.set('client_ip', query.client_ip);
  if (query.status_min) params.set('status_min', String(query.status_min));
  if (query.status_max) params.set('status_max', String(query.status_max));
  if (query.min_latency_ms) params.set('min_latency_ms', String(query.min_latency_ms));
  if (query.only_errors) params.set('only_errors', 'true');
  if (query.limit) params.set('limit', String(query.limit));
  if (query.offset) params.set('offset', String(query.offset));
  return apiFetch<{ logs: LogItem[]; total: number; limit: number; offset: number }>(
    `/api/gateway/admin/observability/logs?${params}`,
  );
}

export function fetchLogDetail(id: number) {
  return apiFetch<{ log: LogDetail }>(`/api/gateway/admin/observability/logs/${id}`);
}

export function fetchTopIPs(window: TimeWindow, route = '', limit = 20) {
  const params = new URLSearchParams({ window, limit: String(limit) });
  if (route) params.set('route', route);
  return apiFetch<{ clients: TopClientStat[] }>(
    `/api/gateway/admin/observability/top-ips?${params}`,
  );
}

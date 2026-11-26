import { apiFetch } from './client';
import type { LogDetail, TimeWindow } from './observability';

export interface TraceSummary {
  trace_id: string;
  request_id?: string;
  root_service: string;
  root_operation: string;
  route?: string;
  start_time: string;
  duration_ms: number;
  span_count: number;
  status_code?: number;
  has_error: boolean;
  source: 'tempo' | 'postgres';
}

export interface TraceEvent {
  name: string;
  time_unix_ns: number;
  attributes?: Record<string, unknown>;
}

export interface TraceSpan {
  span_id: string;
  parent_span_id?: string;
  service_name: string;
  name: string;
  kind?: string;
  start_unix_ns: number;
  end_unix_ns: number;
  offset_ms: number;
  duration_ms: number;
  status: 'ok' | 'error' | 'unset';
  status_message?: string;
  attributes?: Record<string, unknown>;
  events?: TraceEvent[];
}

export interface TraceDetail {
  trace_id: string;
  source: 'tempo' | 'postgres';
  root_service: string;
  root_operation: string;
  start_time: string;
  duration_ms: number;
  has_error: boolean;
  services: string[];
  spans: TraceSpan[];
  logs?: LogDetail[];
}

export interface TracesQuery {
  window: TimeWindow;
  route?: string;
  service?: string;
  min_duration_ms?: number;
  only_errors?: boolean;
  limit?: number;
  source?: 'auto' | 'tempo' | 'postgres';
}

export function fetchTraces(query: TracesQuery) {
  const params = new URLSearchParams({ window: query.window });
  if (query.route) params.set('route', query.route);
  if (query.service) params.set('service', query.service);
  if (query.min_duration_ms) params.set('min_duration_ms', String(query.min_duration_ms));
  if (query.only_errors) params.set('only_errors', 'true');
  if (query.limit) params.set('limit', String(query.limit));
  if (query.source) params.set('source', query.source);
  return apiFetch<{ source: 'tempo' | 'postgres'; traces: TraceSummary[] }>(
    `/api/gateway/admin/traces?${params}`,
  );
}

export function fetchTraceDetail(traceId: string) {
  return apiFetch<{ source: 'tempo' | 'postgres'; trace: TraceDetail }>(
    `/api/gateway/admin/traces/${encodeURIComponent(traceId)}`,
  );
}

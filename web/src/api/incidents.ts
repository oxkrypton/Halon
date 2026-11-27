import { apiFetch } from './client';

export type IncidentStatus = 'queued' | 'processing' | 'completed' | 'failed';

export type BuiltinEventType =
  | 'upstream_timeout'
  | 'upstream_error'
  | 'breaker_open'
  | 'upstream_5xx'
  | 'concurrency_limit'
  | 'rate_limit_abuse';

export interface ToolExecutionRecord {
  step?: number;
  tool?: string;
  arguments?: unknown;
  output?: unknown;
  error?: string;
  duration_ms?: number;
  output_truncated?: boolean;
}

export interface IncidentReport {
  event_id: string;
  event_type: string;
  scope_key: string;
  request_id?: string;
  occurred_at: string;
  method?: string;
  path?: string;
  route?: string;
  client_ip?: string;
  status_code?: number;
  latency_ms?: number;
  summary?: string;
  occurrences: number;
  details?: Record<string, unknown>;
  status: IncidentStatus;
  final_message: string;
  steps: number;
  executions?: ToolExecutionRecord[];
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  error?: string;
  started_at?: string;
  finished_at?: string;
  created_at: string;
  updated_at: string;
}

export function fetchIncidents(limit = 50, eventType = '') {
  const params = new URLSearchParams({ limit: String(limit) });
  if (eventType) params.set('type', eventType);
  return apiFetch<{ incidents: IncidentReport[] }>(`/api/agent/api/v1/incidents?${params}`);
}

export function fetchIncidentDetail(eventId: string) {
  return apiFetch<{ incident: IncidentReport }>(
    `/api/agent/api/v1/incidents/${encodeURIComponent(eventId)}`,
  );
}

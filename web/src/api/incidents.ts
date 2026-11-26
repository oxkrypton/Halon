import { apiFetch } from './client';

export type IncidentStatus = 'queued' | 'processing' | 'completed' | 'failed';

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
  request_id: string;
  occurred_at: string;
  method: string;
  path: string;
  route: string;
  client_ip: string;
  status_code: number;
  latency_ms: number;
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

export function fetchIncidents(limit = 50) {
  return apiFetch<{ incidents: IncidentReport[] }>(`/api/agent/api/v1/incidents?limit=${limit}`);
}

export function fetchIncidentDetail(eventId: string) {
  return apiFetch<{ incident: IncidentReport }>(
    `/api/agent/api/v1/incidents/${encodeURIComponent(eventId)}`,
  );
}

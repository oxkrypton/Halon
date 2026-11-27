import { apiFetch } from './client';

export interface ContainerState {
  name: string;
  ready: boolean;
  restart_count: number;
  state: string;
  reason: string;
}

export interface PodSummary {
  name: string;
  phase: string;
  ready: boolean;
  restarts: number;
  node: string;
  created_at: string;
  containers: ContainerState[];
}

export interface PodListResponse {
  namespace: string;
  namespaces: string[];
  pods: PodSummary[];
}

export interface PodLogResponse {
  namespace: string;
  pod: string;
  container: string;
  previous: boolean;
  logs: string;
}

export interface PodsQuery {
  namespace?: string;
  labelSelector?: string;
  limit?: number;
}

export interface PodLogsQuery {
  namespace?: string;
  container?: string;
  tailLines?: number;
  sinceSeconds?: number;
  previous?: boolean;
}

export function fetchPods(query: PodsQuery = {}) {
  const params = new URLSearchParams();
  if (query.namespace) params.set('namespace', query.namespace);
  if (query.labelSelector) params.set('label_selector', query.labelSelector);
  if (query.limit) params.set('limit', String(query.limit));
  const qs = params.toString();
  return apiFetch<PodListResponse>(`/api/agent/api/v1/k8s/pods${qs ? `?${qs}` : ''}`);
}

export function fetchPodLogs(pod: string, query: PodLogsQuery = {}) {
  const params = new URLSearchParams();
  if (query.namespace) params.set('namespace', query.namespace);
  if (query.container) params.set('container', query.container);
  if (query.tailLines) params.set('tail_lines', String(query.tailLines));
  if (query.sinceSeconds) params.set('since_seconds', String(query.sinceSeconds));
  if (query.previous) params.set('previous', 'true');
  const qs = params.toString();
  return apiFetch<PodLogResponse>(
    `/api/agent/api/v1/k8s/pods/${encodeURIComponent(pod)}/logs${qs ? `?${qs}` : ''}`,
  );
}

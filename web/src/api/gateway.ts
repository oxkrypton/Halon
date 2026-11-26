import { apiFetch } from './client';
import type { Role } from './auth';

export interface BreakerStatus {
  route: string;
  state: 'closed' | 'half-open' | 'open' | 'disabled';
  remaining_delay_seconds?: number;
}

export interface RateLimitSettings {
  requests_per_second: number;
  burst: number;
}

export interface UserView {
  id: string;
  username: string;
  display_name: string;
  role: Role;
  disabled: boolean;
  must_change_password: boolean;
  created_at: string;
}

export function fetchBreakers() {
  return apiFetch<{ breakers: BreakerStatus[] }>('/api/gateway/admin/breakers');
}

export function fetchBlacklist() {
  return apiFetch<{ ips: string[] }>('/api/gateway/admin/rules/blacklist');
}

export function blockIP(ip: string) {
  return apiFetch<{ ip: string; propagation?: string }>(
    `/api/gateway/admin/rules/blacklist/${encodeURIComponent(ip)}`,
    { method: 'PUT' },
  );
}

export function unblockIP(ip: string) {
  return apiFetch<void>(`/api/gateway/admin/rules/blacklist/${encodeURIComponent(ip)}`, {
    method: 'DELETE',
  });
}

export function fetchRateLimit() {
  return apiFetch<RateLimitSettings>('/api/gateway/admin/rules/rate-limit');
}

export function updateRateLimit(requestsPerSecond: number, burst: number) {
  return apiFetch<RateLimitSettings>('/api/gateway/admin/rules/rate-limit', {
    method: 'PUT',
    body: JSON.stringify({ requests_per_second: requestsPerSecond, burst }),
  });
}

export function fetchUsers(offset = 0, limit = 50) {
  return apiFetch<{ users: UserView[]; total: number; offset: number; limit: number }>(
    `/api/gateway/admin/users?offset=${offset}&limit=${limit}`,
  );
}

export function createUser(input: {
  username: string;
  role: Role;
  display_name?: string;
  password?: string;
}) {
  return apiFetch<{ user: UserView; generated_password?: string }>('/api/gateway/admin/users', {
    method: 'POST',
    body: JSON.stringify(input),
  });
}

export function updateUser(
  id: string,
  patch: { role?: Role; disabled?: boolean; display_name?: string },
) {
  return apiFetch<{ user: UserView }>(`/api/gateway/admin/users/${encodeURIComponent(id)}`, {
    method: 'PATCH',
    body: JSON.stringify(patch),
  });
}

export function resetUserPassword(id: string) {
  return apiFetch<{ generated_password: string; must_change_password: boolean }>(
    `/api/gateway/admin/users/${encodeURIComponent(id)}/password`,
    { method: 'POST' },
  );
}

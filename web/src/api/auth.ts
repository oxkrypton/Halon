import { apiFetch } from './client';

export type Role = 'readonly' | 'operator' | 'full';

export interface LoginResponse {
  access_token: string;
  expires_in: number;
  refresh_token: string;
  must_change_password: boolean;
  user: {
    id: string;
    username: string;
    role: Role;
  };
}

export interface RouteEntry {
  prefix: string;
  upstream: string;
}

export interface SystemInfoResponse {
  principal: {
    username: string;
    user_id?: string;
    role: Role;
    type?: string;
    must_change_password?: boolean;
  };
  routes: RouteEntry[];
  capabilities: {
    auth_mode: string;
    login_enabled: boolean;
    otel_enabled: boolean;
    tempo_enabled: boolean;
  };
  health: {
    redis_up: boolean;
  };
}

export async function loginWithPassword(username: string, password: string): Promise<LoginResponse> {
  return apiFetch<LoginResponse>('/api/gateway/auth/login', {
    method: 'POST',
    skipAuthRefresh: true,
    body: JSON.stringify({ username, password }),
  });
}

export async function logoutSession(refreshToken: string): Promise<void> {
  if (!refreshToken) return;
  await apiFetch<void>('/api/gateway/auth/logout', {
    method: 'POST',
    skipAuthRefresh: true,
    body: JSON.stringify({ refresh_token: refreshToken }),
  });
}

export async function changeOwnPassword(currentPassword: string, newPassword: string): Promise<void> {
  await apiFetch<void>('/api/gateway/auth/password', {
    method: 'POST',
    body: JSON.stringify({
      current_password: currentPassword,
      new_password: newPassword,
    }),
  });
}

export async function fetchSystemInfo(): Promise<SystemInfoResponse> {
  return apiFetch<SystemInfoResponse>('/api/gateway/admin/system');
}

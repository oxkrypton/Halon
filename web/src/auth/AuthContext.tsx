import React, { createContext, useCallback, useContext, useEffect, useMemo, useState } from 'react';
import {
  clearAuthSession,
  getAuthTokens,
  setAuthEventListener,
  setBearerTokenSession,
  setLocalSession,
  ApiError,
} from '../api/client';
import {
  changeOwnPassword,
  fetchSystemInfo,
  loginWithPassword,
  logoutSession,
  type Role,
  type SystemInfoResponse,
} from '../api/auth';
import type { TimeWindow } from '../api/observability';

export type Permission =
  | 'incident:read'
  | 'gateway:rules:read'
  | 'gateway:breaker:read'
  | 'gateway:metrics:read'
  | 'gateway:observability:read'
  | 'gateway:blacklist:write'
  | 'gateway:ratelimit:write'
  | 'auth:user:read'
  | 'auth:user:manage';

const ROLE_RANK: Record<Role, number> = {
  readonly: 0,
  operator: 1,
  full: 2,
};

const PERMISSION_REQUIRED_ROLE: Record<Permission, Role> = {
  'incident:read': 'readonly',
  'gateway:rules:read': 'readonly',
  'gateway:breaker:read': 'readonly',
  'gateway:metrics:read': 'readonly',
  'gateway:observability:read': 'readonly',
  'gateway:blacklist:write': 'operator',
  'gateway:ratelimit:write': 'full',
  'auth:user:read': 'full',
  'auth:user:manage': 'full',
};

interface AuthContextValue {
  status: 'loading' | 'authenticated' | 'unauthenticated' | 'password_change_required';
  system: SystemInfoResponse | null;
  timeWindow: TimeWindow;
  setTimeWindow: (w: TimeWindow) => void;
  refreshIntervalMs: number | false;
  setRefreshIntervalMs: (ms: number | false) => void;
  can: (perm: Permission) => boolean;
  signInWithPassword: (username: string, password: string) => Promise<void>;
  signInWithToken: (token: string) => Promise<void>;
  completeForcedPasswordChange: (currentPassword: string, newPassword: string) => Promise<void>;
  refreshSystem: () => Promise<void>;
  signOut: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: React.ReactNode }) {
  const [status, setStatus] = useState<AuthContextValue['status']>('loading');
  const [system, setSystem] = useState<SystemInfoResponse | null>(null);
  const [timeWindow, setTimeWindow] = useState<TimeWindow>('1h');
  const [refreshIntervalMs, setRefreshIntervalMs] = useState<number | false>(30_000);

  const loadSystem = useCallback(async () => {
    const info = await fetchSystemInfo();
    setSystem(info);
    if (info.principal?.must_change_password) {
      setStatus('password_change_required');
    } else {
      setStatus('authenticated');
    }
  }, []);

  useEffect(() => {
    setAuthEventListener((event) => {
      if (event === 'password_change_required') {
        setStatus('password_change_required');
      } else if (event === 'expired') {
        setSystem(null);
        setStatus('unauthenticated');
      }
    });
    return () => setAuthEventListener(null);
  }, []);

  useEffect(() => {
    const tokens = getAuthTokens();
    if (!tokens.accessToken && !tokens.refreshToken) {
      setStatus('unauthenticated');
      return;
    }
    loadSystem().catch(() => {
      clearAuthSession();
      setSystem(null);
      setStatus('unauthenticated');
    });
  }, [loadSystem]);

  const signInWithPassword = useCallback(
    async (username: string, password: string) => {
      const session = await loginWithPassword(username, password);
      setLocalSession(session.access_token, session.refresh_token);
      if (session.must_change_password) {
        setSystem({
          principal: {
            username: session.user.username,
            user_id: session.user.id,
            role: session.user.role,
            type: 'human',
            must_change_password: true,
          },
          routes: [],
          capabilities: {
            auth_mode: 'jwt',
            login_enabled: true,
            otel_enabled: false,
            tempo_enabled: false,
          },
          health: { redis_up: true },
        });
        setStatus('password_change_required');
        return;
      }
      await loadSystem();
    },
    [loadSystem],
  );

  const signInWithToken = useCallback(
    async (token: string) => {
      setBearerTokenSession(token.trim());
      try {
        await loadSystem();
      } catch (err) {
        clearAuthSession();
        if (err instanceof ApiError && err.code === 'PASSWORD_CHANGE_REQUIRED') {
          setBearerTokenSession(token.trim());
          setStatus('password_change_required');
          return;
        }
        throw err;
      }
    },
    [loadSystem],
  );

  const completeForcedPasswordChange = useCallback(
    async (currentPassword: string, newPassword: string) => {
      await changeOwnPassword(currentPassword, newPassword);
      clearAuthSession();
      setSystem(null);
      setStatus('unauthenticated');
    },
    [],
  );

  const signOut = useCallback(async () => {
    const { mode, refreshToken } = getAuthTokens();
    if (mode === 'local' && refreshToken) {
      try {
        await logoutSession(refreshToken);
      } catch {
        // Ignore logout network error and clear local state anyway.
      }
    }
    clearAuthSession();
    setSystem(null);
    setStatus('unauthenticated');
  }, []);

  const can = useCallback(
    (perm: Permission) => {
      const role = system?.principal?.role;
      if (!role) return false;
      const required = PERMISSION_REQUIRED_ROLE[perm];
      return (ROLE_RANK[role] ?? -1) >= (ROLE_RANK[required] ?? 99);
    },
    [system],
  );

  const value = useMemo<AuthContextValue>(
    () => ({
      status,
      system,
      timeWindow,
      setTimeWindow,
      refreshIntervalMs,
      setRefreshIntervalMs,
      can,
      signInWithPassword,
      signInWithToken,
      completeForcedPasswordChange,
      refreshSystem: loadSystem,
      signOut,
    }),
    [
      status,
      system,
      timeWindow,
      refreshIntervalMs,
      can,
      signInWithPassword,
      signInWithToken,
      completeForcedPasswordChange,
      loadSystem,
      signOut,
    ],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error('useAuth must be used inside AuthProvider');
  return ctx;
}

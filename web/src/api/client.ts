export interface ApiErrorDetail {
  code: string;
  message: string;
  request_id?: string;
}

export class ApiError extends Error {
  status: number;
  code: string;
  requestId?: string;

  constructor(status: number, code: string, message: string, requestId?: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

export type AuthSessionMode = 'local' | 'token';

interface TokenState {
  mode: AuthSessionMode;
  accessToken: string;
  refreshToken: string;
}

const REFRESH_STORAGE_KEY = 'halon_console_refresh_token';
const MODE_STORAGE_KEY = 'halon_console_auth_mode';

let tokenState: TokenState = {
  mode: (sessionStorage.getItem(MODE_STORAGE_KEY) as AuthSessionMode) || 'local',
  accessToken: '',
  refreshToken: sessionStorage.getItem(REFRESH_STORAGE_KEY) || '',
};

let refreshPromise: Promise<string | null> | null = null;
let onAuthEvent: ((event: 'expired' | 'password_change_required') => void) | null = null;

export function setAuthEventListener(listener: ((event: 'expired' | 'password_change_required') => void) | null) {
  onAuthEvent = listener;
}

export function getAuthTokens(): TokenState {
  return { ...tokenState };
}

export function setLocalSession(accessToken: string, refreshToken: string) {
  tokenState = { mode: 'local', accessToken, refreshToken };
  sessionStorage.setItem(MODE_STORAGE_KEY, 'local');
  if (refreshToken) {
    sessionStorage.setItem(REFRESH_STORAGE_KEY, refreshToken);
  }
}

export function setBearerTokenSession(token: string) {
  tokenState = { mode: 'token', accessToken: token, refreshToken: '' };
  sessionStorage.setItem(MODE_STORAGE_KEY, 'token');
  sessionStorage.removeItem(REFRESH_STORAGE_KEY);
}

export function clearAuthSession() {
  tokenState = { mode: 'local', accessToken: '', refreshToken: '' };
  sessionStorage.removeItem(REFRESH_STORAGE_KEY);
  sessionStorage.removeItem(MODE_STORAGE_KEY);
}

async function performRefresh(): Promise<string | null> {
  if (tokenState.mode !== 'local' || !tokenState.refreshToken) {
    return null;
  }
  const resp = await fetch('/api/gateway/auth/refresh', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ refresh_token: tokenState.refreshToken }),
  });
  if (!resp.ok) {
    clearAuthSession();
    onAuthEvent?.('expired');
    return null;
  }
  const data = (await resp.json()) as { access_token: string; refresh_token: string };
  setLocalSession(data.access_token, data.refresh_token);
  return data.access_token;
}

export async function refreshAccessTokenSingleFlight(): Promise<string | null> {
  if (!refreshPromise) {
    refreshPromise = performRefresh().finally(() => {
      refreshPromise = null;
    });
  }
  return refreshPromise;
}

export async function apiFetch<T>(
  url: string,
  options: RequestInit & { skipAuthRefresh?: boolean } = {},
): Promise<T> {
  const { skipAuthRefresh, headers: customHeaders, ...rest } = options;
  let token = tokenState.accessToken;
  if (!token && tokenState.mode === 'local' && tokenState.refreshToken && !skipAuthRefresh) {
    token = (await refreshAccessTokenSingleFlight()) || '';
  }

  const doRequest = async (bearer: string): Promise<Response> => {
    const headers = new Headers(customHeaders);
    if (!headers.has('Content-Type') && rest.body !== undefined) {
      headers.set('Content-Type', 'application/json');
    }
    if (bearer) {
      headers.set('Authorization', `Bearer ${bearer}`);
    }
    return fetch(url, { ...rest, headers });
  };

  let resp = await doRequest(token);

  if (resp.status === 401 && !skipAuthRefresh && tokenState.mode === 'local' && tokenState.refreshToken) {
    const refreshed = await refreshAccessTokenSingleFlight();
    if (refreshed) {
      resp = await doRequest(refreshed);
    }
  }

  if (resp.status === 204) {
    return undefined as T;
  }

  let body: unknown = null;
  const text = await resp.text();
  if (text) {
    try {
      body = JSON.parse(text);
    } catch {
      body = text;
    }
  }

  if (!resp.ok) {
    const errObj = (body as { error?: ApiErrorDetail } | null)?.error;
    const code = errObj?.code || `HTTP_${resp.status}`;
    const message = errObj?.message || resp.statusText || 'Request failed';
    if (resp.status === 403 && code === 'PASSWORD_CHANGE_REQUIRED') {
      onAuthEvent?.('password_change_required');
    } else if (resp.status === 401 && !skipAuthRefresh) {
      onAuthEvent?.('expired');
    }
    throw new ApiError(resp.status, code, message, errObj?.request_id);
  }

  return body as T;
}

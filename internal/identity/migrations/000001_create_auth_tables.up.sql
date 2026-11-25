CREATE TABLE auth_users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username VARCHAR(64) NOT NULL UNIQUE
        CHECK (username ~ '^[a-z0-9][a-z0-9._-]{2,63}$'),
    display_name VARCHAR(128) NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL,
    role VARCHAR(16) NOT NULL CHECK (role IN ('readonly','operator','full')),
    disabled BOOLEAN NOT NULL DEFAULT FALSE,
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    failed_login_count INTEGER NOT NULL DEFAULT 0 CHECK (failed_login_count >= 0),
    locked_until TIMESTAMPTZ,
    password_changed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ,
    created_by UUID REFERENCES auth_users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_auth_users_role_enabled ON auth_users (role) WHERE disabled = FALSE;

CREATE TABLE auth_refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES auth_users(id) ON DELETE CASCADE,
    family_id UUID NOT NULL,
    family_started_at TIMESTAMPTZ NOT NULL,
    token_hash BYTEA NOT NULL UNIQUE,
    issued_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    client_ip VARCHAR(45) NOT NULL DEFAULT '',
    user_agent VARCHAR(256) NOT NULL DEFAULT ''
);
CREATE INDEX idx_auth_refresh_user ON auth_refresh_tokens (user_id);
CREATE INDEX idx_auth_refresh_family ON auth_refresh_tokens (family_id);
CREATE INDEX idx_auth_refresh_expires ON auth_refresh_tokens (expires_at);

CREATE TABLE auth_events (
    id BIGSERIAL PRIMARY KEY,
    at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    action VARCHAR(48) NOT NULL,
    result VARCHAR(16) NOT NULL CHECK (result IN ('success','failure')),
    actor_username VARCHAR(64) NOT NULL DEFAULT '',
    target_username VARCHAR(64) NOT NULL DEFAULT '',
    client_ip VARCHAR(45) NOT NULL DEFAULT '',
    request_id VARCHAR(64) NOT NULL DEFAULT '',
    detail JSONB
);
CREATE INDEX idx_auth_events_at ON auth_events (at DESC);
CREATE INDEX idx_auth_events_target ON auth_events (target_username, at DESC);

-- Authentication-store migration only; never applied to a repository database.
CREATE TABLE IF NOT EXISTS ui_sessions (
    token_sha256 TEXT PRIMARY KEY,
    credential_id TEXT NOT NULL REFERENCES credentials(id),
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS ui_sessions_expiry ON ui_sessions(expires_at);

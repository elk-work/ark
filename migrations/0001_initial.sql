-- Ark initial schema. See docs/v1-spec.md §6–§7.
-- Conventions: text ULID primary keys, RFC3339Nano UTC timestamps as text,
-- JSON as text, explicit foreign keys, soft deletion via deleted_at.

CREATE TABLE repositories (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    path            TEXT NOT NULL,
    git_remote_url  TEXT NOT NULL DEFAULT '',
    default_branch  TEXT NOT NULL DEFAULT 'main',
    created_at      TEXT NOT NULL
);

CREATE TABLE actors (
    id           TEXT PRIMARY KEY,
    type         TEXT NOT NULL CHECK (type IN ('human', 'agent')),
    name         TEXT NOT NULL,
    email        TEXT NOT NULL DEFAULT '',
    agent_name   TEXT NOT NULL DEFAULT '',
    agent_version TEXT NOT NULL DEFAULT '',
    delegated_by TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL
);

CREATE TABLE tasks (
    id              TEXT PRIMARY KEY,
    repository_id   TEXT NOT NULL REFERENCES repositories(id),
    number          INTEGER NOT NULL,
    title           TEXT NOT NULL,
    body            TEXT NOT NULL DEFAULT '',
    status          TEXT NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open', 'in_progress', 'blocked', 'done', 'closed')),
    created_at      TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    updated_at      TEXT NOT NULL,
    deleted_at      TEXT,
    version         INTEGER NOT NULL DEFAULT 1,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    server_revision INTEGER NOT NULL DEFAULT 0,
    UNIQUE (repository_id, number)
);

CREATE INDEX idx_tasks_repo_status ON tasks(repository_id, status);

CREATE TABLE comments (
    id              TEXT PRIMARY KEY,
    repository_id   TEXT NOT NULL REFERENCES repositories(id),
    parent_type     TEXT NOT NULL
                    CHECK (parent_type IN ('task', 'pull_request', 'agent_run', 'review')),
    parent_id       TEXT NOT NULL,
    body            TEXT NOT NULL,
    created_at      TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    supersedes_id   TEXT REFERENCES comments(id),
    deleted_at      TEXT,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    server_revision INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_comments_parent ON comments(parent_type, parent_id);

CREATE TABLE agent_threads (
    id              TEXT PRIMARY KEY,
    repository_id   TEXT NOT NULL REFERENCES repositories(id),
    task_id         TEXT REFERENCES tasks(id),
    title           TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'closed')),
    created_at      TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    closed_at       TEXT,
    deleted_at      TEXT,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    server_revision INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_threads_task ON agent_threads(task_id);

CREATE TABLE thread_messages (
    id              TEXT PRIMARY KEY,
    thread_id       TEXT NOT NULL REFERENCES agent_threads(id),
    role            TEXT NOT NULL CHECK (role IN ('user', 'agent', 'system', 'tool')),
    body            TEXT NOT NULL,
    metadata_json   TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    supersedes_id   TEXT REFERENCES thread_messages(id),
    deleted_at      TEXT,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    server_revision INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_thread_messages_thread ON thread_messages(thread_id, created_at);

CREATE TABLE agent_runs (
    id                TEXT PRIMARY KEY,
    repository_id     TEXT NOT NULL REFERENCES repositories(id),
    task_id           TEXT REFERENCES tasks(id),
    thread_id         TEXT REFERENCES agent_threads(id),
    agent_name        TEXT NOT NULL,
    agent_version     TEXT NOT NULL DEFAULT '',
    status            TEXT NOT NULL DEFAULT 'queued'
                      CHECK (status IN ('queued', 'running', 'succeeded', 'failed', 'cancelled')),
    input_summary     TEXT NOT NULL DEFAULT '',
    result_summary    TEXT NOT NULL DEFAULT '',
    started_at        TEXT,
    finished_at       TEXT,
    exit_code         INTEGER,
    branch_name       TEXT NOT NULL DEFAULT '',
    base_commit_sha   TEXT NOT NULL DEFAULT '',
    result_commit_sha TEXT NOT NULL DEFAULT '',
    metadata_json     TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    created_by        TEXT NOT NULL,
    created_by_type   TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    deleted_at        TEXT,
    sync_state        TEXT NOT NULL DEFAULT 'local',
    server_revision   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_agent_runs_task ON agent_runs(task_id, started_at);

CREATE TABLE pull_requests (
    id               TEXT PRIMARY KEY,
    repository_id    TEXT NOT NULL REFERENCES repositories(id),
    number           INTEGER NOT NULL,
    task_id          TEXT REFERENCES tasks(id),
    title            TEXT NOT NULL,
    body             TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'merged', 'closed')),
    base_branch      TEXT NOT NULL,
    head_branch      TEXT NOT NULL,
    base_commit_sha  TEXT NOT NULL DEFAULT '',
    head_commit_sha  TEXT NOT NULL DEFAULT '',
    merge_commit_sha TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    created_by       TEXT NOT NULL,
    created_by_type  TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    merged_at        TEXT,
    closed_at        TEXT,
    updated_at       TEXT NOT NULL,
    deleted_at       TEXT,
    version          INTEGER NOT NULL DEFAULT 1,
    sync_state       TEXT NOT NULL DEFAULT 'local',
    server_revision  INTEGER NOT NULL DEFAULT 0,
    UNIQUE (repository_id, number)
);

CREATE INDEX idx_prs_repo_status ON pull_requests(repository_id, status);

CREATE TABLE reviews (
    id              TEXT PRIMARY KEY,
    repository_id   TEXT NOT NULL REFERENCES repositories(id),
    pull_request_id TEXT NOT NULL REFERENCES pull_requests(id),
    state           TEXT NOT NULL CHECK (state IN ('comment', 'approve', 'request_changes')),
    body            TEXT NOT NULL DEFAULT '',
    commit_sha      TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    deleted_at      TEXT,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    server_revision INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_reviews_pr ON reviews(pull_request_id, created_at);

CREATE TABLE artifacts (
    id              TEXT PRIMARY KEY,
    repository_id   TEXT NOT NULL REFERENCES repositories(id),
    parent_type     TEXT NOT NULL
                    CHECK (parent_type IN ('task', 'agent_run', 'pull_request', 'review')),
    parent_id       TEXT NOT NULL,
    name            TEXT NOT NULL,
    media_type      TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes      INTEGER NOT NULL,
    sha256          TEXT NOT NULL,
    local_path      TEXT NOT NULL DEFAULT '',
    storage_key     TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    created_by      TEXT NOT NULL,
    created_by_type TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    deleted_at      TEXT,
    sync_state      TEXT NOT NULL DEFAULT 'local',
    server_revision INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_artifacts_parent ON artifacts(parent_type, parent_id);
CREATE INDEX idx_artifacts_sha ON artifacts(sha256);

-- Every local write records the intent that produced it. See docs/v1-spec.md §8.
CREATE TABLE mutations (
    id                   TEXT PRIMARY KEY,
    repository_id        TEXT NOT NULL REFERENCES repositories(id),
    record_type          TEXT NOT NULL,
    record_id            TEXT NOT NULL,
    operation            TEXT NOT NULL
                         CHECK (operation IN ('create', 'update', 'delete', 'append', 'submit')),
    base_server_revision INTEGER NOT NULL DEFAULT 0,
    payload_json         TEXT NOT NULL,
    created_at           TEXT NOT NULL,
    created_by           TEXT NOT NULL,
    status               TEXT NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'applied', 'rejected', 'conflict')),
    error_message        TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_mutations_status ON mutations(status, created_at);

CREATE TABLE sync_state (
    repository_id  TEXT PRIMARY KEY REFERENCES repositories(id),
    client_id      TEXT NOT NULL,
    last_revision  INTEGER NOT NULL DEFAULT 0,
    last_synced_at TEXT
);

CREATE TABLE conflicts (
    id          TEXT PRIMARY KEY,
    record_type TEXT NOT NULL,
    record_id   TEXT NOT NULL,
    mutation_id TEXT NOT NULL REFERENCES mutations(id),
    base_json   TEXT NOT NULL DEFAULT '',
    local_json  TEXT NOT NULL DEFAULT '',
    remote_json TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT 'unresolved'
                CHECK (status IN ('unresolved', 'resolved_local', 'resolved_remote',
                                  'resolved_manual', 'abandoned')),
    created_at  TEXT NOT NULL,
    resolved_at TEXT
);

CREATE INDEX idx_conflicts_status ON conflicts(status, created_at);

-- One shared full-text index across searchable record types.
-- Maintained by application code inside the same transaction as the write.
CREATE VIRTUAL TABLE fts USING fts5(
    record_type UNINDEXED,
    record_id UNINDEXED,
    title,
    body
);

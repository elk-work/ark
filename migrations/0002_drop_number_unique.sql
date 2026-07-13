-- Display numbers are reconciled by the sync service (two offline clients
-- can both mint #7, and the server renumbers the later one). A local UNIQUE
-- constraint rejects the pull that delivers the other client's record before
-- ours is renumbered in the same batch, so numbers are indexed but not
-- unique locally; the ULID stays authoritative.

CREATE TABLE tasks_new (
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
    server_revision INTEGER NOT NULL DEFAULT 0
);

INSERT INTO tasks_new SELECT id, repository_id, number, title, body, status,
    created_at, created_by, created_by_type, updated_at, deleted_at, version,
    sync_state, server_revision FROM tasks;
DROP TABLE tasks;
ALTER TABLE tasks_new RENAME TO tasks;
CREATE INDEX idx_tasks_repo_status ON tasks(repository_id, status);
CREATE INDEX idx_tasks_repo_number ON tasks(repository_id, number);

CREATE TABLE pull_requests_new (
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
    server_revision  INTEGER NOT NULL DEFAULT 0
);

INSERT INTO pull_requests_new SELECT id, repository_id, number, task_id, title, body,
    status, base_branch, head_branch, base_commit_sha, head_commit_sha, merge_commit_sha,
    created_at, created_by, created_by_type, merged_at, closed_at, updated_at, deleted_at,
    version, sync_state, server_revision FROM pull_requests;
DROP TABLE pull_requests;
ALTER TABLE pull_requests_new RENAME TO pull_requests;
CREATE INDEX idx_prs_repo_status ON pull_requests(repository_id, status);
CREATE INDEX idx_prs_repo_number ON pull_requests(repository_id, number);

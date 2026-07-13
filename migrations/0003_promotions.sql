-- A promotion records that a version (a merge commit and/or an artifact
-- checksum) became active in an environment. It is the deployment anchor
-- for observability tooling. See docs/v1-spec.md §6.10.

CREATE TABLE promotions (
    id               TEXT PRIMARY KEY,
    repository_id    TEXT NOT NULL REFERENCES repositories(id),
    environment      TEXT NOT NULL,
    service          TEXT NOT NULL DEFAULT '',
    merge_commit_sha TEXT NOT NULL DEFAULT '',
    artifact_sha256  TEXT NOT NULL DEFAULT '',
    pull_request_id  TEXT REFERENCES pull_requests(id),
    activated_at     TEXT NOT NULL,
    ended_at         TEXT,
    metadata_json    TEXT NOT NULL DEFAULT '',
    created_at       TEXT NOT NULL,
    created_by       TEXT NOT NULL,
    created_by_type  TEXT NOT NULL CHECK (created_by_type IN ('human', 'agent')),
    deleted_at       TEXT,
    sync_state       TEXT NOT NULL DEFAULT 'local',
    server_revision  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX idx_promotions_repo_env ON promotions(repository_id, environment, activated_at);
CREATE INDEX idx_promotions_merge_sha ON promotions(merge_commit_sha);

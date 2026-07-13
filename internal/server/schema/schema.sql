-- Ark sync service schema (PostgreSQL). Cloud SQL is authoritative for
-- shared metadata; records are stored as JSON documents with a per-repo
-- revision counter. See docs/v1-spec.md §9, §10, §19.

CREATE TABLE IF NOT EXISTS repositories (
    id             text PRIMARY KEY,
    name           text NOT NULL,
    default_branch text NOT NULL DEFAULT 'main',
    git_remote_url text NOT NULL DEFAULT '',
    revision       bigint NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now()
);

-- One row per record (tasks, comments, threads, messages, runs, PRs,
-- reviews, artifacts, actors). data holds the client-side JSON encoding.
CREATE TABLE IF NOT EXISTS records (
    repository_id   text NOT NULL REFERENCES repositories(id),
    record_type     text NOT NULL,
    record_id       text NOT NULL,
    data            jsonb NOT NULL,
    server_revision bigint NOT NULL,
    deleted_at      timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, record_type, record_id)
);

CREATE INDEX IF NOT EXISTS idx_records_revision
    ON records (repository_id, server_revision);
CREATE INDEX IF NOT EXISTS idx_records_number
    ON records (repository_id, record_type, ((data->>'number')::bigint))
    WHERE data ? 'number';

-- Which repo revision last changed each field of a record. Powers the
-- field-level merge rules in docs/v1-spec.md §10.4.
CREATE TABLE IF NOT EXISTS field_revisions (
    repository_id text NOT NULL,
    record_type   text NOT NULL,
    record_id     text NOT NULL,
    field         text NOT NULL,
    revision      bigint NOT NULL,
    PRIMARY KEY (repository_id, record_type, record_id, field)
);

-- Idempotency: a mutation ID is processed exactly once; replays return the
-- stored outcome.
CREATE TABLE IF NOT EXISTS applied_mutations (
    mutation_id     text PRIMARY KEY,
    repository_id   text NOT NULL,
    status          text NOT NULL, -- applied|rejected|conflict
    error           text NOT NULL DEFAULT '',
    remote          jsonb,
    server_revision bigint NOT NULL DEFAULT 0,
    applied_at      timestamptz NOT NULL DEFAULT now()
);

-- Artifact blobs known to object storage, keyed by content hash.
CREATE TABLE IF NOT EXISTS blobs (
    repository_id text NOT NULL,
    sha256        text NOT NULL,
    size_bytes    bigint NOT NULL,
    storage_key   text NOT NULL,
    stored        boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (repository_id, sha256)
);

-- Ark sync service schema: one SQLite database per repository.
-- The file IS the tenant; there is no repository_id column because the
-- database never holds more than one repository. See docs/rfc-0001.

-- Single-row repository metadata and the revision counter.
CREATE TABLE IF NOT EXISTS meta (
    id             INTEGER PRIMARY KEY CHECK (id = 1),
    repository_id  TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    default_branch TEXT NOT NULL DEFAULT 'main',
    git_remote_url TEXT NOT NULL DEFAULT '',
    revision       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL
);

-- One row per record (tasks, comments, threads, messages, runs, PRs,
-- reviews, artifacts, actors). data holds the client-side JSON encoding.
CREATE TABLE IF NOT EXISTS records (
    record_type     TEXT NOT NULL,
    record_id       TEXT NOT NULL,
    data            TEXT NOT NULL,
    server_revision INTEGER NOT NULL,
    deleted_at      TEXT,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    PRIMARY KEY (record_type, record_id)
);

CREATE INDEX IF NOT EXISTS idx_records_revision ON records (server_revision);

-- Which revision last changed each field of a record. Powers the
-- field-level merge rules in docs/v1-spec.md §10.4.
CREATE TABLE IF NOT EXISTS field_revisions (
    record_type TEXT NOT NULL,
    record_id   TEXT NOT NULL,
    field       TEXT NOT NULL,
    revision    INTEGER NOT NULL,
    PRIMARY KEY (record_type, record_id, field)
);

-- Idempotency: a mutation ID is processed exactly once; replays return the
-- stored outcome.
CREATE TABLE IF NOT EXISTS applied_mutations (
    mutation_id     TEXT PRIMARY KEY,
    status          TEXT NOT NULL, -- applied|rejected|conflict
    error           TEXT NOT NULL DEFAULT '',
    remote          TEXT,
    server_revision INTEGER NOT NULL DEFAULT 0,
    applied_at      TEXT NOT NULL
);

-- Artifact blobs known to object storage, keyed by content hash.
CREATE TABLE IF NOT EXISTS blobs (
    sha256      TEXT PRIMARY KEY,
    size_bytes  INTEGER NOT NULL,
    storage_key TEXT NOT NULL,
    stored      INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL
);

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

-- Every reference the service accepted while holding no record it points at.
--
-- `applyCreate` inserts unconditionally and deliberately: mutations are
-- ordered by ULID *within* a push, but nothing orders two clients' pushes
-- against each other, so a parent's create can legitimately still be in
-- another client's queue. Refusing the child would turn a recoverable
-- ordering skew into a permanent rejection, which since elk-work/ark#46 is a
-- loud terminal state rather than a silent one. What was missing is any trace
-- that it happened, so the service could both declare a record absent and
-- store a child pointing at it, in one transaction, and say nothing about the
-- orphan it had just created (elk-work/ark#56).
--
-- The row records the event, not the state: the child, the field that
-- referred, the record it named, and the mutation that brought it. Whether
-- the reference is *still* dangling is a live comparison against `records`,
-- because that is a question this side of the wire can simply answer:
--
--   SELECT d.* FROM dangling_references d WHERE NOT EXISTS (
--     SELECT 1 FROM records r
--     WHERE r.record_type = d.parent_type AND r.record_id = d.parent_id);
--
-- No resolved_at column, and that is the one place this departs from the
-- shape of a client-side rejection (migrations/0004). A rejection needs a
-- stamp because the client cannot see the service's copy; here both sides of
-- the comparison are one table away, records are never hard-deleted, so the
-- comparison is monotone and true whenever it is made. A stamp would also
-- have to be written by every path that can create a record — the mutation
-- engine and the write API's writeRecord — and the one that forgot would
-- leave the ledger reporting an orphan that no longer exists. A count that
-- cries wolf is the warning light nobody reads, which is the same silence in
-- a different costume.
CREATE TABLE IF NOT EXISTS dangling_references (
    record_type   TEXT NOT NULL, -- the child that was accepted
    record_id     TEXT NOT NULL,
    field         TEXT NOT NULL, -- the referring field: parent_id, thread_id, ...
    parent_type   TEXT NOT NULL, -- the record it names, and does not resolve to
    parent_id     TEXT NOT NULL,
    mutation_id   TEXT NOT NULL DEFAULT '',
    first_seen_at TEXT NOT NULL,
    PRIMARY KEY (record_type, record_id, field)
);

CREATE INDEX IF NOT EXISTS idx_dangling_parent
    ON dangling_references (parent_type, parent_id);

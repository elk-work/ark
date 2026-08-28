-- A pulled record that names a record this client does not hold cannot be
-- written and left as an orphan. Every typed pointer between records is a
-- declared foreign key, and `PRAGMA defer_foreign_keys` moves that check to
-- COMMIT rather than removing it, so writing one fails the commit — which
-- fails the entire pull transaction, discards every good record that arrived
-- with it, and leaves the cursor where it was. The next pull then requests the
-- same range, receives the same record, and fails identically: a client
-- permanently unable to sync with that repository, over a record that lives on
-- the service and that nothing local can remove (elk-work/ark#75).
--
-- So the client holds the record back instead, the way it already skips a
-- record type it does not know (docs/v1-spec.md §9.2). This table is where it
-- holds them, and holding is the part that cannot be skipped: the cursor
-- advances past the record's revision, so the service will never send it
-- again. A skip that kept no copy would trade a wedged client for a client
-- that quietly loses records both sides hold, which is the worse of the two.
--
-- Retried on every later pull and deleted the moment the record applies, so
-- the set is self-clearing and a non-empty one is always a current statement
-- about what this checkout cannot yet see. Nothing here needs an operator.
--
-- repository_id deliberately carries no REFERENCES clause, unlike every other
-- table. The referent this row records as missing can itself be the
-- repository, and a ledger of unresolvable references that cannot be written
-- when a reference does not resolve would reintroduce the failure it exists to
-- prevent.

CREATE TABLE deferred_records (
    record_type     TEXT NOT NULL,
    record_id       TEXT NOT NULL,
    repository_id   TEXT NOT NULL,
    data_json       TEXT NOT NULL,
    server_revision INTEGER NOT NULL,
    -- The reference that could not be resolved: the column that carried it,
    -- the table it pointed into, and the id it named. Kept so an operator is
    -- told which record is missing rather than only that one is.
    field           TEXT NOT NULL,
    missing_table   TEXT NOT NULL,
    missing_id      TEXT NOT NULL,
    first_seen_at   TEXT NOT NULL,
    last_seen_at    TEXT NOT NULL,
    PRIMARY KEY (record_type, record_id)
);

CREATE INDEX idx_deferred_records_repo ON deferred_records(repository_id, server_revision);

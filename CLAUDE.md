# Ark — guidance for coding agents

Ark is a local-first work record system beside Git, written in Go. Read
`docs/principles.md` before making design decisions; `docs/v1-spec.md` is
the implementation contract.

## Commands

- Build: `go build ./...`
- Test: `go test ./...` (integration tests create temp Git repos; no network)
- One test: `go test ./internal/store -run TestTaskLifecycle`
- Sync tests need Postgres: they default to `postgres://ark@127.0.0.1:5499/arktest`
  (override with ARK_TEST_PG) and skip when unreachable. Start a throwaway
  instance: `initdb -D <dir> -U ark --auth=trust`, then
  `pg_ctl -D <dir> -o "-p 5499 -c listen_addresses=127.0.0.1 -c unix_socket_directories=''" start`
  and `createdb -h 127.0.0.1 -p 5499 -U ark arktest`.

## Architecture in one paragraph

`cmd/ark` → `internal/cli` (cobra) → `internal/app` (finds `.ark/`, opens
SQLite, resolves the acting identity) → `internal/store` (all record
operations). Every mutating store method runs one SQLite transaction that
writes the record, a row in `mutations` (the intent, for future cloud sync),
and the FTS index update together. `internal/git` shells out to the Git CLI
with machine-readable flags — never reimplement Git. Schema lives in
`migrations/*.sql` (embedded, forward-only, numbered).

## Invariants to preserve

- A repository managed by Ark stays a completely normal Git repository.
- Every record carries `created_by` + `created_by_type` (human or agent).
- Comments, thread messages: append-only; corrections use `supersedes_id`.
- Submitted reviews are immutable; artifacts are immutable by checksum.
- Every local write logs a mutation in the same transaction — no exceptions.
- Task/PR numbers are display aliases; ULIDs are authoritative.
- Errors use `records.Error` kinds; CLI exit codes follow spec §22
  (2 validation, 3 not found, 4 conflict, 5 permission, 6 offline, 7 partial).
- `--json` output is a stable interface for agents; treat field renames as
  breaking changes.
- Pure-Go SQLite driver (modernc.org/sqlite); do not introduce CGO.

## Sync architecture (Phases 4–5)

`cmd/ark-server` (internal/server) is the authoritative service: records
live as JSONB documents in Postgres keyed (repo, type, id) with a per-repo
revision counter; `field_revisions` powers spec §10.4 field-level merges
(title/body overlap → conflict; other overlaps → cloud wins);
`applied_mutations` makes pushes idempotent. Pushes serialize on a
repository row lock. The client (internal/sync + internal/cloud) pushes the
mutation queue, uploads artifact blobs via signed URLs, then pulls records
after its cursor and upserts them (internal/store/sync.go) with deferred FK
checks. Server-assigned display numbers can be rewritten on collision — the
ULID is authoritative, so local numbers are indexed but not unique. Tokens
resolve ARK_TOKEN → macOS keychain → ~/.ark/credentials.toml and never live
in the repository.

## What is deliberately absent (V1)

Workspaces, projects, milestones, a web UI, hosted Git, a custom merge
engine, full `gh` parity, multi-user authorization (one bearer token).
Do not add primitives without a demonstrated need (principle 005).

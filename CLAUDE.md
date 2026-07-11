# Ark — guidance for coding agents

Ark is a local-first work record system beside Git, written in Go. Read
`docs/principles.md` before making design decisions; `docs/v1-spec.md` is
the implementation contract.

## Commands

- Build: `go build ./...`
- Test: `go test ./...` (integration tests create temp Git repos; no network)
- One test: `go test ./internal/store -run TestTaskLifecycle`

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

## What is deliberately absent (V1)

Workspaces, projects, milestones, a web UI, hosted Git, a custom merge
engine, full `gh` parity. Cloud sync (spec Phases 4–5: push/pull protocol,
Cloud Run + Cloud SQL + GCS) is designed but not built; `ark sync` exits 6.
Do not add primitives without a demonstrated need (principle 005).

# Ark — guidance for coding agents

Ark is a local-first work record system beside Git, written in Go. Read
`docs/principles.md` before making design decisions; `docs/v1-spec.md` is
the implementation contract.

## Commands

- Build: `go build ./...`
- Test: `go test ./...` (integration tests create temp Git repos; no network)
- One test: `go test ./internal/store -run TestTaskLifecycle`
- The whole suite (including sync) is self-contained: temp Git repos, temp
  SQLite files, no external services.

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
- Task/PR numbers are display aliases; ULIDs are authoritative. In prose,
  write `ark:signal#14` — never a bare `#14`, which reads as (and links to) a
  GitHub issue — and always name the repository, including the one you are in.
  See README "Writing about tasks".
- Errors use `records.Error` kinds; CLI exit codes follow spec §22
  (2 validation, 3 not found, 4 conflict, 5 permission, 6 offline, 7 partial,
  8 the service's stored copy of this repository is unusable).
- `--json` output is a stable interface for agents; treat field renames as
  breaking changes.
- Pure-Go SQLite driver (modernc.org/sqlite); do not introduce CGO.

## Sync architecture (Phases 4–5, RFC-0001)

`cmd/ark-server` (internal/server) is the authoritative service: one SQLite
database per repository (internal/server/repodb), persisted to GCS as
`repos/<id>.db` with the object generation as a compare-and-swap, or to a
local directory in dev/tests. Records are JSON documents with a per-repo
revision counter; `field_revisions` powers spec §10.4 field-level merges
(title/body overlap → conflict; other overlaps → cloud wins);
`applied_mutations` makes pushes idempotent, which is also what makes lost
CAS races safe to replay — Update handlers rerun their whole closure on
retry, so they must be idempotent and reset their accumulators. The client
(internal/sync + internal/cloud) pushes the mutation queue, uploads artifact
blobs via signed URLs, then pulls records after its cursor and upserts them
(internal/store/sync.go) with deferred FK checks. Server-assigned display
numbers can be rewritten on collision — the ULID is authoritative, so local
numbers are indexed but not unique. Tokens resolve ARK_TOKEN → OS keyring
(macOS Keychain, Windows Credential Manager, Secret Service, via
zalando/go-keyring — pure Go) → ~/.ark/credentials.toml, never live in the
repository, and never reach another process's argv. A keyring that fails
warns on stderr before the fallback; see spec §20.

## This repository is not itself managed by Ark

Ark bootstraps on GitHub and has no `.ark/` of its own — deliberately. See
**"How Ark's own work is tracked"** in the README for why. What it means here:

- `ark status`, `ark task`, and every other record command **fail in this
  repository** with "no `.ark` directory found". Expected; do not `ark init`.
- File Ark feature requests and bugs as **GitHub issues on `elk-work/ark`**
  (`gh issue create -R elk-work/ark`), not as Ark tasks.
- To exercise Ark for real, use a repo that carries `.ark/` — **scout, signal,
  pulse, watch, or tailor** (`ark -C pulse status`). `pulse` is the smallest.

## Landing a PR

Green CI, not a draft, no `hold` label ⇒ it lands — the fleet-wide standard is
[elk/docs/pr-flow.md](https://github.com/elk-work/elk/blob/main/docs/pr-flow.md).

## What is deliberately absent (V1)

Workspaces, projects, milestones, a web UI, hosted Git, a custom merge
engine, full `gh` parity. Do not add primitives without a demonstrated
need (principle 005).

**Multi-user authorization is absent, but not deliberate.** V1 authorizes
with one bearer token, and `docs/rfc-0003-elk-issued-credentials.md` —
accepted 2026-07-28, unimplemented — replaces it with per-principal
credentials and per-repository `read`/`write`/`admin` grants. Scoped as
elk-work/ark#43, #52, #53 and #54. `internal/server/repometa.go` and
`internal/server/write.go` already carry comments naming where that check
goes; put it there when it lands rather than inventing a grant system
beside them.

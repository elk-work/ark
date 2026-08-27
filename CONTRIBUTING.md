# Contributing to Ark

Thanks for helping build Ark. This document is the short path to a good
first PR.

## Ground rules

- **`docs/v1-spec.md` is the contract.** Behavior changes need a spec edit in
  the same PR, or an RFC (`docs/rfc-NNNN-*.md` — RFC-0001 is the precedent).
- **`docs/principles.md` governs design.** In particular: do not rebuild Git;
  everything worth remembering is a record; sync intent, not rows.
- **Invariants** (breaking any of these is a bug, whatever the tests say):
  - Every record mutation writes its mutation-log row **in the same SQLite
    transaction**, and updates FTS where applicable.
  - Comments and reviews are append-only; they never conflict.
  - No CGO. The build stays pure Go (modernc.org/sqlite), portable, static.
  - `--json` output shapes and exit codes are stable interfaces for agents;
    changing them is a spec change.
  - Migrations are numbered, forward-only SQL in `migrations/`; never edit a
    shipped migration.

## Dev loop

```sh
go build ./...     # must pass
go test ./...      # self-contained: temp git repos, temp SQLite, no network
gofmt -l .         # must print nothing
go vet ./...       # must pass
```

Requirements: Go ≥ 1.26.5, `git` on PATH. No C toolchain needed.

On Windows, use PowerShell and a normal Git checkout. The repository's
`.gitattributes` keeps Go sources and the embedded Ark skill on LF regardless
of the developer's global `core.autocrlf` setting. The same commands above run
unchanged in PowerShell. See [docs/windows.md](docs/windows.md) for the full
Windows setup and local-server smoke test.

Run a single test: `go test ./internal/store -run TestName -v`

Working on sync? The tests need no server, but to exercise a client
against a real one, run `cmd/ark-server` locally — one binary, one
directory, no cloud account:
[docs/self-hosting.md](docs/self-hosting.md) § Mode 1.

## Making a change

Typical shape of a new command:

1. Store method in `internal/store` — one transaction: record + mutation row
   + FTS.
2. Cobra command in `internal/cli`; human table + stable `--json` via
   `internal/output`.
3. Typed errors from `internal/records` so exit codes stay correct
   (0 ok, 1 general, 2 invalid input, 3 not found, 4 conflict, 5 permission,
   6 offline, 7 partial).
4. Tests beside the code you touched.

Adding a migration: drop `000N_name.sql` in `migrations/` (embedded
automatically); mirror any server-side schema need in
`internal/server/schema/schema.sql`.

## PRs

- Branch from `main`; PRs required; CI must pass; ijroth reviews.
- Commit messages use area prefixes as in history: `records:`, `store:`,
  `cli:`, `server:`, `sync:`, `docs:`, `chore:`.
- Keep PRs one-topic. Spec edit rides along when behavior changes.
- First-time contributors: the CLA bot will ask you to sign the
  [CLA](CLA.md) with a single PR comment.

## Questions

Open an issue, or a task in the repo's own ark (`ark task list`) if you have
sync access — we dogfood. You do not need access to the maintainers'
service for any of the above; point a client at your own
([docs/self-hosting.md](docs/self-hosting.md)) or stay entirely offline.

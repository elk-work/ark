# Ark V1 Implementation Specification

## 1. Purpose

Ark V1 is a local-first engineering work record system that sits beside Git.

Git remains the source of truth for source code.

Ark stores the durable work around the code:

- tasks
- comments
- agent threads
- agent runs
- pull requests
- reviews
- artifacts

Ark V1 should be useful as a standalone local tool before the cloud sync layer is complete.

The first implementation should favor:

- clear behavior
- compatibility with Git
- a small schema
- deterministic sync
- simple Go code
- few dependencies

---

## 2. V1 Scope

Ark V1 must support:

1. Initializing Ark inside an existing Git repository
2. Creating and listing tasks
3. Adding comments to tasks
4. Creating agent threads
5. Recording agent runs
6. Attaching artifacts
7. Creating pull-request records linked to Git branches and commits
8. Recording reviews
9. Persisting all local state in SQLite
10. Synchronizing records with a cloud API
11. Resolving common metadata conflicts
12. Providing a CLI suitable for humans and coding agents
13. Providing a limited GitHub CLI compatibility mode

Ark V1 does not need:

- a web UI
- hosted Git repositories
- a custom merge engine
- workspaces
- projects
- milestones
- real-time collaboration
- arbitrary plugins
- a generic workflow engine
- a full GitHub API clone
- active-active database replication

---

## 3. User Model

Ark V1 supports two actor types:

- human
- agent

Every created record must include:

```text
actor_id
actor_type
```

An agent record should also include, where available:

```text
agent_name
agent_version
delegated_by
```

V1 may use locally generated actor IDs.

Cloud sync may later map those IDs to authenticated identities.

---

## 4. Repository Layout

Ark lives beside Git.

```text
repo/
  .git/
  .ark/
    ark.db
    config.toml
    objects/
    tmp/
```

### `.ark/ark.db`

Local SQLite database.

### `.ark/config.toml`

Repository-level configuration.

Example:

```toml
version = 1
repository_id = "01J..."
remote = "https://ark.example.com"
default_actor_id = "01J..."
default_actor_type = "human"
```

### `.ark/objects/`

Optional local cache for artifact files.

The database stores artifact metadata and checksums.

### `.ark/tmp/`

Temporary files created during uploads, downloads, or artifact processing.

---

## 5. Record Model

Every Ark entity is a record.

Every record must contain:

```text
id
repository_id
record_type
created_at
created_by
created_by_type
supersedes_id
deleted_at
```

Most records also contain:

```text
updated_at
version
sync_state
server_revision
```

### Immutability Rule

Ark should prefer append-only records.

When a record must change, V1 may update mutable fields in place locally, but it must also write a mutation record describing the change.

Submitted reviews and published comments should be treated as immutable in V1.

Corrections should create new records that reference the original.

---

## 6. Entity Definitions

## 6.1 Repository

Represents one Git repository.

Fields:

```text
id
name
path
git_remote_url
default_branch
created_at
```

The repository ID is generated when `ark init` runs.

The sync service keeps its own copy of `name`, `default_branch` and
`git_remote_url` — the values a human reads when a repository is listed or
recovered. It is not a record: nothing pulls it, and it has no `created_by`.

**Registration only ever backfills that copy.** The name a client sends is the
basename of wherever it happens to be checked out, so overwriting on every
sync let any client rename the repository for everyone, and blanked the remote
of a scratch checkout that had none. A value already on the service therefore
wins, and a client can only fill a field the service is missing.

Correcting a wrong value is a separate, deliberate act:
`ark repo set` over `POST /v1/repositories/{id}/metadata` (§19). It is
addressed by repository ID and never inferred from the working directory,
because that inference is what caused the overwriting bug.

---

## 6.2 Task

A task represents requested work.

Fields:

```text
id
repository_id
number
title
body
status
created_at
created_by
created_by_type
updated_at
version
sync_state
server_revision
```

Allowed status values:

```text
open
in_progress
blocked
done
closed
```

Task numbers are repository-local integers.

The UUID is authoritative.

The number is for display and CLI ergonomics.

---

## 6.3 Comment

A comment belongs to one record.

Fields:

```text
id
repository_id
parent_type
parent_id
body
created_at
created_by
created_by_type
supersedes_id
sync_state
server_revision
```

V1 parent types:

```text
task
pull_request
agent_run
review
```

Published comments are append-only.

Editing a published comment creates a new comment with `supersedes_id`.

---

## 6.4 Agent Thread

An agent thread is the durable conversation around work.

Fields:

```text
id
repository_id
task_id
title
status
created_at
created_by
created_by_type
closed_at
sync_state
server_revision
```

Allowed status values:

```text
open
closed
```

A thread contains messages.

---

## 6.5 Thread Message

Fields:

```text
id
thread_id
role
body
created_at
created_by
created_by_type
supersedes_id
sync_state
server_revision
```

Allowed roles:

```text
user
agent
system
tool
```

V1 stores text bodies only.

Structured tool calls may be stored as JSON in `metadata_json`.

---

## 6.6 Agent Run

An agent run records one execution.

Fields:

```text
id
repository_id
task_id
thread_id
agent_name
agent_version
status
input_summary
result_summary
started_at
finished_at
exit_code
branch_name
base_commit_sha
result_commit_sha
metadata_json
sync_state
server_revision
```

Allowed status values:

```text
queued
running
succeeded
failed
cancelled
```

Runs may exist without a resulting commit.

---

## 6.7 Pull Request

A pull request is an Ark record describing a proposed Git change.

Fields:

```text
id
repository_id
number
task_id
title
body
status
base_branch
head_branch
base_commit_sha
head_commit_sha
merge_commit_sha
created_at
created_by
created_by_type
merged_at
closed_at
version
sync_state
server_revision
```

Allowed status values:

```text
open
merged
closed
```

Git owns the branches and commits.

Ark owns the review and collaboration state.

---

## 6.8 Review

Fields:

```text
id
repository_id
pull_request_id
state
body
commit_sha
created_at
created_by
created_by_type
sync_state
server_revision
```

Allowed states:

```text
comment
approve
request_changes
```

Submitted reviews are immutable.

A new review supersedes an earlier opinion.

---

## 6.9 Artifact

An artifact is a generated or attached file.

Fields:

```text
id
repository_id
parent_type
parent_id
name
media_type
size_bytes
sha256
local_path
storage_key
created_at
created_by
created_by_type
sync_state
server_revision
```

Artifact parents may be:

```text
task
agent_run
pull_request
review
```

Local artifacts should be content-addressed by SHA-256.

Recommended local path:

```text
.ark/objects/sha256/<first-two>/<full-hash>
```

Cloud artifacts should be stored in Google Cloud Storage.

---

## 6.10 Promotion

A promotion records that a version — a Git merge commit and/or an artifact
checksum — became active in an environment. It is the deployment anchor for
observability tooling.

Fields:

```text
id
repository_id
environment
service
merge_commit_sha
artifact_sha256
pull_request_id
activated_at
ended_at
metadata_json
created_at
created_by
created_by_type
sync_state
server_revision
```

At least one of `merge_commit_sha` and `artifact_sha256` must be set.

Creating a promotion ends the prior active promotion for the same
(environment, service): its `ended_at` becomes the new promotion's
`activated_at`, in the same transaction.

A promotion may also be ended explicitly without a successor.

---

## 7. SQLite Schema

Use SQLite with:

```sql
PRAGMA foreign_keys = ON;
PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
```

Recommended schema style:

- text UUID primary keys
- RFC3339 timestamps stored as text
- JSON stored as text
- explicit foreign keys
- soft deletion where needed

Required tables:

```text
repositories
tasks
comments
agent_threads
thread_messages
agent_runs
pull_requests
reviews
artifacts
promotions
mutations
sync_state
conflicts
actors
```

Indexes should cover:

```text
tasks(repository_id, number)
tasks(repository_id, status)
comments(parent_type, parent_id)
thread_messages(thread_id, created_at)
agent_runs(task_id, started_at)
pull_requests(repository_id, number)
pull_requests(repository_id, status)
reviews(pull_request_id, created_at)
artifacts(parent_type, parent_id)
mutations(status, created_at)
```

Add SQLite FTS5 for:

```text
tasks.title
tasks.body
comments.body
thread_messages.body
pull_requests.title
pull_requests.body
reviews.body
```

---

## 8. Mutation Log

Every local write must create a mutation.

Mutation fields:

```text
id
repository_id
record_type
record_id
operation
base_server_revision
payload_json
created_at
created_by
status
error_message
```

Allowed operations:

```text
create
update
delete
append
submit
```

Allowed statuses:

```text
pending
applied
rejected
conflict
```

Local state and mutation insertion must happen in the same SQLite transaction.

`base_server_revision` is the record's current `server_revision` at the moment
the change is made — 0 only for a `create`, or for a record that has never
synced. It is not a placeholder. §10.1 reads it as "the version this change was
made against", and §10.4 drops every field whose server-side revision is newer
than it, so a base that understates the record's revision silently discards the
change field by field while the mutation is still reported applied.

---

## 9. Sync Model

Cloud SQL is authoritative for shared metadata.

SQLite is the local working copy.

Sync has two directions.

## 9.1 Push

The client sends pending mutations in creation order, which it takes from the
mutation ULID and not from `created_at`. ULIDs are minted with monotonic
entropy, so they order strictly by creation even for two mutations logged in
one transaction; `created_at` is RFC3339Nano text, which SQLite compares byte
by byte and which therefore does not sort chronologically (`records.TimeCompare`).

Request:

```json
{
  "repository_id": "01J...",
  "client_id": "01J...",
  "mutations": []
}
```

Response:

```json
{
  "applied": [],
  "rejected": [],
  "conflicts": [],
  "server_revision": 1234
}
```

The server must process each mutation idempotently by mutation ID.

## 9.2 Pull

The client requests all accepted changes after its last known server revision.

Request:

```json
{
  "repository_id": "01J...",
  "after_revision": 1200
}
```

Response:

```json
{
  "records": [],
  "tombstones": [],
  "server_revision": 1234
}
```

The client applies pulled records inside one SQLite transaction.

---

## 10. Conflict Rules

## 10.1 General Rule

A mutation is safe when:

```text
base_server_revision == current_server_revision_for_record
```

If the server revision is newer, the server applies the entity-specific rule.

## 10.2 Comments and Thread Messages

Append-only.

New comments and messages do not conflict.

Corrections create new records.

## 10.3 Reviews

Submitted reviews are immutable.

A second review is a new record.

## 10.4 Tasks

Fields merge independently.

Rules:

- title changed on both sides: conflict
- body changed on both sides: conflict
- status changed on both sides: cloud wins unless one transition is invalid
- unrelated field changes: merge automatically

## 10.5 Pull Requests

Rules:

- title/body use task-style field merge
- base branch change requires explicit conflict
- head branch is read from Git before merge
- merged or closed cloud state wins
- merge must always be confirmed online

## 10.6 Artifacts

Artifacts are immutable by checksum.

Changing a file creates a new artifact.

## 10.7 Promotions

Only `ended_at` and `metadata_json` mutate after creation.

Concurrent updates never need a person: cloud wins.

## 10.8 Conflict Storage

The local client stores unresolved conflicts in:

```text
conflicts
```

Fields:

```text
id
record_type
record_id
mutation_id
base_json
local_json
remote_json
status
created_at
resolved_at
```

Allowed statuses:

```text
unresolved
resolved_local
resolved_remote
resolved_manual
abandoned
```

---

## 11. Git Integration

Use the installed Git CLI in V1.

Do not use libgit2 initially.

Reasons:

- fewer language bindings
- exact compatibility with local Git behavior
- easier debugging
- existing credentials and configuration continue to work

All Git commands must run with an explicit repository working directory.

Required operations:

```text
git rev-parse
git status
git branch
git show-ref
git fetch
git merge-base
git diff
git merge
git push
git worktree
```

Use `exec.CommandContext`.

Capture stdout, stderr, exit code, and duration.

Do not parse human-oriented Git output when a machine format exists.

Use flags such as:

```text
--porcelain
--format
-z
```

---

## 12. Pull Request Merge Flow

`ark pr merge <number>` must:

1. Require cloud connectivity
2. Push pending Ark mutations
3. Fetch the current remote Git refs
4. Reload the PR record from the server
5. Verify the PR is still open
6. Verify local permissions
7. Verify required checks, if configured
8. Verify the current head SHA
9. Compute mergeability with Git
10. Perform the configured merge strategy
11. Push the result
12. Mark the PR merged in Cloud SQL
13. Pull the updated Ark state

Supported V1 strategies:

```text
merge
squash
```

Rebase merge may be deferred.

Use push-with-lease behavior where appropriate.

If the Git push succeeds but the Cloud SQL update fails, write a local repair record and return a partial-success error.

A later sync or repair command must reconcile the PR by comparing the merge commit to Git history.

---

## 13. CLI

Binary name:

```text
ark
```

V1 commands:

```text
ark init
ark status
ark sync

ark repo show
ark repo set

ark task create
ark task list
ark task view
ark task edit
ark task close
ark task comment

ark thread create
ark thread view
ark thread message

ark run start
ark run finish
ark run list
ark run view

ark pr create
ark pr list
ark pr view
ark pr comment
ark pr review
ark pr merge
ark pr close

ark artifact add
ark artifact list
ark artifact get

ark conflict list
ark conflict view
ark conflict resolve
```

All commands should support machine-readable output:

```text
--json
```

Default output should be stable and easy for agents to parse.

Commands that create records should print the new record ID and human-readable number.

---

## 14. GitHub CLI Compatibility

Ark V1 should support a limited compatibility shim.

Suggested invocation:

```text
ark gh issue create
ark gh issue list
ark gh issue view
ark gh issue comment

ark gh pr create
ark gh pr list
ark gh pr view
ark gh pr comment
ark gh pr review
ark gh pr merge
```

Mapping:

```text
GitHub issue -> Ark task
GitHub PR    -> Ark pull request
```

Do not attempt full `gh` compatibility in V1.

The goal is to preserve common agent workflows.

### No separate `gh` shim

Earlier drafts of this section said a separate `gh` shim might later forward
supported commands to `ark gh`. It will not. The idea was evaluated in
elk-work/ark#44 and declined; this note replaces it so the spec stops implying
a plan.

Three findings settled it.

**Nothing pays the cost it would remove.** A shim earns its keep only if
something already reaches for `gh` when it means Ark records. Nothing does.
`gh` in this fleet means GitHub — code, pull requests, CI — in all ten
repositories, including the six whose *work record* is Ark. The two vocabularies
are kept deliberately distinct, down to the reference syntax (`ark:scout#13`
versus `elk-work/scout#124`). Principle 005 applies directly: do not add the
primitive.

**A drop-in would fail on contact.** Real `gh` takes `--json <fields>` and
`-R [HOST/]OWNER/REPO`. Here `--json` is a boolean and the repository is chosen
with `-C <dir>`, so the flags on essentially every real `gh` invocation are
rejected outright.

**Shadowing `gh` on `PATH` is the dangerous part.** `gh pr merge` merges on
GitHub and can trigger a deployment; `ark gh pr merge` merges a local branch.
Silently redirecting that is far worse than asking for two vocabularies. And in
a repository with no `.ark/` — where GitHub issues *are* the work record — a
shimmed `gh issue list` would exit 3, `no .ark directory found`, instead of
listing the issues.

`ark gh` remains as specified above. Reach it by typing `ark`.

---

## 15. Go Project Structure

Recommended module layout:

```text
cmd/
  ark/
    main.go

internal/
  app/
  cli/
  config/
  db/
  records/
  repository/
  task/
  comment/
  thread/
  run/
  pullrequest/
  review/
  artifact/
  mutation/
  sync/
  conflict/
  git/
  cloud/
  identity/
  output/

migrations/
  0001_initial.sql

pkg/
  api/
```

### Package Responsibilities

`internal/db`

- SQLite connection
- migrations
- transactions

`internal/git`

- Git CLI wrapper
- typed Git operations

`internal/records`

- shared record fields
- IDs
- timestamps
- validation

`internal/mutation`

- mutation creation
- pending queue
- mutation state changes

`internal/sync`

- push
- pull
- conflict dispatch

`internal/cloud`

- HTTP client
- authentication
- API DTOs

`internal/output`

- terminal formatting
- JSON output

---

## 16. Recommended Go Dependencies

Keep dependencies limited.

Suggested:

```text
github.com/spf13/cobra
github.com/mattn/go-sqlite3
github.com/oklog/ulid/v2
github.com/BurntSushi/toml
```

Optional:

```text
github.com/stretchr/testify
```

Use the standard library for:

- HTTP
- JSON
- file I/O
- SHA-256
- process execution
- context
- logging

If CGO is undesirable, use a pure-Go SQLite driver instead.

The driver choice should be made early because it affects cross-compilation.

---

## 17. IDs and Time

Use ULIDs for Ark record IDs.

Reasons:

- locally generated
- sortable
- compact enough for CLI use
- no coordination required

Store full ULIDs.

Allow unambiguous prefixes in CLI commands.

Use UTC internally.

Store timestamps in RFC3339Nano format.

---

## 18. Transactions

Every local command that mutates state must use one SQLite transaction.

The transaction must include:

1. record change
2. mutation insertion
3. related index updates

Artifact file writes should use:

1. write temporary file
2. fsync if needed
3. atomic rename
4. commit metadata transaction

If metadata commit fails, remove the unreferenced object later through garbage collection.

---

## 19. Cloud API

V1 cloud service may be a separate Go service.

Minimum endpoints:

```text
POST /v1/repositories
POST /v1/sync/push
POST /v1/sync/pull
GET  /v1/repositories/{id}
POST /v1/repositories/{id}/metadata
GET  /v1/repositories/{id}/records/{type}/{id}
POST /v1/artifacts/upload-url
POST /v1/artifacts/download-url
POST /v1/pull-requests/{id}/merge
```

Plus the authenticated work-record write routes, so a program can write a
record without being a copy of Ark — see
`docs/rfc-0004-work-record-write-api.md`:

```text
POST /v1/repositories/{id}/tasks
POST /v1/repositories/{id}/comments
POST /v1/repositories/{id}/tasks/{id}/status
```

Cloud stack:

```text
Cloud Run
Cloud SQL for PostgreSQL
Google Cloud Storage
```

The API service owns:

- authentication
- authorization
- server revisions
- mutation idempotency
- conflict checks
- signed GCS URLs

## 19.1 Repository Metadata

`GET /v1/repositories/{id}` returns the service's copy of the repository
record (§6.1): `id`, `name`, `default_branch`, `git_remote_url`, `revision`,
`created_at`. `ark status` reports what the local checkout knows; this is the
only way to see what the service holds.

`POST /v1/repositories/{id}/metadata` corrects it — the one path that
overwrites these fields, since registration can only backfill:

```json
{
  "writer":         { "agent_name": "ark-cli", "delegated_by": "01J8Z..." },
  "name":           "optional",
  "default_branch": "optional",
  "git_remote_url": "optional"
}
```

- **Only the fields present are asserted.** Each is nullable, so omitting one
  and clearing one are different requests; a partial update never sends back
  values the caller did not mean.
- **`git_remote_url` is the one field an explicit `""` clears.** A repository
  can genuinely have no remote, and refusing would leave a wrong non-empty
  URL uncorrectable. `name` and `default_branch` cannot be emptied.
- **Validation** is `400 validation`: a blank name, a branch name
  `git check-ref-format` would refuse, or a remote that is not a URL,
  `[user@]host:path`, or an absolute path. Values are trimmed.
- **A change mints a revision**, in the same transaction, so the repository's
  counter orders it as it orders any other write. Setting a field to the
  value it already holds does not: the response carries `changed: false` and
  the current revision, the reasoning the task-status route uses for a
  transition that asks for the status a task already has.
- **`Idempotency-Key` is honoured but not required**, for the same reason —
  this is an assertion about state, not an increment.
- **The writer** is resolved exactly as the RFC-0004 write routes resolve it,
  and for the same purpose: the authorization rule, not attribution, since
  the record carries no `created_by`. It follows that the acting identity
  must already be known to the service, which it is once the repository has
  pushed.
- **Authorization is the single service token** (§20). Renaming a repository
  is an `admin`-level act under RFC-0003, and when its per-repository grants
  land the check belongs beside the writer resolution in the handler. V1 has
  one token and no level to check against, so there is nothing to enforce
  yet.

---

## 20. Authentication

V1 may begin with one user and one service token.

The client stores credentials outside the repository.

Do not store tokens in `.ark/config.toml`.

Token resolution is ordered, and the order is fixed:

1. the `ARK_TOKEN` environment variable
2. the OS keyring — macOS Keychain, Windows Credential Manager, or the
   freedesktop Secret Service — keyed by the sync service's host, so one
   login covers every repository pointing at that service
3. a local credential file with restricted permissions, as a fallback only

A token must never be passed to another process as a command-line argument.
The process table is readable by every user on the machine.

The keyring is not optional where the platform has one. If the keyring is
unavailable — absent, locked, denied, or a platform without one — the client
must say so on the standard error stream **before** it falls back to the
file. Degrading to plaintext in silence is not permitted. A keyring that
answers and simply holds no entry for the host is a miss, not a failure, and
is not announced.

The fallback file is restricted to the current user before any token is
written into it: mode 0600 on Unix, a DACL granting only the current account
on Windows. Once the keyring holds a host's token, the client removes that
host's entry from the fallback file — the keyring outranks it, so the copy
would be a secret on disk that nothing will ever read.

`ARK_NO_KEYRING` skips the keyring entirely, for a machine where the file is
the deliberate choice. Storage then goes straight to the file, without a
warning, because the operator asked for it.

Commands report which store answered. `ark login` names where it wrote the
token; `ark status` names where the token resolved from. Neither prints the
token itself (§21).

The cloud service must identify:

```text
principal
actor
repository permission
```

Agent actions should record both the agent identity and the delegating principal.

---

## 21. Logging and Diagnostics

Ark should write human-readable errors by default.

Add:

```text
--verbose
--debug
```

Debug logs should include:

- command
- duration
- Git exit code
- HTTP status
- mutation IDs
- repository ID

Do not log:

- access tokens
- full artifact contents
- secret environment variables

---

## 22. Error Model

Define typed errors for:

```text
not found
validation
conflict
permission denied
offline
Git failure
database failure
partial success
```

CLI exit codes:

```text
0 success
1 general failure
2 invalid input
3 not found
4 conflict
5 permission denied
6 offline or remote unavailable
7 partial success requiring repair
```

Exit code 2 covers **every** way the command line can be wrong, not only the
checks Ark performs itself: an unknown command or subcommand, an unknown flag,
a missing required flag, a bad flag value, and the wrong number of arguments.
The caller cannot see which layer rejected the input, so the layers must not
disagree — `unknown flag` and `invalid status` are one class of mistake from
outside. A command group invoked with an unrecognised subcommand is an error,
never a help screen with a success code; invoked bare, it prints help and
succeeds.

---

## 23. Testing

## 23.1 Unit Tests

Cover:

- record validation
- mutation creation
- conflict rules
- ULID prefix resolution
- Git output parsing
- CLI JSON output

## 23.2 Integration Tests

Use temporary Git repositories and temporary SQLite databases.

Cover:

- init
- task lifecycle
- thread lifecycle
- agent run lifecycle
- PR creation
- artifact storage
- sync push/pull
- conflict creation and resolution
- Git merge flow

## 23.3 Cloud Tests

Use a disposable PostgreSQL database.

Fake or emulator-backed GCS is acceptable initially.

## 23.4 End-to-End Test

Required V1 path:

1. initialize repository
2. create task
3. create thread
4. start agent run
5. create branch
6. commit change
7. finish run
8. create PR
9. review PR
10. merge PR
11. sync from another client
12. verify full history is visible

---

## 24. Migration Strategy

Use numbered SQL migrations.

Example:

```text
migrations/
  0001_initial.sql
  0002_add_artifact_metadata.sql
```

Store applied migration numbers in:

```text
schema_migrations
```

Migrations must be forward-only in production.

Development tooling may recreate databases.

---

## 25. V1 Delivery Sequence

### Phase 1 — Local Records

Implement:

- `ark init`
- SQLite
- tasks
- comments
- JSON output
- FTS search

### Phase 2 — Agent History

Implement:

- threads
- messages
- agent runs
- artifacts

### Phase 3 — Git and Pull Requests

Implement:

- Git wrapper
- PR records
- reviews
- local merge preparation

### Phase 4 — Cloud Sync

Implement:

- Cloud API
- mutation push
- record pull
- revisions
- conflict handling

### Phase 5 — Merge and Repair

Implement:

- cloud-confirmed PR merge
- partial-success handling
- repair command

### Phase 6 — Compatibility

Implement:

- `ark gh issue`
- `ark gh pr`
- stable agent-facing JSON

---

## 26. V1 Acceptance Criteria

Ark V1 is complete when:

- A repository can be initialized without changing Git behavior
- A user or agent can create tasks locally while offline
- Threads and runs can be linked to tasks
- Runs can reference commits and branches
- Pull requests can reference real Git objects
- Reviews and comments can be recorded
- Artifacts can be stored locally and uploaded to GCS
- Local records can synchronize to Cloud SQL
- A second client can pull the same records
- Common non-overlapping edits merge automatically
- conflicting task edits create an explicit conflict record
- PR merge always checks current cloud and Git state
- all major commands support JSON output
- the full end-to-end test passes

---

## 27. Starting Opinion

Ark V1 should be a useful local tool with a cloud sync path.

It should not begin as a distributed platform.

The important thing is to get the record model, local behavior, Git boundary, and mutation log right. Those are the parts the rest of the system will depend on.

Everything else can evolve from there.

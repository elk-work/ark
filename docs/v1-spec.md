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

§6.0 defines the actor record itself, including how a named agent resolves to
one and why `delegated_by` is part of that identity rather than a note on it.

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

`sync_state` is one of `local`, `synced`, or `diverged`. `diverged` means the
service refused a change to this record and the local copy kept it — see
§9.1, which is also where it clears.

### Immutability Rule

Ark should prefer append-only records.

When a record must change, V1 may update mutable fields in place locally, but it must also write a mutation record describing the change.

Submitted reviews and published comments should be treated as immutable in V1.

Corrections should create new records that reference the original.

---

## 6. Entity Definitions

## 6.0 Actor

An actor is the identity every other record's `created_by` names. It comes
first because nothing in §6.1–§6.10 exists without one.

Fields:

```text
id
type
name
email
agent_name
agent_version
delegated_by
created_at
```

`type` is `human` or `agent`. `ark init` creates the repository's default
human from the Git identity; a pull brings in the actors other clients
introduced; an agent actor is registered on a named agent's first run.

An agent also carries `agent_name` and a `delegated_by` naming the human whose
authority it acted under. That field is what lets a consumer resolve an
agent's work back to a person, which is the one thing Ark records that a Git
host does not.

**An agent identity is per (agent name, delegating human).** `--agent
claude-code` resolves to the lowest-ULID agent actor whose `agent_name` is
`claude-code` *and* whose `delegated_by` is the human this invocation acts for
— `ARK_DELEGATED_BY`, or the repository's default actor. The first run for
that pair registers it.

The delegating human is part of the key because actors are shared. Every sync
uploads every actor a client holds (§9.1) and a pull brings back the ones other
people introduced, so a repository's actor table contains other developers'
agents. Resolving on `agent_name` alone therefore converged two developers who
both ran `--agent claude-code` onto a single actor — whichever registered
first — and from then on one of them wrote records under an identity
delegating from the other person.

So a repository legitimately holds **several agent actors with the same
`agent_name`**, one per delegating human. Two `claude-code` actors in one
repository is the correct state, not a duplicate: they are two identities,
distinguished by `delegated_by` rather than by name. Nothing that renders a
name changes — `ark task list` and `ark run list` show `claude-code` either
way — and `--json` carries the actor ULID that tells them apart.

`delegated_by` must name a human actor the repository already holds. An
invocation whose delegation is absent, dangling, or names an agent is refused
before it writes anything, because an agent cannot invent the authority it
claims to act under.

**The service resolves a remote writer by the same key**, on the §19 write
routes and on `POST /v1/repositories/{id}/metadata` — the request's `writer`
names an agent and a delegating human, and the pair is what selects or
registers the actor the write is attributed to
(`docs/rfc-0004-work-record-write-api.md`, Decision 2). It has to be the same
key, because the two are one stated equivalence: while the service keyed on
the name alone, the same `--agent claude-code` resolved to one actor through a
local write and possibly another through a remote one, and every person using
the CLI writes remotely through the same `ark-cli` name, so the second person
to run `ark repo set` in a repository was attributed to the first.

Two things differ remotely, because a request is not a local database. The
delegation is checked on **every** write rather than only where an agent is
registered — it is half the lookup key, so a check at registration alone would
leave the reuse path as open as the name-only lookup was. And the human a
request may name is governed by the actor binding (§19.2): one bound to
another principal is refused with `permission`, which is what stops a request
from asserting a delegation it was not given. The stored actor record is never
rewritten from a request either way, so nothing can re-point a registered
agent at a different human.

**Changing that key re-attributes nothing.** Records already written name the
actor they named. A client that upgrades finds no actor for its (name, human)
pair and registers one, which is how a shared repository comes to hold two
actors of one name; everything written under the shared actor keeps pointing
at it. Who actually did that work is not something Ark can decide afterwards,
and guessing would be a worse error than the one being fixed.

---

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

It is also **not durable across a rebuild**. The service reassigns a number
another record already holds, so a repository rebuilt from a client's mutation
log (§9.3) can come back numbered differently. A reference written down outside
Ark — in a commit message, an RFC, another repository's docs — survives only if
it carries the ULID.

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
deferred_records
```

`deferred_records` holds pulled records whose referents have not arrived
(§9.2). It is the one table with no foreign key of its own, deliberately: the
reference it records as unresolvable can be the repository, and a ledger of
unresolvable references that cannot be written when one does not resolve would
reintroduce the failure it exists to prevent.

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
mutations(repository_id, status, resolved_at)
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
resolved_at
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

A `rejected` mutation is never deleted. It is the only durable evidence that
the local database holds a change the service refused, and the queue cannot
carry that evidence because the rejection is what removes the mutation from
the queue. `error_message` holds the service's reason; `resolved_at` is NULL
while the disagreement stands and is stamped when the record next reaches
agreement with the service — an accepted mutation against it, or a pull that
brings the service's copy down. A rejection that is never resolved never stops
being reported (§9.1).

`base_server_revision` is the record's current `server_revision` at the moment
the change is made — 0 only for a `create`, or for a record that has never
synced. It is not a placeholder. §10.1 reads it as "the version this change was
made against", and §10.4 drops every field whose server-side revision is newer
than it, so a base that understates the record's revision silently discards the
change field by field while the mutation is still reported applied.

It is also **only meaningful within one history**. A recorded base counts
revisions of the service that issued it, so after a history reset (§9.2) every
base in the log names a revision of a database that is gone, and comparing it
to the live counter is comparing two scales. A replay therefore re-derives the
base rather than carrying it over; §9.3 has the rule.

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
  "actors": [],
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

**A push is not the only carrier of actors, and must not be the only one.**
Actors also travel on `POST /v1/repositories` (§19), which is the part of a
sync that runs unconditionally. A push does not run at all when the queue is
empty, so a repository that had registered and synced but never pushed used to
hold no actor records on the service — not even the human `ark init` creates.
Every write route resolves its writer against those records and admits a new
agent only under a `delegated_by` naming a human the service already holds, so
the first remote write into such a repository was refused, naming a field the
caller never supplied (elk-work/ark#47). Both carriers upsert idempotently and
mint a revision only for an actor the service has not seen, so repeated
registration stays quiet.

### Rejections

A rejected mutation leaves the queue and is never retried. That much is
correct: a write against a record the service does not hold cannot be made to
succeed by repeating it. What follows from it is the part that binds.

- **The local effect is kept, not rolled back.** A mutation payload is a delta
  of changed fields and carries no before-image, so there is nothing to revert
  to without inventing a prior value. More importantly, the commonest rejection
  is `record not found`, where the *service* is the side missing data —
  reverting would destroy the only copy of a real decision in order to agree
  with a peer that has never heard of the record. Ark keeps the change and
  reports the disagreement; a person decides which side is right.
- **The rejection is durable.** The mutation row survives with
  `status = 'rejected'` and the service's reason (§8), and the affected record
  is marked `sync_state = 'diverged'` so the disagreement is visible to a
  reader of the record and not only to a reader of the sync log. Both clear
  when agreement is restored, and only then.
- **`ark status` must report outstanding rejections.** A client that has
  diverged from the service may not describe itself as in sync. This is not a
  presentation detail: because a rejection empties the queue, `pending
  mutations` reaches zero at the exact moment the two copies stop agreeing, and
  a client once reported `0 pending mutations` about a service that had just
  refused three writes (elk-work/ark#46).
- **A sync with rejections exits 7**, not 0 (§22). The transfer succeeded; the
  repository came out of it in a state needing repair. Conflicts remain exit 0
  — they are a designed state with a resolution path that `ark status` has
  always named, so a conflicting sync was never claiming to be in sync.

### Referential integrity

**The service does not enforce it on push, and stores what it accepts either
way.** A create is applied whether or not the service holds the record its
payload points at. The asymmetry with an update is deliberate: an update has
to find the record to have something to merge into, so `record not found` is
the only answer available to it, while a create carries a whole document and
needs nothing already present.

What makes the strict rule wrong here is ordering. Mutations are ordered by
ULID *within* a push, but nothing orders two clients' pushes against each
other, so the record a new child names can legitimately still be sitting in
another client's queue. Refusing the child would convert that skew — which
ends on its own, usually within a sync interval — into a rejection, and a
rejection is permanent, terminal and loud (above). The service would be
destroying a real write to enforce an invariant that was about to hold.

**What it guarantees instead is that it says so.** Every accepted create is
checked against each reference its record type carries (§6), and one the
service cannot resolve is written to `dangling_references` in the same
transaction: the child, the field that referred, the record it named, the
mutation that carried it, and when it was first seen. The service can no
longer do what it did in elk-work/ark#56 — reject three tasks as `record not
found` and store three comments on those same tasks, in one transaction,
leaving orphans that no client can render and saying nothing about them.

The set that matters is the outstanding one, and it is a comparison rather
than a stored state:

```sql
SELECT d.* FROM dangling_references d WHERE NOT EXISTS (
  SELECT 1 FROM records r
  WHERE r.record_type = d.parent_type AND r.record_id = d.parent_id);
```

**It self-clears, and needs no `resolved_at` to do it.** A client-side
rejection carries that column because the client cannot see the service's copy
and so cannot tell on its own when the two agree again. Here both sides of the
question are one table away and records are never hard-deleted, so the
comparison is monotone and true whenever it is made. It is also the only form
that stays true: a record can be created by the mutation engine or by a write
route (§19), and a stamp written by one path and forgotten by the other would
leave the ledger naming an orphan that no longer exists. The entry survives
after the reference resolves, because that a repository sees this skew — and
how often — is worth knowing.

**An outstanding entry is a defect, not a statistic.** `comment.parent_id` and
`artifact.parent_id` are polymorphic and therefore carry no foreign key, so
those render as orphans and nothing worse. Every other reference is a
declared foreign key on the client, checked when the pull transaction commits
(`PRAGMA defer_foreign_keys` defers that check; it does not remove it), so a
client cannot store such a record the way the service does. Until
elk-work/ark#75 it did not try to do anything else either: pulling one failed
the entire pull — not that record, the pull — and left the cursor where it
was, so the next pull fetched the same batch and failed again. A client now
holds the record back and applies the rest of the batch (§9.2). That ends the
wedge, not the defect: the record stays invisible on every client until the
record it names arrives.

Two things this deliberately is not. It is not **quarantine** — holding the
child back until its referent lands and releasing it on pull — which is the
eventual answer and more work than V1 has spent here. And the ledger is not
yet **surfaced**: it lives in the repository database, so today an operator
reads it there and no push response, route or `ark status` line reports it
(elk-work/ark#74).

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

### Referential integrity

This is the client half of the policy in §9.1, and the two are opposite by
design: the service records a reference it cannot resolve and stores the
record anyway, while the client sets the record aside and applies everything
else. Neither side refuses the write, because on neither side is the record
wrong — it is early.

Every typed pointer between records is a declared foreign key —
`reviews.pull_request_id`, `thread_messages.thread_id`, `agent_runs.task_id`,
`agent_runs.thread_id`, `agent_threads.task_id`, `pull_requests.task_id`,
`promotions.pull_request_id`, and the `supersedes_id` of a comment or a thread
message. `comment.parent_id` and `artifact.parent_id` are polymorphic and
carry none. The pull transaction sets `PRAGMA defer_foreign_keys = ON`, which
moves those checks to `COMMIT` and does not remove them, so the client cannot
do what the service does and simply store the orphan: writing it fails the
commit, which fails the whole batch, which leaves the cursor where it was —
and the next pull then requests the same range, receives the same record, and
fails identically. That is a client permanently unable to sync with a
repository over a record living on the service that nothing local can remove
(elk-work/ark#75).

**The client holds such a record back instead.** This is the judgment §9.2
already makes about a record type the client does not know: a pull that cannot
represent one record must still apply the others and still advance, because
the alternative is not a partial sync but no sync at all.

- **The batch applies and the cursor advances.** Only the record whose
  reference did not resolve is set aside.
- **It is retried within the same batch, to a fixpoint.** Revision order is
  not dependency order — a parent can arrive after its own child in one
  response — so a single pass would hold back a child whose parent came with
  it. A record inserted earlier in the same transaction counts as present.
- **It is kept, not dropped.** The held record is stored with the reference
  that failed, and retried on every later pull, so it lands by itself once its
  referent arrives; no operator action ends this state. Keeping it is not
  optional: the cursor has moved past its revision, so the service will never
  send it again, and dropping it would trade a wedged client for one that
  quietly loses a record both sides hold.
- **It is reported separately from an unknown record type.** The two are
  opposite conditions with opposite answers — an unknown type means this build
  is older than its service and the answer is to upgrade; a missing referent
  means a record has not arrived and the answer is to wait — so a client must
  not report them as one count.
- **It is not a rejection and not a conflict.** Nothing has diverged: both
  sides hold the same record and one of them has not been delivered yet. It
  does not change the exit code of a sync (§22).

The set self-clears, like the service's ledger and unlike a history reset: a
held record is applied and forgotten on the first pull after its referent
lands, and a held record whose tombstone arrives is forgotten too. So a
non-empty set is always a current statement about what this checkout cannot
yet see, which is why it needs no `resolved_at` — the record and its referent
are one lookup apart, and the comparison is true whenever it is made.

**`ark status` reports the count**, as `held_records` in `--json` — unset when
there are none — and as its own line in the human rendering, worded as waiting
rather than as divergence. Self-clearing is why the line is phrased that way;
it is not why the line can be omitted. The set drains only if the referent
arrives, and the service accepts a child whose parent it does not hold by
decision (§9.1), so a held record is the client-side face of a dangling
reference the service has already recorded. Where that parent is never coming,
this count is the only thing on the client that says so
(elk-work/ark#89).

**The check reads its references from the schema**, not from a list of them
kept beside it. A written list is a second copy of the schema, and the way a
second copy fails is that a migration adds a foreign key to the first and
nobody adds it to the second — at which point that reference goes unchecked,
the record is written, and the commit fails exactly as it did before, with the
check that was meant to prevent it appearing to have passed.

### The cursor is a high-water mark

`sync_state.last_revision` never decreases. A repository's `server_revision` is
a counter the service only ever increments, so a pull answering with a lower
value is not the client being ahead of a slow service — it means the service is
**not serving the history this client was tracking**, because the repository's
database was reset, lost, or restored from before that point.

The client must detect this and report it:

- It is one comparison, on data the client already has, on every sync.
- **The cursor does not follow the service down.** Assigning the response's
  revision is what erases the evidence: after one such sync neither side
  remembers there was ever a higher revision.
- **It is recorded durably**, with the revision each side was at and when it
  was first seen — not as derived state, which stops being true as soon as the
  client resumes pushing and the service's counter climbs back past the old
  mark without anything having been recovered.
- **`ark status` reports it, and `ark sync` exits 7.** This is the third state
  `ark status` must be able to tell apart, and the only one whose answer has
  ever been that records are gone. It is not derivable from the other two: a
  repository once sat absent from the service for six weeks with every local
  signal correct — nothing pending, nothing rejected, seventeen mutations all
  acknowledged by a service that no longer held the result (elk-work/ark#58).
- **Ark does not reconcile it.** Which side is authoritative is a judgment
  about which records matter, and unlike a rejection this state does not clear
  itself: no comparison the client can make will tell it a person has decided.

## 9.3 Repair

A reset leaves the client holding records the service acknowledged and no
longer has, in `.ark/ark.db`, along with the mutation log that produced them —
the intents, in order, with `created_by` and `created_at` intact. Every one of
those mutations is `applied`, which is exactly why nothing re-sends them: a
mutation leaves the queue when the service acknowledges it, and the service
that acknowledged these no longer exists as far as the data goes.

`ark repair push` replays that log into the service. It is **the judgment of
§9.2 carried out, not a substitute for it**: Ark still does not decide which
side is authoritative, it gives a person a way to act once they have.

**It is gated twice, and neither gate is decoration.** It refuses unless a
reset is recorded, and it previews unless `--confirm` is passed. The command
re-asserts one checkout's whole history at a service other clients are also
using; no shape of it should be reachable from something that runs on a timer,
which is also why it is not a flag on `ark sync`.

The sequence is:

1. **Rewind the pull cursor to zero.** A repair is the only thing that may
   lower the high-water mark, and it must. After a reset the mark is a
   position in a history nobody serves — a checkout at revision 18 asking a
   service at revision 4 for everything after 18 gets an empty answer, and
   goes on getting one until the service climbs past 18. Without the rewind
   the pull in step 3 returns nothing, and the repair would replay over a
   repository it had never looked at. Nothing is lost by rewinding, because
   §9.2 records the event rather than the cursor.
2. **Register**, now asserting a cursor of zero. That is what lets the service
   create the repository where it has lost it (§19), and it is a no-op where
   the service merely rolled back. One path covers both shapes.
3. **Pull.** A repair must not delete what the service still holds, and after
   a reset it usually holds something — actors from its own re-registration,
   work another client has pushed since. Pulling before pushing merges those
   records into this checkout, and it is also the only truthful source of a
   revision for each of them, which step 4 needs.
4. **Re-queue every mutation the service has ruled on**, in ULID order,
   rebased. Below.
5. **Push, upload artifact blobs, pull.** An ordinary sync from there, with
   one difference: a blob's storage key is ignored rather than treated as
   proof the service holds it. The `blobs` table lives in the repository
   database, so a service that lost the repository lost its record of every
   blob with it; content addressing keeps the re-offer cheap.
6. **Clear the recorded reset — only on a clean run.** A push with rejections,
   or a pull still answering below this checkout, leaves it in place, because
   the repository still needs somebody. This is the only condition that may
   retire the mark.

### Rebasing a replayed mutation

**This is the part that loses data quietly if it is got wrong.**
`base_server_revision` is read against the service's per-field revisions
(§10.4), and after a reset the recorded bases count a history nobody serves.
Replaying with them reads one scale against another, and it fails in both
directions: a base below the record's current revision drops fields silently
while reporting the mutation applied (§8, elk-work/ark#28), and a base above it
skips the merge check altogether, so the replay overwrites whatever the service
does still hold — the common shape, because the dead history was long and the
live one is short or empty, and the worse one for a command whose first duty is
not to delete the survivors.

So a repair re-derives the base rather than adjusting it. Step 3 means the
client has seen exactly what the service holds, and:

- **A record that pull returned** takes the revision it returned. That is what
  this checkout has seen of the live history for that record, and it is true.
- **A record it did not return** takes 0 — the service has never written a
  revision of this record, because it has not.
- **A create takes 0** either way (§8), and the server does not read it.

Nothing depends on the client guessing the right number for its own replayed
edits. A create replayed in the same push publishes its new revision into that
push's session, which lifts every later mutation for the same record to it, so
a create-then-edit sequence replays with the merge check correctly skipped at
each step. It is the same mechanism that applies a burst of offline edits in
order, and a replay is that burst against a service that forgot it.

### Which mutations replay

Those the service has ruled on: `applied` and `rejected`. A refusal issued by a
history that no longer exists is not a verdict about the service being talked
to now, and a rejection's local effect is kept rather than rolled back (§9.1),
so the client still holds that change and a rebuild that left it out would be
quietly partial.

The two are re-queued differently, and the server's idempotency rule is what
forces it. §9.1 has the service keep the outcome it reached for a mutation ID,
which is what makes an accepted replay converge — and equally what makes a
refusal permanent for that ID. So an **applied** mutation is re-queued in
place, keeping the ID that makes it idempotent, while a **rejected** one is
re-submitted under a fresh ULID carrying the same intent, `created_at` and
`created_by`. That is not a workaround: §9.1 says a rejected mutation is never
retried, and §10.8's resolution path already re-submits a refused change as a
new mutation. The rejected row is left untouched, as §8 requires.

Conflicts do not replay. They are a decision waiting on a person, with
`ark conflict resolve` to carry it out (§10.8).

### Several clients

Replay is idempotent by mutation ID, and a re-queue does not change an ID, so
two replicas replaying one history converge on a single copy of each record and
the second mints no revision. Nothing orders one client's replay against
another's, though, so an update to a record whose author has not replayed yet
is refused as `record not found`. That is loud, it leaves the reset recorded,
and running the command again once the author has replayed is the way through.

### What a repair does not preserve

Display numbers. The service reassigns a task or pull-request number another
record already holds (§6.2), so a repaired repository can come back numbered
differently. The command says so before it acts and names each number that
moved afterwards, because an `ark:<repo>#N` written into a commit message or a
design doc is not stored in Ark and cannot be corrected by it. ULIDs do not
change.

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
ark repair push

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
POST /v1/principals
POST /v1/device/code
POST /v1/device/token
POST /v1/device/approve
POST /v1/repositories
POST /v1/sync/push
POST /v1/sync/pull
GET  /v1/repositories/{id}
POST /v1/repositories/{id}/metadata
GET  /v1/repositories/{id}/grants
POST /v1/repositories/{id}/grants
GET  /v1/repositories/{id}/records/{type}/{id}
POST /v1/artifacts/upload-url
POST /v1/artifacts/download-url
POST /v1/pull-requests/{id}/merge
```

The three `/v1/device/` routes are the device-authorization flow — how a
person reaches a credential without already holding one — and are the only
`/v1` routes besides `POST /v1/principals` that no bearer authenticates.
Possession of the `device_code` a caller was handed is the authentication on
two of them, and `ARK_IDP_KEY` on the third. They are absent from a service
with no `ARK_IDP_APPROVAL_URL`; §20.1 is the contract.

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

`POST /v1/principals` is the one route not authenticated by a bearer the
service issued or was configured with as `ARK_API_TOKEN`. It takes
`ARK_BOOTSTRAP_TOKEN` and mints a principal and its first credential, so a
deployment can reach a per-person credential without already holding one
(§20). Unset, the route refuses everything.

`POST /v1/repositories` registers a repository idempotently and carries this
checkout's actors alongside its metadata. It is the only call every sync makes
unconditionally, which is why identity travels on it and not on the push alone
(§9.1). Registration **backfills** metadata and never overwrites it —
correcting a field is a deliberate act with its own route (§19.1).

Registration is also **how a repository comes into existence**: it creates one
when the service holds no database for the ID. Nothing else could — but it is
only right for a client with no history to contradict it, so the request
carries the client's cursor.

```json
{
  "id":            "01KX9B83TF2FV51C6K04563FQ0",
  "name":          "scout",
  "last_revision": 33,
  "actors":        [ "..." ]
}
```

- **`last_revision` is the revision this checkout has already synced past**
  (§9.2). Above zero it asserts that this service issued the client history,
  so a service holding no database for the repository has **lost** it rather
  than never had it. Registration answers `404 not_found` and creates
  nothing; the message names the repository and the revision the client
  claims, because the person reading it has a missing database, not a typo.
- **Zero — or absent — creates.** A checkout that has never synced has no
  history to contradict, and a client built before this field omits it and
  gets the behaviour it has always had.
- **A registration that creates is logged**, distinguishably from the
  idempotent no-op that runs on every sync of every repository.

The refusal is `not_found` because that is already the service's answer on
`POST /v1/sync/pull` and `POST /v1/sync/push` while a repository's database is
missing (§9) — and because that answer is the only clean evidence the
repository was lost. It used to survive exactly until the next `ark sync`: the
first client to reach an absent repository re-created it empty, so an operator
investigating afterwards found a live, registered repository holding one actor
at revision 1 rather than a 404, and nothing recorded that a client had stood
it back up. A client that meets the refusal reports it as a history reset and
exits 7, exactly as it would have reported the same loss on the pull (§9.2,
§22).

**Missing covers two shapes, and they are one answer.** The object may be
absent, or it may be present and hold no repository row — which is what a
zero-length `repos/<id>.db` reads as, since SQLite opens one as a valid empty
database (§22). Every route a repository is reachable through answers both
with the same `404 not_found`, because it is the same loss and the client's
move is the same. The service names which shape in the message and in its log,
because the *operator's* move is not the same: one object was removed, the
other had zero bytes written over it. This is a property of the storage layer
rather than of any handler, and has to be — while the check lived in the
registration handler alone, pull and push answered `500 internal` for a
repository this route was already calling missing (elk-work/ark#85).

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
  synced — registration carries actors (§9.1). It used to say "once the
  repository has *pushed*", which was true and was the bug: a checkout that
  had synced but never pushed could not make this call at all.
- **Authorization is `admin` on the repository** (§19.2). Renaming a
  repository changes what it is called for everyone and nothing about it is
  recoverable from a client, which makes it the clearest `admin`-level act
  this service has. The check runs before the handler touches storage, so a
  caller with no authority costs no fetch, and it is separate from the writer
  resolution above: one is about the caller, the other about the identity the
  write is recorded under.

## 19.2 Grants

Authorization is **per repository, three levels, and nothing else**: `read`
pulls, `write` pulls and pushes, `admin` also grants, revokes, and corrects
metadata. There are no groups, teams, roles, or per-record permissions — a
principal with `read` on a repository reads all of it
(`docs/rfc-0003-elk-issued-credentials.md`, Decision 4; principle 005).

Every route names a repository, and every route consults the caller's level
for it before doing anything:

| Level | Routes |
|---|---|
| `read` | `POST /v1/sync/pull`, `GET /v1/repositories/{id}`, `GET …/records/…`, `POST /v1/artifacts/download-url` |
| `write` | `POST /v1/sync/push`, the §19 write routes, `POST /v1/pull-requests/{id}/merge`, `POST /v1/artifacts/upload-url`, `POST /v1/artifacts/confirm` |
| `admin` | `POST /v1/repositories/{id}/metadata`, `GET` and `POST /v1/repositories/{id}/grants` |

A refusal is `403` with the `permission` code, which is exit 5 (§22) — the
code the client has always mapped, so **nothing on the wire changed** for
grants to become enforceable. `POST /v1/repositories` is the exception in
both directions, below.

- **First-writer-registers.** The principal whose `POST /v1/repositories`
  creates a repository receives `admin` on it. Everyone else has no access
  until granted. Registering against a repository that already exists needs
  `read`, because registration is the call every sync makes before it pulls
  (§19) — refusing it to a reader would be refusing the pull.
- **A repository the service does not hold answers `not_found`, not
  `permission`.** An absent repository is not an authorization question, and
  making it one would break the two things that read that 404: `ark login`
  verifies a credential by pulling an id that cannot exist, and a repository
  the service has *lost* is detected the same way (§19).
- **Grants are keyed on email.** `POST /v1/repositories/{id}/grants` takes
  `{email, level}` or `{email, revoke: true}`. An address nobody holds yet is
  stored as a pending grant and resolves to a principal the first time that
  address logs in, so a grant can be issued before its grantee has ever
  authenticated and no credential is passed person-to-person. Re-granting
  corrects the level; revoking what nobody holds is a success.
- **`ARK_DEFAULT_GRANT`** is `none`, `read`, or `seeded`, and defaults to
  `seeded`. `read` makes every authenticated principal a reader of every
  repository and never confers `write`. `seeded` means grants arrive from the
  identity provider at login; with no identity provider nothing seeds
  anything, so a self-hosted deployment gets deny-by-default without knowing
  the setting exists.
- **The legacy service token carries implicit `admin` everywhere** and is
  checked against no grant at all (§20, "Two kinds of bearer"). It is the
  string the whole fleet holds and it identifies nobody, so there is nothing
  to check it against; it is also the break-glass that issues the first
  grants on repositories that predate them. Retiring it is elk-work/ark#54,
  which has to replace that break-glass rather than only remove it.

### The actor binding

`grants` says which repositories a principal may write to. **The actor
binding says which identity it may write as**, and it is what makes the
`created_by` on every record (§5) checkable rather than asserted.

- **First-writer-binds.** An actor is bound to the principal that first
  introduced it, and — for an actor introduced before any of this existed —
  to the first principal that writes as it.
- **A push is refused if any mutation's `created_by` names a human actor
  bound to a different principal.** The whole push, not the one mutation:
  half-applying a batch whose identity the service does not accept would
  leave the client a rejection it cannot act on.
- **Carrying somebody's actor record is not writing as them.** A client
  uploads every actor it knows on every push, including ones it pulled from
  other people (§9.1), so re-sending another principal's actor is the normal
  case and stays a no-op.
- **The binding is enforced on human actors.** An agent actor was shared
  between people until `store.FindAgentActor` learned to key on
  (`agent_name`, `delegated_by`), so a client that has not upgraded still
  resolves to somebody else's agent and must not be refused for it. Writing as
  an agent is therefore governed by the delegation rule below until the fleet
  has moved; `internal/server/grantsactors.go` records what that leaves open.
- **Delegation is enforced on new agent actors, and on every write through
  the §19 write routes.** An actor of type `agent` must carry a
  `delegated_by` naming a human actor in this repository that does not belong
  to another principal. On a push that is checked where the agent is
  introduced, and every failure is `permission`. On the write routes it is
  checked on every call, because the delegating human is half the key a
  writer is resolved by (§6.0) and a check at registration alone would let a
  later request name somebody else's human and land on their agent; there an
  absent, dangling or non-human value is `validation` — a malformed `writer`
  is a malformed request — and only "belongs to another principal" is
  `permission`.
- **A request selects a registration; it never rewrites one.** The stored
  actor record's `delegated_by` is never taken from a payload, on either
  surface, so nothing can re-point an existing agent at a different human. A
  push re-sending an existing actor is a no-op, and a write route naming a
  different human resolves to that human's own agent, registers it, or is
  refused.
- **The legacy service token binds nothing and is bound by nothing.** An
  actor "introduced by" a string the whole fleet shares is introduced by
  everybody, and binding under it would hand every existing actor to a
  principal that is not a person.

---

## 20. Authentication

V1 may begin with one user and one service token. It no longer ends there: the
service also verifies per-principal credentials it issued itself
(`docs/rfc-0003-elk-issued-credentials.md`, slices 1-2). Both mechanisms are
live at once, and the order is fixed — see "Two kinds of bearer" below.

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

A fallback file that is **absent** is the ordinary state of a machine that has
never stored a token outside the keyring: storage starts from empty, and
neither storage nor resolution says a word about it. A fallback file that
**exists and cannot be read** — a syntax error, a truncated write, a mode that
denies the read — is a failure, and never an empty file. The client must not
write over it: the file is keyed by host and holds a token for each, so
rewriting it from an empty starting point replaces every host's credential
with the one being logged in, and the bytes it destroys are the only copy
those tokens had. Storage refuses, with 5, naming the path. Resolution reports
the same file rather than "no credentials", because that answer is a claim
about a store nobody could read — and it points the user at the login that
performs the write. Both messages leave the user somewhere to go: the file is
plain text, so it can be repaired in place or moved aside and the login run
again.

`ARK_NO_KEYRING` skips the keyring entirely, for a machine where the file is
the deliberate choice. Storage then goes straight to the file, without a
warning, because the operator asked for it.

Commands report which store answered. `ark login` names where it wrote the
token; `ark status` names where the token resolved from; `ark logout` names
the stores it emptied. None of them prints the token itself (§21).

`ark status` must also report a store it **could not read**, and not as an
absence. Its `--json` output carries two fields for this: `token_source` — the
store that answered, `env`, `keyring`, `file`, or `none` — and
`token_source_error`, the resolution failure verbatim when there was one a
person has to act on. The source stays `none` in that case, because nothing
did resolve and the field is a stable interface; the diagnosis arrives beside
it rather than as a new value inside it. `token_source_error` is unset on a
machine that has simply never logged in, which is the ordinary state and needs
no diagnosis, and unset for a keyring that is locked or absent, which
resolution has already announced on the standard error stream. Its text is the
sentence `ark sync` refuses with, so the two commands do not describe one
condition in two vocabularies.

The client must also be able to remove a credential it stored. `ark logout`
is host-scoped, the way `ark login` is, and clears **both** stores for that
host: the keyring entry and any entry in the fallback file. Removing only the
store that currently answers would leave a plaintext copy the keyring
outranks — unread, and therefore unnoticed — which is the worst outcome
available to the one command whose purpose is that the credential is gone.

Removal is idempotent. A host with nothing stored is a success, not a
not-found: the state the caller asked for already holds, and a 3 there would
fail an ordinary teardown on a machine that never logged in. A store that
refuses to give the credential up is a different thing and is a failure (5),
naming the store, the service and the account, because the token is still
there and a person now has to finish by hand.

`ARK_NO_KEYRING` skips the keyring for removal as it does for storage, but
not silently: removal reports that the keyring went unexamined. Storage can
be quiet about the opt-out because the operator asked for it; removal cannot,
or "nothing to remove" is a claim about a store nobody looked in.

`ARK_TOKEN` cannot be removed by a command — no process can unset a variable
in the shell that started it. When it is set, removal still clears the stores
and then reports that a token continues to resolve, for every remote, with
exit code 7 (§22: the command did what it was asked and something still needs
repairing). Reporting plain success there would describe a machine as logged
out while every sync it runs still authenticates.

### Two kinds of bearer

The service accepts two things in `Authorization: Bearer`, and tries them in
this order:

1. **The service token**, `ARK_API_TOKEN`, compared in constant time exactly as
   before. A match synthesizes a `legacy` principal carrying implicit write on
   every repository, and is logged as such. This branch reads no credential
   store, so it keeps working whether or not one exists and whatever state it
   is in — the whole fleet holds this string, and it must not acquire a new
   dependency.
2. **A per-principal credential**, `arkc_` followed by 32 random bytes in
   base64url, which the service minted and stores only as a SHA-256. It is
   refused if the digest is unknown, the credential is revoked or past
   `expires_at`, or its principal is disabled.

A bearer without the `arkc_` prefix is not a credential this service issued, so
nothing is looked up for it. Everything refused is `401` with a `permission`
error code (§22), whichever mechanism refused it.

Credentials live in one SQLite database, `auth.db`, held in the same backend as
the repository databases and written with the same object-generation
compare-and-swap. It holds `principals`, `credentials` and `grants`. It is
cached in memory with a hard 60-second time-to-live, which is the bound on how
long a revocation takes to land: revocation is eventually consistent, and that
is the accepted price of not reading the store on every request. `last_used_on`
is recorded at **day** granularity and flushed lazily, so observing who is
still using which credential does not put a write on every request.

`grants` **is consulted on every route**, and the `Mutation.CreatedBy` actor
binding with it — see §19.2 for both. A credential reaches only the
repositories it has been granted, at the level it was granted; the service
token reaches everything, which is the difference between the two bearers and
the reason the order above is fixed. The device-authorization flow, which is
how a person gets a credential without already holding one, is §20.1, and the
`read` grants its approval may seed are ordinary rows in the same table.

`POST /v1/principals` mints a principal and its first credential, and is the
only route that accepts `ARK_BOOTSTRAP_TOKEN`. The credential is returned in
plaintext exactly once; nothing recovers it afterwards, and reissuing is the
supported repair.

The cloud service must identify:

```text
principal
actor
repository permission
```

Agent actions should record both the agent identity and the delegating
principal. All three are now identified and all three are enforced: the
principal by the bearer above, the repository permission by `grants`, and the
actor by the binding — §19.2.

---

## 20.1 Device Authorization

How a person gets a credential without already holding one
(`docs/rfc-0003-elk-issued-credentials.md`, Decision 3). It is an RFC 8628
device grant rather than an authorization-code flow, chosen because the case
Ark most needs to work is a developer or an agent on a remote host with no
local browser: a redirect flow would need the CLI to bind a loopback listener
and the identity provider to keep a redirect-URI allowlist, and would break
exactly there.

Three routes, and the flow is **off unless the service is configured for it**:
`ARK_IDP_APPROVAL_URL` names the page a person approves a code on, and
`ARK_IDP_KEY` is the shared secret that page's server presents. Unset, all
three answer `404 not_found` — a self-hoster with no identity provider has no
device login, and `ark login --token` is unaffected.

```text
POST /v1/device/code      (unauthenticated)
  {} → 200 { device_code, user_code, verification_uri,
             verification_uri_complete, expires_in: 900, interval: 5 }

POST /v1/device/token     (unauthenticated)
  { device_code }
  → 428 { code: "pending"  }   not yet approved
  → 429 { code: "slow_down" }  polled faster than `interval`
  → 410 { code: "expired"  }   past expires_in, or already redeemed
  → 200 { token, principal: { id, email, display_name } }

POST /v1/device/approve   (authenticated with ARK_IDP_KEY, server-to-server)
  { user_code, subject, email, display_name, repository_ids }
  → 200 {}
```

- **`user_code` is eight characters of Crockford base32 with the vowels
  dropped**, rendered `XXXX-XXXX`. Ark already speaks Crockford (§17); losing
  A and E on top of the letters Crockford already omits removes both
  confusables and accidental words. It is normalised on arrival — case,
  spacing, the hyphen, and Crockford's own I/L→1 and O→0 — because it is read
  off one screen and typed into another.
- **`device_code` is 32 random bytes, stored as a SHA-256**, like the
  credential itself. A leaked pending-code store must not be redeemable.
- **Redemption is one-shot.** The `200` deletes the pending row in the same
  transaction that mints the credential, so a replayed poll gets `expired`.
  That is what makes it safe to return a token over an unauthenticated route:
  possession of the `device_code`, which only the requesting process ever saw,
  **is** the authentication.
- **Nothing is minted before redemption.** Approval records the assertion
  against the pending row and nothing else; the principal, the credential and
  the seeded grants are all written in the transaction that deletes the row.
  An approval nobody redeems leaves no trace.
- **The two unauthenticated routes are unauthenticated because they must be** —
  the caller holds nothing yet. `/v1/device/approve` is not: it asserts who
  somebody is, which no client of the service may do, so neither the service
  token nor a credential reaches it.
- **Codes live 15 minutes and are polled every 5 seconds.** A client that
  polls faster is answered `slow_down` before the store is read, which is the
  point of the limit. The client gives up at the expiry with a message that
  says to run `ark login` again, not with a bare timeout.

**Grants may be seeded at approval, and only there.** The identity provider
may assert, alongside the verified email, the ark repository ids this
principal may read; the service writes them as ordinary `read` rows in
`grants` — the same table, on the same terms, so when enforcement lands there
is nothing special about a seeded grant. (§20: `grants` is not yet consulted,
so seeding today records an intention rather than confining anybody.) Ark
never learns what the list was computed from, and nothing on the request path
calls the identity provider. Two properties are load-bearing (RFC-0003,
resolved decision 2):

- **Seeding runs on every login**, not only the first, or a repository bound
  after somebody paired would be invisible to them forever.
- **Seeding only ever adds `read`.** It never removes a grant and never grants
  `write`, so a membership change at the identity provider cannot revoke Ark
  access as a side effect — revocation stays an explicit act.

**Discovery is `GET /`**, which is already unauthenticated and already returns
a service banner. It gains an `auth` object — `device_flow`, and
`approval_url` when there is one — and `ark login` reads it and picks its mode.
That is the whole mechanism: no client configuration, and nothing written down
on the machine.

`ark login` with no arguments runs the flow, prints the code and the URL,
polls, and stores the credential exactly as it stores a pasted one. `--token`,
the stdin path and `--remote` are unchanged, and a service reporting no device
flow is told so plainly rather than left to time out.

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
remote data corrupt
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
8 the service's stored copy of this repository is unusable
```

Exit code 7 is for a command that did what it was asked and left the
repository needing repair anyway. `ark sync` returns it in two cases: the
service rejected any mutation, so the local database now holds changes it
refused and will not be asked for again (§9.1); or the service's revision came
back below one this checkout had already synced past, so its history for this
repository was reset or lost (§9.2). A caller that scripts against exit codes
learns nothing from a 0 there, which is the whole complaint — the divergence
has to be legible to a program, not only in the prose the command printed.

`ark repair push` (§9.3) answers 0 **only when the repository no longer needs
repair**: a replay the service took whole, after which the recorded reset is
cleared. Everything else it can do is a 7 — a preview, which by design changed
nothing, and a replay the service rejected part of. The command's whole subject
is a repository in a state needing work, so a 0 from one that is still in it
would be the same false "in sync" that let elk-work/ark#58 sit for six weeks.
Refusing to run at all — no reset is recorded — is a 2, like any other
precondition the command line did not meet.

Exit code 8 is for a service that answered and cannot serve this repository:
its stored database will not open. It is a 5xx, and the reason it is not 6 is
that 6 means *try again*. A corrupt stored database is permanent until an
operator restores it, so a caller looping on 6 loops forever, and the fleet's
automation is exactly such a caller. 1 was the
alternative and says nothing a program can act on, which is the same complaint
that earned 7 its own code: the state has to be legible to a program, not only
in the prose the command printed. The three states a sync can now report about
the service are distinct — 6 could not reach it, 7 reached it and this
checkout needs repair, 8 reached it and **it** needs repair — and only the
last of them is somebody else's to fix.

The client tells 8 from an ordinary 5xx by the **error code**, not the status:
both are `500`, because both are the service failing to serve a valid request.
The cloud API's error body (§19) carries a code beside its message, and these
are the values:

```text
validation          400
not_found           404
conflict            409
permission          401, 403
internal            500 — the service failed; try again
repository_corrupt  500 — the service's stored copy of this repository is
                    unusable and will be until it is restored
```

A stored database that opens and holds no repository row is **not**
`repository_corrupt`. SQLite reads a zero-length object as a valid empty
database, so what is missing there is the repository rather than the bytes;
that is the same loss as an absent object, and every route answers it with
the `404 not_found` §19 specifies for one. Two kinds of damage, two
answers, and the difference is whether anything can still be read.

`repository_corrupt` names the repository and what is wrong with its stored
copy rather than the verb that was running when it was noticed. That is the
operator's own storage and not sensitive; withholding it left the diagnosis in
the service's logs, which is the one place nobody looking at a failing client
can see (elk-work/ark#65).

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

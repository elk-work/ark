# RFC-0004 — Authenticated Write Routes on the Sync Service

Status: proposed 2026-08-26
Related: docs/v1-spec.md §2 (scope), §3 (user model), §6.2–6.3 (task, comment),
§9 (sync model), §10.4 (field merge), §20 (authentication) ·
docs/rfc-0001-per-repo-sqlite-storage.md (CAS replay) ·
docs/rfc-0002-elk-work-record-adapter.md (Decision 2 — whose business record
semantics are; Decision 3 — the self-hosting constraint) ·
docs/rfc-0003-elk-issued-credentials.md (principals, credentials, grants)

## Problem

`ark-server` can be read by anyone holding the token and written to by nobody —
unless that writer is a full copy of Ark.

Reading is already a general API. `POST /v1/sync/pull` walks records after a
revision, ascending and server-authoritative; `GET /v1/repositories/{repo}/records/{type}/{id}`
fetches one. A program that wants to know what happened in a repository has a
supported way to ask.

Writing has one door and it is not a door for programs. `POST /v1/sync/push`
takes `api.Mutation` values — `record_type`, `record_id`, `operation`,
`base_server_revision`, and a payload that is the whole record document in
Ark's exact field vocabulary. That is a replication protocol. Its contract is
"the caller is another copy of Ark", and everything in it exists to serve
Ark's conflict rules rather than the caller's intent.

So today the only way for an outside program to create a task is to
reimplement enough of `internal/store` to mint one — a ULID, a display
`number` it cannot know, `created_by_type`, `sync_state`, `version`, an
operation verb chosen from a set (`create|append|submit|update|delete`) whose
distinctions exist for §10's merge rules — and then to interpret a
`PushResponse` that may report the number it chose was renumbered.

There are real callers waiting. The work-record connector's inbound half is
built and its outbound half is not: a person resolves something in another
system and nothing can write that back into Ark. A CI job that wants to file
the flake it just found has no route. A dashboard that renders a repository's
tasks can render a status but cannot change one.

This RFC specifies the missing door.

---

## Decision 1 — thin authenticated REST routes, not client-minted mutations

Three routes on `ark-server`, in the vocabulary of the records themselves:

```text
POST /v1/repositories/{repo}/tasks              create a task
POST /v1/repositories/{repo}/comments           comment on a task, PR, run, or review
POST /v1/repositories/{repo}/tasks/{id}/status  move a task within the allowed set
```

A caller sends what it means — a title, a body, a parent, a status — and the
service answers with the record it wrote, including the authoritative ULID and
the minted number. Nothing about Ark's mutation log, revision arithmetic, or
merge rules crosses the wire.

**Rejected: the caller mints mutations against `POST /v1/sync/push`.** It needs
no new server surface, which is its entire appeal, and it is the wrong trade
for the same reason, in the same direction, that RFC-0002 already refused once.

RFC-0002 Decision 2 put the work-record normalizer in Go, inside this
repository, rather than in TypeScript inside the consuming repo, and named the
principle out loud:

> Ark's record semantics are *Ark's* business: which statuses exist, that ULIDs
> are authoritative and numbers are display aliases, that an agent actor
> delegates to a human, that comments are append-only and corrections use
> `supersedes_id`. Encoding all of that in TypeScript inside Scout would put
> Ark's invariants a repo away from the tests that defend them, and every Ark
> schema change would become a cross-repo change.

Client-minted mutations are that same coupling with the arrow reversed. The
normalizer would read Ark correctly while the writer wrote it by guesswork. A
foreign repository would become the place where Ark's record model is encoded
— without the tests that defend it, in a language that cannot import
`internal/records`, and in as many copies as there are clients.

Priced concretely, this is not a close call:

| | Mutation-minting client | Thin routes |
|---|---|---|
| Where Ark's record model lives | in every client, in every language | `internal/server`, once, in Go |
| Cost of an Ark schema change | a coordinated release per client | none for clients |
| What a client must track | per-record `base_server_revision` | nothing |
| Who mints the display number | the client, then reads the renumbering back | the service, authoritatively (Decision 5) |
| Who enforces §5's immutability rule | the client, by convention | the service, by rejection |
| One-time build cost | zero | three handlers and their tests |

The three handlers are a few hundred lines that exist once. The alternative is
free today and charged monthly, forever, to everyone downstream.

**Not re-litigated: an outbound webhook from `ark-server`.** RFC-0002 Decision
3 rejected it — a delivery queue, retry policy, and per-binding secrets inside
a service whose virtue is being a dumb, scale-to-zero CAS over SQLite files.
Nothing here revisits that. These routes are inbound only.

**Nothing in these routes names a consumer.** `/v1/repositories/{repo}/tasks`
is a work-record API. Someone self-hosting `ark-server` gets a way to file a
task from a script; they do not discover anyone else's integration in it.
That is RFC-0002 Decision 3's constraint applied to this surface, and it is
why the routes are named after records rather than after the thing that
prompted them.

### What is deliberately not here

- **No general record editor.** No `PATCH /tasks/{id}`, no title or body
  rewrite. Editing a mutable field safely requires a base revision to merge
  against (§10.4: title/body overlap is a *conflict* needing a person; other
  overlaps are cloud-wins), and a REST client has no base revision and no
  conflict UI. The status transition is the one exception and Decision 4
  explains why it is safe.
- **No new record types and no new primitives.** These routes write records
  Ark already has. Principle 005.
- **No routes for the record types where a remote write has no meaning yet** —
  runs, threads, reviews, PRs, artifacts, promotions. A run is produced by
  something that ran; a review of a PR the caller cannot read is not a review.
  Each can be added later without changing anything here.
- **Display numbers are never accepted as input.** `{id}` and every
  `parent_id` is a ULID. The service renumbers colliding display numbers
  (§6.2), so a number-keyed write could silently land on a different record —
  the same reasoning that made RFC-0002 key its refs on ULIDs.

---

## Decision 2 — the writer is a registered agent actor; authority is read from the registration, never from the request

Spec §3 requires `created_by` and `created_by_type` on every record, and §20
requires the service to identify a principal, an actor, and a repository
permission, recording "both the agent identity and the delegating principal"
for agent actions. A bearer token satisfies none of that on its own.

**A remote writer is an agent, and it acts under a human's authority.** Every
record these routes write carries `created_by_type = "agent"` and a
`delegated_by` naming a human actor that already exists in the repository.

The registration is an actor record in the target repository. A write request
names its writer once, by agent name:

```json
{
  "writer": {
    "agent_name":    "release-bot",
    "agent_version": "1.4.0",
    "delegated_by":  "01J8ZQ4Q5H0000000000000000"
  },
  "title": "Flaky test in internal/sync"
}
```

The service resolves that to a repository-local actor with exactly the
semantics `internal/store.FindAgentActor` already gives a local agent — first
use creates the actor, every later use reuses it, so repeated writes by one
program share one identity. Three rules make the resolution safe:

1. **`created_by_type` is not an input.** It is always `agent`. A remote caller
   cannot author as a human.
2. **`delegated_by` must resolve to a human actor already in the repository**,
   or the request is rejected with `validation`. An agent cannot invent the
   authority it claims to act under. This is RFC-0003 Decision 5's server-side
   delegation check, applied at the moment an agent actor is introduced —
   which is the moment it matters.
3. **On every later write, `delegated_by` is read from the stored actor
   record**, not from the request. The registration is what attributes the
   write. A request cannot re-point an existing agent at a different human.

**Rejected: `created_by` taken from the request body.** This is what
`/v1/sync/push` effectively does today — it stores whatever the client sent and
never reads `Mutation.CreatedBy` at all — and copying it into a new surface
would bake an already-recorded defect into a second place. RFC-0003 has the
receipts (`server.go` "upserts actors with no checks at all, so an unrelated
caller can introduce an agent claiming to act for anyone"). A new route should
not be built to a standard the existing one is scheduled to be fixed to.

**Rejected: mint an actor per token, server-side, with a generic name.** It
produces records authored by "the token", which is exactly the outcome
RFC-0002 warned about from the other end: attribution collapsing onto whoever
holds the credential, discarding the one thing Ark knows that GitHub does not
— who authorized this agent.

### How this becomes token-derived without a wire change

RFC-0003 is accepted and unimplemented. It already specifies the registration
this decision wants: `credentials.token_sha256 → principals(kind, email,
display_name)`, with `ark token create --agent ci --repo <id> --level write`
minting a principal of `kind = 'agent'` whose `delegated_by` is the issuing
human principal — for precisely the case named there, "an agent with no human
at the keyboard".

When that lands, the credential *is* the registration. The route resolves the
writer from the principal, the `writer` object becomes redundant, and it is
rejected rather than trusted. Until then the field carries what the credential
cannot yet say, and every rule above already holds.

**Accepted cost, stated plainly:** with one shared token, two programs that
both call themselves `release-bot` share one actor, and any token holder can
write as any registered agent in any repository. That is not a regression —
the same token can already push arbitrary mutations authored by anyone — but
it is not a property to build on. Decision 3 says what fixes it.

---

## Decision 3 — the same token model as sync, and per-repository write scoping is the next thing to add

These routes sit behind `s.auth`, the existing constant-time comparison
against the single service token (v1-spec §20: "V1 may begin with one user and
one service token"; multi-user authorization is listed out of scope).

**This grants no new authority.** A holder of the sync token can already
create a task, comment, and close it — by pushing mutations. The routes make
that reachable without reimplementing Ark; they do not widen who can do it.
Any argument that the write surface is too open is an argument about the token,
and it is RFC-0003's argument.

**Rejected: a second, distinct write token (`ARK_WRITE_TOKEN`).** It looks like
scoping and is not. A second shared secret has the same blast radius as the
first — every repository the service knows — so it buys no isolation, while
costing a second value to distribute, store, and rotate, a dual-path `auth`
that must be right in both directions, and a migration to delete it when
RFC-0003's per-repository grants arrive. Two secrets, one boundary.

**The first thing to add is `grants.level = 'write'`.** RFC-0003 Decision 4
already defines it: `grants(repository_id, principal_id, level, ...)` with
`read` pulling, `write` pulling and pushing, `admin` also granting. These
routes are `write`, per repository, and the check is one lookup in the handler
that already knows `{repo}`. When it lands, Decision 2's rule tightens by one
clause — *and that actor is bound to your principal* — with no change to the
wire format, the request shapes, or any client.

Recorded as debt, with a named remedy, in the shape `docs/self-hosting.md`
already uses: until RFC-0003 ships, "anyone holding the token can read and
write every repository the service knows about" covers these routes too.

---

## Decision 4 — server-side writes advance `server_revision` in the same transaction, and idempotency is an explicit `Idempotency-Key`

### Revision

A server-side write mints no mutation. There is no client mutation log here to
append to, and inventing a synthetic one would be Decision 1's rejected
coupling smuggled in through the implementation.

What it does instead is what `handleMerge` already does, and the precedent
matters because merge is the existing authoritative server-side transition
(spec §12 step 12). Inside one `s.Repos.Update` closure:

1. `nextRevision` bumps the repository counter;
2. the record document is written to `records` at that revision;
3. `setFieldRevisions` stamps every field the write set.

Because `/v1/sync/pull` walks `records WHERE server_revision > ?`, the write
reaches every client on their next pull with no change to the pull path, no
new table, and no new cursor. Field revisions matter for exactly one reason:
a client that later edits the same field must see the server's change as
concurrent, so §10.4 can do its job. Skipping step 3 would make a server-side
write invisible to the merge rules and silently winnable by a stale client.

### Idempotency

**Recommendation: an explicit `Idempotency-Key` request header, stored in the
existing `applied_mutations` table.** Required on the two create routes;
accepted but not required on the status transition.

*Why a header rather than reusing a mutation id.* `applied_mutations` keys on
`Mutation.ID`, a ULID from a client's local log. A REST client has no local
log, so there is nothing to key on — the key has to come from the transport.
Naming it in the request also makes the retry contract explicit at the call
site, which is where the retry is written.

*Why the existing table rather than a new one.* Its columns are already the
right shape — `status`, `error`, `remote`, `server_revision`, `applied_at` —
and its semantics are already "a replay returns the stored outcome". `remote`
is a JSON TEXT column, so it can hold the response document verbatim and a
replay can return byte-identical output with `Idempotency-Replayed: true`.
No new table, no new migration, no second thing to reason about.

*Why this is not optional on creates.* Under RFC-0001 the service persists to
GCS with the object generation as a compare-and-swap, and a lost race **reruns
the whole closure**. That replay is safe today only because
`applied_mutations` makes it so. A create with no idempotency key would be
correct on the happy path and would duplicate a task under contention or a
client retry, with nothing in the record to tell the two apart afterwards.
`400 validation` on a missing key is the cheapest possible way to not ship
that.

*Namespace.* Caller-supplied keys are stored prefixed (`idem:<key>`) so they
cannot collide with, or read back, a mutation id.

*What a replay does not check.* A key replays the first outcome regardless of
what the second request's body said. Rejecting a reused key with a changed body
— the stricter and more common contract — needs a stored request fingerprint,
and `applied_mutations` has no column for one. Recorded below as an accepted
cost with its remedy, rather than half-enforced.

*Why the status route is exempt.* A status transition is an assertion about
state, not an increment. If the task is already in the requested status the
handler returns the current record and **mints no revision** — the same
reasoning `applyDelete` already uses, where deleting something already gone is
idempotent but must not bump the counter, "every client would then pull an
empty change set". A key is still honoured when supplied.

*Handler discipline.* CLAUDE.md's rule applies unchanged: update handlers
rerun their whole closure on retry, so they must be idempotent and reset their
accumulators. Every response value these handlers build is assigned inside the
closure, never appended to from outside it.

---

## Decision 5 — a server-side create mints the display number authoritatively

`reconcileNumber` exists because two offline clients can both mint task #7 and
the service has to break the tie: the ULID is authoritative, the number is a
display alias, and the loser is renumbered (§6.2).

A create through these routes is the one case with no tie to break. The
service allocates the number itself — `MAX(number) + 1` for the record type,
inside the same exclusive per-repository transaction that writes the record —
so the number is correct at the moment it is assigned and is never
subsequently rewritten.

Two consequences worth stating, because they are the questions a client will
have:

- **The number is returned in the response and is final.** A caller may print
  `#41` immediately. This is the only creation path in Ark with that property;
  a client-side `ark task create` prints a number that a later push may change.
- **It remains a display alias.** Callers that store a reference must store the
  ULID. A number is stable *for this record* once minted, but it is not the
  identity, and every ref format in RFC-0002 keys on the ULID for that reason.

The service is authoritative for numbering by construction — it is the same
authority that renumbers everyone else — so this decision is really a
statement that the existing model has a case where it costs nothing.

---

## Request and response shapes

Every response is the written record and the revision that carries it, reusing
the shape `MergeResponse` already established:

```json
{
  "record": {
    "record_type": "task",
    "record_id": "01K3...",
    "data": { "...": "the full record document" },
    "server_revision": 412
  },
  "server_revision": 412
}
```

### `POST /v1/repositories/{repo}/tasks`

```json
{
  "writer": { "agent_name": "release-bot", "delegated_by": "01J8Z..." },
  "title":  "required, non-empty after trimming",
  "body":   "optional",
  "status": "optional; defaults to open"
}
```

`status`, when given, must be one of `open|in_progress|blocked|done|closed`.
Returns `201`. `Idempotency-Key` required.

### `POST /v1/repositories/{repo}/comments`

```json
{
  "writer":        { "agent_name": "release-bot", "delegated_by": "01J8Z..." },
  "parent_type":   "task|pull_request|agent_run|review",
  "parent_id":     "01K3...",
  "body":          "required",
  "supersedes_id": "optional"
}
```

The parent must exist in this repository and not be deleted — `404 not_found`
otherwise. Comments are append-only (§6.3), so a correction is a new comment
carrying `supersedes_id`; there is no edit route, and there will not be one.
Returns `201`. `Idempotency-Key` required.

### `POST /v1/repositories/{repo}/tasks/{id}/status`

```json
{
  "writer": { "agent_name": "release-bot", "delegated_by": "01J8Z..." },
  "status": "done"
}
```

`{id}` is the task's ULID. `status` must be in the allowed set. Already in
that status → `200`, current record, no new revision. Returns `200`.

### Errors

The existing contract, unchanged: `api.Error{code, message}` with `code` in
`validation|not_found|conflict|permission|internal`, over the matching HTTP
status. `422` is not used; validation failures are `400 validation`, matching
every existing route.

---

## What ships in the first slice

1. The three routes, wired as `s.auth(s.handleX)` beside their neighbours in
   `internal/server/server.go`, with request and response types in `pkg/api`.
2. Writer resolution: create-or-reuse an agent actor by name, with the
   `delegated_by`-must-resolve-to-a-human check, sharing the record-writing
   path with `upsertActor`.
3. Idempotency over `applied_mutations` with the `idem:` prefix, and
   `Idempotency-Replayed` on a served replay.
4. Revision allocation and `field_revisions` stamping, so a pull carries the
   write and §10.4 can see it.
5. Table-driven handler tests over the existing local-directory backend: temp
   SQLite, no network.

Not in the slice, and each additive: per-repository grants (RFC-0003), routes
for any other record type, a general field editor, list/query routes beyond
the pull that already exists, and any client-side command that calls these.

**These routes are unreachable until the service is deployed.** The
implementation lands behind no flag and changes no existing behaviour, but a
running `ark-server` will not serve them until someone with deploy access
rolls it forward.

---

## Costs accepted

- **Two write paths into one record store.** `/v1/sync/push` and these routes
  both write `records`, and both must keep revision and field-revision
  bookkeeping correct. Mitigated by routing both through the same helpers
  (`nextRevision`, `setFieldRevisions`) rather than duplicating the logic —
  the same containment `handleMerge` already relies on.
- **Agent identity is name-keyed, not credential-keyed, until RFC-0003.**
  Recorded in Decision 2 with its remedy.
- **A required `Idempotency-Key` is friction for a casual `curl`.** Accepted:
  duplicate tasks that nothing can distinguish afterwards are worse than a
  header.
- **A reused `Idempotency-Key` with a different body replays the first
  outcome** rather than rejecting the second request. The stricter contract
  needs a stored request fingerprint and `applied_mutations` has no column for
  one; adding it is a migration and a decision about what to hash. The failure
  it would catch is a client bug (recycling a key across distinct writes), and
  the current behaviour fails safe — nothing extra is written.
- **`applied_mutations` grows without bound.** Already true of push; these
  routes add rows at human rather than sync rates. Whatever eventually prunes
  it prunes both.
- **No list or search route.** A caller that wants to find a task before
  commenting on it must pull. Deliberate: read paths already exist and adding
  a second, differently-shaped one is scope this RFC does not need.

---

## Exit triggers

1. **A caller needs to edit a title or body** → that is the general record
   editor Decision 1 excluded, and it needs a base revision and a conflict
   response. Design it against §10.4; do not bolt fields onto the status route.
2. **A second program wants its own credential** → RFC-0003's
   `ark token create --agent`, plus the `grants.level = 'write'` check named
   in Decision 3. Nothing here changes.
3. **A caller needs to write a run, thread, review, or artifact** → add the
   route in the same shape. Artifacts additionally need the blob path
   (`/v1/artifacts/upload-url`), which already exists.
4. **Write volume stops being human-scale** → revisit the exclusive
   per-repository transaction, which serializes every writer of a repository
   by design (RFC-0001). Nothing in this RFC makes that worse; a higher-volume
   writer would make it visible.

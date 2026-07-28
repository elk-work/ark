# RFC-0002 — The Elk Work-Record Adapter

Status: proposed 2026-07-28
Related: docs/rfc-0001-per-repo-sqlite-storage.md (non-goals), docs/adoption.md,
Elk-scout `docs/superpowers/specs/2026-07-10-work-record-connector-github-design.md`

## Problem

Work tracked in Ark is invisible to Elk. A task closed, a run finished with a
result summary, a PR merged with its review — none of it reaches an Elk
workspace unless a human or an agent stops and writes a manual capture. That is
exactly the "read nothing, observe automatically" promise inverted: the system
of record for *how the work went* is the one system Elk cannot see.

Elk already solved this shape once. The work-record connector ships a GitHub
adapter that turns repository events into Elk signals, captures, and actions,
keyed by `external_refs` for idempotency, with `Closes elk:<id>` in a merged PR
body resolving an Elk action to `done`. That design names Ark as adapter #2 and
its inbound file says so out loud:

> This file is the ONLY inbound code that knows GitHub's payload shapes; Ark
> (adapter #2) ships a sibling file producing the same WorkRecordEvent and
> everything downstream (routing, mapping, refs) is untouched.

The seam was pre-drilled: `source_bindings.provider` and `external_refs.provider`
already carry `check (provider in ('github','ark'))`, and `installation_id` is
already nullable "for ark". Nothing has been built against it.

## Decision 1 — the seam is the work-record connector, not a Signal Tap

Routing Ark through Signal is an attractive framing and it is the wrong one for
this problem. Signal is an analytical read-model: it materializes sources into
`scout_raw` → `scout_stage` → `scout_view` in Neon, and by recorded decision
(Signal `docs/0001-signal-in-elk.md`) it *"never calls an LLM per record, never
writes captures, never builds business views on its own."* An Ark Tap would
therefore deliver queryable SQL about Ark records and would **not** put a single
thing in an Elk workspace stream. The stated goal — activity in a repo's Ark
shows up in Elk — is unreachable down that path.

This is not a new position. RFC-0001 already recorded it in its non-goals:

> Scout Signal … is an analytical read-model … while Ark is an operational
> system of record. They should meet at the data seam (an Ark source connector
> feeding Scout via the pull-by-revision API), not the storage seam.

So the two are complementary, not alternatives:

| | Work-record adapter (this RFC) | Signal Tap (later, optional) |
|---|---|---|
| Produces | signals, captures, actions in a workspace | rows in `scout_raw.raw_ark_*` |
| Consumer | a person reading their Elk stream | SQL, views, digests |
| Latency | minutes | refresh cadence (30m) |
| Status | **build now** | worth doing; not this |

A Signal Tap would additionally have to pay down Signal's own unresolved split
between `pkg/connector` (conformance-gated, materializes nothing) and
`internal/connector` (materializes, ungated), and would invert an already-recorded
dependency — Signal's own work is meant to be recorded *in* Ark, so ingesting Ark
*into* Signal closes a loop nobody has designed. Deferred, with the exit trigger
below.

## Decision 2 — Ark normalizes; Elk routes

The adapter splits at the `WorkRecordEvent` boundary that the GitHub adapter
already established.

```
.ark/ark.db  ─┐
              ├─►  ark's normalizer  ──►  WorkRecordEvent[]  ──►  Elk ingest
/v1/sync/pull ┘    (internal/workrecord)     (the contract)      (provider-agnostic,
                                                                  already shipped)
```

**The normalizer lives in Ark, in Go.** The GitHub design assumed a sibling
TypeScript file in the edge function, and for GitHub that is right — GitHub's
payload shapes are GitHub's business. Ark's record semantics are *Ark's*
business: which statuses exist, that ULIDs are authoritative and numbers are
display aliases, that an agent actor delegates to a human, that comments are
append-only and corrections use `supersedes_id`. Encoding all of that in
TypeScript inside Scout would put Ark's invariants a repo away from the tests
that defend them, and every Ark schema change would become a cross-repo change.

**Everything downstream stays where it is.** `external_refs`, `source_bindings`,
`source_actor_map`, and the `Closes elk:<id>` resolver are already
provider-agnostic and do not change at all. The Scout-side work is:

1. a route that accepts normalized events (auth + workspace routing);
2. widening `provider: "github"` to `"github" | "ark"` — the type is currently a
   hard-pinned literal, though both database CHECK constraints already allow
   `'ark'`;
3. three new arms in `mapEvent()` for the kinds GitHub has no equivalent of
   (`run.finished`, `promotion.activated`, `thread.closed`). The other six kinds
   reuse GitHub's vocabulary and need nothing.

## Decision 3 — pull by revision, not an outbound webhook from ark-server

Ark's mutation log gives the adapter a cursor most connectors would envy:
`POST /v1/sync/pull {repository_id, after_revision}` returns every record whose
`server_revision` exceeds the cursor, **ascending**, server-authoritative,
monotonic per repository. Every page can checkpoint. Replay is free. Backfill
from revision 0 is free — something the GitHub adapter conspicuously cannot do
(its documented backfill is "manually re-deliver webhooks from the App's
Advanced tab").

The rejected alternative is giving `ark-server` an outbound webhook. It costs a
delivery queue, retry policy, and per-binding HMAC secrets inside a service
whose entire virtue is that it is a dumb, scale-to-zero CAS over SQLite files.
More decisively: it would make Elk a first-class concern of a general-purpose,
Apache-2.0, self-hostable tool at exactly the moment that tool goes public.
Someone self-hosting `ark-server` should never discover an Elk webhook in it.
Pull keeps the coupling entirely on the Elk side, where it belongs.

Push remains the exit trigger if event latency ever matters (see below).

## Decision 4 — the producer runs client-side first

This one is genuinely contested and is **queued for the owner** (see "Queued
decision" below). The implementation ships the recommended option and is
structured so the other is additive.

Where the pull actually executes has three candidates:

1. **Ark client** (`ark elk push`, riding the `ark sync` moment). Works on a
   local `.ark/ark.db` with no server at all.
2. **Elk-side scheduled puller** against `/v1/sync/pull`. One job, no per-developer
   setup, no client cooperation.
3. **ark-server** pushing. Rejected in Decision 3.

Option 2 is the better end state and option 1 is the only one that works today,
for a blunt reason established while gathering Phase D evidence: **no repository
has ever synced.** Signal's Ark holds 27 mutations, all `pending`, `last_revision
= 0`, no remote configured; Pulse's holds 19, likewise. An Elk-side puller
deployed today would faithfully poll an empty set.

So: ship the client-side producer, which turns real existing history into an Elk
stream immediately, and add the server-side puller when repositories actually
sync. They share the normalizer and the same `external_refs` keys, so running
both — or migrating between them — is safe by construction. Duplicate emission
from N clients is precisely what the `(provider, external_id)` unique index is
for.

## The mapping

Ark record → Elk object. The governing principle is Elk's, not Ark's: a
**signal** is ambient awareness, a **capture** is something worth remembering,
an **action** is something a person must do. Most Ark activity is awareness;
very little of it is a new obligation.

Six of the nine event kinds deliberately **reuse the vocabulary the GitHub
adapter already established**, so they need no new arms in `mapEvent()` and
inherit its behaviour for free. An Ark task *is* an issue — `ark gh issue`
already maps issues to tasks (spec §14) — so calling it one downstream is
accurate, not a hack.

Only three kinds are new, and they are precisely the concepts GitHub has no
equivalent for. That they are new is the argument for the adapter.

| Ark record | Event kind | Elk object | Notes |
|---|---|---|---|
| `task` created | `issue.opened` ↺ | signal | Not an action: a task in your own repo is already your to-do. Promoting it would double-enter the thing adoption.md warns about. |
| `task` → `in_progress`/`blocked` | `issue.status` ↺ | signal | `blocked` is the one worth surfacing; carried in `refs.status`. |
| `task` → `done`/`closed` | `issue.closed` ↺ | resolveAction | Resolves an Elk action bound to the task's object key, exactly as GitHub issue closure does. |
| `agent_run` finished | **`run.finished`** ★ | **capture** | The crown jewel. `input_summary` + `result_summary` + `result_commit_sha` is the durable "why" that Git and GitHub structurally cannot hold. |
| `agent_run` started | — | skip | Noise. The finish carries the story. |
| `pull_request` merged | `pr.merged` ↺ | **capture** + resolveAction | Body runs through the existing `Closes elk:<id>` parser unchanged. |
| `pull_request` opened | `pr.opened` ↺ | signal | |
| `review` submitted | `review.submitted` ↺ | signal | `changes_requested` carries higher detail than `approved`. |
| `promotion` activated | **`promotion.activated`** ★ | **capture** | An environment changed; that is a fact worth keeping. |
| `comment` | `comment` ↺ | signal | Gated by `fencing_config.comments`, off by default — same lever, same default as GitHub. |
| `agent_thread` closed | **`thread.closed`** ★ | capture | Title + message count. Individual `thread_message`s are **not** ingested — a design conversation is one memory, not forty. |
| `artifact` | — | skip (v1) | Referenced by name inside the owning run's capture (`refs.artifact_names`). Blob transfer needs signed URLs; deferred. |
| `actor` | — | identity input | Feeds the actor map; never an event. |
| tombstones | — | skip (v1) | Ark soft-deletes are rare and Elk has no un-publish. Logged, not applied. |

↺ reuses an existing GitHub kind — no downstream change.
★ new kind — needs one new arm in `mapEvent()`.

**Reusing a kind means reusing its refs.** Found while building: the existing
`issue.*` arms read `refs.issue_number` to render `(#12)`, and Ark emits
`refs.task_number`. Left alone, every Ark task signal would have read
`(#undefined)`.

The fix belongs at the ingest boundary, not in either end. Ark keeps emitting
`task_number` — it has tasks, not issues, and its wire format should say so —
and `ark.ts` aliases it onto `issue_number` as it normalizes, exactly as it
already aliases the display name. `mapping.ts` stays Ark-free, and the ULID
keys are untouched because they never used the display number in the first
place.

The general rule this establishes: **borrowing a kind means satisfying that
kind's whole refs contract**, and the translation is the adapter's job.

### Deriving events from a state feed

`/v1/sync/pull` returns *records at their current state*, not events. That is
fine, and it decomposes cleanly:

- **Append-only types** (`comment`, `thread_message`, `agent_run` on finish,
  `review`, `artifact`, `promotion`) — first sighting *is* the event. Nothing to
  diff.
- **Mutable types** (`task`, `pull_request`) — the state space is small and
  enumerable (`open|in_progress|blocked|done|closed`, `open|merged|closed`), so
  the event is derived from the record's own status field, and the *event key*
  carries the status.

That last point is the whole idempotency story, and it needs no projection table:

```
ark:<repository_id>#task/<ulid>:status/done
```

If that ref exists, the transition has been emitted. The `(provider, external_id)`
unique index on `external_refs` — already there for webhook redelivery dedup —
does state-transition dedup for free. Re-pulling from revision 0 is a no-op.

Accepted cost: a task that goes `done → open → done` emits once. The GitHub
adapter has the identical property on `#issue/{n}:closed`, so this is precedent,
not novelty.

### Key format

Object keys (what actions and resolution bind to):

```
ark:<repository_id>#task/<task_ulid>
ark:<repository_id>#pr/<pr_ulid>
```

Event keys (what signals and captures ref, for dedup):

```
ark:<repository_id>#task/<ulid>:created
ark:<repository_id>#task/<ulid>:status/<status>
ark:<repository_id>#run/<ulid>:finished
ark:<repository_id>#pr/<ulid>:merged
ark:<repository_id>#review/<ulid>
ark:<repository_id>#comment/<ulid>
ark:<repository_id>#promotion/<ulid>
```

ULIDs, never display numbers — the CLAUDE.md invariant ("Task/PR numbers are
display aliases; ULIDs are authoritative") is load-bearing here, because the
server renumbers colliding display numbers and a number-keyed ref would silently
rebind to a different record.

`external_url` is populated with `ark://<repository_id>/task/<number>` — not
clickable (V1 has no web UI, deliberately) but stable, greppable, and
`ark task view` resolves it. The human-readable repo name and task number travel
in the event body.

### Workspace routing

One `source_bindings` row per Ark repository, `provider = 'ark'`,
`installation_id = null` as the schema already anticipates. `repo_full_name`
holds the **Ark repository ULID**, which is what the pull API keys on and what
survives a Git remote rename; the human name travels in the event.

This reuses the existing unique index `(provider, repo_full_name) where connected`
with no migration. It is a mild abuse of the column name and the RFC records it as
such: if a second non-GitHub provider arrives, rename the column to
`external_repo_key` rather than growing a special case.

### Identity

`source_actor_map(workspace_id, provider, provider_login) → person_id`, with
`provider = 'ark'`. Two Ark-specific rules:

**Key on the actor's email, falling back to name — not the actor ULID.** Actors
are minted per repository at `ark init`, so the same human is a different ULID in
every repo. Email is stable across repos and across machines; a ULID key would
need one mapping row per person per repository, which is how an identity map
becomes abandoned.

**Resolve agents to their delegating human.** An Ark agent actor carries
`delegated_by` pointing at the human under whose authority it acted. The adapter
resolves `agent → delegated_by → human → person`. This is not a nicety: in the
Signal repository, **100% of Ark records were written by an agent actor and the
human actor authored nothing.** Without delegation resolution the entire stream
would land unattributed on the binding's creator, and the one thing Ark knows
that GitHub does not — who authorized this agent — would be thrown away at the
door.

Unmapped actors fall back to `binding.created_by`, matching the GitHub adapter
(`signals.owner_id` is NOT NULL). Actions are only ever created for *mapped*
people; an unmapped actor produces a signal naming them instead. Same rule,
inherited.

### Cursor

Per binding, `after_revision bigint not null default 0`. Advance only after the
batch's refs are committed; on crash, re-pull and let `external_refs` absorb the
overlap. Since the walk is ascending and server-authoritative, every page can
checkpoint — no `ResumeState`, no lookback window, no descending-walk trap.

For the client-side producer the cursor lives in the client's own
`.ark/` state; for a server-side puller it lives on the binding. Both key the
same refs, so they cannot double-emit.

## What ships in the first slice

Deliberately small, and chosen so it works on repositories exactly as they exist
today (unsynced, local-only):

1. `internal/workrecord` — the normalizer. Ark records in, `WorkRecordEvent`s
   out, plus the event-key derivation. Pure, table-driven tests, no I/O.
2. A cursor over either source: the local store, or `/v1/sync/pull` for a repo
   with a remote.
3. `ark elk events` — emit NDJSON to stdout, `--since <revision>`, `--dry-run`.
   Inspectable before anything is delivered.
4. Delivery of those events into an Elk workspace, demonstrated end-to-end on
   the Signal repository's real Ark history.

Not in the slice: the Scout-side ingest route, threads, artifacts, tombstones,
write-back (`push_to_work_record` for Ark), and the server-side puller.

## Costs accepted

- **Client-side production means the stream is as fresh as the client runs.**
  A repo whose developer never runs the command is dark. Mitigated by riding
  `ark sync`, and eventually replaced by the server-side puller.
- **Re-entered states emit once.** Documented above; precedent in the GitHub
  adapter.
- **`repo_full_name` holds a ULID for Ark bindings.** Recorded as debt with a
  named remedy.
- **`occurred_at` is still dropped by the ingest side.** The GitHub adapter
  discards it (signals get the literal string `"just now"`), so historical
  backfill will be time-lossy until Elk carries an event timestamp. Ark's events
  *will* carry `occurred_at` correctly; this is a Scout-side gap, and backfilling
  a year of Ark history will look like it all happened today until it is fixed.
  Worth fixing before any large backfill.

## Exit triggers

1. **Event latency becomes a complaint** → add the outbound push from
   `ark-server` (Decision 3), keeping pull as the reconciler.
2. **Someone wants to ask SQL questions across Ark repos** ("which repos have
   runs with no reviews?") → that is the Signal Tap from RFC-0001's non-goals.
   Build it then, additively; it does not replace this.
3. **A second non-GitHub provider** → rename `repo_full_name` to
   `external_repo_key` and drop the ULID-in-a-name-column debt.
4. **Ark grows a web UI** → `external_url` becomes a real link; no other change.

## Queued decision for the owner

Everything above is decided on evidence. One thing is a genuine judgment call:

> **Where should the producer run — the Ark client, or an Elk-side scheduled
> puller?**
>
> The RFC implements the client-side producer because it is the only option that
> works on repositories as they exist today (zero syncs, ever). The Elk-side
> puller is the better end state — no per-developer setup, no client cooperation
> — but it is blocked on repositories actually syncing, which is itself a Phase D
> question.
>
> Choosing "Elk-side puller" makes this RFC depend on the Phase D verdict.
> Choosing "client-side, then both" ships value now and defers nothing that
> cannot be added additively.

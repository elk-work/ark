# RFC-0005 — A Breakdown Board on the Sync Service

Status: proposed 2026-09-24
Related: https://github.com/elk-work/ark/issues/115 · v1-spec §3, §6.2–6.9,
§19.2, §20 · RFC-0003 (credentials and grants) · RFC-0004 (thin REST)

## Problem

Elk holds the human's parent request; Ark holds its technical breakdown.
Without a human-readable Ark board, people move breakdowns into Elk simply
to see them. The sync service already holds the records and the authority to
read them. It needs a small read surface, not a second work-management model.

## Decision 1 — serve embedded HTML from ark-server

`GET /ui/` is sign-in or the repository switcher. `GET /ui/{repo}` is one
repository's board; `?task={ULID}` opens its detail. Go `embed` packages
`html/template`, CSS and a small vanilla JavaScript enhancement. No npm, Node
build, external scripts, fonts or second deploy. The initial page and detail
are server-rendered; JSON GET routes support refresh/filtering enhancements.

This settles issue #115's hosting question: one binary, auth store and deploy
are appropriate for a first surface. A separate site buys independent UI
releases at the cost of another origin, credential boundary and deployment.

## Decision 2 — an existing credential mints a bounded browser session

`POST /ui/session` accepts an `arkc_` credential once, verifies it through the
same verifier as Bearer auth, and issues 32 random bytes as a browser session.
Legacy and bootstrap tokens cannot sign in. Only the session's SHA-256,
credential id, creation time and fixed 12-hour expiry are stored in
`ui_sessions` in `auth.db`. The raw credential is neither retained nor echoed;
no browser storage API holds it. Each successful login rotates the presented
session. `POST /ui/logout` deletes its row and expires the cookie.

The cookie is `__Host-ark_ui`, HttpOnly, Secure, SameSite=Strict, Path=/,
with no Domain and a 12-hour Max-Age. HTTPS is required, including for local
browser testing; no insecure-cookie option. Cookie verification rechecks the
credential's expiry/revocation and principal's disabled state from the same
60-second auth snapshot. Logout and grant revocation have that same bound
across instances, immediate after a write on this instance. Session expiry is
absolute, not sliding. Expired session rows are pruned on session creation.

Cookie authentication is accepted only on the board and its two JSON GET
routes; existing APIs keep requiring Bearer auth. Explicit Authorization takes
precedence over cookies and a bad bearer cannot fall back to a good cookie.
POST session/logout require an Origin matching the request host and HTTPS
scheme (or the TLS-terminating proxy's forwarded HTTPS scheme); missing or
foreign Origin is refused. SameSite and Origin checks protect login/logout
from cross-site requests. No CORS is added. All UI and task-read responses
carry no-store, a self-only CSP, no-referrer and nosniff; templates escape
record text and JavaScript uses textContent, never record HTML.

`/v1/device/*` remains the CLI's flow. Once Elk ships its approval page
(ark:elk#13), the board can adopt it without changing the session boundary.

## Decision 3 — preserve all five task statuses, and remain read-only

The columns, in spec §6.2 order, are **open, in_progress, blocked, done,
closed**. Labels can use spaces; stored values cannot. These are already the
CLI's vocabulary, settling the status question without inventing a state.
Each card shows number, title, creating actor, age, and ULID on hover; detail
also shows the ULID, body and created/updated times. Actor means `created_by`,
not an invented assignee. Actor names are resolved from repository actors,
with the id as fallback.

Filters select a repository, an exact status (including open/closed), creating
actor id and parent Elk reference. `status=all` is the default; `open` is the
exact open column, not an alias hiding blocked/in_progress. Tasks sort by
number then id. Comments sort oldest-first, timestamp then id; superseded
comments stay visible. Runs and PRs are those with this task_id. Artifacts
include direct task attachments and attachments of its runs, PRs and PR
reviews. Artifact metadata is visible; downloads and embedded content wait.
Soft-deleted records do not appear. No writes, drag-and-drop, cross-repository
rollups, grant editing or Elk mirroring ship.

## Decision 4 — resolve Elk parents from existing text, conservatively

Main now includes #112's `elk-parent:` comments. The last valid marker in
creation order wins, matching `ark task view`/`list --elk`. A marker's `#N`
is explicitly an Elk number. Without a marker, use the first explicit
`elk:<id>`, `elk#N`, or `Elk #N` reference in the task body, then oldest-first
comments. A bare `#N` in prose is ambiguous (Elk's own CLAUDE.md says GitHub)
and is not a parent. `elk:35`, `elk#35` and labelled `#35` normalize to `#35`;
a UUID stays a UUID. No schema field or sync semantics are added.

The parent displays as text unless the same source text supplies an explicit
`https://elk.work/open/<workspace>` link. Only that HTTPS host and path shape
is accepted; no workspace is guessed from repository names, numbers or ids.
That link opens the workspace, not a guessed item deep link. This settles the
link question while remaining useful for self-hosters with no Elk at all.

## Decision 5 — the switcher lists this principal's resolved grants

Read, write and admin grants all appear. Pending email grants do not. Fetch
repository names only after finding the principal's grants; missing repositories
are skipped. No operator bypass and no repository inventory is exposed.
`handleListGrants` is an admin-only member roster, so it cannot implement this
lookup; reuse its auth snapshot, not its route.

Board and new task reads require an explicit read-or-better grant, even when
`ARK_DEFAULT_GRANT=read` allows other APIs a broader default. Legacy bearers
cannot enumerate or read this board API. This deliberate narrowing keeps the
new surface equal to its switcher and leaves existing API policy unchanged.

## Decision 6 — a runtime dial hides the entire slice

`ARK_UI` unset or `on` enables the surface; `off` returns 404 for `/ui/`, its
assets/session/logout, and both task GET routes. Other values fail startup.
Existing task POST routes, sync and device login remain available. No deploy
of a different binary is needed to dark the surface.

The session migration is `migrations/auth/0001_ui_sessions.sql`, embedded
separately and applied idempotently by openAuthDB. Root `migrations/*.sql`
are client record migrations: putting sessions there would synchronize an
authentication concern into the wrong database. No client migration is needed.

## Request and response shapes

```text
POST /ui/session
Content-Type: application/x-www-form-urlencoded
Origin: https://ark.example
credential=arkc_…
→ 303 Location: /ui/; Set-Cookie: __Host-ark_ui=…

POST /ui/logout
Origin: https://ark.example
→ 303 Location: /ui/; expired cookie

GET /v1/repositories/{repo}/tasks?status=all&actor={actor-id}&elk=%2335
Authorization: Bearer arkc_…  (or the browser session cookie)
→ 200
{
  "repository_id": "01…",
  "tasks": [{
    "id": "01…", "number": 14, "title": "Build reader", "status": "open",
    "actor": {"id": "01…", "name": "Builder", "type": "agent"},
    "created_at": "2026-09-24T20:00:00Z", "updated_at": "2026-09-24T20:00:00Z",
    "elk_ref": "#35", "elk_url": "https://elk.work/open/example"
  }]
}

GET /v1/repositories/{repo}/tasks/{id}
→ 200
{
  "repository_id": "01…",
  "task": {"…": "list fields plus body"},
  "comments": [{"…": "stored comment document"}],
  "runs": [{"…": "stored agent_run document"}],
  "pull_requests": [{"…": "stored pull_request document"}],
  "artifacts": [{"…": "stored artifact document"}]
}
```

`repo` and `id` are full ULIDs, never display numbers. Empty arrays are `[]`;
absent parent fields are omitted. Status accepts all or any of the five exact
statuses; an invalid value is 400. Actor filtering is exact id equality. Elk
filtering uses the normalization above. Existing `api.Error{code,message}`
applies: 400 validation, 401 permission for an absent/invalid session or
credential, 403 permission for a missing grant, 404 not_found for a missing
repository/task. Store failures retain the service's existing error handling.
An unauthenticated HTML page shows sign-in, and a failed JSON fetch asks the
person to sign in again rather than displaying an empty board as success.

## What ships in the first slice

1. Embedded sign-in, repository switcher, five-column board and task detail;
   filters, empty states, readable mobile layout and keyboard-usable links.
2. The two Bearer-or-session GET routes and explicit-grant checks.
3. Hashed sessions in auth.db, secure cookies, Origin checks, logout and expiry.
4. ARK_UI startup validation and documentation in README and deploy.md.
5. Local servertest-style coverage of grants, credential/session revocation,
   filtering, detail relations, escaping, cookie isolation and the off dial;
   standard make and CI checks, plus a browser verification.

## Costs accepted

- Reads scan a repository's JSON records. This matches the small first board
  and adds no duplicated index or cache; there is no pagination in this slice.
- Sign-in requires presenting a long-lived credential once. HTTPS and no-store
  limit exposure, but the device approval page is the preferable eventual UI.
- Session creation/logout contend on auth.db's CAS, alongside credential and
  grant changes. Sessions add one snapshot map and expire after 12 hours.
- Auth and grants can remain stale for up to 60 seconds across instances.
- Text-derived parents cannot establish item/workspace identity. Conservative
  labels and workspace-only links are less convenient than invented links.
- Plain escaped text preserves bodies and comments; rendered Markdown and
  artifact previews would need a separate sanitization/content policy.

## Exit triggers

1. Measured board latency or response size becomes troublesome → indexed,
   paginated reads with a stable cursor; never silently truncate the board.
2. Humans need status/comments here → specify write intent, actor attribution
   and CSRF defenses before enabling any cookie-authenticated write API.
3. Elk's approval page ships → replace credential entry with device approval.
4. Several repositories need a shared view → demonstrate the need and specify
   authorization per repository before adding a rollup.
5. Parent ambiguity causes real mistakes → add an explicit interoperable
   parent/workspace reference contract rather than expanding prose guesses.

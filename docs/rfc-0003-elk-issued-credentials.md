# RFC-0003 — Elk-Issued Ark Credentials

> Note (2026-08-11): `adoption.md`, `adoption-phase-d-verdict.md`, and
> `ops/elkproject-deployment.md` cited below were project-internal and moved to
> the maintainers' private meta-repo (`elkproject/elk`, `docs/ark/`) before this
> repository was opened. Citations to them are preserved as-is.


Status: accepted 2026-07-28 (replaces the single service token of v1-spec §20).
All three queued decisions resolved by the owner the same day — see "Resolved by
the owner".

**Implementation status (2026-08-28).** Slices 1-4 of "What ships in the first
slice" have landed: `auth.db` and the dual-path verifier (elk-work/ark#43),
`ARK_BOOTSTRAP_TOKEN` and `ark principal create` (#43), grants, `ark repo
grant`, and the `Mutation.CreatedBy` actor binding (#52), and the device flow
with `ark login` (#53). The contract they implement is v1-spec §19.2, §20 and
§20.1; read those first, and this for the reasoning. Slice 5 (the Elk
`ark-auth` function) is Elk-side; the legacy token retires in #54.

Grants therefore resolve at **either** login — `ark principal create` on a
service with no identity provider, and the device flow's redemption on one
with — and both claim an email-keyed grant before seeding runs, so an admin's
`write` is never displaced by a seeded `read`.

Two places where the implementation is narrower than this text, both recorded
where they bite rather than only here:

- **The actor binding is enforced on human actors, not on agent actors.** A
  named agent actor was shared between principals by the client's own
  resolution, so enforcing it would have refused a developer for a choice
  their client made before the request existed. The client half is fixed
  (elk-work/ark#100 keys the lookup on the delegating human); the service
  waits for the fleet to upgrade before enforcing, which is elk-work/ark#101.
  Delegation is enforced on both kinds throughout. See
  `internal/server/grantsactors.go`.
- **Revoking a credential had no route, and now has one.** Decision 2 makes
  `revoked_at` the revocation mechanism and the verifier honours it, but it is
  a service-wide act and Decision 4's grants are per-repository, so nothing in
  the text above says who may perform one. elk-work/ark#94 carried that
  question; it was ruled on 2026-08-30 and the answer is **Amendment 1** at the
  end of this document — an operator, which is a principal, not a widening of
  `ARK_BOOTSTRAP_TOKEN`. Decision 6 is unchanged.
Related: docs/v1-spec.md §20, docs/adoption.md § Credentials, docs/self-hosting.md
§ Authentication, docs/rfc-0002-elk-work-record-adapter.md (Decision 3 — the
self-hosting constraint), Elk-scout `supabase/migrations/0001_init.sql`

## Problem

`ark-server` has one token and no users. `internal/server/server.go:71-80` is the
entire authorization system: strip `Bearer `, constant-time compare against
`Server.Token`, and every `/v1` route behind it (`server.go:51-58`) is fully
open to whoever holds that string. `cmd/ark-server/main.go:49-50` refuses to
start without it. `docs/self-hosting.md:213-215` states the consequence without
euphemism: "There are no users, no per-repository permissions, and no scopes.
Anyone holding the token can read and write every repository the service knows
about."

That was a deliberate V1 simplification — spec §20 line 1171 says "V1 may begin
with one user and one service token" — and it has now expired for three
independent reasons.

**It fails the spec's own requirement.** §20 lines 1183-1191 say the cloud
service must identify `principal`, `actor`, and `repository permission`, and
that "agent actions should record both the agent identity and the delegating
principal". The server identifies none of the three. It does not read
`Mutation.CreatedBy` (`pkg/api/api.go:28`) anywhere; grep for it in
`internal/server/` returns nothing. Actor records are inserted unconditionally
and never bound to anyone (`server.go:158-166`, `server.go:195-217`).

**The bootstrap does not scale past the founders.** `docs/adoption.md:68-71` and
`docs/ops/elkproject-deployment.md:104-109` require each collaborator to hold an
elkproject.com GCP account and run `gcloud secrets versions access latest
--secret=ark-api-token`. Ark is Apache-2.0, tagged v0.1.0, public, with an
outside contributor invited (`docs/adoption-phase-d-verdict.md:154-160`). "Get a
Google Cloud account in our org" is not a credential story for a project that
invites strangers to run their own server.

**Rotation is an outage.** `docs/self-hosting.md:263-265`: "Rotation is a
restart… There is no overlap window: the old token stops working the moment the
new process is serving." One compromised laptop invalidates every client of
every repository, simultaneously.

`docs/adoption.md:72-74` already names the end state — "the user's **Elk login**
issues Ark credentials from the CLI — Elk is the identity provider for Ark, and
no GCP account is involved" — and the backlog has carried the task for months.
This RFC specifies it.

## Decision 1 — ark-server issues and verifies its own credentials; the identity provider only asserts identity at login

The credential a client presents is minted by `ark-server`, signed for or stored
by `ark-server`, and revoked by `ark-server`. An external identity provider
participates in exactly one moment: it tells `ark-server` "the human at this
browser is `alice@example.com`, and I verified that." From then on the IdP is
not in the loop.

**Rejected: ark-server verifies Elk's Supabase JWT directly (JWKS or shared
secret).** This is the obvious design and it is wrong here, on three counts.

*It cannot be specified from evidence.* Elk's own repository does not say what
algorithm its JWTs use. `supabase/config.toml:165-169` declares `jwt_expiry` and
nothing else; `signing_keys_path` is commented out and there is no JWKS URL
anywhere in the repo. Nothing in Elk verifies a JWT signature in its own code —
every edge function either delegates to the Supabase gateway or calls
`auth.getUser(jwt)`, a network round trip to GoTrue (`supabase/functions/onboard/
index.ts:93-104`). The one place a JWT is decoded by hand
(`supabase/functions/meet-reconcile/index.ts:102-110`) carries an explicit
warning that it does **not** verify the signature and is only safe because the
gateway did. Designing ark's request path around a key-publication mechanism
nobody in Elk has yet used would be inventing Elk's auth rather than integrating
with it.

*It puts Elk on ark-server's hot path.* `ark-server` is a scale-to-zero Cloud Run
service pinned to `--max-instances 1` (`docs/ops/elkproject-deployment.md:46`).
A cold start with a stale or empty key cache would have to fetch from Elk before
serving the first request. Elk down, or Elk's key endpoint slow, becomes ark
down — for a system whose entire promise is that "a temporary network problem
should make collaboration stale, not make the tool unusable"
(`docs/principles.md:68`).

*It makes Elk mandatory.* A self-hoster has no Supabase project. See Decision 6.

What ark actually needs from Elk is a verified email address and a stable
subject id. That is a one-shot assertion, not a per-request dependency. Keeping
it one-shot means Elk's availability affects **new logins only**; every existing
credential keeps working through an Elk outage, indefinitely.

## Decision 2 — the credential is an opaque, long-lived, per-principal token, not a JWT plus refresh token

`ark-server` mints 32 random bytes, base64url-encoded, prefixed `arkc_`. It
stores the SHA-256 of that value against a principal row and returns the
plaintext once. The client sends it as `Authorization: Bearer arkc_…` on every
request, exactly as it sends the service token today (`internal/cloud/
client.go:52`).

**Rejected: short-lived signed access token plus refresh token.** The reasons
that normally justify a JWT do not apply to ark.

*Stateless verification buys nothing here.* A JWT's payoff is skipping a
datastore lookup. `ark-server` already performs a datastore round trip on every
`/v1` request — it fetches or opens the repository's SQLite file from the backend
(RFC-0001). Adding a second lookup against a single, tiny, read-mostly,
in-process-cached table is not a new cost class. It is a rounding error against
a GCS object fetch.

*Federation is not a requirement.* JWTs pay for themselves when many independent
services must verify without talking to the issuer. Ark has exactly one
verifier, and it is the issuer.

*Revocation is the actual requirement, and JWTs are worse at it.* The concrete
failure this RFC exists to fix is "one credential is compromised, retire it
without disrupting anyone else". Opaque: set `revoked_at`, effective on the next
request. JWT: maintain a denylist — the same table you were trying to avoid,
plus a window during which the stolen token still works.

*It avoids a whole class of bug.* No clock-skew handling, no key rotation
procedure, no refresh-token rotation and reuse-detection state machine, no
"refresh raced with another refresh" concurrency on a CAS-over-object-storage
store. Principle 005 (`docs/principles.md:102-129`): a primitive must pay for
its weight by making the rest of the system simpler. A refresh protocol does the
opposite.

**Lifetime.** `expires_at` defaults to 365 days from issue. A laptop closed for
a week is not an event. If a credential does expire, recovery is `ark login`
again — fifteen seconds, no operator involvement, no secret store, which is
precisely the "boring recovery path" the principles ask for.

**Offline is not a credential question.** Local operations never touch the
network: `internal/app/app.go` opens `.ark/ark.db` and `internal/store` does the
rest; `internal/cloud` is reached only from `ark sync` and `ark elk push`. A
credential's expiry cannot make `ark task create` fail, because `ark task create`
never looks at it. This is worth stating plainly because it removes the usual
argument for long-lived offline-capable tokens: ark's offline story is structural,
not credential-dependent.

**Usage tracking is best-effort and coarse.** The server records `last_used_at`
at day granularity and flushes it lazily — batched in memory, written on the next
`auth.db` write or a timer. A lost flush is harmless (the credential's clock
simply did not advance). This matters because the migration in this RFC gates
retirement of the legacy token on *observed* usage, not on a date, and that needs
usage to be visible without putting a write on every request.

## Decision 3 — the device-authorization flow lives on ark-server; the approval page lives at the identity provider

`ark login` today reads a token from `--token` or stdin and hands it to
`cloud.StoreToken` (`internal/cli/remote.go:89-100`). It gains a no-argument
form that runs an RFC-8628-shaped device grant. Three new routes, all on
`ark-server`:

```text
POST /v1/device/code      (unauthenticated)
  {}
  → 200 { device_code, user_code, verification_uri,
          verification_uri_complete, expires_in: 900, interval: 5 }

POST /v1/device/token     (unauthenticated)
  { device_code }
  → 428 { code: "pending"  }   not yet approved
  → 429 { code: "slow_down" }  polled faster than `interval`
  → 410 { code: "expired"  }   past expires_in, or already redeemed
  → 200 { token, principal: { id, email, display_name } }

POST /v1/device/approve   (authenticated with ARK_IDP_KEY, called server-to-server)
  { user_code, subject, email, display_name }
  → 200 {}
```

`user_code` is 8 characters from Crockford base32 minus vowels, rendered
`XXXX-XXXX`; ark already speaks Crockford (`records.NewID()` mints ULIDs), and
dropping vowels removes both confusables and accidental words. `device_code` is
32 random bytes and is stored hashed, like the credential itself — a leaked
device-code store must not be redeemable.

Timeouts: 15-minute code TTL, 5-second poll interval, and the CLI gives up at the
TTL with a message that says to run `ark login` again rather than a bare timeout.
Redemption is one-shot: the 200 response deletes the pending row in the same
transaction, so a replayed poll gets `expired`. This is what makes the token safe
to return over an unauthenticated route — possession of the `device_code`, which
only the requesting process ever saw, is the authentication.

**The browser side.** `verification_uri` is whatever `ARK_IDP_APPROVAL_URL`
points at. For elkproject that is a Supabase edge function (`ark-auth`) serving a
minimal page: the user signs in with Elk's existing email-OTP flow, types the
eight-character code, and the function calls `POST /v1/device/approve`
server-to-server with `ARK_IDP_KEY`. The browser never talks to `ark-server`, so
there is no CORS surface and no CSRF surface; the approval is a back-channel call
between two servers.

**Rejected: a full OAuth authorization-code flow with redirects.** It requires
the CLI to bind a loopback listener, requires the IdP to maintain a redirect-URI
allowlist, and breaks entirely for the case ark most needs to work — an agent or
a developer on a remote host with no local browser. The device grant is
strictly better for a CLI and is why RFC 8628 exists.

**Rejected: reusing Elk's existing connector-token shape.** Elk already mints
per-person credentials — `connector_tokens` (`supabase/migrations/
0003_chat_session_capture.sql:16-30`), one active per person, revocable — and
delivers them by having the user copy a URL out of a settings screen
(`ElkKit/Sources/ElkData/Supabase/SupabaseRepository.swift:1834-1868`). Two
problems. The value is stored in plaintext as the table's primary key under
Elk's blanket `authenticated full access` RLS policy (`0001_init.sql:182-193`),
which Elk's own notes flag as the first credentials table needing a per-workspace
RLS pass. And copy-paste delivery is exactly the manual step `ark login` should
eliminate. What is worth reusing is the *shape* — per-principal, revocable, one
active by default — not the plumbing.

**Rejected: the shared-bearer pattern of `0060_ark_ingest_token.sql`.** Elk
already has an "ark token": `private.gh_connector_config.ark_ingest_token`
(`supabase/migrations/0060_ark_ingest_token.sql:20-21`), checked constant-time at
`supabase/functions/gh-connector/ark.ts:19-24`. It is a single global shared
secret proving "you are Ark's producer" — the same primitive this RFC is
retiring, pointed the other direction. It is correct for what it does (one
service authenticating to one service) and is not a model for humans.

## Decision 4 — authorization is per-repository grants keyed on email, stored on the ark side

Three tables, in one new SQLite database (`auth.db`) held in the existing
`repodb.Backend` under a reserved key, with the same object-generation
compare-and-swap as repository databases. No new dependency, no new storage
technology, and it works identically in `DATA_DIR` mode.

```text
principals(id, kind, issuer, subject, email, display_name,
           created_at, disabled_at)
credentials(id, principal_id, token_sha256, label, created_at,
            expires_at, last_used_on, revoked_at)
grants(repository_id, principal_id, level, granted_by, granted_at)
```

`level` is `read`, `write`, or `admin`. Three values, not a matrix: `read` pulls,
`write` pulls and pushes, `admin` also grants and revokes. `admin` exists because
V1 has no web UI (deliberately, `CLAUDE.md:57-61`) and grants have to be issuable
from the CLI by someone.

**Grants may be seeded by the identity provider, but only at pairing time**
(resolved decision 2). At approval the IdP asserts, alongside the verified
email, the list of ark repository IDs this principal may read — a list Elk
computes on its own side from `source_bindings` joined to workspace membership.
`ark-server` writes those as ordinary `read` grant rows and then enforces them
like any other. Ark never learns what a workspace is, and nothing on the request
path calls Elk. Seeding runs on **every** login, not just the first, so a
repository bound after someone paired becomes visible to them; it only ever adds
`read`, never removes a grant and never grants `write`, so revocation stays an
explicit act rather than a side effect of a membership change.

**The bootstrap rule is first-writer-registers.** The principal whose call to
`POST /v1/repositories` first creates a repository receives `admin` on it.
Everyone else has no access until granted or seeded. `ark repo grant <email> --write` from
an admin creates a grant row keyed on **email**, resolved to a principal id at
that person's first login. This is what makes the outside-contributor story work:
the grant is issued before the contributor has ever authenticated, and no token
is passed person-to-person — the invariant `docs/adoption.md:66` already states.

Keying on email is the same choice RFC-0002 made for the Elk actor map ("Key on
the actor's email, falling back to name — not the actor ULID",
rfc-0002:266-272), and it is sound here for the same reason plus one more:
Elk's only sign-in mechanism is email OTP (`SupabaseRepository.swift:54-60`,
`supabase/config.toml:232,260-269`), so an Elk-asserted email is
verified-by-construction. It also inherits that choice's weakness — trust in the
IdP's email verification — which is recorded below as an accepted cost.

**Rejected: derive repository access from Elk workspace membership.** Attractive
because Elk already has the data — membership is a `people` row with a non-null
`auth_user_id` (`supabase/migrations/0001_init.sql:22-31`, plus `access_role in
('admin','member')` at `0045_workspace_member_admin.sql:18-19` and
`workspaces.owner_id` at `0034_workspace_owner.sql:17`). Rejected on four counts:

1. It puts Elk back on the request path, undoing Decision 1.
2. It imports a workspace primitive ark deliberately does not have.
   `docs/principles.md:106-110` is explicit: "At the moment, it is not clear what
   a first-class workspace would add. So Ark does not have workspaces yet."
   `CLAUDE.md:58` lists workspaces first among what is deliberately absent.
3. Ark repositories and Elk workspaces are not 1:1 and never were. RFC-0002
   binds them many-to-one through `source_bindings`, and that binding lives in
   Scout, not in ark.
4. A self-hoster has no workspaces at all, so the authorization model would only
   exist for elkproject's deployment.

The Elk assertion may carry the principal's workspace ids for display and for a
future convenience (suggesting grants), but nothing in `ark-server` may read them
to make an access decision.

**The actor half of spec §20.** `principal` and `repository permission` are the
tables above. `actor` is the binding between them, and it needs care, because the
client re-uploads every actor it knows on every push — `internal/sync/sync.go:96`
calls `Store.AllActors` (`internal/store/sync.go:48-49`), which after a pull
includes actors belonging to other people. So the rule cannot be "every actor in
the payload must belong to the pusher".

The rule is **first-writer-binds, enforced on mutations**: `upsertActor`
(`internal/server/server.go:195-217`) is already insert-if-absent and never
modifies an existing actor, so it gains one column — the principal that first
introduced this actor id. Enforcement then happens on `Mutation.CreatedBy`
(`pkg/api/api.go:28`), which the server currently ignores entirely: a push is
rejected if any mutation's `created_by` names an actor bound to a different
principal. Re-sending someone else's actor record is a harmless no-op; writing
*as* them is a `permission` rejection.

This is a genuinely new use of an existing wire field. No protocol change, no
client change.

## Decision 5 — agents reuse the delegating human's credential; delegation is enforced, not credentialed

An ark agent actor is created on first use with `delegated_by` set to the
repository's default human (`internal/app/app.go:98-105`,
`internal/store/actors.go:48-72`), driven by `--agent` / `ARK_AGENT_NAME` /
`ARK_DELEGATED_BY` (`internal/cli/root.go:32-34`). This is load-bearing: RFC-0002
found that in the Signal repository "100% of Ark records were written by an agent
actor and the human actor authored nothing" (rfc-0002:277-281).

An agent running on a human's machine gets **no separate credential**. It runs in
that human's shell, with that human's filesystem, and `cloud.ResolveToken`
(`internal/cloud/creds.go:33-52`) will hand it the keychain entry regardless. A
distinct token the agent can read from the same keychain confers no isolation; it
is a second secret to manage in exchange for the appearance of a boundary.

What the server gains instead is enforcement of the chain ark already records.
On push, for every newly introduced actor with `type = "agent"`, `delegated_by`
must name a human actor bound to the pushing principal. Absent or dangling
`delegated_by` on a new agent actor is a `permission` rejection. Today
`server.go:158-166` upserts actors with no checks at all, so an unrelated caller
can introduce an agent claiming to act for anyone. That check is the entire
server-side delegation story and it costs one lookup.

**The exception that earns its own credential: an agent with no human at the
keyboard.** CI, a cloud runner, or an Elk-dispatched executor (Elk's
`connected_executors` / `claim_run` machinery, `supabase/migrations/
0017_connected_executors.sql`, `supabase/functions/elk-mcp/queue.ts`) has no
human keychain to borrow from. For that case only:

```sh
ark token create --agent ci --repo <id> --level write --expires 30d
```

issued by a human principal, producing a `principals` row with `kind = 'agent'`,
`delegated_by = <issuing principal>`, grants that are a subset of the issuer's,
and a real expiry. Revoking the human revokes everything they delegated. This is
the scoped derived token — applied only where the isolation it claims is real.

**Rejected: Elk mints agent credentials.** Elk has already recorded the opposite
position and it is the right one. `supabase/migrations/0033_do_handshake.sql:4-7`:
"Elk is coordinator/reviewer, never executor, never credential broker. The
handshake packet NAMES required capabilities; values never pass through Elk.
Agents pull work and hold their own keys." An Elk-minted ark credential would
contradict Elk's own locked decision.

## Decision 6 — the identity provider is configuration, not a compile-time dependency

`ark-server` never contains the word "Elk". Three environment variables, added to
the table at `docs/self-hosting.md:39-46`:

| Variable | Required | Meaning |
|---|---|---|
| `ARK_IDP_APPROVAL_URL` | no | Where `ark login` sends the user. Unset → no device flow. |
| `ARK_IDP_KEY` | with the above | Shared secret the IdP presents to `POST /v1/device/approve`. |
| `ARK_BOOTSTRAP_TOKEN` | no | Accepted only on `POST /v1/principals`, to mint the first principal. |

`GET /` — already unauthenticated and already returning a service banner
(`internal/server/server.go:34-44`) — gains an `auth` object reporting whether a
device flow is available and its approval URL. `ark login` reads it and picks its
mode with no client configuration. That is the whole discovery mechanism.

**A self-hoster with no Elk** sets `ARK_BOOTSTRAP_TOKEN` to a random string
(exactly as they set `ARK_API_TOKEN` today), runs

```sh
ark principal create --email me@example.com --bootstrap <token>
```

which prints an `arkc_…` credential, and pipes it into `ark login` — the stdin
path at `internal/cli/remote.go:89-99`, unchanged. From then on they are an
admin and issue grants from the CLI. Their experience is today's experience for
the first user and strictly better after: per-person credentials, individual
revocation, and per-repository permissions, with no identity provider anywhere.

This mirrors RFC-0002's Decision 3 verbatim in principle — "Someone self-hosting
`ark-server` should never discover an Elk webhook in it" (rfc-0002:109-111). The
same sentence applies to an Elk login. The Elk-specific code in this design is a
Supabase edge function that lives in Elk's repository and calls one documented
ark endpoint. Nothing in ark imports anything from Elk.

## The flow

```
  ark login                    ark-server                 IdP (Elk edge fn)          browser
     │                             │                            │                       │
     ├─ POST /v1/device/code ─────►│                            │                       │
     │◄── user_code, uri, 900s ────┤  writes pending row        │                       │
     │                             │  to auth.db (CAS)          │                       │
     ├─ prints  XXXX-XXXX  +  verification_uri ────────────────────────────────────────►│
     │                             │                            │◄─ sign in (email OTP)─┤
     ├─ poll /v1/device/token ────►│                            │◄─ types XXXX-XXXX ────┤
     │◄── 428 pending ─────────────┤                            │                       │
     │                             │◄─ POST /v1/device/approve ─┤  auth.getUser(jwt),   │
     │                             │   { user_code, subject,    │  then server-to-server│
     │                             │     email, display_name }  │  with ARK_IDP_KEY     │
     │                             │  mint arkc_…, store sha256 │                       │
     ├─ poll /v1/device/token ────►│                            │                       │
     │◄── 200 { token, principal } ┤  deletes pending row       │                       │
     │                             │                            │                       │
     └─ cloud.StoreToken(remote, token)  →  macOS keychain, else ~/.ark/credentials.toml
```

**The client barely changes.** `cloud.StoreToken` (`internal/cloud/
creds.go:56-70`) and `cloud.ResolveToken` (`creds.go:33-52`) are untouched — an
`arkc_…` value is a string like any other, stored in the same keychain entry
keyed by remote host, resolved in the same `ARK_TOKEN` → keychain → `~/.ark/
credentials.toml` order that `docs/self-hosting.md:227-235` documents. CI and
agents keep setting `ARK_TOKEN`. `ark login --token` keeps working for the
paste path. The only additions are the device-flow branch and printing who you
logged in as.

**Server-side request handling** replaces the body of `s.auth`
(`internal/server/server.go:71-80`) with: hash the presented bearer, look it up
in the cached `auth.db`, reject if missing, revoked, expired, or its principal is
disabled, and attach the principal to the request context. Route handlers then
consult `grants` for the repository they are about to touch, returning the
existing `permission` error code (`docs/v1-spec.md:1223-1249`; exit code 5 per
`CLAUDE.md:33-34`), which the client already maps correctly
(`internal/cloud/client.go:75-80`).

**Caching.** The instance holds `auth.db` in memory with the same generation-CAS
discipline as repository databases, plus a 60-second hard TTL so a revoke lands
globally within a minute. At `--max-instances 1` it is effectively immediate.

**One concrete consequence to not overlook.** In `DATA_DIR` mode the `/blobs/`
routes are signed with the service token as the HMAC key —
`internal/server/server.go:59-67` sets `local.Secret = s.Token`, consumed at
`internal/server/blobs.go:93-96,104-108`. When the service token stops being a
bearer, that signing key needs an explicit home or local-mode artifact URLs break.
It becomes `ARK_SIGNING_KEY`, defaulting to `ARK_API_TOKEN` while that still
exists. The comment at `server.go:62-63` ("The service token is the signing key,
so there is nothing extra to configure") stops being true and must be updated.

## Migration

Staged, both mechanisms live simultaneously, retirement gated on evidence rather
than on a date. Three repositories (ark, signal, pulse) currently depend on the
service token; none may break at any stage.

**Stage 1 — dual verification, no behaviour change.** Ship `auth.db`, the three
tables, and the new verifier. `s.auth` tries the legacy comparison first: a match
synthesizes a `legacy` principal holding implicit `write` on every repository and
logs that fact. Everything else falls through to credential lookup. Existing
clients cannot tell the difference. Deployable and revertible on its own.

**Stage 2 — issue credentials.** Deploy the Elk `ark-auth` function, set
`ARK_IDP_APPROVAL_URL` and `ARK_IDP_KEY`. `ark login` with no arguments now runs
the device flow. Founders re-login. A one-off backfill writes `admin` grants for
each founder principal on every currently-registered repository — the set is
small and known. The legacy token still works throughout.

**Stage 3 — narrow, on an announced date** (resolved decision 3). The cutover
date is set and published when Stage 2 completes; new principals are on the new
system from that point regardless. Two weeks before the date, set
`ARK_LEGACY_TOKEN=readonly`: legacy bearers may pull, not push. This catches a
forgotten CI job as a `permission` error on a write rather than as silent
success, and is reversible by one environment variable.

The observability still matters — the server logs a principal id on every
request, so `last_used_on` and the logs answer "who has not moved yet?" — but it
now tells you **who to chase before the date**, rather than deciding when the
date is. Waiting for a week of provable silence was the safer rule and also an
indefinite one: a monthly job means it never arrives.

**Stage 4 — retire.** `ARK_API_TOKEN` becomes optional; unset, the legacy branch
is simply not registered. `ARK_SIGNING_KEY` and `ARK_BOOTSTRAP_TOKEN` take over
its two remaining jobs. `docs/adoption.md:68-71` loses the `gcloud secrets` step
and `docs/ops/elkproject-deployment.md:104-109` loses the collaborator
instructions entirely. Only then does `docs/self-hosting.md:211-235` get rewritten.

## What ships in the first slice

Deliberately small, chosen so each piece is useful before the next exists:

1. `auth.db` over `repodb.Backend` with the three tables, plus the verifier and
   the dual-path `s.auth`. Legacy token still primary. Nothing user-visible.
2. `ARK_BOOTSTRAP_TOKEN` and `ark principal create` — per-principal credentials
   with no identity provider at all. This alone fixes self-hosting and is
   testable with no external services, matching `CLAUDE.md:11-14`.
3. Grants, `ark repo grant`, and the `Mutation.CreatedBy` actor-binding check.
4. The device flow endpoints and the `ark login` branch, against a stub approver
   in tests.
5. The Elk `ark-auth` edge function.

Not in the slice: `ark token create` for headless agents, sliding expiry,
multiple credentials per principal beyond the schema allowing it, and any
Elk-side surfacing of ark grants.

## Costs accepted

- **`auth.db` is a new failure domain and a new single point of contention.**
  Every `/v1` request depends on one object. Mitigated by aggressive caching and
  by the fact that it is read-mostly, but a corrupt `auth.db` locks everyone out
  of everything. The recovery path is the same one RFC-0001 established for
  repository databases — it is a file in a bucket; download it, open it, fix it —
  and `ARK_BOOTSTRAP_TOKEN` is a working break-glass that does not depend on it.
- **Revocation is eventually consistent, bounded at 60 seconds.** Accepted
  because the alternative is a read per request with no cache.
- **Trust in the IdP's email verification.** Whoever controls `alice@example.com`
  at the IdP gets Alice's grants. This is the trust model of every invite system,
  and Elk's OTP sign-in verifies the address by construction, but it is real: an
  Elk account takeover is an ark account takeover. It is also strictly better
  than today, where anyone who reads a GCP secret gets *everyone's* access.
- **An agent on a human's machine has the human's full authority.** Stated
  plainly rather than papered over with a token that provides no isolation. The
  mitigation is accountability, not confinement: `delegated_by` is recorded and
  now enforced.
- **Grants are per-repository and manual.** With three repositories and a handful
  of people this is a few commands. It will not remain acceptable at thirty
  repositories, which is an exit trigger below rather than a reason to build
  groups now (principle 005).
- **`ark login` now requires a browser somewhere.** Not necessarily on the same
  machine — that is the point of the device grant — but a headless environment
  with no browser at all must use `ARK_TOKEN` with a token minted elsewhere.
  This is the normal shape of every CLI device flow.
- **Two credential kinds exist during migration.** Stages 1-3 mean the server has
  two ways to say yes. Bounded in time and gated on observed usage, but it is
  genuinely more code than either end state.

## Exit triggers

1. **A third party wants to verify ark credentials** (a separate read-only
   service, a dashboard) → the opaque token stops paying for itself and a signed
   token with a published key set becomes worth its weight. Decision 2 reverses;
   nothing else does.
2. **Grant administration becomes a chore** (roughly: more than ~20 repositories,
   or grants issued weekly) → introduce a group or org primitive, at which point
   deriving membership from the IdP (Decision 4's rejected alternative) deserves
   a second hearing, because the objection was partly that it imports a primitive
   ark lacks.
3. **Write contention on `auth.db`** — many concurrent logins losing CAS races →
   split pending device codes into their own object, or move `auth.db` off the
   CAS backend entirely. Same trigger shape as RFC-0001's.
4. **A second identity provider** (GitHub, a customer's SSO) → the assertion
   contract is already generic, but `ARK_IDP_KEY` as a shared secret should
   become a detached Ed25519 signature over the assertion so ark can trust
   several issuers without sharing one secret with all of them.
5. **Ark grows a web UI** → grants get a screen, `ark repo grant` stays as the
   scriptable path, and the device flow's approval page could move in-house.

## Non-goals

- **Ark does not gain workspaces, teams, organizations, or roles.** Three grant
  levels on a repository. Principle 005.
- **Elk does not become mandatory.** A self-hoster who never hears the word
  "Elk" gets per-principal credentials, grants, and revocation.
- **Ark does not become an identity provider for anything else.** It issues
  credentials for itself only.
- **No SSO, SCIM, provisioning, audit-log export, or session management.**
- **No end-to-end encryption or per-record ACLs.** Grants are per repository;
  a principal with `read` on a repository reads all of it.
- **No change to the sync protocol.** `pkg/api` is untouched apart from the new
  device-flow types. `Mutation.CreatedBy` becomes authorization-relevant, but it
  is an existing field the server had simply never read.
- **The Elk-side ingest credential is out of scope.** `ARK_ELK_TOKEN`
  (`internal/cli/elk.go:70,149`) authenticates ark *to* Elk for RFC-0002's
  adapter, which is the opposite direction. It has its own shared-secret story
  (`supabase/migrations/0060_ark_ingest_token.sql:20-21`) and should get its own
  treatment.
- **This does not fix Elk's own credential posture.** `connector_tokens` remains
  plaintext under blanket RLS (`0003_chat_session_capture.sql:16-30`,
  `0001_init.sql:182-193`). Elk's problem, recorded here only because this RFC
  deliberately does not build on that table.

## To confirm about Elk before implementing

Every claim above about Elk is cited from its repository. These are the places
where the repository does not answer the question, and they must be settled with
the Elk deployment rather than assumed:

1. **Does Elk have any authenticated browser surface at all?** All evidence
   points to native clients — Swift, Kotlin, .NET (`ElkKit/`, `Apps/`) — signing
   in with email OTP. No web sign-in was found. The `ark-auth` approval page in
   Decision 3 would therefore be Elk's *first* browser-facing authenticated
   surface. It is designed to be self-contained for exactly that reason (it runs
   its own OTP sign-in rather than assuming a session), but the premise that a
   user is "already signed in to Elk in a browser" is **not supported by
   evidence** and should not be relied on.
2. **The Supabase JWT algorithm and whether a JWKS endpoint exists.**
   `supabase/config.toml:165-169` declares neither; `signing_keys_path` is
   commented out. There is circumstantial evidence of a rotation to new-style
   `sb_secret_…` keys (`supabase/functions/meet-reconcile/index.ts:49-58,83-95`)
   which would imply asymmetric signing. This design does not depend on the
   answer — that is Decision 1's whole point — but confirm it before anyone
   proposes the JWKS alternative again.
3. **Which migrations are actually applied to production.** `0045`
   (`access_role`), `0034` (`owner_id`), and `0060` (`ark_ingest_token`) all
   carry `APPLY IS USER-GATED` headers and only `0049` is marked applied. If the
   assertion is to carry a role, confirm the column exists in the live database.
4. **Where the `ark-auth` function holds `ARK_SERVER_ORIGIN` and
   `ARK_IDP_KEY`.** The established Elk pattern for an outbound secret is a
   `private.*` config table behind a `security definer` RPC granted only to
   `service_role` (`0016_work_record_connector.sql:88-108`,
   `0018_claude_routine_dispatch.sql:16-21`). Confirm that is still the
   convention rather than a function environment variable.
5. **Whether Elk wants the approval to record anything.** An "Alice paired a
   device to ark" event is plausible and cheap, but Elk has no existing table for
   it and this RFC does not propose one.

## Resolved by the owner, 2026-07-28

All three queued decisions are settled. The reasoning that produced them is
kept below, because the alternatives explain why these are the answers.

**1. The approval surface is a web page.** As specified: a self-contained page
served by an edge function. It ships without a native release and does not
depend on a browser session Elk may not have. Ark's `ark login` therefore never
blocks on a mobile release cycle.

**2. The default grant is `read`, seeded from workspace membership — not
blanket read, and not deny.** A principal gets `read` on the repositories bound
to workspaces they already belong to; everything else stays deny, and `write`
always requires an explicit grant.

This needs care, because taken naively it reverses Decision 4. It does not, and
the distinction is the whole point: **workspace membership seeds grants at
pairing time; it is never consulted on the request path.**

Concretely, ark does not learn what a workspace is. At `POST /v1/device/approve`
the identity provider already asserts the principal's verified email; it also
asserts the list of **ark repository IDs** that principal may read, which Elk
computes on its own side from `source_bindings` joined to workspace membership.
`ark-server` stores those as ordinary per-repository grants and enforces them
exactly like any other. Elk seeds; ark owns.

That keeps every property Decision 4 was protecting — no Elk call on the request
path, no workspace primitive inside ark, and a self-hoster who simply seeds
nothing — while giving the behaviour the owner asked for: a teammate who can
already see a project in Elk can read its Ark records without a support ticket.

Two consequences to implement rather than discover:

- **Grants are re-seeded on every login**, not only the first, or a repository
  bound after someone paired would be invisible to them forever.
- **Seeding only ever adds `read`.** It never removes a grant and never grants
  `write`, so losing Elk workspace membership does not silently revoke Ark
  access mid-session — revocation stays an explicit, auditable act.

`ARK_DEFAULT_GRANT` therefore takes a third value and defaults to it:
`none | read | seeded` (default `seeded`). A self-hosted deployment with no
identity provider gets `none` for free, since nothing seeds anything.

**3. The legacy token retires on a date, as a cutover.** Stage 3's
"week of zero observed usage" gate is replaced by an announced date. New
principals use the new system from day one; the legacy token keeps working
until the date and then stops. The readonly stage stays as the soft landing,
and the observability from Stage 2 becomes the thing that tells you who still
has to move *before* the date rather than the thing that decides when the date
is.

---

## The reasoning behind them

Everything above was decided on evidence. These three were genuine judgment
calls, recorded here so the choices above can be re-argued if their premises
change.

> **1. Where does the approval page live — a new Elk web surface, or the native
> Elk apps?**
>
> The RFC specifies a Supabase edge function serving a self-contained page,
> because it ships without a native release, works from any device, and does not
> assume a browser session Elk may not have (see "To confirm" #1). The
> alternative is a "Pair a device" screen in the native apps, which reuses an
> already-authenticated session and Elk's existing design system, but requires
> shipping four clients (`Apps/ElkiOS`, `ElkMac`, `ElkAndroid`, `ElkWindows`)
> before a single developer can run `ark login`, and leaves anyone without the
> app installed unable to authenticate at all.
>
> Choosing native makes this RFC depend on a mobile release cycle. Choosing the
> edge function adds a web surface to a product that has deliberately had none.

> **2. Is `read` the default grant for a known principal, or is it deny?**
>
> The RFC specifies deny — a principal with no grant on a repository cannot see
> it. That is the correct security default and it is also the one that will
> generate support questions in a three-person project where everyone expects to
> see everything. The alternative, "any authenticated principal gets `read` on
> every repository; `write` requires a grant", is closer to how these
> repositories are actually used today and closer to what the service token
> already provides, so it is not a regression — but it means a repository is
> readable by anyone who can sign in to Elk, and Elk sign-in is open to any email
> address that receives an OTP.
>
> This is a one-line default (`ARK_DEFAULT_GRANT=none|read`) and it should be set
> deliberately, not discovered.

> **3. Does the legacy token retire on a date or on evidence?**
>
> The RFC specifies evidence — Stage 3 waits for a week of zero observed legacy
> usage. That is safer and it is also indefinite: "zero for a week" may never
> arrive if some forgotten job polls monthly. A date forces the issue and makes
> the breakage happen at a time someone is watching. Given that only three
> repositories and a handful of clients exist, a date is more defensible here
> than it would be at scale, and the readonly stage already provides a soft
> landing.

---

## Amendments

Decisions 1-6 above are accepted and are not rewritten. What follows are later
rulings that add to them, each with its date and the person who made it.

### Amendment 1 — operators: who may act on the service as a whole

**Ruled by Issac, 2026-08-30 (D116). Filed as elk-work/ark#94. Implemented in
`internal/server/operators.go`.**

Two acts had no authorization model. Revoking a credential was reachable only
by editing `auth.db` by hand, and listing principals was not reachable at all
— which made revocation unusable even where it existed, because a credential
is retired by an id that is printed once at issue and never again. Neither act
is *about* a repository, so Decision 4's three levels had nothing to say about
either, and Decision 6 confines `ARK_BOOTSTRAP_TOKEN` to one route, so the one
service-wide secret could not be the answer without amending an accepted
decision.

**The answer is an operator: an ordinary principal that may perform the two
service-wide acts.** Named, holding a credential of its own, and revocable
through the same route it uses to revoke anybody else's. That is the property
the alternative did not have — widening the bootstrap token would have made one
environment variable the whole operator identity, so every service-wide act in
the log would have been attributed to a string several people hold and nobody
owns.

**It is a column, not a `kind`.** The ruling offered either; the implementation
takes `principals.operator_since TEXT`, nullable, its presence being what makes
an operator. `kind` answers a different question — *what* holds this
credential, a human or an agent — and Decision 5 turns on that answer. Spending
that field on authority collapses two axes into one, so promoting a person
would erase the fact that they are a person, and an operator that is a CI agent
could not be described at all. A timestamp column is also the schema's own
idiom for a state with a time on it (`disabled_at`, `revoked_at`, `granted_at`),
and it demotes by going back to NULL, which a repurposed `kind` cannot do
without remembering what it used to say.

`credentials.revoked_by TEXT` lands beside it, because the non-goals rule out
an audit-log export and a revocation still has to say whose act it was.

**Where an operator comes from.**

- The **first** comes from the bootstrap token, and only into a vacuum: `POST
  /v1/principals` promotes the principal it mints when the service has no
  operator at all. After that `ARK_BOOTSTRAP_TOKEN` can still mint — Decision 6,
  unchanged — but it cannot promote, and asking it to is refused rather than
  ignored. So a leaked bootstrap secret can create a principal and cannot make
  itself an authority over the service.
- **Every operator after the first is made by an operator**, presenting their
  own credential to the same route. Decision 6 says where the bootstrap token
  may be presented; it does not say what else that route may accept, so this is
  an addition to Decision 6 rather than a change to it.

**The legacy service token is deliberately not an operator**, though it carries
implicit `admin` on every repository. The point of this amendment is that a
service-wide act is attributed to somebody, and letting the string the whole
fleet holds perform one would leave the model built and unused. elk-work/ark#54
asks that the legacy break-glass not be removed without a replacement: this is
the replacement, and the break-glass for a service with no operator is
`ARK_BOOTSTRAP_TOKEN` on the route it was already confined to.

**Routes** (§19 of `v1-spec.md` carries the list; §20.2 the contract):

```text
GET  /v1/principals                 operator — the roster, and each principal's credentials
GET  /v1/credentials                any principal — its own credentials, and nobody else's
POST /v1/credentials/{id}/revoke    operator, or the credential's own holder
POST /v1/principals                 + an operator's credential, beside ARK_BOOTSTRAP_TOKEN
```

`GET /v1/credentials` is option 3 of elk-work/ark#94 kept as a subset of option
2, and it is not decoration: a laptop goes missing on a Saturday and its owner
should not have to find an operator before the credential on it stops working.
It is also what makes self-revocation addressable at all, since the id is the
thing that was lost with the machine.

**Revocation remains eventually consistent, bounded at `authTTL`.** Nothing
here changes the caching contract of Decision 2; the write drops the issuing
instance's cache, so a revocation is immediate there and lands elsewhere within
the minute. The CLI says so, because a still-served request in that window
otherwise reads as the revocation having failed.

**Attribution.** Every operator act is logged against the named principal and
the credential it presented — `operator`, `operator_email`, `credential` — and
never against a shared secret. The one line that can name a secret is the
first-operator promotion, logged as `issued_by=bootstrap-token`, which by
construction only ever appears on a service that had no operator to name.

**Not built, and left as gaps rather than discovered as ones:**

- **No demotion route.** An operator is promoted and never demoted. Ending a
  departed operator's access is revoking their credentials and disabling the
  principal, both of which exist; removing the flag is an edit to `auth.db`, as
  every principal-state change was before this amendment.
- **No route disables a principal.** `disabled_at` is honoured by the verifier
  and written by nothing, exactly as `revoked_at` was before #94. It is the
  next thing of this shape to build and it is not in scope here.
- **No audit table.** The non-goals rule out audit-log export; the log line and
  `credentials.revoked_by` are the record.

**Storage.** Both columns are added to `authSchema` for a store created today
and applied as tolerated `ALTER TABLE … ADD COLUMN` for one that already exists
(`addColumns`, `internal/server/authstore.go`). Every `auth.db` in existence
predates this amendment, so the second path is the one that runs; it happens on
the next read or write of the store, which means **there is no migration
command and no deploy step** beyond shipping the binary.

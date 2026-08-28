# Self-hosting the Ark sync service

`cmd/ark-server` is the shared authority behind `ark sync`. It is a single
Go binary with no always-on database and no external dependencies beyond
the storage you point it at. This document is the general guide: run your
own instance, on your own machine or your own cloud account.

The maintainers' concrete environment — project, bucket, Cloud Run
service, service account, deploy command — is documented privately in the
`elk-work/elk` meta-repo as `docs/ark/elkproject-deployment.md`. You do
not need it, and nothing here depends on it. (`docs/ops/` used to hold
that file and no longer exists here; the RFCs still cite it by its old
path.)

## What the service stores

Two things, both plain files:

```text
repos/<repository-id>.db        one SQLite database per repository
sha256/<xx>/<sha256>            content-addressed artifact blobs
```

A repository's whole shared history — records, per-record revisions,
field revisions, applied mutation outcomes, blob bookkeeping — is one
SQLite file. The file is the tenant boundary: copying, exporting, or
deleting a repository is one file operation. Artifact content is stored
separately, keyed by SHA-256, so identical bytes are stored once.

There is no server process to keep warm and no database to provision.
A request fetches the repository database, applies the change in one
transaction on a working copy, and writes the file back with a
compare-and-swap on the storage generation. Lost races refetch and
replay; the sync protocol is idempotent by design
(`applied_mutations` returns the stored outcome for a replayed mutation
ID). See [rfc-0001-per-repo-sqlite-storage.md](rfc-0001-per-repo-sqlite-storage.md).

## Configuration

The server takes **no command-line flags**. Everything is environment:

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `ARK_API_TOKEN` | always | — | Bearer token clients must present. Startup fails without it. |
| `ARK_SIGNING_KEY` | no | `ARK_API_TOKEN` | HMAC key for local-mode `/blobs/` URLs. Unset it is the service token, which is what it has always been; set it, the two are independent. Ignored in object-storage mode, where GCS signs. |
| `ARK_BOOTSTRAP_TOKEN` | no | — | Accepted on `POST /v1/principals` and no other route, to mint the first per-principal credential. Unset, that route refuses everything and the service token is the only way in. See [Per-principal credentials](#per-principal-credentials). |
| `GCS_BUCKET` | object-storage mode | — | Google Cloud Storage bucket for repository databases and blobs. Set → object-storage mode; unset → local mode. |
| `BASE_URL` | local mode | — | Externally reachable base URL of this service. Required when `GCS_BUCKET` is unset; used to build blob URLs. Startup fails without it. |
| `DATA_DIR` | no | `data` | Local mode only. Repository databases go in `<DATA_DIR>/repos`, blobs in `<DATA_DIR>/blobs`. Relative paths resolve against the working directory. |
| `CACHE_DIR` | no | `<os temp dir>/ark-repos` | Scratch space for working copies of repository databases and of `auth.db`. Created at startup. Safe to lose — it is a cache, refetched on demand. |
| `PORT` | no | `8080` | Listen port. |

The mode is chosen solely by whether `GCS_BUCKET` is non-empty. There is
no flag, no config file, and no way to mix the two.

## Building

```sh
go build ./cmd/ark-server      # binary in ./ark-server
make build-server              # bin/ark-server, version stamped from git describe
```

Pure Go, no CGO, no C toolchain (the SQLite driver is `modernc.org/sqlite`).
Either form runs; the stamp only affects the version the server logs at
startup and reports on `GET /`.

The repository root `Dockerfile` builds the same binary into a distroless
image if you want a container:

```sh
docker build -t ark-server .
docker build --build-arg VERSION="$(git describe --tags --always --dirty)" -t ark-server .
```

Without the build arg the image reports `dev` plus whatever VCS stamp Go
embedded, which is nothing when the build context has no `.git` — the
usual case for source-upload builds on a hosted builder. Pass `VERSION`
if you want deployed revisions to be identifiable.

---

## Mode 1 — local (`DATA_DIR`)

The zero-dependency path. Everything lives on one disk: repository
databases, artifact blobs, and the scratch cache. Nothing else is
required — no cloud account, no object store, no credentials beyond the
token you choose.

```sh
ARK_API_TOKEN=$(head -c 32 /dev/urandom | base64) \
DATA_DIR=/var/lib/ark \
BASE_URL=http://localhost:8080 \
PORT=8080 \
./ark-server
```

After a first sync the directory looks like:

```text
/var/lib/ark/
  repos/01J....db          one per repository
  blobs/sha256/ab/abcd...  artifact content
```

### The local blob store

In object-storage mode the service hands clients time-limited signed URLs
that point at the bucket, and blob bytes never pass through the service.
In local mode the service signs and serves the blobs itself, on two extra
routes:

```text
PUT /blobs/<key>?exp=…&sig=…     upload artifact content
GET /blobs/<key>?exp=…&sig=…     download artifact content
```

`POST /v1/artifacts/upload-url` returns a signed `<BASE_URL>/blobs/<key>`
and the client `PUT`s to it. This is why `BASE_URL` is required in this
mode and why it must be the URL clients actually reach — if it is wrong,
record sync still works and artifact upload silently targets an
unreachable host.

Three things to understand before exposing this mode:

- **The `/blobs/` routes carry a signature, not the bearer token.** They
  cannot use the bearer middleware: a client treats them as pre-signed
  URLs and sends no `Authorization` header. So each URL is signed with
  HMAC-SHA256 over the method, the key, and an expiry, keyed by
  `ARK_SIGNING_KEY` — which defaults to `ARK_API_TOKEN`, so there is still
  nothing extra to configure and a service with no key at all cannot mint
  blob URLs. Signatures last one hour and are method-bound, so a download
  URL cannot be replayed as an upload.

  Set `ARK_SIGNING_KEY` when you want the two to be independent — for
  instance before rotating `ARK_API_TOKEN`, since changing the signing key
  invalidates every outstanding blob URL. It matters more than that shortly:
  per-principal credentials
  ([RFC-0003](rfc-0003-elk-issued-credentials.md)) end with `ARK_API_TOKEN`
  no longer being a bearer, and a signing key with no home of its own would
  go with it.
- **Uploads are verified against their hash.** `POST /v1/artifacts/confirm`
  streams the stored object, recomputes SHA-256, and refuses — deleting the
  object — if it does not match the key it was stored under. This is what
  makes "artifacts are immutable by checksum" true rather than merely
  claimed, and it applies in both modes. Objects larger than 1 GiB are
  refused rather than trusted unverified.
- **Blob keys are path-checked, not sandboxed.** Keys containing `..` or
  an absolute path are rejected, and that is the whole defense.

**Use local mode for development, tests, and a single machine or small
trusted team on a private network.** It keeps no redundancy: the disk is
the only copy. For anything else, use object storage, or put the service
behind a reverse proxy that terminates TLS and restricts network access.

---

## Mode 2 — object storage (GCS)

Set `GCS_BUCKET` and the service stores repository databases and blobs as
objects in that bucket, using the object generation as the
compare-and-swap primitive.

```sh
ARK_API_TOKEN=... GCS_BUCKET=my-ark-bucket ./ark-server
```

`BASE_URL` and `DATA_DIR` are ignored in this mode. Blob URLs are V4
signed URLs against the bucket, valid for **one hour** for both upload
and download.

### GCS is the only object backend implemented

There is no S3 backend, no MinIO backend, and no generic S3-compatible
path. The storage abstraction is a two-method interface
(`Fetch` / `Store` by repository ID) plus a three-method blob interface
(`SignedPutURL` / `SignedGetURL` / `Exists`), and today exactly two
implementations exist for each: Google Cloud Storage and a local
directory. Adding another object store is a contained change — one type
per interface — but it does not exist yet. Do not point an S3-compatible
endpoint at `GCS_BUCKET` and expect it to work.

### What the deployment needs

1. **A bucket.** One bucket holds both `repos/*.db` and
   `sha256/**`. Nothing in the layout requires separate buckets.
2. **Credentials with object read/write on that bucket.** The Go client
   uses Application Default Credentials: a workload identity / attached
   service account in cloud, or `GOOGLE_APPLICATION_CREDENTIALS` pointing
   at a key file elsewhere. Object-level admin over the bucket is enough;
   the service creates, reads, overwrites, and conditionally-writes
   objects, and never touches bucket metadata.
3. **The ability to sign URLs.** This is the requirement people miss.

### The signed-URL requirement

Artifact upload and download work by handing the client a V4 signed URL,
so the service must be able to *sign* as some identity, not merely
*authenticate* as one.

- With a **downloaded service-account key file**, signing is local: the
  private key is right there and nothing else is needed.
- With **keyless credentials** — the normal case on a managed runtime,
  where the identity is an attached service account with no private key
  on disk — there is no key to sign with. The client falls back to the
  IAM Credentials API (`signBlob`), asking IAM to sign on behalf of that
  same service account.

That fallback is an IAM call, and it is permission-checked: the identity
must hold **service account token creator on itself**. This reads as a
tautology the first time you see it — the account granting itself a role
— but it is exactly right: the service, running *as* account X, is
asking IAM to mint a signature *for* account X, and IAM requires an
explicit grant for that even when caller and subject are identical.

Symptom when it is missing: every `/v1` route works, records sync
cleanly, and only artifact upload/download fails, with a signing or
permission error out of the IAM credentials API. Records and artifacts
fail independently — a healthy sync proves nothing about signing.

Grant it on the service account resource itself (not the project), with
the account as both the resource and the member.

---

## Authentication

V1 is deliberately one token for everything (spec §20). There are no
users, no per-repository permissions, and no scopes. Anyone holding the
token can read and write every repository the service knows about.

That is a stage rather than the end state, and the first part of the end
state has landed:
[rfc-0003-elk-issued-credentials.md](rfc-0003-elk-issued-credentials.md)
replaces one shared token with per-principal credentials, per-repository
grants and individual revocation, and its bootstrap path needs no
identity provider at all. **Per-principal credentials work today** — see
[Per-principal credentials](#per-principal-credentials) below — and
per-repository grants do not yet: a valid credential currently reaches
everything the service token reaches. So the paragraph above is still the
whole authorization model, and the reason to mint credentials now is
attribution and individual revocation, not confinement.

**Server side.** `ARK_API_TOKEN` is read once at startup; empty or
missing is a startup failure. Every `/v1` route strips a leading
`Bearer ` from the `Authorization` header and compares the remainder
against the token in constant time — and, failing that, against the
credential store, if the bearer looks like a credential this service
issued. A mismatch either way is `401` with a `permission` error code.
The only routes without a bearer check are
`GET /` (a service banner), `GET /health`, and `POST /v1/principals`,
which has its own token. In local mode `/blobs/`
also skips the bearer check, but is not unauthenticated: it requires an
HMAC signature derived from `ARK_SIGNING_KEY` — which defaults to that
same token — bound to the method and carrying a one-hour expiry.

**Client side.** The token resolves in this order:

1. `ARK_TOKEN` in the environment
2. the OS keyring, service `ark`, account = the remote's host:
   - macOS: the login keychain
   - Windows: Credential Manager, as generic credential `ark:<host>`
   - Linux: the Secret Service (GNOME Keyring, KWallet, `keepassxc`, …)
3. the user credential file, keyed by remote host:
   - macOS/Linux: `~/.ark/credentials.toml`, mode 0600
   - Windows: `%USERPROFILE%\.ark\credentials.toml`, protected by a
     current-user-only ACL

Tokens are never written into the repository — not into `.ark/config.toml`,
not anywhere under `.ark/` — and never passed to another process as a
command-line argument.

`ark login` writes to the keyring. When the keyring is unavailable — no
Secret Service on a headless box, a locked or denied keychain — it says so
on stderr and falls back to the credentials file; it never degrades to
plaintext silently. `ark status` reports which of the three answered.

Set `ARK_NO_KEYRING=1` to skip the keyring and use the file deliberately;
that path is quiet, because you asked for it.

`ark logout` removes a host's credential again — the keyring entry and any
fallback-file copy, because a plaintext copy left behind is the one nobody
would notice. It succeeds whether or not there was anything to remove, and it
cannot touch `ARK_TOKEN`: if that is set it says so and exits 7, since a token
still resolves.

### Per-principal credentials

The service can issue a credential per person or per agent instead of
handing everybody a copy of `ARK_API_TOKEN`. It needs no identity
provider, no account anywhere, and nothing outside this binary.

Set one more random string on the server:

```sh
ARK_API_TOKEN=... ARK_BOOTSTRAP_TOKEN=$(head -c 32 /dev/urandom | base64) ./ark-server
```

Then, from any machine that can reach the service:

```sh
ARK_BOOTSTRAP_TOKEN=... ark principal create --remote https://ark.example.com --email me@example.com
ark login --remote https://ark.example.com      # paste the credential it printed
```

`ark principal create` prints an `arkc_…` credential **once**. The service
stores only its SHA-256, so a lost credential is reissued, never
recovered — run the command again and you get a new one against the same
principal. Pass the bootstrap token in the environment or on stdin rather
than as `--bootstrap`: an argument lands in the process table and in shell
history.

From the client's side nothing is different. A credential is a token like
any other: same `ark login`, same keyring entry keyed by remote host, same
`ARK_TOKEN` for CI. Credentials expire after 365 days, and recovery from
expiry is logging in again.

What you get for it, today:

- **Individual revocation.** Retiring one person's credential does not
  disturb anybody else, where rotating `ARK_API_TOKEN` is an outage for
  every client of every repository.
- **Attribution that is checked rather than asserted.** The service logs
  a principal id on every request and records `last_used_on` per
  credential, at day granularity.

What you do not get yet: per-repository permissions. A credential reaches
everything the service token reaches, which is why `ARK_API_TOKEN` is
still what the paragraph at the top of this section describes.

**Where the credentials live.** One SQLite database, beside the
repository databases and written the same way:

```text
repos/ark.auth.db     principals, credentials, grants
```

It is a file, like everything else here: back it up with the rest, and
recover it the same way (see [Backup and recovery](#backup-and-recovery)).
If it is lost, the service token still works and
`ARK_BOOTSTRAP_TOKEN` still mints — neither depends on it being readable,
which is the point of both.

Revocation is cached for up to **60 seconds** across instances, so a
revoked credential can keep working for that long. On a single-instance
deployment it is effectively immediate.

### Operational warnings

- **Trailing newlines break the comparison.** The comparison is exact
  bytes; `"tok\n" != "tok"`. This is the single most common self-hosting
  failure, and it looks like a wrong token rather than a formatting
  problem: every request returns `401 invalid or missing token` even
  though the value is visibly correct.

  The usual culprit is a shell `echo` when creating the secret:

  ```sh
  printf '%s' "$TOKEN" | <your secret store: create/write>    # right
  echo "$TOKEN" | <your secret store: create/write>           # wrong: adds \n
  ```

  It bites in more places than secret creation — `$(cat token.txt)`
  strips the newline but a file mounted as an env var does not, and a
  copy-paste out of a terminal often carries one. If a token that looks
  right is rejected, check its length before you check anything else.

  The client side is more forgiving: `ark login` trims whitespace from
  what you pipe or type. The server does not trim what it is given.

- **Generate the token with a real random source** — it is the credential
  every client shares, and `ARK_BOOTSTRAP_TOKEN` mints more of them. 32
  bytes of `/dev/urandom`, base64, is fine for either.

- **Rotation is a restart.** Write the new value, restart the service,
  then `ark login` again on each client. There is no overlap window: the
  old token stops working the moment the new process is serving.

- **Do not put the token in a URL, a container image, or a command line**
  that lands in shell history. `ark login` reads it from stdin for this
  reason.

---

## Work-record write routes

Most of `/v1` is the sync protocol: `POST /v1/sync/push` speaks mutations
because the caller is expected to *be* another copy of Ark. Three routes
are not that. They let an ordinary program — a CI job, a script, a
dashboard — write a work record without reimplementing Ark's record
model. See `docs/rfc-0004-work-record-write-api.md` for why they exist
and what they deliberately leave out.

```text
POST /v1/repositories/{repo}/tasks              create a task
POST /v1/repositories/{repo}/comments           comment on a task, PR, run, or review
POST /v1/repositories/{repo}/tasks/{id}/status  move a task within the allowed set
```

`{repo}` is the repository ULID. `{id}` and every `parent_id` is the
record's ULID — never a display number, because the service renumbers
colliding numbers and a number-keyed write could land on a different
record.

**Every write is attributed to an agent acting under a human.** A request
names its writer once:

```sh
curl -sS "$ARK_URL/v1/repositories/$REPO/tasks" \
  -H "Authorization: Bearer $ARK_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"writer":{"agent_name":"ci","delegated_by":"<human actor ULID>"},
       "title":"Nightly build failed","body":"see run 4821"}'
```

The first write under a given `agent_name` creates that agent actor, and
`delegated_by` must name a human actor already in the repository — find
one with `ark task view` on any existing record, or let a client sync
first. Later writes reuse the actor and read `delegated_by` from the
stored record, so a request cannot re-point a registered agent at
somebody else.

**`Idempotency-Key` is required on the two create routes.** The service
persists with a compare-and-swap and replays the whole request on a lost
race, so a keyless create could file the same task twice. A replay
returns the original response with `Idempotency-Replayed: true`. The
status route does not need one: asking for the status a task already has
returns the record and writes nothing.

The response is the record as written, including the ULID and — for a
task — the display number, which is final at that moment because the
service allocated it:

```json
{"record":{"record_type":"task","record_id":"01K3…","data":{"number":41,…},
 "server_revision":412},"server_revision":412}
```

Clients receive it on their next `ark sync`.

These routes sit behind the same bearer token as everything else, with
the same consequence recorded above: anyone holding it can write to every
repository the service knows about.

---

## Health check

```text
GET /health   ->  200 "ok"
```

Unauthenticated, no dependencies touched: it does not open storage, so it
answers "the process is up and serving", not "storage is reachable".

`GET /` is also unauthenticated and returns the service banner, including
the build stamp when one is known:

```json
{"service":"ark-sync","api":"v1","version":"v0.1.0"}
```

That is the quickest way to confirm which revision is live. The same
version is attached to every startup log line.

**The endpoint is `/health`, not `/healthz`, deliberately.** On Google
Cloud Run's `run.app` hostnames, Google Frontend intercepts `/healthz`
and serves its own 404 before the request ever reaches the container —
no request appears in the container's logs, which makes it look like a
routing bug in your own service.

The portable lesson generalizes past that one vendor: **reverse proxies,
load balancers, and platform frontends reserve paths**, and `/healthz`
in particular is a common one because it is the Kubernetes convention.
If a route 404s and nothing shows up in your application logs, the
request is not reaching your application — check what sits in front of
it before debugging the handler.

---

## Client setup

Per repository, once:

```sh
cd your-git-repo
ark init                                  # if not already initialized
ark remote set https://ark.example.com    # http(s), trailing slash trimmed
ark login                                 # token from stdin, or --token
ark sync
```

`ark remote set` writes the URL to `.ark/config.toml`. `ark login`
stores the token outside the repository (the OS keyring, else
`~/.ark/credentials.toml`) and prints which one it used. Piping is
preferred over `--token` to keep the value out of shell history:

```sh
<your secret store: read the token> | ark login
```

PowerShell can read a token from a file without putting it in command history:

```powershell
Get-Content -Raw path\to\ark-token.txt | ark login
```

For CI and agents, skip `ark login` and set `ARK_TOKEN` in the
environment; it wins over both other sources.

`ark sync` registers the repository (idempotent, it doubles as a token
check, and it carries this checkout's actors so the server knows who is
writing even before anything is pushed), pushes pending mutations,
uploads artifact blobs the server does not have, then pulls records
after the local cursor.

**Exit code 7 means the sync worked and the repository needs a person.**
Two causes. Either the server refused one or more changes, which this
checkout is still holding — `ark sync` names each rejection and
`ark status` counts them until the records agree again. Or the server
answered with a revision *below* one this checkout had already synced
past, which cannot happen to a healthy repository: a revision counter
only increases, so the server is no longer serving the history this
checkout was tracking, and records it previously acknowledged may be
gone. A scripted caller should treat 7 as "look at this", and the second
form as an incident.

**If you see the second form, check your storage before doing anything
else.** It is what a lost or rolled-back `repos/<repository-id>.db`
looks like from a client — a restore from an older copy, a deleted
object, or a repository that was missed by a migration. Ark deliberately
does not reconcile it: the client still holds its own records and its
mutation log, and which side is authoritative is your call, not the
tool's.

### A second machine

The repository ID is the join key. Read it from the first machine
(`ark status`, or `.ark/config.toml`), then:

```sh
cd your-git-repo            # same Git repo, second clone
ark init --repository <repository-id>
ark remote set https://ark.example.com
ark login
ark sync
```

Without `--repository`, `ark init` mints a *new* repository ID and the
second machine syncs into an empty, separate history — a quiet failure
that looks like "sync worked but nothing came down". Repository IDs may
not contain `/`, `\`, or `.`; the server rejects them.

---

## Backup and recovery

The recovery path is deliberately boring, and none of it depends on Ark
being available or even working.

There are two recoveries, and they answer different questions:

| | |
|---|---|
| **Restore from a stored copy** | The repository's `repos/<id>.db` is gone, corrupted, or rolled back, and you still have the storage. Put the object back; the service is whole again. Seconds, and no client involved. **Reach for this first.** |
| **Replay from a client** | You have no usable copy of the object at all — the bucket is gone, or every copy predates something that matters. Rebuild the service from a client's replica and mutation log, with `ark repair push`. Minutes, and it renumbers. |

**What to back up.** The bucket, or the `DATA_DIR`. That is all the
server-side state there is. `CACHE_DIR` is disposable.

**Repository databases are ordinary SQLite files.** Copy
`repos/<repository-id>.db` out of the bucket or directory and open it
with any SQLite tool. No custom format, no Ark binary required, no
proprietary anything:

```sh
sqlite3 <repository-id>.db '.tables'
sqlite3 <repository-id>.db 'SELECT record_type, count(*) FROM records GROUP BY 1'
```

**Artifacts are ordinary files.** `sha256/<xx>/<hash>` — the object name
is the checksum, so integrity is verifiable with `sha256sum` and nothing
else.

**Every client is a full replica.** This is the property that makes the
service disposable. Each client holds its complete record set in
`.ark/ark.db` plus its own mutation log — the intent behind every local
write, not just the resulting rows. So the service can be rebuilt from
nothing and repopulated by replay from any client.

### Choosing a retention posture

Restoring from a stored copy is only worth as much as the oldest copy you
still have. So the question your storage configuration has to answer is not
*how do I restore* — that part takes seconds — but **how long can you fail
to notice and still recover.**

Nothing picks that number for you, and the default is not an answer. A GCS
bucket created with no retention configuration keeps exactly one copy of
each repository, plus whatever the soft-delete window happens to be: seven
days, which is a Google default rather than a decision anyone made about
Ark.

Seven days is short against the failure this exists for. In the incident
that prompted this section, a repository's history was lost and **nobody
noticed for six weeks** — the bucket-side copy had expired more than a month
before anyone went looking. Client-side detection has since improved: a
checkout that syncs against a service holding a revision below its own now
says so and exits 7. But detection and retention have to overlap, and
"somebody syncs this repository within seven days" is not a property a fleet
has. A repository nobody is currently syncing is precisely the case that
produces the incident.

Three postures, and they are not exclusive:

| | What it buys | What it does not cover |
|---|---|---|
| **Lengthen the soft-delete duration** | One flag. Retains deleted and overwritten generations for longer. | Caps at 90 days, and it leans harder on the one mechanism you already depend on rather than adding a second. |
| **Object versioning, with a lifecycle rule** | Keeps every generation for as long as your rule says, with no cap. | Losing the bucket itself. |
| **Periodic export elsewhere** | The only one that survives losing the bucket, the project, or the account. | More moving parts, and the only one that is not a single configuration change. |

Versioning plus a lifecycle rule is the sensible default, and it matches how
this storage is already used — whole-object writes of a small file. Cost is
not the constraint at these sizes: the reference deployment's entire store
is under 7 MiB across eight repositories.

**What the reference deployment runs**, offered as a worked example rather
than a prescription:

```sh
gcloud storage buckets update gs://<bucket> --versioning
gcloud storage buckets update gs://<bucket> --lifecycle-file=lifecycle.json
```

```json
{
  "rule": [
    {
      "action": { "type": "Delete" },
      "condition": {
        "daysSinceNoncurrentTime": 90,
        "numNewerVersions": 10,
        "isLive": false
      }
    }
  ]
}
```

Conditions within a lifecycle rule are **and**-ed, which is the whole point
of writing two of them: a superseded generation is deleted only once it is
*both* older than 90 days *and* has at least ten newer generations behind
it. A busy repository therefore cannot bury a good copy under a burst of
writes, and a repository nobody has touched in a year still keeps its last
ten. Ninety days is chosen to sit comfortably past the six weeks that
incident went unnoticed.

Soft-delete stays at its default underneath this, where it is now a backstop
against deleting *versions* rather than the only line of defence.

### Restore from a stored copy

The whole procedure is: put the right bytes back at `repos/<id>.db`. The
service picks them up on its next request. There is no import step, no
restart, and nothing to tell the clients.

**Find a copy.** In order of what you are likeliest to have:

1. **Noncurrent versions**, if you turned versioning on — see *Choosing a
   retention posture* above. Every overwrite leaves the previous generation
   behind, and it stays until your lifecycle rule removes it, so this is the
   copy you are likeliest to still have and the first place to look.

   ```sh
   gcloud storage ls --all-versions gs://<bucket>/repos/<repository-id>.db
   ```

2. **The soft-delete window.** GCS buckets retain deleted *and overwritten*
   object generations for the bucket's soft-delete duration — seven days by
   default. This is the fallback when versioning is off, or when the
   generation you want has already aged out of your lifecycle rule.

   ```sh
   gcloud storage ls --soft-deleted gs://<bucket>/repos/<repository-id>.db
   ```

3. **Any out-of-band copy** — a `gcloud storage cp` of the object, a copy of
   the `DATA_DIR`, or a file somebody kept. It is an ordinary SQLite file, so
   any copy of it is a complete backup.

In each case the listing ends every line in `#<generation>`. Generations
increase, so the last one from before the loss is the one you want.

**Check it before you put it back.** It is a SQLite file; read it:

```sh
gcloud storage cp gs://<bucket>/repos/<id>.db#<generation> ./candidate.db
sqlite3 ./candidate.db 'SELECT revision FROM meta WHERE id = 1'
sqlite3 ./candidate.db 'SELECT record_type, count(*) FROM records GROUP BY 1'
```

**Put it back.** From the soft-delete window:

```sh
gcloud storage rm gs://<bucket>/repos/<id>.db          # if a live object is in the way
gcloud storage restore gs://<bucket>/repos/<id>.db#<generation>
```

`gcloud storage restore` will not overwrite a live object synchronously —
`--allow-overwrite` is an `--async`-only flag — so clear the live object
first. Deleting it soft-deletes it too, so the thing you are replacing
stays recoverable for the rest of the window. From an out-of-band copy it
is just a copy:

```sh
gcloud storage cp ./candidate.db gs://<bucket>/repos/<id>.db
```

**Verify without a client.** The point of verifying is to learn about the
object, not about somebody's replica, so ask the service directly:

```sh
curl -sS -X POST "$BASE_URL/v1/sync/pull" -H "Authorization: Bearer $ARK_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"repository_id":"<id>","after_revision":0}' | jq '.server_revision, (.records | length)'
```

**Then leave the clients alone.** A client that had synced before the loss
converges on its next sync with nothing to push and nothing to pull. A
client that has never seen the repository pulls the whole restored set.
Neither needs to be told anything happened.

#### What the restore does to the generation compare-and-swap

Nothing that needs handling. A restored object carries a **new, higher**
generation than the one it replaces, and the service does not depend on
the old one:

- **A running instance does not need restarting.** Every request refetches
  the object and compares generations before using its cached copy, so the
  restore is picked up on the next request. `CACHE_DIR` never has to be
  cleared by hand.
- **A write through an instance whose cache is stale lands on the restored
  file, not on the stale one.** The refetch happens inside the same update
  that does the compare-and-swap, so the write is applied to what the
  storage actually holds. Drilled: a client pushed through an instance
  holding a generation from before the loss, and the result kept every
  restored record and added the new one.
- **A restore does not race with a lost CAS.** A lost race refetches and
  reruns the request, which is what a restore looks like from the inside.

#### The two failure modes look different, and both of them say so

**A deleted object** answers `404 not_found` on pull and on push, and keeps
answering it. A client that has already synced the repository sends its
cursor on registration, so the service refuses to create one in its place
and reports the loss instead (spec §19). That client's `ark sync` exits 7
with a history reset naming service revision **0** — the service holds
nothing at all — rather than the revision 1 its own registration used to
stand up. **The `404` is the clean signal and it now survives contact**, so
you can diagnose a missing repository with clients still running against
it, and the good generation stops being pushed further down the listing by
each sync. What you cannot do is leave it: nothing recovers on its own, and
every client of that repository is stopped until somebody acts. Restoring
is the act to reach for. Where there is nothing left to restore from, a
client can rebuild the repository out of its own mutation log instead —
"Replay procedure" below.

**A corrupted object** — bytes that will not open as a SQLite database —
answers `500`, but with the code `repository_corrupt`, and the client prints
what is actually wrong:

```text
repository 01M13YJT…: its stored database will not open: apply schema:
database disk image is malformed (11) — restore this repository's stored
database from a copy; retrying will not clear it
```

Exit **8**, and it has its own code precisely so that a retry loop cannot
mistake it for weather. The three things a sync can say about the service are
now distinct: **6** could not reach it, **7** reached it and *this checkout*
needs repair, **8** reached it and *its storage* does. The service still logs
the underlying SQLite error, but you no longer have to go and read it — that
trip was the defect (elk-work/ark#65). A 500 that will keep being a 500 until
a person acts is not the same state as a socket that will not open, and the
two must not share an exit code, because 6 is the one retry loops key on.

**A zero-length object is the neighbouring case, and it lands somewhere
else.** SQLite reads it as a perfectly valid empty database, so nothing fails
to open and `repository_corrupt` never fires. What is absent is the repository
row — which registration treats exactly as it treats no database at all, so
this one is a `404`, exit 7, and a history reset. The damaged object stays
where it is for you to restore over; nothing adopts it and nothing empties it
further.

#### Two things that are not restored with it

- **Artifact blobs are separate objects** under `sha256/<xx>/<hash>` and are
  untouched by anything you do to `repos/<id>.db`. They survive a
  repository restore on their own — and a repository restored to a point
  before an artifact was recorded leaves that blob orphaned but harmless.
- **A locally-run server cannot sign artifact URLs under your own
  credentials.** Running `ark-server` against a bucket with `GCS_BUCKET` and
  a person's Application Default Credentials — the obvious thing to do while
  recovering — works for repository databases and fails for every artifact
  transfer, with `sign upload url failed` and, in the logs, `unable to
  detect default GoogleAccessID`. V4 signing needs service-account,
  external-account or impersonated credentials. The deployed service runs as
  a service account and is unaffected; a laptop is not. Do not work around
  it by downloading a service-account key.

#### How long it takes

Seconds. The drill below restored a 100 KiB repository database from the
soft-delete window in **3–4 seconds** and from an out-of-band copy in
**2**, and both times the service served the restored history on the next
request. It is one object copy, so the cost is a round trip rather than
the byte count — a repository with two thousand revisions restores in the
same breath as one with thirty.

### Replay procedure

Rebuilding the service from a client is a command, `ark repair push`. It
used to be four lines of SQL typed into `.ark/ark.db` by hand, which was
undiscoverable, unsafe to recommend, and silently wrong about one thing
the SQL could not express — see "What the command does that the SQL did
not" below.

1. Stand up a fresh `ark-server` (empty bucket or empty `DATA_DIR`).
2. Point the client at it: `ark remote set <new-url>`, then `ark login`.
3. `ark sync`. It exits 7 and records a history reset: the service is
   serving a revision below one this checkout had already synced past,
   which is the finding the repair is gated on. Expect this — it is the
   step that unlocks the next one.
4. `ark repair push`. Nothing changes; it prints what it would replay and
   what that costs, including the display numbers.
5. `ark repair push --confirm`. The client rewinds its cursor, registers,
   pulls whatever the new service already holds, re-queues every mutation
   the old service ruled on — rebased onto the live history — pushes,
   re-offers its artifact blobs, and pulls back. On a clean run it clears
   the recorded reset and exits 0.

Take a copy of `.ark/ark.db` before step 5. It is a plain SQLite file, and
while the repair is not destructive to records it does rewrite sync
bookkeeping that cannot be reconstructed.

Notes:

- **Replay is safe to run more than once.** Mutations carry stable IDs and
  the server records applied outcomes, so a re-run of a partially
  completed replay resumes rather than duplicating.
- **Server-assigned display numbers can differ after a rebuild.** Task and
  PR numbers are display aliases; the ULID is authoritative, and the
  server renumbers on collision. Do not treat `#7` as durable across a
  rebuild. The command names each number it saw move, which is the list of
  references written down elsewhere that are now wrong.
- **If several clients replay into the same fresh service**, the first sets
  the baseline and the rest reconcile against it. Records the first client
  never had still arrive from the others. Order matters in one direction
  only: a client that edited a record somebody else authored has its edit
  refused until the author has replayed, which is reported, leaves the
  reset recorded, and is fixed by running the command again afterwards.
- **This is a rehearsed path, not a theoretical one**: the maintainers'
  deployment was migrated between storage backends by exactly this
  procedure — and that migration is itself worth a note, because a
  migration performed by clients only migrates the repositories whose
  clients sync afterwards. One that nobody synced was how elk-work/ark#58
  happened.

#### What the command does that the SQL did not

The old procedure re-queued the mutation log and reset the cursor, both of
which the command still does. What it could not do is fix the mutations'
`base_server_revision`, and that is the part that loses data quietly.

A base is "the version of the record this change was made against", and the
service reads it against its own per-field revisions when merging. After a
reset those recorded bases count a history nobody is serving any more, so
replaying with them compares two different scales — and it goes wrong in
both directions. A base that lands *below* the record's live revision drops
fields, silently, while still reporting the mutation applied. A base *above*
it — the usual shape, because the dead history was long and the new one is
short — suppresses the merge entirely, so the replay overwrites whatever the
new service does hold. That second one is the sharper risk for an operator:
the four lines of SQL, run against a service that had been rolled back
rather than emptied, would replay straight over the records that survived.

`ark repair push` pulls before it pushes and rebases each mutation onto what
that pull returned, so a base is either a revision of the live history or
zero. Spec §9.3 has the rule and the reasoning.

**Drill it.** A recovery path you have never run is a guess. Rebuilding
into a throwaway `DATA_DIR` instance takes minutes and is the only way
to know your clients still hold what you think they do.

### The drill

[`scripts/restore-drill.sh`](../scripts/restore-drill.sh) runs the whole
of the restore path against a scratch repository and asserts on every
step, so the recovery is something you re-run rather than something you
read. It populates a repository, deletes the object, truncates it,
zeroes it, rolls it back to an earlier revision, restores from each, and
checks what a pre-loss client, a fresh client and the raw API each see.

Its last phase drills the *other* recovery, because that one is not a
special case of this one: it deletes the object with nothing left to
restore from and rebuilds the repository with `ark repair push`, checking
that the preview changes nothing, that the replay comes back whole, that a
client which has never seen the repository can then pull it, and that a
second replica replaying the same log adds no record and mints no
revision.

```sh
scripts/restore-drill.sh --mode local
scripts/restore-drill.sh --mode gcs --bucket <a scratch bucket, never production>
```

`--mode local` rehearses every step against a `DATA_DIR` instance and
needs nothing but a Go toolchain and `git`, `jq`, `curl`, `sqlite3`; it
validates the script and exercises no object storage. `--mode gcs` is the
drill: it also needs `gcloud`, Application Default Credentials, and a
bucket you are willing to have objects deleted and overwritten in. The
script refuses a bucket name that looks like the reference deployment's,
and leaves its objects behind when an assertion fails.

Run it after any change under `internal/server/repodb`, before a change to
how storage is configured, and on whatever schedule makes the answer
current. It exits non-zero and names the assertion when something has
stopped being true.

Last run 2026-08-28 against a scratch GCS bucket: 80 assertions, all
passing, 52 seconds end to end (elk-work/ark#41). The replay phase landed
after that run and has been rehearsed only in `--mode local` (118
assertions, 6 seconds); the next `--mode gcs` run is what will have drilled
it against object storage.

---

## Concurrency and scaling out

Writes to one repository serialize, by design. Within a process, a
per-repository mutex holds the lock. Across processes, `Store` does a
compare-and-swap against the storage generation: fetch at generation N,
apply, write back only if the object is still at N. A lost race returns
"repository changed concurrently", and the manager invalidates its
cache, refetches, and reruns the whole update closure — **up to three
attempts**, then the request fails with `409` and the client retries.

This is correct but not free. Every conflicting write costs a refetch
and a full replay of the request, and the whole database file is
rewritten on each successful write.

**The reference deployment therefore runs a single instance
(`max-instances 1`).** With one instance, the in-process mutex handles
all contention, and the CAS path is a safety net for the rare
overlapping-restart case rather than a routine cost.

If you scale out anyway:

- Correctness holds. The CAS is the authority, and idempotent mutation
  replay is what makes a lost race safe.
- Contention becomes real. Concurrent writers to the *same* repository
  will burn retries; three lost races in a row surfaces as a `409` and
  the client has to retry.
- Different repositories never contend — the lock granularity is one
  repository. Many repositories with light per-repository write rates
  scale out fine; one repository with many concurrent agents does not.
- Each instance keeps its own `CACHE_DIR`, so cache misses multiply and
  every instance refetches independently.

Sustained multi-writer contention on a single repository is one of the
documented triggers for revisiting the storage design (RFC-0001,
"exit triggers"), not something to tune around.

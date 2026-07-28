# Self-hosting the Ark sync service

`cmd/ark-server` is the shared authority behind `ark sync`. It is a single
Go binary with no always-on database and no external dependencies beyond
the storage you point it at. This document is the general guide: run your
own instance, on your own machine or your own cloud account.

For the maintainers' concrete environment, see
[ops/elkproject-deployment.md](ops/elkproject-deployment.md) — you do not
need it, and nothing here depends on it.

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
| `GCS_BUCKET` | object-storage mode | — | Google Cloud Storage bucket for repository databases and blobs. Set → object-storage mode; unset → local mode. |
| `BASE_URL` | local mode | — | Externally reachable base URL of this service. Required when `GCS_BUCKET` is unset; used to build blob URLs. Startup fails without it. |
| `DATA_DIR` | no | `data` | Local mode only. Repository databases go in `<DATA_DIR>/repos`, blobs in `<DATA_DIR>/blobs`. Relative paths resolve against the working directory. |
| `CACHE_DIR` | no | `<os temp dir>/ark-repos` | Scratch space for working copies of repository databases. Created at startup. Safe to lose — it is a cache, refetched on demand. |
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
In local mode there is no signer, so the service serves the blobs itself
on two extra routes:

```text
PUT /blobs/<key>     upload artifact content
GET /blobs/<key>     download artifact content
```

`POST /v1/artifacts/upload-url` returns `<BASE_URL>/blobs/<key>` instead
of a signed URL, and the client `PUT`s to it. This is why `BASE_URL` is
required in this mode and why it must be the URL clients actually reach
— if it is wrong, record sync still works and artifact upload silently
targets an unreachable host.

Two things to understand before exposing this mode:

- **The `/blobs/` routes are not behind the bearer token.** Every `/v1`
  route is; the blob routes are not, because they stand in for signed
  URLs, which are unauthenticated by construction. Unlike a signed URL,
  they do not expire. Anyone who can guess or observe a content hash can
  read that artifact. Key names are SHA-256 digests, so this is not
  trivially enumerable, but it is not access control either.
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

**Server side.** `ARK_API_TOKEN` is read once at startup; empty or
missing is a startup failure. Every `/v1` route strips a leading
`Bearer ` from the `Authorization` header and compares the remainder
against the token in constant time. A mismatch is `401` with a
`permission` error code. Unauthenticated routes: `GET /` (returns a
service banner), `GET /health`, and — in local mode only — `/blobs/`.

**Client side.** The token resolves in this order:

1. `ARK_TOKEN` in the environment
2. the macOS keychain, service `ark`, account = the remote's host
3. `~/.ark/credentials.toml`, mode 0600, keyed by remote host

Tokens are never written into the repository — not into `.ark/config.toml`,
not anywhere under `.ark/`. `ark login` writes to the keychain on macOS
and falls back to the credentials file elsewhere.

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

- **Generate the token with a real random source** — it is the only
  credential in the system. 32 bytes of `/dev/urandom`, base64, is fine.

- **Rotation is a restart.** Write the new value, restart the service,
  then `ark login` again on each client. There is no overlap window: the
  old token stops working the moment the new process is serving.

- **Do not put the token in a URL, a container image, or a command line**
  that lands in shell history. `ark login` reads it from stdin for this
  reason.

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
stores the token outside the repository (keychain, else
`~/.ark/credentials.toml`). Piping is preferred over `--token` to keep
the value out of shell history:

```sh
<your secret store: read the token> | ark login
```

For CI and agents, skip `ark login` and set `ARK_TOKEN` in the
environment; it wins over both other sources.

`ark sync` registers the repository (idempotent, and it doubles as a
token check), pushes pending mutations, uploads artifact blobs the
server does not have, then pulls records after the local cursor.

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

### Replay procedure

Rebuilding the service from a client:

1. Stand up a fresh `ark-server` (empty bucket or empty `DATA_DIR`).
2. Point the client at it: `ark remote set <new-url>`, then `ark login`.
3. On the client, reset the mutation queue and the pull cursor in
   `.ark/ark.db`:

   ```sql
   UPDATE mutations SET status = 'pending' WHERE status = 'applied';
   UPDATE sync_state SET last_revision = 0;
   ```

4. `ark sync`. The client re-registers the repository, replays its whole
   mutation log, re-uploads any artifact blobs the new service does not
   have, and pulls back from revision 0.

Take a copy of `.ark/ark.db` before step 3 — it is a plain SQLite file
too, and the reset is destructive to sync bookkeeping (not to records).

Notes:

- Replay is safe to run more than once. Mutations carry stable IDs and
  the server records applied outcomes, so a re-run of a partially
  completed replay resumes rather than duplicating.
- Server-assigned display numbers can differ after a rebuild. Task and
  PR numbers are display aliases; the ULID is authoritative, and the
  server renumbers on collision. Do not treat `#7` as durable across a
  rebuild.
- If several clients replay into the same fresh service, the first sets
  the baseline and the rest reconcile against it. Records the first
  client never had still arrive from the others.
- This is a rehearsed path, not a theoretical one: the maintainers'
  deployment was migrated between storage backends by exactly this
  procedure.

**Drill it.** A recovery path you have never run is a guess. Rebuilding
into a throwaway `DATA_DIR` instance takes minutes and is the only way
to know your clients still hold what you think they do.

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

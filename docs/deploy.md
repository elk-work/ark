# Deploying the Ark sync service

`cmd/ark-server` is the shared authority behind `ark sync`: one SQLite
database per repository (`repos/<repository-id>.db`) plus content-addressed
artifact blobs, no always-on database, one bearer token for all clients in
V1 (spec §20, RFC-0001 superseding spec §19).

Two ways to run it, chosen by whether `GCS_BUCKET` is set:

- **Local (`DATA_DIR`)** — a directory on one disk holds repository
  databases and blobs, and the service serves blob content itself on
  `GET/PUT /blobs/`. Zero dependencies. For development, tests, and
  single-machine or small trusted-network use.
- **Object storage (`GCS_BUCKET`)** — repository databases and blobs are
  objects in a Google Cloud Storage bucket, with the object generation as
  the compare-and-swap and V4 signed URLs for artifact transfer. GCS is
  the only object backend implemented today.

**→ [self-hosting.md](self-hosting.md)** is the guide: exact environment
variables, both modes end to end, auth and its trailing-newline trap,
health checks, client setup, restore-from-a-copy and replay recovery,
and why the reference deployment runs a single instance.

The maintainers' own environment (GCP project, bucket, Cloud Run service,
service account, secret name, deploy command) is documented privately in
the `elk-work/elk` meta-repo (`docs/ark/elkproject-deployment.md`).
Useful only with access to that project; nothing in Ark depends on it.

## Breakdown board

`ARK_UI` defaults to `on` (unset is equivalent). `off` returns 404 for the
entire `/ui/` surface, including sign-in, logout and assets, and for
`GET /v1/repositories/{repo}/tasks` and `GET /v1/repositories/{repo}/tasks/{id}`.
Any other value fails startup. Existing task writes, sync and device login
are unaffected. Change this environment setting on the running service to
dark the board; no different binary is needed.

Open `https://<service>/ui/` and present an existing `arkc_` principal credential.
Legacy service and bootstrap tokens cannot sign in. The switcher and task
reads require explicit read/write/admin grants, including on services with
`ARK_DEFAULT_GRANT=read`; an operator has no board bypass. Grant changes and
credential revocation take effect within the existing 60-second auth cache
window across instances (immediately after writes on the same instance).

Sessions expire after 12 hours. Only the session hash and credential id live
in `ui_sessions` in `auth.db`; cookies are HttpOnly, Secure, SameSite=Strict
and never authorize the pre-existing write APIs. No credential is kept in
localStorage. The additive SQL under `migrations/auth/` is embedded and applied
automatically to auth.db on open; it is not a client/repository migration.
Expired rows are pruned at sign-in. Disabling the board does not revoke
credentials or delete sessions; a re-enabled board honors unexpired sessions.

HTTPS is required for browser sign-in, also in local mode. Cloud Run terminates
TLS; a self-hosted reverse proxy must preserve the public Host and overwrite
`X-Forwarded-Proto` with `https` on TLS requests, and prevent direct public
access to the backend. Sign-in/logout reject missing or foreign Origin headers.
For local UI development use a local HTTPS reverse proxy or a TLS test server;
there is deliberately no insecure-cookie switch. The UI is embedded Go HTML,
CSS and vanilla JavaScript: `make build-server` includes it without a Node or
npm step. The board shows the service's last synced records, not unsynced
changes on somebody's machine.

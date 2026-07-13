# Deploying the Ark sync service

Production stack (spec §19): Cloud Run + Cloud SQL for PostgreSQL + Google
Cloud Storage. One service token authenticates all clients in V1 (§20).

## Current production environment (elkproject.com)

Provisioned 2026-07-12 in project `steadfast-sound-502323-q0`, region
`us-west1`:

| Piece | Name |
|---|---|
| Cloud SQL (Postgres 17, enterprise, db-f1-micro, 10GB SSD) | `ark-sql` |
| Database / user | `ark` / `ark` |
| GCS bucket (uniform access, no public access) | `elkproject-ark-artifacts` |
| Cloud Run service | `ark` — https://ark-709757975936.us-west1.run.app |
| Runtime service account | `elk-issac-operator@steadfast-sound-502323-q0.iam.gserviceaccount.com` |
| Secrets | `ark-api-token`, `ark-db-password`, `ark-database-url` |

The service account holds `roles/cloudsql.client` (project),
`roles/storage.objectAdmin` (bucket), `roles/secretmanager.secretAccessor`
(the secrets), and `roles/iam.serviceAccountTokenCreator` on itself (V4
signed URLs sign via the IAM credentials API on Cloud Run).

## Gotchas learned the hard way

- **`/healthz` is unreachable on run.app URLs.** Google Frontend intercepts
  that path and serves its own 404 before the request reaches the container
  (no request logs appear). The health endpoint is `/health`.
- **Domain Restricted Sharing.** The elkproject.com org blocks `allUsers`
  IAM members by default; this project carries an org-policy override
  (`iam.allowedPolicyMemberDomains: allowAll`) so the service can be public.
  The service enforces its own bearer token.
- **Cloud Build default SA.** New projects need
  `roles/cloudbuild.builds.builder` granted to
  `<project-number>-compute@developer.gserviceaccount.com` before
  `gcloud run deploy --source` works.
- **Secrets end with what you put in them.** A trailing newline in a token
  secret breaks bearer comparison; create secrets with `printf`, not `echo`.

## Deploying a new revision

From the repository root (the root `Dockerfile` builds `cmd/ark-server`):

```sh
gcloud run deploy ark \
  --source . \
  --region us-west1 \
  --project steadfast-sound-502323-q0 \
  --service-account elk-issac-operator@steadfast-sound-502323-q0.iam.gserviceaccount.com \
  --add-cloudsql-instances steadfast-sound-502323-q0:us-west1:ark-sql \
  --set-env-vars GCS_BUCKET=elkproject-ark-artifacts \
  --set-secrets DATABASE_URL=ark-database-url:latest,ARK_API_TOKEN=ark-api-token:latest \
  --allow-unauthenticated \
  --min-instances 0 --max-instances 2 --memory 256Mi
```

`--allow-unauthenticated` is correct: the service enforces its own bearer
token on every /v1 route; only /healthz is meaningfully public.

The `ark-database-url` secret holds the complete DSN:

```text
postgres://ark:<password>@/ark?host=/cloudsql/steadfast-sound-502323-q0:us-west1:ark-sql
```

(pgx treats a directory `host` as a Unix socket; Cloud Run mounts the
instance socket under /cloudsql.)

## Client setup

```sh
ark remote set https://<cloud-run-url>
ark login    # paste the value of the ark-api-token secret
ark sync
```

Read the token when needed:

```sh
gcloud secrets versions access latest --secret=ark-api-token
```

## Recovery path (principle: keep a boring recovery path)

Nothing here depends on Ark itself. The database is plain Postgres —
`gcloud sql connect ark-sql --user=ark` reaches it; the schema is
`internal/server/schema/schema.sql`. Artifacts are plain content-addressed
objects (`sha256/<xx>/<hash>`) in the bucket. Every client keeps a full
local copy of its records in `.ark/ark.db` (SQLite), so the service can be
rebuilt from scratch and repopulated by client pushes: recreate the infra
with the commands above, point clients at the new URL, and `ark sync`.

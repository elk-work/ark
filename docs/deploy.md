# Deploying the Ark sync service

Production stack (RFC-0001, superseding spec §19): Cloud Run + Google Cloud
Storage. Each repository's metadata is one SQLite object
(`repos/<repository-id>.db`); artifact blobs are content-addressed objects
in the same bucket. No always-on database. One service token authenticates
all clients in V1 (§20).

## Current production environment (elkproject.com)

Provisioned 2026-07-12 in project `steadfast-sound-502323-q0`, region
`us-west1`:

| Piece | Name |
|---|---|
| GCS bucket (repo databases + artifact blobs) | `elkproject-ark-artifacts` |
| Cloud Run service | `ark` — https://ark-709757975936.us-west1.run.app |
| Runtime service account | `elk-issac-operator@steadfast-sound-502323-q0.iam.gserviceaccount.com` |
| Secret | `ark-api-token` |

The service account holds `roles/storage.objectAdmin` (bucket),
`roles/secretmanager.secretAccessor` (the secret), and
`roles/iam.serviceAccountTokenCreator` on itself (V4 signed URLs sign via
the IAM credentials API on Cloud Run). The Cloud SQL instance from the
first deployment was decommissioned 2026-07-13 after migrating by mutation
replay (see RFC-0001).

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
  --set-env-vars GCS_BUCKET=elkproject-ark-artifacts \
  --set-secrets ARK_API_TOKEN=ark-api-token:latest \
  --allow-unauthenticated \
  --min-instances 0 --max-instances 1 --memory 256Mi
```

`--allow-unauthenticated` is correct: the service enforces its own bearer
token on every /v1 route; only /healthz is meaningfully public.

`--max-instances 1` keeps the optimistic-concurrency retry path
(GCS generation CAS) a rarity rather than a routine.

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

Nothing here depends on Ark itself. Each repository's shared history is a
plain SQLite file — `gcloud storage cp gs://elkproject-ark-artifacts/repos/<id>.db .`
and open it with any SQLite tool. Artifacts are plain content-addressed
objects (`sha256/<xx>/<hash>`) in the same bucket. Every client keeps a
full local copy of its records in `.ark/ark.db` plus its mutation log, so
the service can be rebuilt from nothing and repopulated by replay — proven
during the 2026-07-13 migration: reset the client cursor
(`UPDATE mutations SET status='pending' WHERE status='applied'; UPDATE
sync_state SET last_revision=0;`) and `ark sync`.

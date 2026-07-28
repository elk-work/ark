# Ark sync service — elkproject deployment (internal)

This file documents one specific private environment (the maintainers'
deployment on elkproject.com infrastructure) and carries **no secret
values** — resource identifiers only. It is of no use to anyone without
access to that GCP project; for running your own instance see
[../self-hosting.md](../self-hosting.md).

## Environment

Provisioned 2026-07-12 in project `steadfast-sound-502323-q0`, region
`us-west1`:

| Piece | Name |
|---|---|
| GCS bucket (repo databases + artifact blobs) | `elkproject-ark-artifacts` |
| Cloud Run service | `ark` — https://ark-709757975936.us-west1.run.app |
| Runtime service account | `elk-issac-operator@steadfast-sound-502323-q0.iam.gserviceaccount.com` |
| Secret (Secret Manager) | `ark-api-token` |

The service account holds `roles/storage.objectAdmin` (bucket),
`roles/secretmanager.secretAccessor` (the secret), and
`roles/iam.serviceAccountTokenCreator` on itself (V4 signed URLs sign via
the IAM credentials API on Cloud Run — see the signed-URL section of
self-hosting.md for why the self-grant is required).

The Cloud SQL instance from the first deployment was decommissioned
2026-07-13 after migrating by mutation replay (see
[../rfc-0001-per-repo-sqlite-storage.md](../rfc-0001-per-repo-sqlite-storage.md)).
That migration is the proof-of-life for the generic replay procedure in
self-hosting.md.

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

**A source deploy does not stamp the version.** `gcloud run deploy --source`
passes no Docker build arg, and it excludes `.git` from the upload, so both
paths `internal/buildinfo` could use are unavailable and `GET /` reports
`"version":"dev"`. The Cloud Run revision name (`ark-00005-dxr`) still
identifies the running build, which is usually what you actually want. To
stamp it properly, build and push the image yourself instead:

```sh
docker build --platform linux/amd64 --build-arg VERSION="$(git describe --tags --always --dirty)" -t us-west1-docker.pkg.dev/steadfast-sound-502323-q0/cloud-run-source-deploy/ark:$(git describe --tags --always) .
docker push us-west1-docker.pkg.dev/steadfast-sound-502323-q0/cloud-run-source-deploy/ark:$(git describe --tags --always)
gcloud run deploy ark --image us-west1-docker.pkg.dev/steadfast-sound-502323-q0/cloud-run-source-deploy/ark:$(git describe --tags --always) --region us-west1 --project steadfast-sound-502323-q0
```

`--allow-unauthenticated` is correct: the service enforces its own bearer
token on every `/v1` route; only `/` and `/health` are meaningfully
public.

`--max-instances 1` keeps the optimistic-concurrency retry path (GCS
generation CAS) a rarity rather than a routine.

`--source` uploads a build context without `.git`, so the image carries no
version stamp and `GET /` reports `dev`. To identify a deployed revision,
build and push the image with the `VERSION` build arg
(`docker build --build-arg VERSION="$(git describe --tags --always --dirty)"`)
and deploy `--image` instead of `--source`.

## Project-specific gotchas

- **Domain Restricted Sharing.** The elkproject.com org blocks `allUsers`
  IAM members by default; this project carries an org-policy override
  (`iam.allowedPolicyMemberDomains: allowAll`) so the service can be
  public. The service enforces its own bearer token.
- **Cloud Build default SA.** New projects need
  `roles/cloudbuild.builds.builder` granted to
  `<project-number>-compute@developer.gserviceaccount.com` before
  `gcloud run deploy --source` works.
- **`/healthz` is unreachable on run.app URLs.** Google Frontend
  intercepts that path and serves its own 404 before the request reaches
  the container (no request logs appear); verified empirically
  2026-07-13. The health endpoint is `/health`. Kept here because it is
  where this was learned; the portable form of the lesson lives in
  self-hosting.md.
- **Secrets end with what you put in them.** A trailing newline in the
  token secret breaks bearer comparison; create secret versions with
  `printf`, not `echo`. (Portable — repeated in self-hosting.md because
  everyone hits it.)

## Client setup against this deployment

```sh
ark remote set https://ark-709757975936.us-west1.run.app
ark login    # paste or pipe the value of the ark-api-token secret
ark sync
```

Collaborators fetch the token themselves rather than receiving it from
another person (see [../adoption.md](../adoption.md) § Credentials):

```sh
gcloud secrets versions access latest --secret=ark-api-token --project steadfast-sound-502323-q0 | ark login
```

## Recovery

Repository databases:

```sh
gcloud storage cp gs://elkproject-ark-artifacts/repos/<repository-id>.db .
```

Artifacts are content-addressed objects under `sha256/<xx>/<hash>` in the
same bucket. The full generic recovery and replay procedure — including
the client-side cursor reset — is in
[../self-hosting.md](../self-hosting.md) § Backup and recovery. It was
executed for real during the 2026-07-13 Cloud SQL → GCS migration.

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
health checks, client setup, backup/replay recovery, and why the
reference deployment runs a single instance.

The maintainers' own environment (GCP project, bucket, Cloud Run service,
service account, secret name, deploy command) is documented privately in
the `elk-work/elk` meta-repo (`docs/ark/elkproject-deployment.md`).
Useful only with access to that project; nothing in Ark depends on it.

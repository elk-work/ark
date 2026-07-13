# RFC-0001 — Per-Repository SQLite Storage for the Sync Service

Status: adopted 2026-07-13 (supersedes the Cloud SQL choice in v1-spec §19)

## Problem

The V1 spec called for Cloud SQL for PostgreSQL. That worked, but it put an
always-on database instance (~$11/month at the smallest tier) under a system
whose load is a handful of small writes per day, and it made the shared
multi-tenant database the tenancy model — one Postgres holding every
repository's records, isolated only by a `repository_id` column. GCP has no
scale-to-zero SQL, so the cost floor and the tenancy shape were both wrong
for Ark.

## Decision

One SQLite database per repository, persisted as a single object in GCS
(`repos/<repository-id>.db`), fronted by the same Cloud Run service and the
same HTTP API. Nothing changes for clients.

- The Cloud Run instance fetches a repository's database on demand, caches
  it, applies the request in one transaction on a working copy, and writes
  the file back using the object's **generation as a compare-and-swap**.
- A lost race (another instance wrote first) refetches and replays the
  request. This is safe because the sync protocol is idempotent by design:
  `applied_mutations` returns stored outcomes for replayed mutation IDs.
- Within a process, a per-repository mutex serializes writers.
- In development and tests the backend is a plain local directory; the
  entire test suite runs with no external services.

## Why this fits Ark

- **The file is the tenant.** Copying, snapshotting, exporting, or deleting
  a repository's shared history is one object operation. This is the
  per-project isolation model the multi-tenant Postgres lacked.
- **Local-first symmetry.** SQLite on the client, SQLite in the cloud. The
  recovery path (principles: *keep a boring recovery path*) degrades to
  "download a file from a bucket and open it" — and because every client
  holds a full replica plus its mutation log, the service itself can be
  rebuilt by client replay.
- **Cost.** Cloud Run scales to zero; GCS storage for metadata is pennies.
  The Cloud SQL instance is decommissioned.

## Costs accepted

- Whole-file rewrite per push. Repository metadata is small (KBs–MBs);
  fine at Ark's scale.
- Write throughput serializes per repository. The protocol already
  serialized pushes per repository (the Postgres version locked the
  repository row), so nothing is lost.
- Cross-repository queries on the server become impossible by construction.
  None exist today; if they become a product need, that is one of the
  triggers below.

## Exit triggers (when to revisit, e.g. toward Neon)

1. Many agents pushing concurrently to a single repository, making CAS
   retries a real contention cost.
2. A server-side cross-repository query surface (org-wide search or
   dashboards).
3. Repositories large enough that whole-file rewrites dominate push latency.

The storage sits behind `repodb.Backend` (GCS or local directory), so a
future move to hosted Postgres/Neon is contained to the server.

## Non-goals

Scout Signal (Elk-scout's materialization data plane, Neon-backed) stays a
separate system. It is an analytical read-model — cross-source SQL
transforms and views — while Ark is an operational system of record. They
should meet at the data seam (an Ark source connector feeding Scout via the
pull-by-revision API), not the storage seam.

# Adopting Ark in an existing project

The playbook for bringing a project onto Elk's work-record system, proven
on Elk-scout (its issue #23 / PR #26). The premise: never a cutover cliff.
Ark runs in parallel with the incumbent (GitHub) until it has earned trust,
and the adoption itself is staged as a PR the project's owner lands — which
makes their machine (and their agent) the second participant automatically.

Start by inspecting the project's GitHub repo: open issues, collaborators,
docs conventions, in-flight branches. Then:

## Phase A — attach (no behavior change)

```sh
cd <project>
ark init
ark remote set <sync-service-url>
ark login
ark sync
```

`.ark/` self-ignores; nothing lands in git. Record the printed repository
ID in the tracking issue — additional machines join with
`ark init --repository <id>`.

## Phase B — mirror the open backlog

One-way, idempotent import of open GitHub issues into Ark tasks, each body
carrying a `Mirrored from GH#<n>` marker so re-runs skip existing mirrors
(reference implementation: Elk-scout's `scripts/ark-mirror-issues.sh`).
During the trial, GitHub issues stay canonical for *what* to do; Ark
records *how it went*.

## Phase C — agents record their work

This is the real test. Agents wrap substantive sessions in
`ark run start/finish`, record design discussions as threads, and attach
evidence (benchmarks, screenshots, reports) as artifacts. Commit a
`docs/ark-work-records.md` guide to the project with its repository ID,
join steps, and these conventions — a docs file, not CLAUDE.md, because
collaborators often keep CLAUDE.md local and untracked. Record the
migration itself as the project's first thread and run: the history should
begin with its own origin story.

## Phase D — evaluate (~2 weeks), then decide

- **Durability:** did Ark capture context the incumbent loses (agent
  reasoning, run outcomes, artifacts)?
- **Friction:** did dual-entry cost more than the history is worth?
  (Automate the mirror or cut scope before giving up.)
- **Trust:** no data loss across syncs and conflicts; a second machine
  joined cleanly; one recovery drill passed.

Decide: expand (Ark canonical for tasks/runs), hold steady, or retreat.

## Staging as a PR

The adoption lands as a small PR: the work-records doc, the mirror script,
and an index entry — plus an explicit "after you merge" section the owner's
agent can execute (join commands, credential steps). Phases A–C run from
the adopter's machine before the PR merges; the owner's post-merge join
doubles as Phase D's second-machine check.

## Credentials

Tokens are never passed person-to-person.

- **Bootstrap (while Elk is being built):** each collaborator has their own
  elkproject.com GCP account and fetches the service token themselves:
  `gcloud secrets versions access latest --secret=ark-api-token
  --project <project> | ark login`.
- **End state:** the user's **Elk login** issues Ark credentials from the
  CLI — Elk is the identity provider for Ark, and no GCP account is
  involved. Tracked as a task in this repository's Ark backlog.

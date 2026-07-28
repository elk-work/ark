# Phase D verdict brief — the Ark adoption trial

Prepared 2026-07-28 for ijroth. The decision is yours; this is the evidence
and a recommendation. Criteria are adoption.md Phase D: **durability**,
**friction**, **trust**.

## Recommendation: HOLD — with three conditions and a real re-decision date

Not *expand*: not one of the three trust criteria has been met, so nothing has
earned the right to become canonical. Not *retreat*: the thing Ark was supposed
to prove — that it captures context the incumbent loses — **is the one thing the
evidence actually shows**. Every failure is in a different category, and each has
a known, cheap fix.

## First, a caveat that shapes everything below

**The trial as designed did not run.** adoption.md Phase D specifies "~2 weeks"
of real use. The designated subject was Elk-scout (its GH#23), which has the
mirror script, the coverage script, the run hook, the work-records doc, and the
only configured sync remote. Scout's last commit is 2026-07-23 and its `.ark`
replica is not on this machine.

What we actually have is two substitute repositories:

| | signal | pulse |
|---|---|---|
| Ark initialized | 2026-07-25 | 2026-07-27 |
| Window of use | **1.8 days** | **~1 day, still active** |
| Adoption artifacts (work-records doc, mirror, hook) | none | none |

So this is a verdict on ~3 repository-days without the tooling the program
itself concluded was necessary. That is thin for a strong move in either
direction, and is itself an argument for Hold.

## Durability — the content proved out; the storage did not

**What Ark captured that Git and GitHub structurally cannot.** Signal has 15
comments that are a genuinely better narrative than its 36 commit subjects:

> "Extraction landed and merged to planning/foundations: engine lives here as
> cmd/signald + internal/ (31 files, ~5.2k LOC), 41/41 tests pass with live
> Postgres. Contract delta (8 points) recorded in docs/0002 as the convergence
> work list."

> "COMPLETE: cutover gate met (two green scheduled runs, 04:21Z and 07:08Z),
> scout#101 merged at 07:11Z — scout no longer contains or schedules the data
> plane."

Pulse's two agent runs carry input, result, branch and commit in one linked
record. Signal has **zero GitHub issues and zero PRs** — that history had
nowhere else to live. This is the value proposition, and it is real.

**Where it lives is the problem.**

| | signal | pulse |
|---|---|---|
| Mutations pending / total | **27 / 27** | **19 / 19** |
| `sync_state.last_revision` | 0 | 0 |
| Remote configured | **none** | **none** |
| Artifacts with `storage_key` | n/a (0 artifacts) | **0 of 2** |

**No repository has ever synced. Not once.** Every record is in a gitignored
SQLite file on one laptop, with no backup and no second reader. The recovery
path documented in deploy.md has never been exercised outside the 2026-07-13
migration.

**Latent hazard, not yet a loss:** pulse's `.ark` exists in three places (the
submodule plus two session worktrees) all sharing repository ID
`01KYGRBJZ9G52KMRSZRX0PDGHE`, and one is already divergent. It is currently a
strict subset, so nothing is stranded — but a worktree-per-session workflow
physically copies the database, and sync is the only thing that could reconcile
them. Two concurrent sessions would fork the history permanently.

## Friction — untested, and the record is silent

Dual entry never materialized: signal has no GitHub issues to duplicate
against. So the cost adoption.md worried about was never actually paid, and
"friction" is the one criterion with no evidence either way.

The more telling number is **coverage**, which is where the real friction shows:

| Repository | Commits in window | Agent runs recorded |
|---|---|---|
| scout (per adoption.md's own measurement) | 34 merged PRs | **~2** |
| signal | 36 | **0** |
| pulse | (active) | 2 (manual) |

Signal used 6 of Ark's 11 record types and never invoked `ark run start/finish`
once. Nine of eleven tasks are still open; ten were never touched after
creation; five have empty bodies. 73% of all records landed on day one, 29 of
them in a single hour — the seeding burst — and writing stopped on two "HANDOFF
for a fresh session" comments.

**The cause is identified and is not a mystery.** adoption.md already recorded
this failure once and built the fix:

> "The 'substantive sessions get a run' convention above used to depend on an
> agent remembering to type `ark run start/finish`. It didn't happen — coverage
> sat at ~2 runs against 34 merged PRs."

The fix was `scripts/ark-run-hook.sh`. It lives only in scout. Neither signal
nor pulse has a `.claude/settings.json` installing it. **The convention was
carried to both repositories; the automation was not.** Signal then reproduced
the exact failure the automation existed to prevent.

Applying the trial's own instrument (`ark-coverage.sh`), signal scores **DARK** —
"GitHub saw real work this window; Ark recorded no runs."

## Trust — all three sub-criteria unmet

| adoption.md criterion | Status |
|---|---|
| No data loss across syncs and conflicts | **Unproven.** Zero syncs; conflicts table empty because nothing met a server. |
| A second machine joined cleanly | **Never attempted.** |
| One recovery drill passed | **No evidence** since the 2026-07-13 migration, which predates this trial. |

## kraman — invited, not yet onboarded

Correcting the session's framing: the prerequisites landed (Apache-2.0, CLA
workflow, CI, module rename to `github.com/elkproject/ark`, PR #2, merged
2026-07-25), and the invitation to **krishnaramannet** (Krishna Raman) was sent
**2026-07-27 23:31Z with write permission**. It is still **pending — not
accepted**. No commits, no PRs, no CLA signature, no org membership.

So there is **no onboarding experience to evaluate yet**, and the collaboration
hardening done this session (v0.1.0 plumbing, 36 sync-path tests, the generic
self-hosting doc) lands *before* the first outside contributor arrives rather
than after. That is the right order, and it is worth noting that this is now
true — it was not true yesterday.

## What changed this session, and why it matters to the verdict

Until today, recording work in Ark had **no payoff surface**. Records went into
a local file and stopped there; the reason to type `ark run finish` was
abstract. RFC-0002 and the shipped adapter change that: Ark activity now
normalizes into Elk work-record events and lands in a workspace stream — proven
end to end on signal's real history this session.

**Judging Ark's adoption on pre-adapter behaviour understates it.** The trial
measured a tool whose output nobody could see.

## The three conditions

1. **Make sync non-optional.** `ark init` should set the remote and sync
   should happen without being asked. Until a repository syncs, durability,
   the second-machine check, and the recovery drill are all *unmeasurable* —
   this single fix unblocks the entire trust criterion.
2. **Ship the run hook with `ark init`, not as a per-repo manual step.** It is
   the one intervention with evidence behind it. The repos that got it recorded
   runs; the repos that didn't recorded zero.
3. **Resolve the worktree replica hazard** before it causes the data loss that
   criterion 1 is supposed to prove doesn't happen.

## Re-decide

Two weeks of *instrumented* use — sync on, hook installed, `ark-coverage.sh`
run weekly — on signal and pulse, which are the two repositories where work is
actually happening. That is the trial adoption.md specified and has not yet
had.

If coverage is still DARK with the automation installed and sync working, that
is a real retreat signal and it will be unambiguous. Right now it is not.

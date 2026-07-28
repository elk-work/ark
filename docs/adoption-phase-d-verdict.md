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

**Where it lives is the problem — and the shape of the problem is a regression,
not an absence.**

| | signal | pulse |
|---|---|---|
| Mutations pending / total | **27 / 27** | **19 / 19** |
| `sync_state.last_revision` | 0 | 0 |
| Remote configured | **none** | **none** |
| Artifacts with `storage_key` | n/a (0 artifacts) | **0 of 2** |

Neither of the two repositories under trial has ever synced. But the sync
service holds four repository databases, and three of them are real history
from an earlier wave:

| Repository on the service | Records | Last write |
|---|---|---|
| `ark` (`ijroth/ark`, pre-transfer) | 5 tasks, 6 comments, 1 actor — rev 18 | 2026-07-13 |
| `Elk-scout` | 5 tasks, 3 actors, 2 agent runs, 1 thread + 1 message — rev 13 | 2026-07-15 |
| `clawfight` | 1 gap, 1 promotion, 2 actors — rev 4 | 2026-07-14 |

**So sync is not unproven — it worked, on three repositories, 2026-07-13 to
07-15, and the data is intact and readable today.** What happened is that the
practice *stopped*: every repository onboarded since (signal 07-25, pulse
07-27) was `ark init`-ed without a remote and has stayed disconnected. `ark
init` does not set one, so a new repository starts offline and silently
remains offline.

That reframes the durability finding. The mechanism works; the default is
wrong. It also makes condition 1 below much cheaper than it looked — this is a
default to change, not a subsystem to prove.

Three qualifications keep this from being good news:

- **Elk-scout's two agent runs are both still `status = "running"`.** They were
  started and never finished, so even the runs that exist do not carry a
  result. Combined with adoption.md's own "~2 runs against 34 merged PRs", the
  run-recording story is worse than the raw count suggests.
- **The artifact bucket holds no blobs at all.** Across every repository, no
  artifact has ever reached object storage, so that half of the sync path is
  genuinely unexercised.
- **A recovery drill still has no evidence** since the 2026-07-13 migration,
  which predates the trial.

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
| No data loss across syncs and conflicts | **Partly met, outside the trial window.** Three repositories synced cleanly 07-13→07-15 and their data is intact. Neither trial repository has synced, so nothing was tested *during* the trial. |
| A second machine joined cleanly | **Never attempted.** Elk-scout's three actors (one human, two distinct agents delegating to it) show multi-actor use, but that is one machine. |
| One recovery drill passed | **No evidence** since the 2026-07-13 migration, which predates this trial. |

**Open defect found while checking the service.** The `clawfight` repository
holds a record of type `gap` — an entire record type that is *commented out* in
the client (`internal/records/records.go:40`, "reserved for a future record
type"). The server stores records as opaque JSON and will hand it to any client
that pulls that repository. This wants a deliberate answer before an outside
contributor meets it: either the client tolerates unknown record types by
design and that is tested, or this is latent breakage.

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

1. **Make sync non-optional.** `ark init` should take (or inherit) a remote and
   sync without being asked. This is the highest-value fix and the cheapest:
   the machinery already works — three repositories proved it in July — and
   what broke is simply that the default is "offline" and nobody opted in
   afterwards. Until the trial repositories sync, durability, the
   second-machine check and the recovery drill stay unmeasurable *for them*.
2. **Ship the guidance as a skill, installed by `ark init`.** The scout-era fix
   was a shell hook (`ark-run-hook.sh`), and it worked — but a hook is a
   workaround for a human forgetting. These repositories are built almost
   entirely by agents, which is the population Ark targets, and an agent's
   failure to record is a *context* problem: nothing in the session ever told
   it to. The native fix is a skill, which the harness loads automatically.

   Done in this session: `skills/ark/SKILL.md` ships in the binary and
   `ark init` writes it to `.claude/skills/ark/SKILL.md` as a tracked file, so
   every agent in every clone gets it without anyone remembering. It names the
   moments that actually get missed — check `ark status` first, fix a missing
   remote *immediately*, wrap substantive sessions in `run start`/`run finish`,
   and sync before finishing. `ark init` also now prints the missing-remote
   warning every time.
3. **Resolve the worktree replica hazard** before it causes the data loss that
   criterion 1 is supposed to prove doesn't happen.

## Re-decide

Two weeks of *instrumented* use — sync on, hook installed, `ark-coverage.sh`
run weekly — on signal and pulse, which are the two repositories where work is
actually happening. That is the trial adoption.md specified and has not yet
had.

If coverage is still DARK with the automation installed and sync working, that
is a real retreat signal and it will be unambiguous. Right now it is not.

---
name: ark
description: Record engineering work in Ark — the work-record system beside Git that keeps tasks, agent runs, threads, reviews and artifacts. Use at the start and end of any substantive coding session in a repository containing .ark/, when setting Ark up in a new repository, when asked what happened or why in this repo's history, or when Ark data should reach Elk. Also use when `ark status` reports no remote, which silently means nothing is being preserved.
---

# ark

Git remembers what changed. Ark remembers **why**, and it is the only place that
memory exists — a commit message cannot hold what you were asked, what you
tried, what you rejected, or what the benchmark said afterwards.

This matters more here than in a human-run project. These repositories are built
almost entirely by agents, and an agent's context is erased at the end of every
session. Whatever you don't record is not "documented later"; it is gone the
moment the session ends.

Ark's failure mode is not bugs. It is agents finishing sessions without writing
anything down. In one trial repository this produced **0 runs recorded against
36 commits**. Everything below exists to prevent that.

## First: check the repository is actually recording

Run this before anything else in a repo that has `.ark/`:

```sh
ark status
```

Read the `sync` line. There are three failure states. Two of them are dangerous,
and for the same reason — they are **quiet**. Nothing breaks, the work proceeds,
and no record is kept:

| What you see | What it means | What to do |
|---|---|---|
| `ark: command not found` | Ark isn't installed **in this environment** | **Say so prominently in your reply, at the end of the session as well as the start.** Don't block the work — but don't let it pass silently either. See below. |
| `not an Ark repository` | Never initialized | Ask whether to adopt Ark here. Don't initialize a repo unasked. |
| `sync  no remote configured; N local mutations recorded` | **Recording to one laptop only.** No backup, no second reader, nothing reaches Elk. | Fix it now — see below. |

### When Ark isn't installed, escalate — do not just skip

This instruction used to read "note it and skip Ark for this session." That was
wrong, and it cost a full two-week measurement window: work moved to an
environment where the binary was never installed, every session skipped Ark
exactly as instructed, and the repositories went fourteen days with **zero
records of any type** while looking perfectly healthy. A silent skip is
indistinguishable from an unused tool.

So when you see `command not found`:

- **Report it in your response to the human**, in the summary they will actually
  read — not only in a tool call they may scroll past. Name the environment.
- **Say what was lost**: "this session recorded nothing in Ark" is the sentence
  that matters, not "ark is not installed."
- **Point at the fix.** A missing binary in a sandbox or cloud environment is
  almost never a per-session problem; it is the environment's setup script
  missing an install step. Fixing that is what stops it recurring. A tool that
  works only on one laptop is not installed.
- **Still don't block the work.** Escalating and blocking are different things.

The third line is the one that has actually cost this project history. A
repository can look healthy, accumulate weeks of work, and be one disk failure
from losing all of it. If you see it, say so and fix it:

```sh
ark remote set <sync-service-url>     # the team's ark-server
ark login                             # paste the service token
ark sync                              # pushes everything recorded so far
```

Joining a repository someone else already set up:

```sh
ark init --repository <repository-id>
ark remote set <sync-service-url> && ark login && ark sync
```

## The session shape

Wrap substantive work in a run. Not every session — a question answered or a
one-line typo fix earns nothing — but **any session that produces a commit, a
PR, a design decision, or a real investigation.**

**At the start**, once you know what you've been asked to do:

```sh
ark run start --task <n> -i "<what you were asked, in one or two sentences>"
```

Drop `--task <n>` if no task fits; create one first if the work deserves
tracking (`ark task create -t "..." -b "..."`). The command prints a run ID —
keep it.

**At the end**, before you write your summary to the user:

```sh
ark run finish <run-id> -s succeeded -r "<what actually happened>"
ark sync
```

Use `-s failed` when it didn't work. A failed run with an honest result is more
valuable than no record — it stops the next agent repeating the attempt.

Finishing also attaches a rendered review of the run — the task, the thread,
the diff between the two commits it recorded — as a `review.html` artifact, so
the session is saved as part of the saved session. Pass `--no-review` when you
don't want one.

**Asked what happened here?** `ark review` is the read side. It answers two
questions separately — *is anything happening* (`working`/`waiting`/`errored`/`idle`)
and *did the work land* (`settled`/`faulted`/`unanswered`/`unclear`) — and gives
every unsettled run one sentence saying what would move it. `ark review --json`
is the parseable form; `--since 24h` or `--run <id>` narrow it. Reach for it
before reconstructing a session from `git log`.

The result summary is the single highest-value string you will write all
session. Make it specific and factual:

> Adapter landed (PR elk-work/pulse#2, branch android-adapter). First real
> run vs iOS baseline: 4 match, 3 known-gap, 9 closed-gap, 0 new-drift — the
> punch list was 12 days stale. After pruning the 9 closed waivers: 13 match,
> 3 known-gap, 0 drift.

Not: "Implemented the Android adapter."

## What else earns a record

- **A decision, or a change of direction** → comment on the task. This is where
  the reasoning goes.
  ```sh
  ark task comment <n> -b "Chose pull-by-revision over an outbound webhook: ..."
  ```
- **A design discussion worth keeping** → a thread.
  ```sh
  ark thread create --task <n> -t "<topic>"
  ark thread message <thread-id> -r user|agent -b "..."
  ```
- **Evidence** — a benchmark, a report, a screenshot, a failing log.
  ```sh
  ark artifact add <file> --parent run:<run-id>
  ```
- **A task's state actually changing** → `ark task edit <n> --status in_progress|blocked|done`,
  or `ark task close <n>`. A backlog nobody updates is worse than none.

What does **not** earn a record: narrating each file you edited, restating the
diff, or opening a task per commit. Ark is memory, not a changelog — Git
already has the changelog.

## Attribution

Always identify yourself, so records attribute to the human you're acting for
rather than landing anonymous:

```sh
export ARK_AGENT_NAME=claude      # or pass --agent claude
```

Ark records that an agent acted **under a delegating human's authority**, and
everything downstream — including Elk — resolves the agent back to that person.
Skip this and the attribution chain breaks.

## Getting it into Elk

Ark records reach the Elk workspace stream through the work-record adapter:

```sh
ark elk events            # inspect what would be sent
ark elk push --dry-run    # same, as a delivery plan
ark elk push              # deliver
```

`ark elk push` needs an endpoint and token — `ARK_ELK_ENDPOINT` and
`ARK_ELK_TOKEN`, or `--url` / `--token`. Neither is stored in the repository.

Re-sending is safe and expected: Elk deduplicates on the event key, so a
repeated push is a no-op, there is no cursor to keep, and an interrupted run
needs no repair. A repository can be backfilled from nothing. Push after
finishing a run, alongside `ark sync`.

## Writing task numbers

Ark numbers tasks **per repository**, and the number is a display alias — the
ULID is authoritative, and the sync server may rewrite a number when two
offline clients mint the same one. So a number is for reading, not for
identity.

| Write | Means |
|---|---|
| `ark:signal#14` | Ark task 14 in the signal repository |
| `ark:signal#14 (01KYM1NCY7)` | the same, made durable — see below |
| `#6` | a GitHub issue or PR, unchanged |

**Never write a bare `#13` for an Ark task.** GitHub links it to an issue of
that number in whatever repository is being viewed — silently wrong rather
than merely unhelpful. `ark:` cannot be mistaken and never auto-links.

**Always name the repository, even the one you are in.** References travel:
into commit messages, PR bodies, other repositories' docs. `ark:signal#14` and
`ark:pulse#14` are different tasks, and a bare number stops meaning anything
the moment the sentence moves.

For a reference meant to outlive the task — a commit message, a design doc, a
comment another repository will read — append a ULID prefix, the part that
cannot change:

```text
ark:signal#14 (01KYM1NCY7)
```

## For parsing

- `--json` on every command; treat the field names as a stable interface.
- Exit codes: `0` ok, `2` invalid input, `3` not found, `4` conflict,
  `5` permission, `6` offline, `7` partial success needing repair.
- `ark search <query>` does full-text search across tasks, comments, threads,
  PRs and reviews — use it before asking the user to re-explain history.
- `ark task view <n>` shows a task with its comments, threads and runs.

## If Ark is unavailable

If `ark` isn't installed or the repo isn't initialized, **say so once and carry
on with the actual work**. Never block a task on Ark, and never invent records
to fill a gap — a fabricated history is worse than a missing one.

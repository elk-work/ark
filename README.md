# Ark

Ark is a local-first, agent-native work record system that sits beside Git.

Git is the durable memory for source code. Ark is the durable memory for
everything around it: tasks, conversations, agent runs, pull requests,
reviews, and artifacts.

```text
repo/
  .git/   <- source history (Git owns this)
  .ark/   <- work history (Ark owns this)
```

Ark is part of the Elk project family.

## Status

V1: **complete**. The local tool works entirely offline; when a sync service
is configured, mutations push up, records pull down, artifacts flow through
object storage, and PR merges are cloud-confirmed. The sync service
(`cmd/ark-server`) keeps one SQLite database per repository — persisted to
GCS in production (Cloud Run, scale-to-zero, no always-on database), or to
a plain directory for development. See docs/rfc-0001-per-repo-sqlite-storage.md,
and [docs/self-hosting.md](docs/self-hosting.md) to run your own instance.

See [docs/v1-spec.md](docs/v1-spec.md) for the full specification and
[docs/principles.md](docs/principles.md) for the design principles.

### Sync in brief

```sh
ark remote set https://ark.example.com   # per repository
ark login                                # token -> OS keyring, else credentials file
ark sync                                 # push mutations, upload blobs, pull records
ark repo show                            # the repository record the service holds
ark repo set --name scout                # correct it; registration can only backfill
ark repo dangling                        # references the service holds and cannot resolve
ark logout                               # take that token back out of both stores
ark repair push                          # the service lost this repository: replay it back
```

A second machine joins with `ark init --repository <id>`, then remote/login/
sync. Conflict rules follow spec §10: comments and reviews never conflict,
task/PR fields merge independently, concurrent title/body edits surface in
`ark conflict list` where `--keep local|remote` resolves them. Display
numbers minted concurrently by offline clients are renumbered by the server
(the ULID is authoritative).

### Elk work-record delivery

`ark elk push` normalizes this repository's work records and sends them to
Elk's authenticated work-record endpoint. When both existing environment
settings are present, every successful local filing starts that replay-safe
push immediately after its SQLite transaction commits:

```sh
export ARK_ELK_ENDPOINT=https://example.invalid/gh-connector/ark/events
export ARK_ELK_TOKEN=... # resolve from your secret store; never commit it
ark task create -t "Ship the widget"
```

The filing never waits on Elk and remains successful through an Elk outage.
Delivery output and failures append to `.ark/elk-push.log`; the manual
`ark elk push` command remains available for retries and backfills. Elk dedupes
on event keys, so this immediate path can safely overlap a scheduled puller.

Running the service yourself: [docs/self-hosting.md](docs/self-hosting.md)
covers both storage modes, authentication — the shared service token, and the
per-principal credentials and per-repository `read`/`write`/`admin` grants
that now sit beside it — health checks, and the backup/replay recovery path. [docs/deploy.md](docs/deploy.md) is the
short index of deployment options.

## How Ark's own work is tracked

**Ark bootstraps on GitHub. This repository has no `.ark/` directory of its
own**, and that is deliberate: a work record system cannot be the sole tracker
of its own development before it is trustworthy enough to depend on. Ark's
issues and pull requests therefore live on GitHub, at `elk-work/ark`.

So, in this repository:

- `ark status`, `ark task`, and the other record commands **fail here** with
  `no .ark directory found`. That is expected, not a bug — and not something to
  fix by running `ark init`.
- File issues and open pull requests on GitHub in the ordinary way.
- To exercise Ark against a real repository, use one that actually carries
  `.ark/`. Within the Elk project family those are **scout**, **signal**,
  **pulse**, **watch**, and **tailor** — e.g. `ark -C pulse status`.

Those five are Ark-primary; this repository is the exception, not they. Ark
passed its adoption HOLD on 2026-08-12, scout included.

## Install

Ark release binaries do not require Go or a C toolchain. Ark invokes the Git
CLI, so install [Git](https://git-scm.com/downloads) and make sure `git` is on
your PATH.

### Windows (PowerShell)

The installer selects amd64 or arm64, verifies the release SHA-256, installs
without administrator privileges, and adds its directory to your user PATH:

```powershell
$installer = Join-Path $env:TEMP 'install-ark.ps1'
Invoke-WebRequest https://raw.githubusercontent.com/elk-work/ark/main/scripts/install.ps1 -OutFile $installer
Unblock-File $installer
& $installer
ark --version
```

Open a new terminal if another program does not see the updated PATH. See
[docs/windows.md](docs/windows.md) for manual installation, joining an
existing Ark project, authentication, and source-development steps.

### Go toolchain (all platforms)

With Go >= 1.26.5 installed:

```sh
go install github.com/elk-work/ark/cmd/ark@latest
```

Prebuilt binaries for Windows, macOS, and Linux (arm64 and amd64) are attached
to each [release](https://github.com/elk-work/ark/releases).

To build from source instead:

```sh
git clone git@github.com:elk-work/ark.git
cd ark
go build ./...
```

## Quick start

```sh
cd your-git-repo
ark init                      # creates .ark/, adds it to .gitignore

ark task create -t "Ship the widget" -b "Details here"
ark task list
ark task comment 1 -b "Started on this"

ark thread create --task 1 -t "Widget design"
ark thread message <thread-id> -r user -b "Let's make it round"

ark run start --task 1 --thread <thread-id> -i "Implement round widget"
# ... agent does work on a branch ...
ark run finish <run-id> -s succeeded -r "Widget implemented"
ark artifact add bench.txt --parent run:<run-id>

ark pr create -t "Round widget" --task 1 --head widget-branch
ark pr review 1 --approve -b "LGTM"
ark pr merge 1                # verifies Git state, merges, pushes if origin exists

ark review                    # what ran here, what it produced, what still needs a person
ark review --html --open      # the same as a self-contained page

ark search widget             # FTS5 across tasks, comments, threads, PRs, reviews
ark status
```

### Reading a session back

`ark review` is the read side of everything above. It gathers the runs in
scope with what already hangs off them — the task, the thread, the pull
request and its reviews, the artifacts, and the diff between the two commits
the run recorded — and answers two questions separately, because one status
word cannot answer both:

- **liveness** — is anything happening? `working` · `waiting` · `errored` · `idle`
- **outcome** — did the work land? `settled` · `faulted` · `unanswered` · `unclear`

They are independent. A run can be idle and settled, or idle and unanswered,
and those are opposite situations. Anything not settled carries one sentence
saying what would move it, and `waiting` sorts above `errored` because a
person is the only thing that will move it.

```sh
ark review                        # finished since the last review, plus anything still running
ark review --since 24h            # a window of your own
ark review --run <id>             # one run
ark review --json                 # the same data, stable, for an agent
ark review --html --out page.html # a self-contained page: one file, no requests
ark review --run <id> --artifact  # attach the page to the run
```

`ark run finish` attaches a rendered review to the run automatically, so the
session is saved as part of the saved session. `--no-review` (or
`ARK_NO_RUN_REVIEW=1`) turns that off.

It adds nothing: no record type, no storage, no server surface. The only state
it keeps is a timestamp in `.ark/review-cursor`, which you can delete at any
time — the next review is simply wider.

## For agents

Ark is built for repositories that are themselves built by agents, so the
guidance ships with the tool rather than living in a runbook nobody reads.
`ark init` installs it at `.claude/skills/ark/SKILL.md` — a tracked file, so
every clone and every agent gets it — and `ark skill install --force` updates
it after an upgrade. `ark skill show` prints it.

It names the moments that actually get missed: check `ark status` before
anything else, fix a missing remote *immediately* (a repository with no remote
records to one machine, with no backup, and looks perfectly healthy while doing
it), wrap substantive sessions in `ark run start` / `ark run finish`, and sync
before you finish.

- **`--json` everywhere.** Every command emits stable JSON for parsing.
- **Identity.** Pass `--agent <name>` (or set `ARK_AGENT_NAME`) and records
  are attributed to that agent, delegated by the repository's default human.
  `ARK_ACTOR_ID` selects an exact actor.
- **`gh` compatibility.** `ark gh issue create|list|view|comment|close` and
  `ark gh pr create|list|view|comment|review|merge|close` mirror the GitHub
  CLI shapes agents already know. Issues map to tasks.
- **Exit codes.** `0` ok, `1` general, `2` invalid input, `3` not found,
  `4` conflict, `5` permission, `6` offline, `7` partial success needing repair.
- **References.** Tasks and PRs accept their number (`1`) or any unambiguous
  ULID prefix; threads, runs, and artifacts accept ULID prefixes.

### Writing about tasks

Task numbers are **repository-local display aliases**, not identifiers. The
ULID is authoritative, and a number can be rewritten when the sync server
resolves a collision. Two habits follow, and they matter as soon as a second
repository starts using Ark:

- **Write `ark:signal#14`, never bare `#14`.** A bare `#14` reads as a GitHub
  issue or pull request — GitHub will even link it to one — and both live in
  the same sentences. `ark:` cannot be mistaken and never auto-links.
- **Name the repository, including the one you are in.** Numbering restarts
  per repository, so `ark:signal#14` and `ark:pulse#14` are different tasks,
  and references travel: into commit messages, PR bodies, other repositories'
  docs, where the surrounding context is gone.

For a reference that must survive — a commit message, a design doc, anything
outliving the task — add a ULID prefix, since it is the part that cannot
change: `ark:signal#14 (01KYM1NCY7)`.

## Principles (short form)

1. **Do not rebuild Git.** Ark references Git objects; it never replaces them.
2. **Start with records.** Everything worth remembering is a record.
3. **Local first.** SQLite is the fast working copy; the cloud is the shared authority.
4. **Sync intent.** Ark synchronizes mutations, not rows.
5. **Use small primitives.** Repository, task, comment, PR, review, thread, run, artifact.
6. **Agents are participants.** Humans and agents create records under recorded authority.
7. **Preserve the why.** Task → thread → run → review → artifact → commit, all linked.
8. **Compatibility is leverage.** Ordinary Git repos, familiar CLI shapes.

## Development

```sh
go build ./...
go test ./...
```

`make build` writes `bin/ark` with the version stamped from
`git describe --tags --always --dirty`; `make build-server` does the same for
`bin/ark-server`. A plain `go build` needs no stamp — `ark --version` then
falls back to the module version (so `go install ...@v0.1.0` self-describes)
or to `dev` plus the short commit Go embeds automatically.

Layout follows [docs/v1-spec.md](docs/v1-spec.md) §15, with one deliberate
deviation: entity operations live in one `internal/store` package instead of
a dozen single-file packages. They can split out when cloud sync gives each
entity enough distinct behavior to earn it.

| Package | Owns |
|---|---|
| `internal/records` | ULIDs, timestamps, actor types, typed errors, exit codes |
| `internal/db` | SQLite connection, embedded migrations, transactions |
| `internal/store` | all record operations + mutation log + FTS index |
| `internal/git` | Git CLI wrapper (porcelain formats, explicit workdir) |
| `internal/app` | repository discovery, `ark init`, actor resolution |
| `internal/cli` | cobra command tree, `ark gh` shim |
| `internal/output` | human tables / stable JSON |
| `internal/buildinfo` | version reported by both binaries (ldflags → build info → `dev`) |
| `migrations/` | numbered forward-only SQL migrations |

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Contributions are accepted under an
individual contributor license agreement ([CLA.md](CLA.md)) handled
automatically on your first pull request.

## License

Apache-2.0 — see [LICENSE](LICENSE).

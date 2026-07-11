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

V1 local tool: **working**. Everything below runs today, entirely offline,
with all state in `.ark/` (SQLite + content-addressed objects). Every write
also records the *intent* behind it in a mutation log, so cloud sync
(Cloud SQL + GCS, spec Phases 4–5) can replay local history when it lands.
`ark sync` currently reports the queued mutation count and exits 6 (offline).

See [docs/v1-spec.md](docs/v1-spec.md) for the full specification and
[docs/principles.md](docs/principles.md) for the design principles.

## Install

```sh
go install github.com/ijroth/ark/cmd/ark@latest   # or: go build ./cmd/ark
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

ark search widget             # FTS5 across tasks, comments, threads, PRs, reviews
ark status
```

## For agents

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
| `migrations/` | numbered forward-only SQL migrations |

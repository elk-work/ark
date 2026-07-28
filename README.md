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
ark login                                # token -> keychain (or ~/.ark/credentials.toml)
ark sync                                 # push mutations, upload blobs, pull records
```

A second machine joins with `ark init --repository <id>`, then remote/login/
sync. Conflict rules follow spec §10: comments and reviews never conflict,
task/PR fields merge independently, concurrent title/body edits surface in
`ark conflict list` where `--keep local|remote` resolves them. Display
numbers minted concurrently by offline clients are renumbered by the server
(the ULID is authoritative).

Running the service yourself: [docs/self-hosting.md](docs/self-hosting.md)
covers both storage modes, the single-token auth model, health checks, and
the backup/replay recovery path. [docs/deploy.md](docs/deploy.md) is the
short index of deployment options.

## Install

Prerequisites: Go >= 1.26, `git` on your PATH. No C toolchain required.

The repository is currently private, so clone and build from source:

```sh
git clone git@github.com:elkproject/ark.git
cd ark
go build ./cmd/ark
```

Once the repository is public, `go install github.com/elkproject/ark/cmd/ark@latest`
will work as well.

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

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

V1, under active development. The local tool works standalone; cloud sync
(Cloud SQL + GCS) is designed but not yet wired up. See
[docs/v1-spec.md](docs/v1-spec.md) for the full specification and
[docs/principles.md](docs/principles.md) for the design principles.

## Install

```sh
go install ./cmd/ark
```

## Quick start

```sh
cd your-git-repo
ark init
ark task create --title "Ship the widget" --body "Details here"
ark task list
ark task comment 1 --body "Started on this"
ark thread create --task 1 --title "Widget design discussion"
ark run start --task 1 --agent claude-code
ark pr create --title "Widget" --head my-branch
ark pr review 1 --approve --body "LGTM"
ark pr merge 1
```

Every command supports `--json` for machine-readable output, which makes
Ark friendly to coding agents. A GitHub CLI compatibility shim is available
under `ark gh` (`ark gh issue create`, `ark gh pr merge`, ...) so existing
agent skills carry over.

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

The repository layout follows [docs/v1-spec.md](docs/v1-spec.md) §15.

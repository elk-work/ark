# Ark Design Transcript (Working Notes)

## Vision

Ark is a local-first, agent-native collaboration tool.

Git remains the source of truth for code. Ark manages the surrounding
context: tasks, conversations, agent executions, reviews, and artifacts.

## Principles

-   Do not rebuild Git.
-   Be local-first.
-   Sync mutations rather than database rows.
-   Keep the number of primitives small.
-   Let the data model evolve from actual usage.

## Architecture

Local: - Git repository - SQLite - Mutation queue - CLI

Cloud: - Cloud SQL - Google Cloud Storage - API service

SQLite is a local replica. Cloud SQL is authoritative.

## Core Entities

-   Repository
-   Task
-   Comment
-   Pull Request
-   Review
-   Agent Thread
-   Agent Run
-   Artifact

## Why Not Workspaces?

Git already provides repositories, branches, and working trees.

Agents already provide conversation and execution state.

A Workspace is therefore a derived concept and is intentionally omitted
from V1.

## Synchronization

Git synchronizes source code.

Ark synchronizes metadata using mutations:

-   create task
-   edit task
-   add comment
-   submit review

Mutation replay is simpler and more robust than row replication.

## Conflict Resolution

Git resolves source conflicts.

Cloud SQL is authoritative for metadata.

Comments are append-only.

Reviews become immutable.

Tasks merge field-by-field where practical.

## Agent Model

Agent Threads store the conversation and rationale.

Agent Runs store individual executions.

Artifacts store generated outputs.

Git answers what changed.

Agent Threads answer why it changed.

## CLI

Expose a GitHub-compatible command surface where practical so existing
agent skills can be reused.

Examples:

ark task create ark pr create ark pr merge ark review

## Storage

Git: - commits - branches - tags

SQLite: - tasks - comments - runs - threads - sync state

Cloud SQL: - canonical metadata

Google Cloud Storage: - artifacts - attachments - logs

## Naming

Project name: Ark

Suggested layout:

repo/ .git/ .ark/

Git stores source history.

Ark stores the history of work.

## Long-Term Vision

Ark is a durable library of engineering work.

It preserves goals, conversations, execution history, reviews, code, and
artifacts while remaining compatible with Git and existing developer
tooling.

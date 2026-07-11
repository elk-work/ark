# Ark Principles


# 001 — Do Not Rebuild Git

Git already does the hard part.

It stores source as a durable graph. It branches, merges, tags, fetches, pushes, and works without a server. It is installed everywhere, every coding agent already understands it, and a large part of the software world is built around its behavior.

Ark should use that.

Git owns commits, trees, blobs, branches, tags, and source merges. Ark can refer to those objects and add context around them, but it should not create a second source-control model beside them.

This is partly about avoiding unnecessary work. More importantly, it keeps Ark compatible with the tools we already use. A repository managed through Ark should still be a normal Git repository. It should still work with existing editors, agents, CI systems, and hosting services.

There may eventually be places where Git is awkward. That is not yet a reason to replace it. We should first see whether the problem can be handled by adding a record, an index, or a better local view.

If we find ourselves implementing a new commit graph or merge algorithm, we should stop and ask what problem we are actually trying to solve.

Git keeps the source history.

Ark keeps the history of the work around it.


---


# 002 — Start With Records

Software work leaves things behind.

Someone asks for a change. There is a conversation about it. An agent tries something. A review points out a problem. A second run fixes it. A benchmark, screenshot, or document is produced. Eventually a commit is merged.

Those things are worth keeping.

Ark starts with records because records are a simple way to preserve what happened without deciding too early what the final product model should be.

A task is a record. A comment is a record. An agent run is a record. A review is a record. An artifact is a record. Records can refer to other records and to Git objects.

This does not mean every table has to be a generic event table, or that all records have identical behavior. It is a way of thinking about the system. The default is to preserve a thing and relate it to the other things that give it meaning.

Over time, repeated patterns may deserve stronger abstractions. We can add those when they help. Starting with records leaves room for that evolution.

The reverse is harder. If we begin with a large hierarchy of projects, workspaces, plans, sessions, jobs, and containers, we will spend a lot of time deciding which box something belongs in.

At the beginning, Ark only needs to remember the work clearly.


---


# 003 — Local First

Most of the work happens on a local machine.

The repository is there. The branch is there. The agent is running there. The person using Ark should not have to wait for a cloud round trip to create a task, read a thread, record a run, search prior work, or attach a result.

Ark should feel like a local tool.

SQLite is the local database. It holds the working copy of Ark's records, the search indexes, and the queue of changes that have not yet reached the cloud. Git continues to hold the source itself.

The cloud is still important. It provides sharing, coordination, permissions, backup, and a canonical view when several machines or people are involved. Cloud SQL holds the authoritative shared metadata. Object storage holds larger artifacts.

This is not an active-active database design. The local database is the fast working copy. The cloud is the shared authority.

That distinction keeps the design understandable.

Local-first also gives Ark a useful failure mode. A temporary network problem should make collaboration stale, not make the tool unusable. Work can continue locally and synchronize when the connection returns.

The cloud makes Ark shared.

The local copy makes it pleasant to use.


---


# 004 — Sync Intent

Ark should synchronize actions, not just final rows.

A row tells us what something looks like now. A mutation tells us what someone was trying to do.

Create a task. Change the title. Add a comment. Submit a review. Attach an artifact. Close the task.

That difference matters when local and cloud state have moved apart.

If two databases exchange replacement rows, a conflict becomes a comparison between two finished values. It is often unclear what should be kept. If Ark exchanges mutations, it has more useful information. It can usually apply independent actions, reject an invalid action, or show a conflict in terms a person can understand.

Comments can normally append. Labels can often merge as sets. A review can become immutable once submitted. A title changed in two places may require a choice. The right behavior depends on the kind of record and the action being taken.

Cloud SQL remains authoritative for shared ordering, permissions, and accepted state. SQLite keeps the local records and the pending mutation log.

This is not event sourcing as a goal in itself. We do not need to reconstruct the entire world from an eternal event stream unless that becomes useful.

We keep the intent because it makes synchronization and history clearer.


---


# 005 — Use Small Primitives

We should not build a concept simply because other systems have one.

We considered workspaces. A workspace usually means some combination of a repository, a branch, an environment, and an agent session. Git already has repositories and branches. The agent already has a session. The environment already exists somewhere.

At the moment, it is not clear what a first-class workspace would add.

So Ark does not have workspaces yet.

The same rule applies to plans, milestones, projects, agent teams, knowledge bases, and execution graphs. Some of those may become useful. They should enter the system when repeated use makes their absence awkward, not because they make the architecture diagram look complete.

Every new primitive creates vocabulary, storage, permissions, synchronization rules, CLI commands, and migration work. It should pay for that weight by making the rest of the system simpler.

The initial set is deliberately small:

- repository
- task
- comment
- pull request
- review
- agent thread
- agent run
- artifact

Even that list may change as we build.

The goal is not minimalism for its own sake. The goal is to keep the system easy to understand while we learn what it needs to become.


---


# 006 — Agents Are Participants

Ark assumes that agents do real work.

A human may state the goal, choose an approach, review a result, or intervene when something is going wrong. An agent may inspect the repository, propose a plan, make several attempts, produce artifacts, open a pull request, and respond to review.

Both are participants in the same history.

This means agent threads and agent runs should not be hidden inside logs or treated as temporary implementation details.

A thread preserves the conversation around the work. A run records a particular execution: what agent ran, what it was asked to do, what happened, and what it produced. A review can come from a person or an agent. An artifact should have a clear origin.

The point is not to pretend agents are people. Their capabilities, permissions, and accountability may be different. Ark should record those differences rather than collapsing everyone into a generic author field.

The useful boundary is simple:

Humans and agents can both create records.

The record should say who or what created it, under whose authority, and in response to which task or prior record.


---


# 007 — Preserve the Why

Git is very good at showing what changed.

It is less good at preserving the conversation that led to the change.

Commit messages help. Pull requests help. Issue trackers help. Agent transcripts help. But the explanation is usually spread across several systems, and some of it disappears when a session ends.

Ark should keep enough of that context that a later person or agent can understand the work without reconstructing it from fragments.

A task records the intent.

A thread records the discussion.

A run records the attempt.

A review records the judgment.

An artifact records the evidence.

A commit records the resulting source change.

These records should link to one another. The links are what turn a collection of logs into a useful history.

Ark does not need to preserve every token, terminal line, or intermediate file forever. That would create noise and cost without necessarily creating understanding. The system should keep the durable parts and allow lower-level traces to be retained when they are useful.

The aim is not perfect memory.

It is enough memory to continue the work well.


---


# 008 — Compatibility Is Leverage

Ark should fit into the way software is already built.

Git repositories should remain ordinary Git repositories. Existing tools should continue to work. The command line should borrow familiar shapes where that saves users and agents from learning a new dialect.

In particular, many coding agents already know how to use the GitHub CLI. Commands such as `gh issue create`, `gh pr create`, and `gh pr merge` are part of their working vocabulary.

Ark should provide a compatible surface where it is practical.

That does not require pretending to be GitHub in every detail. It means preserving the common commands and behaviors that carry useful existing knowledge. Ark can add its own concepts—tasks, threads, runs, and artifacts—without making familiar source-control operations unfamiliar.

The installed command is `ark`. An optional `gh` compatibility mode or shim can translate the subset Ark supports. The exact implementation can evolve.

Compatibility is not a constraint we tolerate.

It is a large body of tools, habits, and agent skills we get to reuse.

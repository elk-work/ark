I think there are two categories of ideas in that previous discussion:

1. Ideas that belong in Ark’s long-term architecture.
2. Ideas that belong in the V1 spec.

Most of what you added falls into the first category. I would not change the V1 implementation spec much, but I would absolutely add two new Principles and one future RFC because they capture an important design philosophy. Google Cloud Service Tokens.txt

1. Add a Principle: Ark Builds Itself

I think this belongs alongside “Do Not Rebuild Git.”

Something like:

Ark should eventually manage its own deployment, but it must never require itself to recover itself.

Self-hosting is a goal.

Self-dependency is a bug.

The system should always have a small, boring recovery path that exists outside Ark.

Every layer should be capable of rebuilding the one above it.

That’s a much more general principle than “bootstrap registry.”

It explains why.

⸻

2. Add a Principle: Keep a Recovery Path

This one is actually broader than Ark.

One of the patterns I’ve noticed across Ark, Pulse, Watch, Scout, and your infrastructure ideas is:

Never allow the management plane to be the only way to recover the management plane.

That’s a great engineering principle.

Examples:

* Git should not live only inside Ark.
* Ark should not deploy only itself.
* Pulse shouldn’t be the only way to test Pulse.
* Watch shouldn’t be the only source of observability.

That’s a recurring philosophy.

I’d probably write something like:

Every system should have a boring recovery path.

The normal path can be elegant.

The recovery path should be obvious.

If recovery depends on the system being recovered, it isn’t really a recovery path.

That feels very “you.”

⸻

3. V2 RFC: Self Hosting

I’d eventually add something like:

RFC-0012
Self Hosting

which explains:

bootstrap
↓
seed
↓
core
↓
preview
↓
core

without locking in names.

Because honestly, those names will evolve.

The layering probably won’t. Google Cloud Service Tokens.txt

⸻

4. Bootstrap Registry

I actually would not put this in the Principles.

It’s an implementation.

The principle is:

There is always an external recovery path.

The bootstrap registry is simply one way to implement it.

⸻

5. Promotion Records

This one is interesting.

This does fit our “Everything is a Record” idea perfectly.

Instead of saying:

Promotion is a deployment event.

I’d say:

Promotion is another Record.

Just like:

Task
Review
Run
Artifact
Promotion

Promotion becomes just another immutable fact in Ark.

That feels much more coherent with the rest of the model than making promotions special. Google Cloud Service Tokens.txt

⸻

6. One thing I’d change in the current docs

This is the only thing I think is actually missing.

We’ve said:

Everything is a Record.

I think the more precise statement is:

Everything worth remembering is a Record.

That tiny change matters.

You don’t want:

* every token
* every keystroke
* every log line
* every websocket event

to become a record.

You want the durable things.

That fits your earlier comments about avoiding unnecessary abstractions and unnecessary data.

⸻

One larger observation

Reading that earlier discussion made something click for me.

Ark isn’t primarily a source control tool.

It isn’t even primarily a collaboration tool.

It’s a durable memory system for engineering.

Git is the durable memory for source code.

Ark is the durable memory for everything else.

That’s a cleaner mental model than “GitHub for agents,” and I think it aligns much better with everything you’ve designed—Echo, Pulse, Watch, Scout, and the observability/data engine all want to consume and produce durable engineering memory.

If we continue refining the documents, I’d subtly steer all of them toward that idea. It feels like the architectural center of gravity that the project has been converging toward.

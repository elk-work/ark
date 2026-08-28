package server

// The actor half of spec §20: binding a repository's actors to the principals
// that introduced them, and enforcing the delegation chain Ark already records
// (RFC-0003 Decisions 4 and 5; elk-work/ark#52).
//
// `Mutation.CreatedBy` has been on the wire since the sync protocol shipped
// and was read nowhere in this package. Every record carries `created_by` and
// `created_by_type`, and until now nothing verified either — the agent skill's
// "skip this and the attribution chain breaks" was an honour system. This is
// what makes it checkable.
//
// # The rule, and the trap it has to avoid
//
// `internal/sync/sync.go` sends `Store.AllActors` on every push and every
// registration, so a client re-uploads every actor it knows, including ones it
// pulled from other people. The rule therefore cannot be "every actor in the
// payload belongs to the pusher". **Carrying somebody's actor record is a
// no-op; writing as them is the rejection.**
//
// # Why humans and agents are treated differently
//
// A human actor belongs to one person: `ark init` mints one per checkout and
// nothing else ever adopts it. Binding it and enforcing it is exact.
//
// An agent actor has been *shared*, and the sharing was the client's.
// `store.FindAgentActor` used to resolve a named agent to the lowest-ULID
// actor with that `agent_name` in the local database — which, after a pull,
// includes agents other people introduced — so two developers who both ran
// `--agent claude-code` in one repository converged on one agent actor.
// elk-work/ark#100 fixed that: the lookup now keys on
// (`agent_name`, `delegated_by`), so each person's agent is their own.
//
// **The service still does not enforce the binding on agent actors, and must
// not yet.** A client that has not upgraded past #100 still resolves to the
// shared actor, and refusing it would stop that person's sync with an error
// they cannot fix — the choice is made by their client before the request
// exists. Enforcement can follow once the fleet has moved; elk-work/ark#101
// carries it, and the test that would pin it is one line away.
//
// So the binding is *recorded* for every actor and *enforced* on human actors,
// where it has always been exact — plus the delegation rule below, which is
// what governs agents in the meantime. What that leaves open is stated rather
// than hidden: a principal can write as an agent an old client introduced, and
// the record then names that agent's delegating human. It is a smaller hole
// than the one it replaces, where anyone could write as anyone.
//
// # The legacy service token binds nothing and is bound by nothing
//
// It is one string the whole fleet holds and it identifies nobody, so an actor
// "introduced by" it is introduced by everybody. Binding under it would hand
// every existing actor to a principal that is not a person, and the first
// developer to move to a credential would find their own actor already owned.
// The legacy branch therefore skips this file entirely, and an actor with no
// binding is free for whoever first writes as it — which is how six live
// repositories full of unbound actors survive this change.

import (
	"context"
	"database/sql"
	"errors"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// binderOf is the principal an actor introduced by this request belongs to,
// or "" when nothing should be bound — the legacy token, or a route that
// somehow reached here unauthenticated.
func binderOf(who *authenticated) string {
	if who == nil || who.Legacy {
		return ""
	}
	return who.ID
}

// actorFacts is what the service knows about an actor already in this
// repository: enough to judge a write as it, and nothing more.
type actorFacts struct {
	Type        string
	DelegatedBy string
	// Principal is the principal that introduced or first wrote as this
	// actor. Empty means unbound: an actor from before this rule existed, or
	// one the legacy token introduced.
	Principal string
}

// loadActorFacts reads an actor record and its binding. ok is false when the
// repository holds no such actor, which is not an error: a mutation may name
// an actor whose record has not arrived, and refusing that would turn an
// ordering skew into a permanent rejection (the reasoning applyCreate uses
// for dangling references).
func loadActorFacts(ctx context.Context, tx *sql.Tx, actorID string) (actorFacts, bool, error) {
	var f actorFacts
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(r.data->>'type', ''),
		COALESCE(r.data->>'delegated_by', ''), COALESCE(b.principal_id, '')
		FROM records r LEFT JOIN actor_principals b ON b.actor_id = r.record_id
		WHERE r.record_type = 'actor' AND r.record_id = ?`, actorID).
		Scan(&f.Type, &f.DelegatedBy, &f.Principal)
	if errors.Is(err, sql.ErrNoRows) {
		return f, false, nil
	}
	return f, err == nil, err
}

// bindActor records that a principal introduced, or first wrote as, an actor.
// DO NOTHING on conflict is what makes it first-writer-binds rather than
// last: the binding is never moved, and the statement is safe to replay after
// a lost compare-and-swap.
func bindActor(ctx context.Context, tx *sql.Tx, actorID, principalID string) error {
	if actorID == "" || principalID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO actor_principals (actor_id, principal_id, bound_at)
		VALUES (?, ?, ?) ON CONFLICT (actor_id) DO NOTHING`, actorID, principalID, records.Now())
	return err
}

// introduceActors upserts the actors a request carries and applies the two
// rules that govern a *new* one: it is bound to the principal introducing it,
// and a new agent must name a human it may legitimately act for.
//
// The delegation check runs in a second pass, over the actors this request
// actually introduced, so it cannot depend on the order they arrive in. A
// client sends whatever `AllActors` returns, and an agent that happened to
// sort before its own human would otherwise be refused for a reason that has
// nothing to do with authority.
func introduceActors(ctx context.Context, tx *sql.Tx, actors []api.Actor, who *authenticated) error {
	binder := binderOf(who)
	introduced := make([]api.Actor, 0, len(actors))
	for _, a := range actors {
		if a.ID == "" {
			continue
		}
		created, err := upsertActor(ctx, tx, a)
		if err != nil {
			return err
		}
		if !created {
			// An actor the service already holds. Re-sending it is what
			// every client does on every sync, and it changes nothing —
			// including, deliberately, its `delegated_by`: a request cannot
			// re-point an existing agent at a different human, because the
			// stored record is never rewritten from the payload.
			continue
		}
		introduced = append(introduced, a)
		if err := bindActor(ctx, tx, a.ID, binder); err != nil {
			return err
		}
	}
	if binder == "" {
		// The legacy token, which predates the rule and carries the whole
		// fleet. Nothing was bound above and nothing is checked here.
		return nil
	}
	for _, a := range introduced {
		if a.Type != string(records.ActorAgent) {
			continue
		}
		if err := checkDelegation(ctx, tx, a, who); err != nil {
			return err
		}
	}
	return nil
}

// checkDelegation is RFC-0003 Decision 5 on the sync path: a new agent actor
// must name a human it acts for, and that human must not be somebody else's.
//
// The write routes have applied exactly this rule since RFC-0004
// (`resolveWriter`); `/v1/sync/push` is the surface that never had it, so an
// unrelated caller could introduce an agent claiming to act for anyone. The
// check costs one lookup.
//
// "Not somebody else's" rather than "bound to me" is deliberate. An actor
// introduced before this rule existed is unbound, and every human actor in
// the six live repositories is one — requiring a positive binding would refuse
// the first agent every one of those people registers after moving to a
// credential, for a chain that is in fact correct.
func checkDelegation(ctx context.Context, tx *sql.Tx, a api.Actor, who *authenticated) error {
	if a.DelegatedBy == "" {
		return faultPermission("actor " + a.ID + " (" + a.Name + ") is an agent with no delegated_by: " +
			"an agent acts under a human's authority, and a new one must say whose")
	}
	human, ok, err := loadActorFacts(ctx, tx, a.DelegatedBy)
	if err != nil {
		return err
	}
	if !ok || human.Type != string(records.ActorHuman) {
		return faultPermission("actor " + a.ID + " (" + a.Name + ") delegates from " + a.DelegatedBy +
			", which is not a human actor in this repository")
	}
	if human.Principal != "" && human.Principal != who.ID {
		return faultPermission("actor " + a.ID + " (" + a.Name + ") delegates from " + a.DelegatedBy +
			", which belongs to another principal: an agent cannot claim authority it was not given")
	}
	return nil
}

// authorizeMutations is the enforcement half — the point of the slice.
//
// For every mutation in a push, the actor its `created_by` names must not be
// one this repository has already bound to somebody else. An unbound actor is
// claimed here rather than refused: first-writer-binds applies to writing as
// an actor exactly as it applies to introducing one, and it is what closes the
// gap for the actors that existed before any of this did.
//
// A refusal fails the whole push rather than the one mutation. A push is a
// client's queue, in order, and half-applying one whose identity the service
// does not accept would leave the client to reconcile a rejection it cannot
// act on. `permission` is the code, exit 5 (spec §22).
func authorizeMutations(ctx context.Context, tx *sql.Tx, muts []api.Mutation, who *authenticated) error {
	binder := binderOf(who)
	if binder == "" {
		return nil
	}
	// One lookup per distinct actor, not per mutation: a push of two hundred
	// mutations from one client names one actor.
	seen := map[string]bool{}
	for _, m := range muts {
		if m.CreatedBy == "" || seen[m.CreatedBy] {
			continue
		}
		seen[m.CreatedBy] = true

		facts, ok, err := loadActorFacts(ctx, tx, m.CreatedBy)
		if err != nil {
			return err
		}
		if !ok {
			// No record for this actor yet. There is nothing to bind and
			// nothing to protect; the mutation engine's own referential
			// handling deals with the record it produces.
			continue
		}
		if facts.Principal == "" {
			// Unbound: this principal is the first to write as it, and now
			// owns it. Agents are bound here too — the record is worth
			// having — but only human actors are refused below.
			if err := bindActor(ctx, tx, m.CreatedBy, binder); err != nil {
				return err
			}
			continue
		}
		if facts.Principal == binder {
			continue
		}
		if facts.Type != string(records.ActorHuman) {
			// A shared agent actor. See the file comment: refusing here
			// would break a client whose own actor resolution chose this
			// actor for it, and the chain it writes under is the agent's
			// `delegated_by`, not the pusher.
			continue
		}
		return faultPermission("mutation " + m.ID + " is written as actor " + m.CreatedBy +
			", which belongs to another principal: " + principalLabel(who) +
			" cannot write as somebody else. Carrying their actor record is fine; writing as them is not")
	}
	return nil
}

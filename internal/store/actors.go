package store

import (
	"context"
	"database/sql"

	"github.com/elk-work/ark/internal/records"
)

// CreateActor inserts an actor row. Actors are identity records, not work
// records, so they do not enter the mutation log in V1.
func CreateActor(ctx context.Context, d *sql.DB, a *Actor) error {
	if a.ID == "" {
		a.ID = records.NewID()
	}
	if !a.Type.Valid() {
		return records.Validationf("invalid actor type %q", a.Type)
	}
	_, err := d.ExecContext(ctx, `INSERT INTO actors
		(id, type, name, email, agent_name, agent_version, delegated_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, string(a.Type), a.Name, a.Email, a.AgentName, a.AgentVersion, a.DelegatedBy, records.Now())
	if err != nil {
		return records.DBErr("create actor", err)
	}
	return nil
}

// GetActor loads an actor by ID.
func GetActor(ctx context.Context, d *sql.DB, id string) (*Actor, error) {
	var a Actor
	var typ string
	err := d.QueryRowContext(ctx, `SELECT id, type, name, email, agent_name, agent_version, delegated_by
		FROM actors WHERE id = ?`, id).
		Scan(&a.ID, &typ, &a.Name, &a.Email, &a.AgentName, &a.AgentVersion, &a.DelegatedBy)
	if err == sql.ErrNoRows {
		return nil, records.NotFoundf("actor %s not found", id)
	}
	if err != nil {
		return nil, records.DBErr("load actor", err)
	}
	a.Type = records.ActorType(typ)
	return &a, nil
}

// FindAgentActor returns the actor row for a named agent (creating it if
// missing) so repeated runs by the same agent share one identity.
//
// That identity is per (agent name, delegating human), not per agent name.
// Sync brings in the agent actors other people introduced, so a name-only
// lookup converged two developers who both run `--agent claude-code` in one
// repository onto a single actor — whichever row happened to hold the lowest
// ULID — and from then on one of them wrote records under an identity whose
// delegated_by names the other person (elk-work/ark#93). Keying on
// delegated_by as well resolves `--agent claude-code` to *this* human's
// claude-code, so a repository holds one agent actor per name per delegating
// human and each record says whose authority it was actually written under.
//
// Records already written keep the actor they name; nothing is re-attributed.
// This decides who the *next* write is attributed to and nothing else. So the
// first run after the upgrade finds no actor for (name, this human) and
// registers one, and a repository two people share ends up with two actors of
// that name. That is the repair, not a symptom of it: they are two identities
// that were always distinct and had been collapsed into one.
//
// Within that set the lookup still has to pick one and always pick the same
// one: the first registration, which is the lowest ULID. Ordering by
// created_at would not be that — it is RFC3339Nano text, which SQLite
// compares byte by byte and which does not sort chronologically
// (records.TimeCompare) — and with LIMIT 1 the misorder changes which
// identity every later run is attributed to.
//
// delegatedBy must name a human actor this database already holds; see
// requireDelegatingHuman.
func FindAgentActor(ctx context.Context, d *sql.DB, agentName, agentVersion, delegatedBy string) (*Actor, error) {
	if err := requireDelegatingHuman(ctx, d, agentName, delegatedBy); err != nil {
		return nil, err
	}
	var a Actor
	var typ string
	err := d.QueryRowContext(ctx, `SELECT id, type, name, email, agent_name, agent_version, delegated_by
		FROM actors WHERE type = 'agent' AND agent_name = ? AND delegated_by = ?
		ORDER BY id LIMIT 1`, agentName, delegatedBy).
		Scan(&a.ID, &typ, &a.Name, &a.Email, &a.AgentName, &a.AgentVersion, &a.DelegatedBy)
	if err == nil {
		a.Type = records.ActorType(typ)
		return &a, nil
	}
	if err != sql.ErrNoRows {
		return nil, records.DBErr("find agent actor", err)
	}
	a = Actor{
		Type:         records.ActorAgent,
		Name:         agentName,
		AgentName:    agentName,
		AgentVersion: agentVersion,
		DelegatedBy:  delegatedBy,
	}
	if err := CreateActor(ctx, d, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// requireDelegatingHuman checks that delegatedBy names a human actor this
// database holds, before a lookup keyed on it is allowed to mint anything.
//
// An agent acts under a human's authority and the records it writes have to
// name whose, so a missing, dangling or non-human delegation is refused
// rather than registered. Refusing is also what stops the new key from being
// satisfiable only by a row it would have to create again on every run: the
// value the lookup keys on is one this repository can resolve, and the actor
// minted under it is the actor the next run finds. A dangling delegated_by
// would otherwise sit in the actors table, ride every push, and come back as
// a service-side rejection (RFC-0003 Decision 5, RFC-0004 Decision 2) long
// after the person who set the value could do anything about it.
//
// Both bad cases are reachable in ordinary use, not just by hand-editing:
// actors arrive by pull, so an id can name someone this client does not hold,
// and nothing else checks an id typed into ARK_DELEGATED_BY.
func requireDelegatingHuman(ctx context.Context, d *sql.DB, agentName, delegatedBy string) error {
	if delegatedBy == "" {
		return records.Validationf(
			"agent %q has no delegating human: an agent acts under a human's authority, and the records it writes have to name whose",
			agentName)
	}
	var typ string
	err := d.QueryRowContext(ctx, `SELECT type FROM actors WHERE id = ?`, delegatedBy).Scan(&typ)
	if err == sql.ErrNoRows {
		return records.Validationf("agent %q delegates from %s, which is not an actor in this repository",
			agentName, delegatedBy)
	}
	if err != nil {
		return records.DBErr("load delegating actor", err)
	}
	if records.ActorType(typ) != records.ActorHuman {
		return records.Validationf("agent %q delegates from %s, which is a %s actor: an agent delegates from a human",
			agentName, delegatedBy, typ)
	}
	return nil
}

// ActorNames returns a map of actor ID to display name for rendering lists.
func ActorNames(ctx context.Context, d *sql.DB) (map[string]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT id, name FROM actors`)
	if err != nil {
		return nil, records.DBErr("load actors", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, records.DBErr("scan actor", err)
		}
		out[id] = name
	}
	return out, rows.Err()
}

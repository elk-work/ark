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
func FindAgentActor(ctx context.Context, d *sql.DB, agentName, agentVersion, delegatedBy string) (*Actor, error) {
	var a Actor
	var typ string
	err := d.QueryRowContext(ctx, `SELECT id, type, name, email, agent_name, agent_version, delegated_by
		FROM actors WHERE type = 'agent' AND agent_name = ? ORDER BY created_at LIMIT 1`, agentName).
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

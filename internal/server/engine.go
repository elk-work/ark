// Package server implements the Ark sync service: an HTTP API over
// PostgreSQL that accepts mutations, applies the conflict rules from
// docs/v1-spec.md §10, and serves record pulls by revision.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ijroth/ark/pkg/api"
)

// Rules per record type. Append-only types never conflict; mergeable types
// merge field-by-field with title/body requiring human resolution.
var (
	appendOnlyTypes = map[string]bool{
		"comment":        true,
		"thread_message": true,
		"review":         true,
		"artifact":       true,
		"actor":          true,
	}
	// Fields where concurrent edits need a person: conflict.
	conflictFields = map[string]bool{"title": true, "body": true}
	// Numbered types get repository-local numbers reconciled at create.
	numberedTypes = map[string]bool{"task": true, "pull_request": true}
)

// outcomeStatus classifies a processed mutation.
type outcomeStatus string

const (
	statusApplied  outcomeStatus = "applied"
	statusRejected outcomeStatus = "rejected"
	statusConflict outcomeStatus = "conflict"
)

type outcome struct {
	status   outcomeStatus
	err      string
	remote   json.RawMessage
	revision int64
}

// sessionRevisions tracks revisions produced earlier in the same push, so a
// client's own sequence of offline changes applies causally: a create
// followed by an edit of the same record must not read as a concurrent
// server-side change that cloud-wins away the edit.
type sessionRevisions map[string]int64

func (s sessionRevisions) key(m api.Mutation) string { return m.RecordType + "/" + m.RecordID }

// processMutation handles one mutation inside the push transaction: replays
// return their stored outcome; fresh mutations apply inside a savepoint so a
// failed statement cannot poison the rest of the batch, and the outcome is
// recorded after the savepoint resolves. The repository row is locked by the
// caller, serializing revision allocation.
func processMutation(ctx context.Context, tx *sql.Tx, repoID string, m api.Mutation, session sessionRevisions) outcome {
	// Idempotency: replays return the stored outcome.
	var prior outcome
	var priorStatus string
	err := tx.QueryRowContext(ctx, `SELECT status, error, COALESCE(remote, 'null'), server_revision
		FROM applied_mutations WHERE mutation_id = $1`, m.ID).
		Scan(&priorStatus, &prior.err, &prior.remote, &prior.revision)
	if err == nil {
		prior.status = outcomeStatus(priorStatus)
		if string(prior.remote) == "null" {
			prior.remote = nil
		}
		return prior
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return outcome{status: statusRejected, err: "idempotency check failed: " + err.Error()}
	}

	// This client's earlier mutations in this push are causal history, not
	// concurrent edits: lift the base revision to what this push produced.
	if sessionRev, ok := session[session.key(m)]; ok && sessionRev > m.BaseServerRevision {
		m.BaseServerRevision = sessionRev
	}

	if _, err := tx.ExecContext(ctx, `SAVEPOINT mut`); err != nil {
		return outcome{status: statusRejected, err: "savepoint: " + err.Error()}
	}
	out := applyMutation(ctx, tx, repoID, m)
	if out.status == statusApplied {
		if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT mut`); err != nil {
			return outcome{status: statusRejected, err: "release savepoint: " + err.Error()}
		}
	} else {
		// Rejections and conflicts leave no partial writes behind, and a
		// rollback also clears any aborted-statement state in the scope.
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT mut`); err != nil {
			return outcome{status: statusRejected, err: "rollback savepoint: " + err.Error()}
		}
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO applied_mutations
		(mutation_id, repository_id, status, error, remote, server_revision)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		m.ID, repoID, string(out.status), out.err, nullableJSON(out.remote), out.revision); err != nil {
		return outcome{status: statusRejected, err: "record outcome: " + err.Error()}
	}
	if out.status == statusApplied && out.revision > 0 {
		session[session.key(m)] = out.revision
	}
	return out
}

func applyMutation(ctx context.Context, tx *sql.Tx, repoID string, m api.Mutation) outcome {
	switch m.Operation {
	case "create", "append", "submit":
		return applyCreate(ctx, tx, repoID, m)
	case "update":
		return applyUpdate(ctx, tx, repoID, m)
	case "delete":
		return applyDelete(ctx, tx, repoID, m)
	default:
		return outcome{status: statusRejected, err: fmt.Sprintf("unknown operation %q", m.Operation)}
	}
}

// nextRevision bumps and returns the repository revision counter.
func nextRevision(ctx context.Context, tx *sql.Tx, repoID string) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx,
		`UPDATE repositories SET revision = revision + 1 WHERE id = $1 RETURNING revision`,
		repoID).Scan(&rev)
	return rev, err
}

func applyCreate(ctx context.Context, tx *sql.Tx, repoID string, m api.Mutation) outcome {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE repository_id = $1 AND record_type = $2 AND record_id = $3`,
		repoID, m.RecordType, m.RecordID).Scan(&exists); err == nil {
		// Same record pushed twice (e.g. lost response): already there.
		var rev int64
		tx.QueryRowContext(ctx, `SELECT server_revision FROM records
			WHERE repository_id = $1 AND record_type = $2 AND record_id = $3`,
			repoID, m.RecordType, m.RecordID).Scan(&rev)
		return outcome{status: statusApplied, revision: rev}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return outcome{status: statusRejected, err: err.Error()}
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(m.Payload, &fields); err != nil {
		return outcome{status: statusRejected, err: "payload is not a JSON object"}
	}

	data := m.Payload
	// Two offline clients can both mint task #7; the ULID is authoritative
	// and the server reassigns the display number (docs/v1-spec.md §6.2).
	if numberedTypes[m.RecordType] {
		renumbered, err := reconcileNumber(ctx, tx, repoID, m.RecordType, fields)
		if err != nil {
			return outcome{status: statusRejected, err: err.Error()}
		}
		if renumbered != nil {
			data = renumbered
		}
	}
	// An artifact whose content already lives in object storage gets its
	// storage key stamped at creation (content addressing dedups blobs).
	if m.RecordType == "artifact" {
		enriched, err := stampStoredBlob(ctx, tx, repoID, fields)
		if err != nil {
			return outcome{status: statusRejected, err: err.Error()}
		}
		if enriched != nil {
			data = enriched
		}
	}

	rev, err := nextRevision(ctx, tx, repoID)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO records
		(repository_id, record_type, record_id, data, server_revision)
		VALUES ($1, $2, $3, $4, $5)`,
		repoID, m.RecordType, m.RecordID, string(data), rev); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if err := setFieldRevisions(ctx, tx, repoID, m.RecordType, m.RecordID, keys(fields), rev); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	return outcome{status: statusApplied, revision: rev}
}

// reconcileNumber returns a rewritten payload when the incoming display
// number is already taken by a different record, and records the type's
// high-water mark.
func reconcileNumber(ctx context.Context, tx *sql.Tx, repoID, recordType string, fields map[string]json.RawMessage) (json.RawMessage, error) {
	var number int64
	if raw, ok := fields["number"]; ok {
		json.Unmarshal(raw, &number)
	}
	var taken bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE repository_id = $1 AND record_type = $2 AND (data->>'number')::bigint = $3 LIMIT 1`,
		repoID, recordType, number).Scan(&taken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // number is free; keep it
	}
	if err != nil {
		return nil, err
	}
	var max sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX((data->>'number')::bigint) FROM records
		WHERE repository_id = $1 AND record_type = $2`, repoID, recordType).Scan(&max); err != nil {
		return nil, err
	}
	fields["number"] = json.RawMessage(fmt.Sprintf("%d", max.Int64+1))
	return json.Marshal(fields)
}

// stampStoredBlob fills storage_key on a new artifact when its blob is
// already in object storage.
func stampStoredBlob(ctx context.Context, tx *sql.Tx, repoID string, fields map[string]json.RawMessage) (json.RawMessage, error) {
	var sha string
	if raw, ok := fields["sha256"]; ok {
		json.Unmarshal(raw, &sha)
	}
	if sha == "" {
		return nil, nil
	}
	if raw, ok := fields["storage_key"]; ok {
		var existing string
		if json.Unmarshal(raw, &existing) == nil && existing != "" {
			return nil, nil
		}
	}
	var key string
	err := tx.QueryRowContext(ctx, `SELECT storage_key FROM blobs
		WHERE repository_id = $1 AND sha256 = $2 AND stored`, repoID, sha).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	keyJSON, _ := json.Marshal(key)
	fields["storage_key"] = keyJSON
	return json.Marshal(fields)
}

func applyUpdate(ctx context.Context, tx *sql.Tx, repoID string, m api.Mutation) outcome {
	if appendOnlyTypes[m.RecordType] {
		return outcome{status: statusRejected,
			err: fmt.Sprintf("%s records are immutable; create a superseding record", m.RecordType)}
	}
	var current json.RawMessage
	var currentRev int64
	err := tx.QueryRowContext(ctx, `SELECT data, server_revision FROM records
		WHERE repository_id = $1 AND record_type = $2 AND record_id = $3 AND deleted_at IS NULL`,
		repoID, m.RecordType, m.RecordID).Scan(&current, &currentRev)
	if errors.Is(err, sql.ErrNoRows) {
		return outcome{status: statusRejected, err: "record not found"}
	}
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}

	var incoming map[string]json.RawMessage
	if err := json.Unmarshal(m.Payload, &incoming); err != nil || len(incoming) == 0 {
		return outcome{status: statusRejected, err: "payload must be a JSON object of changed fields"}
	}
	delete(incoming, "version") // client-local bookkeeping

	// Field-level merge (spec §10.4): a field also changed on the server
	// since the client's base revision either conflicts (title/body) or is
	// won by the cloud (everything else, dropped from the payload).
	if m.BaseServerRevision < currentRev {
		fieldRevs, err := loadFieldRevisions(ctx, tx, repoID, m.RecordType, m.RecordID)
		if err != nil {
			return outcome{status: statusRejected, err: err.Error()}
		}
		for field := range incoming {
			if fieldRevs[field] <= m.BaseServerRevision {
				continue // untouched on the server since base: merges cleanly
			}
			if conflictFields[field] {
				return outcome{status: statusConflict,
					err:    fmt.Sprintf("%s changed on the server since your base revision", field),
					remote: current, revision: currentRev}
			}
			delete(incoming, field) // cloud wins
		}
		if len(incoming) == 0 {
			// Everything the client changed was superseded by the server.
			return outcome{status: statusApplied, revision: currentRev}
		}
	}

	merged, err := mergeJSON(current, incoming)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	rev, err := nextRevision(ctx, tx, repoID)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records SET data = $4, server_revision = $5, updated_at = now()
		WHERE repository_id = $1 AND record_type = $2 AND record_id = $3`,
		repoID, m.RecordType, m.RecordID, string(merged), rev); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if err := setFieldRevisions(ctx, tx, repoID, m.RecordType, m.RecordID, keys(incoming), rev); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	return outcome{status: statusApplied, revision: rev}
}

func applyDelete(ctx context.Context, tx *sql.Tx, repoID string, m api.Mutation) outcome {
	rev, err := nextRevision(ctx, tx, repoID)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	res, err := tx.ExecContext(ctx, `UPDATE records SET deleted_at = now(), server_revision = $4
		WHERE repository_id = $1 AND record_type = $2 AND record_id = $3 AND deleted_at IS NULL`,
		repoID, m.RecordType, m.RecordID, rev)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return outcome{status: statusApplied, revision: rev} // already gone: idempotent
	}
	return outcome{status: statusApplied, revision: rev}
}

// mergeJSON overlays the incoming fields onto the current document.
func mergeJSON(current json.RawMessage, incoming map[string]json.RawMessage) (json.RawMessage, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(current, &doc); err != nil {
		return nil, fmt.Errorf("stored record is not an object: %w", err)
	}
	for k, v := range incoming {
		doc[k] = v
	}
	return json.Marshal(doc)
}

func loadFieldRevisions(ctx context.Context, tx *sql.Tx, repoID, recordType, recordID string) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT field, revision FROM field_revisions
		WHERE repository_id = $1 AND record_type = $2 AND record_id = $3`,
		repoID, recordType, recordID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var f string
		var r int64
		if err := rows.Scan(&f, &r); err != nil {
			return nil, err
		}
		out[f] = r
	}
	return out, rows.Err()
}

func setFieldRevisions(ctx context.Context, tx *sql.Tx, repoID, recordType, recordID string, fields []string, rev int64) error {
	for _, f := range fields {
		if _, err := tx.ExecContext(ctx, `INSERT INTO field_revisions
			(repository_id, record_type, record_id, field, revision)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (repository_id, record_type, record_id, field)
			DO UPDATE SET revision = EXCLUDED.revision`,
			repoID, recordType, recordID, f, rev); err != nil {
			return err
		}
	}
	return nil
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func nullableJSON(raw json.RawMessage) any {
	if raw == nil {
		return nil
	}
	return string(raw)
}

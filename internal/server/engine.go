// Package server implements the Ark sync service: an HTTP API over
// per-repository SQLite databases that accepts mutations, applies the
// conflict rules from docs/v1-spec.md §10, and serves record pulls by
// revision.
package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// Rules per record type. Append-only types never conflict; mergeable types
// merge field-by-field with title/body requiring human resolution.
//
// promotion appears in none of these maps deliberately: it is not
// append-only (ended_at closes it later), not numbered, and needs no
// conflictFields — only ended_at and metadata_json ever mutate, and
// cloud-wins is the correct resolution for a concurrent update.
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
// recorded after the savepoint resolves. The per-repository database gives
// each push exclusive access, serializing revision allocation.
func processMutation(ctx context.Context, tx *sql.Tx, m api.Mutation, session sessionRevisions) outcome {
	// Idempotency: replays return the stored outcome.
	var prior outcome
	var priorStatus string
	var priorRemote sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT status, error, remote, server_revision
		FROM applied_mutations WHERE mutation_id = ?`, m.ID).
		Scan(&priorStatus, &prior.err, &priorRemote, &prior.revision)
	if err == nil {
		prior.status = outcomeStatus(priorStatus)
		if priorRemote.Valid {
			prior.remote = json.RawMessage(priorRemote.String)
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
	out := applyMutation(ctx, tx, m)
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
		(mutation_id, status, error, remote, server_revision, applied_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		m.ID, string(out.status), out.err, nullableJSON(out.remote), out.revision, records.Now()); err != nil {
		return outcome{status: statusRejected, err: "record outcome: " + err.Error()}
	}
	if out.status == statusApplied && out.revision > 0 {
		session[session.key(m)] = out.revision
	}
	return out
}

func applyMutation(ctx context.Context, tx *sql.Tx, m api.Mutation) outcome {
	switch m.Operation {
	case "create", "append", "submit":
		return applyCreate(ctx, tx, m)
	case "update":
		return applyUpdate(ctx, tx, m)
	case "delete":
		return applyDelete(ctx, tx, m)
	default:
		return outcome{status: statusRejected, err: fmt.Sprintf("unknown operation %q", m.Operation)}
	}
}

// nextRevision bumps and returns the repository revision counter.
func nextRevision(ctx context.Context, tx *sql.Tx) (int64, error) {
	var rev int64
	err := tx.QueryRowContext(ctx,
		`UPDATE meta SET revision = revision + 1 WHERE id = 1 RETURNING revision`).Scan(&rev)
	return rev, err
}

func applyCreate(ctx context.Context, tx *sql.Tx, m api.Mutation) outcome {
	var existingRev int64
	err := tx.QueryRowContext(ctx, `SELECT server_revision FROM records
		WHERE record_type = ? AND record_id = ?`, m.RecordType, m.RecordID).Scan(&existingRev)
	if err == nil {
		// Same record pushed twice (e.g. lost response): already there.
		return outcome{status: statusApplied, revision: existingRev}
	}
	if !errors.Is(err, sql.ErrNoRows) {
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
		renumbered, err := reconcileNumber(ctx, tx, m.RecordType, fields)
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
		enriched, err := stampStoredBlob(ctx, tx, fields)
		if err != nil {
			return outcome{status: statusRejected, err: err.Error()}
		}
		if enriched != nil {
			data = enriched
		}
	}

	rev, err := nextRevision(ctx, tx)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	now := records.Now()
	if _, err := tx.ExecContext(ctx, `INSERT INTO records
		(record_type, record_id, data, server_revision, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		m.RecordType, m.RecordID, string(data), rev, now, now); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if err := setFieldRevisions(ctx, tx, m.RecordType, m.RecordID, keys(fields), rev); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	return outcome{status: statusApplied, revision: rev}
}

// reconcileNumber returns a rewritten payload when the incoming display
// number is already taken by a different record.
func reconcileNumber(ctx context.Context, tx *sql.Tx, recordType string, fields map[string]json.RawMessage) (json.RawMessage, error) {
	var number int64
	if raw, ok := fields["number"]; ok {
		json.Unmarshal(raw, &number)
	}
	var taken bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE record_type = ? AND CAST(data->>'number' AS INTEGER) = ? LIMIT 1`,
		recordType, number).Scan(&taken)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil // number is free; keep it
	}
	if err != nil {
		return nil, err
	}
	var max sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(CAST(data->>'number' AS INTEGER)) FROM records
		WHERE record_type = ?`, recordType).Scan(&max); err != nil {
		return nil, err
	}
	fields["number"] = json.RawMessage(fmt.Sprintf("%d", max.Int64+1))
	return json.Marshal(fields)
}

// stampStoredBlob fills storage_key on a new artifact when its blob is
// already in object storage.
func stampStoredBlob(ctx context.Context, tx *sql.Tx, fields map[string]json.RawMessage) (json.RawMessage, error) {
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
	err := tx.QueryRowContext(ctx, `SELECT storage_key FROM blobs WHERE sha256 = ? AND stored`, sha).Scan(&key)
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

func applyUpdate(ctx context.Context, tx *sql.Tx, m api.Mutation) outcome {
	if appendOnlyTypes[m.RecordType] {
		return outcome{status: statusRejected,
			err: fmt.Sprintf("%s records are immutable; create a superseding record", m.RecordType)}
	}
	var currentStr string
	var currentRev int64
	err := tx.QueryRowContext(ctx, `SELECT data, server_revision FROM records
		WHERE record_type = ? AND record_id = ? AND deleted_at IS NULL`,
		m.RecordType, m.RecordID).Scan(&currentStr, &currentRev)
	current := json.RawMessage(currentStr)
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
		fieldRevs, err := loadFieldRevisions(ctx, tx, m.RecordType, m.RecordID)
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

	// The client sends only the fields it changed (field-level payloads keep
	// merges precise), so an update never carries updated_at. Left alone the
	// stored document keeps the timestamp it was created with — and because
	// the document is what clients pull and store, the next pull overwrites
	// the client's own correct value with the frozen one. Observed in
	// production: a task closed on 07-26 read as last updated on 07-25 in
	// every client.
	//
	// The mutation's creation time is the authoritative answer to "when was
	// this change made" — it may have been made offline days before the push,
	// so records.Now() would be wrong. Stamped after the merge checks above so
	// an update whose every field lost to the cloud does not bump the clock,
	// and only for record types that actually carry the field.
	if _, sent := incoming["updated_at"]; !sent && m.CreatedAt != "" {
		var doc map[string]json.RawMessage
		if json.Unmarshal(current, &doc) == nil {
			if _, carries := doc["updated_at"]; carries {
				if stamp, err := json.Marshal(m.CreatedAt); err == nil {
					incoming["updated_at"] = stamp
				}
			}
		}
	}

	merged, err := mergeJSON(current, incoming)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	rev, err := nextRevision(ctx, tx)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records SET data = ?, server_revision = ?, updated_at = ?
		WHERE record_type = ? AND record_id = ?`,
		string(merged), rev, records.Now(), m.RecordType, m.RecordID); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if err := setFieldRevisions(ctx, tx, m.RecordType, m.RecordID, keys(incoming), rev); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	return outcome{status: statusApplied, revision: rev}
}

func applyDelete(ctx context.Context, tx *sql.Tx, m api.Mutation) outcome {
	// Deleting something already gone (or never present) is idempotent, but it
	// must not mint a revision: every client would then pull an empty change
	// set, and a repeated repair run would inflate the revision counter for
	// nothing.
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE record_type = ? AND record_id = ? AND deleted_at IS NULL`,
		m.RecordType, m.RecordID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		var current int64
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).Scan(&current); err != nil {
			return outcome{status: statusRejected, err: err.Error()}
		}
		return outcome{status: statusApplied, revision: current}
	}
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}

	rev, err := nextRevision(ctx, tx)
	if err != nil {
		return outcome{status: statusRejected, err: err.Error()}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE records SET deleted_at = ?, server_revision = ?
		WHERE record_type = ? AND record_id = ? AND deleted_at IS NULL`,
		records.Now(), rev, m.RecordType, m.RecordID); err != nil {
		return outcome{status: statusRejected, err: err.Error()}
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

func loadFieldRevisions(ctx context.Context, tx *sql.Tx, recordType, recordID string) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT field, revision FROM field_revisions
		WHERE record_type = ? AND record_id = ?`, recordType, recordID)
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

func setFieldRevisions(ctx context.Context, tx *sql.Tx, recordType, recordID string, fields []string, rev int64) error {
	for _, f := range fields {
		if _, err := tx.ExecContext(ctx, `INSERT INTO field_revisions
			(record_type, record_id, field, revision) VALUES (?, ?, ?, ?)
			ON CONFLICT (record_type, record_id, field)
			DO UPDATE SET revision = excluded.revision`,
			recordType, recordID, f, rev); err != nil {
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

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/elk-work/ark/internal/db"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// PendingMutationRows returns the pending mutation queue in the order the
// mutations were written, which is the order the server must replay them in.
//
// The ordering key is the ULID, not created_at. Both are produced by the same
// logMutation call, but only the ULID orders correctly in SQLite: created_at
// is RFC3339Nano text, that format trims trailing zeros from the fractional
// second, and SQLite compares TEXT byte by byte — so ".1724Z" sorts *after*
// ".172492Z" (see records.TimeCompare). The ULID carries no such trap.
//
// It is also strictly stronger here. records.NewID() uses monotonic entropy,
// so ids increase on every call within a process even inside one millisecond;
// that is what keeps several mutations logged in a single transaction — a
// promotion superseding its predecessor, say — in the order they were made.
// created_at cannot do that: two calls can land on the same clock tick and
// tie, and a tie in a replay queue is a coin flip.
func (s *Store) PendingMutationRows(ctx context.Context) ([]api.Mutation, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, record_type, record_id, operation,
		base_server_revision, payload_json, created_at, created_by
		FROM mutations WHERE repository_id = ? AND status = 'pending' ORDER BY id`,
		s.RepoID)
	if err != nil {
		return nil, records.DBErr("load pending mutations", err)
	}
	defer rows.Close()
	var out []api.Mutation
	for rows.Next() {
		var m api.Mutation
		var payload string
		if err := rows.Scan(&m.ID, &m.RecordType, &m.RecordID, &m.Operation,
			&m.BaseServerRevision, &payload, &m.CreatedAt, &m.CreatedBy); err != nil {
			return nil, records.DBErr("scan mutation", err)
		}
		m.Payload = json.RawMessage(payload)
		out = append(out, m)
	}
	return out, rows.Err()
}

// MarkMutation records the server's verdict on a mutation.
//
// Rejections go through RejectMutation instead: they are the one verdict that
// leaves the local database holding a change the server does not have, so
// they carry bookkeeping beyond the status word.
func (s *Store) MarkMutation(ctx context.Context, id, status, errMsg string) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE mutations SET status = ?, error_message = ? WHERE id = ?`,
		status, errMsg, id)
	if err != nil {
		return records.DBErr("mark mutation", err)
	}
	return nil
}

// SyncStateDiverged marks a local record the server refused a change to. It
// is neither `local` (the record may well have synced before) nor `synced`
// (the two copies demonstrably disagree), and both of the paths that restore
// agreement — SetRecordRevision after an accepted push, upsertServerRecord
// after a pull — already write `synced` unconditionally, so the mark clears
// itself exactly when the divergence ends.
const SyncStateDiverged = "diverged"

// RejectMutation records a rejection and the divergence it leaves behind.
//
// The mutation is kept rather than dropped, and the record it targeted is
// marked diverged, in one transaction. Both halves matter and for different
// readers: the mutation row is the forensic trace — what was attempted, when,
// and the server's reason — while the mark on the record is what makes the
// disagreement visible to anyone looking at the record itself rather than at
// the sync log.
//
// The local effect of the mutation is deliberately *not* rolled back. Two
// reasons, and the second is the stronger one. Mechanically, the payload is a
// delta of changed fields with no before-image anywhere in the schema
// (conflicts.base_json is written empty for the same reason), so there is
// nothing to roll back to without inventing a prior value. And in substance,
// the commonest rejection is `record not found` — the server is the side
// missing data — so reverting would destroy the only copy of a real decision
// in order to agree with a peer that has never heard of the record. Losing
// work quietly to reach agreement is the same defect as reporting agreement
// that does not exist. Ark keeps the change, says so, and lets a person
// decide which side is right (docs/v1-spec.md §9.1, §22 exit 7).
func (s *Store) RejectMutation(ctx context.Context, m api.Mutation, errMsg string) error {
	return db.InTx(ctx, s.DB, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE mutations SET status = 'rejected', error_message = ?,
			resolved_at = NULL WHERE id = ?`, errMsg, m.ID); err != nil {
			return records.DBErr("mark mutation rejected", err)
		}
		table, ok := tableForType(m.RecordType)
		if !ok {
			// A record type with no local table cannot be marked, but the
			// mutation row still carries the rejection, which is what
			// UnresolvedRejections counts. The alarm is not lost.
			return nil
		}
		if _, err := tx.Exec(fmt.Sprintf(
			`UPDATE %s SET sync_state = ? WHERE id = ?`, table), SyncStateDiverged, m.RecordID); err != nil {
			return records.DBErr("mark record diverged", err)
		}
		return nil
	})
}

// UnresolvedRejections counts mutations the server refused whose effect it
// still does not hold. This is the honest answer to "am I in sync" that
// pending_mutations alone cannot give: a rejected mutation leaves the queue,
// so counting the queue reports zero for a repository that has diverged —
// which is exactly how a client came to report `0 pending mutations` about a
// server that had refused three writes (elk-work/ark#46).
func (s *Store) UnresolvedRejections(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM mutations
		WHERE repository_id = ? AND status = 'rejected' AND resolved_at IS NULL`, s.RepoID).Scan(&n)
	if err != nil {
		return 0, records.DBErr("count rejected mutations", err)
	}
	return n, nil
}

// resolveRejections closes out any outstanding rejection against a record
// that has just reached agreement with the server. Called from both paths
// that establish agreement — an accepted push and an applied pull — because
// either one ends the divergence, whichever change actually caused it.
func resolveRejections(tx *sql.Tx, recordType, recordID string) error {
	if _, err := tx.Exec(`UPDATE mutations SET resolved_at = ?
		WHERE record_type = ? AND record_id = ? AND status = 'rejected' AND resolved_at IS NULL`,
		records.Now(), recordType, recordID); err != nil {
		return records.DBErr("resolve rejections", err)
	}
	return nil
}

// AllActors returns every local actor for inclusion in a push.
func (s *Store) AllActors(ctx context.Context) ([]api.Actor, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, type, name, email, agent_name,
		agent_version, delegated_by, created_at FROM actors`)
	if err != nil {
		return nil, records.DBErr("load actors", err)
	}
	defer rows.Close()
	var out []api.Actor
	for rows.Next() {
		var a api.Actor
		if err := rows.Scan(&a.ID, &a.Type, &a.Name, &a.Email, &a.AgentName,
			&a.AgentVersion, &a.DelegatedBy, &a.CreatedAt); err != nil {
			return nil, records.DBErr("scan actor", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SyncCursor returns the client ID and last seen server revision.
func (s *Store) SyncCursor(ctx context.Context) (clientID string, lastRevision int64, err error) {
	err = s.DB.QueryRowContext(ctx, `SELECT client_id, last_revision FROM sync_state
		WHERE repository_id = ?`, s.RepoID).Scan(&clientID, &lastRevision)
	if err != nil {
		return "", 0, records.DBErr("load sync state", err)
	}
	return clientID, lastRevision, nil
}

// HistoryReset reports that the service's revision for this repository was
// seen below a revision this checkout had already synced past — the service is
// not serving the history the client was tracking.
type HistoryReset struct {
	// DetectedAt is the first time this was observed, not the most recent.
	// The event is what matters; every sync after it sees the same thing.
	DetectedAt string `json:"detected_at"`
	// ServerRevision is where the service was when last seen behind, and
	// LocalRevision is how far this checkout had synced. The gap between
	// them is the number of revisions the service no longer accounts for.
	ServerRevision int64 `json:"server_revision"`
	LocalRevision  int64 `json:"local_revision"`
}

// RecordHistoryReset stores a detected reset. It is called once per sync that
// observes the condition, but only the first detection's timestamp is kept:
// the client keeps syncing afterwards, so a later run would otherwise walk the
// timestamp forward and lose the one fact worth knowing — when the history it
// was tracking stopped being served.
func (s *Store) RecordHistoryReset(ctx context.Context, localRevision, serverRevision int64) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sync_state SET
		history_reset_at = COALESCE(history_reset_at, ?),
		history_reset_server_revision = ?,
		history_reset_local_revision = ?
		WHERE repository_id = ?`,
		records.Now(), serverRevision, localRevision, s.RepoID)
	if err != nil {
		return records.DBErr("record history reset", err)
	}
	return nil
}

// HistoryReset returns the recorded reset for this repository, or nil.
func (s *Store) HistoryReset(ctx context.Context) (*HistoryReset, error) {
	var at sql.NullString
	var serverRev, localRev sql.NullInt64
	err := s.DB.QueryRowContext(ctx, `SELECT history_reset_at,
		history_reset_server_revision, history_reset_local_revision
		FROM sync_state WHERE repository_id = ?`, s.RepoID).Scan(&at, &serverRev, &localRev)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, records.DBErr("load history reset", err)
	}
	if !at.Valid {
		return nil, nil
	}
	return &HistoryReset{DetectedAt: at.String,
		ServerRevision: serverRev.Int64, LocalRevision: localRev.Int64}, nil
}

// SetRecordRevision stamps a record row after the server accepts a mutation,
// and closes out any earlier rejection against the same record: the server
// has just accepted a change to it, so whatever it disagreed about before is
// over. Both statements run in one transaction so a record can never read as
// synced while the rejection that contradicts it is still outstanding.
func (s *Store) SetRecordRevision(ctx context.Context, recordType, recordID string, revision int64) error {
	table, ok := tableForType(recordType)
	if !ok {
		return nil
	}
	return db.InTx(ctx, s.DB, func(tx *sql.Tx) error {
		if _, err := tx.Exec(fmt.Sprintf(
			`UPDATE %s SET server_revision = ?, sync_state = 'synced' WHERE id = ?`, table),
			revision, recordID); err != nil {
			return records.DBErr("stamp record revision", err)
		}
		return resolveRejections(tx, recordType, recordID)
	})
}

// RecordConflict stores an unresolved conflict for `ark conflict` commands.
func (s *Store) RecordConflict(ctx context.Context, m api.Mutation, remote json.RawMessage) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO conflicts
		(id, record_type, record_id, mutation_id, base_json, local_json, remote_json, status, created_at)
		VALUES (?, ?, ?, ?, '', ?, ?, 'unresolved', ?)`,
		records.NewID(), m.RecordType, m.RecordID, m.ID,
		string(m.Payload), string(remote), records.Now())
	if err != nil {
		return records.DBErr("record conflict", err)
	}
	return nil
}

// RequeueConflictLocal re-submits a conflict's local change as a fresh
// mutation based on the record's current server revision.
func (s *Store) RequeueConflictLocal(ctx context.Context, conflictID string) error {
	var recordType, recordID, localJSON string
	err := s.DB.QueryRowContext(ctx, `SELECT record_type, record_id, local_json
		FROM conflicts WHERE id LIKE ? AND status = 'unresolved'`, conflictID+"%").
		Scan(&recordType, &recordID, &localJSON)
	if err == sql.ErrNoRows {
		return records.NotFoundf("no unresolved conflict matching %q", conflictID)
	}
	if err != nil {
		return records.DBErr("load conflict", err)
	}
	table, ok := tableForType(recordType)
	if !ok {
		return records.Validationf("cannot requeue %s conflicts", recordType)
	}
	var rev int64
	if err := s.DB.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT server_revision FROM %s WHERE id = ?`, table), recordID).Scan(&rev); err != nil {
		return records.DBErr("read record revision", err)
	}
	return db.InTx(ctx, s.DB, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO mutations
			(id, repository_id, record_type, record_id, operation, base_server_revision,
			 payload_json, created_at, created_by, status)
			VALUES (?, ?, ?, ?, 'update', ?, ?, ?, ?, 'pending')`,
			records.NewID(), s.RepoID, recordType, recordID, rev, localJSON,
			records.Now(), s.Actor.ID)
		if err != nil {
			return records.DBErr("requeue mutation", err)
		}
		return nil
	})
}

func tableForType(recordType string) (string, bool) {
	switch records.RecordType(recordType) {
	case records.TypeTask:
		return "tasks", true
	case records.TypeComment:
		return "comments", true
	case records.TypeThread:
		return "agent_threads", true
	case records.TypeMessage:
		return "thread_messages", true
	case records.TypeRun:
		return "agent_runs", true
	case records.TypePullRequest:
		return "pull_requests", true
	case records.TypeReview:
		return "reviews", true
	case records.TypeArtifact:
		return "artifacts", true
	case records.TypePromotion:
		return "promotions", true
	}
	return "", false
}

// pulledTable maps a record type a pull can carry to the table it lands in.
// It is tableForType plus actors: actors travel in a pull like records but
// are not records — no mutation log, no tombstone, no sync_state — so
// tableForType, which serves those paths, does not know them, while the
// referential-integrity check below has to.
//
// A pulled table missing from here is a table whose references go unchecked,
// which is the exact silence that check exists to end;
// TestEveryForeignKeyOnAPulledTableIsChecked fails when one appears.
func pulledTable(recordType string) (string, bool) {
	if recordType == "actor" {
		return "actors", true
	}
	return tableForType(recordType)
}

// foreignKey is one reference a table declares: the column carrying the id,
// the table it points into, and the column there.
type foreignKey struct {
	Column string
	Table  string
	Target string
}

// refCheck carries what one pull has already established about references:
// the foreign keys each table declares, and the referents already found.
//
// Only presence is remembered, never absence. A pull inserts records as it
// goes, so a referent missing a moment ago can be present now — that is
// exactly how a parent later in the batch rescues its child — while one
// already found cannot go missing, because nothing in a pull deletes a row.
type refCheck struct {
	keys  map[string][]foreignKey
	found map[string]bool
}

func newRefCheck() *refCheck {
	return &refCheck{keys: map[string][]foreignKey{}, found: map[string]bool{}}
}

// foreignKeysOf reads a table's declared references out of the schema itself,
// memoized for the length of one pull.
//
// Reading them rather than listing them in Go is the whole point. A written
// list of "the references a review carries" is a second copy of the schema,
// and the failure mode of a second copy is that a migration adds a reference
// to the first and nobody adds it to the second — at which point that record
// goes unchecked, gets written, and fails the commit exactly as it did before
// elk-work/ark#75, with the check that was supposed to prevent it looking
// like it had passed. There is nothing here to keep in step: the check is
// derived from the constraint it is protecting, so a migration that adds a
// foreign key is enforced on the pull path the moment it ships.
func foreignKeysOf(tx *sql.Tx, table string, check *refCheck) ([]foreignKey, error) {
	if fks, ok := check.keys[table]; ok {
		return fks, nil
	}
	// The table name comes from pulledTable, never from the server.
	rows, err := tx.Query(fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, table))
	if err != nil {
		return nil, records.DBErr("read foreign keys", err)
	}
	defer rows.Close()
	fks := []foreignKey{}
	for rows.Next() {
		var id, seq int
		var refTable, from string
		var to sql.NullString
		var onUpdate, onDelete, match string
		if err := rows.Scan(&id, &seq, &refTable, &from, &to, &onUpdate, &onDelete, &match); err != nil {
			return nil, records.DBErr("scan foreign key", err)
		}
		target := to.String
		if target == "" {
			target = "id" // an omitted target means the referenced primary key
		}
		fks = append(fks, foreignKey{Column: from, Table: refTable, Target: target})
	}
	if err := rows.Err(); err != nil {
		return nil, records.DBErr("read foreign keys", err)
	}
	check.keys[table] = fks
	return fks, nil
}

// refMiss names a reference a pulled record carries that this client cannot
// resolve: the column that referred, the table it pointed into, and the id.
type refMiss struct {
	Field string
	Table string
	ID    string
}

// missingReference reports the first reference in a pulled record that this
// client cannot resolve, or nil when every one of them is present.
//
// The lookups run inside the pull transaction, so a record inserted earlier
// in the same batch already counts as present — which is what lets a parent
// and its child arrive together and both apply.
//
// A pulled document names its references by column name (`pull_request_id`,
// `task_id`, …), which is what makes one check serve every record type;
// TestPulledDocumentsNameTheirReferencesByColumn holds the structs to it.
func missingReference(tx *sql.Tx, table string, data json.RawMessage, check *refCheck) (*refMiss, error) {
	fks, err := foreignKeysOf(tx, table, check)
	if err != nil {
		return nil, err
	}
	if len(fks) == 0 {
		return nil, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		// A payload that will not decode is upsertServerRecord's error to
		// report, in its own words. Saying nothing here leaves that intact.
		return nil, nil
	}
	for _, fk := range fks {
		id, _ := doc[fk.Column].(string)
		if id == "" {
			continue // a null reference is not a reference
		}
		key := fk.Table + "\x00" + id
		if check.found[key] {
			continue
		}
		var one int
		err := tx.QueryRow(fmt.Sprintf(
			`SELECT 1 FROM %s WHERE %s = ?`, fk.Table, fk.Target), id).Scan(&one)
		if err == sql.ErrNoRows {
			return &refMiss{Field: fk.Column, Table: fk.Table, ID: id}, nil
		}
		if err != nil {
			return nil, records.DBErr("check reference", err)
		}
		check.found[key] = true
	}
	return nil, nil
}

// PullSkips counts pulled records this client could not represent, by record
// type. A non-empty map means the server holds record types this build does
// not know — normal during a version skew, and worth showing the operator
// rather than dropping in silence.
//
// It deliberately does not count records held back for a referent that has
// not arrived. The two look alike and mean opposite things: an unknown type
// is a client older than its service and the answer is to upgrade, while a
// missing referent is a record still in flight and the answer is to wait —
// and the line `ark sync` prints for this map says "this build does not know
// that type", which would be a false diagnosis for the second. A held record
// is also the durable kind of fact, like a rejection rather than like a
// version skew, so it is kept in a table and read back with DeferredRecords.
type PullSkips map[string]int

// ApplyPull applies a pull response — records, tombstones, and the new
// cursor — in one transaction (docs/v1-spec.md §9.2).
//
// Two kinds of record are set aside rather than applied, for one reason: a
// client that fails a whole pull over a single record it cannot write stops
// syncing altogether, because the cursor does not advance and the next pull
// fetches the same batch.
//
//   - Records of an unknown type are skipped, not rejected: a client must
//     tolerate a server that knows more record types than it does, or a
//     single unrecognized record would wedge every future pull. The skips
//     are returned so the caller can report them.
//   - Records naming a referent this client does not hold are held back.
//     Every typed pointer between records is a declared foreign key, and
//     `PRAGMA defer_foreign_keys` moves that check to COMMIT rather than
//     removing it, so applying one used to fail the entire transaction —
//     discarding every good record in the batch and leaving the cursor
//     where it was, permanently, since the offending record lives on the
//     service and nothing local can remove it (elk-work/ark#75).
//
// A held record is retried within this same batch, to a fixpoint, and stored
// and retried on every later pull. Both halves are load-bearing: revision
// order is not dependency order, so a parent can arrive after its own child
// in one response; and the cursor moves past the held record's revision, so
// the service will never send it again and a client that kept no copy would
// trade a wedge for the quiet loss of a record both sides hold.
func (s *Store) ApplyPull(ctx context.Context, resp *api.PullResponse) (PullSkips, error) {
	skips := PullSkips{}
	err := db.InTx(ctx, s.DB, func(tx *sql.Tx) error {
		// The closure reruns on retry; reset so counts cannot double.
		clear(skips)
		// Revision order is not dependency order (an updated PR can outrank
		// its reviews), so FK checks wait for the end of the transaction.
		// They are the backstop now rather than the only line: an unresolvable
		// reference is caught before its insert, where it can still be
		// attributed to the one record that carries it.
		if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
			return records.DBErr("defer foreign keys", err)
		}
		// Records held by earlier pulls ride along with this batch: the
		// referent one of them waits for may be arriving in it, and no later
		// response will carry the held record itself.
		carried, carriedKeys, err := s.deferredForRetry(tx)
		if err != nil {
			return err
		}
		pending := make([]api.Record, 0, len(resp.Records)+len(carried))
		pending = append(pending, resp.Records...)
		inBatch := make(map[string]bool, len(resp.Records))
		for _, rec := range resp.Records {
			inBatch[recordKey(rec.RecordType, rec.RecordID)] = true
		}
		for _, rec := range carried {
			// The service re-sending a held record supersedes our copy of it.
			if !inBatch[recordKey(rec.RecordType, rec.RecordID)] {
				pending = append(pending, rec)
			}
		}

		check := newRefCheck()
		misses := map[string]refMiss{}
		// Each pass applies what it can and holds the rest, so a pass that
		// applies a parent lets the next one apply its child. It ends when a
		// pass applies nothing, which bounds it by the depth of the deepest
		// reference chain in the batch (task → pull request → review).
		for {
			progress := false
			var held []api.Record
			for _, rec := range pending {
				key := recordKey(rec.RecordType, rec.RecordID)
				if table, known := pulledTable(rec.RecordType); known {
					miss, err := missingReference(tx, table, rec.Data, check)
					if err != nil {
						return err
					}
					if miss != nil {
						misses[key] = *miss
						held = append(held, rec)
						continue
					}
				}
				handled, err := upsertServerRecord(tx, rec)
				if err != nil {
					return err
				}
				if !handled {
					skips[rec.RecordType]++
					continue
				}
				progress = true
				if carriedKeys[key] {
					if err := clearDeferred(tx, rec.RecordType, rec.RecordID); err != nil {
						return err
					}
				}
				// The server's copy of this record has just landed locally, so
				// the two agree again and any rejection standing against it is
				// spent. Note that this is how a divergence ends in the direction
				// the server wins: the local change stays rejected history, and
				// the record now reads as the server has it.
				if err := resolveRejections(tx, rec.RecordType, rec.RecordID); err != nil {
					return err
				}
			}
			pending = held
			if !progress || len(pending) == 0 {
				break
			}
		}
		for _, rec := range pending {
			if err := s.holdDeferred(tx, rec, misses[recordKey(rec.RecordType, rec.RecordID)]); err != nil {
				return err
			}
		}
		for _, tomb := range resp.Tombstones {
			// A held record the server says is deleted is one nothing is
			// waiting for any more, whether or not this client ever wrote it.
			if err := clearDeferred(tx, tomb.RecordType, tomb.RecordID); err != nil {
				return err
			}
			table, ok := tableForType(tomb.RecordType)
			if !ok {
				continue
			}
			if _, err := tx.Exec(fmt.Sprintf(
				`UPDATE %s SET deleted_at = ?, server_revision = ?, sync_state = 'synced' WHERE id = ?`, table),
				tomb.DeletedAt, tomb.ServerRevision, tomb.RecordID); err != nil {
				return records.DBErr("apply tombstone", err)
			}
			// A record the server says is deleted is one the two sides now
			// agree about, however loudly they disagreed before.
			if err := resolveRejections(tx, tomb.RecordType, tomb.RecordID); err != nil {
				return err
			}
		}
		// MAX, not assignment: the cursor is a high-water mark and a server
		// revision counter only ever increases, so a response carrying a
		// lower one cannot be a reason to rewind. Assigning it is what erased
		// the evidence in elk-work/ark#58 — a checkout that had synced to
		// revision 18 quietly adopted a service sitting at 4, and after that
		// nothing on either side remembered there had ever been an 18.
		// Pull records the event before this runs; this makes sure the
		// number it recorded is still here to compare against tomorrow.
		if _, err := tx.Exec(`UPDATE sync_state SET last_revision = MAX(last_revision, ?),
			last_synced_at = ? WHERE repository_id = ?`,
			resp.ServerRevision, records.Now(), s.RepoID); err != nil {
			return records.DBErr("advance sync cursor", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(skips) == 0 {
		return nil, nil
	}
	return skips, nil
}

// DeferredRecord is one pulled record this client is holding rather than
// applying, because a record it points at has not arrived.
type DeferredRecord struct {
	RecordType     string `json:"record_type"`
	RecordID       string `json:"record_id"`
	ServerRevision int64  `json:"server_revision"`
	// Field, MissingTable and MissingID are the reference that did not
	// resolve, so a reader is told which record is missing rather than only
	// that one is.
	Field        string `json:"field"`
	MissingTable string `json:"missing_table"`
	MissingID    string `json:"missing_id"`
	FirstSeenAt  string `json:"first_seen_at"`
	LastSeenAt   string `json:"last_seen_at"`
}

// DeferredRecords returns the records this client is holding until the
// records they name arrive, oldest revision first.
//
// It is the pull-side counterpart of UnresolvedRejections, and durable for
// the same reason: a count reported once at the end of a sync is a fact the
// next command cannot see, and this is exactly the fact a client needs in
// order not to describe itself as holding everything the service holds.
//
// Unlike a rejection it needs no resolved_at. A held record and its referent
// are one lookup apart, so the set is a comparison the client can always make
// afresh: the row is deleted the moment the record applies, which happens on
// the first pull after its referent lands.
func (s *Store) DeferredRecords(ctx context.Context) ([]DeferredRecord, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT record_type, record_id, server_revision,
		field, missing_table, missing_id, first_seen_at, last_seen_at
		FROM deferred_records WHERE repository_id = ? ORDER BY server_revision`, s.RepoID)
	if err != nil {
		return nil, records.DBErr("load deferred records", err)
	}
	defer rows.Close()
	var out []DeferredRecord
	for rows.Next() {
		var d DeferredRecord
		if err := rows.Scan(&d.RecordType, &d.RecordID, &d.ServerRevision, &d.Field,
			&d.MissingTable, &d.MissingID, &d.FirstSeenAt, &d.LastSeenAt); err != nil {
			return nil, records.DBErr("scan deferred record", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeferredRecordCount counts records held for a referent that has not
// arrived, for callers that only need to know whether there are any.
func (s *Store) DeferredRecordCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM deferred_records
		WHERE repository_id = ?`, s.RepoID).Scan(&n)
	if err != nil {
		return 0, records.DBErr("count deferred records", err)
	}
	return n, nil
}

// recordKey identifies a record across a pull. Record ids are ULIDs and
// unique on their own, but the pair is what the ledger is keyed by.
func recordKey(recordType, recordID string) string {
	return recordType + "\x00" + recordID
}

// deferredForRetry loads the held records so this pull can try them again,
// in the order the service minted them.
func (s *Store) deferredForRetry(tx *sql.Tx) ([]api.Record, map[string]bool, error) {
	rows, err := tx.Query(`SELECT record_type, record_id, data_json, server_revision
		FROM deferred_records WHERE repository_id = ? ORDER BY server_revision`, s.RepoID)
	if err != nil {
		return nil, nil, records.DBErr("load deferred records", err)
	}
	defer rows.Close()
	var out []api.Record
	keys := map[string]bool{}
	for rows.Next() {
		var rec api.Record
		var data string
		if err := rows.Scan(&rec.RecordType, &rec.RecordID, &data, &rec.ServerRevision); err != nil {
			return nil, nil, records.DBErr("scan deferred record", err)
		}
		rec.Data = json.RawMessage(data)
		out = append(out, rec)
		keys[recordKey(rec.RecordType, rec.RecordID)] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, records.DBErr("load deferred records", err)
	}
	return out, keys, nil
}

// holdDeferred stores a record whose referent has not arrived, with the
// reference that did not resolve.
//
// first_seen_at is not overwritten on a repeat, the way history_reset_at is
// not: how long a record has been waiting is the fact worth having, and a
// timestamp walked forward by every pull would only ever say "recently".
func (s *Store) holdDeferred(tx *sql.Tx, rec api.Record, miss refMiss) error {
	now := records.Now()
	_, err := tx.Exec(`INSERT INTO deferred_records
		(record_type, record_id, repository_id, data_json, server_revision,
		 field, missing_table, missing_id, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (record_type, record_id) DO UPDATE SET
			data_json = excluded.data_json, server_revision = excluded.server_revision,
			field = excluded.field, missing_table = excluded.missing_table,
			missing_id = excluded.missing_id, last_seen_at = excluded.last_seen_at`,
		rec.RecordType, rec.RecordID, s.RepoID, string(rec.Data), rec.ServerRevision,
		miss.Field, miss.Table, miss.ID, now, now)
	if err != nil {
		return records.DBErr("hold deferred record", err)
	}
	return nil
}

// clearDeferred forgets a held record, because it has applied or because the
// server says it is deleted.
func clearDeferred(tx *sql.Tx, recordType, recordID string) error {
	if _, err := tx.Exec(`DELETE FROM deferred_records
		WHERE record_type = ? AND record_id = ?`, recordType, recordID); err != nil {
		return records.DBErr("clear deferred record", err)
	}
	return nil
}

// ApplyServerRecord upserts one authoritative record (e.g. the response of
// a cloud-confirmed merge) without touching the pull cursor.
func (s *Store) ApplyServerRecord(ctx context.Context, rec api.Record) error {
	return db.InTx(ctx, s.DB, func(tx *sql.Tx) error {
		handled, err := upsertServerRecord(tx, rec)
		if err != nil || !handled {
			return err
		}
		return resolveRejections(tx, rec.RecordType, rec.RecordID)
	})
}

// upsertServerRecord writes one authoritative server record into the local
// tables. The server's Data is the JSON encoding of the client structs, so
// each type round-trips through its struct for validation.
// upsertServerRecord applies one pulled record. It reports whether the record
// type is one this client knows; an unknown type is not an error (see
// ApplyPull), but the caller needs to know it happened.
func upsertServerRecord(tx *sql.Tx, rec api.Record) (handled bool, err error) {
	unmarshal := func(v any) error {
		if err := json.Unmarshal(rec.Data, v); err != nil {
			return records.DBErr(fmt.Sprintf("decode %s %s", rec.RecordType, rec.RecordID), err)
		}
		return nil
	}
	switch records.RecordType(rec.RecordType) {
	case records.TypeTask:
		var t Task
		if err := unmarshal(&t); err != nil {
			return false, err
		}
		_, err := tx.Exec(`INSERT INTO tasks
			(id, repository_id, number, title, body, status, created_at, created_by,
			 created_by_type, updated_at, version, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET number = excluded.number, title = excluded.title,
			 body = excluded.body, status = excluded.status, updated_at = excluded.updated_at,
			 version = excluded.version, sync_state = 'synced', server_revision = excluded.server_revision`,
			t.ID, t.RepositoryID, t.Number, t.Title, t.Body, t.Status, t.CreatedAt,
			t.CreatedBy, t.CreatedByType, t.UpdatedAt, t.Version, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert task", err)
		}
		if _, err := tx.Exec(`DELETE FROM fts WHERE record_type = 'task' AND record_id = ?`, t.ID); err == nil {
			tx.Exec(`INSERT INTO fts (record_type, record_id, title, body) VALUES ('task', ?, ?, ?)`,
				t.ID, t.Title, t.Body)
		}
		return true, nil
	case records.TypeComment:
		var c Comment
		if err := unmarshal(&c); err != nil {
			return false, err
		}
		var super any
		if c.SupersedesID != "" {
			super = c.SupersedesID
		}
		_, err := tx.Exec(`INSERT INTO comments
			(id, repository_id, parent_type, parent_id, body, created_at, created_by,
			 created_by_type, supersedes_id, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET sync_state = 'synced', server_revision = excluded.server_revision`,
			c.ID, c.RepositoryID, c.ParentType, c.ParentID, c.Body, c.CreatedAt,
			c.CreatedBy, c.CreatedByType, super, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert comment", err)
		}
		tx.Exec(`DELETE FROM fts WHERE record_type = 'comment' AND record_id = ?`, c.ID)
		tx.Exec(`INSERT INTO fts (record_type, record_id, title, body) VALUES ('comment', ?, '', ?)`,
			c.ID, c.Body)
		return true, nil
	case records.TypeThread:
		var t Thread
		if err := unmarshal(&t); err != nil {
			return false, err
		}
		_, err := tx.Exec(`INSERT INTO agent_threads
			(id, repository_id, task_id, title, status, created_at, created_by,
			 created_by_type, closed_at, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET title = excluded.title, status = excluded.status,
			 closed_at = excluded.closed_at, sync_state = 'synced', server_revision = excluded.server_revision`,
			t.ID, t.RepositoryID, nullable(t.TaskID), t.Title, t.Status, t.CreatedAt,
			t.CreatedBy, t.CreatedByType, nullable(t.ClosedAt), rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert thread", err)
		}
		return true, nil
	case records.TypeMessage:
		var m Message
		if err := unmarshal(&m); err != nil {
			return false, err
		}
		var super any
		if m.SupersedesID != "" {
			super = m.SupersedesID
		}
		_, err := tx.Exec(`INSERT INTO thread_messages
			(id, thread_id, role, body, metadata_json, created_at, created_by,
			 created_by_type, supersedes_id, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET sync_state = 'synced', server_revision = excluded.server_revision`,
			m.ID, m.ThreadID, m.Role, m.Body, m.MetadataJSON, m.CreatedAt,
			m.CreatedBy, m.CreatedByType, super, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert message", err)
		}
		tx.Exec(`DELETE FROM fts WHERE record_type = 'thread_message' AND record_id = ?`, m.ID)
		tx.Exec(`INSERT INTO fts (record_type, record_id, title, body) VALUES ('thread_message', ?, '', ?)`,
			m.ID, m.Body)
		return true, nil
	case records.TypeRun:
		var r Run
		if err := unmarshal(&r); err != nil {
			return false, err
		}
		var exit any
		if r.ExitCode != nil {
			exit = *r.ExitCode
		}
		_, err := tx.Exec(`INSERT INTO agent_runs
			(id, repository_id, task_id, thread_id, agent_name, agent_version, status,
			 input_summary, result_summary, started_at, finished_at, exit_code, branch_name,
			 base_commit_sha, result_commit_sha, metadata_json, created_at, created_by,
			 created_by_type, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET status = excluded.status,
			 result_summary = excluded.result_summary, finished_at = excluded.finished_at,
			 exit_code = excluded.exit_code, result_commit_sha = excluded.result_commit_sha,
			 sync_state = 'synced', server_revision = excluded.server_revision`,
			r.ID, r.RepositoryID, nullable(r.TaskID), nullable(r.ThreadID), r.AgentName,
			r.AgentVersion, r.Status, r.InputSummary, r.ResultSummary, nullable(r.StartedAt),
			nullable(r.FinishedAt), exit, r.BranchName, r.BaseCommitSHA, r.ResultCommitSHA,
			r.MetadataJSON, r.CreatedAt, r.CreatedBy, r.CreatedByType, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert run", err)
		}
		return true, nil
	case records.TypePullRequest:
		var pr PullRequest
		if err := unmarshal(&pr); err != nil {
			return false, err
		}
		_, err := tx.Exec(`INSERT INTO pull_requests
			(id, repository_id, number, task_id, title, body, status, base_branch, head_branch,
			 base_commit_sha, head_commit_sha, merge_commit_sha, created_at, created_by,
			 created_by_type, merged_at, closed_at, updated_at, version, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET number = excluded.number, title = excluded.title,
			 body = excluded.body, status = excluded.status,
			 head_commit_sha = excluded.head_commit_sha, merge_commit_sha = excluded.merge_commit_sha,
			 merged_at = excluded.merged_at, closed_at = excluded.closed_at,
			 updated_at = excluded.updated_at, version = excluded.version,
			 sync_state = 'synced', server_revision = excluded.server_revision`,
			pr.ID, pr.RepositoryID, pr.Number, nullable(pr.TaskID), pr.Title, pr.Body, pr.Status,
			pr.BaseBranch, pr.HeadBranch, pr.BaseCommitSHA, pr.HeadCommitSHA, pr.MergeCommitSHA,
			pr.CreatedAt, pr.CreatedBy, pr.CreatedByType, nullable(pr.MergedAt),
			nullable(pr.ClosedAt), pr.UpdatedAt, pr.Version, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert pull request", err)
		}
		tx.Exec(`DELETE FROM fts WHERE record_type = 'pull_request' AND record_id = ?`, pr.ID)
		tx.Exec(`INSERT INTO fts (record_type, record_id, title, body) VALUES ('pull_request', ?, ?, ?)`,
			pr.ID, pr.Title, pr.Body)
		return true, nil
	case records.TypeReview:
		var r Review
		if err := unmarshal(&r); err != nil {
			return false, err
		}
		_, err := tx.Exec(`INSERT INTO reviews
			(id, repository_id, pull_request_id, state, body, commit_sha, created_at,
			 created_by, created_by_type, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET sync_state = 'synced', server_revision = excluded.server_revision`,
			r.ID, r.RepositoryID, r.PullRequestID, r.State, r.Body, r.CommitSHA,
			r.CreatedAt, r.CreatedBy, r.CreatedByType, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert review", err)
		}
		tx.Exec(`DELETE FROM fts WHERE record_type = 'review' AND record_id = ?`, r.ID)
		tx.Exec(`INSERT INTO fts (record_type, record_id, title, body) VALUES ('review', ?, '', ?)`,
			r.ID, r.Body)
		return true, nil
	case records.TypeArtifact:
		var a Artifact
		if err := unmarshal(&a); err != nil {
			return false, err
		}
		// local_path stays local: another client's path is meaningless here.
		_, err := tx.Exec(`INSERT INTO artifacts
			(id, repository_id, parent_type, parent_id, name, media_type, size_bytes, sha256,
			 local_path, storage_key, created_at, created_by, created_by_type, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET storage_key = excluded.storage_key,
			 sync_state = 'synced', server_revision = excluded.server_revision`,
			a.ID, a.RepositoryID, a.ParentType, a.ParentID, a.Name, a.MediaType, a.SizeBytes,
			a.SHA256, a.StorageKey, a.CreatedAt, a.CreatedBy, a.CreatedByType, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert artifact", err)
		}
		return true, nil
	case records.TypePromotion:
		var p Promotion
		if err := unmarshal(&p); err != nil {
			return false, err
		}
		_, err := tx.Exec(`INSERT INTO promotions
			(id, repository_id, environment, service, merge_commit_sha, artifact_sha256,
			 pull_request_id, activated_at, ended_at, metadata_json, created_at, created_by,
			 created_by_type, sync_state, server_revision)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'synced', ?)
			ON CONFLICT (id) DO UPDATE SET ended_at = excluded.ended_at,
			 metadata_json = excluded.metadata_json, sync_state = 'synced',
			 server_revision = excluded.server_revision`,
			p.ID, p.RepositoryID, p.Environment, p.Service, p.MergeCommitSHA,
			p.ArtifactSHA256, nullable(p.PullRequestID), p.ActivatedAt, nullable(p.EndedAt),
			p.MetadataJSON, p.CreatedAt, p.CreatedBy, p.CreatedByType, rec.ServerRevision)
		if err != nil {
			return false, records.DBErr("upsert promotion", err)
		}
		return true, nil
	case "actor":
		var a api.Actor
		if err := unmarshal(&a); err != nil {
			return false, err
		}
		createdAt := a.CreatedAt
		if createdAt == "" {
			createdAt = records.Now()
		}
		_, err := tx.Exec(`INSERT INTO actors
			(id, type, name, email, agent_name, agent_version, delegated_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, email = excluded.email`,
			a.ID, a.Type, a.Name, a.Email, a.AgentName, a.AgentVersion, a.DelegatedBy, createdAt)
		if err != nil {
			return false, records.DBErr("upsert actor", err)
		}
		return true, nil
	}
	// An unknown record type is not an error. Server and client versions
	// skew by design, so a client must tolerate records it cannot represent
	// rather than refusing the whole pull. The caller surfaces the skip so it
	// is visible rather than silent.
	return false, nil
}

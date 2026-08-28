// Package store implements Ark's record operations over SQLite.
//
// Every mutating operation runs inside one transaction that writes the
// record change, the mutation describing the intent, and any search-index
// update together. See docs/v1-spec.md §8 and §18.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/elk-work/ark/internal/db"
	"github.com/elk-work/ark/internal/records"
)

// Actor is the identity performing the current command.
type Actor struct {
	ID           string            `json:"id"`
	Type         records.ActorType `json:"type"`
	Name         string            `json:"name"`
	Email        string            `json:"email,omitempty"`
	AgentName    string            `json:"agent_name,omitempty"`
	AgentVersion string            `json:"agent_version,omitempty"`
	DelegatedBy  string            `json:"delegated_by,omitempty"`
}

// Store performs record operations for one repository as one actor.
type Store struct {
	DB            *sql.DB
	RepoID        string
	Actor         Actor
	AfterMutation func()
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := db.InTx(ctx, s.DB, fn); err != nil {
		return err
	}
	// Delivery hooks run only after the record and its mutation are durable.
	// They deliberately cannot return an error: an optional observer must
	// never turn a successful local filing into a failed command.
	if s.AfterMutation != nil {
		s.AfterMutation()
	}
	return nil
}

// logMutation appends the intent behind a record change to the mutation log,
// inside the same transaction as the change itself.
func (s *Store) logMutation(tx *sql.Tx, rt records.RecordType, recordID, operation string, baseRevision int64, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return records.DBErr("encode mutation payload", err)
	}
	_, err = tx.Exec(`INSERT INTO mutations
		(id, repository_id, record_type, record_id, operation, base_server_revision,
		 payload_json, created_at, created_by, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		records.NewID(), s.RepoID, string(rt), recordID, operation, baseRevision,
		string(body), records.Now(), s.Actor.ID)
	if err != nil {
		return records.DBErr("write mutation", err)
	}
	return nil
}

// logUpdate logs an update mutation against the revision the record is
// currently at on the server.
//
// Every update path must go through this rather than passing a base revision
// of its own. `base_server_revision` is how the client says "this change was
// made against that version of the record", and the server's field-level
// merge (docs/v1-spec.md §10.4) drops — cloud-wins — every field whose
// server-side revision is newer than the base. A literal 0 therefore claims
// the change was made against a record the server had never written, so
// *every* field the record carried at creation is treated as a concurrent
// remote edit and silently discarded, while the mutation is still reported
// applied. For a run that meant `status` never left "running" once the run
// had been pushed by an earlier sync, though `result_summary` and
// `finished_at` — absent from the create payload, so carrying no field
// revision — merged cleanly (elk-work/ark#28).
//
// A record that has never synced sits at revision 0, which is the honest
// base for it, so this is also correct offline.
func (s *Store) logUpdate(tx *sql.Tx, rt records.RecordType, recordID string, payload any) error {
	table, ok := tableForType(string(rt))
	if !ok {
		return records.Validationf("no local table for record type %q", rt)
	}
	var rev int64
	if err := tx.QueryRow(fmt.Sprintf(
		`SELECT server_revision FROM %s WHERE id = ?`, table), recordID).Scan(&rev); err != nil {
		return records.DBErr("read revision", err)
	}
	return s.logMutation(tx, rt, recordID, "update", rev, payload)
}

// ftsSet replaces the search-index entry for a record.
func (s *Store) ftsSet(tx *sql.Tx, rt records.RecordType, id, title, body string) error {
	if _, err := tx.Exec(`DELETE FROM fts WHERE record_type = ? AND record_id = ?`, string(rt), id); err != nil {
		return records.DBErr("update search index", err)
	}
	if _, err := tx.Exec(`INSERT INTO fts (record_type, record_id, title, body) VALUES (?, ?, ?, ?)`,
		string(rt), id, title, body); err != nil {
		return records.DBErr("update search index", err)
	}
	return nil
}

// nextNumber allocates the next repository-local number for a numbered table
// (tasks, pull_requests). Safe because commands hold the single write
// transaction.
func (s *Store) nextNumber(tx *sql.Tx, table string) (int64, error) {
	var n sql.NullInt64
	err := tx.QueryRow(fmt.Sprintf(`SELECT MAX(number) FROM %s WHERE repository_id = ?`, table), s.RepoID).Scan(&n)
	if err != nil {
		return 0, records.DBErr("allocate number", err)
	}
	return n.Int64 + 1, nil
}

type querier interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

// resolveID resolves a full ULID or unambiguous prefix to a record ID in the
// given table.
func (s *Store) resolveID(q querier, table, ref string) (string, error) {
	ref = strings.ToUpper(strings.TrimSpace(ref))
	if ref == "" {
		return "", records.Validationf("empty id")
	}
	rows, err := q.Query(fmt.Sprintf(
		`SELECT id FROM %s WHERE repository_id = ? AND id LIKE ? AND deleted_at IS NULL LIMIT 3`, table),
		s.RepoID, ref+"%")
	if err != nil {
		return "", records.DBErr("resolve id", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", records.DBErr("resolve id", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", records.DBErr("resolve id", err)
	}
	switch len(ids) {
	case 0:
		return "", records.NotFoundf("no %s matching %q", strings.TrimSuffix(table, "s"), ref)
	case 1:
		return ids[0], nil
	default:
		return "", records.Validationf("ambiguous id prefix %q (matches %s and %s)", ref, ids[0], ids[1])
	}
}

// PendingMutations counts mutations not yet accepted by the cloud.
func (s *Store) PendingMutations(ctx context.Context) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mutations WHERE repository_id = ? AND status = 'pending'`, s.RepoID).Scan(&n)
	if err != nil {
		return 0, records.DBErr("count mutations", err)
	}
	return n, nil
}

// SearchResult is one full-text search hit.
type SearchResult struct {
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	Title      string `json:"title"`
	Snippet    string `json:"snippet"`
}

// Search runs a full-text query across tasks, comments, thread messages,
// pull requests, and reviews.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, records.Validationf("empty search query")
	}
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.DB.QueryContext(ctx, `
		SELECT record_type, record_id, title, snippet(fts, 3, '', '', '…', 12)
		FROM fts WHERE fts MATCH ? ORDER BY rank LIMIT ?`, query, limit)
	if err != nil {
		return nil, records.Validationf("search failed (FTS5 syntax): %v", err)
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var r SearchResult
		if err := rows.Scan(&r.RecordType, &r.RecordID, &r.Title, &r.Snippet); err != nil {
			return nil, records.DBErr("scan search result", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Repair (docs/v1-spec.md §9.3, elk-work/ark#60) ---------------------
//
// The surface `ark repair push` runs on: replay this checkout's acknowledged
// mutations into a service that no longer holds their result. Everything here
// is reachable only after a history reset has been recorded (§9.2), which is
// the gate that keeps a replay a decision rather than a retry.

// RecordKey names one record the way the mutation stream does, so a map built
// from a pull response can be read by a caller holding only a mutation row.
func RecordKey(recordType, recordID string) string { return recordType + "/" + recordID }

// replayable selects the mutations a repair sends: every one the service has
// already ruled on, whether it accepted or refused.
//
// The accepted ones are the obvious half and the reason the command exists. A
// mutation leaves the queue when the service acknowledges it, so after a reset
// the applied set is precisely the work the service confirmed and no longer
// holds — the reason nothing re-sends it on its own, and the reason the only
// recovery before this was an UPDATE typed into SQLite by hand.
//
// The refused ones are here because a verdict from a history that no longer
// exists is not a verdict about the service being talked to now. It is the
// same category error as trusting a stale `base_server_revision`: the number
// and the refusal were both issued by a database that is gone. A rejection's
// local effect is kept rather than rolled back (§9.1), so the client still
// holds that change, and a rebuild that left it out would be quietly partial.
// It is also what makes an interrupted repair resumable, which a multi-client
// rebuild needs: nothing orders one client's replay against another's, so an
// update to a record whose author has not replayed yet is refused, and running
// the command again once they have is the way through.
//
// The two halves are re-queued differently, and it is the server's idempotency
// rule that forces it. §9.1 has the service process each mutation by ID and
// keep the outcome, which is what makes an accepted replay converge instead of
// duplicating — and equally what makes a *refusal* permanent for that ID. So an
// applied mutation is re-queued in place, keeping the ID that makes it
// idempotent, while a refused one is re-submitted under a fresh ULID carrying
// the same intent. That is not a workaround for the rule, it is the rule:
// §9.1 says a rejected mutation is never retried, and §10.8's resolution path
// already re-submits a refused change as a new mutation for exactly this
// reason. The rejected row stays untouched as the durable evidence §8 requires.
//
// Conflicts are left alone entirely. They are not a verdict the service reached
// but a decision waiting on a person, with `ark conflict resolve` to carry it
// out (§10.8); replaying one would answer a question that is still open.
const replayable = `status IN ('applied', 'rejected')`

// ReplayableMutations counts the mutations a repair would replay.
func (s *Store) ReplayableMutations(ctx context.Context) (int, error) {
	var n int
	err := s.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mutations WHERE repository_id = ? AND `+replayable, s.RepoID).Scan(&n)
	if err != nil {
		return 0, records.DBErr("count replayable mutations", err)
	}
	return n, nil
}

// ResetSyncCursor rewinds the pull cursor to zero.
//
// The cursor is a high-water mark and no ordinary sync may lower it (§9.2):
// assigning the service's revision to it is what erased the evidence in
// elk-work/ark#58. A repair is the one place it has to come down, and for a
// reason the high-water rule does not cover — after a reset the mark is a
// position in a history nobody is serving, so it does not point at anything.
//
// Left alone it is not merely wrong, it is silent. A checkout at revision 18
// asking a service at revision 4 for "everything after 18" gets an empty
// answer, and goes on getting one until the service climbs back past 18. The
// pull that is supposed to merge in the records the service *does* still hold
// would return none of them, and the repair would replay over a repository it
// had never looked at. The recorded reset is what keeps the old mark: it
// stores the event rather than the cursor, precisely so the cursor can move.
func (s *Store) ResetSyncCursor(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx,
		`UPDATE sync_state SET last_revision = 0 WHERE repository_id = ?`, s.RepoID)
	if err != nil {
		return records.DBErr("rewind sync cursor", err)
	}
	return nil
}

// RequeueForReplay puts every replayable mutation back in the queue, rebased
// onto the history the service is serving now, and reports how many.
//
// `held` maps RecordKey to the revision the repair's own pull just returned
// for each record the service still has. It is the whole of what this checkout
// has seen of the live history, gathered one call earlier.
//
// The rebase is the part that has to be right, and it is why a repair is a
// considered command rather than a flag. `base_server_revision` means "the
// version of this record my change was made against" (§8), and §10.4 reads it
// against the service's per-field revisions: every field written after that
// base is a concurrent edit, which conflicts (title, body) or loses to the
// cloud and is dropped (everything else). The comparison means something only
// while both numbers count the same history, and after a reset they do not.
// Replaying with the recorded bases reads one scale against another, and it
// fails in both directions:
//
//   - A base *below* the service's current revision for the record drops
//     fields silently while the mutation is still reported applied — the
//     defect §8 warns about and elk-work/ark#28 already paid for.
//   - A base *above* it skips the merge check altogether, so the replay
//     overwrites whatever the service does still hold. That is the common
//     shape here, because the dead history was long and the live one is short
//     or empty, and it is the worse failure for a repair: its first duty is
//     not to delete the records that survived.
//
// So the base is re-derived rather than adjusted. The repair pulls before it
// pushes, so at this point the client has seen exactly what the service holds:
// the honest base for a record is the revision that pull returned for it, or 0
// where it returned nothing — which says the service has never written a
// revision of this record, because it has not. Both halves are literally true,
// which is what a base has to be.
//
// A create carries 0 whatever the map says (§8). The server does not read a
// create's base, and the client's own causal chain is rebuilt on the far side
// without it: a create replayed in the same push publishes its new revision
// into that push's session, which lifts every later mutation for the same
// record up to it (internal/server/engine.go). A create-then-edit sequence
// therefore replays with the merge check correctly skipped at every step and
// no field of it dropped — the same mechanism that already applies a burst of
// offline edits in order.
//
// Which mutations move, and why, is `replayable` above.
func (s *Store) RequeueForReplay(ctx context.Context, held map[string]int64) (int, error) {
	var n int
	err := db.InTx(ctx, s.DB, func(tx *sql.Tx) error {
		n = 0 // the closure reruns on retry; reset so the count cannot double
		type queued struct {
			id, status, recordType, recordID, operation string
			payload, createdAt, createdBy               string
		}
		var queue []queued
		// ULID order, which is replay order (§9.1). The whole set is read
		// before any of it is written: one statement cannot walk a result set
		// and rewrite the rows under it. Reading in order also keeps the
		// re-minted IDs below in the order their originals were made, because
		// records.NewID is monotonic within a process.
		rows, err := tx.Query(`SELECT id, status, record_type, record_id, operation,
			payload_json, created_at, created_by FROM mutations
			WHERE repository_id = ? AND `+replayable+` ORDER BY id`, s.RepoID)
		if err != nil {
			return records.DBErr("load replayable mutations", err)
		}
		for rows.Next() {
			var q queued
			if err := rows.Scan(&q.id, &q.status, &q.recordType, &q.recordID, &q.operation,
				&q.payload, &q.createdAt, &q.createdBy); err != nil {
				rows.Close()
				return records.DBErr("scan mutation", err)
			}
			queue = append(queue, q)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return records.DBErr("load replayable mutations", err)
		}
		rows.Close()

		for _, q := range queue {
			var base int64
			if q.operation == "update" || q.operation == "delete" {
				base = held[RecordKey(q.recordType, q.recordID)]
			}
			if q.status == "rejected" {
				// A fresh ID, the original intent. `created_at` travels with
				// it because it is the authoritative answer to "when was this
				// change made" and the service stamps `updated_at` from it
				// (internal/server/engine.go); `created_by` travels because a
				// replay must not re-attribute somebody's work to whoever ran
				// the repair.
				if _, err := tx.Exec(`INSERT INTO mutations
					(id, repository_id, record_type, record_id, operation, base_server_revision,
					 payload_json, created_at, created_by, status)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
					records.NewID(), s.RepoID, q.recordType, q.recordID, q.operation, base,
					q.payload, q.createdAt, q.createdBy); err != nil {
					return records.DBErr("re-submit refused mutation", err)
				}
				n++
				continue
			}
			if _, err := tx.Exec(`UPDATE mutations SET status = 'pending',
				base_server_revision = ?, error_message = '' WHERE id = ?`, base, q.id); err != nil {
				return records.DBErr("requeue mutation", err)
			}
			n++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ClearHistoryReset forgets a recorded reset.
//
// The one honest clearing condition, and the reason the reset is not
// self-clearing in the first place. A rejection clears itself because "the
// record and the service agree again" is something the client can observe;
// this cannot, because what ends it is a person deciding what to do about
// records the service no longer holds (migration 0005, §9.2). A completed
// repair is that decision carried out, so it — and nothing else — may retire
// the mark.
func (s *Store) ClearHistoryReset(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, `UPDATE sync_state SET history_reset_at = NULL,
		history_reset_server_revision = NULL, history_reset_local_revision = NULL
		WHERE repository_id = ?`, s.RepoID)
	if err != nil {
		return records.DBErr("clear history reset", err)
	}
	return nil
}

// DisplayNumbers returns the current display number of every numbered record,
// keyed by RecordKey.
//
// A repair reads this on both sides of its replay. The service reassigns a
// task or pull-request number already taken by another record (§6.2), so a
// repaired repository can come back numbered differently — and the numbers
// that moved are the thing a person has to be told, because an `ark:<repo>#N`
// written down anywhere outside Ark still names the old one.
func (s *Store) DisplayNumbers(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range []struct{ table, recordType string }{
		{"tasks", string(records.TypeTask)},
		{"pull_requests", string(records.TypePullRequest)},
	} {
		rows, err := s.DB.QueryContext(ctx, fmt.Sprintf(
			`SELECT id, number FROM %s WHERE repository_id = ?`, t.table), s.RepoID)
		if err != nil {
			return nil, records.DBErr("load display numbers", err)
		}
		for rows.Next() {
			var id string
			var number int64
			if err := rows.Scan(&id, &number); err != nil {
				rows.Close()
				return nil, records.DBErr("scan display number", err)
			}
			out[RecordKey(t.recordType, id)] = number
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, records.DBErr("load display numbers", err)
		}
		rows.Close()
	}
	return out, nil
}

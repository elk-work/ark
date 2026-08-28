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

// PullSkips counts pulled records this client could not represent, by record
// type. A non-empty map means the server holds record types this build does
// not know — normal during a version skew, and worth showing the operator
// rather than dropping in silence.
type PullSkips map[string]int

// ApplyPull applies a pull response — records, tombstones, and the new
// cursor — in one transaction (docs/v1-spec.md §9.2).
//
// Records of an unknown type are skipped, not rejected: a client must
// tolerate a server that knows more record types than it does, or a single
// unrecognized record would wedge every future pull. The skips are returned
// so the caller can report them.
func (s *Store) ApplyPull(ctx context.Context, resp *api.PullResponse) (PullSkips, error) {
	skips := PullSkips{}
	err := db.InTx(ctx, s.DB, func(tx *sql.Tx) error {
		// The closure reruns on retry; reset so counts cannot double.
		clear(skips)
		// Revision order is not dependency order (an updated PR can outrank
		// its reviews), so FK checks wait for the end of the transaction.
		if _, err := tx.Exec(`PRAGMA defer_foreign_keys = ON`); err != nil {
			return records.DBErr("defer foreign keys", err)
		}
		for _, rec := range resp.Records {
			handled, err := upsertServerRecord(tx, rec)
			if err != nil {
				return err
			}
			if !handled {
				skips[rec.RecordType]++
				continue
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
		for _, tomb := range resp.Tombstones {
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
		if _, err := tx.Exec(`UPDATE sync_state SET last_revision = ?, last_synced_at = ?
			WHERE repository_id = ?`, resp.ServerRevision, records.Now(), s.RepoID); err != nil {
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

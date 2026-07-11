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

	"github.com/ijroth/ark/internal/db"
	"github.com/ijroth/ark/internal/records"
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
	DB     *sql.DB
	RepoID string
	Actor  Actor
}

func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	return db.InTx(ctx, s.DB, fn)
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

// resolveThreadMessageParent is like resolveID but for tables keyed by
// thread_id rather than repository_id.
func resolveInThread(q querier, threadID, ref string) (string, error) {
	ref = strings.ToUpper(strings.TrimSpace(ref))
	rows, err := q.Query(`SELECT id FROM thread_messages WHERE thread_id = ? AND id LIKE ? LIMIT 3`,
		threadID, ref+"%")
	if err != nil {
		return "", records.DBErr("resolve message id", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", records.DBErr("resolve message id", err)
		}
		ids = append(ids, id)
	}
	switch len(ids) {
	case 0:
		return "", records.NotFoundf("no message matching %q", ref)
	case 1:
		return ids[0], nil
	default:
		return "", records.Validationf("ambiguous message id prefix %q", ref)
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

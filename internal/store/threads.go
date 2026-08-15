package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/elk-work/ark/internal/records"
)

// Thread is the durable conversation around work. See docs/v1-spec.md §6.4.
type Thread struct {
	ID            string `json:"id"`
	RepositoryID  string `json:"repository_id"`
	TaskID        string `json:"task_id,omitempty"`
	Title         string `json:"title"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
	CreatedByType string `json:"created_by_type"`
	ClosedAt      string `json:"closed_at,omitempty"`
}

// Message is one entry in a thread. See docs/v1-spec.md §6.5.
type Message struct {
	ID            string `json:"id"`
	ThreadID      string `json:"thread_id"`
	Role          string `json:"role"`
	Body          string `json:"body"`
	MetadataJSON  string `json:"metadata_json,omitempty"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
	CreatedByType string `json:"created_by_type"`
	SupersedesID  string `json:"supersedes_id,omitempty"`
}

// CreateThread opens a thread, optionally linked to a task.
func (s *Store) CreateThread(ctx context.Context, taskID, title string) (*Thread, error) {
	if strings.TrimSpace(title) == "" {
		return nil, records.Validationf("thread title is required")
	}
	t := &Thread{
		ID:            records.NewID(),
		RepositoryID:  s.RepoID,
		TaskID:        taskID,
		Title:         title,
		Status:        "open",
		CreatedAt:     records.Now(),
		CreatedBy:     s.Actor.ID,
		CreatedByType: string(s.Actor.Type),
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var task any
		if taskID != "" {
			task = taskID
		}
		if _, err := tx.Exec(`INSERT INTO agent_threads
			(id, repository_id, task_id, title, status, created_at, created_by, created_by_type, sync_state)
			VALUES (?, ?, ?, ?, 'open', ?, ?, ?, 'local')`,
			t.ID, t.RepositoryID, task, t.Title, t.CreatedAt, t.CreatedBy, t.CreatedByType); err != nil {
			return records.DBErr("create thread", err)
		}
		return s.logMutation(tx, records.TypeThread, t.ID, "create", 0, t)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ResolveThread loads a thread by ULID or prefix.
func (s *Store) ResolveThread(ctx context.Context, ref string) (*Thread, error) {
	id, err := s.resolveID(s.DB, "agent_threads", ref)
	if err != nil {
		return nil, err
	}
	var t Thread
	var taskID, closedAt sql.NullString
	err = s.DB.QueryRowContext(ctx, `SELECT id, repository_id, task_id, title, status,
		created_at, created_by, created_by_type, closed_at
		FROM agent_threads WHERE id = ?`, id).
		Scan(&t.ID, &t.RepositoryID, &taskID, &t.Title, &t.Status,
			&t.CreatedAt, &t.CreatedBy, &t.CreatedByType, &closedAt)
	if err != nil {
		return nil, records.DBErr("load thread", err)
	}
	t.TaskID = taskID.String
	t.ClosedAt = closedAt.String
	return &t, nil
}

// ListThreads returns threads, optionally filtered to one task.
func (s *Store) ListThreads(ctx context.Context, taskID string) ([]*Thread, error) {
	q := `SELECT id, repository_id, task_id, title, status, created_at, created_by,
		created_by_type, closed_at FROM agent_threads
		WHERE repository_id = ? AND deleted_at IS NULL`
	args := []any{s.RepoID}
	if taskID != "" {
		q += ` AND task_id = ?`
		args = append(args, taskID)
	}
	q += ` ORDER BY created_at`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, records.DBErr("list threads", err)
	}
	defer rows.Close()
	var out []*Thread
	for rows.Next() {
		var t Thread
		var taskID, closedAt sql.NullString
		if err := rows.Scan(&t.ID, &t.RepositoryID, &taskID, &t.Title, &t.Status,
			&t.CreatedAt, &t.CreatedBy, &t.CreatedByType, &closedAt); err != nil {
			return nil, records.DBErr("scan thread", err)
		}
		t.TaskID = taskID.String
		t.ClosedAt = closedAt.String
		out = append(out, &t)
	}
	return out, rows.Err()
}

// CloseThread marks a thread closed.
func (s *Store) CloseThread(ctx context.Context, ref string) (*Thread, error) {
	t, err := s.ResolveThread(ctx, ref)
	if err != nil {
		return nil, err
	}
	if t.Status == "closed" {
		return t, nil
	}
	t.Status = "closed"
	t.ClosedAt = records.Now()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`UPDATE agent_threads SET status = 'closed', closed_at = ? WHERE id = ?`,
			t.ClosedAt, t.ID); err != nil {
			return records.DBErr("close thread", err)
		}
		return s.logMutation(tx, records.TypeThread, t.ID, "update",
			0, map[string]string{"status": "closed", "closed_at": t.ClosedAt})
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// AddMessage appends a message to a thread.
func (s *Store) AddMessage(ctx context.Context, threadRef, role, body, metadataJSON string) (*Message, error) {
	if err := records.OneOf("role", role, records.MessageRoles); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		return nil, records.Validationf("message body is required")
	}
	t, err := s.ResolveThread(ctx, threadRef)
	if err != nil {
		return nil, err
	}
	if t.Status == "closed" {
		return nil, records.Validationf("thread %s is closed", t.ID)
	}
	m := &Message{
		ID:            records.NewID(),
		ThreadID:      t.ID,
		Role:          role,
		Body:          body,
		MetadataJSON:  metadataJSON,
		CreatedAt:     records.Now(),
		CreatedBy:     s.Actor.ID,
		CreatedByType: string(s.Actor.Type),
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO thread_messages
			(id, thread_id, role, body, metadata_json, created_at, created_by, created_by_type, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'local')`,
			m.ID, m.ThreadID, m.Role, m.Body, m.MetadataJSON,
			m.CreatedAt, m.CreatedBy, m.CreatedByType); err != nil {
			return records.DBErr("create message", err)
		}
		if err := s.ftsSet(tx, records.TypeMessage, m.ID, "", m.Body); err != nil {
			return err
		}
		return s.logMutation(tx, records.TypeMessage, m.ID, "append", 0, m)
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ListMessages returns a thread's messages in order.
func (s *Store) ListMessages(ctx context.Context, threadID string) ([]*Message, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, thread_id, role, body, metadata_json,
		created_at, created_by, created_by_type, COALESCE(supersedes_id, '')
		FROM thread_messages WHERE thread_id = ? AND deleted_at IS NULL
		ORDER BY created_at`, threadID)
	if err != nil {
		return nil, records.DBErr("list messages", err)
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ThreadID, &m.Role, &m.Body, &m.MetadataJSON,
			&m.CreatedAt, &m.CreatedBy, &m.CreatedByType, &m.SupersedesID); err != nil {
			return nil, records.DBErr("scan message", err)
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

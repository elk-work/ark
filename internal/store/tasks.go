package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/elk-work/ark/internal/records"
)

// Task represents requested work. See docs/v1-spec.md §6.2.
type Task struct {
	ID            string `json:"id"`
	RepositoryID  string `json:"repository_id"`
	Number        int64  `json:"number"`
	Title         string `json:"title"`
	Body          string `json:"body,omitempty"`
	Status        string `json:"status"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
	CreatedByType string `json:"created_by_type"`
	UpdatedAt     string `json:"updated_at"`
	Version       int64  `json:"version"`
	SyncState     string `json:"sync_state"`
}

const taskCols = `id, repository_id, number, title, body, status, created_at,
	created_by, created_by_type, updated_at, version, sync_state`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	err := row.Scan(&t.ID, &t.RepositoryID, &t.Number, &t.Title, &t.Body, &t.Status,
		&t.CreatedAt, &t.CreatedBy, &t.CreatedByType, &t.UpdatedAt, &t.Version, &t.SyncState)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTask creates a task with the next repository-local number.
func (s *Store) CreateTask(ctx context.Context, title, body string) (*Task, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, records.Validationf("task title is required")
	}
	now := records.Now()
	t := &Task{
		ID:            records.NewID(),
		RepositoryID:  s.RepoID,
		Title:         title,
		Body:          body,
		Status:        "open",
		CreatedAt:     now,
		CreatedBy:     s.Actor.ID,
		CreatedByType: string(s.Actor.Type),
		UpdatedAt:     now,
		Version:       1,
		SyncState:     "local",
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := s.nextNumber(tx, "tasks")
		if err != nil {
			return err
		}
		t.Number = n
		if _, err := tx.Exec(`INSERT INTO tasks
			(id, repository_id, number, title, body, status, created_at, created_by,
			 created_by_type, updated_at, version, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, 'local')`,
			t.ID, t.RepositoryID, t.Number, t.Title, t.Body, t.Status,
			t.CreatedAt, t.CreatedBy, t.CreatedByType, t.UpdatedAt); err != nil {
			return records.DBErr("create task", err)
		}
		if err := s.ftsSet(tx, records.TypeTask, t.ID, t.Title, t.Body); err != nil {
			return err
		}
		return s.logMutation(tx, records.TypeTask, t.ID, "create", 0, t)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// ResolveTask accepts a task number ("12") or ULID (prefix) and returns the task.
func (s *Store) ResolveTask(ctx context.Context, ref string) (*Task, error) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		// Numbers can transiently duplicate mid-sync; the oldest record wins.
		// "Oldest" is the lowest ULID, not the lowest created_at — the text
		// ordering of an RFC3339Nano stamp is not chronological, and this
		// LIMIT 1 decides *which* task a bare number resolves to.
		row := s.DB.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks
			WHERE repository_id = ? AND number = ? AND deleted_at IS NULL
			ORDER BY id LIMIT 1`, s.RepoID, n)
		t, err := scanTask(row)
		if err == sql.ErrNoRows {
			return nil, records.NotFoundf("task #%d not found", n)
		}
		if err != nil {
			return nil, records.DBErr("load task", err)
		}
		return t, nil
	}
	id, err := s.resolveID(s.DB, "tasks", ref)
	if err != nil {
		return nil, err
	}
	row := s.DB.QueryRowContext(ctx, `SELECT `+taskCols+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if err != nil {
		return nil, records.DBErr("load task", err)
	}
	return t, nil
}

// ListTasks returns tasks, optionally filtered by status.
func (s *Store) ListTasks(ctx context.Context, status string) ([]*Task, error) {
	q := `SELECT ` + taskCols + ` FROM tasks WHERE repository_id = ? AND deleted_at IS NULL`
	args := []any{s.RepoID}
	if status != "" && status != "all" {
		if err := records.OneOf("status", status, records.TaskStatuses); err != nil {
			return nil, err
		}
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY number`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, records.DBErr("list tasks", err)
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, records.DBErr("scan task", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaskEdit describes the fields an update may change. Nil means unchanged.
type TaskEdit struct {
	Title  *string `json:"title,omitempty"`
	Body   *string `json:"body,omitempty"`
	Status *string `json:"status,omitempty"`
}

// UpdateTask applies an edit, bumps the version, and logs the mutation with
// only the changed fields (field-level payloads keep sync merges precise).
func (s *Store) UpdateTask(ctx context.Context, ref string, edit TaskEdit) (*Task, error) {
	t, err := s.ResolveTask(ctx, ref)
	if err != nil {
		return nil, err
	}
	if edit.Title == nil && edit.Body == nil && edit.Status == nil {
		return nil, records.Validationf("nothing to change (pass --title, --body, or --status)")
	}
	if edit.Title != nil && strings.TrimSpace(*edit.Title) == "" {
		return nil, records.Validationf("task title cannot be empty")
	}
	if edit.Status != nil {
		if err := records.OneOf("status", *edit.Status, records.TaskStatuses); err != nil {
			return nil, err
		}
		if !records.ValidTaskTransition(t.Status, *edit.Status) {
			return nil, records.Validationf("cannot move task from %s to %s", t.Status, *edit.Status)
		}
	}
	if edit.Title != nil {
		t.Title = *edit.Title
	}
	if edit.Body != nil {
		t.Body = *edit.Body
	}
	if edit.Status != nil {
		t.Status = *edit.Status
	}
	t.UpdatedAt = records.Now()
	t.Version++
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE tasks SET title = ?, body = ?, status = ?, updated_at = ?, version = ?
			WHERE id = ? AND version = ?`,
			t.Title, t.Body, t.Status, t.UpdatedAt, t.Version, t.ID, t.Version-1)
		if err != nil {
			return records.DBErr("update task", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return records.Conflictf("task #%d changed concurrently; retry", t.Number)
		}
		if err := s.ftsSet(tx, records.TypeTask, t.ID, t.Title, t.Body); err != nil {
			return err
		}
		return s.logUpdate(tx, records.TypeTask, t.ID, edit)
	})
	if err != nil {
		return nil, err
	}
	return t, nil
}

// CloseTask marks a task closed.
func (s *Store) CloseTask(ctx context.Context, ref string) (*Task, error) {
	st := "closed"
	return s.UpdateTask(ctx, ref, TaskEdit{Status: &st})
}

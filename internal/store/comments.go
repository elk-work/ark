package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/ijroth/ark/internal/records"
)

// Comment belongs to one parent record. Published comments are append-only;
// an edit creates a new comment with SupersedesID set. See docs/v1-spec.md §6.3.
type Comment struct {
	ID            string `json:"id"`
	RepositoryID  string `json:"repository_id"`
	ParentType    string `json:"parent_type"`
	ParentID      string `json:"parent_id"`
	Body          string `json:"body"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
	CreatedByType string `json:"created_by_type"`
	SupersedesID  string `json:"supersedes_id,omitempty"`
}

// AddComment appends a comment to a parent record.
func (s *Store) AddComment(ctx context.Context, parentType, parentID, body, supersedes string) (*Comment, error) {
	if err := records.OneOf("parent type", parentType, records.CommentParents); err != nil {
		return nil, err
	}
	if strings.TrimSpace(body) == "" {
		return nil, records.Validationf("comment body is required")
	}
	c := &Comment{
		ID:            records.NewID(),
		RepositoryID:  s.RepoID,
		ParentType:    parentType,
		ParentID:      parentID,
		Body:          body,
		CreatedAt:     records.Now(),
		CreatedBy:     s.Actor.ID,
		CreatedByType: string(s.Actor.Type),
		SupersedesID:  supersedes,
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var super any
		if supersedes != "" {
			super = supersedes
		}
		if _, err := tx.Exec(`INSERT INTO comments
			(id, repository_id, parent_type, parent_id, body, created_at, created_by,
			 created_by_type, supersedes_id, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'local')`,
			c.ID, c.RepositoryID, c.ParentType, c.ParentID, c.Body,
			c.CreatedAt, c.CreatedBy, c.CreatedByType, super); err != nil {
			return records.DBErr("create comment", err)
		}
		if err := s.ftsSet(tx, records.TypeComment, c.ID, "", c.Body); err != nil {
			return err
		}
		return s.logMutation(tx, records.TypeComment, c.ID, "append", 0, c)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ListComments returns comments for a parent record in creation order.
func (s *Store) ListComments(ctx context.Context, parentType, parentID string) ([]*Comment, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, repository_id, parent_type, parent_id, body,
		created_at, created_by, created_by_type, COALESCE(supersedes_id, '')
		FROM comments WHERE parent_type = ? AND parent_id = ? AND deleted_at IS NULL
		ORDER BY created_at`, parentType, parentID)
	if err != nil {
		return nil, records.DBErr("list comments", err)
	}
	defer rows.Close()
	var out []*Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.RepositoryID, &c.ParentType, &c.ParentID, &c.Body,
			&c.CreatedAt, &c.CreatedBy, &c.CreatedByType, &c.SupersedesID); err != nil {
			return nil, records.DBErr("scan comment", err)
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

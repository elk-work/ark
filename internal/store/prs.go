package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"github.com/elk-work/ark/internal/records"
)

// PullRequest describes a proposed Git change. Git owns the branches and
// commits; Ark owns the review state. See docs/v1-spec.md §6.7.
type PullRequest struct {
	ID             string `json:"id"`
	RepositoryID   string `json:"repository_id"`
	Number         int64  `json:"number"`
	TaskID         string `json:"task_id,omitempty"`
	Title          string `json:"title"`
	Body           string `json:"body,omitempty"`
	Status         string `json:"status"`
	BaseBranch     string `json:"base_branch"`
	HeadBranch     string `json:"head_branch"`
	BaseCommitSHA  string `json:"base_commit_sha,omitempty"`
	HeadCommitSHA  string `json:"head_commit_sha,omitempty"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	CreatedAt      string `json:"created_at"`
	CreatedBy      string `json:"created_by"`
	CreatedByType  string `json:"created_by_type"`
	MergedAt       string `json:"merged_at,omitempty"`
	ClosedAt       string `json:"closed_at,omitempty"`
	UpdatedAt      string `json:"updated_at"`
	Version        int64  `json:"version"`
}

// Review is an immutable judgment on a pull request. See docs/v1-spec.md §6.8.
type Review struct {
	ID            string `json:"id"`
	RepositoryID  string `json:"repository_id"`
	PullRequestID string `json:"pull_request_id"`
	State         string `json:"state"`
	Body          string `json:"body,omitempty"`
	CommitSHA     string `json:"commit_sha,omitempty"`
	CreatedAt     string `json:"created_at"`
	CreatedBy     string `json:"created_by"`
	CreatedByType string `json:"created_by_type"`
}

// CreatePR records a pull request over existing Git branches.
func (s *Store) CreatePR(ctx context.Context, pr *PullRequest) (*PullRequest, error) {
	if strings.TrimSpace(pr.Title) == "" {
		return nil, records.Validationf("pull request title is required")
	}
	if pr.BaseBranch == "" || pr.HeadBranch == "" {
		return nil, records.Validationf("base and head branches are required")
	}
	if pr.BaseBranch == pr.HeadBranch {
		return nil, records.Validationf("base and head branch are both %q", pr.BaseBranch)
	}
	now := records.Now()
	pr.ID = records.NewID()
	pr.RepositoryID = s.RepoID
	pr.Status = "open"
	pr.CreatedAt = now
	pr.UpdatedAt = now
	pr.CreatedBy = s.Actor.ID
	pr.CreatedByType = string(s.Actor.Type)
	pr.Version = 1
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		n, err := s.nextNumber(tx, "pull_requests")
		if err != nil {
			return err
		}
		pr.Number = n
		if _, err := tx.Exec(`INSERT INTO pull_requests
			(id, repository_id, number, task_id, title, body, status, base_branch, head_branch,
			 base_commit_sha, head_commit_sha, created_at, created_by, created_by_type,
			 updated_at, version, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, ?, 1, 'local')`,
			pr.ID, pr.RepositoryID, pr.Number, nullable(pr.TaskID), pr.Title, pr.Body,
			pr.BaseBranch, pr.HeadBranch, pr.BaseCommitSHA, pr.HeadCommitSHA,
			pr.CreatedAt, pr.CreatedBy, pr.CreatedByType, pr.UpdatedAt); err != nil {
			return records.DBErr("create pull request", err)
		}
		if err := s.ftsSet(tx, records.TypePullRequest, pr.ID, pr.Title, pr.Body); err != nil {
			return err
		}
		return s.logMutation(tx, records.TypePullRequest, pr.ID, "create", 0, pr)
	})
	if err != nil {
		return nil, err
	}
	return pr, nil
}

const prCols = `id, repository_id, number, task_id, title, body, status, base_branch,
	head_branch, base_commit_sha, head_commit_sha, merge_commit_sha, created_at,
	created_by, created_by_type, merged_at, closed_at, updated_at, version`

func scanPR(row interface{ Scan(...any) error }) (*PullRequest, error) {
	var pr PullRequest
	var taskID, mergedAt, closedAt sql.NullString
	err := row.Scan(&pr.ID, &pr.RepositoryID, &pr.Number, &taskID, &pr.Title, &pr.Body,
		&pr.Status, &pr.BaseBranch, &pr.HeadBranch, &pr.BaseCommitSHA, &pr.HeadCommitSHA,
		&pr.MergeCommitSHA, &pr.CreatedAt, &pr.CreatedBy, &pr.CreatedByType,
		&mergedAt, &closedAt, &pr.UpdatedAt, &pr.Version)
	if err != nil {
		return nil, err
	}
	pr.TaskID = taskID.String
	pr.MergedAt = mergedAt.String
	pr.ClosedAt = closedAt.String
	return &pr, nil
}

// ResolvePR accepts a PR number or ULID prefix.
func (s *Store) ResolvePR(ctx context.Context, ref string) (*PullRequest, error) {
	ref = strings.TrimSpace(ref)
	if n, err := strconv.ParseInt(ref, 10, 64); err == nil {
		pr, err := scanPR(s.DB.QueryRowContext(ctx, `SELECT `+prCols+` FROM pull_requests
			WHERE repository_id = ? AND number = ? AND deleted_at IS NULL
			ORDER BY created_at LIMIT 1`, s.RepoID, n))
		if err == sql.ErrNoRows {
			return nil, records.NotFoundf("pull request #%d not found", n)
		}
		if err != nil {
			return nil, records.DBErr("load pull request", err)
		}
		return pr, nil
	}
	id, err := s.resolveID(s.DB, "pull_requests", ref)
	if err != nil {
		return nil, err
	}
	pr, err := scanPR(s.DB.QueryRowContext(ctx, `SELECT `+prCols+` FROM pull_requests WHERE id = ?`, id))
	if err != nil {
		return nil, records.DBErr("load pull request", err)
	}
	return pr, nil
}

// ListPRs returns pull requests, optionally filtered by status.
func (s *Store) ListPRs(ctx context.Context, status string) ([]*PullRequest, error) {
	q := `SELECT ` + prCols + ` FROM pull_requests WHERE repository_id = ? AND deleted_at IS NULL`
	args := []any{s.RepoID}
	if status != "" && status != "all" {
		if err := records.OneOf("status", status, records.PRStatuses); err != nil {
			return nil, err
		}
		q += ` AND status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY number`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, records.DBErr("list pull requests", err)
	}
	defer rows.Close()
	var out []*PullRequest
	for rows.Next() {
		pr, err := scanPR(rows)
		if err != nil {
			return nil, records.DBErr("scan pull request", err)
		}
		out = append(out, pr)
	}
	return out, rows.Err()
}

// SubmitReview records an immutable review of a pull request.
func (s *Store) SubmitReview(ctx context.Context, prRef, state, body, commitSHA string) (*Review, *PullRequest, error) {
	if err := records.OneOf("review state", state, records.ReviewStates); err != nil {
		return nil, nil, err
	}
	if state != "approve" && strings.TrimSpace(body) == "" {
		return nil, nil, records.Validationf("a %s review needs a body", state)
	}
	pr, err := s.ResolvePR(ctx, prRef)
	if err != nil {
		return nil, nil, err
	}
	if pr.Status != "open" {
		return nil, nil, records.Validationf("pull request #%d is %s; reviews need an open PR", pr.Number, pr.Status)
	}
	r := &Review{
		ID:            records.NewID(),
		RepositoryID:  s.RepoID,
		PullRequestID: pr.ID,
		State:         state,
		Body:          body,
		CommitSHA:     commitSHA,
		CreatedAt:     records.Now(),
		CreatedBy:     s.Actor.ID,
		CreatedByType: string(s.Actor.Type),
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO reviews
			(id, repository_id, pull_request_id, state, body, commit_sha,
			 created_at, created_by, created_by_type, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'local')`,
			r.ID, r.RepositoryID, r.PullRequestID, r.State, r.Body, r.CommitSHA,
			r.CreatedAt, r.CreatedBy, r.CreatedByType); err != nil {
			return records.DBErr("create review", err)
		}
		if err := s.ftsSet(tx, records.TypeReview, r.ID, "", r.Body); err != nil {
			return err
		}
		return s.logMutation(tx, records.TypeReview, r.ID, "submit", 0, r)
	})
	if err != nil {
		return nil, nil, err
	}
	return r, pr, nil
}

// ListReviews returns a PR's reviews in order.
func (s *Store) ListReviews(ctx context.Context, prID string) ([]*Review, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT id, repository_id, pull_request_id, state, body,
		commit_sha, created_at, created_by, created_by_type
		FROM reviews WHERE pull_request_id = ? AND deleted_at IS NULL ORDER BY created_at`, prID)
	if err != nil {
		return nil, records.DBErr("list reviews", err)
	}
	defer rows.Close()
	var out []*Review
	for rows.Next() {
		var r Review
		if err := rows.Scan(&r.ID, &r.RepositoryID, &r.PullRequestID, &r.State, &r.Body,
			&r.CommitSHA, &r.CreatedAt, &r.CreatedBy, &r.CreatedByType); err != nil {
			return nil, records.DBErr("scan review", err)
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// PRStateChange records merge/close transitions.
type PRStateChange struct {
	Status         string `json:"status"`
	MergeCommitSHA string `json:"merge_commit_sha,omitempty"`
	MergedAt       string `json:"merged_at,omitempty"`
	ClosedAt       string `json:"closed_at,omitempty"`
	HeadCommitSHA  string `json:"head_commit_sha,omitempty"`
}

// UpdatePRHead refreshes the recorded head commit after the head branch
// moves. Git owns branches (docs/v1-spec.md §10.5: the head branch is read
// from Git before merge); the record follows.
func (s *Store) UpdatePRHead(ctx context.Context, pr *PullRequest, headSHA string) error {
	if pr.HeadCommitSHA == headSHA {
		return nil
	}
	now := records.Now()
	change := PRStateChange{HeadCommitSHA: headSHA}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE pull_requests SET head_commit_sha = ?, updated_at = ?,
			version = version + 1 WHERE id = ? AND status = 'open'`, headSHA, now, pr.ID)
		if err != nil {
			return records.DBErr("update pull request head", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return records.Conflictf("pull request #%d is no longer open", pr.Number)
		}
		return s.logUpdate(tx, records.TypePullRequest, pr.ID, change)
	})
	if err == nil {
		pr.HeadCommitSHA = headSHA
	}
	return err
}

// MarkPRMerged transitions a PR to merged with its merge commit.
func (s *Store) MarkPRMerged(ctx context.Context, pr *PullRequest, mergeSHA, headSHA string) error {
	now := records.Now()
	change := PRStateChange{Status: "merged", MergeCommitSHA: mergeSHA, MergedAt: now, HeadCommitSHA: headSHA}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE pull_requests SET status = 'merged', merge_commit_sha = ?,
			head_commit_sha = ?, merged_at = ?, updated_at = ?, version = version + 1
			WHERE id = ? AND status = 'open'`, mergeSHA, headSHA, now, now, pr.ID)
		if err != nil {
			return records.DBErr("mark merged", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return records.Conflictf("pull request #%d is no longer open", pr.Number)
		}
		return s.logUpdate(tx, records.TypePullRequest, pr.ID, change)
	})
	if err == nil {
		pr.Status = "merged"
		pr.MergeCommitSHA = mergeSHA
		pr.MergedAt = now
	}
	return err
}

// ClosePR closes a PR without merging.
func (s *Store) ClosePR(ctx context.Context, ref string) (*PullRequest, error) {
	pr, err := s.ResolvePR(ctx, ref)
	if err != nil {
		return nil, err
	}
	if pr.Status != "open" {
		return nil, records.Validationf("pull request #%d is already %s", pr.Number, pr.Status)
	}
	now := records.Now()
	change := PRStateChange{Status: "closed", ClosedAt: now}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE pull_requests SET status = 'closed', closed_at = ?,
			updated_at = ?, version = version + 1 WHERE id = ? AND status = 'open'`,
			now, now, pr.ID)
		if err != nil {
			return records.DBErr("close pull request", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return records.Conflictf("pull request #%d is no longer open", pr.Number)
		}
		return s.logUpdate(tx, records.TypePullRequest, pr.ID, change)
	})
	if err != nil {
		return nil, err
	}
	pr.Status = "closed"
	pr.ClosedAt = now
	return pr, nil
}

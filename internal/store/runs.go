package store

import (
	"context"
	"database/sql"
	"strings"

	"github.com/ijroth/ark/internal/records"
)

// Run records one agent execution. See docs/v1-spec.md §6.6.
type Run struct {
	ID              string `json:"id"`
	RepositoryID    string `json:"repository_id"`
	TaskID          string `json:"task_id,omitempty"`
	ThreadID        string `json:"thread_id,omitempty"`
	AgentName       string `json:"agent_name"`
	AgentVersion    string `json:"agent_version,omitempty"`
	Status          string `json:"status"`
	InputSummary    string `json:"input_summary,omitempty"`
	ResultSummary   string `json:"result_summary,omitempty"`
	StartedAt       string `json:"started_at,omitempty"`
	FinishedAt      string `json:"finished_at,omitempty"`
	ExitCode        *int64 `json:"exit_code,omitempty"`
	BranchName      string `json:"branch_name,omitempty"`
	BaseCommitSHA   string `json:"base_commit_sha,omitempty"`
	ResultCommitSHA string `json:"result_commit_sha,omitempty"`
	MetadataJSON    string `json:"metadata_json,omitempty"`
	CreatedAt       string `json:"created_at"`
	CreatedBy       string `json:"created_by"`
	CreatedByType   string `json:"created_by_type"`
}

// StartRun records a new running execution.
func (s *Store) StartRun(ctx context.Context, r *Run) (*Run, error) {
	if strings.TrimSpace(r.AgentName) == "" {
		return nil, records.Validationf("agent name is required")
	}
	now := records.Now()
	r.ID = records.NewID()
	r.RepositoryID = s.RepoID
	r.Status = "running"
	r.StartedAt = now
	r.CreatedAt = now
	r.CreatedBy = s.Actor.ID
	r.CreatedByType = string(s.Actor.Type)
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO agent_runs
			(id, repository_id, task_id, thread_id, agent_name, agent_version, status,
			 input_summary, started_at, branch_name, base_commit_sha, metadata_json,
			 created_at, created_by, created_by_type, sync_state)
			VALUES (?, ?, ?, ?, ?, ?, 'running', ?, ?, ?, ?, ?, ?, ?, ?, 'local')`,
			r.ID, r.RepositoryID, nullable(r.TaskID), nullable(r.ThreadID),
			r.AgentName, r.AgentVersion, r.InputSummary, r.StartedAt,
			r.BranchName, r.BaseCommitSHA, r.MetadataJSON,
			r.CreatedAt, r.CreatedBy, r.CreatedByType); err != nil {
			return records.DBErr("create run", err)
		}
		return s.logMutation(tx, records.TypeRun, r.ID, "create", 0, r)
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// RunFinish describes the completion of a run.
type RunFinish struct {
	Status          string `json:"status"`
	ResultSummary   string `json:"result_summary,omitempty"`
	ExitCode        *int64 `json:"exit_code,omitempty"`
	ResultCommitSHA string `json:"result_commit_sha,omitempty"`
	FinishedAt      string `json:"finished_at"`
}

// FinishRun completes a run with a terminal status.
func (s *Store) FinishRun(ctx context.Context, ref string, fin RunFinish) (*Run, error) {
	switch fin.Status {
	case "succeeded", "failed", "cancelled":
	default:
		return nil, records.Validationf("finish status must be succeeded, failed, or cancelled (got %q)", fin.Status)
	}
	r, err := s.ResolveRun(ctx, ref)
	if err != nil {
		return nil, err
	}
	if r.Status != "running" && r.Status != "queued" {
		return nil, records.Validationf("run %s already finished (%s)", r.ID, r.Status)
	}
	fin.FinishedAt = records.Now()
	r.Status = fin.Status
	r.ResultSummary = fin.ResultSummary
	r.ExitCode = fin.ExitCode
	r.ResultCommitSHA = fin.ResultCommitSHA
	r.FinishedAt = fin.FinishedAt
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var exit any
		if fin.ExitCode != nil {
			exit = *fin.ExitCode
		}
		if _, err := tx.Exec(`UPDATE agent_runs SET status = ?, result_summary = ?,
			exit_code = ?, result_commit_sha = ?, finished_at = ? WHERE id = ?`,
			fin.Status, fin.ResultSummary, exit, fin.ResultCommitSHA, fin.FinishedAt, r.ID); err != nil {
			return records.DBErr("finish run", err)
		}
		return s.logMutation(tx, records.TypeRun, r.ID, "update", 0, fin)
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

const runCols = `id, repository_id, task_id, thread_id, agent_name, agent_version, status,
	input_summary, result_summary, started_at, finished_at, exit_code, branch_name,
	base_commit_sha, result_commit_sha, metadata_json, created_at, created_by, created_by_type`

func scanRun(row interface{ Scan(...any) error }) (*Run, error) {
	var r Run
	var taskID, threadID, startedAt, finishedAt sql.NullString
	var exit sql.NullInt64
	err := row.Scan(&r.ID, &r.RepositoryID, &taskID, &threadID, &r.AgentName, &r.AgentVersion,
		&r.Status, &r.InputSummary, &r.ResultSummary, &startedAt, &finishedAt, &exit,
		&r.BranchName, &r.BaseCommitSHA, &r.ResultCommitSHA, &r.MetadataJSON,
		&r.CreatedAt, &r.CreatedBy, &r.CreatedByType)
	if err != nil {
		return nil, err
	}
	r.TaskID = taskID.String
	r.ThreadID = threadID.String
	r.StartedAt = startedAt.String
	r.FinishedAt = finishedAt.String
	if exit.Valid {
		r.ExitCode = &exit.Int64
	}
	return &r, nil
}

// ResolveRun loads a run by ULID or prefix.
func (s *Store) ResolveRun(ctx context.Context, ref string) (*Run, error) {
	id, err := s.resolveID(s.DB, "agent_runs", ref)
	if err != nil {
		return nil, err
	}
	r, err := scanRun(s.DB.QueryRowContext(ctx, `SELECT `+runCols+` FROM agent_runs WHERE id = ?`, id))
	if err != nil {
		return nil, records.DBErr("load run", err)
	}
	return r, nil
}

// ListRuns returns runs, optionally filtered to a task.
func (s *Store) ListRuns(ctx context.Context, taskID string) ([]*Run, error) {
	q := `SELECT ` + runCols + ` FROM agent_runs WHERE repository_id = ? AND deleted_at IS NULL`
	args := []any{s.RepoID}
	if taskID != "" {
		q += ` AND task_id = ?`
		args = append(args, taskID)
	}
	q += ` ORDER BY created_at`
	rows, err := s.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, records.DBErr("list runs", err)
	}
	defer rows.Close()
	var out []*Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, records.DBErr("scan run", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

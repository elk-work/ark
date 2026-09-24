package api

import "encoding/json"

// BoardActor is a task's creating actor, not an assignee.
type BoardActor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type BoardTask struct {
	ID        string     `json:"id"`
	Number    int64      `json:"number"`
	Title     string     `json:"title"`
	Status    string     `json:"status"`
	Actor     BoardActor `json:"actor"`
	CreatedAt string     `json:"created_at"`
	UpdatedAt string     `json:"updated_at"`
	ElkRef    string     `json:"elk_ref,omitempty"`
	ElkURL    string     `json:"elk_url,omitempty"`
	Body      string     `json:"body,omitempty"`
}

type TaskListResponse struct {
	RepositoryID string      `json:"repository_id"`
	Tasks        []BoardTask `json:"tasks"`
}

// Related documents retain their existing record field vocabulary.
type TaskDetailResponse struct {
	RepositoryID string            `json:"repository_id"`
	Task         BoardTask         `json:"task"`
	Comments     []json.RawMessage `json:"comments"`
	Runs         []json.RawMessage `json:"runs"`
	PullRequests []json.RawMessage `json:"pull_requests"`
	Artifacts    []json.RawMessage `json:"artifacts"`
}

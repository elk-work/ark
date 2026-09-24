package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// boardRecord is a read projection; Raw preserves the full related document.
type boardRecord struct {
	ID            string          `json:"id"`
	Number        int64           `json:"number"`
	Title         string          `json:"title"`
	Body          string          `json:"body"`
	Status        string          `json:"status"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
	CreatedBy     string          `json:"created_by"`
	CreatedByType string          `json:"created_by_type"`
	TaskID        string          `json:"task_id"`
	ParentID      string          `json:"parent_id"`
	ParentType    string          `json:"parent_type"`
	PullRequestID string          `json:"pull_request_id"`
	SupersedesID  string          `json:"supersedes_id"`
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	AgentName     string          `json:"agent_name"`
	InputSummary  string          `json:"input_summary"`
	ResultSummary string          `json:"result_summary"`
	StartedAt     string          `json:"started_at"`
	FinishedAt    string          `json:"finished_at"`
	MediaType     string          `json:"media_type"`
	SizeBytes     int64           `json:"size_bytes"`
	SHA256        string          `json:"sha256"`
	Raw           json.RawMessage `json:"-"`
}

type boardData struct {
	Repo    api.RepositoryMetadata
	Records map[string][]boardRecord
	Actors  map[string]api.BoardActor
}

// One View keeps all relationships in one repository snapshot. There are no
// per-card queries and no calls to Elk.
func (s *Server) readBoard(ctx context.Context, repo string) (*boardData, error) {
	b := &boardData{Records: map[string][]boardRecord{}, Actors: map[string]api.BoardActor{}}
	err := s.Repos.View(ctx, repo, func(db *sql.DB) error {
		if err := loadMetadata(db.QueryRowContext(ctx, metadataQuery), &b.Repo); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT record_type, record_id, data FROM records WHERE deleted_at IS NULL
   AND record_type IN ('task','comment','agent_run','pull_request','review','artifact','actor')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var kind, id string
			var raw []byte
			if err := rows.Scan(&kind, &id, &raw); err != nil {
				return err
			}
			var rec boardRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				return err
			}
			rec.ID = id
			rec.Raw = json.RawMessage(raw)
			if kind == "actor" {
				b.Actors[id] = api.BoardActor{ID: id, Name: rec.Name, Type: rec.Type}
			} else {
				b.Records[kind] = append(b.Records[kind], rec)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	for kind := range b.Records {
		sort.Slice(b.Records[kind], func(i, j int) bool {
			a, c := b.Records[kind][i], b.Records[kind][j]
			if kind == "task" && a.Number != c.Number {
				return a.Number < c.Number
			}
			ta, ea := time.Parse(time.RFC3339Nano, a.CreatedAt)
			tc, ec := time.Parse(time.RFC3339Nano, c.CreatedAt)
			if kind != "task" && ea == nil && ec == nil && !ta.Equal(tc) {
				return ta.Before(tc)
			}
			return a.ID < c.ID
		})
	}
	return b, nil
}

func (b *boardData) comments(id string) []boardRecord {
	out := []boardRecord{}
	for _, c := range b.Records["comment"] {
		if c.ParentType == "task" && c.ParentID == id {
			out = append(out, c)
		}
	}
	return out
}

func (b *boardData) task(t boardRecord, body bool) api.BoardTask {
	actor, ok := b.Actors[t.CreatedBy]
	if !ok {
		actor = api.BoardActor{ID: t.CreatedBy, Name: t.CreatedBy, Type: t.CreatedByType}
	}
	if actor.Name == "" {
		actor.Name = actor.ID
	}
	ref, link := boardParent(t.Body, b.comments(t.ID))
	out := api.BoardTask{ID: t.ID, Number: t.Number, Title: t.Title, Status: t.Status, Actor: actor, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, ElkRef: ref, ElkURL: link}
	if body {
		out.Body = t.Body
	}
	return out
}

var explicitElk = regexp.MustCompile(`(?i)\belk(?::|#|\s+#)([a-z0-9][a-z0-9_-]*|#[0-9]+)`)
var workspacePath = regexp.MustCompile(`^/open/[A-Za-z0-9_-]+$`)
var explicitHTTPS = regexp.MustCompile(`https://[^\s<>"'()]+`)

func parentWorkspace(source string) string {
	for _, candidate := range explicitHTTPS.FindAllString(source, -1) {
		candidate = strings.TrimRight(candidate, ".,;]")
		u, err := url.Parse(candidate)
		if err == nil && u.Scheme == "https" && u.Host == "elk.work" && u.User == nil &&
			workspacePath.MatchString(u.Path) && u.RawQuery == "" && u.Fragment == "" {
			return u.String()
		}
	}
	return ""
}

func parentKey(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) >= 4 && strings.EqualFold(ref[:4], "elk:") {
		ref = strings.TrimSpace(ref[4:])
	}
	if strings.HasPrefix(strings.ToLower(ref), "elk#") || strings.HasPrefix(strings.ToLower(ref), "elk #") {
		ref = strings.TrimSpace(ref[3:])
	}
	return strings.ToLower(strings.TrimPrefix(ref, "#"))
}

func normalizedParent(ref string) string {
	key := parentKey(ref)
	if _, err := strconv.ParseUint(key, 10, 64); err == nil {
		return "#" + key
	}
	return key
}

func boardParent(body string, comments []boardRecord) (string, string) {
	// Markers use ULID creation order, exactly as CLI ListComments does,
	// independently of display sorting by the stored timestamp.
	var markerID, ref, source string
	for _, c := range comments {
		first, _, _ := strings.Cut(c.Body, "\n")
		first = strings.TrimSpace(first)
		if len(first) >= 11 && strings.EqualFold(first[:11], "elk-parent:") {
			candidate := strings.TrimSpace(first[11:])
			if candidate != "" && !strings.ContainsAny(candidate, " \t\r\n") && c.ID > markerID {
				markerID = c.ID
				ref = candidate
				source = c.Body
			}
		}
	}
	if ref == "" {
		sources := []string{body}
		for _, c := range comments {
			sources = append(sources, c.Body)
		}
		for _, text := range sources {
			if m := explicitElk.FindStringSubmatch(text); m != nil {
				ref = m[1]
				source = text
				break
			}
		}
	}
	if ref == "" {
		return "", ""
	}
	return normalizedParent(ref), parentWorkspace(source)
}

func boardFilters(q url.Values) (string, string, string, error) {
	status := q.Get("status")
	if status == "" {
		status = "all"
	}
	if status != "all" {
		if err := records.OneOf("status", status, records.TaskStatuses); err != nil {
			return "", "", "", err
		}
	}
	return status, q.Get("actor"), parentKey(q.Get("elk")), nil
}

func (b *boardData) list(q url.Values) (api.TaskListResponse, error) {
	out := api.TaskListResponse{RepositoryID: b.Repo.ID, Tasks: []api.BoardTask{}}
	status, actor, elk, err := boardFilters(q)
	if err != nil {
		return out, err
	}
	for _, record := range b.Records["task"] {
		if status != "all" && status != record.Status || actor != "" && actor != record.CreatedBy {
			continue
		}
		task := b.task(record, false)
		if elk != "" && parentKey(task.ElkRef) != elk {
			continue
		}
		out.Tasks = append(out.Tasks, task)
	}
	return out, nil
}

func (b *boardData) detail(id string) (*api.TaskDetailResponse, error) {
	out := &api.TaskDetailResponse{RepositoryID: b.Repo.ID, Comments: []json.RawMessage{}, Runs: []json.RawMessage{}, PullRequests: []json.RawMessage{}, Artifacts: []json.RawMessage{}}
	for _, t := range b.Records["task"] {
		if t.ID == id {
			out.Task = b.task(t, true)
			break
		}
	}
	if out.Task.ID == "" {
		return nil, faultNotFound("task not found")
	}
	parents := map[string]bool{"task/" + id: true}
	for _, c := range b.comments(id) {
		out.Comments = append(out.Comments, c.Raw)
	}
	for _, run := range b.Records["agent_run"] {
		if run.TaskID == id {
			out.Runs = append(out.Runs, run.Raw)
			parents["agent_run/"+run.ID] = true
		}
	}
	for _, pr := range b.Records["pull_request"] {
		if pr.TaskID == id {
			out.PullRequests = append(out.PullRequests, pr.Raw)
			parents["pull_request/"+pr.ID] = true
		}
	}
	for _, review := range b.Records["review"] {
		if parents["pull_request/"+review.PullRequestID] {
			parents["review/"+review.ID] = true
		}
	}
	for _, a := range b.Records["artifact"] {
		if parents[a.ParentType+"/"+a.ParentID] {
			out.Artifacts = append(out.Artifacts, a.Raw)
		}
	}
	return out, nil
}

func (s *Server) handleTaskList(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if !s.allowUI(w, r, repo) {
		return
	}
	if _, _, _, err := boardFilters(r.URL.Query()); err != nil {
		writeErr(w, 400, "validation", err.Error())
		return
	}
	b, err := s.readBoard(r.Context(), repo)
	if err != nil {
		s.respond(w, "read tasks", err)
		return
	}
	out, err := b.list(r.URL.Query())
	if err != nil {
		writeErr(w, 400, "validation", err.Error())
		return
	}
	writeJSON(w, out)
}

func (s *Server) handleTaskDetail(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if !s.allowUI(w, r, repo) {
		return
	}
	id := r.PathValue("id")
	if !records.ValidID(id) {
		writeErr(w, 400, "validation", "task must be a full ULID")
		return
	}
	b, err := s.readBoard(r.Context(), repo)
	if err != nil {
		s.respond(w, "read task", err)
		return
	}
	out, err := b.detail(id)
	if err != nil {
		s.finish(w, "read task", err, nil, 200)
		return
	}
	writeJSON(w, out)
}

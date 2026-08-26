package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

const (
	humanID = "01TESTHUMAN000000000000000"
	agent   = "release-bot"
)

// writeReq exercises a write route with auth and an optional idempotency key.
func writeReq(t *testing.T, s *Server, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// seedHuman introduces the human actor a remote writer must delegate from,
// through the ordinary push path a client would have used.
func seedHuman(t *testing.T, s *Server) {
	t.Helper()
	body, err := json.Marshal(api.PushRequest{
		RepositoryID: repoID, ClientID: "c1",
		Actors:    []api.Actor{{ID: humanID, Type: "human", Name: "Alice", Email: "alice@example.com"}},
		Mutations: []api.Mutation{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := doRequest(t, s, "POST", "/v1/sync/push", string(body)); rec.Code != 200 {
		t.Fatalf("seed human: %d %s", rec.Code, rec.Body.String())
	}
}

// writeServer is a registered repository with a human actor already in it.
func writeServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	registerRepo(t, s)
	seedHuman(t, s)
	return s
}

func decodeWrite(t *testing.T, rec *httptest.ResponseRecorder) api.RecordResponse {
	t.Helper()
	var resp api.RecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

// taskBody builds a create-task request naming the seeded writer.
func taskBody(title string) string {
	return fmt.Sprintf(`{"writer":{"agent_name":%q,"delegated_by":%q},"title":%q}`,
		agent, humanID, title)
}

func createTask(t *testing.T, s *Server, key, title string) api.RecordResponse {
	t.Helper()
	rec := writeReq(t, s, "/v1/repositories/"+repoID+"/tasks", key, taskBody(title))
	if rec.Code != 201 {
		t.Fatalf("create task: %d %s", rec.Code, rec.Body.String())
	}
	return decodeWrite(t, rec)
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error %q: %v", rec.Body.String(), err)
	}
	return e.Code
}

func revision(t *testing.T, s *Server) int64 {
	t.Helper()
	var rev int64
	err := s.Repos.View(context.Background(), repoID, func(db *sql.DB) error {
		return db.QueryRow(`SELECT revision FROM meta WHERE id = 1`).Scan(&rev)
	})
	if err != nil {
		t.Fatalf("read revision: %v", err)
	}
	return rev
}

// TestCreateTaskRoundTripsThroughTheClientStruct is the drift guard. The
// service writes record documents as maps, so nothing but this test stops a
// field name here from diverging from what a client unmarshals on pull.
func TestCreateTaskRoundTripsThroughTheClientStruct(t *testing.T) {
	s := writeServer(t)
	resp := createTask(t, s, "k1", "Flaky test in internal/sync")

	var task store.Task
	if err := json.Unmarshal(resp.Record.Data, &task); err != nil {
		t.Fatalf("decode as store.Task: %v", err)
	}
	if !records.ValidID(task.ID) {
		t.Errorf("id %q is not a ULID", task.ID)
	}
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"repository_id", task.RepositoryID, repoID},
		{"number", task.Number, int64(1)},
		{"title", task.Title, "Flaky test in internal/sync"},
		{"status", task.Status, "open"},
		{"created_by_type", task.CreatedByType, "agent"},
		{"version", task.Version, int64(1)},
		{"sync_state", task.SyncState, "synced"},
		{"record_id matches id", resp.Record.RecordID, task.ID},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if task.CreatedAt == "" || task.UpdatedAt == "" {
		t.Errorf("timestamps: created_at %q updated_at %q", task.CreatedAt, task.UpdatedAt)
	}
	if !records.ValidID(task.CreatedBy) {
		t.Errorf("created_by %q is not an actor ULID", task.CreatedBy)
	}
}

// TestWriterRegistration covers RFC-0004 Decision 2: an agent cannot invent
// the authority it acts under, and once registered it is reused.
func TestWriterRegistration(t *testing.T) {
	path := "/v1/repositories/" + repoID + "/tasks"
	cases := []struct {
		name     string
		writer   string
		wantCode int
		wantErr  string
	}{
		{"no agent name", `{"delegated_by":"` + humanID + `"}`, 400, "validation"},
		{"new agent without delegation", `{"agent_name":"drifter"}`, 400, "validation"},
		{"delegated to an unknown actor", `{"agent_name":"drifter","delegated_by":"01NOBODY0000000000000000000"}`, 400, "validation"},
		{"delegated to an agent, not a human", `{"agent_name":"drifter","delegated_by":"01AGENT00000000000000000000"}`, 400, "validation"},
		{"registered", `{"agent_name":"drifter","delegated_by":"` + humanID + `"}`, 201, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := writeServer(t)
			body, err := json.Marshal(api.PushRequest{RepositoryID: repoID, ClientID: "c1",
				Actors: []api.Actor{{ID: "01AGENT00000000000000000000", Type: "agent",
					Name: "other", AgentName: "other", DelegatedBy: humanID}},
				Mutations: []api.Mutation{}})
			if err != nil {
				t.Fatal(err)
			}
			doRequest(t, s, "POST", "/v1/sync/push", string(body))

			rec := writeReq(t, s, path, "k1",
				`{"writer":`+c.writer+`,"title":"T"}`)
			if rec.Code != c.wantCode {
				t.Fatalf("code %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantErr != "" && errCode(t, rec) != c.wantErr {
				t.Errorf("error code %q, want %q", errCode(t, rec), c.wantErr)
			}
		})
	}
}

// A second write by the same agent name reuses the actor rather than
// minting a new identity per request, and cannot re-point it at someone else.
func TestWriterIsReusedAndCannotBeRepointed(t *testing.T) {
	s := writeServer(t)
	first := createTask(t, s, "k1", "One")

	// A second human, and a request claiming the existing agent now
	// delegates from them.
	body, err := json.Marshal(api.PushRequest{RepositoryID: repoID, ClientID: "c1",
		Actors:    []api.Actor{{ID: "01TESTHUMAN2NDPERSON0000000", Type: "human", Name: "Bob"}},
		Mutations: []api.Mutation{}})
	if err != nil {
		t.Fatal(err)
	}
	doRequest(t, s, "POST", "/v1/sync/push", string(body))

	rec := writeReq(t, s, "/v1/repositories/"+repoID+"/tasks", "k2",
		fmt.Sprintf(`{"writer":{"agent_name":%q,"delegated_by":"01TESTHUMAN2NDPERSON0000000"},"title":"Two"}`, agent))
	if rec.Code != 201 {
		t.Fatalf("second create: %d %s", rec.Code, rec.Body.String())
	}
	second := decodeWrite(t, rec)

	var a, b store.Task
	json.Unmarshal(first.Record.Data, &a)
	json.Unmarshal(second.Record.Data, &b)
	if a.CreatedBy != b.CreatedBy {
		t.Errorf("same agent name produced two actors: %s and %s", a.CreatedBy, b.CreatedBy)
	}

	actor := recordData(t, s, "actor", a.CreatedBy)
	if actor["delegated_by"] != humanID {
		t.Errorf("delegated_by = %v, want %s (a request must not re-point a registered agent)",
			actor["delegated_by"], humanID)
	}
}

// TestIdempotency covers Decision 4: a key is required on creates, and a
// replay returns the stored outcome without minting a revision.
func TestIdempotency(t *testing.T) {
	s := writeServer(t)
	path := "/v1/repositories/" + repoID + "/tasks"

	if rec := writeReq(t, s, path, "", taskBody("No key")); rec.Code != 400 {
		t.Errorf("missing key: %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	first := createTask(t, s, "same-key", "Once")
	revAfterFirst := revision(t, s)

	rec := writeReq(t, s, path, "same-key", taskBody("Once"))
	if rec.Code != 201 {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay did not announce itself: %v", rec.Header())
	}
	replay := decodeWrite(t, rec)
	if replay.Record.RecordID != first.Record.RecordID {
		t.Errorf("replay minted a second record: %s then %s",
			first.Record.RecordID, replay.Record.RecordID)
	}
	if replay.ServerRevision != first.ServerRevision {
		t.Errorf("replay revision %d, want %d", replay.ServerRevision, first.ServerRevision)
	}
	if got := revision(t, s); got != revAfterFirst {
		t.Errorf("replay bumped the repository revision %d -> %d", revAfterFirst, got)
	}

	// A different key is a different write.
	second := createTask(t, s, "other-key", "Twice")
	if second.Record.RecordID == first.Record.RecordID {
		t.Error("a fresh key reused the first record")
	}
}

// TestNumberingIsAuthoritative covers Decision 5: the service allocates, and
// it allocates above anything a client has already pushed.
func TestNumberingIsAuthoritative(t *testing.T) {
	s := writeServer(t)
	for i, want := range []int64{1, 2, 3} {
		resp := createTask(t, s, fmt.Sprintf("k%d", i), fmt.Sprintf("Task %d", i))
		var task store.Task
		json.Unmarshal(resp.Record.Data, &task)
		if task.Number != want {
			t.Errorf("task %d: number %d, want %d", i, task.Number, want)
		}
	}
	// A client pushes a task numbered well above the server's sequence.
	push(t, s, mut("m-client", "task", "01CLIENTTASK00000000000000", "create", 0,
		`{"id":"01CLIENTTASK00000000000000","number":41,"title":"From a client","status":"open"}`))

	resp := createTask(t, s, "k-after", "After the client")
	var task store.Task
	json.Unmarshal(resp.Record.Data, &task)
	if task.Number != 42 {
		t.Errorf("number after a client push = %d, want 42", task.Number)
	}
}

// TestWriteReachesPullAndTheFieldMerge covers Decision 4's revision half: a
// server-side write must travel on the next pull, and must be visible to the
// §10.4 field merge so a stale client cannot silently win it back.
func TestWriteReachesPullAndTheFieldMerge(t *testing.T) {
	s := writeServer(t)
	created := createTask(t, s, "k1", "Findable")
	taskID := created.Record.RecordID

	rec := doRequest(t, s, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID))
	if rec.Code != 200 {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body.String())
	}
	var pull api.PullResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pull); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pull.Records {
		if r.RecordType == "task" && r.RecordID == taskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("pull did not carry the created task: %+v", pull.Records)
	}

	// A client editing status from revision 0 has a base older than the
	// server's write. Status is a cloud-wins field, so the edit is dropped.
	push(t, s, mut("m1", "task", taskID, "update", 0, `{"status":"closed"}`))
	if got := recordData(t, s, "task", taskID)["status"]; got != "open" {
		t.Errorf("stale client edit won: status = %v, want open", got)
	}

	// Title is a conflict field, so a stale edit needs a person.
	resp := push(t, s, mut("m2", "task", taskID, "update", 0, `{"title":"Renamed"}`))
	if len(resp.Conflicts) != 1 {
		t.Errorf("stale title edit: %+v, want one conflict", resp)
	}
}

func TestCreateComment(t *testing.T) {
	s := writeServer(t)
	task := createTask(t, s, "k1", "Parent")
	path := "/v1/repositories/" + repoID + "/comments"
	writer := fmt.Sprintf(`"writer":{"agent_name":%q,"delegated_by":%q}`, agent, humanID)

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"unknown parent type", `{` + writer + `,"parent_type":"epic","parent_id":"x","body":"b"}`, 400, "validation"},
		{"missing body", `{` + writer + `,"parent_type":"task","parent_id":"` + task.Record.RecordID + `","body":"  "}`, 400, "validation"},
		{"parent not here", `{` + writer + `,"parent_type":"task","parent_id":"01ABSENT0000000000000000000","body":"b"}`, 404, "not_found"},
		{"on the task", `{` + writer + `,"parent_type":"task","parent_id":"` + task.Record.RecordID + `","body":"looks fixed"}`, 201, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := writeReq(t, s, path, "key-"+c.name, c.body)
			if rec.Code != c.wantCode {
				t.Fatalf("code %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantErr != "" {
				if got := errCode(t, rec); got != c.wantErr {
					t.Errorf("error code %q, want %q", got, c.wantErr)
				}
				return
			}
			var comment store.Comment
			if err := json.Unmarshal(decodeWrite(t, rec).Record.Data, &comment); err != nil {
				t.Fatalf("decode as store.Comment: %v", err)
			}
			if comment.ParentType != "task" || comment.ParentID != task.Record.RecordID {
				t.Errorf("parent: %+v", comment)
			}
			if comment.Body != "looks fixed" || comment.CreatedByType != "agent" {
				t.Errorf("comment: %+v", comment)
			}
			if !records.ValidID(comment.ID) {
				t.Errorf("id %q is not a ULID", comment.ID)
			}
		})
	}
}

// A correction is a new comment carrying supersedes_id — §6.3's append-only
// rule is why there is no edit route.
func TestCommentSupersedes(t *testing.T) {
	s := writeServer(t)
	task := createTask(t, s, "k1", "Parent")
	path := "/v1/repositories/" + repoID + "/comments"
	writer := fmt.Sprintf(`"writer":{"agent_name":%q,"delegated_by":%q}`, agent, humanID)

	first := writeReq(t, s, path, "c1",
		`{`+writer+`,"parent_type":"task","parent_id":"`+task.Record.RecordID+`","body":"wrong"}`)
	if first.Code != 201 {
		t.Fatalf("first comment: %d %s", first.Code, first.Body.String())
	}
	firstID := decodeWrite(t, first).Record.RecordID

	second := writeReq(t, s, path, "c2",
		`{`+writer+`,"parent_type":"task","parent_id":"`+task.Record.RecordID+
			`","body":"right","supersedes_id":"`+firstID+`"}`)
	if second.Code != 201 {
		t.Fatalf("correction: %d %s", second.Code, second.Body.String())
	}
	var c store.Comment
	json.Unmarshal(decodeWrite(t, second).Record.Data, &c)
	if c.SupersedesID != firstID {
		t.Errorf("supersedes_id = %q, want %q", c.SupersedesID, firstID)
	}
	if recordData(t, s, "comment", firstID)["body"] != "wrong" {
		t.Error("the superseded comment was modified; comments are append-only")
	}
}

func TestTaskStatus(t *testing.T) {
	s := writeServer(t)
	task := createTask(t, s, "k1", "Movable")
	id := task.Record.RecordID
	path := "/v1/repositories/" + repoID + "/tasks/" + id + "/status"
	writer := fmt.Sprintf(`"writer":{"agent_name":%q,"delegated_by":%q}`, agent, humanID)

	if rec := writeReq(t, s, path, "", `{`+writer+`,"status":"nope"}`); rec.Code != 400 {
		t.Errorf("invalid status: %d, want 400", rec.Code)
	}
	missing := "/v1/repositories/" + repoID + "/tasks/01ABSENT0000000000000000000/status"
	if rec := writeReq(t, s, missing, "", `{`+writer+`,"status":"done"}`); rec.Code != 404 {
		t.Errorf("unknown task: %d, want 404", rec.Code)
	}

	before := revision(t, s)
	rec := writeReq(t, s, path, "", `{`+writer+`,"status":"in_progress"}`)
	if rec.Code != 200 {
		t.Fatalf("transition: %d %s", rec.Code, rec.Body.String())
	}
	moved := decodeWrite(t, rec)
	if moved.ServerRevision <= before {
		t.Errorf("revision %d did not advance past %d", moved.ServerRevision, before)
	}
	var task2 store.Task
	json.Unmarshal(moved.Record.Data, &task2)
	if task2.Status != "in_progress" || task2.Title != "Movable" {
		t.Errorf("moved task: %+v", task2)
	}
	if task2.UpdatedAt == task.Record.RecordID {
		t.Error("updated_at was not stamped")
	}

	// Asking for the status it already has is a correct answer, not a write.
	after := revision(t, s)
	same := writeReq(t, s, path, "", `{`+writer+`,"status":"in_progress"}`)
	if same.Code != 200 {
		t.Fatalf("repeat: %d %s", same.Code, same.Body.String())
	}
	if got := revision(t, s); got != after {
		t.Errorf("a no-op transition bumped the revision %d -> %d", after, got)
	}
	if decodeWrite(t, same).ServerRevision != after {
		t.Errorf("no-op reported revision %d, want %d", decodeWrite(t, same).ServerRevision, after)
	}
}

func TestWriteRoutesRequireAuth(t *testing.T) {
	s := writeServer(t)
	for _, path := range []string{
		"/v1/repositories/" + repoID + "/tasks",
		"/v1/repositories/" + repoID + "/comments",
		"/v1/repositories/" + repoID + "/tasks/01X/status",
	} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s without a token: %d, want 401", path, rec.Code)
		}
	}
}

func TestWriteToUnregisteredRepository(t *testing.T) {
	s := newTestServer(t)
	rec := writeReq(t, s, "/v1/repositories/01NOSUCHREPO00000000000000/tasks", "k1", taskBody("T"))
	if rec.Code != 404 {
		t.Errorf("unregistered repository: %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

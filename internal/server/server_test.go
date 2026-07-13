package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ijroth/ark/internal/records"
	"github.com/ijroth/ark/internal/servertest"
	"github.com/ijroth/ark/pkg/api"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return &Server{DB: servertest.NewDB(t), Token: "test-token",
		Blobs: &LocalBlobStore{Dir: t.TempDir(), BaseURL: "http://unused"}}
}

const repoID = "01TESTREPO0000000000000000"

func registerRepo(t *testing.T, s *Server) {
	t.Helper()
	_, err := s.DB.Exec(`INSERT INTO repositories (id, name) VALUES ($1, 'test')`, repoID)
	if err != nil {
		t.Fatal(err)
	}
}

// push applies mutations through the same path the HTTP handler uses.
func push(t *testing.T, s *Server, muts ...api.Mutation) api.PushResponse {
	t.Helper()
	ctx := context.Background()
	resp := api.PushResponse{Applied: []api.MutationOutcome{}, Rejected: []api.MutationOutcome{},
		Conflicts: []api.MutationOutcome{}}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var locked bool
	if err := tx.QueryRow(`SELECT true FROM repositories WHERE id = $1 FOR UPDATE`, repoID).Scan(&locked); err != nil {
		t.Fatalf("lock repo: %v", err)
	}
	for _, m := range muts {
		out := processMutation(ctx, tx, repoID, m)
		mo := api.MutationOutcome{MutationID: m.ID, Error: out.err, Remote: out.remote, ServerRevision: out.revision}
		switch out.status {
		case statusApplied:
			resp.Applied = append(resp.Applied, mo)
		case statusConflict:
			resp.Conflicts = append(resp.Conflicts, mo)
		default:
			resp.Rejected = append(resp.Rejected, mo)
		}
	}
	tx.QueryRow(`SELECT revision FROM repositories WHERE id = $1`, repoID).Scan(&resp.ServerRevision)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return resp
}

func mut(id, recordType, recordID, op string, baseRev int64, payload string) api.Mutation {
	return api.Mutation{ID: id, RecordType: recordType, RecordID: recordID, Operation: op,
		BaseServerRevision: baseRev, Payload: json.RawMessage(payload),
		CreatedAt: records.Now(), CreatedBy: "actor1"}
}

func recordData(t *testing.T, s *Server, recordType, recordID string) map[string]any {
	t.Helper()
	var data []byte
	err := s.DB.QueryRow(`SELECT data FROM records WHERE repository_id = $1
		AND record_type = $2 AND record_id = $3`, repoID, recordType, recordID).Scan(&data)
	if err != nil {
		t.Fatalf("load record: %v", err)
	}
	var m map[string]any
	json.Unmarshal(data, &m)
	return m
}

func TestPushIdempotency(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	m := mut("m1", "task", "t1", "create", 0, `{"id":"t1","number":1,"title":"A","status":"open"}`)
	r1 := push(t, s, m)
	if len(r1.Applied) != 1 {
		t.Fatalf("first push: %+v", r1)
	}
	rev := r1.Applied[0].ServerRevision

	// Replay: same outcome, no new revision.
	r2 := push(t, s, m)
	if len(r2.Applied) != 1 || r2.Applied[0].ServerRevision != rev {
		t.Fatalf("replay: %+v", r2)
	}
	if r2.ServerRevision != r1.ServerRevision {
		t.Errorf("replay bumped revision %d -> %d", r1.ServerRevision, r2.ServerRevision)
	}
}

func TestFieldMerge(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	r := push(t, s, mut("m1", "task", "t1", "create", 0,
		`{"id":"t1","number":1,"title":"Original","body":"b","status":"open"}`))
	baseRev := r.Applied[0].ServerRevision

	// Server-side edit: title changes at a newer revision.
	push(t, s, mut("m2", "task", "t1", "update", baseRev, `{"title":"Server title"}`))

	// Client edit from the old base touching only status: unrelated fields
	// merge automatically (spec §10.4).
	r3 := push(t, s, mut("m3", "task", "t1", "update", baseRev, `{"status":"in_progress"}`))
	if len(r3.Applied) != 1 {
		t.Fatalf("unrelated field should merge: %+v", r3)
	}
	data := recordData(t, s, "task", "t1")
	if data["title"] != "Server title" || data["status"] != "in_progress" {
		t.Errorf("merged record: %+v", data)
	}

	// Client edit from the old base touching title: conflict.
	r4 := push(t, s, mut("m4", "task", "t1", "update", baseRev, `{"title":"Client title"}`))
	if len(r4.Conflicts) != 1 {
		t.Fatalf("title overlap should conflict: %+v", r4)
	}
	if !strings.Contains(string(r4.Conflicts[0].Remote), "Server title") {
		t.Errorf("conflict remote missing server state: %s", r4.Conflicts[0].Remote)
	}

	// Status overlap: cloud wins silently (applied, but server value kept).
	// m5 is an up-to-date edit (current base revision) setting the "server"
	// value; m6 arrives from the stale base and loses.
	push(t, s, mut("m5", "task", "t1", "update", r3.Applied[0].ServerRevision, `{"status":"blocked"}`))
	r6 := push(t, s, mut("m6", "task", "t1", "update", baseRev, `{"status":"done"}`))
	if len(r6.Applied) != 1 {
		t.Fatalf("status overlap should apply with cloud winning: %+v", r6)
	}
	if data := recordData(t, s, "task", "t1"); data["status"] != "blocked" {
		t.Errorf("cloud should win status: %v", data["status"])
	}
}

func TestAppendOnlyImmutable(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	push(t, s, mut("m1", "comment", "c1", "append", 0, `{"id":"c1","body":"hello"}`))
	r := push(t, s, mut("m2", "comment", "c1", "update", 0, `{"body":"edited"}`))
	if len(r.Rejected) != 1 {
		t.Fatalf("comment update should be rejected: %+v", r)
	}
	r2 := push(t, s, mut("m3", "review", "r1", "submit", 0, `{"id":"r1","state":"approve"}`))
	if len(r2.Applied) != 1 {
		t.Fatalf("review submit: %+v", r2)
	}
	r3 := push(t, s, mut("m4", "review", "r1", "update", 0, `{"state":"request_changes"}`))
	if len(r3.Rejected) != 1 {
		t.Fatalf("review update should be rejected: %+v", r3)
	}
}

func TestNumberReconciliation(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	// Two offline clients both created task #1 with different ULIDs.
	push(t, s, mut("m1", "task", "aaa", "create", 0, `{"id":"aaa","number":1,"title":"First"}`))
	r := push(t, s, mut("m2", "task", "bbb", "create", 0, `{"id":"bbb","number":1,"title":"Second"}`))
	if len(r.Applied) != 1 {
		t.Fatalf("second create: %+v", r)
	}
	if n := recordData(t, s, "task", "bbb")["number"]; fmt.Sprintf("%v", n) != "2" {
		t.Errorf("second task renumbered to %v, want 2", n)
	}
	if n := recordData(t, s, "task", "aaa")["number"]; fmt.Sprintf("%v", n) != "1" {
		t.Errorf("first task kept number %v, want 1", n)
	}
}

func TestBatchSurvivesBadMutation(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	r := push(t, s,
		mut("m1", "task", "t1", "create", 0, `{"id":"t1","number":1,"title":"ok"}`),
		mut("m2", "task", "t2", "update", 0, `{"title":"no such record"}`),
		mut("m3", "task", "t3", "create", 0, `not even json`),
		mut("m4", "task", "t4", "create", 0, `{"id":"t4","number":2,"title":"also ok"}`),
	)
	if len(r.Applied) != 2 || len(r.Rejected) != 2 {
		t.Fatalf("batch outcome: applied=%d rejected=%d", len(r.Applied), len(r.Rejected))
	}
	// Both good records exist.
	recordData(t, s, "task", "t1")
	recordData(t, s, "task", "t4")
}

func TestMergeEndpointStateMachine(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)
	ctx := context.Background()

	push(t, s, mut("m1", "pull_request", "pr1", "create", 0,
		`{"id":"pr1","number":1,"title":"P","status":"open","head_commit_sha":"abc"}`))

	// Simulate the handler's core transition via HTTP-level helper.
	srv := s
	req := api.MergeRequest{RepositoryID: repoID, HeadCommitSHA: "abc", MergeCommitSHA: "mmm"}
	body, _ := json.Marshal(req)
	rec := doRequest(t, srv, "POST", "/v1/pull-requests/pr1/merge", string(body))
	if rec.Code != 200 {
		t.Fatalf("merge: %d %s", rec.Code, rec.Body.String())
	}
	if st := recordData(t, s, "pull_request", "pr1")["status"]; st != "merged" {
		t.Errorf("status = %v", st)
	}

	// Second merge attempt conflicts.
	rec = doRequest(t, srv, "POST", "/v1/pull-requests/pr1/merge", string(body))
	if rec.Code != 409 {
		t.Errorf("double merge: %d, want 409", rec.Code)
	}
	_ = ctx
}

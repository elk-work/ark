package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

// The run routes (runs.go, elk-work/ark#130). The fixtures send the bodies
// the first server-side caller sends — Elk Cloud's deploy_app — so a change
// that would refuse it fails here rather than in production.

const (
	runsPath   = "/v1/repositories/" + repoID + "/runs"
	deployName = "elk-cloud-deploy"
)

func runPath(runID, suffix string) string { return runsPath + "/" + runID + suffix }

// deployRunBody is deploy_app_db.ts's start body, field for field.
func deployRunBody(commit string) string {
	return fmt.Sprintf(`{"writer":{"agent_name":%q,"delegated_by":%q},"agent_name":%q,`+
		`"input_summary":"Deploy porch to Elk Cloud at %s","base_commit_sha":%q,`+
		`"metadata":{"kind":"deploy","action":"deploy","app":"app-1","commit":%q,"checks":[{"suite":"build","result":"pass"}],"credits_quoted":null}}`,
		deployName, humanID, deployName, commit, commit, commit)
}

func finishBody(status, summary string) string {
	return fmt.Sprintf(`{"writer":{"agent_name":%q,"delegated_by":%q},"status":%q,"result_summary":%q}`,
		deployName, humanID, status, summary)
}

func createRun(t *testing.T, s *Server, key string) api.RecordResponse {
	t.Helper()
	rec := writeReq(t, s, runsPath, key, deployRunBody("abc123"))
	if rec.Code != 201 {
		t.Fatalf("create run: %d %s", rec.Code, rec.Body.String())
	}
	return decodeWrite(t, rec)
}

func asRun(t *testing.T, resp api.RecordResponse) store.Run {
	t.Helper()
	var run store.Run
	if err := json.Unmarshal(resp.Record.Data, &run); err != nil {
		t.Fatalf("decode as store.Run: %v", err)
	}
	return run
}

// storeBlob puts content through the ordinary artifact path — upload-url, the
// signed PUT, confirm — and returns its digest.
func storeBlob(t *testing.T, s *Server, content []byte) string {
	t.Helper()
	digest := sha256Hex(content)
	up := uploadURL(t, s, digest, int64(len(content)))
	if !up.AlreadyStored {
		if code := putBlob(t, up.URL, content); code != http.StatusOK {
			t.Fatalf("put: %d", code)
		}
	}
	if rec := confirm(t, s, digest); rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}
	return digest
}

func artifactBody(name, digest string) string {
	return fmt.Sprintf(`{"writer":{"agent_name":"rein","delegated_by":%q},"name":%q,"media_type":"application/javascript","sha256":%q}`,
		humanID, name, digest)
}

// liveWriteServer is writeServer whose signed blob URLs are reachable.
func liveWriteServer(t *testing.T) *Server {
	t.Helper()
	s, _ := live(t)
	registerRepo(t, s)
	seedHuman(t, s)
	return s
}

// The drift guard for all three record shapes these routes write. The service
// writes documents as maps, so only this stops a key here from parting from
// the field a client unmarshals on pull.
func TestRunRoutesRoundTripThroughTheClientStructs(t *testing.T) {
	s := liveWriteServer(t)
	task := createTask(t, s, "t1", "Ship porch")

	body := fmt.Sprintf(`{"writer":{"agent_name":"ci","agent_version":"2.1","delegated_by":%q},`+
		`"task_id":%q,"input_summary":"build","branch_name":"main","base_commit_sha":"abc123",`+
		`"metadata":{ "kind" : "check" }}`, humanID, task.Record.RecordID)
	rec := writeReq(t, s, runsPath, "r1", body)
	if rec.Code != 201 {
		t.Fatalf("create run: %d %s", rec.Code, rec.Body.String())
	}
	created := decodeWrite(t, rec)
	run := asRun(t, created)
	if created.Record.RecordType != "agent_run" || run.ID != created.Record.RecordID {
		t.Fatalf("record %s/%s, run id %s", created.Record.RecordType, created.Record.RecordID, run.ID)
	}
	if created.Record.ID != created.Record.RecordID {
		t.Errorf("record.id %q, want the record id %q", created.Record.ID, created.Record.RecordID)
	}
	if run.RepositoryID != repoID || run.Status != "running" || run.StartedAt == "" ||
		run.AgentName != "ci" || run.AgentVersion != "2.1" || run.TaskID != task.Record.RecordID ||
		run.InputSummary != "build" || run.BranchName != "main" || run.BaseCommitSHA != "abc123" {
		t.Errorf("run fields did not round-trip: %+v", run)
	}
	if run.MetadataJSON != `{"kind":"check"}` {
		t.Errorf("metadata_json = %q, want the compacted object", run.MetadataJSON)
	}
	if run.CreatedByType != "agent" || run.CreatedBy == "" {
		t.Errorf("run attributed to %q (%s), want an agent", run.CreatedBy, run.CreatedByType)
	}
	if actor := recordData(t, s, "actor", run.CreatedBy); actor["delegated_by"] != humanID || actor["agent_name"] != "ci" {
		t.Errorf("run's actor %+v, want agent ci under %s", actor, humanID)
	}

	exit := int64(0)
	finish, _ := json.Marshal(api.FinishRunRequest{
		Writer: api.Writer{AgentName: "ci", DelegatedBy: humanID}, Status: "succeeded",
		ResultSummary: "built", ExitCode: &exit, ResultCommitSHA: "def456",
	})
	rec = writeReq(t, s, runPath(run.ID, "/finish"), "", string(finish))
	if rec.Code != 200 {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body.String())
	}
	done := asRun(t, decodeWrite(t, rec))
	if done.Status != "succeeded" || done.ResultSummary != "built" || done.FinishedAt == "" ||
		done.ExitCode == nil || *done.ExitCode != 0 || done.ResultCommitSHA != "def456" {
		t.Errorf("finished run: %+v", done)
	}
	if done.InputSummary != "build" || done.MetadataJSON != `{"kind":"check"}` {
		t.Errorf("finish lost what the start wrote: %+v", done)
	}

	digest := storeBlob(t, s, []byte("export default {}"))
	rec = writeReq(t, s, runPath(run.ID, "/artifacts"), "a1", artifactBody("worker.js", digest))
	if rec.Code != 201 {
		t.Fatalf("create artifact: %d %s", rec.Code, rec.Body.String())
	}
	var art store.Artifact
	if err := json.Unmarshal(decodeWrite(t, rec).Record.Data, &art); err != nil {
		t.Fatalf("decode as store.Artifact: %v", err)
	}
	if art.ParentType != "agent_run" || art.ParentID != run.ID || art.Name != "worker.js" ||
		art.MediaType != "application/javascript" || art.SHA256 != digest ||
		art.SizeBytes != int64(len("export default {}")) || art.StorageKey != blobKey(digest) ||
		art.RepositoryID != repoID || art.CreatedByType != "agent" || art.CreatedBy == "" {
		t.Errorf("artifact fields did not round-trip: %+v", art)
	}
}

// What deploy_app sends, and what it reads back: `record.id` from the start,
// and nothing from the finish but a 2xx.
func TestTheDeployClientsRequests(t *testing.T) {
	s := writeServer(t)
	rec := writeReq(t, s, runsPath, "deploy_app:dep-1", deployRunBody("abc123"))
	if rec.Code != 201 {
		t.Fatalf("start: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Record struct {
			ID string `json:"id"`
		} `json:"record"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Record.ID == "" {
		t.Fatalf("the client reads record.id and found none: %s", rec.Body.String())
	}
	run := recordData(t, s, "agent_run", out.Record.ID)
	if run["agent_name"] != deployName || run["base_commit_sha"] != "abc123" ||
		!strings.Contains(run["metadata_json"].(string), `"kind":"deploy"`) {
		t.Errorf("stored run: %+v", run)
	}

	rec = writeReq(t, s, runPath(out.Record.ID, "/finish"), "deploy_app:finish:"+out.Record.ID,
		finishBody("succeeded", "Deployed porch at v12"))
	if rec.Code != 200 {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body.String())
	}
	if got := recordData(t, s, "agent_run", out.Record.ID)["status"]; got != "succeeded" {
		t.Errorf("status after finish = %v", got)
	}
}

func TestCreateRunIdempotency(t *testing.T) {
	s := writeServer(t)
	first := createRun(t, s, "deploy_app:dep-1")
	before := revision(t, s)

	rec := writeReq(t, s, runsPath, "deploy_app:dep-1", deployRunBody("abc123"))
	if rec.Code != 201 {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("replay was not marked Idempotency-Replayed")
	}
	if again := decodeWrite(t, rec); again.Record.RecordID != first.Record.RecordID || again.Record.ID != first.Record.ID {
		t.Errorf("replay answered run %s, first was %s", again.Record.RecordID, first.Record.RecordID)
	}
	if after := revision(t, s); after != before {
		t.Errorf("a replay minted revision %d -> %d", before, after)
	}

	if other := createRun(t, s, "deploy_app:dep-2"); other.Record.RecordID == first.Record.RecordID {
		t.Error("a different key reused the first run")
	}
	if rec := writeReq(t, s, runsPath, "", deployRunBody("abc123")); rec.Code != 400 {
		t.Errorf("keyless create: %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
}

func TestCreateRunValidation(t *testing.T) {
	s := writeServer(t)
	writer := fmt.Sprintf(`"writer":{"agent_name":"ci","delegated_by":%q}`, humanID)
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"metadata that is not an object", `{` + writer + `,"metadata":["a"]}`, 400},
		{"metadata that is a string", `{` + writer + `,"metadata":"x"}`, 400},
		{"no writer", `{"input_summary":"x"}`, 400},
		{"no delegating human", `{"writer":{"agent_name":"ci"}}`, 400},
		{"a delegating human this repository does not hold", `{"writer":{"agent_name":"ci","delegated_by":"01NOTANACTOR00000000000000"}}`, 400},
		{"a task this repository does not hold", `{` + writer + `,"task_id":"01NOSUCHTASK00000000000000"}`, 404},
		{"null metadata", `{` + writer + `,"metadata":null}`, 201},
		{"nothing but a writer", `{` + writer + `}`, 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := writeReq(t, s, runsPath, "k-"+tc.name, tc.body)
			if rec.Code != tc.code {
				t.Errorf("%d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
		})
	}
	if rec := writeReq(t, s, "/v1/repositories/01NOSUCHREP00000000000000A/runs", "k", deployRunBody("a")); rec.Code != 404 {
		t.Errorf("unregistered repository: %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestFinishRun(t *testing.T) {
	s := writeServer(t)
	runID := createRun(t, s, "r1").Record.RecordID

	for _, tc := range []struct{ name, status string }{
		{"a status that is not terminal", "running"},
		{"an unknown status", "done"},
	} {
		if rec := writeReq(t, s, runPath(runID, "/finish"), "", finishBody(tc.status, "")); rec.Code != 400 {
			t.Errorf("%s: %d, want 400 (%s)", tc.name, rec.Code, rec.Body.String())
		}
	}
	if rec := writeReq(t, s, runPath("01NOSUCHRUN000000000000000", "/finish"), "", finishBody("failed", "")); rec.Code != 404 {
		t.Errorf("unknown run: %d, want 404", rec.Code)
	}

	before := revision(t, s)
	rec := writeReq(t, s, runPath(runID, "/finish"), "", finishBody("failed", "broker refused"))
	if rec.Code != 200 {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body.String())
	}
	finished := decodeWrite(t, rec)
	if finished.ServerRevision <= before {
		t.Errorf("finish did not mint a revision: %d -> %d", before, finished.ServerRevision)
	}

	// Asking again for how it already ended is an answer, not a write.
	rec = writeReq(t, s, runPath(runID, "/finish"), "", finishBody("failed", "said twice"))
	if rec.Code != 200 {
		t.Fatalf("repeat finish: %d %s", rec.Code, rec.Body.String())
	}
	if again := decodeWrite(t, rec); again.ServerRevision != finished.ServerRevision ||
		asRun(t, again).ResultSummary != "broker refused" {
		t.Errorf("repeat finish rewrote the run: %+v", asRun(t, again))
	}

	// Ending it differently is refused: a run finishes once.
	rec = writeReq(t, s, runPath(runID, "/finish"), "", finishBody("succeeded", ""))
	if rec.Code != 409 || errCode(t, rec) != "conflict" {
		t.Errorf("refinish as succeeded: %d %s, want 409 conflict", rec.Code, rec.Body.String())
	}
	if got := recordData(t, s, "agent_run", runID)["status"]; got != "failed" {
		t.Errorf("status = %v, want failed", got)
	}
}

// A finish that carries a key replays its first answer, as the status route
// does — so a retry of a finish that landed reads as success.
func TestFinishRunHonoursItsKey(t *testing.T) {
	s := writeServer(t)
	runID := createRun(t, s, "r1").Record.RecordID
	key := "deploy_app:finish:" + runID
	if rec := writeReq(t, s, runPath(runID, "/finish"), key, finishBody("succeeded", "ok")); rec.Code != 200 {
		t.Fatalf("finish: %d %s", rec.Code, rec.Body.String())
	}
	rec := writeReq(t, s, runPath(runID, "/finish"), key, finishBody("succeeded", "ok"))
	if rec.Code != 200 || rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("keyed retry: %d replayed=%q", rec.Code, rec.Header().Get("Idempotency-Replayed"))
	}
}

func TestRunArtifacts(t *testing.T) {
	s := liveWriteServer(t)
	runID := createRun(t, s, "r1").Record.RecordID
	content := []byte("export default { fetch() {} }")
	digest := sha256Hex(content)

	// The bytes have to be stored first, through the one door they have.
	rec := writeReq(t, s, runPath(runID, "/artifacts"), "a1", artifactBody("worker.js", digest))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "upload-url") {
		t.Fatalf("artifact before its blob: %d %s, want 409 naming the upload path", rec.Code, rec.Body.String())
	}
	// Registered with upload-url but never confirmed is not stored either.
	uploadURL(t, s, digest, int64(len(content)))
	if rec := writeReq(t, s, runPath(runID, "/artifacts"), "a1", artifactBody("worker.js", digest)); rec.Code != 409 {
		t.Errorf("artifact over an unconfirmed blob: %d %s, want 409", rec.Code, rec.Body.String())
	}
	storeBlob(t, s, content)

	for _, tc := range []struct {
		name, path, body string
		code             int
	}{
		{"no name", runPath(runID, "/artifacts"), artifactBody(" ", digest), 400},
		{"a digest that is not one", runPath(runID, "/artifacts"), artifactBody("w", "abc"), 400},
		{"an uppercase digest", runPath(runID, "/artifacts"), artifactBody("w", strings.ToUpper(digest)), 400},
		{"a size that disagrees with the blob", runPath(runID, "/artifacts"),
			fmt.Sprintf(`{"writer":{"agent_name":"rein","delegated_by":%q},"name":"w","sha256":%q,"size_bytes":3}`, humanID, digest), 400},
		{"an unknown run", runPath("01NOSUCHRUN000000000000000", "/artifacts"), artifactBody("w", digest), 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := writeReq(t, s, tc.path, "k-"+tc.name, tc.body); rec.Code != tc.code {
				t.Errorf("%d %s, want %d", rec.Code, rec.Body.String(), tc.code)
			}
		})
	}
	if rec := writeReq(t, s, runPath(runID, "/artifacts"), "", artifactBody("w", digest)); rec.Code != 400 {
		t.Errorf("keyless artifact: %d, want 400", rec.Code)
	}

	created := decodeWrite(t, writeReq(t, s, runPath(runID, "/artifacts"), "a1", artifactBody("worker.js", digest)))
	replay := writeReq(t, s, runPath(runID, "/artifacts"), "a1", artifactBody("worker.js", digest))
	if replay.Header().Get("Idempotency-Replayed") != "true" || decodeWrite(t, replay).Record.RecordID != created.Record.RecordID {
		t.Errorf("artifact replay: %s", replay.Body.String())
	}

	list := doRequest(t, s, "GET", runPath(runID, "/artifacts"), "")
	if list.Code != 200 {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	var got api.RunArtifactsResponse
	if err := json.Unmarshal(list.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Artifacts) != 1 || got.Artifacts[0].RecordID != created.Record.RecordID ||
		got.Artifacts[0].ID != created.Record.RecordID || got.ServerRevision < created.ServerRevision {
		t.Fatalf("list = %+v, want the one artifact", got)
	}

	// What a caller does next with it: the ordinary signed download.
	var art store.Artifact
	json.Unmarshal(got.Artifacts[0].Data, &art)
	dlBody, _ := json.Marshal(api.DownloadURLRequest{RepositoryID: repoID, StorageKey: art.StorageKey})
	dl := doRequest(t, s, "POST", "/v1/artifacts/download-url", string(dlBody))
	if dl.Code != 200 {
		t.Fatalf("download-url for the run's artifact: %d %s", dl.Code, dl.Body.String())
	}

	// Another run's list does not carry it, and an unknown run is not found.
	other := createRun(t, s, "r2").Record.RecordID
	var none api.RunArtifactsResponse
	json.Unmarshal(doRequest(t, s, "GET", runPath(other, "/artifacts"), "").Body.Bytes(), &none)
	if len(none.Artifacts) != 0 || none.Artifacts == nil {
		t.Errorf("another run's list = %+v, want an empty array", none.Artifacts)
	}
	if rec := doRequest(t, s, "GET", runPath("01NOSUCHRUN000000000000000", "/artifacts"), ""); rec.Code != 404 {
		t.Errorf("unknown run's list: %d, want 404", rec.Code)
	}
}

// Blobs are content-addressed in a store every repository shares; the blobs
// table is per repository. A blob another repository uploaded is not one this
// repository can record, any more than it could download it.
func TestARunArtifactIsScopedToTheRepositoryThatStoredTheBlob(t *testing.T) {
	s := liveWriteServer(t)
	runID := createRun(t, s, "r1").Record.RecordID

	content := []byte("someone else's bundle")
	digest := sha256Hex(content)
	if rec := doRequest(t, s, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"other"}`, secondRepoID)); rec.Code != 200 {
		t.Fatalf("register the second repository: %d %s", rec.Code, rec.Body.String())
	}
	body, _ := json.Marshal(api.UploadURLRequest{RepositoryID: secondRepoID, SHA256: digest, SizeBytes: int64(len(content))})
	var up api.UploadURLResponse
	json.Unmarshal(doRequest(t, s, "POST", "/v1/artifacts/upload-url", string(body)).Body.Bytes(), &up)
	if code := putBlob(t, up.URL, content); code != http.StatusOK {
		t.Fatalf("put: %d", code)
	}
	body, _ = json.Marshal(api.UploadURLRequest{RepositoryID: secondRepoID, SHA256: digest})
	if rec := doRequest(t, s, "POST", "/v1/artifacts/confirm", string(body)); rec.Code != 200 {
		t.Fatalf("confirm in the second repository: %d %s", rec.Code, rec.Body.String())
	}

	if rec := writeReq(t, s, runPath(runID, "/artifacts"), "a1", artifactBody("w", digest)); rec.Code != 409 {
		t.Errorf("recorded another repository's blob: %d %s, want 409", rec.Code, rec.Body.String())
	}
}

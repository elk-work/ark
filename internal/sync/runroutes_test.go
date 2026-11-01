package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/elk-work/ark/internal/servertest"
	"github.com/elk-work/ark/pkg/api"
)

// arkCall is a server-side caller: plain HTTP, no copy of Ark, the way a
// program using the work-record write routes talks to the service.
func arkCall(t *testing.T, method, url, key string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+servertest.Token)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil && resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, url, raw, err)
		}
	}
	if resp.StatusCode >= 300 {
		t.Logf("%s %s: %d %s", method, url, resp.StatusCode, raw)
	}
	return resp.StatusCode
}

// TestAServerWrittenRunAndItsArtifactReachAClient is the sync-safety guard for
// the run routes (elk-work/ark#130). A record those routes write is pulled by
// every client of the repository, into tables with NOT NULL and foreign-key
// constraints; one a client could not store would fail `ark sync` for
// everyone. So: open a run, record an artifact on it, finish it — all over
// HTTP — then sync a real client and fetch the artifact's bytes through it.
func TestAServerWrittenRunAndItsArtifactReachAClient(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()
	if _, err := a.Store.CreateTask(ctx, "Ship porch", ""); err != nil {
		t.Fatal(err)
	}
	mustRun(t, a) // registers the repository and puts this checkout's human on it

	repo := a.Config.RepositoryID
	human := scalar[string](t, a, `SELECT id FROM actors WHERE type = 'human' ORDER BY id LIMIT 1`)
	base := url + "/v1/repositories/" + repo
	writer := api.Writer{AgentName: "elk-cloud-deploy", DelegatedBy: human}

	var started api.RecordResponse
	if code := arkCall(t, "POST", base+"/runs", "deploy_app:dep-1", map[string]any{
		"writer":          writer,
		"agent_name":      "elk-cloud-deploy",
		"input_summary":   "Deploy porch to Elk Cloud at abc123",
		"base_commit_sha": "abc123",
		"metadata":        map[string]any{"kind": "deploy", "credits_quoted": nil},
	}, &started); code != 201 {
		t.Fatalf("open run: %d", code)
	}
	runID := started.Record.RecordID

	content := []byte("export default { fetch() { return new Response('hi') } }")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	var up api.UploadURLResponse
	if code := arkCall(t, "POST", url+"/v1/artifacts/upload-url", "",
		api.UploadURLRequest{RepositoryID: repo, SHA256: digest, SizeBytes: int64(len(content))}, &up); code != 200 {
		t.Fatalf("upload-url: %d", code)
	}
	put, _ := http.NewRequest(http.MethodPut, up.URL, bytes.NewReader(content))
	putResp, err := http.DefaultClient.Do(put)
	if err != nil || putResp.StatusCode != 200 {
		t.Fatalf("put blob: %v %v", putResp, err)
	}
	putResp.Body.Close()
	if code := arkCall(t, "POST", url+"/v1/artifacts/confirm", "",
		api.UploadURLRequest{RepositoryID: repo, SHA256: digest}, nil); code != 200 {
		t.Fatalf("confirm: %d", code)
	}
	var attached api.RecordResponse
	if code := arkCall(t, "POST", base+"/runs/"+runID+"/artifacts", "check:art-1", api.CreateRunArtifactRequest{
		Writer: api.Writer{AgentName: "rein", DelegatedBy: human}, Name: "worker.js",
		MediaType: "application/javascript", SHA256: digest,
	}, &attached); code != 201 {
		t.Fatalf("attach artifact: %d", code)
	}
	if code := arkCall(t, "POST", base+"/runs/"+runID+"/finish", "deploy_app:finish:"+runID,
		api.FinishRunRequest{Writer: writer, Status: "succeeded", ResultSummary: "Deployed porch at v12"}, nil); code != 200 {
		t.Fatalf("finish run: %d", code)
	}

	res := mustRun(t, a)
	if len(res.Issues) != 0 || res.Rejected != 0 || len(res.SkippedRecords) != 0 {
		t.Fatalf("sync after the server-side writes was not clean: %+v", res)
	}

	run, err := a.Store.ResolveRun(ctx, runID)
	if err != nil {
		t.Fatalf("the server-written run did not reach the client: %v", err)
	}
	if run.Status != "succeeded" || run.ResultSummary != "Deployed porch at v12" || run.FinishedAt == "" ||
		run.AgentName != "elk-cloud-deploy" || run.BaseCommitSHA != "abc123" ||
		run.MetadataJSON != `{"credits_quoted":null,"kind":"deploy"}` || run.CreatedByType != "agent" {
		t.Errorf("client's copy of the run: %+v", run)
	}
	var delegated string
	if err := a.DB.QueryRow(`SELECT delegated_by FROM actors WHERE id = ?`, run.CreatedBy).Scan(&delegated); err != nil ||
		delegated != human {
		t.Errorf("the run's agent actor on the client: delegated_by %q (%v), want %s", delegated, err, human)
	}

	arts, err := a.Store.ListArtifacts(ctx, "agent_run", runID)
	if err != nil || len(arts) != 1 {
		t.Fatalf("client's artifacts of the run: %d (%v)", len(arts), err)
	}
	if arts[0].ID != attached.Record.RecordID || arts[0].SHA256 != digest || arts[0].StorageKey == "" {
		t.Errorf("client's artifact: %+v", arts[0])
	}
	rel, err := FetchArtifact(ctx, a, arts[0])
	if err != nil {
		t.Fatalf("fetch the artifact through the client: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(a.ArkDir, rel))
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("fetched bytes %q (%v), want %q", got, err, content)
	}

	// And a second client joining later converges on the same record.
	b := newClient(t, url, repo)
	mustRun(t, b)
	fromB, err := b.Store.ResolveRun(ctx, runID)
	if err != nil || fromB.Status != "succeeded" {
		t.Errorf("a joining client's run: %+v (%v)", fromB, err)
	}
	if res := mustRun(t, a); res.PulledRecords > 1 {
		// B's own actor is the one record A may not have seen yet.
		t.Errorf("a repeat sync pulled %d records: %s", res.PulledRecords, fmt.Sprint(res))
	}
}

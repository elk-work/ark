package sync

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/config"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/servertest"
	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

// startServer boots the real sync service over temp-dir SQLite and blob
// storage, points ARK_TOKEN at its test token, and returns the server (for
// storage-side assertions) and its base URL. No external services.
func startServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	s := servertest.NewServer(t)
	ts := httptest.NewServer(s.Handler())
	s.Blobs.(*server.LocalBlobStore).BaseURL = ts.URL
	t.Cleanup(ts.Close)
	t.Setenv("ARK_TOKEN", servertest.Token)
	return s, ts.URL
}

// gitRepo creates a Git repository with one commit on main.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test Human")
	git("config", "user.email", "t@example.com")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644)
	git("add", ".")
	git("commit", "-m", "init")
	return dir
}

// newClient initializes an Ark client in a fresh Git repository. A non-empty
// repoID joins an existing Ark repository (a second client of the same
// project); a non-empty remote configures the sync target.
func newClient(t *testing.T, remote, repoID string) *app.Context {
	t.Helper()
	ctx := context.Background()
	dir := gitRepo(t)
	if _, err := app.Init(ctx, dir, repoID); err != nil {
		t.Fatalf("ark init: %v", err)
	}
	if remote != "" {
		arkDir := filepath.Join(dir, ".ark")
		cfg, err := config.Load(arkDir)
		if err != nil {
			t.Fatalf("load config: %v", err)
		}
		cfg.Remote = remote
		if err := config.Save(arkDir, cfg); err != nil {
			t.Fatalf("save config: %v", err)
		}
	}
	a, err := app.Open(ctx, dir, app.Options{})
	if err != nil {
		t.Fatalf("open ark: %v", err)
	}
	t.Cleanup(a.Close)
	return a
}

// mustRun performs a full sync and fails the test on error.
func mustRun(t *testing.T, a *app.Context) *Result {
	t.Helper()
	res, err := Run(context.Background(), a)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	return res
}

// scalar runs a single-value query against a client's local database.
func scalar[T any](t *testing.T, a *app.Context, query string, args ...any) T {
	t.Helper()
	var v T
	if err := a.DB.QueryRow(query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

// TestPushMarksAppliedAndStampsRecordRevisions: the push half of a sync
// marks every accepted mutation `applied` and stamps the server revision
// onto the record row (SetRecordRevision) before any pull runs; the pull
// half then advances the cursor to the server revision (spec §9.1, §9.2).
func TestPushMarksAppliedAndStampsRecordRevisions(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Round trip", "the body")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	comment, err := a.Store.AddComment(ctx, "task", task.ID, "first comment", "")
	if err != nil {
		t.Fatalf("add comment: %v", err)
	}

	client, err := Client(a)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	// Run's registration step, done by hand so Push and Pull are observable
	// in isolation.
	if err := client.RegisterRepo(ctx, api.RegisterRepositoryRequest{
		ID: a.Config.RepositoryID, Name: "test"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	res := &Result{}
	if err := Push(ctx, a, client, res); err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.Pushed != 2 || res.Applied != 2 || res.Rejected != 0 || res.Conflicts != 0 {
		t.Fatalf("push result: %+v", res)
	}
	if n := scalar[int](t, a, `SELECT COUNT(*) FROM mutations WHERE status = 'pending'`); n != 0 {
		t.Errorf("pending mutations after push = %d, want 0", n)
	}
	if n := scalar[int](t, a, `SELECT COUNT(*) FROM mutations WHERE status = 'applied'`); n != 2 {
		t.Errorf("applied mutations = %d, want 2", n)
	}
	// SetRecordRevision stamped both records straight from the push verdicts.
	if rev := scalar[int64](t, a, `SELECT server_revision FROM tasks WHERE id = ?`, task.ID); rev <= 0 {
		t.Errorf("task server_revision = %d, want > 0", rev)
	}
	if st := scalar[string](t, a, `SELECT sync_state FROM tasks WHERE id = ?`, task.ID); st != "synced" {
		t.Errorf("task sync_state = %q, want synced", st)
	}
	if rev := scalar[int64](t, a, `SELECT server_revision FROM comments WHERE id = ?`, comment.ID); rev <= 0 {
		t.Errorf("comment server_revision = %d, want > 0", rev)
	}

	if err := Pull(ctx, a, client, res); err != nil {
		t.Fatalf("pull: %v", err)
	}
	_, cursor, err := a.Store.SyncCursor(ctx)
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if cursor != res.ServerRevision {
		t.Errorf("cursor = %d, want server revision %d", cursor, res.ServerRevision)
	}
}

// TestSyncCursorIsIdempotentAcrossRepeatedSyncs: after a sync the local
// cursor equals the server revision, so an immediate second sync pushes
// nothing, pulls nothing, and leaves the revision unchanged (spec §9.2).
// This is the guarantee that keeps a fleet of polling clients cheap.
func TestSyncCursorIsIdempotentAcrossRepeatedSyncs(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	if _, err := a.Store.CreateTask(ctx, "Cursor", ""); err != nil {
		t.Fatalf("create task: %v", err)
	}
	first := mustRun(t, a)
	if first.PulledRecords == 0 {
		t.Fatalf("first sync pulled nothing: %+v", first)
	}
	_, cursor, err := a.Store.SyncCursor(ctx)
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if cursor != first.ServerRevision {
		t.Fatalf("cursor = %d, want %d", cursor, first.ServerRevision)
	}

	second := mustRun(t, a)
	if second.Pushed != 0 || second.PulledRecords != 0 || second.PulledTombstones != 0 {
		t.Errorf("second sync should be a no-op: %+v", second)
	}
	if second.ServerRevision != first.ServerRevision {
		t.Errorf("second sync moved revision %d -> %d", first.ServerRevision, second.ServerRevision)
	}
	if _, cursor2, _ := a.Store.SyncCursor(ctx); cursor2 != cursor {
		t.Errorf("cursor moved %d -> %d on a no-op sync", cursor, cursor2)
	}
}

// TestReplayedPushDoesNotDuplicateRecordsOrMintRevisions: when a push
// verdict is lost (crash, dropped response) the mutation stays pending and
// the next sync re-sends it. `applied_mutations` must serve the stored
// outcome — no duplicate record, no new revision (spec §9.1; this is what
// makes lost CAS races safe to replay).
func TestReplayedPushDoesNotDuplicateRecordsOrMintRevisions(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Replayed", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	first := mustRun(t, a)
	taskRev := scalar[int64](t, a, `SELECT server_revision FROM tasks WHERE id = ?`, task.ID)

	// Simulate the lost response: the server applied the mutation, but the
	// verdict never landed, so the mutation is still pending locally.
	mutID := scalar[string](t, a, `SELECT id FROM mutations WHERE record_type = 'task'`)
	if _, err := a.DB.Exec(`UPDATE mutations SET status = 'pending' WHERE id = ?`, mutID); err != nil {
		t.Fatalf("reset mutation: %v", err)
	}

	second := mustRun(t, a)
	if second.Pushed != 1 || second.Applied != 1 || second.Rejected != 0 || second.Conflicts != 0 {
		t.Fatalf("replay result: %+v", second)
	}
	if second.ServerRevision != first.ServerRevision {
		t.Errorf("replay minted a revision: %d -> %d", first.ServerRevision, second.ServerRevision)
	}
	if second.PulledRecords != 0 {
		t.Errorf("replay pulled %d records, want 0", second.PulledRecords)
	}
	if rev := scalar[int64](t, a, `SELECT server_revision FROM tasks WHERE id = ?`, task.ID); rev != taskRev {
		t.Errorf("replay corrupted record revision: %d -> %d", taskRev, rev)
	}

	// Server truth: still exactly one task record.
	raw, err := cloud.New(url)
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	resp, err := raw.Pull(ctx, api.PullRequest{RepositoryID: a.Config.RepositoryID})
	if err != nil {
		t.Fatalf("raw pull: %v", err)
	}
	tasks := 0
	for _, rec := range resp.Records {
		if rec.RecordType == "task" {
			tasks++
		}
	}
	if tasks != 1 {
		t.Errorf("server holds %d task records after replay, want 1", tasks)
	}
}

// TestSameBatchCausalityAppliesOfflineEditSequence: mutations push in
// creation order, and a client's own offline sequence — create, edit, edit,
// plus a comment on the just-created task — is causal history, not a set of
// concurrent edits for cloud-wins to eat. Pins the fix from commit c340d17.
func TestSameBatchCausalityAppliesOfflineEditSequence(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Causal", "original body")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	st := "in_progress"
	if _, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Status: &st}); err != nil {
		t.Fatalf("edit status: %v", err)
	}
	title := "Causal (renamed)"
	if _, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Title: &title}); err != nil {
		t.Fatalf("edit title: %v", err)
	}
	if _, err := a.Store.AddComment(ctx, "task", task.ID, "same-batch comment", ""); err != nil {
		t.Fatalf("comment: %v", err)
	}

	res := mustRun(t, a)
	if res.Pushed != 4 || res.Applied != 4 || res.Conflicts != 0 || res.Rejected != 0 {
		t.Fatalf("batch result: %+v (issues: %+v)", res, res.Issues)
	}

	// The pull half applied the server's truth; if the edits had been
	// cloud-winsed away, the create's values would have come back.
	got, err := a.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Title != "Causal (renamed)" || got.Status != "in_progress" {
		t.Errorf("offline edits lost: title=%q status=%q", got.Title, got.Status)
	}
	comments, err := a.Store.ListComments(ctx, "task", task.ID)
	if err != nil || len(comments) != 1 {
		t.Errorf("same-batch comment: %d comments (%v)", len(comments), err)
	}
}

// TestConcurrentTitleEditsConflictAndStoreRemoteSide: two clients editing
// the same task title from the same base revision is a genuine conflict
// (spec §10.4). The loser's mutation is marked `conflict`, the server's
// side is stored in the conflicts table (spec §10.8), and the local record
// shows the server truth after the pull.
func TestConcurrentTitleEditsConflictAndStoreRemoteSide(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Contested", "original")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	mustRun(t, a)

	b := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, b) // B pulls the task at the same base revision as A

	titleA := "Title from A"
	if _, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Title: &titleA}); err != nil {
		t.Fatalf("A edit: %v", err)
	}
	mustRun(t, a) // A wins the race

	titleB := "Title from B"
	if _, err := b.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Title: &titleB}); err != nil {
		t.Fatalf("B edit: %v", err)
	}
	res := mustRun(t, b)

	if res.Conflicts != 1 || res.Applied != 0 {
		t.Fatalf("B sync should conflict: %+v", res)
	}
	if len(res.Issues) != 1 || res.Issues[0].Status != "conflict" ||
		res.Issues[0].RecordType != "task" || res.Issues[0].RecordID != task.ID {
		t.Fatalf("conflict issue: %+v", res.Issues)
	}
	if !strings.Contains(res.Issues[0].Error, "title") {
		t.Errorf("conflict error should name the field: %q", res.Issues[0].Error)
	}
	// MarkMutation recorded the verdict.
	if n := scalar[int](t, b, `SELECT COUNT(*) FROM mutations WHERE status = 'conflict'`); n != 1 {
		t.Errorf("conflict-status mutations = %d, want 1", n)
	}
	// RecordConflict stored both sides for `ark conflict resolve`.
	remote := scalar[string](t, b, `SELECT remote_json FROM conflicts WHERE record_id = ? AND status = 'unresolved'`, task.ID)
	if !strings.Contains(remote, "Title from A") {
		t.Errorf("remote_json missing server side: %s", remote)
	}
	local := scalar[string](t, b, `SELECT local_json FROM conflicts WHERE record_id = ?`, task.ID)
	if !strings.Contains(local, "Title from B") {
		t.Errorf("local_json missing local side: %s", local)
	}
	// B's record shows the server truth; the conflict row holds B's edit.
	got, err := b.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("resolve on B: %v", err)
	}
	if got.Title != "Title from A" {
		t.Errorf("B title after conflict = %q, want server's", got.Title)
	}
}

// TestRejectedMutationIsMarkedAndSurfacedAsIssue: a mutation the server
// rejects (here: an update to a record the server has never seen) is marked
// `rejected` locally with the server's error, and surfaces as an Issue so
// the CLI can report partial success (spec §8, §9.1).
func TestRejectedMutationIsMarkedAndSurfacedAsIssue(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")

	mutID := records.NewID()
	if _, err := a.DB.Exec(`INSERT INTO mutations
		(id, repository_id, record_type, record_id, operation, base_server_revision,
		 payload_json, created_at, created_by, status)
		VALUES (?, ?, 'task', ?, 'update', 0, '{"title":"x"}', ?, ?, 'pending')`,
		mutID, a.Config.RepositoryID, records.NewID(), records.Now(), a.Store.Actor.ID); err != nil {
		t.Fatalf("queue mutation: %v", err)
	}

	res := mustRun(t, a)
	if res.Rejected != 1 || res.Applied != 0 || res.Conflicts != 0 {
		t.Fatalf("result: %+v", res)
	}
	if len(res.Issues) != 1 || res.Issues[0].Status != "rejected" ||
		!strings.Contains(res.Issues[0].Error, "record not found") {
		t.Fatalf("rejected issue: %+v", res.Issues)
	}
	if st := scalar[string](t, a, `SELECT status FROM mutations WHERE id = ?`, mutID); st != "rejected" {
		t.Errorf("mutation status = %q, want rejected", st)
	}
	if msg := scalar[string](t, a, `SELECT error_message FROM mutations WHERE id = ?`, mutID); msg == "" {
		t.Error("rejection reason not recorded on the mutation")
	}
}

// TestSecondClientConvergesWithRecordsAndActors: a second client of the
// same repository pulls the full history — records and the actor rows
// needed to render who did what (spec §9.2, §26).
func TestSecondClientConvergesWithRecordsAndActors(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Shared task", "with details")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if _, err := a.Store.AddComment(ctx, "task", task.ID, "hello from A", ""); err != nil {
		t.Fatalf("comment: %v", err)
	}
	mustRun(t, a)

	b := newClient(t, url, a.Config.RepositoryID)
	res := mustRun(t, b)
	if res.PulledRecords != 3 { // A's actor, task, comment
		t.Fatalf("B pulled %d records, want 3: %+v", res.PulledRecords, res)
	}

	tasks, err := b.Store.ListTasks(ctx, "all")
	if err != nil || len(tasks) != 1 {
		t.Fatalf("B tasks: %d (%v)", len(tasks), err)
	}
	if tasks[0].ID != task.ID || tasks[0].Title != "Shared task" || tasks[0].CreatedBy != a.Store.Actor.ID {
		t.Errorf("B task: %+v", tasks[0])
	}
	comments, err := b.Store.ListComments(ctx, "task", task.ID)
	if err != nil || len(comments) != 1 {
		t.Fatalf("B comments: %d (%v)", len(comments), err)
	}
	// A's actor row traveled with the push so B can render names.
	if n := scalar[int](t, b, `SELECT COUNT(*) FROM actors WHERE id = ?`, a.Store.Actor.ID); n != 1 {
		t.Errorf("A's actor missing on B")
	}
}

// TestDisplayNumberCollisionKeepsULIDsAuthoritative: two offline clients
// each mint task #1. The server keeps the first and renumbers the second —
// display numbers are aliases, ULIDs are authoritative (spec §6.2) — and
// both clients converge on the same numbering.
func TestDisplayNumberCollisionKeepsULIDsAuthoritative(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	b := newClient(t, url, a.Config.RepositoryID)
	ctx := context.Background()

	taskA, err := a.Store.CreateTask(ctx, "From A", "")
	if err != nil {
		t.Fatalf("A create: %v", err)
	}
	taskB, err := b.Store.CreateTask(ctx, "From B", "")
	if err != nil {
		t.Fatalf("B create: %v", err)
	}
	if taskA.Number != 1 || taskB.Number != 1 {
		t.Fatalf("both clients should mint #1: A=%d B=%d", taskA.Number, taskB.Number)
	}

	mustRun(t, a) // A's #1 lands first
	mustRun(t, b) // B's create is renumbered; pull brings both tasks
	mustRun(t, a) // A learns about B's renumbered task

	for name, c := range map[string]*app.Context{"A": a, "B": b} {
		tasks, err := c.Store.ListTasks(ctx, "all")
		if err != nil || len(tasks) != 2 {
			t.Fatalf("client %s tasks: %d (%v)", name, len(tasks), err)
		}
		byID := map[string]int64{}
		for _, task := range tasks {
			byID[task.ID] = task.Number
		}
		// Both ULIDs intact; the earlier sync kept #1, the later got #2.
		if byID[taskA.ID] != 1 || byID[taskB.ID] != 2 {
			t.Errorf("client %s numbering: %v (want %s=1, %s=2)", name, byID, taskA.ID, taskB.ID)
		}
	}
}

// TestDeletedRecordPullsAsTombstoneAndAppliesLocally: a record deleted on
// the server comes down as a tombstone and soft-deletes the local row
// inside the pull transaction (spec §9.2).
func TestDeletedRecordPullsAsTombstoneAndAppliesLocally(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Doomed", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	mustRun(t, a)

	// Another client (simulated with a raw API client) deletes the task.
	raw, err := cloud.New(url)
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	resp, err := raw.Push(ctx, api.PushRequest{
		RepositoryID: a.Config.RepositoryID,
		ClientID:     "raw-client",
		Mutations: []api.Mutation{{
			ID: records.NewID(), RecordType: "task", RecordID: task.ID,
			Operation: "delete", Payload: json.RawMessage(`{}`),
			CreatedAt: records.Now(), CreatedBy: a.Store.Actor.ID,
		}},
	})
	if err != nil || len(resp.Applied) != 1 {
		t.Fatalf("delete push: %v (%+v)", err, resp)
	}

	res := mustRun(t, a)
	if res.PulledTombstones != 1 {
		t.Fatalf("pulled tombstones = %d, want 1: %+v", res.PulledTombstones, res)
	}
	var deletedAt sql.NullString
	if err := a.DB.QueryRow(`SELECT deleted_at FROM tasks WHERE id = ?`, task.ID).Scan(&deletedAt); err != nil {
		t.Fatalf("read task: %v", err)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		t.Error("tombstone did not set deleted_at locally")
	}
	tasks, err := a.Store.ListTasks(ctx, "all")
	if err != nil || len(tasks) != 0 {
		t.Errorf("deleted task still listed: %d (%v)", len(tasks), err)
	}
}

// TestArtifactUploadSkipsStoredBlobsAndConfirms: uploadArtifacts uploads
// blobs the server lacks, calls confirm (which stamps storage_key on the
// record so the same-run pull brings it back), and skips artifacts that
// already carry a storage_key. Identical content dedups server-side: the
// signed-URL response says already_stored and no bytes move (spec §6.9).
func TestArtifactUploadSkipsStoredBlobsAndConfirms(t *testing.T) {
	srv, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "With artifact", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	src := filepath.Join(t.TempDir(), "evidence.txt")
	os.WriteFile(src, []byte("proof\n"), 0o644)
	art, err := a.Store.AddArtifact(ctx, a.ArkDir, src, "task", task.ID, "")
	if err != nil {
		t.Fatalf("add artifact: %v", err)
	}

	res := mustRun(t, a)
	if res.ArtifactsUploaded != 1 {
		t.Fatalf("uploaded = %d, want 1: %+v", res.ArtifactsUploaded, res)
	}
	// Confirm stamped storage_key server-side; the same run's pull applied it.
	got, err := a.Store.ResolveArtifact(ctx, art.ID)
	if err != nil || got.StorageKey == "" {
		t.Fatalf("artifact after sync: storage_key=%q (%v)", got.StorageKey, err)
	}
	// The blob really landed in the server's object store.
	key := "sha256/" + art.SHA256[:2] + "/" + art.SHA256
	if ok, err := srv.Blobs.Exists(ctx, key); err != nil || !ok {
		t.Errorf("blob %s not in server storage (%v)", key, err)
	}

	// Second sync: the storage_key short-circuits the upload loop entirely.
	if res2 := mustRun(t, a); res2.ArtifactsUploaded != 0 {
		t.Errorf("re-sync uploaded %d artifacts, want 0", res2.ArtifactsUploaded)
	}

	// Same content, new artifact record: the server reports already_stored
	// (no signed URL, so a byte upload would fail loudly) and confirm still
	// stamps the new record.
	src2 := filepath.Join(t.TempDir(), "evidence-copy.txt")
	os.WriteFile(src2, []byte("proof\n"), 0o644)
	art2, err := a.Store.AddArtifact(ctx, a.ArkDir, src2, "task", task.ID, "")
	if err != nil {
		t.Fatalf("add duplicate-content artifact: %v", err)
	}
	res3 := mustRun(t, a)
	// No bytes moved, so nothing is reported as uploaded — it is counted as
	// deduped instead. Conflating the two made every replica of a repository
	// look like it had pushed content it never sent.
	if res3.ArtifactsUploaded != 0 {
		t.Errorf("dedup sync: uploaded = %d, want 0 (no bytes moved): %+v", res3.ArtifactsUploaded, res3)
	}
	if res3.ArtifactsDeduped != 1 {
		t.Errorf("dedup sync: deduped = %d, want 1: %+v", res3.ArtifactsDeduped, res3)
	}
	got2, err := a.Store.ResolveArtifact(ctx, art2.ID)
	if err != nil || got2.StorageKey == "" {
		t.Errorf("deduped artifact missing storage_key: %q (%v)", got2.StorageKey, err)
	}
}

// TestSyncWithoutRemoteIsOffline: no configured remote is the offline
// condition — a records.Error of kind offline, which the CLI maps to exit
// code 6 (spec §22).
func TestSyncWithoutRemoteIsOffline(t *testing.T) {
	a := newClient(t, "", "")

	_, err := Client(a)
	if err == nil {
		t.Fatal("Client with no remote should fail")
	}
	var re *records.Error
	if !errors.As(err, &re) || re.Kind != records.KindOffline {
		t.Fatalf("error = %v, want kind offline", err)
	}
	if code := records.ExitCode(err); code != 6 {
		t.Errorf("exit code = %d, want 6", code)
	}
	if _, err := Run(context.Background(), a); records.ExitCode(err) != 6 {
		t.Errorf("Run without remote: %v, want offline (exit 6)", err)
	}
}

// An update mutation carries only the fields it changed, so it never carries
// updated_at. The server must stamp it anyway, from the mutation's own
// creation time — otherwise the stored document keeps the timestamp it was
// created with, and because that document is what clients pull and store, the
// next pull overwrites each client's correct value with the frozen one.
//
// Found in production: a task closed on 2026-07-26 read as last updated on
// 07-25 in every client, and anything deriving an event time from updated_at
// (the Elk work-record adapter does) dated the closure wrong.
func TestUpdateStampsUpdatedAtFromTheMutationNotTheCreate(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Close me", "")
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, a)

	created := task.CreatedAt
	status := "done"
	updated, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Status: &status})
	if err != nil {
		t.Fatal(err)
	}
	if updated.UpdatedAt == created {
		t.Fatal("precondition: the local edit should have moved updated_at")
	}
	localUpdatedAt := updated.UpdatedAt

	// Push the edit and pull the server's version back over it.
	mustRun(t, a)

	got, err := a.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if got.UpdatedAt == created {
		t.Errorf("updated_at was reset to created_at (%s) — the sync round trip lost the edit's timestamp", created)
	}
	// Compare as instants, not as text: time.RFC3339Nano trims trailing zeros
	// from the fractional second, so two timestamps microseconds apart sort
	// lexically in the wrong order whenever the earlier one's string is a
	// prefix of the later one's. See records.TimeCompare.
	if records.TimeBefore(got.UpdatedAt, localUpdatedAt) {
		t.Errorf("updated_at went backwards: %s is before %s", got.UpdatedAt, localUpdatedAt)
	}

	// A second client sees the same thing, since it reads the same document.
	b := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, b)
	fromB, err := b.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fromB.UpdatedAt != got.UpdatedAt {
		t.Errorf("clients disagree on updated_at: %s vs %s", fromB.UpdatedAt, got.UpdatedAt)
	}
	if fromB.UpdatedAt == created {
		t.Errorf("a joining client sees updated_at frozen at creation")
	}
}

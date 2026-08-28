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
	// A's actor, the task, the comment — and B's own actor, which B's
	// registration put on the service in the same sync. B has nothing to
	// push, and before elk-work/ark#47 that meant B's identity never reached
	// the service at all.
	if res.PulledRecords != 4 {
		t.Fatalf("B pulled %d records, want 4: %+v", res.PulledRecords, res)
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

// An update mutation must carry the revision the record is actually at on
// the server. The server's field-level merge (spec §10.4) drops every field
// whose server-side revision is newer than the mutation's base, so an update
// that claims a base of 0 has every field the record was created with
// discarded — cloud wins — while still being reported applied.
//
// The tests below are all the same shape: create the record, sync so the
// create lands on its own, then change it and sync again. The second sync is
// the one that matters, because a create and an update pushed together are
// rescued by the server's session-revision lift (engine.go, sessionRevisions)
// and never exercise the merge at all. That is why elk-work/ark#28 only
// showed up in a `.ark/` another session was syncing — the concurrent sync
// pushed the create, so the finish travelled alone.
//
// Each test asserts through a second client as well, because the local row
// is only a copy: the question is what the server's document says.

func TestFinishRunAfterTheRunHasAlreadySynced(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	run, err := a.Store.StartRun(ctx, &store.Run{AgentName: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, a) // the create reaches the server on its own

	if _, err := a.Store.FinishRun(ctx, run.ID, store.RunFinish{
		Status: "succeeded", ResultSummary: "did the thing"}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, a) // push the finish, then pull the server's copy back over it

	got, err := a.Store.ResolveRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The reported shape: the two fields absent from the create payload stuck,
	// and the one present in it reverted.
	if got.Status != "succeeded" {
		t.Errorf("status = %q, want succeeded (result_summary = %q, finished_at = %q)",
			got.Status, got.ResultSummary, got.FinishedAt)
	}
	if got.ResultSummary != "did the thing" {
		t.Errorf("result_summary = %q, want %q", got.ResultSummary, "did the thing")
	}
	if got.FinishedAt == "" {
		t.Error("finished_at is empty")
	}

	b := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, b)
	fromB, err := b.Store.ResolveRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fromB.Status != "succeeded" {
		t.Errorf("a joining client sees status %q — the finish never reached the server", fromB.Status)
	}
}

func TestCloseThreadAfterTheThreadHasAlreadySynced(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	th, err := a.Store.CreateThread(ctx, "", "Working on it")
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, a)
	if _, err := a.Store.CloseThread(ctx, th.ID); err != nil {
		t.Fatal(err)
	}
	mustRun(t, a)

	got, err := a.Store.ResolveThread(ctx, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Errorf("status = %q, want closed (closed_at = %q)", got.Status, got.ClosedAt)
	}

	b := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, b)
	fromB, err := b.Store.ResolveThread(ctx, th.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fromB.Status != "closed" {
		t.Errorf("a joining client sees status %q — the close never reached the server", fromB.Status)
	}
}

func TestMergePRAfterThePRHasAlreadySynced(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	pr, err := a.Store.CreatePR(ctx, &store.PullRequest{
		Title: "Change", BaseBranch: "main", HeadBranch: "feature",
		BaseCommitSHA: "aaa", HeadCommitSHA: "bbb"})
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, a)
	if err := a.Store.MarkPRMerged(ctx, pr, "ccc", "bbb"); err != nil {
		t.Fatal(err)
	}
	mustRun(t, a)

	got, err := a.Store.ResolvePR(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "merged" {
		t.Errorf("status = %q, want merged (merge_commit_sha = %q, merged_at = %q)",
			got.Status, got.MergeCommitSHA, got.MergedAt)
	}

	b := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, b)
	fromB, err := b.Store.ResolvePR(ctx, pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fromB.Status != "merged" {
		t.Errorf("a joining client sees status %q — the merge never reached the server", fromB.Status)
	}
}

// A record synced long ago and edited repeatedly must keep every edit: each
// update rebases on the revision the previous one produced.
func TestRepeatedEditsAfterSyncAllSurvive(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Ship it", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"in_progress", "blocked", "in_progress", "done"} {
		mustRun(t, a)
		s := status
		if _, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Status: &s}); err != nil {
			t.Fatalf("set %s: %v", s, err)
		}
	}
	mustRun(t, a)

	b := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, b)
	fromB, err := b.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fromB.Status != "done" {
		t.Errorf("status = %q, want done", fromB.Status)
	}
}

// The base revision the client stamps on an update is the record's current
// server revision, not a literal. This is the invariant the tests above
// depend on, asserted directly so a regression names itself.
func TestUpdateMutationsCarryTheRecordsServerRevision(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	run, err := a.Store.StartRun(ctx, &store.Run{AgentName: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, a)

	rev := scalar[int64](t, a, `SELECT server_revision FROM agent_runs WHERE id = ?`, run.ID)
	if rev == 0 {
		t.Fatal("precondition: the run should carry a server revision after its first sync")
	}
	if _, err := a.Store.FinishRun(ctx, run.ID, store.RunFinish{Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	base := scalar[int64](t, a, `SELECT base_server_revision FROM mutations
		WHERE record_id = ? AND operation = 'update'`, run.ID)
	if base != rev {
		t.Errorf("update mutation base_server_revision = %d, want %d (the run's revision)", base, rev)
	}
}

// A pull applies the server's document over the local row without consulting
// sync_state, so it will overwrite a change that is committed locally but not
// yet pushed. That is reachable in practice: a second session sharing one
// .ark/ pushes its own view of the queue and then pulls into the shared
// database, so its pull can land over the first session's fresh write.
//
// The overwrite is a window, not a loss. ApplyPull does not touch the
// mutations table, so the queued mutation is still there and the next full
// sync replays it. This test pins that down, because it is the difference
// between the pull being a nuisance and the pull being a data-integrity bug —
// and it is what rules the pull out as the cause of elk-work/ark#28, where
// the loss was permanent and no replay was left to make.
func TestPullOverAnUnpushedEditIsRecoveredByTheNextSync(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "Original", "body one")
	if err != nil {
		t.Fatal(err)
	}
	mustRun(t, a)

	// A second client changes a different field, so the server's revision
	// moves past a's cursor and the next pull carries the record.
	b := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, b)
	newBody := "body two"
	if _, err := b.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Body: &newBody}); err != nil {
		t.Fatal(err)
	}
	mustRun(t, b)

	// a edits locally and does not push.
	newTitle := "Renamed locally"
	if _, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Title: &newTitle}); err != nil {
		t.Fatal(err)
	}

	// Someone else pulls into the same database, without pushing a's queue.
	client, err := Client(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := Pull(ctx, a, client, &Result{}); err != nil {
		t.Fatal(err)
	}
	afterPull, err := a.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterPull.Title != "Original" {
		t.Logf("note: the pull no longer overwrites unpushed local state (title = %q)", afterPull.Title)
	}
	if n, err := a.Store.PendingMutations(ctx); err != nil || n != 1 {
		t.Fatalf("pending mutations after pull = %d (err %v), want 1 — the replay is what makes the overwrite recoverable", n, err)
	}

	// The next full sync pushes the queued edit and both values converge.
	mustRun(t, a)
	final, err := a.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Title != "Renamed locally" {
		t.Errorf("title = %q, want the local edit back", final.Title)
	}
	if final.Body != "body two" {
		t.Errorf("body = %q, want the remote edit kept", final.Body)
	}

	c := newClient(t, url, a.Config.RepositoryID)
	mustRun(t, c)
	fromC, err := c.Store.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fromC.Title != "Renamed locally" || fromC.Body != "body two" {
		t.Errorf("a joining client sees title %q, body %q", fromC.Title, fromC.Body)
	}
}

// TestSyncWithNothingToPushStillRegistersActors is elk-work/ark#47.
//
// Actors used to travel only as a field of api.PushRequest, and Push returns
// early when the mutation queue is empty. So a repository that had registered
// and synced but never pushed held no actor records on the service — not even
// the human `ark init` creates — and the RFC-0004 write routes, which resolve
// their writer against exactly those records, refused the first remote write
// into it. The complaint named `delegated_by`, a field nobody types: the CLI
// derives it from the local default actor, so from outside the command simply
// refused for no visible reason.
//
// The sequence below is the issue's reproduction. Note what is absent: no
// task, no push. Inserting one is what used to make this pass, and that
// workaround is what identified the bug.
func TestSyncWithNothingToPushStillRegistersActors(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	res := mustRun(t, a)
	if res.Pushed != 0 {
		t.Fatalf("this test is only meaningful with an empty queue: %+v", res)
	}

	client, err := Client(a)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := client.GetRecord(ctx, a.Config.RepositoryID, "actor", a.Store.Actor.ID); err != nil {
		t.Fatalf("the service does not hold this checkout's human actor after a sync: %v", err)
	}

	// And the consequence that made it worth fixing: a write route can now
	// resolve the writer this repository's own CLI delegates from.
	name := "renamed by a checkout that never pushed"
	resp, err := client.SetRepositoryMetadata(ctx, a.Config.RepositoryID, api.SetRepositoryMetadataRequest{
		Writer: api.Writer{AgentName: "ark-cli", AgentVersion: "test", DelegatedBy: a.Store.Actor.ID},
		Name:   &name,
	})
	if err != nil {
		t.Fatalf("first remote write into a synced-but-never-pushed repository: %v", err)
	}
	if resp.Repository.Name != name {
		t.Errorf("metadata after the write: %+v", resp.Repository)
	}
}

// TestRepeatedSyncDoesNotRepublishKnownActors: carrying actors on every
// registration must not make a no-op sync noisy. upsertActor mints a revision
// only for an actor it has not seen, so the second sync leaves the revision
// alone and pulls nothing — the property that keeps a fleet of polling
// clients cheap (see TestSyncCursorIsIdempotentAcrossRepeatedSyncs).
func TestRepeatedSyncDoesNotRepublishKnownActors(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")

	first := mustRun(t, a)
	second := mustRun(t, a)
	if second.ServerRevision != first.ServerRevision {
		t.Errorf("a second no-op sync moved the server revision %d -> %d",
			first.ServerRevision, second.ServerRevision)
	}
	if second.PulledRecords != 0 || second.PulledTombstones != 0 {
		t.Errorf("a second no-op sync pulled something: %+v", second)
	}
}

// TestRejectionMarksTheRecordDivergedAndCountsAsUnresolved covers the store
// half of elk-work/ark#46: a rejection leaves two durable traces — the
// mutation row, which is the forensic record of what was refused and why, and
// a mark on the record itself, which is what makes the disagreement visible
// to anyone reading the record rather than the sync log. Neither is cleared
// by the sync that produced it.
func TestRejectionMarksTheRecordDivergedAndCountsAsUnresolved(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "never reached the server", "")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	// Retire the create the way a stale `.ark/` store does: pushed once, to a
	// service that no longer holds the result, so it is never sent again.
	if _, err := a.DB.Exec(`UPDATE mutations SET status = 'applied' WHERE operation = 'create'`); err != nil {
		t.Fatalf("retire the create mutation: %v", err)
	}
	if _, err := a.Store.CloseTask(ctx, task.ID); err != nil {
		t.Fatalf("close task: %v", err)
	}

	res := mustRun(t, a)
	if res.Rejected != 1 || len(res.Issues) != 1 {
		t.Fatalf("expected exactly one rejection: %+v", res)
	}

	if st := scalar[string](t, a, `SELECT status FROM mutations WHERE id = ?`, res.Issues[0].MutationID); st != "rejected" {
		t.Errorf("mutation status = %q, want rejected", st)
	}
	if st := scalar[string](t, a, `SELECT sync_state FROM tasks WHERE id = ?`, task.ID); st != store.SyncStateDiverged {
		t.Errorf("task sync_state = %q, want %q", st, store.SyncStateDiverged)
	}
	// The local effect is deliberately kept, not rolled back: the payload
	// carries no before-image, and here the *server* is the side missing
	// data, so reverting would destroy the only copy of a real decision.
	if st := scalar[string](t, a, `SELECT status FROM tasks WHERE id = ?`, task.ID); st != "closed" {
		t.Errorf("task status = %q; the rejected change should be kept and reported, not silently undone", st)
	}

	n, err := a.Store.UnresolvedRejections(ctx)
	if err != nil {
		t.Fatalf("count rejections: %v", err)
	}
	if n != 1 {
		t.Fatalf("unresolved rejections = %d, want 1", n)
	}

	// Agreement clears it: put the record on the service and the divergence
	// is genuinely over, so the alarm has to stop.
	if _, err := a.DB.Exec(`UPDATE mutations SET status = 'pending' WHERE operation = 'create'`); err != nil {
		t.Fatalf("requeue the create: %v", err)
	}
	mustRun(t, a)
	if n, err := a.Store.UnresolvedRejections(ctx); err != nil || n != 0 {
		t.Errorf("unresolved rejections = %d (%v) after the server accepted the record, want 0", n, err)
	}
}

package sync

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

// serverRecords reads the service's own copy of a repository with no client
// bookkeeping in the way, so an assertion about what was recovered is an
// assertion about the service rather than about a replica's opinion of it.
func serverRecords(t *testing.T, url, repoID string) map[string]api.Record {
	t.Helper()
	raw, err := cloud.New(url)
	if err != nil {
		t.Fatalf("raw client: %v", err)
	}
	resp, err := raw.Pull(context.Background(), api.PullRequest{RepositoryID: repoID})
	if err != nil {
		t.Fatalf("raw pull: %v", err)
	}
	out := map[string]api.Record{}
	for _, rec := range resp.Records {
		out[store.RecordKey(rec.RecordType, rec.RecordID)] = rec
	}
	return out
}

// field reads one field out of a server-held record document.
func field(t *testing.T, rec api.Record, name string) string {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(rec.Data, &doc); err != nil {
		t.Fatalf("decode %s %s: %v", rec.RecordType, rec.RecordID, err)
	}
	var s string
	json.Unmarshal(doc[name], &s)
	return s
}

// mustRepair runs a repair and fails the test on error.
func mustRepair(t *testing.T, a *app.Context, confirm bool) *RepairResult {
	t.Helper()
	res, err := Repair(context.Background(), a, confirm)
	if err != nil {
		t.Fatalf("repair: %v", err)
	}
	return res
}

// cloneCheckout copies a checkout's whole `.ark` directory into a second Git
// repository, producing a client that holds the same records, the same
// mutation IDs and the same recorded reset.
//
// That is what "several clients repairing at once" means at its sharpest: two
// replicas replaying one acknowledged history. Distinct checkouts of the same
// repository hold disjoint mutation logs and cannot collide on an ID at all,
// so they would test the easy half. The database is closed and reopened around
// the copy because it runs in WAL mode, where the file alone is not the
// database.
func cloneCheckout(t *testing.T, a *app.Context) (*app.Context, *app.Context) {
	t.Helper()
	root := a.Root
	src := filepath.Join(root, ".ark")
	dst := filepath.Join(gitRepo(t), ".ark")
	a.Close()

	if err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		t.Fatalf("clone checkout: %v", err)
	}

	ctx := context.Background()
	reopened, err := app.Open(ctx, root, app.Options{})
	if err != nil {
		t.Fatalf("reopen original: %v", err)
	}
	t.Cleanup(reopened.Close)
	clone, err := app.Open(ctx, filepath.Dir(dst), app.Options{})
	if err != nil {
		t.Fatalf("open clone: %v", err)
	}
	t.Cleanup(clone.Close)
	return reopened, clone
}

// TestRepairRefusesWithoutARecordedHistoryReset is the gate.
//
// A replay is recovery only where the service has been found to have lost the
// history it acknowledged. Anywhere else it is one checkout re-asserting its
// records over every other client's, which is the reconciliation §9.2
// deliberately does not do — so the command has to be unavailable rather than
// merely inadvisable, and it must not have moved anything on its way to
// saying so.
func TestRepairRefusesWithoutARecordedHistoryReset(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	if _, err := a.Store.CreateTask(ctx, "healthy", "body"); err != nil {
		t.Fatalf("create task: %v", err)
	}
	before := mustRun(t, a)

	_, err := Repair(ctx, a, true)
	if err == nil {
		t.Fatal("repair ran against a repository with no recorded history reset")
	}
	if code := records.ExitCode(err); code != 2 {
		t.Errorf("refusal exits %d, want 2 (invalid input, spec §22): %v", code, err)
	}
	if !strings.Contains(err.Error(), "history reset") {
		t.Errorf("refusal does not say why: %v", err)
	}

	// Refusing has to be inert. The queue, the cursor and the acknowledged
	// mutations are all exactly as they were.
	if n, err := a.Store.PendingMutations(ctx); err != nil || n != 0 {
		t.Errorf("refusal left %d pending mutations (%v), want 0", n, err)
	}
	if _, cursor, _ := a.Store.SyncCursor(ctx); cursor != before.ServerRevision {
		t.Errorf("refusal moved the cursor to %d, want %d", cursor, before.ServerRevision)
	}
}

// TestRepairPreviewsByDefaultAndChangesNothing: --confirm is the second gate,
// and the first one a person actually meets. `ark repair push` describes the
// replay it would run and stops, because the thing it is about to do is the
// judgment §9.2 says a tool must not make on its own.
func TestRepairPreviewsByDefaultAndChangesNothing(t *testing.T) {
	a, _ := lostRepository(t)
	ctx := context.Background()

	_, cursorBefore, _ := a.Store.SyncCursor(ctx)
	res := mustRepair(t, a, false)

	if !res.DryRun {
		t.Error("a repair without --confirm reported itself as a real run")
	}
	if res.Replayable == 0 {
		t.Error("the preview counted no mutations to replay")
	}
	if res.Requeued != 0 || res.Cleared {
		t.Errorf("the preview changed something: requeued=%d cleared=%v", res.Requeued, res.Cleared)
	}
	if res.HistoryReset == nil {
		t.Fatal("the preview does not carry the reset it is proposing to repair")
	}
	if n, err := a.Store.PendingMutations(ctx); err != nil || n != 0 {
		t.Errorf("the preview queued %d mutations (%v), want 0", n, err)
	}
	if _, cursor, _ := a.Store.SyncCursor(ctx); cursor != cursorBefore {
		t.Errorf("the preview moved the cursor %d -> %d", cursorBefore, cursor)
	}
	if reset, _ := a.Store.HistoryReset(ctx); reset == nil {
		t.Error("the preview cleared the reset")
	}
}

// lostRepository builds the shape of elk-work/ark#58: a checkout that synced
// its whole history cleanly, pointed at a service that has never heard of the
// repository. It returns the checkout, with the reset already detected and
// recorded, and the new service's URL.
func lostRepository(t *testing.T) (*app.Context, string) {
	t.Helper()
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	task, err := a.Store.CreateTask(ctx, "acknowledged", "the original body")
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	mustRun(t, a)
	// Two further syncs, so the update mutations carry real base revisions
	// from the history that is about to disappear rather than zeros.
	status := "in_progress"
	if _, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Status: &status}); err != nil {
		t.Fatalf("edit status: %v", err)
	}
	if _, err := a.Store.AddComment(ctx, "task", task.ID, "a comment that was acknowledged", ""); err != nil {
		t.Fatalf("comment: %v", err)
	}
	mustRun(t, a)
	title := "acknowledged, then lost"
	if _, err := a.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Title: &title}); err != nil {
		t.Fatalf("edit title: %v", err)
	}
	mustRun(t, a)

	_, fresh := startServer(t)
	a = repointRemote(t, a, fresh)
	if res := mustRun(t, a); res.HistoryReset == nil {
		t.Fatal("the fixture did not reach the state under test: no reset detected")
	}
	return a, fresh
}

// TestRepairReplaysTheAcknowledgedHistoryIntoALostRepository is elk-work/ark#60.
//
// The client holds everything needed to rebuild: the records, and the mutation
// log that produced them with `created_by` and `created_at` intact. Every one
// of those mutations is `applied`, which is exactly why nothing re-sends them —
// a mutation leaves the queue when the service acknowledges it, and the
// service that acknowledged these no longer exists as far as the data goes.
func TestRepairReplaysTheAcknowledgedHistoryIntoALostRepository(t *testing.T) {
	a, url := lostRepository(t)
	ctx := context.Background()

	replayable, err := a.Store.ReplayableMutations(ctx)
	if err != nil {
		t.Fatalf("count replayable: %v", err)
	}
	res := mustRepair(t, a, true)

	if res.Requeued != replayable {
		t.Errorf("re-queued %d of %d replayable mutations", res.Requeued, replayable)
	}
	if res.Sync.Rejected != 0 || res.Sync.Conflicts != 0 {
		t.Fatalf("replay was not accepted whole: %+v (issues %+v)", res.Sync, res.Sync.Issues)
	}
	if !res.Cleared {
		t.Error("a clean repair did not clear the recorded reset")
	}
	if reset, _ := a.Store.HistoryReset(ctx); reset != nil {
		t.Errorf("the reset is still recorded after a clean repair: %+v", reset)
	}

	// The service holds the records again, and holds the *edited* ones: a
	// replay that landed the creates and lost the updates would leave the
	// task under its original title, which is the quiet failure this whole
	// command is built around.
	held := serverRecords(t, url, a.Config.RepositoryID)
	tasks, comments := 0, 0
	for key, rec := range held {
		switch {
		case strings.HasPrefix(key, "task/"):
			tasks++
			if got := field(t, rec, "title"); got != "acknowledged, then lost" {
				t.Errorf("recovered task title %q, want the edited one", got)
			}
			if got := field(t, rec, "status"); got != "in_progress" {
				t.Errorf("recovered task status %q, want in_progress", got)
			}
			if got := field(t, rec, "body"); got != "the original body" {
				t.Errorf("recovered task body %q, want the original", got)
			}
		case strings.HasPrefix(key, "comment/"):
			comments++
		}
	}
	if tasks != 1 || comments != 1 {
		t.Errorf("service holds %d tasks and %d comments after the repair, want 1 and 1", tasks, comments)
	}

	// And the checkout is ordinary again: nothing queued, and the next sync
	// is a quiet no-op rather than another exit 7.
	if n, err := a.Store.PendingMutations(ctx); err != nil || n != 0 {
		t.Errorf("%d mutations still queued after the repair (%v)", n, err)
	}
	if after := mustRun(t, a); after.HistoryReset != nil {
		t.Errorf("the sync after a repair still reports a reset: %+v", after.HistoryReset)
	}
}

// TestRepairRebasesEveryMutationOntoTheHistoryTheServiceIsServingNow is the
// crux of #60, and the reason it was filed rather than built alongside #59.
//
// `base_server_revision` is read against the service's per-field revisions
// (§10.4), and after a reset the recorded bases count a history nobody serves.
// This drives the repair's own steps by hand so the queue can be inspected
// between the rebase and the push: the base of every re-queued mutation has to
// be a number from the live history or zero, and never the dead number that
// was there before.
func TestRepairRebasesEveryMutationOntoTheHistoryTheServiceIsServingNow(t *testing.T) {
	a, url := lostRepository(t)
	ctx := context.Background()

	// The bases as recorded: at least one of them is a real revision from the
	// dead history, which is what makes this test worth running.
	stale := map[string]int64{}
	for _, m := range replayableMutations(t, a) {
		stale[m.ID] = m.BaseServerRevision
	}
	var deadBases int
	for _, base := range stale {
		if base > 0 {
			deadBases++
		}
	}
	if deadBases == 0 {
		t.Fatal("the fixture recorded no non-zero bases, so there is nothing to rebase")
	}

	client, err := Client(a)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := a.Store.ResetSyncCursor(ctx); err != nil {
		t.Fatalf("rewind cursor: %v", err)
	}
	if err := registerRepo(ctx, a, client, 0); err != nil {
		t.Fatalf("register: %v", err)
	}
	held, err := pull(ctx, a, client, &Result{})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if _, err := a.Store.RequeueForReplay(ctx, held); err != nil {
		t.Fatalf("requeue: %v", err)
	}

	queue, err := a.Store.PendingMutationRows(ctx)
	if err != nil {
		t.Fatalf("queue: %v", err)
	}
	if len(queue) != len(stale) {
		t.Fatalf("re-queued %d mutations, want %d", len(queue), len(stale))
	}
	for i, m := range queue {
		// Replay order is the ULID order the mutations were written in
		// (§9.1), and it is what makes a create precede its own edits.
		if i > 0 && queue[i-1].ID >= m.ID {
			t.Errorf("queue is not in ULID order at %d: %s then %s", i, queue[i-1].ID, m.ID)
		}
		want := held[store.RecordKey(m.RecordType, m.RecordID)]
		if m.Operation != "update" && m.Operation != "delete" {
			want = 0 // §8: a create's base is 0, and the server does not read it
		}
		if m.BaseServerRevision != want {
			t.Errorf("%s %s base %d, want %d (the revision the live history holds for it)",
				m.Operation, m.RecordType, m.BaseServerRevision, want)
		}
		if stale[m.ID] != 0 && m.BaseServerRevision == stale[m.ID] {
			t.Errorf("%s %s kept its dead-history base %d", m.Operation, m.RecordType, stale[m.ID])
		}
	}

	// The service lost this repository, so it holds no revision for any record
	// this checkout has mutations for, and every base above is therefore zero.
	// That is the shape a service whose counter starts fresh produces — the
	// client-driven migration named in #58 as much as an outright deletion —
	// and it is the shape the server's own session causality replays. What the
	// pull does return is the actor the registration in step 2 just wrote,
	// which no mutation touches.
	for _, m := range queue {
		if rev, ok := held[store.RecordKey(m.RecordType, m.RecordID)]; ok {
			t.Errorf("a lost repository answered with %s %s at revision %d",
				m.RecordType, m.RecordID, rev)
		}
	}

	// Push it and confirm the rebase did its job end to end: the edits
	// survive rather than losing to the cloud field by field.
	sync := &Result{Issues: []Issue{}}
	if err := Push(ctx, a, client, sync); err != nil {
		t.Fatalf("push: %v", err)
	}
	if sync.Rejected != 0 || sync.Conflicts != 0 {
		t.Fatalf("replay not accepted whole: %+v", sync.Issues)
	}
	for key, rec := range serverRecords(t, url, a.Config.RepositoryID) {
		if !strings.HasPrefix(key, "task/") {
			continue
		}
		if got := field(t, rec, "title"); got != "acknowledged, then lost" {
			t.Errorf("replayed title %q: an edit was dropped by the field merge", got)
		}
		if got := field(t, rec, "status"); got != "in_progress" {
			t.Errorf("replayed status %q: an edit was dropped by the field merge", got)
		}
	}
}

// replayableMutations reads the mutation rows a repair would replay.
func replayableMutations(t *testing.T, a *app.Context) []api.Mutation {
	t.Helper()
	rows, err := a.DB.Query(`SELECT id, record_type, record_id, operation, base_server_revision
		FROM mutations WHERE status IN ('applied', 'rejected') ORDER BY id`)
	if err != nil {
		t.Fatalf("read mutations: %v", err)
	}
	defer rows.Close()
	var out []api.Mutation
	for rows.Next() {
		var m api.Mutation
		if err := rows.Scan(&m.ID, &m.RecordType, &m.RecordID, &m.Operation, &m.BaseServerRevision); err != nil {
			t.Fatalf("scan mutation: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// TestRepairKeepsWhatTheServiceStillHolds: after a reset the service usually
// holds something — actors from its own re-registration, or work another
// client has pushed since — and a repair's first duty is not to delete it.
//
// The pull cursor is what makes that possible, and it is not free: a checkout
// at revision 18 asking a service at revision 4 for everything after 18 gets
// an empty answer forever, so a repair that did not rewind it would replay
// over a repository it had never looked at.
func TestRepairKeepsWhatTheServiceStillHolds(t *testing.T) {
	a, url := lostRepository(t)
	ctx := context.Background()

	// Another client joins the resurrected repository and does real work in
	// it before anybody repairs. Nothing about it is in a's mutation log.
	other := newClient(t, url, a.Config.RepositoryID)
	written, err := other.Store.CreateTask(ctx, "written after the loss", "")
	if err != nil {
		t.Fatalf("other create: %v", err)
	}
	mustRun(t, other)

	res := mustRepair(t, a, true)
	if !res.Cleared {
		t.Fatalf("repair did not complete: %+v", res.Sync)
	}

	held := serverRecords(t, url, a.Config.RepositoryID)
	if _, ok := held[store.RecordKey("task", written.ID)]; !ok {
		t.Error("the repair removed a record the service still held")
	}
	// And the merge ran the other way too: pulling before pushing is what
	// puts that record into this checkout.
	if _, err := a.Store.ResolveTask(ctx, written.ID); err != nil {
		t.Errorf("the repair did not pull the service's own records first: %v", err)
	}
	tasks, err := a.Store.ListTasks(ctx, "all")
	if err != nil || len(tasks) != 2 {
		t.Errorf("checkout holds %d tasks after the repair, want 2 (%v)", len(tasks), err)
	}
}

// TestTwoClientsReplayingOneHistoryConverge: `applied_mutations` is keyed by
// mutation ID and a re-queue does not change an ID, so two replicas replaying
// the same acknowledged history must land one copy of each record rather than
// two — and the second replay must mint no revision at all.
//
// The issue asked for this to be a test rather than an assumption. It is the
// property that makes a repair safe to run from more than one machine, and
// safe to re-run after one that half-finished.
func TestTwoClientsReplayingOneHistoryConverge(t *testing.T) {
	a, url := lostRepository(t)
	a, clone := cloneCheckout(t, a)

	first := mustRepair(t, a, true)
	if !first.Cleared {
		t.Fatalf("the first repair did not complete: %+v", first.Sync)
	}
	before := serverRecords(t, url, a.Config.RepositoryID)

	second := mustRepair(t, clone, true)
	if !second.Cleared {
		t.Fatalf("the second repair did not complete: %+v", second.Sync)
	}
	if second.Sync.Rejected != 0 || second.Sync.Conflicts != 0 {
		t.Errorf("the second replay was not idempotent: %+v", second.Sync.Issues)
	}
	if second.Sync.ServerRevision != first.Sync.ServerRevision {
		t.Errorf("the second replay minted revisions: %d -> %d",
			first.Sync.ServerRevision, second.Sync.ServerRevision)
	}

	after := serverRecords(t, url, a.Config.RepositoryID)
	if len(after) != len(before) {
		t.Errorf("the second replay duplicated records: %d -> %d", len(before), len(after))
	}
	for key, rec := range before {
		got, ok := after[key]
		if !ok {
			t.Errorf("%s disappeared across the second replay", key)
			continue
		}
		if got.ServerRevision != rec.ServerRevision {
			t.Errorf("%s moved revision %d -> %d across a replay that changed nothing",
				key, rec.ServerRevision, got.ServerRevision)
		}
	}
}

// TestRepairClearsTheResetOnlyWhenTheServiceTookEverything.
//
// The recorded reset is the durable evidence that a service stopped serving
// this checkout's history, and it does not self-clear — nothing the client can
// compare tells it a person has decided (§9.2). A repair is that decision, so
// a repair may retire it; a repair the service only half accepted may not,
// because the repository still needs somebody.
//
// The shape here is the ordinary multi-client one. A record another client
// authored is that client's to replay, so a checkout holding only edits to it
// gets `record not found` until the author has run its own repair — and then
// the same command, run again, finishes the job.
func TestRepairClearsTheResetOnlyWhenTheServiceTookEverything(t *testing.T) {
	_, url := startServer(t)
	author := newClient(t, url, "")
	ctx := context.Background()

	task, err := author.Store.CreateTask(ctx, "authored elsewhere", "")
	if err != nil {
		t.Fatalf("author create: %v", err)
	}
	mustRun(t, author)

	editor := newClient(t, url, author.Config.RepositoryID)
	mustRun(t, editor) // pulls the task; logs no mutation for it
	status := "done"
	if _, err := editor.Store.UpdateTask(ctx, task.ID, store.TaskEdit{Status: &status}); err != nil {
		t.Fatalf("editor edit: %v", err)
	}
	mustRun(t, editor)

	_, fresh := startServer(t)
	author = repointRemote(t, author, fresh)
	editor = repointRemote(t, editor, fresh)
	if res := mustRun(t, editor); res.HistoryReset == nil {
		t.Fatal("the editor did not detect the loss")
	}
	if res := mustRun(t, author); res.HistoryReset == nil {
		t.Fatal("the author did not detect the loss")
	}

	// The editor goes first, and cannot finish: the record it edited is not
	// there to edit, and no amount of replaying its own log will put it there.
	early := mustRepair(t, editor, true)
	if early.Sync.Rejected == 0 {
		t.Fatal("an update replayed against a record nobody has restored was accepted")
	}
	if early.Cleared {
		t.Error("a repair the service refused part of cleared the reset anyway")
	}
	if reset, _ := editor.Store.HistoryReset(ctx); reset == nil {
		t.Error("the reset is gone after an incomplete repair")
	}
	// §8: a rejected mutation is never deleted, and a repair does not get to
	// launder one. The row and the service's reason both survive the replay
	// that produced them.
	if n, err := editor.Store.UnresolvedRejections(ctx); err != nil || n == 0 {
		t.Errorf("the refusal left no unresolved rejection (%d, %v)", n, err)
	}

	// The author restores the record. Its own reset clears, because from
	// where it stands the repair completed.
	if res := mustRepair(t, author, true); !res.Cleared {
		t.Fatalf("the author's repair did not complete: %+v", res.Sync)
	}

	// And the editor's repair, run again, now finishes. Re-running is the
	// documented recovery from a partial one, so it has to work.
	late := mustRepair(t, editor, true)
	if late.Sync.Rejected != 0 {
		t.Errorf("the second repair was still refused: %+v", late.Sync.Issues)
	}
	if !late.Cleared {
		t.Error("a repair that completed did not clear the reset")
	}
	// The retry had to travel under a new ULID — §9.1 has the service keep the
	// outcome it reached for a mutation ID, so re-sending the refused one
	// would only have fetched the cached refusal back. The original row is
	// still there beside its replacement.
	if n := scalar[int64](t, editor, `SELECT COUNT(*) FROM mutations
		WHERE record_type = 'task' AND record_id = ? AND operation = 'update'`, task.ID); n != 2 {
		t.Errorf("the retried edit left %d mutation rows, want the refused one and its replacement", n)
	}
	for key, rec := range serverRecords(t, fresh, author.Config.RepositoryID) {
		if key == store.RecordKey("task", task.ID) {
			if got := field(t, rec, "status"); got != "done" {
				t.Errorf("recovered status %q, want the editor's replayed edit", got)
			}
		}
	}
}

// TestRepairReportsTheDisplayNumbersTheServiceReassigned.
//
// Numbers are display aliases and the ULID is authoritative (§6.2), so a
// repaired repository coming back numbered differently is correct behaviour
// rather than a bug. It is still the thing most likely to surprise somebody,
// because an `ark:<repo>#N` written into a commit message or a design doc is
// not stored in Ark and cannot be corrected by it. The command warns before it
// acts, and names each number that actually moved afterwards.
func TestRepairReportsTheDisplayNumbersTheServiceReassigned(t *testing.T) {
	a, url := lostRepository(t)
	ctx := context.Background()

	// Somebody else's task takes #1 on the resurrected repository first, so
	// this checkout's #1 has to be renumbered when it replays.
	other := newClient(t, url, a.Config.RepositoryID)
	if _, err := other.Store.CreateTask(ctx, "took the number", ""); err != nil {
		t.Fatalf("other create: %v", err)
	}
	mustRun(t, other)

	mine, err := a.Store.ListTasks(ctx, "all")
	if err != nil || len(mine) != 1 {
		t.Fatalf("fixture tasks: %d (%v)", len(mine), err)
	}
	res := mustRepair(t, a, true)

	if len(res.Renumbered) != 1 {
		t.Fatalf("reported %d renumberings, want 1: %+v", len(res.Renumbered), res.Renumbered)
	}
	got := res.Renumbered[0]
	if got.RecordType != "task" || got.RecordID != mine[0].ID {
		t.Errorf("renumbering names %s %s, want task %s", got.RecordType, got.RecordID, mine[0].ID)
	}
	if got.From != mine[0].Number || got.To == got.From {
		t.Errorf("renumbering reads #%d -> #%d, want it to start at #%d and move",
			got.From, got.To, mine[0].Number)
	}
	// The ULID is what did not change, which is the whole point of saying so.
	after, err := a.Store.ResolveTask(ctx, mine[0].ID)
	if err != nil {
		t.Fatalf("the renumbered task is not resolvable by its ULID: %v", err)
	}
	if after.Number != got.To {
		t.Errorf("checkout holds #%d for the renumbered task, reported #%d", after.Number, got.To)
	}
}

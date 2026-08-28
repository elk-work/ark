package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/db"
	"github.com/elk-work/ark/internal/records"
)

// divergedCheckout builds the repository from elk-work/ark#46's transcript:
// a checkout holding tasks the sync service does not have, with a close
// queued against one of them.
//
// The route there is the one Issac actually took — retiring a stale `.ark/`
// store. Its `task create` mutations had already been pushed, to a service
// that no longer holds the result, so they sit in the log as `applied` and
// will never be sent again. Marking them applied by hand is the whole of the
// setup, and it is what makes the next push's verdict a rejection rather
// than a create: the server is asked to update a record it has never seen.
func divergedCheckout(t *testing.T, url string) string {
	t.Helper()
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", url)
	ark(t, dir, "task", "create", "-t", "closed against a server that never had it")

	d, err := db.Open(filepath.Join(dir, ".ark", "ark.db"))
	if err != nil {
		t.Fatalf("open repo db: %v", err)
	}
	if _, err := d.Exec(`UPDATE mutations SET status = 'applied' WHERE operation = 'create'`); err != nil {
		t.Fatalf("retire the create mutations: %v", err)
	}
	d.Close()

	ark(t, dir, "task", "close", "1")
	return dir
}

// TestRejectedSyncCannotReportBeingInSync is elk-work/ark#46 end to end.
//
// The bug was never that a doomed mutation gets dropped — a write against a
// record the server does not hold cannot succeed, and retrying it forever
// would be worse. It was that the drop was silent *and* terminal: the local
// database kept the effect of the refused write, the queue that would have
// shown the disagreement was emptied by the refusal itself, and `ark status`
// — the one command whose entire job is to answer "am I in sync" — reported
// a clean repository. The assertion that matters is the last one.
func TestRejectedSyncCannotReportBeingInSync(t *testing.T) {
	url := startSyncServer(t)
	dir := divergedCheckout(t, url)

	out, err := arkErr(t, dir, "sync")
	if err == nil {
		t.Fatalf("a sync the server refused reported success: %s", out)
	}
	if code := records.ExitCode(err); code != 7 {
		t.Errorf("exit code %d, want 7 (spec §22 partial success): %v", code, err)
	}
	if !strings.Contains(out, "record not found") {
		t.Errorf("sync did not name the rejection: %s", out)
	}

	// The local record still reads as closed, which is the divergence.
	var task struct {
		Status    string `json:"status"`
		SyncState string `json:"sync_state"`
	}
	arkJSON(t, dir, &task, "task", "view", "1")
	if task.Status != "closed" {
		t.Errorf("local task status = %q; the rejected change was expected to be kept", task.Status)
	}
	if task.SyncState != "diverged" {
		t.Errorf("task sync_state = %q, want diverged", task.SyncState)
	}

	// And status says so. Before the fix this printed `0 pending mutations`
	// and nothing else, about a repository the server had just refused.
	var status struct {
		PendingMutations  int64 `json:"pending_mutations"`
		RejectedMutations int64 `json:"rejected_mutations"`
	}
	arkJSON(t, dir, &status, "status")
	if status.PendingMutations != 0 {
		t.Errorf("pending mutations = %d, want 0 (the rejection left the queue)", status.PendingMutations)
	}
	if status.RejectedMutations != 1 {
		t.Fatalf("rejected mutations = %d, want 1 — status is reporting a diverged repository as clean",
			status.RejectedMutations)
	}

	human := ark(t, dir, "status")
	if !strings.Contains(human, "1 rejected") || !strings.Contains(human, "diverged") {
		t.Errorf("human status hides the divergence:\n%s", human)
	}
}

// TestRepeatedSyncKeepsReportingTheDivergence: the alarm is not a one-shot.
// A rejection is terminal, so nothing about the next sync repairs it — and a
// warning that appears once and then goes quiet is the same silence with an
// extra step.
func TestRepeatedSyncKeepsReportingTheDivergence(t *testing.T) {
	url := startSyncServer(t)
	dir := divergedCheckout(t, url)

	if _, err := arkErr(t, dir, "sync"); err == nil {
		t.Fatal("first sync reported success")
	}
	// The second sync has nothing left to push, so it succeeds — the
	// transfer really is clean now. The divergence it left behind is not.
	ark(t, dir, "sync")

	var status struct {
		RejectedMutations int64 `json:"rejected_mutations"`
	}
	arkJSON(t, dir, &status, "status")
	if status.RejectedMutations != 1 {
		t.Errorf("rejected mutations = %d after a second sync, want 1", status.RejectedMutations)
	}
}

// TestAgreementClearsTheDivergence: the counterpart to the test above. The
// alarm has to be clearable, or it becomes a permanently-lit warning nobody
// reads — which is how a real signal turns back into noise. Here the record
// reaches the server by another route (a second client creates it), the pull
// brings it down, and status goes quiet on its own.
func TestAgreementClearsTheDivergence(t *testing.T) {
	url := startSyncServer(t)
	dir := divergedCheckout(t, url)
	if _, err := arkErr(t, dir, "sync"); err == nil {
		t.Fatal("first sync reported success")
	}

	// Requeue the retired create so the record genuinely lands on the
	// service, standing in for whatever repair puts it there.
	d, err := db.Open(filepath.Join(dir, ".ark", "ark.db"))
	if err != nil {
		t.Fatalf("open repo db: %v", err)
	}
	if _, err := d.Exec(`UPDATE mutations SET status = 'pending' WHERE operation = 'create'`); err != nil {
		t.Fatalf("requeue the create: %v", err)
	}
	d.Close()

	ark(t, dir, "sync")

	var status struct {
		RejectedMutations int64 `json:"rejected_mutations"`
	}
	arkJSON(t, dir, &status, "status")
	if status.RejectedMutations != 0 {
		t.Errorf("rejected mutations = %d after the record reached the server, want 0",
			status.RejectedMutations)
	}
	var task struct {
		SyncState string `json:"sync_state"`
	}
	arkJSON(t, dir, &task, "task", "view", "1")
	if task.SyncState == "diverged" {
		t.Error("task still reads as diverged after the server accepted it")
	}
}

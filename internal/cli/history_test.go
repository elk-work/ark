package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

// statusJSON is the shape of `ark status --json` these tests assert on. It is
// a stable agent interface, so it is decoded field by field rather than into
// the command's own struct — a rename would break a caller, and should break
// a test.
type statusJSON struct {
	PendingMutations  int64               `json:"pending_mutations"`
	RejectedMutations int64               `json:"rejected_mutations"`
	HistoryReset      *store.HistoryReset `json:"history_reset"`
}

func statusOf(t *testing.T, dir string) statusJSON {
	t.Helper()
	var s statusJSON
	out := ark(t, dir, "--json", "status")
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("bad status JSON: %v\n%s", err, out)
	}
	return s
}

// TestStatusTellsTheThreeSyncStatesApart is the requirement the three of
// elk-work/ark#46, #47 and #58 add up to: `ark status` has to distinguish
// nothing pending, something rejected, and the service disagreeing about what
// exists. The third is not derivable from the first two — in #58 the queue was
// empty and every mutation had been acknowledged, by a service that no longer
// held the result — and it is the one that cost a repository.
func TestStatusTellsTheThreeSyncStatesApart(t *testing.T) {
	url := startSyncServer(t)
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", url)
	ark(t, dir, "task", "create", "-t", "acknowledged, then lost")
	ark(t, dir, "sync")

	// State one: clean. Everything the client sent was accepted.
	clean := statusOf(t, dir)
	if clean.PendingMutations != 0 || clean.RejectedMutations != 0 || clean.HistoryReset != nil {
		t.Fatalf("a healthy checkout does not read as clean: %+v", clean)
	}
	if human := ark(t, dir, "status"); strings.Contains(human, "history") ||
		strings.Contains(human, "diverged") {
		t.Errorf("a healthy checkout is being warned at:\n%s", human)
	}

	// State three: the service loses the repository. Same repository ID,
	// pointed at a service that has never heard of it.
	ark(t, dir, "remote", "set", startSyncServer(t))

	out, err := arkErr(t, dir, "sync")
	if err == nil {
		t.Fatalf("syncing against a service that lost the repository reported success: %s", out)
	}
	if code := records.ExitCode(err); code != 7 {
		t.Errorf("exit code %d, want 7 (spec §22 partial success): %v", code, err)
	}
	if !strings.Contains(out, "WARNING") ||
		!strings.Contains(out, "revision counter only ever increases") {
		t.Errorf("sync did not name the history loss:\n%s", out)
	}
	if !strings.Contains(err.Error(), "reset or lost") {
		t.Errorf("the error a scripted caller sees does not say what happened: %v", err)
	}

	lost := statusOf(t, dir)
	if lost.HistoryReset == nil {
		t.Fatal("status does not report that the service lost this repository")
	}
	if lost.HistoryReset.ServerRevision >= lost.HistoryReset.LocalRevision {
		t.Errorf("history_reset does not describe a service behind us: %+v", lost.HistoryReset)
	}
	// The distinguishing property: this state is invisible to the other two
	// counters, which is exactly why it went unnoticed for six weeks.
	if lost.PendingMutations != 0 || lost.RejectedMutations != 0 {
		t.Errorf("expected the queue counters to be silent here: %+v", lost)
	}
	if human := ark(t, dir, "status"); !strings.Contains(human, "history") ||
		!strings.Contains(human, "reset or lost") {
		t.Errorf("human status hides the history loss:\n%s", human)
	}
}

package sync

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/server/repodb"
)

// held reports whether a service holds a database for a repository — the
// question an operator asks after a loss, asked of storage rather than of a
// client.
func held(t *testing.T, s *server.Server, repoID string) bool {
	t.Helper()
	err := s.Repos.View(context.Background(), repoID, func(*sql.DB) error { return nil })
	switch {
	case err == nil:
		return true
	case errors.Is(err, repodb.ErrNotFound):
		return false
	default:
		t.Fatalf("view repository: %v", err)
		return false
	}
}

// TestSyncAtAnAbsentRepositoryLeavesItAbsent is elk-work/ark#66 from the
// client's end, and it is the same shape as #58: a checkout that has synced,
// pointed at a service that no longer holds its repository.
//
// The client used to re-create the repository on the way past — registration
// runs unconditionally on every sync, with create=true — so the 404 that said
// the repository was lost survived exactly until somebody synced. It now
// carries its cursor, the service refuses, and the loss stays diagnosable.
//
// What must not change is what the client reports: the same history reset,
// the same exit 7 (§22), the same empty queue. The detection arrives from the
// registration instead of from the pull, and that is the only difference a
// person should be able to see.
func TestSyncAtAnAbsentRepositoryLeavesItAbsent(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	if _, err := a.Store.CreateTask(ctx, "acknowledged, then lost", "body"); err != nil {
		t.Fatalf("create task: %v", err)
	}
	mustRun(t, a)
	_, synced, err := a.Store.SyncCursor(ctx)
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if synced == 0 {
		t.Fatal("this test needs a checkout that has actually synced")
	}

	// The service loses the repository: same ID, a service that has never
	// heard of it.
	lost, freshURL := startServer(t)
	a = repointRemote(t, a, freshURL)
	repoID := a.Config.RepositoryID

	res := mustRun(t, a)
	if held(t, lost, repoID) {
		t.Error("the sync stood an empty repository back up — the 404 is gone and so is the diagnosis")
	}
	if res.HistoryReset == nil {
		t.Fatal("the service does not hold this repository and the sync reported nothing")
	}
	if res.HistoryReset.LocalRevision != synced {
		t.Errorf("reported local revision %d, want the cursor %d",
			res.HistoryReset.LocalRevision, synced)
	}
	if res.HistoryReset.ServerRevision != 0 {
		t.Errorf("reported server revision %d, want 0 — the service holds nothing at all",
			res.HistoryReset.ServerRevision)
	}
	// A refusal is not a reason to push or to move the cursor; the mutation
	// queue is untouched and stays that way, which is #59's property.
	if res.Pushed != 0 {
		t.Errorf("pushed %d mutations at a repository the service does not hold", res.Pushed)
	}
	if _, after, _ := a.Store.SyncCursor(ctx); after != synced {
		t.Errorf("cursor moved %d -> %d; a high-water mark cannot rewind", synced, after)
	}

	// And it keeps saying so, carrying the first detection's time, without
	// ever creating the repository on a later attempt.
	second := mustRun(t, a)
	if second.HistoryReset == nil {
		t.Fatal("the second sync went quiet about it")
	}
	if second.HistoryReset.DetectedAt != res.HistoryReset.DetectedAt {
		t.Errorf("detection time moved %q -> %q; the event is when it was first seen",
			res.HistoryReset.DetectedAt, second.HistoryReset.DetectedAt)
	}
	if held(t, lost, repoID) {
		t.Error("a later sync re-created the repository")
	}
}

// The other half of the rule: registration is still how a repository comes
// into existence. A checkout that has never synced — `ark init` here, and a
// client joining an ID it has not pulled yet — creates as it always has.
func TestSyncCreatesForACheckoutThatHasNeverSynced(t *testing.T) {
	s, url := startServer(t)
	a := newClient(t, url, "")
	repoID := a.Config.RepositoryID

	if held(t, s, repoID) {
		t.Fatal("the repository exists before the first sync")
	}
	res := mustRun(t, a)
	if res.HistoryReset != nil {
		t.Errorf("a first sync reported a history reset: %+v", res.HistoryReset)
	}
	if !held(t, s, repoID) {
		t.Fatal("the first sync did not create the repository")
	}

	// A second checkout of the same repository, joining at cursor 0 against a
	// service that already holds it.
	b := newClient(t, url, repoID)
	if res := mustRun(t, b); res.HistoryReset != nil {
		t.Errorf("a joining client reported a history reset: %+v", res.HistoryReset)
	}
}

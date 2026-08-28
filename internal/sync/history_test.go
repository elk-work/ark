package sync

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/config"
)

// repointRemote aims an already-initialized checkout at a different sync
// service, which is how a test stands in for "the service lost this
// repository": the repository ID is unchanged, and the service answering for
// it holds none of its history.
func repointRemote(t *testing.T, a *app.Context, url string) *app.Context {
	t.Helper()
	arkDir := filepath.Join(a.Root, ".ark")
	cfg, err := config.Load(arkDir)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Remote = url
	if err := config.Save(arkDir, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}
	root := a.Root
	a.Close()
	b, err := app.Open(context.Background(), root, app.Options{})
	if err != nil {
		t.Fatalf("reopen ark: %v", err)
	}
	t.Cleanup(b.Close)
	return b
}

// TestServiceRevisionGoingBackwardsIsReportedAsHistoryLoss is elk-work/ark#58.
//
// A checkout synced cleanly to revision 18 on 2026-07-13, every one of its
// seventeen mutations acknowledged. The repository was absent from the service
// afterwards and nobody found out for six weeks, because every local signal
// was correct: the queue was empty, nothing was pending, nothing was rejected.
// The client had no way to ask whether the service still agreed those records
// existed, so it could not report the one thing that had gone wrong.
//
// A revision counter only ever increases, which makes the check one
// comparison the client already holds both sides of.
func TestServiceRevisionGoingBackwardsIsReportedAsHistoryLoss(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	if _, err := a.Store.CreateTask(ctx, "acknowledged, then lost", "body"); err != nil {
		t.Fatalf("create task: %v", err)
	}
	first := mustRun(t, a)
	if first.HistoryReset != nil {
		t.Fatalf("a healthy sync reported a history reset: %+v", first.HistoryReset)
	}
	_, synced, err := a.Store.SyncCursor(ctx)
	if err != nil {
		t.Fatalf("cursor: %v", err)
	}
	if synced < 2 {
		t.Fatalf("this test needs a cursor with room below it, got %d", synced)
	}

	// The service loses the repository. Same repository ID, a service that
	// has never heard of it — which is what the client actually saw: a row
	// minted fresh, `created_at` today, revision far below its own.
	_, fresh := startServer(t)
	a = repointRemote(t, a, fresh)

	second := mustRun(t, a)
	if second.HistoryReset == nil {
		t.Fatal("the service lost the repository and the sync reported nothing")
	}
	if second.HistoryReset.LocalRevision != synced {
		t.Errorf("reported local revision %d, want the cursor %d",
			second.HistoryReset.LocalRevision, synced)
	}
	if second.HistoryReset.ServerRevision >= synced {
		t.Errorf("reported server revision %d, which is not behind %d",
			second.HistoryReset.ServerRevision, synced)
	}

	// The cursor must not have followed the service down. Assigning it is
	// what erased the evidence: after that, nothing on either side remembers
	// there had ever been a higher revision.
	if _, after, _ := a.Store.SyncCursor(ctx); after != synced {
		t.Errorf("cursor moved %d -> %d; a high-water mark cannot rewind", synced, after)
	}

	// And it is durable — the next sync still says so, carrying the original
	// detection time rather than a fresh one.
	third := mustRun(t, a)
	if third.HistoryReset == nil {
		t.Fatal("the second sync went quiet about it")
	}
	if third.HistoryReset.DetectedAt != second.HistoryReset.DetectedAt {
		t.Errorf("detection time moved %q -> %q; the event is when it was first seen",
			second.HistoryReset.DetectedAt, third.HistoryReset.DetectedAt)
	}
}

// TestHealthySyncNeverReportsHistoryLoss guards the other direction. The check
// runs on a comparison every sync makes, so a false positive would put a
// data-loss warning in front of every user — and an alarm that cries wolf is
// worth less than no alarm, which is the whole argument of #46 and #58.
func TestHealthySyncNeverReportsHistoryLoss(t *testing.T) {
	_, url := startServer(t)
	a := newClient(t, url, "")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := a.Store.CreateTask(ctx, "work", ""); err != nil {
			t.Fatalf("create task: %v", err)
		}
		if res := mustRun(t, a); res.HistoryReset != nil {
			t.Fatalf("sync %d reported a reset on a healthy service: %+v", i, res.HistoryReset)
		}
		// A no-op sync answers with the revision the cursor already holds,
		// which is equal, not below.
		if res := mustRun(t, a); res.HistoryReset != nil {
			t.Fatalf("no-op sync %d reported a reset: %+v", i, res.HistoryReset)
		}
	}

	// A second client joining sits at cursor 0 and must not read the service
	// being ahead of it as anything at all.
	b := newClient(t, url, a.Config.RepositoryID)
	if res := mustRun(t, b); res.HistoryReset != nil {
		t.Errorf("a joining client reported a reset: %+v", res.HistoryReset)
	}
}

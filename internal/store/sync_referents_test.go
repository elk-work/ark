package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// A record naming a referent this client does not hold must not fail the
// pull. Every typed pointer between records is a declared foreign key and
// `PRAGMA defer_foreign_keys` moves that check to COMMIT rather than removing
// it, so writing such a record failed the whole transaction: the good records
// in the batch rolled back with it and the cursor stayed where it was, which
// made the next pull fetch the same range and fail identically. The client was
// wedged against that repository for good, because the offending record lives
// on the service and nothing local can remove it (elk-work/ark#75).
//
// This is the issue's own reproduction. Before the fix it logged
// `constraint failed: FOREIGN KEY constraint failed (787)`, a task still
// `local`, and `cursor = 0`.
func TestPullHoldsARecordWhoseReferentHasNotArrived(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedSyncState(t, s)

	task, err := s.CreateTask(ctx, "Known type", "body")
	if err != nil {
		t.Fatal(err)
	}
	taskJSON, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}

	review := Review{
		ID:            records.NewID(),
		RepositoryID:  s.RepoID,
		PullRequestID: records.NewID(), // this client does not hold it
		State:         "approve",
		Body:          "ok",
		CreatedAt:     records.Now(),
		CreatedBy:     s.Actor.ID,
		CreatedByType: "human",
	}
	reviewJSON, err := json.Marshal(review)
	if err != nil {
		t.Fatal(err)
	}

	skips, err := s.ApplyPull(ctx, &api.PullResponse{
		Records: []api.Record{
			{RecordType: string(records.TypeTask), RecordID: task.ID, Data: taskJSON, ServerRevision: 1},
			{RecordType: string(records.TypeReview), RecordID: review.ID, Data: reviewJSON, ServerRevision: 2},
		},
		ServerRevision: 2,
	})
	if err != nil {
		t.Fatalf("a missing referent must not fail the pull: %v", err)
	}

	// It is not an unknown record type, and must not be reported as one:
	// `ark sync` tells the operator to upgrade for those.
	if len(skips) != 0 {
		t.Errorf("skips = %v, want none — a held record is not a version skew", skips)
	}

	// The good record in the same batch applied...
	got, err := s.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("the good record in the batch should have applied: %v", err)
	}
	if got.SyncState != "synced" {
		t.Errorf("task sync_state = %q, want synced", got.SyncState)
	}

	// ...and the cursor advanced, which is what ends the wedge: the next pull
	// asks for the range after this one instead of receiving the same
	// unappliable record forever.
	if _, rev, err := s.SyncCursor(ctx); err != nil {
		t.Fatal(err)
	} else if rev != 2 {
		t.Errorf("cursor = %d, want 2 — a held record must not stall the cursor", rev)
	}

	if rowExists(t, s, "reviews", review.ID) {
		t.Error("the review must not have been written; its pull request is missing")
	}

	held, err := s.DeferredRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 {
		t.Fatalf("held records = %d, want 1", len(held))
	}
	if held[0].RecordType != string(records.TypeReview) || held[0].RecordID != review.ID {
		t.Errorf("held record = %s %s, want review %s", held[0].RecordType, held[0].RecordID, review.ID)
	}
	// The reference itself is recorded, not just the fact that one failed:
	// "a review is waiting" is not actionable, "waiting for pull request X" is.
	if held[0].Field != "pull_request_id" || held[0].MissingTable != "pull_requests" ||
		held[0].MissingID != review.PullRequestID {
		t.Errorf("held reference = %s -> %s %s, want pull_request_id -> pull_requests %s",
			held[0].Field, held[0].MissingTable, held[0].MissingID, review.PullRequestID)
	}
	if n, err := s.DeferredRecordCount(ctx); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Errorf("DeferredRecordCount = %d, want 1", n)
	}
}

// A parent and its child can legitimately arrive in the same pull, and
// revision order is not dependency order — the service assigns revisions as
// writes land, and two clients' writes interleave. So a child can sit *before*
// the record it names in one response, and a single pass over the batch would
// hold it back although its parent arrived with it. Holding a record whose
// referent is right there in the same batch would be a regression, not a
// partial fix.
func TestPullAppliesAChainThatArrivesInReverseOrder(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedSyncState(t, s)

	now := records.Now()
	task := Task{ID: records.NewID(), RepositoryID: s.RepoID, Number: 1, Title: "root",
		Status: "open", CreatedAt: now, CreatedBy: s.Actor.ID, CreatedByType: "human",
		UpdatedAt: now, Version: 1}
	pr := PullRequest{ID: records.NewID(), RepositoryID: s.RepoID, Number: 1, TaskID: task.ID,
		Title: "change", Status: "open", BaseBranch: "main", HeadBranch: "work",
		CreatedAt: now, CreatedBy: s.Actor.ID, CreatedByType: "human", UpdatedAt: now, Version: 1}
	review := Review{ID: records.NewID(), RepositoryID: s.RepoID, PullRequestID: pr.ID,
		State: "approve", CreatedAt: now, CreatedBy: s.Actor.ID, CreatedByType: "human"}

	// Deepest child first: the review needs the pull request, which needs the
	// task, and all three are in this one response.
	skips, err := s.ApplyPull(ctx, &api.PullResponse{
		Records: []api.Record{
			{RecordType: string(records.TypeReview), RecordID: review.ID, Data: mustJSON(t, review), ServerRevision: 1},
			{RecordType: string(records.TypePullRequest), RecordID: pr.ID, Data: mustJSON(t, pr), ServerRevision: 2},
			{RecordType: string(records.TypeTask), RecordID: task.ID, Data: mustJSON(t, task), ServerRevision: 3},
		},
		ServerRevision: 3,
	})
	if err != nil {
		t.Fatalf("ApplyPull: %v", err)
	}
	if len(skips) != 0 {
		t.Errorf("skips = %v, want none", skips)
	}
	for _, want := range []struct{ table, id string }{
		{"tasks", task.ID}, {"pull_requests", pr.ID}, {"reviews", review.ID},
	} {
		if !rowExists(t, s, want.table, want.id) {
			t.Errorf("%s %s did not apply; a parent later in the same batch still counts as present",
				want.table, want.id)
		}
	}
	if n, err := s.DeferredRecordCount(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("held records = %d, want 0 — nothing in this batch was actually missing", n)
	}
}

// The held record must land by itself once its referent arrives, with nobody
// asked to do anything. This is why holding a copy is not optional: the cursor
// advanced past the record's revision when it was held, so the service will
// never send it again, and a skip that kept no copy would have traded a wedged
// client for one that quietly loses a record both sides hold.
func TestAHeldRecordLandsOnTheNextPullAfterItsReferentArrives(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedSyncState(t, s)

	now := records.Now()
	pr := PullRequest{ID: records.NewID(), RepositoryID: s.RepoID, Number: 1,
		Title: "change", Status: "open", BaseBranch: "main", HeadBranch: "work",
		CreatedAt: now, CreatedBy: s.Actor.ID, CreatedByType: "human", UpdatedAt: now, Version: 1}
	review := Review{ID: records.NewID(), RepositoryID: s.RepoID, PullRequestID: pr.ID,
		State: "approve", CreatedAt: now, CreatedBy: s.Actor.ID, CreatedByType: "human"}

	// The child alone: its pull request is still on another client's queue.
	if _, err := s.ApplyPull(ctx, &api.PullResponse{
		Records:        []api.Record{{RecordType: string(records.TypeReview), RecordID: review.ID, Data: mustJSON(t, review), ServerRevision: 1}},
		ServerRevision: 1,
	}); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	if n, err := s.DeferredRecordCount(ctx); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("held records after the first pull = %d, want 1", n)
	}

	// The parent, in a later pull that does not carry the child again — the
	// service cannot, the cursor is past it.
	if _, err := s.ApplyPull(ctx, &api.PullResponse{
		Records:        []api.Record{{RecordType: string(records.TypePullRequest), RecordID: pr.ID, Data: mustJSON(t, pr), ServerRevision: 2}},
		ServerRevision: 2,
	}); err != nil {
		t.Fatalf("second pull: %v", err)
	}

	if !rowExists(t, s, "reviews", review.ID) {
		t.Error("the held review should have applied once its pull request arrived")
	}
	if n, err := s.DeferredRecordCount(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("held records = %d, want 0 — the set must clear itself", n)
	}
	if _, rev, err := s.SyncCursor(ctx); err != nil {
		t.Fatal(err)
	} else if rev != 2 {
		t.Errorf("cursor = %d, want 2", rev)
	}
}

// The two reasons a record is set aside stay apart. An unknown type means this
// build is older than its service and the answer is to upgrade; a missing
// referent means a record has not arrived and the answer is to wait. Counting
// them together would report the second under a message that prescribes the
// first.
func TestAnUnknownTypeIsSkippedAndNotHeld(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedSyncState(t, s)

	skips, err := s.ApplyPull(ctx, &api.PullResponse{
		Records: []api.Record{{RecordType: "gap", RecordID: records.NewID(),
			Data: json.RawMessage(`{"title":"a type this build does not know"}`), ServerRevision: 1}},
		ServerRevision: 1,
	})
	if err != nil {
		t.Fatalf("ApplyPull: %v", err)
	}
	if skips["gap"] != 1 {
		t.Errorf("gap skips = %d, want 1", skips["gap"])
	}
	// Nothing to wait for: no later pull can make this build understand it.
	if n, err := s.DeferredRecordCount(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("held records = %d, want 0 — an unknown type is not waiting for anything", n)
	}
}

// A held record the service says is deleted is one nothing is waiting for.
// Keeping it would leave the client retrying a record that no longer exists on
// either side, and reporting it as outstanding forever.
func TestATombstoneForgetsAHeldRecord(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	seedSyncState(t, s)

	review := Review{ID: records.NewID(), RepositoryID: s.RepoID, PullRequestID: records.NewID(),
		State: "approve", CreatedAt: records.Now(), CreatedBy: s.Actor.ID, CreatedByType: "human"}
	if _, err := s.ApplyPull(ctx, &api.PullResponse{
		Records:        []api.Record{{RecordType: string(records.TypeReview), RecordID: review.ID, Data: mustJSON(t, review), ServerRevision: 1}},
		ServerRevision: 1,
	}); err != nil {
		t.Fatalf("first pull: %v", err)
	}

	if _, err := s.ApplyPull(ctx, &api.PullResponse{
		Tombstones: []api.Tombstone{{RecordType: string(records.TypeReview), RecordID: review.ID,
			DeletedAt: records.Now(), ServerRevision: 2}},
		ServerRevision: 2,
	}); err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if n, err := s.DeferredRecordCount(ctx); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Errorf("held records = %d, want 0 after the record was deleted", n)
	}
}

// pulledDocuments is one populated record of every type a pull can carry into
// a local table, with every reference it can hold set. The two tests below
// hold the code to the schema through it; adding a record type means adding an
// entry here, and both tests say so when it is missing.
func pulledDocuments(repoID string) map[string]any {
	id := records.NewID
	now := records.Now()
	return map[string]any{
		string(records.TypeTask): Task{ID: id(), RepositoryID: repoID, Number: 1, Title: "t",
			Status: "open", CreatedAt: now, CreatedBy: "a", CreatedByType: "human", UpdatedAt: now},
		string(records.TypeComment): Comment{ID: id(), RepositoryID: repoID, ParentType: "task",
			ParentID: id(), Body: "b", CreatedAt: now, CreatedBy: "a", CreatedByType: "human",
			SupersedesID: id()},
		string(records.TypeThread): Thread{ID: id(), RepositoryID: repoID, TaskID: id(),
			Title: "t", Status: "open", CreatedAt: now, CreatedBy: "a", CreatedByType: "human"},
		string(records.TypeMessage): Message{ID: id(), ThreadID: id(), Role: "user", Body: "b",
			CreatedAt: now, CreatedBy: "a", CreatedByType: "human", SupersedesID: id()},
		string(records.TypeRun): Run{ID: id(), RepositoryID: repoID, TaskID: id(), ThreadID: id(),
			AgentName: "agent", Status: "running", CreatedAt: now, CreatedBy: "a", CreatedByType: "human"},
		string(records.TypePullRequest): PullRequest{ID: id(), RepositoryID: repoID, Number: 1,
			TaskID: id(), Title: "p", Status: "open", BaseBranch: "main", HeadBranch: "work",
			CreatedAt: now, CreatedBy: "a", CreatedByType: "human", UpdatedAt: now},
		string(records.TypeReview): Review{ID: id(), RepositoryID: repoID, PullRequestID: id(),
			State: "approve", CreatedAt: now, CreatedBy: "a", CreatedByType: "human"},
		string(records.TypeArtifact): Artifact{ID: id(), RepositoryID: repoID, ParentType: "task",
			ParentID: id(), Name: "n", MediaType: "text/plain", SizeBytes: 1, SHA256: "x",
			CreatedAt: now, CreatedBy: "a", CreatedByType: "human"},
		string(records.TypePromotion): Promotion{ID: id(), RepositoryID: repoID, Environment: "prod",
			PullRequestID: id(), ActivatedAt: now, CreatedAt: now, CreatedBy: "a", CreatedByType: "human"},
		"actor": api.Actor{ID: id(), Type: "human", Name: "n", CreatedAt: now},
	}
}

// notPulledTables declare foreign keys but never receive a pulled record:
// they are this checkout's own bookkeeping, written only by local code that
// already holds what it points at.
var notPulledTables = map[string]bool{
	"mutations":  true,
	"sync_state": true,
	"conflicts":  true,
}

// Every foreign key in the schema is checked before a pull writes the record
// that carries it — and this is the test that keeps that true as the schema
// grows, because the alternative failure is silent. The check reads its
// references from the schema rather than from a list in Go, so a migration
// that adds one to an already-pulled table is covered the moment it ships. A
// migration that adds a whole new pulled *table* is not, and that is what this
// catches: the table is neither known to pulledTable nor named as local-only,
// so its references would go unchecked and its records would fail the commit
// exactly as elk-work/ark#75 did.
func TestEveryForeignKeyOnAPulledTableIsChecked(t *testing.T) {
	s, _ := newTestStore(t)

	pulled := map[string]bool{}
	for recordType := range pulledDocuments(s.RepoID) {
		table, ok := pulledTable(recordType)
		if !ok {
			t.Errorf("pulledTable does not know %q, so records of that type are never checked", recordType)
			continue
		}
		pulled[table] = true
	}

	rows, err := s.DB.Query(`SELECT name FROM sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	check := newRefCheck()
	for _, table := range tables {
		fks, err := foreignKeysOf(tx, table, check)
		if err != nil {
			t.Fatalf("read foreign keys of %s: %v", table, err)
		}
		if len(fks) == 0 || pulled[table] || notPulledTables[table] {
			continue
		}
		t.Errorf("table %s declares %d foreign key(s) and no pulled record type maps to it: "+
			"add its record type to pulledDocuments (and to pulledTable), or name it in "+
			"notPulledTables if a pull never writes it", table, len(fks))
	}
}

// The reference check finds a record's references by column name, which is
// what lets one check serve every record type instead of nine hand-written
// lists that drift. That holds only while the pulled document names its
// references the way the table does, so this asserts it: a foreign key column
// with no matching JSON field is a reference that would be read as absent —
// silently unchecked, and back to a failed commit.
func TestPulledDocumentsNameTheirReferencesByColumn(t *testing.T) {
	s, _ := newTestStore(t)

	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	check := newRefCheck()

	for recordType, doc := range pulledDocuments(s.RepoID) {
		table, ok := pulledTable(recordType)
		if !ok {
			continue // reported by TestEveryForeignKeyOnAPulledTableIsChecked
		}
		fks, err := foreignKeysOf(tx, table, check)
		if err != nil {
			t.Fatalf("read foreign keys of %s: %v", table, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(mustJSON(t, doc), &fields); err != nil {
			t.Fatalf("%s: %v", recordType, err)
		}
		for _, fk := range fks {
			id, ok := fields[fk.Column].(string)
			if !ok || id == "" {
				t.Errorf("a pulled %s does not carry %s.%s as a string field: the reference to "+
					"%s would never be checked. Give the struct that field with a json tag of %q, "+
					"and populate it in pulledDocuments",
					recordType, table, fk.Column, fk.Table, fk.Column)
			}
		}
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func rowExists(t *testing.T, s *Store, table, id string) bool {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n > 0
}

package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// A client must tolerate record types it does not know. Server and client
// versions skew by design — a repository can be written by a newer build, or
// carry a type this build has not implemented — and if one unrecognized
// record could fail a pull, that repository would be permanently unsyncable
// for that client.
//
// This is not hypothetical. The sync service holds a `gap` record in the
// clawfight repository; `records.go` has that type commented out as reserved.
// Ark deliberately does not implement it (it is Watch's vocabulary — problem,
// severity, affected_users, evidence_query — not a work record, and principle
// 005 says no primitives without demonstrated need). So the guarantee that
// matters is this one: unknown types are skipped, everything else applies,
// and the cursor still advances.
func TestPullSkipsUnknownRecordTypesAndKeepsGoing(t *testing.T) {
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

	// A real `gap` payload, trimmed — the shape actually on the service.
	gap := map[string]any{
		"title":                    "Recover Anthropic latency after load spikes",
		"problem":                  "p95 exceeds the 20s budget during load spikes.",
		"severity":                 "high",
		"affected_users":           12,
		"affected_operations":      []string{"anthropic.messages"},
		"evidence_query":           map[string]any{"expectation_id": "demo-exp"},
		"representative_incidents": []any{},
	}
	gapJSON, err := json.Marshal(gap)
	if err != nil {
		t.Fatal(err)
	}

	resp := &api.PullResponse{
		Records: []api.Record{
			{RecordType: "gap", RecordID: records.NewID(), Data: gapJSON, ServerRevision: 1},
			{RecordType: string(records.TypeTask), RecordID: task.ID, Data: taskJSON, ServerRevision: 2},
			{RecordType: "some_future_thing", RecordID: records.NewID(), Data: json.RawMessage(`{"x":1}`), ServerRevision: 3},
			{RecordType: "gap", RecordID: records.NewID(), Data: gapJSON, ServerRevision: 4},
		},
		ServerRevision: 4,
	}

	skips, err := s.ApplyPull(ctx, resp)
	if err != nil {
		t.Fatalf("an unknown record type must not fail the pull: %v", err)
	}

	if got := skips["gap"]; got != 2 {
		t.Errorf("gap skips = %d, want 2", got)
	}
	if got := skips["some_future_thing"]; got != 1 {
		t.Errorf("some_future_thing skips = %d, want 1", got)
	}
	if _, ok := skips[string(records.TypeTask)]; ok {
		t.Error("a known type must not be reported as skipped")
	}

	// The known record in the same batch still applied...
	got, err := s.ResolveTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("the known record should have applied: %v", err)
	}
	if got.SyncState != "synced" {
		t.Errorf("sync_state = %q, want synced", got.SyncState)
	}

	// ...and the cursor advanced, so the next pull does not re-fetch the
	// records it could not use. Refusing to advance would wedge the client.
	_, rev, err := s.SyncCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rev != 4 {
		t.Errorf("cursor = %d, want 4 — an unknown record must not stall the cursor", rev)
	}
}

// A clean pull reports no skips at all, so an operator only ever sees the
// message when something was genuinely dropped.
func TestPullReportsNoSkipsWhenEverythingIsUnderstood(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "Known", "")
	if err != nil {
		t.Fatal(err)
	}
	taskJSON, _ := json.Marshal(task)

	skips, err := s.ApplyPull(ctx, &api.PullResponse{
		Records:        []api.Record{{RecordType: string(records.TypeTask), RecordID: task.ID, Data: taskJSON, ServerRevision: 1}},
		ServerRevision: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(skips) != 0 {
		t.Errorf("skips = %v, want none", skips)
	}
}

// Malformed data for a type we DO know is a real error — the tolerance above
// is for unknown types, not for corrupt payloads, and conflating the two
// would hide genuine breakage.
func TestPullStillFailsOnMalformedKnownRecords(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	_, err := s.ApplyPull(ctx, &api.PullResponse{
		Records: []api.Record{{
			RecordType:     string(records.TypeTask),
			RecordID:       records.NewID(),
			Data:           json.RawMessage(`{"title": ["not", "a", "string"]}`),
			ServerRevision: 1,
		}},
		ServerRevision: 1,
	})
	if err == nil {
		t.Fatal("a corrupt payload for a known type must fail the pull")
	}
}

// seedSyncState creates the row `ark init` normally writes, so a test can
// assert on the pull cursor.
func seedSyncState(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB.Exec(`INSERT INTO sync_state (repository_id, client_id) VALUES (?, ?)`,
		s.RepoID, records.NewID()); err != nil {
		t.Fatalf("seed sync_state: %v", err)
	}
}

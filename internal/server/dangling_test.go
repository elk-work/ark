package server

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

// danglingRow is one entry in the service's ledger of references it accepted
// without holding what they name.
type danglingRow struct {
	recordType string
	recordID   string
	field      string
	parentType string
	parentID   string
	mutationID string
}

func queryDangling(t *testing.T, s *Server, where string) []danglingRow {
	t.Helper()
	var out []danglingRow
	err := s.Repos.View(context.Background(), repoID, func(db *sql.DB) error {
		rows, err := db.Query(`SELECT d.record_type, d.record_id, d.field,
			d.parent_type, d.parent_id, d.mutation_id FROM dangling_references d ` +
			where + ` ORDER BY d.record_type, d.record_id, d.field`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r danglingRow
			if err := rows.Scan(&r.recordType, &r.recordID, &r.field,
				&r.parentType, &r.parentID, &r.mutationID); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read dangling_references: %v", err)
	}
	return out
}

// outstandingDangling asks the question docs/v1-spec.md §9.1 defines: which
// of the references this repository is serving still resolve to nothing.
func outstandingDangling(t *testing.T, s *Server) []danglingRow {
	t.Helper()
	return queryDangling(t, s, `WHERE NOT EXISTS (SELECT 1 FROM records r
		WHERE r.record_type = d.parent_type AND r.record_id = d.parent_id)`)
}

// ledgerDangling is every entry ever recorded, resolved or not.
func ledgerDangling(t *testing.T, s *Server) []danglingRow {
	t.Helper()
	return queryDangling(t, s, "")
}

const absentTask = "01KXEHFQHP0000000000000000"

// commentOnAbsentTask is the reproduction from elk-work/ark#56: one push in
// which the service declares a task absent and stores a comment on that same
// task.
func commentOnAbsentTask(t *testing.T, s *Server) api.PushResponse {
	t.Helper()
	return push(t, s,
		mut("m1", "task", absentTask, "update", 0, `{"status":"done"}`),
		mut("m2", "comment", "c1", "create", 0, fmt.Sprintf(
			`{"id":"c1","parent_type":"task","parent_id":%q,"body":"looks fixed"}`, absentTask)),
	)
}

// TestPushRecordsTheOrphanItAcceptsWhileRejectingTheParent pins the asymmetry
// the issue is about, and the fact that it is no longer silent. applyUpdate
// refuses a record the service does not hold; applyCreate stores a child of
// that same record in the same transaction. Both halves stay — rejecting the
// child would turn a cross-client ordering skew into a permanent loss — but
// the orphan is now on the record as one.
func TestPushRecordsTheOrphanItAcceptsWhileRejectingTheParent(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	resp := commentOnAbsentTask(t, s)
	if len(resp.Rejected) != 1 || resp.Rejected[0].Error != "record not found" {
		t.Fatalf("want one `record not found` rejection, got %+v", resp.Rejected)
	}
	if len(resp.Applied) != 1 || resp.Applied[0].MutationID != "m2" {
		t.Fatalf("want the comment applied, got %+v", resp.Applied)
	}

	got := outstandingDangling(t, s)
	want := []danglingRow{{"comment", "c1", "parent_id", "task", absentTask, "m2"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("outstanding dangling references = %+v, want %+v", got, want)
	}
}

// TestDanglingReferenceClearsWhenTheParentArrives covers the ordinary ending:
// the other client's push lands and the skew is over. The service answers by
// comparison rather than by a stamp, so a parent arriving through any create
// path clears it — and the entry survives as evidence that the repository saw
// the skew at all.
func TestDanglingReferenceClearsWhenTheParentArrives(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)
	commentOnAbsentTask(t, s)

	if got := outstandingDangling(t, s); len(got) != 1 {
		t.Fatalf("before the parent lands: %+v", got)
	}
	push(t, s, mut("m3", "task", absentTask, "create", 0,
		fmt.Sprintf(`{"id":%q,"number":1,"title":"A","status":"open"}`, absentTask)))

	if got := outstandingDangling(t, s); len(got) != 0 {
		t.Errorf("still outstanding after the parent landed: %+v", got)
	}
	if got := ledgerDangling(t, s); len(got) != 1 {
		t.Errorf("ledger = %+v, want the entry kept as history", got)
	}
}

// TestDanglingReferenceCoverage walks every pointer in recordReferences —
// checked against migrations/0001_initial.sql, not against the shorter list
// in the issue — plus the cases that must record nothing.
func TestDanglingReferenceCoverage(t *testing.T) {
	// seed is a record to create before the one under test, so a case can
	// distinguish a reference that resolves from one that does not.
	type seed struct{ recordType, id string }

	tests := []struct {
		name       string
		seeds      []seed
		recordType string
		recordID   string
		payload    string
		want       []danglingRow
	}{
		{
			name:       "comment on an absent task",
			recordType: "comment", recordID: "c1",
			payload: `{"id":"c1","parent_type":"task","parent_id":"absent-task","body":"b"}`,
			want:    []danglingRow{{"comment", "c1", "parent_id", "task", "absent-task", "m"}},
		},
		{
			name:       "comment on an absent run: the parent type comes from the payload",
			recordType: "comment", recordID: "c1",
			payload: `{"id":"c1","parent_type":"agent_run","parent_id":"absent-run","body":"b"}`,
			want:    []danglingRow{{"comment", "c1", "parent_id", "agent_run", "absent-run", "m"}},
		},
		{
			name:  "comment superseding an absent comment",
			seeds: []seed{{"task", "t1"}},
			// The correction pointer dangles while the parent resolves, so
			// exactly one entry: the ledger is per field, not per record.
			recordType: "comment", recordID: "c2",
			payload: `{"id":"c2","parent_type":"task","parent_id":"t1","supersedes_id":"absent-comment","body":"b"}`,
			want:    []danglingRow{{"comment", "c2", "supersedes_id", "comment", "absent-comment", "m"}},
		},
		{
			name:       "thread on an absent task",
			recordType: "agent_thread", recordID: "th1",
			payload: `{"id":"th1","task_id":"absent-task","title":"T","status":"open"}`,
			want:    []danglingRow{{"agent_thread", "th1", "task_id", "task", "absent-task", "m"}},
		},
		{
			name:       "message in an absent thread",
			recordType: "thread_message", recordID: "tm1",
			payload: `{"id":"tm1","thread_id":"absent-thread","role":"agent","body":"b"}`,
			want:    []danglingRow{{"thread_message", "tm1", "thread_id", "agent_thread", "absent-thread", "m"}},
		},
		{
			name:       "message superseding an absent message",
			seeds:      []seed{{"agent_thread", "th1"}},
			recordType: "thread_message", recordID: "tm2",
			payload: `{"id":"tm2","thread_id":"th1","supersedes_id":"absent-message","role":"agent","body":"b"}`,
			want:    []danglingRow{{"thread_message", "tm2", "supersedes_id", "thread_message", "absent-message", "m"}},
		},
		{
			name:       "run with both references absent",
			recordType: "agent_run", recordID: "r1",
			payload: `{"id":"r1","task_id":"absent-task","thread_id":"absent-thread","agent_name":"a"}`,
			want: []danglingRow{
				{"agent_run", "r1", "task_id", "task", "absent-task", "m"},
				{"agent_run", "r1", "thread_id", "agent_thread", "absent-thread", "m"},
			},
		},
		{
			name:       "pull request on an absent task",
			recordType: "pull_request", recordID: "pr1",
			payload: `{"id":"pr1","number":1,"task_id":"absent-task","title":"T","status":"open"}`,
			want:    []danglingRow{{"pull_request", "pr1", "task_id", "task", "absent-task", "m"}},
		},
		{
			name:       "review on an absent pull request",
			recordType: "review", recordID: "rv1",
			payload: `{"id":"rv1","pull_request_id":"absent-pr","state":"approve"}`,
			want:    []danglingRow{{"review", "rv1", "pull_request_id", "pull_request", "absent-pr", "m"}},
		},
		{
			name:       "artifact on an absent review",
			recordType: "artifact", recordID: "a1",
			payload: `{"id":"a1","parent_type":"review","parent_id":"absent-review","name":"log","size_bytes":1}`,
			want:    []danglingRow{{"artifact", "a1", "parent_id", "review", "absent-review", "m"}},
		},
		{
			name:       "promotion of an absent pull request",
			recordType: "promotion", recordID: "p1",
			payload: `{"id":"p1","environment":"prod","pull_request_id":"absent-pr","activated_at":"2026-08-28T00:00:00Z"}`,
			want:    []danglingRow{{"promotion", "p1", "pull_request_id", "pull_request", "absent-pr", "m"}},
		},
		{
			name:       "a reference the service holds records nothing",
			seeds:      []seed{{"task", "t1"}},
			recordType: "comment", recordID: "c1",
			payload: `{"id":"c1","parent_type":"task","parent_id":"t1","body":"b"}`,
		},
		{
			name:       "a nullable reference left unset records nothing",
			recordType: "agent_run", recordID: "r1",
			payload: `{"id":"r1","agent_name":"a","task_id":null,"thread_id":""}`,
		},
		{
			name:       "a record type with no references records nothing",
			recordType: "task", recordID: "t1",
			payload: `{"id":"t1","number":1,"title":"A","status":"open"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(t)
			registerRepo(t, s)
			for _, sd := range tt.seeds {
				r := push(t, s, mut("seed-"+sd.id, sd.recordType, sd.id, "create", 0,
					fmt.Sprintf(`{"id":%q}`, sd.id)))
				if len(r.Applied) != 1 {
					t.Fatalf("seed %s/%s: %+v", sd.recordType, sd.id, r)
				}
			}

			r := push(t, s, mut("m", tt.recordType, tt.recordID, "create", 0, tt.payload))
			if len(r.Applied) != 1 {
				t.Fatalf("create %s: %+v", tt.recordType, r)
			}
			if got := outstandingDangling(t, s); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("outstanding dangling references = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestDanglingReferenceIsRecordedOnce keeps the ledger from double-counting a
// replayed push: the mutation is idempotent, and so is its entry.
func TestDanglingReferenceIsRecordedOnce(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	commentOnAbsentTask(t, s)
	commentOnAbsentTask(t, s)

	if got := ledgerDangling(t, s); len(got) != 1 {
		t.Errorf("ledger after a replayed push = %+v, want one entry", got)
	}
}

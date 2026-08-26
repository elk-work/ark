package cli

import (
	"path/filepath"
	"testing"

	"github.com/elk-work/ark/internal/db"
	"github.com/elk-work/ark/internal/records"
)

// TestConflictListOrdersByULIDNotTimestampText pins `ark conflict list` to the
// ULID. created_at is RFC3339Nano text and SQLite compares TEXT byte by byte,
// so the trimmed fractional second of an earlier write sorts after a later
// one — see records.TimeCompare. The pair below is that trap, constructed:
// ".1724Z" is the earlier instant and ".17249Z" the later, and 'Z' (0x5A)
// beats '9' (0x39), so byte ordering returns them backwards.
func TestConflictListOrdersByULIDNotTimestampText(t *testing.T) {
	const (
		earlierInstant = "2026-08-26T07:00:00.1724Z"
		laterInstant   = "2026-08-26T07:00:00.17249Z"
	)
	if !records.TimeBefore(earlierInstant, laterInstant) || earlierInstant <= laterInstant {
		t.Fatalf("%q/%q is no longer the trap it was written to be", earlierInstant, laterInstant)
	}

	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "task", "create", "-t", "one")
	ark(t, dir, "task", "create", "-t", "two")

	d, err := db.Open(filepath.Join(dir, ".ark", "ark.db"))
	if err != nil {
		t.Fatalf("open repo db: %v", err)
	}
	defer d.Close()

	rows, err := d.Query(`SELECT id FROM mutations ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read mutations: %v", err)
	}
	var mutationIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		mutationIDs = append(mutationIDs, id)
	}
	rows.Close()
	if len(mutationIDs) != 2 {
		t.Fatalf("mutations = %d, want 2", len(mutationIDs))
	}

	// One conflict per mutation, recorded in order, stamped with the trap.
	want := []string{records.NewID(), records.NewID()}
	stamps := []string{earlierInstant, laterInstant}
	for i, id := range want {
		if _, err := d.Exec(`INSERT INTO conflicts
			(id, record_type, record_id, mutation_id, base_json, local_json, remote_json,
			 status, created_at)
			VALUES (?, 'task', ?, ?, '', '{}', '{}', 'unresolved', ?)`,
			id, "01TESTRECORD00000000000000", mutationIDs[i], stamps[i]); err != nil {
			t.Fatalf("record conflict: %v", err)
		}
	}
	d.Close()

	var listed []struct {
		ID string `json:"id"`
	}
	arkJSON(t, dir, &listed, "conflict", "list")
	got := make([]string, len(listed))
	for i, c := range listed {
		got[i] = c.ID
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("conflict list order = %v, want %v", got, want)
	}
}

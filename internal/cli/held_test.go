package cli

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/db"
	"github.com/elk-work/ark/internal/records"
)

// heldJSON is the `ark status --json` shape these tests assert on. Decoded
// field by field rather than into the command's own struct, for the reason
// statusJSON gives: it is a stable agent interface, so a rename should break a
// test rather than a caller.
type heldJSON struct {
	HeldRecords       int64 `json:"held_records"`
	RejectedMutations int64 `json:"rejected_mutations"`
	PendingMutations  int64 `json:"pending_mutations"`
}

// holdARecord puts one row in the ledger #75 writes when a pulled record names
// a referent this checkout does not hold.
//
// Seeded rather than provoked. That the client *holds* such a record — and
// applies it once the referent lands — is #75's property and is tested at the
// store layer, where it belongs. What is under test here is only whether
// `ark status` says so, and seeding the ledger exercises exactly that without
// pretending to re-prove the layer beneath it.
func holdARecord(t *testing.T, dir string) {
	t.Helper()
	d, err := db.Open(filepath.Join(dir, ".ark", "ark.db"))
	if err != nil {
		t.Fatalf("open repo db: %v", err)
	}
	defer d.Close()
	now := records.Now()
	if _, err := d.Exec(
		`INSERT INTO deferred_records
		   (record_type, record_id, repository_id, data_json, server_revision,
		    field, missing_table, missing_id, first_seen_at, last_seen_at)
		 VALUES ('review', ?, (SELECT id FROM repositories LIMIT 1), '{}', 4,
		         'pull_request_id', 'pull_requests', ?, ?, ?)`,
		records.NewID(), records.NewID(), now, now,
	); err != nil {
		t.Fatalf("seed a held record: %v", err)
	}
}

// TestStatusNamesRecordsItIsHolding is elk-work/ark#89.
//
// #75 stopped a record whose referent has not arrived from wedging the pull:
// the batch and the cursor go through, and the record is held and applied
// later. Nothing then told the operator it was happening, and the set is not
// guaranteed to drain — the service accepts a child whose parent it does not
// hold by decision (#56), so a held record is the client-side face of a
// dangling reference the service has already recorded (#77). If the parent is
// never coming, this line is the only thing that will ever say so.
func TestStatusNamesRecordsItIsHolding(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	holdARecord(t, dir)

	var s heldJSON
	out := ark(t, dir, "--json", "status")
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("bad status JSON: %v\n%s", err, out)
	}
	if s.HeldRecords != 1 {
		t.Errorf("held_records = %d, want 1\n%s", s.HeldRecords, out)
	}

	// Held is not diverged. A rejection is terminal and needs a person; this
	// resolves itself, and reporting it as divergence would spend the
	// attention the divergence lines exist to get.
	if s.RejectedMutations != 0 {
		t.Errorf("holding a record reported %d rejected mutation(s)", s.RejectedMutations)
	}

	human := ark(t, dir, "status")
	if !strings.Contains(human, "held") {
		t.Errorf("status does not mention the held record:\n%s", human)
	}
	if strings.Contains(human, "diverged") {
		t.Errorf("a held record is being reported as divergence:\n%s", human)
	}
}

// The counterpart, and the one that keeps the line from becoming noise: a
// checkout holding nothing must not mention holding, and `held_records` is
// omitempty so it does not appear in the JSON at all.
func TestStatusStaysQuietWhenHoldingNothing(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	out := ark(t, dir, "--json", "status")
	if strings.Contains(out, "held_records") {
		t.Errorf("held_records present for a checkout holding nothing:\n%s", out)
	}
	if human := ark(t, dir, "status"); strings.Contains(human, "held") {
		t.Errorf("a clean checkout is being told it is holding something:\n%s", human)
	}
}

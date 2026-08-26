package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

// TestResolveWriterPicksTheFirstAgentRegistration pins which actor a remote
// write is attributed to when a repository holds more than one record for an
// agent name — which sync can leave behind, since a client registers agents
// locally too.
//
// The lookup is LIMIT 1, so its ordering does not shuffle a list: it decides
// an identity. It orders by record_id, the client's immutable ULID. Not by
// created_at, which the service writes as RFC3339Nano text and SQLite
// compares byte by byte, so the trimmed fractional second of the earlier
// registration sorts after the later one (records.TimeCompare). The pair
// below is that trap: ".1724Z" is the earlier instant, ".17249Z" the later,
// and 'Z' (0x5A) loses to '9' (0x39).
func TestResolveWriterPicksTheFirstAgentRegistration(t *testing.T) {
	const (
		earlierInstant = "2026-08-26T07:00:00.1724Z"
		laterInstant   = "2026-08-26T07:00:00.17249Z"
	)
	if !records.TimeBefore(earlierInstant, laterInstant) || earlierInstant <= laterInstant {
		t.Fatalf("%q/%q is no longer the trap it was written to be", earlierInstant, laterInstant)
	}

	s := writeServer(t)
	ctx := context.Background()

	// The first write registers the agent; the record it authors names it.
	var first store.Task
	if err := json.Unmarshal(createTask(t, s, "k1", "first").Record.Data, &first); err != nil {
		t.Fatalf("decode first task: %v", err)
	}
	firstActor := first.CreatedBy
	if firstActor == "" {
		t.Fatal("first task has no created_by")
	}

	// A duplicate registration of the same agent name, minted later, so its
	// ULID is higher.
	secondActor := records.NewID()
	if secondActor <= firstActor {
		t.Fatalf("second actor id %s does not follow %s", secondActor, firstActor)
	}
	err := s.Repos.Update(ctx, repoID, false, func(tx *sql.Tx) error {
		if err := upsertActor(ctx, tx, api.Actor{
			ID: secondActor, Type: string(records.ActorAgent), Name: agent,
			AgentName: agent, DelegatedBy: humanID, CreatedAt: records.Now(),
		}); err != nil {
			return err
		}
		for i, id := range []string{firstActor, secondActor} {
			stamp := []string{earlierInstant, laterInstant}[i]
			if _, err := tx.ExecContext(ctx, `UPDATE records SET created_at = ?
				WHERE record_type = 'actor' AND record_id = ?`, stamp, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed duplicate actor: %v", err)
	}

	var second store.Task
	if err := json.Unmarshal(createTask(t, s, "k2", "second").Record.Data, &second); err != nil {
		t.Fatalf("decode second task: %v", err)
	}
	if second.CreatedBy != firstActor {
		t.Fatalf("second task attributed to %s, want the first registration %s",
			second.CreatedBy, firstActor)
	}
}

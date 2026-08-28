package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// The tests here are about one question: which actor does `--agent <name>`
// write as, in a repository more than one person syncs. Before
// elk-work/ark#93 the answer was "whichever agent actor of that name has the
// lowest ULID", which after a pull is somebody else's — so one developer's
// records were written under an identity delegating from the other person.

func addHuman(t *testing.T, d *sql.DB, name string) *Actor {
	t.Helper()
	a := &Actor{Type: records.ActorHuman, Name: name}
	if err := CreateActor(context.Background(), d, a); err != nil {
		t.Fatalf("create human %s: %v", name, err)
	}
	return a
}

// copyActors moves actor rows the way a pull does: insert if absent, never
// re-point an existing row (internal/store/sync.go's actor upsert updates
// name and email only). It is how the other client's agent actor arrives.
func copyActors(t *testing.T, from, to *sql.DB) {
	t.Helper()
	rows, err := from.Query(`SELECT id, type, name, email, agent_name, agent_version,
		delegated_by, created_at FROM actors`)
	if err != nil {
		t.Fatalf("read actors: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, typ, name, email, agentName, agentVersion, delegatedBy, createdAt string
		if err := rows.Scan(&id, &typ, &name, &email, &agentName, &agentVersion,
			&delegatedBy, &createdAt); err != nil {
			t.Fatalf("scan actor: %v", err)
		}
		if _, err := to.Exec(`INSERT INTO actors
			(id, type, name, email, agent_name, agent_version, delegated_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET name = excluded.name, email = excluded.email`,
			id, typ, name, email, agentName, agentVersion, delegatedBy, createdAt); err != nil {
			t.Fatalf("upsert actor: %v", err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read actors: %v", err)
	}
}

func countAgents(t *testing.T, d *sql.DB, agentName string) int {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM actors
		WHERE type = 'agent' AND agent_name = ?`, agentName).Scan(&n); err != nil {
		t.Fatalf("count agents: %v", err)
	}
	return n
}

// delegatingHuman resolves a record's author the way the Elk adapter does
// (internal/workrecord.Actors.Resolve): agent → delegated_by → human.
func delegatingHuman(t *testing.T, d *sql.DB, actorID string) string {
	t.Helper()
	ctx := context.Background()
	a, err := GetActor(ctx, d, actorID)
	if err != nil {
		t.Fatalf("load actor %s: %v", actorID, err)
	}
	if a.Type != records.ActorAgent {
		return a.Name
	}
	if a.DelegatedBy == "" {
		t.Fatalf("agent %s (%s) delegates from nobody", a.ID, a.AgentName)
	}
	h, err := GetActor(ctx, d, a.DelegatedBy)
	if err != nil {
		t.Fatalf("load delegating human of %s: %v", a.ID, err)
	}
	return h.Name
}

func TestAgentActorIsPerDelegatingHuman(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	alice := addHuman(t, s.DB, "Alice")
	bob := addHuman(t, s.DB, "Bob")

	hers, err := FindAgentActor(ctx, s.DB, "claude-code", "1.0", alice.ID)
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	his, err := FindAgentActor(ctx, s.DB, "claude-code", "1.0", bob.ID)
	if err != nil {
		t.Fatalf("bob: %v", err)
	}
	if hers.ID == his.ID {
		t.Fatalf("one agent actor %s serves both humans", hers.ID)
	}
	if hers.DelegatedBy != alice.ID || his.DelegatedBy != bob.ID {
		t.Fatalf("delegation crossed: alice=%s bob=%s", hers.DelegatedBy, his.DelegatedBy)
	}

	// Resolving again mints nothing: each human keeps the actor they have.
	again, err := FindAgentActor(ctx, s.DB, "claude-code", "1.0", alice.ID)
	if err != nil {
		t.Fatalf("alice again: %v", err)
	}
	if again.ID != hers.ID {
		t.Errorf("alice's agent moved: %s then %s", hers.ID, again.ID)
	}
	if n := countAgents(t, s.DB, "claude-code"); n != 2 {
		t.Errorf("claude-code actors = %d, want 2 (one per human)", n)
	}
}

// TestFindAgentActorNeverSelectsAnotherClientsAgent covers the shape the bug
// actually took: the other person's agent actor arrives by pull, and because
// it registered first it holds the lower ULID that the old name-only lookup
// preferred.
func TestFindAgentActorNeverSelectsAnotherClientsAgent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	bob := addHuman(t, s.DB, "Bob")
	alice := addHuman(t, s.DB, "Alice")

	// Pulled first, so it sorts first. ULIDs are monotonic within a process.
	pulled := &Actor{Type: records.ActorAgent, Name: "claude-code",
		AgentName: "claude-code", DelegatedBy: bob.ID}
	if err := CreateActor(ctx, s.DB, pulled); err != nil {
		t.Fatal(err)
	}

	got, err := FindAgentActor(ctx, s.DB, "claude-code", "", alice.ID)
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	if got.ID == pulled.ID {
		t.Fatalf("alice writes as Bob's agent %s", pulled.ID)
	}
	// The setup only reproduces the bug while Bob's actor is the one a
	// name-only ORDER BY id would have picked.
	if pulled.ID >= got.ID {
		t.Fatalf("pulled actor %s does not sort before alice's %s; the case is not the one under test",
			pulled.ID, got.ID)
	}
	if delegatingHuman(t, s.DB, got.ID) != "Alice" {
		t.Errorf("alice's agent delegates from %s", delegatingHuman(t, s.DB, got.ID))
	}

	// And the pulled actor is still Bob's, unchanged.
	if delegatingHuman(t, s.DB, pulled.ID) != "Bob" {
		t.Errorf("the pulled agent stopped being Bob's")
	}
	if n := countAgents(t, s.DB, "claude-code"); n != 2 {
		t.Errorf("claude-code actors = %d, want 2", n)
	}
}

// TestFindAgentActorDoesNotAdoptAnUndelegatedAgent is the upgrade path for an
// agent actor written before delegation was recorded, and the reason an empty
// delegated_by is a deliberate non-match rather than a wildcard. Adopting it
// would attach this human's name to whatever it has already written.
func TestFindAgentActorDoesNotAdoptAnUndelegatedAgent(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	alice := addHuman(t, s.DB, "Alice")

	legacy := &Actor{Type: records.ActorAgent, Name: "claude-code", AgentName: "claude-code"}
	if err := CreateActor(ctx, s.DB, legacy); err != nil {
		t.Fatal(err)
	}
	legacyStore := &Store{DB: s.DB, RepoID: s.RepoID, Actor: *legacy}
	old, err := legacyStore.CreateTask(ctx, "Written before the upgrade", "")
	if err != nil {
		t.Fatal(err)
	}

	got, err := FindAgentActor(ctx, s.DB, "claude-code", "", alice.ID)
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	if got.ID == legacy.ID {
		t.Fatal("adopted the undelegated agent, back-dating Alice's authority over its records")
	}
	again, err := FindAgentActor(ctx, s.DB, "claude-code", "", alice.ID)
	if err != nil {
		t.Fatalf("alice again: %v", err)
	}
	if again.ID != got.ID {
		t.Errorf("minted a second actor for the same human: %s then %s", got.ID, again.ID)
	}

	// The old record still names the actor it named. Nothing is rewritten.
	reloaded, err := s.ResolveTask(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.CreatedBy != legacy.ID {
		t.Errorf("task %s re-attributed from %s to %s", old.ID, legacy.ID, reloaded.CreatedBy)
	}
	stored, err := GetActor(ctx, s.DB, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DelegatedBy != "" {
		t.Errorf("the legacy agent was re-pointed at %s", stored.DelegatedBy)
	}
}

func TestFindAgentActorRefusesUnusableDelegation(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	agent, err := FindAgentActor(ctx, s.DB, "helper", "", s.Actor.ID)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name        string
		delegatedBy string
	}{
		{"no delegating human at all", ""},
		{"a human this client does not hold", records.NewID()},
		{"an agent, which cannot delegate authority", agent.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countAgents(t, s.DB, "claude-code")
			_, err := FindAgentActor(ctx, s.DB, "claude-code", "", tc.delegatedBy)
			if err == nil {
				t.Fatal("delegation accepted")
			}
			var ae *records.Error
			if !errors.As(err, &ae) || ae.Kind != records.KindValidation {
				t.Fatalf("want a validation error, got %v", err)
			}
			// Refusing has to leave nothing behind: an actor minted under a
			// delegation the repository cannot resolve would be found by the
			// next run and pushed to a service that rejects it.
			if after := countAgents(t, s.DB, "claude-code"); after != before {
				t.Errorf("claude-code actors %d → %d; a refusal minted one", before, after)
			}
		})
	}
}

// TestTwoClientsAttributeToTheirOwnHuman is elk-work/ark#93's reproduction:
// two clients of one repository, both running `--agent claude-code`, both
// syncing, both writing. Each side's work must resolve to its own human.
func TestTwoClientsAttributeToTheirOwnHuman(t *testing.T) {
	ctx := context.Background()
	aliceDB, _ := newTestStore(t)
	bobDB, _ := newTestStore(t)

	// 1. Both join the same repository. Each client has its own default human.
	alice := addHuman(t, aliceDB.DB, "Alice")
	bob := addHuman(t, bobDB.DB, "Bob")

	// 2. Each mints its own agent actor on first use of --agent claude-code.
	aliceAgent, err := FindAgentActor(ctx, aliceDB.DB, "claude-code", "1.0", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	bobAgent, err := FindAgentActor(ctx, bobDB.DB, "claude-code", "1.0", bob.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 3. Both sync. Each now holds both agent actors and both humans.
	copyActors(t, aliceDB.DB, bobDB.DB)
	copyActors(t, bobDB.DB, aliceDB.DB)

	// 4. From here on, each resolves --agent claude-code to its own identity.
	aliceAgain, err := FindAgentActor(ctx, aliceDB.DB, "claude-code", "1.0", alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	bobAgain, err := FindAgentActor(ctx, bobDB.DB, "claude-code", "1.0", bob.ID)
	if err != nil {
		t.Fatal(err)
	}
	if aliceAgain.ID != aliceAgent.ID {
		t.Errorf("alice's agent changed after the sync: %s → %s", aliceAgent.ID, aliceAgain.ID)
	}
	if bobAgain.ID != bobAgent.ID {
		t.Errorf("bob's agent changed after the sync: %s → %s", bobAgent.ID, bobAgain.ID)
	}

	aliceWork := &Store{DB: aliceDB.DB, RepoID: aliceDB.RepoID, Actor: *aliceAgain}
	bobWork := &Store{DB: bobDB.DB, RepoID: bobDB.RepoID, Actor: *bobAgain}
	hers, err := aliceWork.CreateTask(ctx, "Alice's work", "")
	if err != nil {
		t.Fatal(err)
	}
	his, err := bobWork.CreateTask(ctx, "Bob's work", "")
	if err != nil {
		t.Fatal(err)
	}
	if got := delegatingHuman(t, aliceDB.DB, hers.CreatedBy); got != "Alice" {
		t.Errorf("Alice's work is attributed to %s", got)
	}
	if got := delegatingHuman(t, bobDB.DB, his.CreatedBy); got != "Bob" {
		t.Errorf("Bob's work is attributed to %s", got)
	}
}

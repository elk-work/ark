package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// Two people, each with their own human actor, in one repository. These are
// ULIDs because everything the client mints is.
const (
	aliceActor = "01HAAAAAAAAAAAAAAAAAAAAAAA"
	bobActor   = "01HBBBBBBBBBBBBBBBBBBBBBBB"
)

// pushAs sends a push as a chosen bearer.
func pushAs(t *testing.T, s *Server, bearer string, req api.PushRequest) *httptest.ResponseRecorder {
	t.Helper()
	req.RepositoryID, req.ClientID = repoID, "c1"
	if req.Mutations == nil {
		req.Mutations = []api.Mutation{}
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return doRequestAs(t, s, bearer, "POST", "/v1/sync/push", string(body))
}

// human builds an actor record of the kind `ark init` mints.
func human(id, name string) api.Actor {
	return api.Actor{ID: id, Type: string(records.ActorHuman), Name: name,
		Email: name + "@example.com", CreatedAt: records.Now()}
}

// writes is a mutation authored by an actor — the field this whole slice
// exists to start reading.
func writes(mutationID, recordID, actorID string) api.Mutation {
	m := mut(mutationID, "task", recordID, "create", 0,
		fmt.Sprintf(`{"id":%q,"number":1,"title":"work","status":"open"}`, recordID))
	m.CreatedBy = actorID
	return m
}

// actorBoundTo reports which principal owns an actor, or "" for none.
func actorBoundTo(t *testing.T, s *Server, actorID string) string {
	t.Helper()
	var principal string
	err := s.Repos.View(t.Context(), repoID, func(db *sql.DB) error {
		err := db.QueryRow(`SELECT principal_id FROM actor_principals WHERE actor_id = ?`,
			actorID).Scan(&principal)
		if err == sql.ErrNoRows {
			principal = ""
			return nil
		}
		return err
	})
	if err != nil {
		t.Fatalf("read actor binding: %v", err)
	}
	return principal
}

// twoPrincipals is a repository both of them may write to. The grants are
// equal on purpose: what separates them here is identity, not authority.
func twoPrincipals(t *testing.T) (*authServer, api.CreatePrincipalResponse, api.CreatePrincipalResponse) {
	t.Helper()
	a := newAuthServer(t)
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	alice := mintCredentialFor(t, a, "alice@example.com")
	bob := mintCredentialFor(t, a, "bob@example.com")
	grantTo(t, a, repoID, "alice@example.com", api.GrantWrite)
	grantTo(t, a, repoID, "bob@example.com", api.GrantWrite)
	return a, alice, bob
}

// The distinction the whole actor half turns on, and the one that is easy to
// get wrong: `internal/sync/sync.go` uploads every actor a client knows on
// every push, including ones it pulled from other people. Carrying somebody's
// actor record has to stay a harmless no-op. Writing *as* them is the
// rejection.
func TestCarryingAnActorIsFineAndWritingAsThemIsNot(t *testing.T) {
	a, alice, bob := twoPrincipals(t)

	// Alice introduces herself and writes as herself.
	rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
		Actors:    []api.Actor{human(aliceActor, "alice")},
		Mutations: []api.Mutation{writes("m1", "01TASKAAAAAAAAAAAAAAAAAAAA", aliceActor)},
	})
	if rec.Code != 200 {
		t.Fatalf("alice writing as herself: %d %s", rec.Code, rec.Body.String())
	}
	if got := actorBoundTo(t, a.Server, aliceActor); got != alice.Principal.ID {
		t.Fatalf("alice's actor is bound to %q, want %s", got, alice.Principal.ID)
	}

	// Bob has pulled Alice's actor, so his client re-uploads it on every
	// push — alongside his own, and writing only as himself. This is the
	// ordinary shape of every push in a shared repository and must succeed.
	rec = pushAs(t, a.Server, bob.Token, api.PushRequest{
		Actors:    []api.Actor{human(aliceActor, "alice"), human(bobActor, "bob")},
		Mutations: []api.Mutation{writes("m2", "01TASKBBBBBBBBBBBBBBBBBBBB", bobActor)},
	})
	if rec.Code != 200 {
		t.Fatalf("bob carrying alice's actor record: %d %s", rec.Code, rec.Body.String())
	}
	if got := actorBoundTo(t, a.Server, aliceActor); got != alice.Principal.ID {
		t.Errorf("carrying alice's record moved her binding to %q", got)
	}
	if got := actorBoundTo(t, a.Server, bobActor); got != bob.Principal.ID {
		t.Errorf("bob's own actor is bound to %q, want %s", got, bob.Principal.ID)
	}

	// Writing as her is the rejection — and it fails the whole push rather
	// than one mutation, so nothing in the batch lands.
	rec = pushAs(t, a.Server, bob.Token, api.PushRequest{
		Actors: []api.Actor{human(aliceActor, "alice"), human(bobActor, "bob")},
		Mutations: []api.Mutation{
			writes("m3", "01TASKCCCCCCCCCCCCCCCCCCCC", bobActor),
			writes("m4", "01TASKDDDDDDDDDDDDDDDDDDDD", aliceActor),
		},
	})
	if rec.Code != 403 {
		t.Fatalf("bob wrote as alice: %d %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "permission" {
		t.Errorf("error code %q, want permission", got)
	}
	for _, id := range []string{"01TASKCCCCCCCCCCCCCCCCCCCC", "01TASKDDDDDDDDDDDDDDDDDDDD"} {
		if recordExistsInRepo(t, a.Server, "task", id) {
			t.Errorf("a refused push left %s behind", id)
		}
	}
}

// recordExistsInRepo reports whether a record landed.
func recordExistsInRepo(t *testing.T, s *Server, recordType, recordID string) bool {
	t.Helper()
	found := false
	err := s.Repos.View(t.Context(), repoID, func(db *sql.DB) error {
		return db.QueryRow(`SELECT count(*) > 0 FROM records
			WHERE record_type = ? AND record_id = ?`, recordType, recordID).Scan(&found)
	})
	if err != nil {
		t.Fatalf("look for %s %s: %v", recordType, recordID, err)
	}
	return found
}

// An actor nobody has claimed belongs to whoever first writes as it —
// first-writer-binds, applied to writing as well as to introducing. This is
// what closes the gap for the actors that already exist: every one of them
// was introduced under the shared service token, which binds nothing.
func TestAnUnboundActorIsClaimedByTheFirstPrincipalToWriteAsIt(t *testing.T) {
	a, alice, bob := twoPrincipals(t)

	// The legacy token introduces both actors, exactly as every sync in the
	// fleet does today, and binds neither.
	if rec := pushAs(t, a.Server, a.Token, api.PushRequest{
		Actors: []api.Actor{human(aliceActor, "alice"), human(bobActor, "bob")},
	}); rec.Code != 200 {
		t.Fatalf("legacy introduction: %d %s", rec.Code, rec.Body.String())
	}
	for _, id := range []string{aliceActor, bobActor} {
		if got := actorBoundTo(t, a.Server, id); got != "" {
			t.Fatalf("the service token bound actor %s to %q; it identifies nobody", id, got)
		}
	}

	// Alice moves to a credential and pushes. Her actor is unclaimed, so it
	// becomes hers rather than refusing her.
	if rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
		Mutations: []api.Mutation{writes("m1", "01TASKAAAAAAAAAAAAAAAAAAAA", aliceActor)},
	}); rec.Code != 200 {
		t.Fatalf("alice could not write as her own unbound actor: %d %s", rec.Code, rec.Body.String())
	}
	if got := actorBoundTo(t, a.Server, aliceActor); got != alice.Principal.ID {
		t.Fatalf("alice's actor is bound to %q after her first write", got)
	}

	// And from then on it is hers.
	if rec := pushAs(t, a.Server, bob.Token, api.PushRequest{
		Mutations: []api.Mutation{writes("m2", "01TASKBBBBBBBBBBBBBBBBBBBB", aliceActor)},
	}); rec.Code != 403 {
		t.Errorf("bob wrote as alice's now-claimed actor: %d %s", rec.Code, rec.Body.String())
	}

	// The service token is never refused, whatever anything is bound to.
	// Six repositories sync on it today.
	if rec := pushAs(t, a.Server, a.Token, api.PushRequest{
		Mutations: []api.Mutation{writes("m3", "01TASKCCCCCCCCCCCCCCCCCCCC", aliceActor)},
	}); rec.Code != 200 {
		t.Errorf("the service token was refused: %d %s", rec.Code, rec.Body.String())
	}
}

// RFC-0003 Decision 5 on the sync path: a new agent actor must name a human
// it acts for, and cannot claim one that belongs to somebody else. The write
// routes have enforced this since RFC-0004; /v1/sync/push never had it, so an
// unrelated caller could introduce an agent claiming to act for anyone.
func TestANewAgentActorMustNameAHumanItMayActFor(t *testing.T) {
	agentActor := "01AGENTAAAAAAAAAAAAAAAAAAA"
	cases := []struct {
		name        string
		delegatedBy string
		want        int
	}{
		{"no delegated_by", "", 403},
		{"delegated to nobody", "01N0BDYAAAAAAAAAAAAAAAAAAA", 403},
		{"delegated to another principal's human", aliceActor, 403},
		{"delegated to my own human", bobActor, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, alice, bob := twoPrincipals(t)
			// Alice's human actor is hers; Bob's is his.
			if rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
				Actors: []api.Actor{human(aliceActor, "alice")},
			}); rec.Code != 200 {
				t.Fatalf("alice introduces herself: %d %s", rec.Code, rec.Body.String())
			}
			if rec := pushAs(t, a.Server, bob.Token, api.PushRequest{
				Actors: []api.Actor{human(bobActor, "bob")},
			}); rec.Code != 200 {
				t.Fatalf("bob introduces himself: %d %s", rec.Code, rec.Body.String())
			}
			_ = alice

			rec := pushAs(t, a.Server, bob.Token, api.PushRequest{
				Actors: []api.Actor{{ID: agentActor, Type: string(records.ActorAgent),
					Name: "claude-code", AgentName: "claude-code",
					DelegatedBy: c.delegatedBy, CreatedAt: records.Now()}},
			})
			if rec.Code != c.want {
				t.Fatalf("%s: %d %s, want %d", c.name, rec.Code, rec.Body.String(), c.want)
			}
			if c.want == 403 {
				if got := errCode(t, rec); got != "permission" {
					t.Errorf("error code %q, want permission", got)
				}
				if recordExistsInRepo(t, a.Server, "actor", agentActor) {
					t.Error("a refused agent was registered anyway")
				}
			}
		})
	}
}

// A request cannot re-point an existing agent at a different human: the
// delegation is read from the stored actor record and never from the payload,
// which is what makes the chain worth recording at all.
func TestAnExistingAgentCannotBeRepointed(t *testing.T) {
	agentActor := "01AGENTAAAAAAAAAAAAAAAAAAA"
	a, alice, bob := twoPrincipals(t)

	if rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
		Actors: []api.Actor{human(aliceActor, "alice")},
	}); rec.Code != 200 {
		t.Fatalf("alice introduces herself: %d %s", rec.Code, rec.Body.String())
	}
	if rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
		Actors: []api.Actor{{ID: agentActor, Type: string(records.ActorAgent),
			Name: "claude-code", AgentName: "claude-code",
			DelegatedBy: aliceActor, CreatedAt: records.Now()}},
	}); rec.Code != 200 {
		t.Fatalf("alice registers her agent: %d %s", rec.Code, rec.Body.String())
	}

	// Bob re-sends the same actor id claiming it delegates from him. It is a
	// no-op, not a rewrite and not a rejection: re-sending an actor record is
	// what every client does on every sync.
	if rec := pushAs(t, a.Server, bob.Token, api.PushRequest{
		Actors: []api.Actor{human(bobActor, "bob"), {ID: agentActor,
			Type: string(records.ActorAgent), Name: "claude-code", AgentName: "claude-code",
			DelegatedBy: bobActor, CreatedAt: records.Now()}},
	}); rec.Code != 200 {
		t.Fatalf("re-sending an existing agent record: %d %s", rec.Code, rec.Body.String())
	}
	stored := recordData(t, a.Server, "actor", agentActor)
	if stored["delegated_by"] != aliceActor {
		t.Errorf("the stored agent now delegates from %v, want %s", stored["delegated_by"], aliceActor)
	}
	if got := actorBoundTo(t, a.Server, agentActor); got != alice.Principal.ID {
		t.Errorf("the agent's binding moved to %q", got)
	}
}

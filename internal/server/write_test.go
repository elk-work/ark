package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

const (
	humanID = "01TESTHUMAN000000000000000"
	agent   = "release-bot"
)

// writeReq exercises a write route with auth and an optional idempotency key.
func writeReq(t *testing.T, s *Server, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	return writeReqAs(t, s, s.Token, path, key, body)
}

// writeReqAs is writeReq as a chosen bearer, so a test can put two principals
// on one route and see which identity each of them resolves to.
func writeReqAs(t *testing.T, s *Server, bearer, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// seedHuman introduces the human actor a remote writer must delegate from,
// through the ordinary push path a client would have used.
func seedHuman(t *testing.T, s *Server) {
	t.Helper()
	body, err := json.Marshal(api.PushRequest{
		RepositoryID: repoID, ClientID: "c1",
		Actors:    []api.Actor{{ID: humanID, Type: "human", Name: "Alice", Email: "alice@example.com"}},
		Mutations: []api.Mutation{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := doRequest(t, s, "POST", "/v1/sync/push", string(body)); rec.Code != 200 {
		t.Fatalf("seed human: %d %s", rec.Code, rec.Body.String())
	}
}

// writeServer is a registered repository with a human actor already in it.
func writeServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	registerRepo(t, s)
	seedHuman(t, s)
	return s
}

func decodeWrite(t *testing.T, rec *httptest.ResponseRecorder) api.RecordResponse {
	t.Helper()
	var resp api.RecordResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

// taskBody builds a create-task request naming the seeded writer.
func taskBody(title string) string {
	return taskBodyAs(agent, humanID, title)
}

// taskBodyAs builds one naming any (agent name, delegating human) pair — the
// key a writer is now resolved by.
func taskBodyAs(agentName, delegatedBy, title string) string {
	return fmt.Sprintf(`{"writer":{"agent_name":%q,"delegated_by":%q},"title":%q}`,
		agentName, delegatedBy, title)
}

// writerOf is the actor a written record is attributed to.
func writerOf(t *testing.T, resp api.RecordResponse) string {
	t.Helper()
	var task store.Task
	if err := json.Unmarshal(resp.Record.Data, &task); err != nil {
		t.Fatalf("decode as store.Task: %v", err)
	}
	return task.CreatedBy
}

func createTask(t *testing.T, s *Server, key, title string) api.RecordResponse {
	t.Helper()
	rec := writeReq(t, s, "/v1/repositories/"+repoID+"/tasks", key, taskBody(title))
	if rec.Code != 201 {
		t.Fatalf("create task: %d %s", rec.Code, rec.Body.String())
	}
	return decodeWrite(t, rec)
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error %q: %v", rec.Body.String(), err)
	}
	return e.Code
}

func revision(t *testing.T, s *Server) int64 {
	t.Helper()
	var rev int64
	err := s.Repos.View(context.Background(), repoID, func(db *sql.DB) error {
		return db.QueryRow(`SELECT revision FROM meta WHERE id = 1`).Scan(&rev)
	})
	if err != nil {
		t.Fatalf("read revision: %v", err)
	}
	return rev
}

// TestCreateTaskRoundTripsThroughTheClientStruct is the drift guard. The
// service writes record documents as maps, so nothing but this test stops a
// field name here from diverging from what a client unmarshals on pull.
func TestCreateTaskRoundTripsThroughTheClientStruct(t *testing.T) {
	s := writeServer(t)
	resp := createTask(t, s, "k1", "Flaky test in internal/sync")

	var task store.Task
	if err := json.Unmarshal(resp.Record.Data, &task); err != nil {
		t.Fatalf("decode as store.Task: %v", err)
	}
	if !records.ValidID(task.ID) {
		t.Errorf("id %q is not a ULID", task.ID)
	}
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"repository_id", task.RepositoryID, repoID},
		{"number", task.Number, int64(1)},
		{"title", task.Title, "Flaky test in internal/sync"},
		{"status", task.Status, "open"},
		{"created_by_type", task.CreatedByType, "agent"},
		{"version", task.Version, int64(1)},
		{"sync_state", task.SyncState, "synced"},
		{"record_id matches id", resp.Record.RecordID, task.ID},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.field, c.got, c.want)
		}
	}
	if task.CreatedAt == "" || task.UpdatedAt == "" {
		t.Errorf("timestamps: created_at %q updated_at %q", task.CreatedAt, task.UpdatedAt)
	}
	if !records.ValidID(task.CreatedBy) {
		t.Errorf("created_by %q is not an actor ULID", task.CreatedBy)
	}
}

// TestWriterRegistration covers RFC-0004 Decision 2: an agent cannot invent
// the authority it acts under, and once registered it is reused.
func TestWriterRegistration(t *testing.T) {
	path := "/v1/repositories/" + repoID + "/tasks"
	cases := []struct {
		name     string
		writer   string
		wantCode int
		wantErr  string
	}{
		{"no agent name", `{"delegated_by":"` + humanID + `"}`, 400, "validation"},
		{"new agent without delegation", `{"agent_name":"drifter"}`, 400, "validation"},
		{"delegated to an unknown actor", `{"agent_name":"drifter","delegated_by":"01NOBODY0000000000000000000"}`, 400, "validation"},
		{"delegated to an agent, not a human", `{"agent_name":"drifter","delegated_by":"01AGENT00000000000000000000"}`, 400, "validation"},
		{"registered", `{"agent_name":"drifter","delegated_by":"` + humanID + `"}`, 201, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := writeServer(t)
			body, err := json.Marshal(api.PushRequest{RepositoryID: repoID, ClientID: "c1",
				Actors: []api.Actor{{ID: "01AGENT00000000000000000000", Type: "agent",
					Name: "other", AgentName: "other", DelegatedBy: humanID}},
				Mutations: []api.Mutation{}})
			if err != nil {
				t.Fatal(err)
			}
			doRequest(t, s, "POST", "/v1/sync/push", string(body))

			rec := writeReq(t, s, path, "k1",
				`{"writer":`+c.writer+`,"title":"T"}`)
			if rec.Code != c.wantCode {
				t.Fatalf("code %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantErr != "" && errCode(t, rec) != c.wantErr {
				t.Errorf("error code %q, want %q", errCode(t, rec), c.wantErr)
			}
		})
	}
}

// secondHuman is a second person's actor, seeded the way a client would.
const secondHuman = "01TESTHUMAN2NDPERSON0000000"

// seedActor introduces an actor through the ordinary push path.
func seedActor(t *testing.T, s *Server, a api.Actor) {
	t.Helper()
	body, err := json.Marshal(api.PushRequest{RepositoryID: repoID, ClientID: "c1",
		Actors: []api.Actor{a}, Mutations: []api.Mutation{}})
	if err != nil {
		t.Fatal(err)
	}
	if rec := doRequest(t, s, "POST", "/v1/sync/push", string(body)); rec.Code != 200 {
		t.Fatalf("seed actor %s: %d %s", a.ID, rec.Code, rec.Body.String())
	}
}

// A writer resolves per (agent name, delegating human), matching what
// store.FindAgentActor gives a local agent after elk-work/ark#100 — the
// equivalence RFC-0004 Decision 2 states and elk-work/ark#102 restored. The
// same pair reuses one identity; a different human is a different identity;
// and naming a different human never re-points the registration that exists
// (Decision 2 rule 3).
func TestWriterIsReusedPerDelegatingHuman(t *testing.T) {
	s := writeServer(t)
	seedActor(t, s, api.Actor{ID: secondHuman, Type: "human", Name: "Bob"})
	path := "/v1/repositories/" + repoID + "/tasks"

	first := writerOf(t, createTask(t, s, "k1", "One"))
	again := writerOf(t, createTask(t, s, "k2", "Two"))
	if first != again {
		t.Errorf("the same (agent, human) minted two actors: %s and %s", first, again)
	}

	rec := writeReq(t, s, path, "k3", taskBodyAs(agent, secondHuman, "Three"))
	if rec.Code != 201 {
		t.Fatalf("same agent name under a second human: %d %s", rec.Code, rec.Body.String())
	}
	second := writerOf(t, decodeWrite(t, rec))
	if second == first {
		t.Fatalf("two people writing as %q shared one actor %s", agent, first)
	}

	// Rule 3: the registration that already existed is untouched. The
	// request selected a different one — it did not rewrite this one.
	if got := recordData(t, s, "actor", first)["delegated_by"]; got != humanID {
		t.Errorf("the first agent now delegates from %v, want %s "+
			"(a request must not re-point a registered agent)", got, humanID)
	}
	if got := recordData(t, s, "actor", second)["delegated_by"]; got != secondHuman {
		t.Errorf("the second agent delegates from %v, want %s", got, secondHuman)
	}
	for _, id := range []string{first, second} {
		if got := recordData(t, s, "actor", id)["agent_name"]; got != agent {
			t.Errorf("actor %s has agent_name %v, want %s", id, got, agent)
		}
	}
}

// The divergence elk-work/ark#102 is about, at the surface it appeared on:
// every person using the CLI writes through the same `ark-cli` agent name, so
// keying on the name alone attributed the second person's writes to the
// first — under a delegated_by naming the first person. Two principals, one
// agent name, two identities.
func TestTwoPrincipalsWritingAsOneAgentNameDoNotShareAnActor(t *testing.T) {
	a, alice, bob := twoPrincipals(t)
	path := "/v1/repositories/" + repoID + "/tasks"

	// Each introduces their own human actor, which binds it to them.
	if rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
		Actors: []api.Actor{human(aliceActor, "alice")}}); rec.Code != 200 {
		t.Fatalf("alice introduces herself: %d %s", rec.Code, rec.Body.String())
	}
	if rec := pushAs(t, a.Server, bob.Token, api.PushRequest{
		Actors: []api.Actor{human(bobActor, "bob")}}); rec.Code != 200 {
		t.Fatalf("bob introduces himself: %d %s", rec.Code, rec.Body.String())
	}

	rec := writeReqAs(t, a.Server, alice.Token, path, "k-alice",
		taskBodyAs("ark-cli", aliceActor, "Alice ran repo set"))
	if rec.Code != 201 {
		t.Fatalf("alice writing as ark-cli: %d %s", rec.Code, rec.Body.String())
	}
	aliceAgent := writerOf(t, decodeWrite(t, rec))

	rec = writeReqAs(t, a.Server, bob.Token, path, "k-bob",
		taskBodyAs("ark-cli", bobActor, "Bob ran it second"))
	if rec.Code != 201 {
		t.Fatalf("bob writing as ark-cli: %d %s", rec.Code, rec.Body.String())
	}
	bobAgent := writerOf(t, decodeWrite(t, rec))

	if aliceAgent == bobAgent {
		t.Fatalf("two principals writing as ark-cli share actor %s", aliceAgent)
	}
	if got := recordData(t, a.Server, "actor", bobAgent)["delegated_by"]; got != bobActor {
		t.Errorf("bob's write is attributed to an agent delegating from %v, want %s", got, bobActor)
	}
	// And the binding this route records now names the right person for
	// each, which first-caller-wins made meaningless (elk-work/ark#52).
	if got := actorBoundTo(t, a.Server, aliceAgent); got != alice.Principal.ID {
		t.Errorf("alice's ark-cli is bound to %q, want %s", got, alice.Principal.ID)
	}
	if got := actorBoundTo(t, a.Server, bobAgent); got != bob.Principal.ID {
		t.Errorf("bob's ark-cli is bound to %q, want %s", got, bob.Principal.ID)
	}
}

// Keying the lookup on the delegating human means the value arrives in the
// request, so the request must not be able to assert one it should not. The
// authenticated principal is what stops it: a human actor bound to somebody
// else is refused — checkDelegation's rule (grantsactors.go), applied here on
// every write rather than only where an agent is registered.
func TestARequestCannotClaimAnotherPrincipalsHuman(t *testing.T) {
	a, alice, bob := twoPrincipals(t)
	path := "/v1/repositories/" + repoID + "/tasks"

	if rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
		Actors: []api.Actor{human(aliceActor, "alice")}}); rec.Code != 200 {
		t.Fatalf("alice introduces herself: %d %s", rec.Code, rec.Body.String())
	}
	rec := writeReqAs(t, a.Server, alice.Token, path, "k-alice",
		taskBodyAs("ark-cli", aliceActor, "Alice's"))
	if rec.Code != 201 {
		t.Fatalf("alice writing as her own ark-cli: %d %s", rec.Code, rec.Body.String())
	}
	aliceAgent := writerOf(t, decodeWrite(t, rec))
	before := revision(t, a.Server)

	// Bob names Alice's human. Under the name-only lookup he did not even
	// have to: `{"agent_name":"ark-cli"}` landed on her actor by itself.
	rec = writeReqAs(t, a.Server, bob.Token, path, "k-bob",
		taskBodyAs("ark-cli", aliceActor, "Bob's, as Alice"))
	if rec.Code != 403 {
		t.Fatalf("bob claimed alice's authority: %d %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "permission" {
		t.Errorf("error code %q, want permission", got)
	}
	// Rule 3, from the other side: the refusal changed nothing, and a
	// rolled-back write left no revision behind either.
	if got := recordData(t, a.Server, "actor", aliceAgent)["delegated_by"]; got != aliceActor {
		t.Errorf("alice's agent now delegates from %v, want %s", got, aliceActor)
	}
	if got := actorBoundTo(t, a.Server, aliceAgent); got != alice.Principal.ID {
		t.Errorf("alice's agent is bound to %q, want %s", got, alice.Principal.ID)
	}
	if got := revision(t, a.Server); got != before {
		t.Errorf("a refused write bumped the revision %d -> %d", before, got)
	}

	// Bob under his own human is not refused — the rule is about identity,
	// not about the agent name, which people share by design.
	if rec := pushAs(t, a.Server, bob.Token, api.PushRequest{
		Actors: []api.Actor{human(bobActor, "bob")}}); rec.Code != 200 {
		t.Fatalf("bob introduces himself: %d %s", rec.Code, rec.Body.String())
	}
	if rec := writeReqAs(t, a.Server, bob.Token, path, "k-bob-2",
		taskBodyAs("ark-cli", bobActor, "Bob's, as Bob")); rec.Code != 201 {
		t.Fatalf("bob writing as his own ark-cli: %d %s", rec.Code, rec.Body.String())
	}
}

// The legacy service token is one string the whole fleet holds and it
// identifies nobody, so there is no principal to check a delegation against
// and it is never refused for one. Six live repositories sync on it.
func TestTheLegacyTokenWritesUnderAnyDelegation(t *testing.T) {
	a, alice, _ := twoPrincipals(t)
	path := "/v1/repositories/" + repoID + "/tasks"

	if rec := pushAs(t, a.Server, alice.Token, api.PushRequest{
		Actors: []api.Actor{human(aliceActor, "alice")}}); rec.Code != 200 {
		t.Fatalf("alice introduces herself: %d %s", rec.Code, rec.Body.String())
	}
	if got := actorBoundTo(t, a.Server, aliceActor); got != alice.Principal.ID {
		t.Fatalf("alice's actor is bound to %q, want %s", got, alice.Principal.ID)
	}

	rec := writeReqAs(t, a.Server, a.Token, path, "k-legacy",
		taskBodyAs("ark-cli", aliceActor, "From the fleet"))
	if rec.Code != 201 {
		t.Fatalf("the service token was refused: %d %s", rec.Code, rec.Body.String())
	}
	// It binds nothing either, so the actor it registers stays free for
	// whoever first writes as it (grantsactors.go).
	if got := actorBoundTo(t, a.Server, writerOf(t, decodeWrite(t, rec))); got != "" {
		t.Errorf("the service token bound an actor to %q", got)
	}
}

// What a client built before this change does. The fleet is pinned to v0.7.0,
// whose `remoteWriter` (internal/cli/repo.go) already sends a delegation on
// every request — the local agent's for an `--agent` run, the person's own
// human actor otherwise — so its writes keep working and simply stop sharing
// one `ark-cli` identity between people. A caller that read `delegated_by` as
// registration-only and omitted it afterwards is refused, in the field's own
// words rather than as a lookup that quietly found nobody.
func TestAnOlderClientsWriter(t *testing.T) {
	path := "/v1/repositories/" + repoID + "/tasks"
	cases := []struct {
		name     string
		writer   string
		wantCode int
		wantErr  string
	}{
		{"v0.7.0 CLI, a person at the keyboard",
			`{"agent_name":"ark-cli","agent_version":"0.7.0","delegated_by":"` + humanID + `"}`, 201, ""},
		{"v0.7.0 CLI, an --agent run under its own delegation",
			`{"agent_name":"claude-code","agent_version":"0.7.0","delegated_by":"` + humanID + `"}`, 201, ""},
		{"v0.7.0 CLI, an --agent run under somebody else's, unbound",
			`{"agent_name":"claude-code","agent_version":"0.7.0","delegated_by":"` + secondHuman + `"}`, 201, ""},
		{"a caller that omits the delegation it sent once",
			`{"agent_name":"release-bot"}`, 400, "validation"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := writeServer(t)
			seedActor(t, s, api.Actor{ID: secondHuman, Type: "human", Name: "Bob"})
			rec := writeReq(t, s, path, "k1", `{"writer":`+c.writer+`,"title":"T"}`)
			if rec.Code != c.wantCode {
				t.Fatalf("code %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantErr == "" {
				return
			}
			if got := errCode(t, rec); got != c.wantErr {
				t.Errorf("error code %q, want %q", got, c.wantErr)
			}
			if !strings.Contains(rec.Body.String(), "delegated_by") {
				t.Errorf("the refusal does not name the field: %s", rec.Body.String())
			}
		})
	}
}

// TestIdempotency covers Decision 4: a key is required on creates, and a
// replay returns the stored outcome without minting a revision.
func TestIdempotency(t *testing.T) {
	s := writeServer(t)
	path := "/v1/repositories/" + repoID + "/tasks"

	if rec := writeReq(t, s, path, "", taskBody("No key")); rec.Code != 400 {
		t.Errorf("missing key: %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	first := createTask(t, s, "same-key", "Once")
	revAfterFirst := revision(t, s)

	rec := writeReq(t, s, path, "same-key", taskBody("Once"))
	if rec.Code != 201 {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay did not announce itself: %v", rec.Header())
	}
	replay := decodeWrite(t, rec)
	if replay.Record.RecordID != first.Record.RecordID {
		t.Errorf("replay minted a second record: %s then %s",
			first.Record.RecordID, replay.Record.RecordID)
	}
	if replay.ServerRevision != first.ServerRevision {
		t.Errorf("replay revision %d, want %d", replay.ServerRevision, first.ServerRevision)
	}
	if got := revision(t, s); got != revAfterFirst {
		t.Errorf("replay bumped the repository revision %d -> %d", revAfterFirst, got)
	}

	// A different key is a different write.
	second := createTask(t, s, "other-key", "Twice")
	if second.Record.RecordID == first.Record.RecordID {
		t.Error("a fresh key reused the first record")
	}
}

// TestNumberingIsAuthoritative covers Decision 5: the service allocates, and
// it allocates above anything a client has already pushed.
func TestNumberingIsAuthoritative(t *testing.T) {
	s := writeServer(t)
	for i, want := range []int64{1, 2, 3} {
		resp := createTask(t, s, fmt.Sprintf("k%d", i), fmt.Sprintf("Task %d", i))
		var task store.Task
		json.Unmarshal(resp.Record.Data, &task)
		if task.Number != want {
			t.Errorf("task %d: number %d, want %d", i, task.Number, want)
		}
	}
	// A client pushes a task numbered well above the server's sequence.
	push(t, s, mut("m-client", "task", "01CLIENTTASK00000000000000", "create", 0,
		`{"id":"01CLIENTTASK00000000000000","number":41,"title":"From a client","status":"open"}`))

	resp := createTask(t, s, "k-after", "After the client")
	var task store.Task
	json.Unmarshal(resp.Record.Data, &task)
	if task.Number != 42 {
		t.Errorf("number after a client push = %d, want 42", task.Number)
	}
}

// TestWriteReachesPullAndTheFieldMerge covers Decision 4's revision half: a
// server-side write must travel on the next pull, and must be visible to the
// §10.4 field merge so a stale client cannot silently win it back.
func TestWriteReachesPullAndTheFieldMerge(t *testing.T) {
	s := writeServer(t)
	created := createTask(t, s, "k1", "Findable")
	taskID := created.Record.RecordID

	rec := doRequest(t, s, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID))
	if rec.Code != 200 {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body.String())
	}
	var pull api.PullResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &pull); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range pull.Records {
		if r.RecordType == "task" && r.RecordID == taskID {
			found = true
		}
	}
	if !found {
		t.Fatalf("pull did not carry the created task: %+v", pull.Records)
	}

	// A client editing status from revision 0 has a base older than the
	// server's write. Status is a cloud-wins field, so the edit is dropped.
	push(t, s, mut("m1", "task", taskID, "update", 0, `{"status":"closed"}`))
	if got := recordData(t, s, "task", taskID)["status"]; got != "open" {
		t.Errorf("stale client edit won: status = %v, want open", got)
	}

	// Title is a conflict field, so a stale edit needs a person.
	resp := push(t, s, mut("m2", "task", taskID, "update", 0, `{"title":"Renamed"}`))
	if len(resp.Conflicts) != 1 {
		t.Errorf("stale title edit: %+v, want one conflict", resp)
	}
}

func TestCreateComment(t *testing.T) {
	s := writeServer(t)
	task := createTask(t, s, "k1", "Parent")
	path := "/v1/repositories/" + repoID + "/comments"
	writer := fmt.Sprintf(`"writer":{"agent_name":%q,"delegated_by":%q}`, agent, humanID)

	cases := []struct {
		name     string
		body     string
		wantCode int
		wantErr  string
	}{
		{"unknown parent type", `{` + writer + `,"parent_type":"epic","parent_id":"x","body":"b"}`, 400, "validation"},
		{"missing body", `{` + writer + `,"parent_type":"task","parent_id":"` + task.Record.RecordID + `","body":"  "}`, 400, "validation"},
		{"parent not here", `{` + writer + `,"parent_type":"task","parent_id":"01ABSENT0000000000000000000","body":"b"}`, 404, "not_found"},
		{"on the task", `{` + writer + `,"parent_type":"task","parent_id":"` + task.Record.RecordID + `","body":"looks fixed"}`, 201, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := writeReq(t, s, path, "key-"+c.name, c.body)
			if rec.Code != c.wantCode {
				t.Fatalf("code %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantErr != "" {
				if got := errCode(t, rec); got != c.wantErr {
					t.Errorf("error code %q, want %q", got, c.wantErr)
				}
				return
			}
			var comment store.Comment
			if err := json.Unmarshal(decodeWrite(t, rec).Record.Data, &comment); err != nil {
				t.Fatalf("decode as store.Comment: %v", err)
			}
			if comment.ParentType != "task" || comment.ParentID != task.Record.RecordID {
				t.Errorf("parent: %+v", comment)
			}
			if comment.Body != "looks fixed" || comment.CreatedByType != "agent" {
				t.Errorf("comment: %+v", comment)
			}
			if !records.ValidID(comment.ID) {
				t.Errorf("id %q is not a ULID", comment.ID)
			}
		})
	}
}

// A correction is a new comment carrying supersedes_id — §6.3's append-only
// rule is why there is no edit route.
func TestCommentSupersedes(t *testing.T) {
	s := writeServer(t)
	task := createTask(t, s, "k1", "Parent")
	path := "/v1/repositories/" + repoID + "/comments"
	writer := fmt.Sprintf(`"writer":{"agent_name":%q,"delegated_by":%q}`, agent, humanID)

	first := writeReq(t, s, path, "c1",
		`{`+writer+`,"parent_type":"task","parent_id":"`+task.Record.RecordID+`","body":"wrong"}`)
	if first.Code != 201 {
		t.Fatalf("first comment: %d %s", first.Code, first.Body.String())
	}
	firstID := decodeWrite(t, first).Record.RecordID

	second := writeReq(t, s, path, "c2",
		`{`+writer+`,"parent_type":"task","parent_id":"`+task.Record.RecordID+
			`","body":"right","supersedes_id":"`+firstID+`"}`)
	if second.Code != 201 {
		t.Fatalf("correction: %d %s", second.Code, second.Body.String())
	}
	var c store.Comment
	json.Unmarshal(decodeWrite(t, second).Record.Data, &c)
	if c.SupersedesID != firstID {
		t.Errorf("supersedes_id = %q, want %q", c.SupersedesID, firstID)
	}
	if recordData(t, s, "comment", firstID)["body"] != "wrong" {
		t.Error("the superseded comment was modified; comments are append-only")
	}
}

func TestTaskStatus(t *testing.T) {
	s := writeServer(t)
	task := createTask(t, s, "k1", "Movable")
	id := task.Record.RecordID
	path := "/v1/repositories/" + repoID + "/tasks/" + id + "/status"
	writer := fmt.Sprintf(`"writer":{"agent_name":%q,"delegated_by":%q}`, agent, humanID)

	if rec := writeReq(t, s, path, "", `{`+writer+`,"status":"nope"}`); rec.Code != 400 {
		t.Errorf("invalid status: %d, want 400", rec.Code)
	}
	missing := "/v1/repositories/" + repoID + "/tasks/01ABSENT0000000000000000000/status"
	if rec := writeReq(t, s, missing, "", `{`+writer+`,"status":"done"}`); rec.Code != 404 {
		t.Errorf("unknown task: %d, want 404", rec.Code)
	}

	before := revision(t, s)
	rec := writeReq(t, s, path, "", `{`+writer+`,"status":"in_progress"}`)
	if rec.Code != 200 {
		t.Fatalf("transition: %d %s", rec.Code, rec.Body.String())
	}
	moved := decodeWrite(t, rec)
	if moved.ServerRevision <= before {
		t.Errorf("revision %d did not advance past %d", moved.ServerRevision, before)
	}
	var task2 store.Task
	json.Unmarshal(moved.Record.Data, &task2)
	if task2.Status != "in_progress" || task2.Title != "Movable" {
		t.Errorf("moved task: %+v", task2)
	}
	if task2.UpdatedAt == task.Record.RecordID {
		t.Error("updated_at was not stamped")
	}

	// Asking for the status it already has is a correct answer, not a write.
	after := revision(t, s)
	same := writeReq(t, s, path, "", `{`+writer+`,"status":"in_progress"}`)
	if same.Code != 200 {
		t.Fatalf("repeat: %d %s", same.Code, same.Body.String())
	}
	if got := revision(t, s); got != after {
		t.Errorf("a no-op transition bumped the revision %d -> %d", after, got)
	}
	if decodeWrite(t, same).ServerRevision != after {
		t.Errorf("no-op reported revision %d, want %d", decodeWrite(t, same).ServerRevision, after)
	}
}

func TestWriteRoutesRequireAuth(t *testing.T) {
	s := writeServer(t)
	for _, path := range []string{
		"/v1/repositories/" + repoID + "/tasks",
		"/v1/repositories/" + repoID + "/comments",
		"/v1/repositories/" + repoID + "/tasks/01X/status",
	} {
		req := httptest.NewRequest("POST", path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s without a token: %d, want 401", path, rec.Code)
		}
	}
}

func TestWriteToUnregisteredRepository(t *testing.T) {
	s := newTestServer(t)
	rec := writeReq(t, s, "/v1/repositories/01NOSUCHREPO00000000000000/tasks", "k1", taskBody("T"))
	if rec.Code != 404 {
		t.Errorf("unregistered repository: %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

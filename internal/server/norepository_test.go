package server

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// populated stands a repository up and puts one record in it, so the loss
// under test is a loss of something.
func populated(t *testing.T, s *Server) {
	t.Helper()
	registerRepo(t, s)
	push(t, s, mut("m1", "task", "t1", "create", 0,
		`{"id":"t1","number":1,"title":"A","status":"open"}`))
}

// TestAStoredDatabaseHoldingNoRepositoryIsNotFoundOnEveryRoute is
// elk-work/ark#85, and it covers the half of elk-work/ark#66 that was only
// true for one shape of the loss.
//
// A zero-length `repos/<id>.db` opens as a valid empty SQLite database, so the
// service finds a file, applies the schema and sees no repository. #66 answers
// that at registration with the `404` it answers for an absent object; pull
// and push were never changed, so they fell through to whichever query first
// noticed the empty table — `500 {"code":"internal","message":"pull failed"}`,
// with `sql: no rows in result set` visible only in the service's own logs.
//
// No client reached it, because registration runs first and answers one call
// earlier. An operator did: the restore runbook has them curl the pull route
// to check the service's copy before trusting it (docs/self-hosting.md), which
// is exactly the moment a repository is in this state.
func TestAStoredDatabaseHoldingNoRepositoryIsNotFoundOnEveryRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
		// ownMessage marks the route that answers with its own wording rather
		// than through respond: registration, which names the client's cursor.
		ownMessage bool
	}{
		{
			name: "pull", method: "POST", path: "/v1/sync/pull",
			body: fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID),
		},
		{
			name: "push", method: "POST", path: "/v1/sync/push",
			body: fmt.Sprintf(`{"repository_id":%q,"client_id":"c1","mutations":[
				{"id":"m2","record_type":"task","record_id":"t2","operation":"create",
				 "base_server_revision":0,
				 "payload":{"id":"t2","number":2,"title":"B","status":"open"}}]}`, repoID),
		},
		{
			name: "get repository", method: "GET", path: "/v1/repositories/" + repoID,
		},
		{
			name: "get record", method: "GET",
			path: "/v1/repositories/" + repoID + "/records/task/t1",
		},
		{
			// The route #66 already fixed, kept here so the set is the whole
			// API surface a repository is reachable through: moving the check
			// into repodb must not have moved this answer.
			name: "register with history", method: "POST", path: "/v1/repositories",
			body:       fmt.Sprintf(`{"id":%q,"name":"test","last_revision":33}`, repoID),
			ownMessage: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, backend := corruptibleServer(t)
			populated(t, s)
			writeStored(t, backend, repoID, nil) // zero bytes: a valid empty database

			rec := doRequest(t, s, tc.method, tc.path, tc.body)
			if rec.Code != 404 {
				t.Fatalf("%s against a stored database holding no repository: %d %s, want 404",
					tc.name, rec.Code, rec.Body.String())
			}
			e := apiError(t, rec.Body.Bytes())
			if e.Code != "not_found" {
				t.Errorf("code = %q, want not_found — the same answer every other route gives", e.Code)
			}
			if tc.ownMessage {
				return
			}
			// The status and the code cannot carry the difference between an
			// object that was removed and one that had zero bytes written over
			// it, and the operator's next move is not the same. The message can.
			if !strings.Contains(e.Message, "holds no repository") {
				t.Errorf("message %q does not say the stored object is there and empty", e.Message)
			}
		})
	}
}

// A client at revision 0 is the one caller that must still be allowed to write
// into a stored database holding no repository row: it has no history to
// contradict, and this is how a repository comes into existence over an object
// somebody zeroed. The decision belongs to `create`, so moving the check into
// repodb must not have made it unconditional — a guard that fired here would
// leave the repository unrecoverable except by deleting the object first.
func TestRegistrationWithNoHistoryStillAdoptsAZeroLengthObject(t *testing.T) {
	s, backend := corruptibleServer(t)
	populated(t, s)
	writeStored(t, backend, repoID, nil)

	if rec := doRequest(t, s, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test","last_revision":0}`, repoID)); rec.Code != 200 {
		t.Fatalf("register at revision 0 against a zeroed object: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRequest(t, s, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID)); rec.Code != 200 {
		t.Fatalf("pull after re-registering: %d %s", rec.Code, rec.Body.String())
	}
}

// The two shapes want the same restore and not the same diagnosis — an object
// somebody deleted against one something wrote zero bytes over — so the
// service log has to tell them apart even where the response deliberately does
// not.
func TestTheServiceLogsWhichShapeOfLossItFound(t *testing.T) {
	s, backend := corruptibleServer(t)
	populated(t, s)

	var logs bytes.Buffer
	s.Log = slog.New(slog.NewJSONHandler(&logs, nil))

	writeStored(t, backend, repoID, nil)
	if rec := doRequest(t, s, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID)); rec.Code != 404 {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logs.String(), "holds no repository row") {
		t.Errorf("the service logged nothing about the empty stored database: %s", logs.String())
	}
}

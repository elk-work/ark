package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

// captureLog points a server's logger at a buffer, which is how these tests
// read the one signal an operator has that a repository was created rather
// than re-registered.
func captureLog(t *testing.T, s *Server) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	s.Log = slog.New(slog.NewJSONHandler(&buf, nil))
	return &buf
}

// registerBody posts a raw registration body, so a test can send exactly what
// an old client would — including omitting last_revision entirely.
func registerBody(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, s, "POST", "/v1/repositories", body)
}

// stored reports whether the service holds an *object* for the repository.
//
// Deliberately not a pull: this asks the storage layer, which is where the
// resurrected object actually appeared. And deliberately not repodb.View
// either, which is what it used to ask — "an object is there" stopped being
// the same question as "a repository is registered" when View began answering
// ErrNotFound for a stored database holding no repository row
// (elk-work/ark#85). #66 is about whether an object came back from the dead,
// so the probe has to be about the object.
func stored(t *testing.T, s *Server) bool {
	t.Helper()
	_, err := s.Repos.Backend.Fetch(context.Background(), repoID,
		filepath.Join(t.TempDir(), "probe.db"))
	switch {
	case err == nil:
		return true
	case errors.Is(err, repodb.ErrNotFound):
		return false
	default:
		t.Fatalf("fetch repository object: %v", err)
		return false
	}
}

// storedRepositoryRows counts the repository rows in the service's stored copy,
// read out of the object itself rather than through repodb. The assertion it
// serves is about what a registration wrote into storage, and repodb's answer
// for a database with no repository row is now the behaviour under test — read
// through there, the test would only be agreeing with itself.
func storedRepositoryRows(t *testing.T, s *Server) int {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rows.db")
	if _, err := s.Repos.Backend.Fetch(context.Background(), repoID, path); err != nil {
		t.Fatalf("fetch repository object: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open the stored database: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM meta WHERE id = 1`).Scan(&n); err != nil {
		t.Fatalf("count repository rows in the stored database: %v", err)
	}
	return n
}

func decodeErr(t *testing.T, rec *httptest.ResponseRecorder) api.Error {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	return e
}

// TestRegistrationWillNotResurrectARepositoryTheClientHasSynced is
// elk-work/ark#66.
//
// While `repos/<id>.db` is missing the service is honest about it: pull and
// push both answer 404, which is the one clean signal that a repository has
// been lost. Registration runs on every sync with create=true, so the first
// client to sync stood an empty repository back up at revision 1 — after
// which an operator investigating saw a live, empty, registered repository
// instead, and the two look nothing alike while meaning the same thing.
//
// The client's cursor is what tells them apart, and the service now has it.
func TestRegistrationWillNotResurrectARepositoryTheClientHasSynced(t *testing.T) {
	s := newTestServer(t)
	logs := captureLog(t, s)

	body := fmt.Sprintf(`{"id":%q,"name":"scout","last_revision":33,
		"actors":[{"id":%q,"type":"human","name":"Someone"}]}`, repoID, humanID)
	rec := registerBody(t, s, body)
	if rec.Code != 404 {
		t.Fatalf("register at an absent repository: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if got := decodeErr(t, rec); got.Code != "not_found" {
		t.Errorf("error code %q, want not_found — the same answer pull and push give", got.Code)
	}

	// The message is what a person reads while their repository is missing,
	// so it has to say that, and not read as a complaint about the request.
	msg := decodeErr(t, rec).Message
	for _, want := range []string{repoID, "33", "missing"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal message does not mention %q: %s", want, msg)
		}
	}

	// The whole point: the 404 survives the sync that used to destroy it.
	if stored(t, s) {
		t.Error("the refused registration created the repository anyway")
	}
	if rec := doRequest(t, s, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID)); rec.Code != 404 {
		t.Errorf("pull after a refused registration: %d, want 404 — the loss is still visible", rec.Code)
	}
	if !strings.Contains(logs.String(), "refused to create a repository") {
		t.Errorf("the refusal was not logged: %s", logs.String())
	}
}

// A client that has never synced is the case registration exists for, and it
// has to keep working — including for a client too old to send the field at
// all, which is every client built before this change.
func TestRegistrationCreatesForAClientWithNoHistory(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"cursor at zero", fmt.Sprintf(`{"id":%q,"name":"scout","last_revision":0}`, repoID)},
		{"field omitted (an older client)", fmt.Sprintf(`{"id":%q,"name":"scout"}`, repoID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			if rec := registerBody(t, s, tc.body); rec.Code != 200 {
				t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
			}
			if !stored(t, s) {
				t.Fatal("registration did not create the repository")
			}
			var name string
			if err := s.Repos.View(context.Background(), repoID, func(db *sql.DB) error {
				return db.QueryRow(`SELECT name FROM meta WHERE id = 1`).Scan(&name)
			}); err != nil {
				t.Fatalf("read metadata: %v", err)
			}
			if name != "scout" {
				t.Errorf("registered name %q, want scout", name)
			}
		})
	}
}

// The same loss can arrive wearing a live object. A zero-length
// `repos/<id>.db` reads as a perfectly valid empty SQLite database, so the
// service finds a file, applies the schema to it and sees no repository —
// which is a deletion in every way that matters (docs/self-hosting.md).
func TestRegistrationWillNotAdoptAStoredDatabaseHoldingNoRepository(t *testing.T) {
	s := newTestServer(t)
	// An object with the schema and nothing in it, which is what the service
	// makes of a zero-length one.
	if err := s.Repos.Update(context.Background(), repoID, true, func(*sql.Tx) error {
		return nil
	}); err != nil {
		t.Fatalf("stand up an empty database: %v", err)
	}
	if !stored(t, s) {
		t.Fatal("the empty database was not stored")
	}

	body := fmt.Sprintf(`{"id":%q,"name":"scout","last_revision":33}`, repoID)
	rec := registerBody(t, s, body)
	if rec.Code != 404 {
		t.Fatalf("register against an empty database: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if n := storedRepositoryRows(t, s); n != 0 {
		t.Errorf("the stored database holds %d repository rows — the refused registration wrote one into it anyway", n)
	}
	// And it left the object alone. An operator restores over this file; a
	// refusal that removed it would have taken away the thing they are about
	// to overwrite, and the evidence of what happened with it.
	if !stored(t, s) {
		t.Error("the refused registration removed the stored object")
	}
}

// A cursor above zero is not a refusal on its own — it is only a refusal when
// the service has nothing. Against a repository that is there, a registration
// carrying history is the ordinary one every sync makes, and it still
// backfills metadata and upserts the checkout's actors (elk-work/ark#47).
func TestRegistrationWithHistoryAgainstALiveRepository(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s) // created by a client at revision 0

	body := fmt.Sprintf(`{"id":%q,"name":"weirdly-named-dir","last_revision":33,
		"actors":[{"id":%q,"type":"human","name":"Someone"}]}`, repoID, humanID)
	if rec := registerBody(t, s, body); rec.Code != 200 {
		t.Fatalf("register against a live repository: %d %s", rec.Code, rec.Body.String())
	}
	var name string
	var actors int
	if err := s.Repos.View(context.Background(), repoID, func(db *sql.DB) error {
		if err := db.QueryRow(`SELECT name FROM meta WHERE id = 1`).Scan(&name); err != nil {
			return err
		}
		return db.QueryRow(`SELECT count(*) FROM records WHERE record_type = 'actor'`).Scan(&actors)
	}); err != nil {
		t.Fatalf("read repository: %v", err)
	}
	if name != "test" {
		t.Errorf("name is now %q — registration still only backfills", name)
	}
	if actors != 1 {
		t.Errorf("actor records: %d, want 1 — the checkout's identity still travels", actors)
	}
}

// The cheapest half of #66, and the one worth having whatever happens to the
// rest: a registration that creates a repository is a rare event, and it used
// to be indistinguishable in the log from the idempotent no-op that runs on
// every sync of every repository.
func TestCreatingRegistrationIsLoggedAndTheIdempotentOneIsNot(t *testing.T) {
	s := newTestServer(t)
	logs := captureLog(t, s)

	registerRepo(t, s)
	created := logs.String()
	if !strings.Contains(created, "registration created a repository") {
		t.Fatalf("creating a repository logged nothing: %s", created)
	}
	if !strings.Contains(created, repoID) {
		t.Errorf("the log line does not name the repository: %s", created)
	}

	logs.Reset()
	registerRepo(t, s)
	registerRepo(t, s)
	if again := logs.String(); strings.Contains(again, "registration created a repository") {
		t.Errorf("a repeat registration reported a creation: %s", again)
	}
}

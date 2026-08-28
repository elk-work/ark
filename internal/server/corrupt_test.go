package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

// corruptibleServer is newTestServer with the storage directory kept, so a
// test can do to the stored object what an operator's storage can do to it.
func corruptibleServer(t *testing.T) (*Server, *repodb.LocalBackend) {
	t.Helper()
	backend := &repodb.LocalBackend{Dir: t.TempDir()}
	return &Server{
		Repos: repodb.NewManager(backend, t.TempDir()),
		Token: "test-token",
		Blobs: &LocalBlobStore{Dir: t.TempDir(), BaseURL: "http://unused"},
	}, backend
}

// writeStored replaces a repository's stored database with content, and moves
// the file's timestamp forward.
//
// LocalBackend uses the modification time in nanoseconds as its generation, so
// without the nudge a rewrite fast enough to land in the same nanosecond would
// be indistinguishable from no rewrite at all and the manager would serve its
// cached copy. GCS has no such hazard — every write mints a generation — so
// this is an artefact of the test backend rather than of the case under test.
func writeStored(t *testing.T, backend *repodb.LocalBackend, repoID string, content []byte) {
	t.Helper()
	path := backend.PathFor(repoID)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

func storedBytes(t *testing.T, backend *repodb.LocalBackend, repoID string) []byte {
	t.Helper()
	b, err := os.ReadFile(backend.PathFor(repoID))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// apiError decodes the error body a non-2xx response carries.
func apiError(t *testing.T, body []byte) api.Error {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body is not an api.Error (%v): %s", err, body)
	}
	return e
}

// TestACorruptStoredDatabaseIsNotAnAnonymousFailure is elk-work/ark#65 at the
// service. A stored database that will not open used to arrive as
// `500 {"code":"internal","message":"pull failed"}` — the same body a genuine
// server-side blip produces, which is how a permanent condition became
// indistinguishable from a transient one from every vantage point except the
// service's own logs, where it said `apply schema: database disk image is
// malformed (11)`.
//
// Two shapes of the same damage, because SQLite refuses them at different
// points and an operator's storage produces both: bytes cut short, and bytes
// the right length with the header wrong.
func TestACorruptStoredDatabaseIsNotAnAnonymousFailure(t *testing.T) {
	cases := []struct {
		name string
		// corrupt returns the bytes to leave in storage, given the healthy ones.
		corrupt func(healthy []byte) []byte
		// wantMessage is the part of the message that says which corruption
		// this was — the whole point of reporting it rather than "pull failed".
		wantMessage string
	}{
		{
			name:        "truncated",
			corrupt:     func(healthy []byte) []byte { return healthy[:len(healthy)/2] },
			wantMessage: "will not open",
		},
		{
			// The right number of bytes and the wrong ones at the front, which
			// is what a partial overwrite or a restore of the wrong object
			// looks like. SQLite refuses it at a different point than it
			// refuses a truncation, and the report has to be the same.
			name: "header overwritten",
			corrupt: func(healthy []byte) []byte {
				damaged := append([]byte(nil), healthy...)
				copy(damaged[:16], make([]byte, 16)) // the "SQLite format 3" magic
				return damaged
			},
			wantMessage: "will not open",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, backend := corruptibleServer(t)
			registerRepo(t, s)
			push(t, s, mut("m1", "task", "t1", "create", 0,
				`{"id":"t1","number":1,"title":"A","status":"open"}`))

			writeStored(t, backend, repoID, tc.corrupt(storedBytes(t, backend, repoID)))

			rec := doRequest(t, s, "POST", "/v1/sync/pull",
				fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID))
			if rec.Code != 500 {
				t.Fatalf("pull against a %s database: %d %s", tc.name, rec.Code, rec.Body.String())
			}
			e := apiError(t, rec.Body.Bytes())
			if e.Code != api.ErrorCodeRepositoryCorrupt {
				t.Errorf("code = %q, want %q — an anonymous `internal` is what the client reads as offline",
					e.Code, api.ErrorCodeRepositoryCorrupt)
			}
			if !strings.Contains(e.Message, repoID) {
				t.Errorf("message %q does not name the repository", e.Message)
			}
			if !strings.Contains(e.Message, tc.wantMessage) {
				t.Errorf("message %q does not say what is wrong (want %q)", e.Message, tc.wantMessage)
			}
			if strings.Contains(e.Message, "pull failed") {
				t.Errorf("message %q reports the verb rather than the state", e.Message)
			}

			// Push takes the write path, through Update rather than View, and
			// has to reach the same verdict: a database that will not open
			// cannot be written to either.
			body, err := json.Marshal(api.PushRequest{RepositoryID: repoID, ClientID: "c1",
				Mutations: []api.Mutation{mut("m2", "task", "t2", "create", 0,
					`{"id":"t2","number":2,"title":"B","status":"open"}`)}})
			if err != nil {
				t.Fatal(err)
			}
			rec = doRequest(t, s, "POST", "/v1/sync/push", string(body))
			if rec.Code != 500 || apiError(t, rec.Body.Bytes()).Code != api.ErrorCodeRepositoryCorrupt {
				t.Errorf("push against a %s database: %d %s", tc.name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAZeroLengthObjectIsNotReportedAsCorruption is the boundary between this
// change and elk-work/ark#66. A zero-length `repos/<id>.db` is a valid empty
// SQLite database: it opens, the schema applies, and what is missing is the
// repository row, not the bytes. That is a repository the service no longer
// holds, and registration already answers it with the 404 it answers for an
// absent object — a better signal than "corrupt", because the client turns it
// into a recorded history reset.
//
// So the corruption code must not claim it. Two kinds of damage, two answers,
// and the difference is whether anything can still be read.
func TestAZeroLengthObjectIsNotReportedAsCorruption(t *testing.T) {
	s, backend := corruptibleServer(t)
	registerRepo(t, s)
	push(t, s, mut("m1", "task", "t1", "create", 0,
		`{"id":"t1","number":1,"title":"A","status":"open"}`))

	writeStored(t, backend, repoID, nil)

	rec := doRequest(t, s, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test","last_revision":33}`, repoID))
	if rec.Code != 404 {
		t.Fatalf("register against a zero-length object: %d %s, want 404 (elk-work/ark#66)",
			rec.Code, rec.Body.String())
	}
	if code := apiError(t, rec.Body.Bytes()).Code; code != "not_found" {
		t.Errorf("code = %q, want not_found — a missing repository, not a corrupt one", code)
	}
}

// TestAFirstRegistrationStillCreatesTheRepository pins what the check above
// must not break, and it is every repository's first sync: no stored object at
// all, so there is no repository row to look for and the transaction that
// registers it is the one about to write it. A guard that fired here would
// make Ark impossible to start using.
func TestAFirstRegistrationStillCreatesTheRepository(t *testing.T) {
	s, backend := corruptibleServer(t)
	if _, err := os.Stat(backend.PathFor(repoID)); !os.IsNotExist(err) {
		t.Fatalf("this test needs storage with no object for the repository (%v)", err)
	}

	registerRepo(t, s)
	push(t, s, mut("m1", "task", "t1", "create", 0,
		`{"id":"t1","number":1,"title":"A","status":"open"}`))

	rec := doRequest(t, s, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q,"after_revision":0}`, repoID))
	if rec.Code != 200 {
		t.Fatalf("pull against a healthy repository: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(backend.Dir, repoID+".db")); err != nil {
		t.Errorf("the repository was never stored: %v", err)
	}
}

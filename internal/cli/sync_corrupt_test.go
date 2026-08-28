package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/server/repodb"
)

// startCorruptibleSyncServer is startSyncServer with the storage directory
// handed back, so a test can damage a repository's stored database the way an
// operator's storage can.
func startCorruptibleSyncServer(t *testing.T) (url string, backend *repodb.LocalBackend) {
	t.Helper()
	backend = &repodb.LocalBackend{Dir: t.TempDir()}
	blobs := &server.LocalBlobStore{Dir: t.TempDir()}
	s := &server.Server{
		Repos: repodb.NewManager(backend, t.TempDir()),
		Token: "test-token",
		Blobs: blobs,
	}
	ts := httptest.NewServer(s.Handler())
	blobs.BaseURL = ts.URL
	t.Cleanup(ts.Close)
	t.Setenv("ARK_TOKEN", "test-token")
	return ts.URL, backend
}

// corruptStored replaces a repository's stored database and moves its
// timestamp forward, because LocalBackend's generation is the modification
// time in nanoseconds and two writes inside one nanosecond would read as no
// write at all.
func corruptStored(t *testing.T, backend *repodb.LocalBackend, repoID string, content []byte) {
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

// TestSyncAgainstACorruptRepositoryIsNotOffline is elk-work/ark#65 at the
// surface that made it invisible. `ark sync` against a service whose stored
// copy of the repository will not open exited 6 — offline — which is the code
// a retry loop keys on, for a condition that will still be there on every
// later attempt. The client had no way to know: the service sent
// `500 {"code":"internal","message":"pull failed"}`, and `internal/cloud`
// mapped every status at or above 500 to offline.
//
// The message is asserted to name the repository, which is the other half of
// the fix — the client can now print what an operator has to act on rather
// than the verb that happened to be running.
func TestSyncAgainstACorruptRepositoryIsNotOffline(t *testing.T) {
	cases := []struct {
		name    string
		corrupt func(healthy []byte) []byte
	}{
		{"truncated", func(healthy []byte) []byte { return healthy[:len(healthy)/2] }},
		{"header overwritten", func(healthy []byte) []byte {
			damaged := append([]byte(nil), healthy...)
			copy(damaged[:16], make([]byte, 16))
			return damaged
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url, backend := startCorruptibleSyncServer(t)
			dir := gitRepo(t)
			ark(t, dir, "init")
			ark(t, dir, "remote", "set", url)
			ark(t, dir, "task", "create", "-t", "Something worth losing")
			ark(t, dir, "sync")

			repoID := repoIDOf(t, dir)
			healthy, err := os.ReadFile(backend.PathFor(repoID))
			if err != nil {
				t.Fatal(err)
			}
			corruptStored(t, backend, repoID, tc.corrupt(healthy))

			out, err := arkErr(t, dir, "sync")
			if err == nil {
				t.Fatalf("sync succeeded against a %s stored database:\n%s", tc.name, out)
			}
			if code := records.ExitCode(err); code != 8 {
				t.Errorf("exit code = %d, want 8 — 6 is offline, which tells a caller to retry forever: %v",
					code, err)
			}
			if !strings.Contains(err.Error(), repoID) {
				t.Errorf("error %q does not name the repository whose stored copy is damaged", err)
			}
			if !strings.Contains(err.Error(), "restore") {
				t.Errorf("error %q does not say what has to happen next", err)
			}
		})
	}
}

// TestSyncAgainstAGenuine5xxIsStillOffline is the case exit 8 must not absorb.
// A service that failed for any other reason may well succeed on the next
// attempt, and telling a caller to stop retrying that would be the same
// mistake in the other direction.
func TestSyncAgainstAGenuine5xxIsStillOffline(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	t.Setenv("ARK_TOKEN", "test-token")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":"internal","message":"register repository failed"}`))
	}))
	t.Cleanup(ts.Close)
	ark(t, dir, "remote", "set", ts.URL)

	_, err := arkErr(t, dir, "sync")
	if err == nil {
		t.Fatal("sync succeeded against a service returning 500")
	}
	if code := records.ExitCode(err); code != 6 {
		t.Errorf("exit code = %d, want 6 (offline) for an ordinary 5xx: %v", code, err)
	}
}

// TestSyncAgainstAZeroLengthObjectStaysAHistoryReset is the boundary this
// change deliberately does not cross. A zero-length `repos/<id>.db` opens as a
// valid empty SQLite database, so it is a repository the service no longer
// holds rather than one it cannot read, and elk-work/ark#66 already answers it
// where a sync meets it first: registration refuses, and `sync.Run` records a
// history reset and exits 7.
//
// 7 carries more than 8 would here — `ark status` keeps the reset durably —
// so this pins that exit 8 did not swallow it.
func TestSyncAgainstAZeroLengthObjectStaysAHistoryReset(t *testing.T) {
	url, backend := startCorruptibleSyncServer(t)
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", url)
	ark(t, dir, "task", "create", "-t", "Something worth losing")
	ark(t, dir, "sync")

	corruptStored(t, backend, repoIDOf(t, dir), nil)

	if _, err := arkErr(t, dir, "sync"); err == nil {
		t.Fatal("sync succeeded against a zero-length stored database")
	} else if code := records.ExitCode(err); code != 7 {
		t.Errorf("exit code = %d, want 7 (partial, history reset): %v", code, err)
	}
}

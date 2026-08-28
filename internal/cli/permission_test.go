package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/servertest"
	"github.com/elk-work/ark/pkg/api"
)

// bootstrapToken is what these tests mint principals with. It is the
// service's break-glass (ARK_BOOTSTRAP_TOKEN), and the only way to get a real
// `arkc_…` credential out of a service with no identity provider.
const bootstrapToken = "bootstrap-token"

// startGrantedSyncServer is startSyncServer with principal creation turned
// on, so a test can hold a credential the service genuinely issued and be
// refused by the real grant check rather than by a hand-written 403.
func startGrantedSyncServer(t *testing.T) string {
	t.Helper()
	s := servertest.NewServer(t)
	s.BootstrapToken = bootstrapToken
	blobs := s.Blobs.(*server.LocalBlobStore)
	ts := httptest.NewServer(s.Handler())
	blobs.BaseURL = ts.URL
	t.Cleanup(ts.Close)
	return ts.URL
}

// mintCredential creates a principal on the service and returns its
// credential — a live one, holding no grant on anything.
func mintCredential(t *testing.T, url, email string) string {
	t.Helper()
	body := strings.NewReader(fmt.Sprintf(`{"email":%q,"kind":"human"}`, email))
	req, err := http.NewRequest(http.MethodPost, url+"/v1/principals", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+bootstrapToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mint credential for %s: %d", email, resp.StatusCode)
	}
	var out api.CreatePrincipalResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Token == "" {
		t.Fatal("the service issued no credential")
	}
	return out.Token
}

// TestSyncWithoutAGrantIsNotAskedToLogInAgain is elk-work/ark#95 end to end,
// at the surface that made it wrong: what a person actually reads when a sync
// stops.
//
// Before grants were enforced (elk-work/ark#52) `permission` had one cause,
// so the client appended one remedy — "Run `ark login`" — to every 401 and
// 403 alike. A missing grant is now the second cause, and it arrives as a 403
// from a credential the service just accepted. Logging in again re-issues a
// credential that is already working, so the advice cannot help, and the
// person it is given to concludes the credential is the problem.
//
// The refusal the service sends already names who they are, which repository,
// which level, and the command that issues one. The client's job is to leave
// it alone and stop contradicting it.
func TestSyncWithoutAGrantIsNotAskedToLogInAgain(t *testing.T) {
	url := startGrantedSyncServer(t)

	// The legacy service token registers the repository and syncs, which is
	// what every client in the fleet still does. It carries implicit admin
	// everywhere and is checked against no grant (#54 retires it).
	dir := gitRepo(t)
	t.Setenv("ARK_TOKEN", servertest.Token)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", url)
	ark(t, dir, "task", "create", "-t", "Work behind a grant")
	ark(t, dir, "sync")
	repoID := repoIDOf(t, dir)

	// Now somebody with a perfectly good credential and no grant on this
	// repository. Registration is the first call a sync makes and it needs
	// `read`, so this is where they meet the refusal.
	t.Setenv("ARK_TOKEN", mintCredential(t, url, "alice@example.com"))
	out, err := arkErr(t, dir, "sync")
	if err == nil {
		t.Fatalf("a principal with no grant synced anyway:\n%s", out)
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}

	// The service's half, kept whole: who, where, what is needed, and the one
	// command that supplies it. The reader cannot fix this themselves, so the
	// message has to be something they can hand to somebody who can.
	for _, want := range []string{"alice@example.com", repoID, "read", "ark repo grant"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	// The client's half. The credential is fine and saying otherwise is the
	// whole bug.
	if !strings.Contains(err.Error(), "the stored credential was accepted") {
		t.Errorf("refusal %q does not say the credential is not the problem", err)
	}
	if strings.Contains(err.Error(), "ark login") {
		t.Errorf("refusal %q sends the reader to re-authenticate a credential "+
			"the service had just accepted", err)
	}

	// And the grant is what was missing: issue one and the same client syncs.
	t.Setenv("ARK_TOKEN", servertest.Token)
	ark(t, dir, "repo", "grant", "alice@example.com", "--read")
	t.Setenv("ARK_TOKEN", mintCredential(t, url, "alice@example.com"))
	ark(t, dir, "sync")
}

// TestSyncWithADeadCredentialStillSaysArkLogin is the case the split must not
// take with it. A bearer the service does not recognise is a 401, the remedy
// really is `ark login`, and a reader who stopped being told that would have
// nowhere to go.
func TestSyncWithADeadCredentialStillSaysArkLogin(t *testing.T) {
	url := startGrantedSyncServer(t)
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", url)

	t.Setenv("ARK_TOKEN", "arkc_this-was-never-issued")
	out, err := arkErr(t, dir, "sync")
	if err == nil {
		t.Fatalf("a credential the service never issued synced anyway:\n%s", out)
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	if !strings.Contains(err.Error(), "ark login") {
		t.Errorf("a rejected credential must still name its remedy, got: %v", err)
	}
}

// TestTheLegacyServiceTokenSyncsUnaffected pins the other side of the same
// change. The shared service token authenticates on a path that never reads
// auth.db and carries implicit admin on every repository, so it meets neither
// 401 nor 403 — the fleet keeps working while #54 is open.
func TestTheLegacyServiceTokenSyncsUnaffected(t *testing.T) {
	url := startGrantedSyncServer(t)
	dir := gitRepo(t)
	t.Setenv("ARK_TOKEN", servertest.Token)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", url)
	ark(t, dir, "task", "create", "-t", "Nothing here needs a grant")
	ark(t, dir, "sync")
	ark(t, dir, "sync") // and again, now that the repository exists
}

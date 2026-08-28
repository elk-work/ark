package cli

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/internal/servertest"
)

const testBootstrap = "test-bootstrap-token"

// startBootstrapServer boots the real service with a bootstrap token set, the
// way a self-hoster who wants per-principal credentials configures it.
func startBootstrapServer(t *testing.T) string {
	t.Helper()
	s := &server.Server{
		Repos:          repodb.NewManager(&repodb.LocalBackend{Dir: t.TempDir()}, t.TempDir()),
		Token:          servertest.Token,
		BootstrapToken: testBootstrap,
		Blobs:          &server.LocalBlobStore{Dir: t.TempDir()},
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// principalResult mirrors api.CreatePrincipalResponse for --json assertions.
type principalResult struct {
	Principal struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Email string `json:"email"`
	} `json:"principal"`
	Created      bool   `json:"created"`
	Token        string `json:"token"`
	CredentialID string `json:"credential_id"`
	ExpiresAt    string `json:"expires_at"`
}

// The self-hosting story RFC-0003 promises, end to end and with no identity
// provider in it: one environment variable on the server, one command here,
// and a person holds a credential of their own.
func TestPrincipalCreateMintsACredentialAgainstARunningService(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)

	var got principalResult
	arkJSON(t, dir, &got, "principal", "create", "--remote", url,
		"--email", "me@example.com", "--display-name", "Me", "--bootstrap", testBootstrap)

	if !got.Created || got.Principal.Email != "me@example.com" || got.Principal.Kind != "human" {
		t.Fatalf("result = %+v", got)
	}
	if !strings.HasPrefix(got.Token, "arkc_") || got.CredentialID == "" || got.ExpiresAt == "" {
		t.Fatalf("result = %+v", got)
	}

	// The credential is a token like any other, which is the point: nothing in
	// the client had to learn a new shape. `ark login --no-verify` is not used
	// here — verification against the live service is the assertion.
	out := ark(t, dir, "login", "--remote", url, "--token", got.Token)
	if !strings.Contains(out, "Token stored in") {
		t.Errorf("the minted credential did not survive `ark login`: %s", out)
	}
}

// The human output has one job beyond reporting success: the credential is
// shown once and never recoverable, so it has to say so and say what to do
// next.
func TestPrincipalCreateShowsTheCredentialOnceAndSaysSo(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)

	out := ark(t, dir, "principal", "create", "--remote", url,
		"--email", "me@example.com", "--bootstrap", testBootstrap)
	if !strings.Contains(out, "arkc_") {
		t.Fatalf("no credential in the output:\n%s", out)
	}
	if !strings.Contains(out, "shown once") || !strings.Contains(out, "SHA-256") {
		t.Errorf("the output does not say the credential is unrecoverable:\n%s", out)
	}
	if !strings.Contains(out, "ark login") {
		t.Errorf("the output does not say what to do with it:\n%s", out)
	}
}

// The bootstrap token is a secret, so an argument is the worst of the three
// ways to pass one (spec §20). Both quieter paths have to work.
func TestPrincipalCreateReadsTheBootstrapTokenWithoutAnArgument(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)

	t.Setenv("ARK_BOOTSTRAP_TOKEN", testBootstrap)
	var got principalResult
	arkJSON(t, dir, &got, "principal", "create", "--remote", url, "--email", "env@example.com")
	if got.Token == "" {
		t.Fatalf("ARK_BOOTSTRAP_TOKEN was not used: %+v", got)
	}
}

// Failures have to leave the operator somewhere to go, and with the right exit
// code — an agent scripting this sees only the code (spec §22).
func TestPrincipalCreateFailures(t *testing.T) {
	url := startBootstrapServer(t)
	dir := gitRepo(t)

	t.Run("wrong bootstrap token", func(t *testing.T) {
		out, err := arkErr(t, dir, "principal", "create", "--remote", url,
			"--email", "me@example.com", "--bootstrap", "not-the-token")
		if err == nil {
			t.Fatalf("a wrong bootstrap token succeeded:\n%s", out)
		}
		if code := records.ExitCode(err); code != 5 {
			t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
		}
		// The sync client's 401 advice is "run `ark login`", which is exactly
		// backwards for the command you run in order to have something to log
		// in with. This one has to name the server's variable instead.
		if !strings.Contains(err.Error(), "ARK_BOOTSTRAP_TOKEN") {
			t.Errorf("error %q does not say which token was refused", err)
		}
	})

	t.Run("no email", func(t *testing.T) {
		_, err := arkErr(t, dir, "principal", "create", "--remote", url, "--bootstrap", testBootstrap)
		if err == nil {
			t.Fatal("a principal with no identity was accepted")
		}
		if code := records.ExitCode(err); code != 2 {
			t.Errorf("exit code = %d, want 2 (validation): %v", code, err)
		}
	})

	t.Run("no remote", func(t *testing.T) {
		_, err := arkErr(t, t.TempDir(), "principal", "create",
			"--email", "me@example.com", "--bootstrap", testBootstrap)
		if err == nil {
			t.Fatal("a create with nowhere to send it was accepted")
		}
		if code := records.ExitCode(err); code != 2 {
			t.Errorf("exit code = %d, want 2 (validation): %v", code, err)
		}
	})

	t.Run("service without a bootstrap token", func(t *testing.T) {
		plain := startSyncServer(t)
		out, err := arkErr(t, dir, "principal", "create", "--remote", plain,
			"--email", "me@example.com", "--bootstrap", testBootstrap)
		if err == nil {
			t.Fatalf("a service with bootstrap disabled minted a principal:\n%s", out)
		}
		if !strings.Contains(err.Error(), "ARK_BOOTSTRAP_TOKEN") {
			t.Errorf("error %q does not tell the operator what to set", err)
		}
	})
}

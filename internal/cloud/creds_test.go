package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// credsTestRemote uses an .invalid host (RFC 2606) so the darwin keychain
// lookup — which runs against the developer's real keychain — can never
// match an actual credential. That makes the keychain step a guaranteed
// miss on macOS and a no-op elsewhere, letting these tests exercise the
// env and file steps deterministically on every platform.
const credsTestRemote = "https://ark-creds-test.invalid"

// isolateHome points HOME at a scratch directory and clears ARK_TOKEN so
// token resolution can only find what the test plants.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// os.UserHomeDir uses USERPROFILE on Windows, not HOME. Set both so a
	// test run can never read or write the developer's real credentials.
	t.Setenv("USERPROFILE", home)
	t.Setenv("ARK_TOKEN", "")
	return home
}

// TestTokenResolutionOrder: ARK_TOKEN beats the keychain beats the
// credentials file, and with nothing planted anywhere resolution fails
// with a permission error telling the user to log in (spec §20).
func TestTokenResolutionOrder(t *testing.T) {
	isolateHome(t)

	// Nothing anywhere: a permission error (exit 5), not a panic or "".
	_, err := ResolveToken(credsTestRemote)
	if err == nil {
		t.Fatal("resolution with no credentials should fail")
	}
	var re *records.Error
	if !errors.As(err, &re) || re.Kind != records.KindPermission {
		t.Fatalf("error = %v, want kind permission", err)
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5", code)
	}

	// The credentials file is the last resort.
	if err := writeFileToken(RemoteHost(credsTestRemote), "from-file"); err != nil {
		t.Fatalf("write file token: %v", err)
	}
	if tok, err := ResolveToken(credsTestRemote); err != nil || tok != "from-file" {
		t.Fatalf("file resolution: %q (%v), want from-file", tok, err)
	}

	// ARK_TOKEN beats everything.
	t.Setenv("ARK_TOKEN", "from-env")
	if tok, err := ResolveToken(credsTestRemote); err != nil || tok != "from-env" {
		t.Fatalf("env resolution: %q (%v), want from-env", tok, err)
	}
}

// TestTokenIsNeverReadFromRepositoryConfig: a token planted in a
// repository's .ark/config.toml — even in the working directory — must
// never resolve. Credentials live outside the repository, always
// (spec §20: "Do not store tokens in .ark/config.toml").
func TestTokenIsNeverReadFromRepositoryConfig(t *testing.T) {
	isolateHome(t)
	repo := t.TempDir()
	arkDir := filepath.Join(repo, ".ark")
	if err := os.MkdirAll(arkDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "version = 1\nremote = \"" + credsTestRemote + "\"\ntoken = \"leaked-token\"\n"
	if err := os.WriteFile(filepath.Join(arkDir, "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	tok, err := ResolveToken(credsTestRemote)
	if tok == "leaked-token" {
		t.Fatal("token was read from the repository's .ark/config.toml")
	}
	if err == nil {
		t.Fatalf("resolution should fail, got token %q", tok)
	}
}

// TestCredentialsFileKeepsOtherRemotesAndTightPermissions: rewriting one
// remote's token preserves the others, and the file stays 0600 — it is the
// restricted development fallback, not a shared config (spec §20).
func TestCredentialsFileKeepsOtherRemotesAndTightPermissions(t *testing.T) {
	home := isolateHome(t)

	if err := writeFileToken("a.example.com", "tok-a"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileToken("b.example.com", "tok-b"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileToken("a.example.com", "tok-a2"); err != nil {
		t.Fatal(err)
	}
	if got := fileToken("a.example.com"); got != "tok-a2" {
		t.Errorf("a.example.com = %q, want tok-a2", got)
	}
	if got := fileToken("b.example.com"); got != "tok-b" {
		t.Errorf("b.example.com = %q, want tok-b (clobbered by rewrite)", got)
	}

	assertRestrictedCredentials(t, filepath.Join(home, ".ark", "credentials.toml"))
}

// TestStoreTokenFallsBackToCredentialsFile: off macOS there is no keychain,
// so StoreToken lands in ~/.ark/credentials.toml and ResolveToken finds it
// again (spec §20). Skipped on darwin: the keychain path would write a real
// entry into the developer's keychain, and the file path is covered above.
func TestStoreTokenFallsBackToCredentialsFile(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("StoreToken on darwin writes the real macOS keychain; the file fallback is covered directly")
	}
	isolateHome(t)

	where, err := StoreToken(credsTestRemote, "stored-token")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if where != credentialsPath() {
		t.Errorf("stored in %q, want %q", where, credentialsPath())
	}
	if tok, err := ResolveToken(credsTestRemote); err != nil || tok != "stored-token" {
		t.Fatalf("round trip: %q (%v), want stored-token", tok, err)
	}
}

// TestRemoteHostNormalization: credential lookup keys on the remote's host,
// so the same token serves every path under one service; a non-URL remote
// falls back to the raw string.
func TestRemoteHostNormalization(t *testing.T) {
	cases := []struct{ remote, want string }{
		{"https://ark.example.com/api/", "ark.example.com"},
		{"https://ark.example.com:8443", "ark.example.com:8443"},
		{"ark.example.com", "ark.example.com"},
	}
	for _, tc := range cases {
		if got := RemoteHost(tc.remote); got != tc.want {
			t.Errorf("RemoteHost(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

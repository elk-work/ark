package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// The keyring branches are not reachable from this package and must not be:
// TestMain sets ARK_NO_KEYRING so nothing here can touch the developer's real
// credential store, and the fake lives in internal/cloud where the store
// variable is. What these tests own is the command surface — the flags, the
// --json shape, the exit codes — exercised against the fallback file, which is
// the store ARK_NO_KEYRING leaves in play.

// logoutHome narrows the package-wide temporary HOME to one test. TestMain
// already keeps the real one out of reach; these tests write real credential
// files and then assert about their absence, so they must not share a HOME with
// each other either.
func logoutHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ARK_TOKEN", "")
	return home
}

type logoutJSON struct {
	Host           string   `json:"host"`
	Remote         string   `json:"remote"`
	Removed        []string `json:"removed"`
	RemovedFrom    []string `json:"removed_from"`
	KeyringSkipped bool     `json:"keyring_skipped"`
	EnvToken       bool     `json:"env_token"`
}

// TestLogoutRemovesTheStoredTokenAndNamesTheStore walks the pair a person
// walks: log in, log out. The assertion that carries the issue is the last one
// — the credentials file is gone — because until this command existed a stored
// token had no removal path that was Ark's rather than the platform's.
func TestLogoutRemovesTheStoredTokenAndNamesTheStore(t *testing.T) {
	dir := gitRepo(t)
	home := logoutHome(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", "https://ark-logout-test.invalid")
	ark(t, dir, "login", "--no-verify", "--token", "a-token")

	credentials := filepath.Join(home, ".ark", "credentials.toml")
	if _, err := os.Stat(credentials); err != nil {
		t.Fatalf("login did not write %s: %v", credentials, err)
	}

	var out logoutJSON
	arkJSON(t, dir, &out, "logout")
	if out.Host != "ark-logout-test.invalid" {
		t.Errorf("host = %q, want the remote's host", out.Host)
	}
	if len(out.Removed) != 1 || out.Removed[0] != "file" {
		t.Errorf("removed = %v, want [file]", out.Removed)
	}
	if len(out.RemovedFrom) != 1 || out.RemovedFrom[0] != credentials {
		t.Errorf("removed_from = %v, want [%s]", out.RemovedFrom, credentials)
	}
	if !out.KeyringSkipped {
		t.Error("keyring_skipped = false, but ARK_NO_KEYRING is set for this package")
	}
	if out.EnvToken {
		t.Error("env_token = true without ARK_TOKEN set")
	}
	if _, err := os.Stat(credentials); !os.IsNotExist(err) {
		t.Errorf("%s survived the logout (%v)", credentials, err)
	}

	// The human rendering names the host and the store the token came out of,
	// and never the token. --remote also has to reach the same credential from
	// outside the repository, since that is how you log out of a machine you
	// are cleaning up rather than a project you are working in.
	ark(t, dir, "login", "--no-verify", "--token", "a-token")
	human := ark(t, t.TempDir(), "logout", "--remote", "https://ark-logout-test.invalid")
	if !strings.Contains(human, "ark-logout-test.invalid") {
		t.Errorf("output does not name the host:\n%s", human)
	}
	if !strings.Contains(human, credentials) {
		t.Errorf("output does not name the store it came out of:\n%s", human)
	}
	if strings.Contains(human, "a-token") {
		t.Errorf("output printed the token itself:\n%s", human)
	}
	if _, err := os.Stat(credentials); !os.IsNotExist(err) {
		t.Errorf("--remote did not reach the credential (%v)", err)
	}
}

// TestLogoutWithNothingStoredSucceeds: logout is idempotent by nature. The
// state a caller wants — no credential for this host on this machine — is
// already true, so there is nothing to report as a failure. Exit 3 would make a
// teardown script fail on a machine that never logged in, and the fix people
// reach for is `|| true`, which also hides a store genuinely refusing to let
// the credential go.
func TestLogoutWithNothingStoredSucceeds(t *testing.T) {
	dir := gitRepo(t)
	logoutHome(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", "https://ark-logout-empty.invalid")

	var out logoutJSON
	arkJSON(t, dir, &out, "logout")
	if len(out.Removed) != 0 {
		t.Errorf("removed = %v, want empty", out.Removed)
	}
	// Empty, never null: an agent iterating this should not have to special-case
	// the shape of "there was nothing stored".
	if out.Removed == nil || out.RemovedFrom == nil {
		t.Errorf("removed/removed_from came back null (%v, %v); --json is a stable interface",
			out.Removed, out.RemovedFrom)
	}

	human := ark(t, dir, "logout")
	if !strings.Contains(human, "nothing to remove") {
		t.Errorf("output does not say there was nothing stored:\n%s", human)
	}
	// And that the keyring went unexamined, which is the difference between
	// "there is no credential" and "there is no credential where I looked".
	if !strings.Contains(human, "ARK_NO_KEYRING") {
		t.Errorf("output does not say the keyring was skipped:\n%s", human)
	}
}

// TestLogoutExitsSevenWhileARKTokenIsSet: no command can unset a variable in
// the shell that started it, so the stores can be empty while every remote
// still authenticates. Exit 0 there would be a command describing a state it
// has not got — the divergence shape of elk-work/ark#46 and #58 — and spec §22
// has the code for exactly this: 7, did what it was asked and left something
// needing repair. The result is still printed, because the removals happened.
func TestLogoutExitsSevenWhileARKTokenIsSet(t *testing.T) {
	dir := gitRepo(t)
	logoutHome(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", "https://ark-logout-env.invalid")
	ark(t, dir, "login", "--no-verify", "--token", "a-token")
	t.Setenv("ARK_TOKEN", "from-env")

	out, err := arkErr(t, dir, "--json", "logout")
	if err == nil {
		t.Fatalf("logout reported success while ARK_TOKEN is set:\n%s", out)
	}
	if code := records.ExitCode(err); code != 7 {
		t.Errorf("exit code = %d, want 7 (partial success requiring repair): %v", code, err)
	}
	if !strings.Contains(err.Error(), "ARK_TOKEN") || !strings.Contains(err.Error(), "unset") {
		t.Errorf("error %q does not say what to do about ARK_TOKEN", err)
	}
	// The JSON is still well-formed and still says what happened: a caller
	// scripting against --json gets the record and the honest exit code both.
	var report logoutJSON
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("bad JSON alongside the exit-7 warning: %v\n%s", err, out)
	}
	if !report.EnvToken {
		t.Errorf("env_token = false while ARK_TOKEN is set:\n%s", out)
	}
	if len(report.Removed) != 1 || report.Removed[0] != "file" {
		t.Errorf("removed = %v, want [file]; the stored copy still had to go", report.Removed)
	}
}

// TestLogoutNeedsARemoteItCanScope: the credential is per service, so with no
// repository to read a remote from there is nothing to scope the removal to.
// Asking is better than guessing — logging out of the wrong host is a silent
// no-op that looks exactly like success.
func TestLogoutNeedsARemoteItCanScope(t *testing.T) {
	logoutHome(t)
	outside := t.TempDir()
	noRemote := gitRepo(t)
	ark(t, noRemote, "init")

	cases := []struct {
		name string
		dir  string
		args []string
	}{
		{"outside a repository", outside, []string{"logout"}},
		{"in a repository with no remote", noRemote, []string{"logout"}},
		{"a remote that is not an http(s) URL", outside, []string{"logout", "--remote", "ark.example.com"}},
		{"an argument it has no meaning for", outside, []string{"logout", "some-host"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := arkErr(t, tc.dir, tc.args...)
			if err == nil {
				t.Fatalf("ark %v should have failed, got:\n%s", tc.args, out)
			}
			if code := records.ExitCode(err); code != 2 {
				t.Errorf("exit code = %d, want 2 (invalid input): %v", code, err)
			}
		})
	}
}

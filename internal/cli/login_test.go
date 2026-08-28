package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// TestMain sets ARK_NO_KEYRING for this package, so the fallback credentials
// file is the store `ark login` reaches here — which is the machine #62 lives
// on, and the only one where the file is written at all.

// TestLoginWillNotDestroyACredentialsFileItCannotRead is the issue end to end,
// at the surface a person actually uses. Before this, the reproduction below
// printed "Token stored in …" and exited 0 while replacing the file with a
// single entry; a.example.com's token was gone and nothing said so.
//
// The exit code is asserted as well as the file, because an agent scripting
// `ark login` sees only the code: a 0 there was a report of a state the machine
// did not have, which is the divergence shape of #46 and #58.
func TestLoginWillNotDestroyACredentialsFileItCannotRead(t *testing.T) {
	dir := gitRepo(t)
	home := logoutHome(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", "https://ark-login-b.invalid")

	path := filepath.Join(home, ".ark", "credentials.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	before := "[remotes.\"ark-login-a.invalid\"]\ntoken = \"tok-a-precious\"\n\nthis line is not TOML\n"
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := arkErr(t, dir, "login", "--no-verify", "--token", "tok-b")
	if err == nil {
		t.Fatalf("ark login reported success over an unreadable credentials file:\n%s", out)
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file the user has to repair", err)
	}
	if !strings.Contains(err.Error(), "ark login") {
		t.Errorf("error %q stops the user without saying what to do next", err)
	}
	if strings.Contains(err.Error()+out, "tok-b") {
		t.Errorf("the token itself was printed (spec §21):\n%s\n%v", out, err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the credentials file is gone: %v", err)
	}
	if string(after) != before {
		t.Fatalf("ark login rewrote the credentials file\n--- before\n%s--- after\n%s", before, after)
	}
}

// TestLoginOnAMachineWithNoCredentialsFileStillWorks pins the case the refusal
// above must not touch, and it is nearly every machine: no file, first login,
// no keyring in play. A guard that fired here would break `ark login` for
// everyone rather than for the few with a damaged file.
func TestLoginOnAMachineWithNoCredentialsFileStillWorks(t *testing.T) {
	dir := gitRepo(t)
	home := logoutHome(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", "https://ark-login-fresh.invalid")

	path := filepath.Join(home, ".ark", "credentials.toml")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("this test needs a home with no credentials file (%v)", err)
	}

	var out struct {
		StoredIn string `json:"stored_in"`
		Storage  string `json:"storage"`
		Host     string `json:"host"`
	}
	arkJSON(t, dir, &out, "login", "--no-verify", "--token", "tok-fresh")
	if out.Storage != "file" || out.StoredIn != path {
		t.Errorf("storage = %q, stored_in = %q, want file and %s", out.Storage, out.StoredIn, path)
	}
	if out.Host != "ark-login-fresh.invalid" {
		t.Errorf("host = %q, want the remote's host", out.Host)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("login reported storing the token but wrote no file: %v", err)
	}
}

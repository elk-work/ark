package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStatusReportsWhereTheTokenCameFrom: `ark login` says where it wrote the
// token, so resolution has to be equally legible — "sync is authenticated" and
// "sync is authenticated out of a plaintext file" are different states and
// only one of them is fine (spec §20). The keyring case is not reachable from
// here: TestMain opts this package out of the real OS keyring, and the keyring
// branches are exercised against a fake in internal/cloud.
func TestStatusReportsWhereTheTokenCameFrom(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var before struct {
		TokenSource string `json:"token_source"`
	}
	arkJSON(t, dir, &before, "status")
	if before.TokenSource != "" {
		t.Errorf("token_source = %q with no remote configured, want it omitted", before.TokenSource)
	}

	ark(t, dir, "remote", "set", "https://ark-status-test.invalid")

	var none struct {
		TokenSource string `json:"token_source"`
	}
	arkJSON(t, dir, &none, "status")
	if none.TokenSource != "none" {
		t.Errorf("token_source = %q with no credentials, want none", none.TokenSource)
	}

	t.Setenv("ARK_TOKEN", "from-env")
	var env struct {
		TokenSource string `json:"token_source"`
	}
	arkJSON(t, dir, &env, "status")
	if env.TokenSource != "env" {
		t.Errorf("token_source = %q with ARK_TOKEN set, want env", env.TokenSource)
	}

	// And the human rendering names the source without printing the token.
	out := ark(t, dir, "status")
	if !strings.Contains(out, "token       ARK_TOKEN") {
		t.Errorf("status output does not name the token source:\n%s", out)
	}
	if strings.Contains(out, "from-env") {
		t.Errorf("status printed the token itself:\n%s", out)
	}
}

// TestStatusNamesACredentialsFileItCannotRead is #63, at the surface a person
// reaches for first. The reproduction is the issue's: a credentials file with
// a valid entry and a line of damage after it. Before this, `ark status`
// dropped the resolution error and reported token_source "none" — the same
// answer it gives a machine that has never logged in, and the same advice
// ("run `ark login`"), which is the command that overwrites the file (#62).
//
// token_source is asserted to stay "none" as well, because --json is a stable
// interface: the diagnosis had to arrive beside it, not inside it.
func TestStatusNamesACredentialsFileItCannotRead(t *testing.T) {
	dir := gitRepo(t)
	home := logoutHome(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", "https://ark-status-unreadable.invalid")

	path := filepath.Join(home, ".ark", "credentials.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content := "[remotes.\"a.example.com\"]\ntoken = \"tok-a-precious\"\n\nthis line is not TOML\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	var rep struct {
		TokenSource      string `json:"token_source"`
		TokenSourceError string `json:"token_source_error"`
	}
	arkJSON(t, dir, &rep, "status")
	if rep.TokenSource != "none" {
		t.Errorf("token_source = %q, want none — nothing resolved, and the field is stable", rep.TokenSource)
	}
	if rep.TokenSourceError == "" {
		t.Fatal("token_source_error is empty: a damaged credentials file still reads as `never logged in`")
	}
	if !strings.Contains(rep.TokenSourceError, path) {
		t.Errorf("token_source_error %q does not name the file the user has to repair", rep.TokenSourceError)
	}
	if strings.Contains(rep.TokenSourceError, "tok-a-precious") {
		t.Errorf("status printed a token (spec §21): %q", rep.TokenSourceError)
	}

	// The human rendering carries the same sentence, and stops sending the
	// user to the command that would destroy the file.
	out := ark(t, dir, "status")
	if !strings.Contains(out, path) {
		t.Errorf("status output does not name the unreadable file:\n%s", out)
	}
	if strings.Contains(out, "none — run `ark login`") {
		t.Errorf("status still reports an unreadable file as `never logged in`:\n%s", out)
	}

	// And `ark sync` refuses in the same words. One condition described two
	// ways is how a user ends up believing they are two conditions.
	_, syncErr := arkErr(t, dir, "sync")
	if syncErr == nil {
		t.Fatal("ark sync succeeded against an unreadable credentials file")
	}
	if syncErr.Error() != rep.TokenSourceError {
		t.Errorf("status and sync word the same condition differently:\nstatus: %s\nsync:   %s",
			rep.TokenSourceError, syncErr)
	}
}

// TestStatusStaysQuietWhenNothingIsStored pins the case the field must not
// fire on, and it is nearly every machine: no credentials file at all. That is
// a first login, not damage, and token_source_error appearing there would make
// the ordinary state look like a fault.
func TestStatusStaysQuietWhenNothingIsStored(t *testing.T) {
	dir := gitRepo(t)
	home := logoutHome(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", "https://ark-status-fresh.invalid")

	if _, err := os.Stat(filepath.Join(home, ".ark", "credentials.toml")); !os.IsNotExist(err) {
		t.Fatalf("this test needs a home with no credentials file (%v)", err)
	}

	var rep struct {
		TokenSource      string `json:"token_source"`
		TokenSourceError string `json:"token_source_error"`
	}
	arkJSON(t, dir, &rep, "status")
	if rep.TokenSource != "none" {
		t.Errorf("token_source = %q, want none", rep.TokenSource)
	}
	if rep.TokenSourceError != "" {
		t.Errorf("token_source_error = %q on a machine that has simply never logged in", rep.TokenSourceError)
	}
	if out := ark(t, dir, "status"); !strings.Contains(out, "none — run `ark login`") {
		t.Errorf("status no longer tells a fresh machine what to do:\n%s", out)
	}
}

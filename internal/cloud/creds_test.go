package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// credsTestRemote uses an .invalid host (RFC 2606). The keyring is a fake in
// every test below, so this is belt and braces — but it means that a test
// added later which forgets to install the fake still cannot collide with a
// real credential on the developer's machine.
const credsTestRemote = "https://ark-creds-test.invalid"

// isolateHome gives a test its own HOME, an empty keyring, and no ARK_TOKEN,
// so token resolution can only find what that test plants.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// os.UserHomeDir uses USERPROFILE on Windows, not HOME. Set both so a
	// test run can never read or write the developer's real credentials.
	t.Setenv("USERPROFILE", home)
	t.Setenv("ARK_TOKEN", "")
	t.Setenv("ARK_NO_KEYRING", "")
	useFakeKeyring(t)
	return home
}

// TestTokenResolutionOrder: ARK_TOKEN beats the keyring beats the credentials
// file, and with nothing planted anywhere resolution fails with a permission
// error telling the user to log in (spec §20). Each step is added on top of
// the last, so every assertion is also a statement about precedence.
func TestTokenResolutionOrder(t *testing.T) {
	isolateHome(t)
	fake := testKeyring(t)
	host := RemoteHost(credsTestRemote)

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
	if err := writeFileToken(host, "from-file"); err != nil {
		t.Fatalf("write file token: %v", err)
	}
	assertResolves(t, "from-file", SourceFile)

	// The keyring outranks it.
	if err := fake.Set(keychainService, host, "from-keyring"); err != nil {
		t.Fatalf("plant keyring token: %v", err)
	}
	assertResolves(t, "from-keyring", SourceKeyring)

	// ARK_TOKEN beats everything.
	t.Setenv("ARK_TOKEN", "from-env")
	assertResolves(t, "from-env", SourceEnv)
}

// assertResolves checks both halves of a resolution: the token, and the store
// that `ark status` will name as having produced it.
func assertResolves(t *testing.T, wantToken string, wantSource TokenSource) {
	t.Helper()
	cred, err := ResolveCredential(credsTestRemote)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cred.Token != wantToken || cred.Source != wantSource {
		t.Fatalf("resolved %q from %q, want %q from %q",
			cred.Token, cred.Source, wantToken, wantSource)
	}
}

// TestKeyringFailureIsAnnouncedBeforeTheFallback: a keyring that is locked,
// denied, or absent must say so on stderr before resolution drops to reading
// a plaintext file. Falling through in silence is the defect this replaces,
// and the one the GitHub CLI maintainers regret (cli/cli#10108, #13317).
func TestKeyringFailureIsAnnouncedBeforeTheFallback(t *testing.T) {
	isolateHome(t)
	fake := testKeyring(t)
	fake.getErr = errors.New("the user name or passphrase you entered is not correct")
	warnings := captureWarnings(t)

	if err := writeFileToken(RemoteHost(credsTestRemote), "from-file"); err != nil {
		t.Fatal(err)
	}
	assertResolves(t, "from-file", SourceFile)

	got := warnings.String()
	if !strings.Contains(got, "OS keyring unavailable") {
		t.Errorf("warnings = %q, want it to report the keyring is unavailable", got)
	}
	if !strings.Contains(got, "not correct") {
		t.Errorf("warnings = %q, want the underlying keyring error", got)
	}
	if !strings.Contains(got, credentialsPath()) {
		t.Errorf("warnings = %q, want the fallback path named", got)
	}
}

// TestKeyringMissIsSilent: "no entry for this host" is the ordinary state of a
// machine that has never logged in. Warning about it would train people to
// ignore the warning that matters.
func TestKeyringMissIsSilent(t *testing.T) {
	isolateHome(t)
	warnings := captureWarnings(t)

	if err := writeFileToken(RemoteHost(credsTestRemote), "from-file"); err != nil {
		t.Fatal(err)
	}
	assertResolves(t, "from-file", SourceFile)

	if got := warnings.String(); got != "" {
		t.Errorf("warnings = %q, want none for an empty keyring", got)
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

// TestCredentialsFileIsRestrictedDespiteAStaleTempFile: an interrupted login
// can leave credentials.toml.tmp behind. O_CREATE does not re-apply its mode
// to a file that already exists — and on Windows the mode never configured the
// ACL in the first place — so the next login must restrict the temp file
// itself before the token goes into it, or it inherits the stale file's wider
// permissions all the way through the rename.
func TestCredentialsFileIsRestrictedDespiteAStaleTempFile(t *testing.T) {
	home := isolateHome(t)
	path := filepath.Join(home, ".ark", "credentials.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// A world-readable leftover from a login that died mid-write.
	if err := os.WriteFile(path+".tmp", []byte("remnant\n"), 0o666); err != nil {
		t.Fatal(err)
	}

	if err := writeFileToken("a.example.com", "tok-a"); err != nil {
		t.Fatal(err)
	}
	assertRestrictedCredentials(t, path)
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("credentials.toml.tmp survived the write (%v); a token file was left behind", err)
	}
	if got := fileToken("a.example.com"); got != "tok-a" {
		t.Errorf("a.example.com = %q, want tok-a", got)
	}
}

// TestStoreTokenPrefersTheKeyring: with a working keyring the token goes
// there and nowhere else — in particular, no plaintext file appears as a
// side effect (spec §20).
func TestStoreTokenPrefersTheKeyring(t *testing.T) {
	isolateHome(t)
	fake := testKeyring(t)
	warnings := captureWarnings(t)

	src, err := StoreToken(credsTestRemote, "stored-token")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if src != SourceKeyring {
		t.Errorf("stored in %q, want %q", src, SourceKeyring)
	}
	if got := fake.entries[fake.key(keychainService, RemoteHost(credsTestRemote))]; got != "stored-token" {
		t.Errorf("keyring holds %q, want stored-token", got)
	}
	if _, err := os.Stat(credentialsPath()); !os.IsNotExist(err) {
		t.Errorf("a credentials file exists (%v); the keyring took the token, so nothing should be on disk", err)
	}
	if got := warnings.String(); got != "" {
		t.Errorf("warnings = %q, want none when the keyring works", got)
	}
	assertResolves(t, "stored-token", SourceKeyring)
}

// TestStoreTokenWarnsThenFallsBackToTheCredentialsFile: a keyring that
// refuses the write must not cost the user their login, but it must also not
// put a token in a plaintext file without saying so (spec §20).
func TestStoreTokenWarnsThenFallsBackToTheCredentialsFile(t *testing.T) {
	isolateHome(t)
	testKeyring(t).setErr = errors.New("keyring is locked")
	warnings := captureWarnings(t)

	src, err := StoreToken(credsTestRemote, "stored-token")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if src != SourceFile {
		t.Errorf("stored in %q, want %q", src, SourceFile)
	}
	if src.Description() != credentialsPath() {
		t.Errorf("description = %q, want %q", src.Description(), credentialsPath())
	}
	assertRestrictedCredentials(t, credentialsPath())
	assertResolves(t, "stored-token", SourceFile)

	got := warnings.String()
	if !strings.Contains(got, "OS keyring unavailable") || !strings.Contains(got, "locked") {
		t.Errorf("warnings = %q, want the keyring failure reported", got)
	}
	if !strings.Contains(got, credentialsPath()) {
		t.Errorf("warnings = %q, want the file it fell back to named", got)
	}
}

// TestStoreTokenRemovesThePlaintextCopyItSupersedes: the upgrade path. Anyone
// who logged in before the keyring worked on their platform has a token in
// ~/.ark/credentials.toml. Once the keyring holds it, that file entry is a
// plaintext copy nothing will ever read again — the keyring outranks it — so
// leaving it behind would be a secret on disk with no purpose. Other remotes
// in the same file are not this login's business and stay.
func TestStoreTokenRemovesThePlaintextCopyItSupersedes(t *testing.T) {
	isolateHome(t)
	host := RemoteHost(credsTestRemote)

	if err := writeFileToken(host, "old-plaintext"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileToken("other.example.com", "someone-elses"); err != nil {
		t.Fatal(err)
	}

	if _, err := StoreToken(credsTestRemote, "new-token"); err != nil {
		t.Fatalf("store: %v", err)
	}
	if got := fileToken(host); got != "" {
		t.Errorf("credentials file still holds %q for %s", got, host)
	}
	if got := fileToken("other.example.com"); got != "someone-elses" {
		t.Errorf("other.example.com = %q, want someone-elses (an unrelated remote was dropped)", got)
	}
	assertResolves(t, "new-token", SourceKeyring)
}

// TestStoreTokenRemovesTheCredentialsFileOnceItIsEmpty: the same, for the last
// remote in the file. An empty credentials.toml is litter that reads, to
// anyone auditing the machine later, like a credential store still in use.
func TestStoreTokenRemovesTheCredentialsFileOnceItIsEmpty(t *testing.T) {
	isolateHome(t)

	if err := writeFileToken(RemoteHost(credsTestRemote), "old-plaintext"); err != nil {
		t.Fatal(err)
	}
	if _, err := StoreToken(credsTestRemote, "new-token"); err != nil {
		t.Fatalf("store: %v", err)
	}
	if _, err := os.Stat(credentialsPath()); !os.IsNotExist(err) {
		t.Errorf("credentials file survives with nothing in it (%v)", err)
	}
}

// TestNoKeyringOptsOutEntirely: ARK_NO_KEYRING makes the file the deliberate
// choice, so the keyring is neither read nor written — and because the user
// asked for this, it happens without a warning.
func TestNoKeyringOptsOutEntirely(t *testing.T) {
	isolateHome(t)
	fake := testKeyring(t)
	host := RemoteHost(credsTestRemote)
	if err := fake.Set(keychainService, host, "in-the-keyring"); err != nil {
		t.Fatal(err)
	}

	t.Setenv("ARK_NO_KEYRING", "1")
	warnings := captureWarnings(t)

	src, err := StoreToken(credsTestRemote, "stored-token")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	if src != SourceFile {
		t.Errorf("stored in %q, want %q", src, SourceFile)
	}
	if got := fake.entries[fake.key(keychainService, host)]; got != "in-the-keyring" {
		t.Errorf("keyring entry changed to %q; ARK_NO_KEYRING must not write to it", got)
	}
	// The keyring holds a different token, so this also proves it was skipped.
	assertResolves(t, "stored-token", SourceFile)
	if got := warnings.String(); got != "" {
		t.Errorf("warnings = %q, want none for a deliberate opt-out", got)
	}
}

// TestTokenSourceDescriptions: `ark login` and `ark status` print these, so
// each source has to name a place rather than leak a token or print a blank.
func TestTokenSourceDescriptions(t *testing.T) {
	isolateHome(t)
	cases := []struct {
		source TokenSource
		want   string
	}{
		{SourceEnv, "ARK_TOKEN"},
		{SourceKeyring, keyringName()},
		{SourceFile, credentialsPath()},
		{SourceNone, "nowhere"},
	}
	for _, tc := range cases {
		if got := tc.source.Description(); got != tc.want {
			t.Errorf("%q.Description() = %q, want %q", tc.source, got, tc.want)
		}
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

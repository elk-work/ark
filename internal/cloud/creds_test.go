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

// storedFileToken reads one host's entry out of the fallback file. Reading it
// can fail now — a file that will not decode is a state of its own rather than
// an empty one — and every caller below has just written the file itself, so a
// failure there is the test's own setup coming apart, not the case under test.
func storedFileToken(t *testing.T, host string) string {
	t.Helper()
	tok, err := fileToken(host)
	if err != nil {
		t.Fatalf("read the credentials file for %s: %v", host, err)
	}
	return tok
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
	if got := storedFileToken(t, "a.example.com"); got != "tok-a2" {
		t.Errorf("a.example.com = %q, want tok-a2", got)
	}
	if got := storedFileToken(t, "b.example.com"); got != "tok-b" {
		t.Errorf("b.example.com = %q, want tok-b (clobbered by rewrite)", got)
	}

	assertRestrictedCredentials(t, filepath.Join(home, ".ark", "credentials.toml"))
}

// corruptCredentialsFile plants the file from #62: one host's token, intact
// and precious, followed by the sort of damage an interrupted write or a
// one-character hand edit leaves behind. It returns the exact bytes, because
// every test using it asserts they are still on disk afterwards.
func corruptCredentialsFile(t *testing.T) (path, content string) {
	t.Helper()
	path = credentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	content = "[remotes.\"a.example.com\"]\ntoken = \"tok-a-precious\"\n\nthis line is not TOML\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, content
}

// TestLoginRefusesToOverwriteACredentialsFileItCannotRead is #62 itself: a
// valid entry for one host, a line of damage after it, and a login for a
// different host. The read that preserves other remotes used to be best
// effort, so the one run where it failed was the one run where the write
// replaced the file with a single entry — and `ark login` said it had worked.
//
// The load-bearing assertion is the last one. Everything else here is about
// how the refusal reads; that one is about whether tok-a-precious still exists.
func TestLoginRefusesToOverwriteACredentialsFileItCannotRead(t *testing.T) {
	isolateHome(t)
	// The fallback file is only reached when the keyring is out of the way,
	// which is the machine this bug lives on: no Secret Service, or an
	// operator who chose the file.
	t.Setenv("ARK_NO_KEYRING", "1")
	path, before := corruptCredentialsFile(t)

	src, err := StoreToken("https://b.example.com", "tok-b")
	if err == nil {
		t.Fatalf("login stored the token in %q over a credentials file it could not read", src)
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	if src != SourceNone {
		t.Errorf("source = %q on a refused login, want %q", src, SourceNone)
	}
	// A user with a corrupt file now has a command that refuses, so the error
	// has to name the file and leave them somewhere to go.
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the file the user has to repair", err)
	}
	if !strings.Contains(err.Error(), "ark login") {
		t.Errorf("error %q stops the user without saying what to do next", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the credentials file is gone: %v", err)
	}
	if string(after) != before {
		t.Fatalf("the credentials file was rewritten; a.example.com's token is unrecoverable\n--- before\n%s--- after\n%s", before, after)
	}
	// writeCredentials was never reached, so nothing should have been staged
	// beside the file either — a stray .tmp is a token on disk with no owner.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("credentials.toml.tmp was left behind (%v); a refused login must not write at all", err)
	}
}

// TestACredentialsFileThatWillNotReadIsNotNoCredentials: the same file on the
// resolution path. Returning "" for a failed decode made a corrupt file
// indistinguishable from an empty one, so `ark status` and `ark sync` reported
// no credential and sent the user to `ark login` — which, until the fix above,
// was the command that destroyed the file. Exit 5 either way, so nothing
// scripting against the code changes; what changes is that the message is
// about the file rather than about a credential that was never looked for.
func TestACredentialsFileThatWillNotReadIsNotNoCredentials(t *testing.T) {
	isolateHome(t)
	path, _ := corruptCredentialsFile(t)

	_, err := ResolveCredential(credsTestRemote)
	if err == nil {
		t.Fatal("resolution succeeded against a credentials file that does not parse")
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the unreadable file; it reads as `nothing is stored`", err)
	}
	// And matchable without reading the message, so `ark status` can report
	// this state without matching on prose (#63).
	if !errors.Is(err, ErrCredentialsUnreadable) {
		t.Errorf("error %v does not match ErrCredentialsUnreadable", err)
	}
}

// TestAMissingCredentialsFileIsAnOrdinaryFirstLogin is the other half of the
// distinction, and the reason the fix cannot be "treat a failed decode as an
// error" and stop. Nearly every machine reaching this code has no credentials
// file at all; that is the first login, and it must stay silent and succeed.
// toml.DecodeFile opens the file itself, so absent arrives as ENOENT and
// corrupt as a parse error — which is the only reason the two can be told
// apart without stat-ing the path separately and racing the read.
func TestAMissingCredentialsFileIsAnOrdinaryFirstLogin(t *testing.T) {
	isolateHome(t)
	t.Setenv("ARK_NO_KEYRING", "1")
	warnings := captureWarnings(t)
	if _, err := os.Stat(credentialsPath()); !os.IsNotExist(err) {
		t.Fatalf("this test needs a home with no credentials file (%v)", err)
	}

	_, err := ResolveCredential(credsTestRemote)
	if err == nil {
		t.Fatal("resolution succeeded with nothing stored anywhere")
	}
	if strings.Contains(err.Error(), "could not be read") {
		t.Errorf("a file that is simply not there was reported as unreadable: %v", err)
	}
	// The other direction of the sentinel: absence must not match, or every
	// caller that keys on it treats a first login as damage.
	if errors.Is(err, ErrCredentialsUnreadable) {
		t.Errorf("an absent credentials file matched ErrCredentialsUnreadable: %v", err)
	}

	src, err := StoreToken(credsTestRemote, "first-token")
	if err != nil {
		t.Fatalf("the first login on a machine with no credentials file failed: %v", err)
	}
	if src != SourceFile {
		t.Errorf("stored in %q, want %q", src, SourceFile)
	}
	assertResolves(t, "first-token", SourceFile)
	if got := warnings.String(); got != "" {
		t.Errorf("warnings = %q, want none for a deliberate opt-out and a first login", got)
	}
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
	if got := storedFileToken(t, "a.example.com"); got != "tok-a" {
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
	if got := storedFileToken(t, host); got != "" {
		t.Errorf("credentials file still holds %q for %s", got, host)
	}
	if got := storedFileToken(t, "other.example.com"); got != "someone-elses" {
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

// TestRemoveTokenClearsBothStores is the central claim of `ark logout`: the
// keyring entry and the plaintext copy both go, and no other host's does.
// Removing only the store that currently answers would leave the file copy
// behind on every machine that logged in before the keyring worked there —
// unread, because the keyring outranks it, and therefore invisible.
func TestRemoveTokenClearsBothStores(t *testing.T) {
	isolateHome(t)
	fake := testKeyring(t)
	host := RemoteHost(credsTestRemote)

	if err := fake.Set(keychainService, host, "in-the-keyring"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileToken(host, "in-the-file"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileToken("other.example.com", "someone-elses"); err != nil {
		t.Fatal(err)
	}

	rem, err := RemoveToken(credsTestRemote)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if rem.Host != host {
		t.Errorf("host = %q, want %q", rem.Host, host)
	}
	assertRemovedFrom(t, rem, SourceKeyring, SourceFile)
	if _, ok := fake.entries[fake.key(keychainService, host)]; ok {
		t.Error("the keyring still holds the token")
	}
	if got := storedFileToken(t, host); got != "" {
		t.Errorf("the credentials file still holds %q for %s", got, host)
	}
	if got := storedFileToken(t, "other.example.com"); got != "someone-elses" {
		t.Errorf("other.example.com = %q, want someone-elses; logout is host-scoped", got)
	}
	if _, err := ResolveCredential(credsTestRemote); err == nil {
		t.Error("a token still resolves after logout")
	}
}

// TestRemoveTokenTakesTheCredentialsFileWithTheLastEntry: an empty
// credentials.toml is litter that reads, to anyone auditing the machine later,
// like a credential store still in use. Login already cleans it up; logout has
// the same duty and more reason.
func TestRemoveTokenTakesTheCredentialsFileWithTheLastEntry(t *testing.T) {
	isolateHome(t)
	if err := writeFileToken(RemoteHost(credsTestRemote), "in-the-file"); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveToken(credsTestRemote); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(credentialsPath()); !os.IsNotExist(err) {
		t.Errorf("credentials file survives with nothing in it (%v)", err)
	}
}

// TestRemoveTokenOnAHostWithNothingStoredSucceeds: logout is idempotent, and
// the postcondition a caller wants — this machine holds no credential for that
// host — already holds. Exit 3 would make a teardown script fail on a machine
// that was never logged in, and the fix people reach for is `|| true`, which
// also swallows the keyring genuinely refusing to give a token up.
func TestRemoveTokenOnAHostWithNothingStoredSucceeds(t *testing.T) {
	isolateHome(t)

	rem, err := RemoveToken(credsTestRemote)
	if err != nil {
		t.Fatalf("logging out of a host with nothing stored failed: %v", err)
	}
	assertRemovedFrom(t, rem)
	if rem.KeyringSkipped {
		t.Error("KeyringSkipped is set without ARK_NO_KEYRING")
	}
}

// TestRemoveTokenClearsTheFileEvenWhenTheKeyringRefuses: the two stores are
// independent, and the one reached second is the plaintext one. Stopping at
// the first error would leave exactly the copy this command exists to remove,
// on the run where the user is most likely to assume it worked.
func TestRemoveTokenClearsTheFileEvenWhenTheKeyringRefuses(t *testing.T) {
	isolateHome(t)
	host := RemoteHost(credsTestRemote)
	testKeyring(t).delErr = errors.New("keyring is locked")
	if err := writeFileToken(host, "in-the-file"); err != nil {
		t.Fatal(err)
	}

	rem, err := RemoveToken(credsTestRemote)
	if err == nil {
		t.Fatal("a keyring that refused the delete must not report a clean logout")
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	// Named precisely enough that a person can finish the removal by hand,
	// which is the state this command exists to spare them.
	for _, want := range []string{keyringName(), keychainService, host} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	assertRemovedFrom(t, rem, SourceFile)
	if got := storedFileToken(t, host); got != "" {
		t.Errorf("the plaintext copy survived the keyring failure: %q", got)
	}
}

// TestRemoveTokenReportsAnUnreadableCredentialsFile: a credentials.toml that
// will not parse may hold this host's token in bytes nothing here can read, so
// the one thing logout must not do is call it "nothing to remove".
func TestRemoveTokenReportsAnUnreadableCredentialsFile(t *testing.T) {
	isolateHome(t)
	path := credentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("this is not TOML = = =\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	rem, err := RemoveToken(credsTestRemote)
	if err == nil {
		t.Fatal("an unparseable credentials file must not report a clean logout")
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code = %d, want 5 (permission): %v", code, err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name %q", err, path)
	}
	assertRemovedFrom(t, rem)
}

// TestRemoveTokenSaysWhenARKNoKeyringKeptItOutOfTheKeyring: storage honours the
// opt-out in silence because the operator asked for it. Removal cannot — an
// empty result would otherwise be a claim about a store nobody looked in, and a
// token stored before the variable was set would sit there through a logout
// that reported success.
func TestRemoveTokenSaysWhenARKNoKeyringKeptItOutOfTheKeyring(t *testing.T) {
	isolateHome(t)
	fake := testKeyring(t)
	host := RemoteHost(credsTestRemote)
	if err := fake.Set(keychainService, host, "in-the-keyring"); err != nil {
		t.Fatal(err)
	}
	if err := writeFileToken(host, "in-the-file"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARK_NO_KEYRING", "1")

	rem, err := RemoveToken(credsTestRemote)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !rem.KeyringSkipped {
		t.Error("KeyringSkipped is false; the caller cannot tell the keyring went unexamined")
	}
	assertRemovedFrom(t, rem, SourceFile)
	if got := fake.entries[fake.key(keychainService, host)]; got != "in-the-keyring" {
		t.Errorf("keyring entry is now %q; ARK_NO_KEYRING must keep the keyring untouched", got)
	}
}

// TestRemoveTokenReportsThatARKTokenStillResolves: no process can unset a
// variable in the shell that started it, so the stores can be empty while every
// remote still authenticates. Reporting a clean logout there is the divergence
// shape of #46 and #58 — a command describing a state it has not got — so the
// fact travels in the result, and `ark logout` turns it into exit 7.
func TestRemoveTokenReportsThatARKTokenStillResolves(t *testing.T) {
	isolateHome(t)
	if err := writeFileToken(RemoteHost(credsTestRemote), "in-the-file"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARK_TOKEN", "from-env")

	rem, err := RemoveToken(credsTestRemote)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !rem.EnvToken {
		t.Fatal("EnvToken is false while ARK_TOKEN is set")
	}
	assertRemovedFrom(t, rem, SourceFile)
	// The stores are empty and resolution still succeeds. That is the whole
	// point: removal did everything it could and the machine is not logged out.
	cred, err := ResolveCredential(credsTestRemote)
	if err != nil || cred.Source != SourceEnv {
		t.Fatalf("resolved %q from %q (%v), want the env token to still answer", cred.Token, cred.Source, err)
	}
}

// assertRemovedFrom checks the stores a removal reports, in order. The order is
// resolution order, which is what makes the sentence `ark logout` prints read
// the way the lookup works.
func assertRemovedFrom(t *testing.T, rem Removal, want ...TokenSource) {
	t.Helper()
	if len(rem.From) != len(want) {
		t.Fatalf("removed from %v, want %v", rem.From, want)
	}
	for i := range want {
		if rem.From[i] != want[i] {
			t.Fatalf("removed from %v, want %v", rem.From, want)
		}
	}
}

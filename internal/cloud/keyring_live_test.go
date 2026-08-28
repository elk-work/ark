package cloud

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The tests in this file are the only ones in the package that reach the
// machine's real credential store. Everything else substitutes a fake, for the
// reasons keyring_test.go gives — and that is what leaves a gap: a fake proves
// the resolution order, not that Windows Credential Manager or the freedesktop
// Secret Service will hand a secret back. Until these ran in CI, wincred and
// D-Bus were proven to compile, vet and pass, and nothing more.
//
// The gate is an environment variable rather than a build tag on purpose. A
// tagged file is invisible to `go build ./...`, `go vet ./...` and an ordinary
// `go test`, so it rots unnoticed on the platforms nobody develops on — which
// is the exact failure this file exists to close. Gating at run time keeps the
// code compiled and vetted in every job on every platform, and leaves running
// it a matter of one variable rather than a flag each caller must remember.
const liveKeyringEnv = "ARK_KEYRING_LIVE"

// requireLiveKeyring skips unless the run has opted in. Default-off is not
// timidity: `go test ./...` on a laptop would otherwise write to the
// developer's own login keychain, and on macOS the first such write is a modal
// prompt in the middle of a test run.
func requireLiveKeyring(t *testing.T) {
	t.Helper()
	if v := strings.TrimSpace(os.Getenv(liveKeyringEnv)); v == "" || v == "0" {
		t.Skipf("set %s=1 to drive this machine's real OS keyring (%s)", liveKeyringEnv, keyringName())
	}
}

// liveKeyringService returns a throwaway service name. It is deliberately not
// keychainService ("ark"): these tests write into a real credential store, and
// filing under the name real logins use would put a test entry one careless
// Delete away from someone's working credential. The random suffix also keeps
// two runs against the same user account — a CI job and a laptop, or two jobs
// on one self-hosted box — out of each other's entries.
func liveKeyringService() string {
	return "ark-live-keyring-test-" + strings.ToLower(rand.Text())
}

// These tests do not isolate HOME the way isolateHome does, and must not. On
// macOS the keychain a `security` call reaches belongs to the logged-in user's
// session; moving HOME underneath it either fails outright or writes into a
// keychain nobody opened, which would turn a live test into a fake one. Safety
// comes from the names instead: a random service, a random .invalid host, and
// a Cleanup that removes both whichever way the test ends.

// TestLiveKeyringRoundTrip drives osKeyring itself — Set, Get, overwrite,
// Delete, miss — against whatever store this platform actually has.
func TestLiveKeyringRoundTrip(t *testing.T) {
	requireLiveKeyring(t)

	store := osKeyring{}
	service := liveKeyringService()
	const account = "round-trip.invalid"
	secret := "ark-live-secret-" + rand.Text()

	t.Cleanup(func() {
		if err := store.Delete(service, account); err != nil && !errors.Is(err, errNoKeyringEntry) {
			t.Errorf("cleanup: %s still holds %s:%s: %v", keyringName(), service, account, err)
		}
	})

	// Before anything is written, so that whatever Get returns below is this
	// run's secret and not a leftover from a previous one.
	if _, err := store.Get(service, account); !errors.Is(err, errNoKeyringEntry) {
		t.Fatalf("Get before Set = %v, want errNoKeyringEntry", err)
	}

	if err := store.Set(service, account, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := store.Get(service, account)
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if got != secret {
		t.Fatalf("Get = %q, want %q", got, secret)
	}

	// Logging in twice is ordinary, and on the Secret Service it is the
	// interesting case: the backend creates an item rather than updating one
	// and Get takes the first search hit, so a store that appends instead of
	// replacing would keep authenticating with the superseded token.
	replacement := "ark-live-secret-" + rand.Text()
	if err := store.Set(service, account, replacement); err != nil {
		t.Fatalf("Set over an existing entry: %v", err)
	}
	got, err = store.Get(service, account)
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	if got != replacement {
		t.Fatalf("Get = %q after overwrite, want %q; the store kept the superseded secret", got, replacement)
	}

	if err := store.Delete(service, account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// A deleted entry must read as a miss, not as a failure: resolution treats
	// the two differently, and only a miss falls through to the file quietly.
	if _, err := store.Get(service, account); !errors.Is(err, errNoKeyringEntry) {
		t.Fatalf("Get after Delete = %v, want errNoKeyringEntry", err)
	}
}

// liveHelperServiceEnv names the service a re-executed test binary should read
// from. Its presence is also what tells that binary it is the helper.
const liveHelperServiceEnv = "ARK_KEYRING_LIVE_HELPER_SERVICE"

// liveHelperAccount and liveHelperMarker are the helper's side of the
// contract: which entry to read, and how the parent finds the answer in the
// subprocess's output.
const (
	liveHelperAccount = "cross-process.invalid"
	liveHelperMarker  = "ARK-LIVE-SECRET:"
)

// TestLiveKeyringSecretCrossesProcesses proves the secret reached the OS store
// rather than a cache inside this process: a second process, sharing nothing
// but the user account, reads back what this one wrote. That is the property
// `ark login` in one terminal and `ark sync` in another depends on, and the
// one a fake keyring can never demonstrate.
func TestLiveKeyringSecretCrossesProcesses(t *testing.T) {
	requireLiveKeyring(t)

	store := osKeyring{}
	service := liveKeyringService()
	secret := "ark-live-secret-" + rand.Text()

	t.Cleanup(func() {
		if err := store.Delete(service, liveHelperAccount); err != nil && !errors.Is(err, errNoKeyringEntry) {
			t.Errorf("cleanup: %s still holds %s:%s: %v", keyringName(), service, liveHelperAccount, err)
		}
	})
	if err := store.Set(service, liveHelperAccount, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// os.Args[0] is this test binary; re-running it with one test selected is
	// cheaper and more portable than building a helper command.
	cmd := exec.Command(os.Args[0], "-test.run=TestLiveKeyringHelperRead")
	cmd.Env = append(os.Environ(), liveHelperServiceEnv+"="+service)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper process: %v\n%s", err, out)
	}
	if want := liveHelperMarker + secret; !strings.Contains(string(out), want) {
		t.Fatalf("helper did not read the secret back out of %s; output:\n%s", keyringName(), out)
	}
}

// TestLiveKeyringHelperRead is the subprocess half of the test above. It is a
// test only because that is how the binary re-enters itself; without the
// helper variable set it is not this run's business and skips.
func TestLiveKeyringHelperRead(t *testing.T) {
	service := os.Getenv(liveHelperServiceEnv)
	if service == "" {
		t.Skipf("helper for TestLiveKeyringSecretCrossesProcesses; %s is unset", liveHelperServiceEnv)
	}
	secret, err := osKeyring{}.Get(service, liveHelperAccount)
	if err != nil {
		t.Fatalf("helper Get: %v", err)
	}
	fmt.Println(liveHelperMarker + secret)
}

// liveScopedKeyring is the real store, addressed under a service name of the
// test's choosing. StoreToken and ResolveCredential hardcode keychainService,
// rightly — it is the name a real credential lives at — so this is what lets
// the production path be driven end to end against a live store without a test
// ever writing under that name.
type liveScopedKeyring struct{ service string }

func (k liveScopedKeyring) Get(_, account string) (string, error) {
	return osKeyring{}.Get(k.service, account)
}

func (k liveScopedKeyring) Set(_, account, secret string) error {
	return osKeyring{}.Set(k.service, account, secret)
}

func (k liveScopedKeyring) Delete(_, account string) error {
	return osKeyring{}.Delete(k.service, account)
}

// TestLiveLoginThenResolveUsesTheKeyring is the `ark login` / `ark status`
// pair, run against the platform's real credential store: the token goes into
// the keyring with nothing said on stderr, and resolution finds it there
// rather than in a file. The fake-keyring tests assert the same behaviour; the
// point of repeating it here is that this time the keyring is the OS's.
func TestLiveLoginThenResolveUsesTheKeyring(t *testing.T) {
	requireLiveKeyring(t)

	service := liveKeyringService()
	remote := "https://" + strings.ToLower(rand.Text()) + ".invalid"
	host := RemoteHost(remote)
	token := "ark-live-token-" + rand.Text()

	t.Setenv("ARK_TOKEN", "")
	t.Setenv("ARK_NO_KEYRING", "")
	previous := credentialKeyring
	credentialKeyring = liveScopedKeyring{service: service}
	t.Cleanup(func() { credentialKeyring = previous })
	t.Cleanup(func() {
		if err := (osKeyring{}).Delete(service, host); err != nil && !errors.Is(err, errNoKeyringEntry) {
			t.Errorf("cleanup: %s still holds %s:%s: %v", keyringName(), service, host, err)
		}
	})
	// If the keyring refuses the write, StoreToken's contract is to fall back
	// to the credentials file — the real one, since HOME is deliberately left
	// alone here. Take this host's entry back out either way.
	t.Cleanup(func() { _ = clearFileToken(host) })

	warnings := captureWarnings(t)

	source, err := StoreToken(remote, token)
	if err != nil {
		t.Fatalf("`ark login`: %v", err)
	}
	if source != SourceKeyring {
		t.Fatalf("`ark login` stored the token in %q, want %q — %s refused the write: %s",
			source, SourceKeyring, keyringName(), warnings)
	}
	if got := warnings.String(); got != "" {
		t.Errorf("warnings = %q, want none when the keyring works", got)
	}

	cred, err := ResolveCredential(remote)
	if err != nil {
		t.Fatalf("`ark status`: %v", err)
	}
	if cred.Token != token || cred.Source != SourceKeyring {
		t.Fatalf("resolved %q from %q, want the stored token from %q", cred.Token, cred.Source, SourceKeyring)
	}
	if got := cred.Source.Description(); got != keyringName() {
		t.Errorf("`ark status` would name %q, want %q", got, keyringName())
	}
}

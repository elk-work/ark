package cloud

import (
	"bytes"
	"errors"
	"testing"
)

// fakeKeyring stands in for the OS credential store. Every credential test
// installs one, for two reasons: the suite must not be able to read or write
// the developer's real keychain on any platform, and a locked or denied
// keyring is not something CI can arrange for real.
type fakeKeyring struct {
	entries map[string]string
	getErr  error // returned by Get instead of an entry
	setErr  error // returned by Set instead of storing
	delErr  error // returned by Delete instead of removing
}

func (k *fakeKeyring) key(service, account string) string { return service + "\x00" + account }

func (k *fakeKeyring) Get(service, account string) (string, error) {
	if k.getErr != nil {
		return "", k.getErr
	}
	secret, ok := k.entries[k.key(service, account)]
	if !ok {
		return "", errNoKeyringEntry
	}
	return secret, nil
}

func (k *fakeKeyring) Set(service, account, secret string) error {
	if k.setErr != nil {
		return k.setErr
	}
	k.entries[k.key(service, account)] = secret
	return nil
}

// Delete mirrors what all three real backends do, including the part that is
// easy to get wrong: removing an account that is not there is ErrNotFound, not
// success. `ark logout` leans on that distinction — a miss is the ordinary
// state of a machine that never logged in, and a failure means the OS still
// holds the credential — so a fake that answered nil to everything would let
// the two cases be conflated and never say a word.
func (k *fakeKeyring) Delete(service, account string) error {
	if k.delErr != nil {
		return k.delErr
	}
	key := k.key(service, account)
	if _, ok := k.entries[key]; !ok {
		return errNoKeyringEntry
	}
	delete(k.entries, key)
	return nil
}

// useFakeKeyring swaps the package's keyring for an empty fake and restores
// the real one afterwards.
func useFakeKeyring(t *testing.T) *fakeKeyring {
	t.Helper()
	fake := &fakeKeyring{entries: map[string]string{}}
	previous := credentialKeyring
	credentialKeyring = fake
	t.Cleanup(func() { credentialKeyring = previous })
	return fake
}

// testKeyring hands back the fake isolateHome installed, for the tests that
// need to plant an entry in it or make it fail.
func testKeyring(t *testing.T) *fakeKeyring {
	t.Helper()
	fake, ok := credentialKeyring.(*fakeKeyring)
	if !ok {
		t.Fatal("no fake keyring installed — call isolateHome first, or this test would reach the real one")
	}
	return fake
}

// captureWarnings redirects this package's stderr warnings for one test, so a
// test can assert that a fallback was announced rather than silent.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := warnTo
	warnTo = &buf
	t.Cleanup(func() { warnTo = previous })
	return &buf
}

// TestKeyringDisabledParsing: ARK_NO_KEYRING is an opt-out, so the values a
// person plausibly types to mean "no" must not turn it on.
func TestKeyringDisabledParsing(t *testing.T) {
	cases := []struct {
		value string
		want  bool
	}{
		{"", false},
		{"0", false},
		{"false", false},
		{"FALSE", false},
		{"1", true},
		{"true", true},
		{"yes", true},
		{" 1 ", true},
	}
	for _, tc := range cases {
		t.Setenv("ARK_NO_KEYRING", tc.value)
		if got := keyringDisabled(); got != tc.want {
			t.Errorf("ARK_NO_KEYRING=%q: keyringDisabled() = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// TestKeyringNameIsAlwaysSomethingYouCanOpen: the warnings name the store, so
// the name must never come out empty on any platform this builds for.
func TestKeyringNameIsAlwaysSomethingYouCanOpen(t *testing.T) {
	if keyringName() == "" {
		t.Fatal("keyringName() is empty; a warning would read `OS keyring unavailable ()`")
	}
}

// TestKeyringMissIsNotAnError: "the keyring holds nothing for this host" is a
// miss, and a miss must be distinguishable from a failure — everything else
// in this file depends on that line.
func TestKeyringMissIsNotAnError(t *testing.T) {
	fake := useFakeKeyring(t)
	_, err := fake.Get(keychainService, "nothing.example.com")
	if !errors.Is(err, errNoKeyringEntry) {
		t.Fatalf("miss returned %v, want errNoKeyringEntry", err)
	}
}

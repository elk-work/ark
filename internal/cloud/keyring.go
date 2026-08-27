package cloud

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/zalando/go-keyring"
)

// keychainService is the service name every Ark credential is filed under in
// the OS keyring. The account is the sync service's host, which is what makes
// one `ark login` cover every repository pointing at that service.
const keychainService = "ark"

// keyringStore is the OS credential store. It is an interface, and a package
// variable, for one reason: the test suite must not be able to read or write
// the developer's real keychain on any platform, and substituting a fake is
// the only way to exercise the keyring branches deterministically in CI.
type keyringStore interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

// osKeyring is the real store: macOS Keychain, Windows Credential Manager, or
// the freedesktop Secret Service, whichever this build has. go-keyring reaches
// all three in pure Go — no cgo, which is a hard invariant here — and on macOS
// it feeds `security -i` on stdin, so the token never appears in argv where
// every other user on the machine can read it out of the process table.
type osKeyring struct{}

func (osKeyring) Get(service, account string) (string, error) {
	return keyring.Get(service, account)
}

func (osKeyring) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}

func (osKeyring) Delete(service, account string) error {
	return keyring.Delete(service, account)
}

var credentialKeyring keyringStore = osKeyring{}

// errNoKeyringEntry means the keyring answered and holds nothing for this
// account. That is an ordinary miss, not a failure: resolution moves on to the
// fallback file without a word. Every other error is a failure, and failures
// are loud — see warnKeyringRead.
var errNoKeyringEntry = keyring.ErrNotFound

// keyringName is what the keyring is called on this platform, so a message can
// name something the reader can go and open.
func keyringName() string {
	switch runtime.GOOS {
	case "darwin":
		return "macOS keychain"
	case "windows":
		return "Windows Credential Manager"
	case "linux":
		return "Secret Service keyring"
	default:
		return "OS keyring"
	}
}

// keyringDisabled reports whether ARK_NO_KEYRING opts this machine out. It
// exists for the hosts where consulting the keyring is worse than reading the
// file: a headless box with no Secret Service, a CI image where a prompt would
// hang, a shared account. Opting out is deliberate, so it is also quiet — the
// warnings below are for a keyring that was meant to work and did not.
func keyringDisabled() bool {
	v := strings.TrimSpace(os.Getenv("ARK_NO_KEYRING"))
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// warnTo receives the warnings this package must not swallow. A variable so
// tests can capture them; nothing outside the package should reassign it.
var warnTo io.Writer = os.Stderr

// warnKeyringRead says the keyring did not answer, and that the token is about
// to be looked for in a plaintext file instead. Silence here is the defect
// being fixed: a locked or denied keychain used to fall straight through to
// the file with no word to anyone, which is the behaviour the GitHub CLI
// maintainers now regret (cli/cli#10108, cli/cli#13317).
func warnKeyringRead(err error) {
	fmt.Fprintf(warnTo, "ark: OS keyring unavailable (%s): %v\n", keyringName(), err)
	fmt.Fprintf(warnTo, "ark: looking for the token in %s instead\n", credentialsPath())
}

// warnKeyringWrite says the token could not go into the keyring, and where it
// is going instead. `ark login` names its destination either way; this is the
// part the user did not ask for and must not have to infer.
func warnKeyringWrite(err error) {
	fmt.Fprintf(warnTo, "ark: OS keyring unavailable (%s): %v\n", keyringName(), err)
	fmt.Fprintf(warnTo, "ark: storing the token in %s instead — plaintext, protected by %s\n",
		credentialsPath(), credentialFileProtection)
}

package cli

import (
	"os"
	"testing"
)

// TestMain fences this package's tests off from the developer's real
// credentials. `ark status` and `ark sync` resolve a sync token, and
// resolution reaches the OS keyring and then the credentials file — so
// without this, a test that configures a remote and forgets to set ARK_TOKEN
// would read the machine's actual keychain and ~/.ark/credentials.toml. The
// sync tests do set ARK_TOKEN and never get that far; this is here for the
// next test that does not.
//
// os.UserHomeDir reads USERPROFILE on Windows and HOME everywhere else, so
// both are redirected. A test that wants the real resolution path has to
// clear ARK_NO_KEYRING and point HOME/USERPROFILE somewhere of its own.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "ark-cli-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	os.Setenv("USERPROFILE", home)
	os.Setenv("ARK_NO_KEYRING", "1")

	code := m.Run()

	os.RemoveAll(home)
	os.Exit(code)
}

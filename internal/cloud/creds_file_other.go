//go:build !windows

package cloud

import "os"

// credentialFileProtection names, for a warning message, what stands between
// the fallback file and everyone else on this machine.
const credentialFileProtection = "mode 0600"

func restrictCredentialFile(path string) error {
	return os.Chmod(path, 0o600)
}

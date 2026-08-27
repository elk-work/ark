//go:build !windows

package cloud

import "os"

func restrictCredentialFile(path string) error {
	return os.Chmod(path, 0o600)
}

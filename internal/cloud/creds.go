package cloud

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/ijroth/ark/internal/records"
)

// Token resolution order (docs/v1-spec.md §20): environment variable, OS
// keychain, then a restricted credentials file. Tokens never live in
// .ark/config.toml.

const keychainService = "ark"

// remoteHost normalizes a remote URL to its host for credential lookup.
func remoteHost(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return remote
	}
	return u.Host
}

// ResolveToken finds the API token for a remote.
func ResolveToken(remote string) (string, error) {
	if tok := os.Getenv("ARK_TOKEN"); tok != "" {
		return tok, nil
	}
	host := remoteHost(remote)
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("security", "find-generic-password",
			"-s", keychainService, "-a", host, "-w").Output()
		if err == nil {
			if tok := strings.TrimSpace(string(out)); tok != "" {
				return tok, nil
			}
		}
	}
	if tok := fileToken(host); tok != "" {
		return tok, nil
	}
	return "", &records.Error{Kind: records.KindPermission,
		Message: fmt.Sprintf("no credentials for %s (run `ark login`, or set ARK_TOKEN)", host)}
}

// StoreToken saves the token in the OS keychain (macOS) or the credentials
// file (other platforms). Returns a description of where it went.
func StoreToken(remote, token string) (string, error) {
	host := remoteHost(remote)
	if runtime.GOOS == "darwin" {
		// -U updates an existing item in place.
		err := exec.Command("security", "add-generic-password",
			"-s", keychainService, "-a", host, "-w", token, "-U").Run()
		if err == nil {
			return "macOS keychain", nil
		}
	}
	if err := writeFileToken(host, token); err != nil {
		return "", err
	}
	return credentialsPath(), nil
}

func credentialsPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".ark", "credentials.toml")
}

type credentialsFile struct {
	Remotes map[string]struct {
		Token string `toml:"token"`
	} `toml:"remotes"`
}

func fileToken(host string) string {
	path := credentialsPath()
	if path == "" {
		return ""
	}
	var c credentialsFile
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return ""
	}
	return c.Remotes[host].Token
}

func writeFileToken(host, token string) error {
	path := credentialsPath()
	if path == "" {
		return fmt.Errorf("cannot determine home directory")
	}
	var c credentialsFile
	toml.DecodeFile(path, &c) // best effort: keep other remotes
	if c.Remotes == nil {
		c.Remotes = map[string]struct {
			Token string `toml:"token"`
		}{}
	}
	entry := c.Remotes[host]
	entry.Token = token
	c.Remotes[host] = entry

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		os.Remove(path + ".tmp")
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(path + ".tmp")
		return err
	}
	return os.Rename(path+".tmp", path)
}

package cloud

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/elk-work/ark/internal/records"
)

// Token resolution order (docs/v1-spec.md §20): the ARK_TOKEN environment
// variable, the OS keyring, then a restricted credentials file. Tokens never
// live in .ark/config.toml, never reach another process's argv, and a keyring
// that fails is never quiet about it.

// TokenSource names the store a token came out of. `ark status` prints it,
// because "sync is authenticated" and "sync is authenticated out of a
// plaintext file" are different states and only one of them is fine.
type TokenSource string

const (
	SourceNone    TokenSource = "none"
	SourceEnv     TokenSource = "env"
	SourceKeyring TokenSource = "keyring"
	SourceFile    TokenSource = "file"
)

// Description renders a source as the place a human would go to find it.
func (s TokenSource) Description() string {
	switch s {
	case SourceEnv:
		return "ARK_TOKEN"
	case SourceKeyring:
		return keyringName()
	case SourceFile:
		return credentialsPath()
	default:
		return "nowhere"
	}
}

// Credential is a resolved token and the store that answered for it.
type Credential struct {
	Token  string
	Source TokenSource
}

// RemoteHost normalizes a remote URL to the host a credential is filed under.
// Exported because the scope of a credential is user-facing: `ark login` tells
// you which host it covers, which is how you know one login was enough.
func RemoteHost(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		return remote
	}
	return u.Host
}

// ResolveToken finds the API token for a remote.
func ResolveToken(remote string) (string, error) {
	c, err := ResolveCredential(remote)
	return c.Token, err
}

// ResolveCredential finds the API token for a remote and reports which store
// answered, in the fixed order of spec §20.
func ResolveCredential(remote string) (Credential, error) {
	if tok := os.Getenv("ARK_TOKEN"); tok != "" {
		return Credential{Token: tok, Source: SourceEnv}, nil
	}
	host := RemoteHost(remote)
	if !keyringDisabled() {
		tok, err := credentialKeyring.Get(keychainService, host)
		switch {
		case err == nil && tok != "":
			return Credential{Token: tok, Source: SourceKeyring}, nil
		case err != nil && !errors.Is(err, errNoKeyringEntry):
			// Not a miss: the keyring is locked, denied, or absent. Say so
			// before quietly reading a plaintext file instead.
			warnKeyringRead(err)
		}
	}
	if tok := fileToken(host); tok != "" {
		return Credential{Token: tok, Source: SourceFile}, nil
	}
	return Credential{Source: SourceNone}, &records.Error{Kind: records.KindPermission,
		Message: fmt.Sprintf("no credentials for %s (run `ark login`, or set ARK_TOKEN)", host)}
}

// StoreToken saves the token in the OS keyring, or — having said on stderr
// that it could not — in the fallback credentials file. Returns the store it
// landed in; call Description for the human name of it.
func StoreToken(remote, token string) (TokenSource, error) {
	host := RemoteHost(remote)
	if !keyringDisabled() {
		err := credentialKeyring.Set(keychainService, host, token)
		if err == nil {
			// The keyring holds it now, and the keyring is consulted first, so
			// a token still sitting in the fallback file is a plaintext copy
			// nothing will ever read again. That is the whole upgrade path for
			// anyone who logged in before the keyring worked on their platform.
			if err := clearFileToken(host); err != nil {
				fmt.Fprintf(warnTo, "ark: stored the token in %s, but could not remove the older copy in %s: %v\n",
					keyringName(), credentialsPath(), err)
			}
			return SourceKeyring, nil
		}
		warnKeyringWrite(err)
	}
	if err := writeFileToken(host, token); err != nil {
		return SourceNone, err
	}
	return SourceFile, nil
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
	return writeCredentials(path, c)
}

// clearFileToken drops one host's entry from the fallback file, and the file
// itself once nothing is left in it. Best effort by design: an unreadable or
// absent file means there is no plaintext copy to worry about.
func clearFileToken(host string) error {
	path := credentialsPath()
	if path == "" {
		return nil
	}
	var c credentialsFile
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil
	}
	if _, ok := c.Remotes[host]; !ok {
		return nil
	}
	delete(c.Remotes, host)
	if len(c.Remotes) == 0 {
		return os.Remove(path)
	}
	return writeCredentials(path, c)
}

// writeCredentials replaces the credentials file with c, restricted to this
// user before any token reaches the disk.
func writeCredentials(path string, c credentialsFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// Restrict the temp file BEFORE the token reaches the disk. OpenFile's
	// mode argument does not configure a Windows ACL at all, and on Unix it is
	// ignored when the file already exists — so a temp file left behind by an
	// interrupted login would carry its old, wider permissions through the
	// rename and onto the real credential file.
	if err := restrictCredentialFile(path + ".tmp"); err != nil {
		f.Close()
		os.Remove(path + ".tmp")
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

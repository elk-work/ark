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
			if _, err := clearFileToken(host); err != nil {
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

// Removal is what `ark logout` took out of this machine, and what it could
// not. It is a record rather than a bare error because the interesting cases
// are all successes: nothing was stored, or something was — and the caller has
// to be able to say which store, since "signed out of the keychain" and
// "signed out of a plaintext file" are the two states §20 keeps apart
// everywhere else.
type Removal struct {
	Host string
	// From lists the stores that actually held a token, in resolution order.
	// Empty means there was nothing to remove.
	From []TokenSource
	// KeyringSkipped means ARK_NO_KEYRING kept the keyring out of this, so an
	// empty From is a statement about the file alone. Storage honours the
	// opt-out silently because the operator asked for it; removal cannot, or
	// "nothing to remove" would be a claim about a store nobody looked in.
	KeyringSkipped bool
	// EnvToken means ARK_TOKEN is set, so a token still resolves — for every
	// remote, not just this one — no matter what came out of the stores. No
	// process can unset it in the shell that started it.
	EnvToken bool
}

// RemoveToken deletes a host's credential from both stores: the OS keyring and
// the fallback file. It is host-scoped for the same reason `ark login` is —
// one credential covers every repository pointing at one service — and it is
// idempotent: a host with nothing stored is a Removal with an empty From and
// no error, because the postcondition a caller wants ("this machine holds no
// credential for that host") already held.
func RemoveToken(remote string) (Removal, error) {
	host := RemoteHost(remote)
	rem := Removal{Host: host, EnvToken: os.Getenv("ARK_TOKEN") != ""}

	// Clear both stores before reporting either failure, and clear the file
	// even when the keyring refuses. A logout that gives up at the first error
	// leaves whichever copy it had not reached yet — and the one it reaches
	// second is the plaintext one, which is the copy the user cannot see and
	// often does not know is there.
	var keyringErr error
	if keyringDisabled() {
		rem.KeyringSkipped = true
	} else {
		switch err := credentialKeyring.Delete(keychainService, host); {
		case err == nil:
			rem.From = append(rem.From, SourceKeyring)
		case errors.Is(err, errNoKeyringEntry):
			// A miss, exactly as on the read path: the keyring answered and
			// holds nothing for this host. Ordinary, and not worth a word.
		default:
			keyringErr = err
		}
	}

	removedFile, fileErr := clearFileToken(host)
	if removedFile {
		rem.From = append(rem.From, SourceFile)
	}

	// Permission (exit 5) for a store that would not give the credential up.
	// The precise cause varies — a locked keychain, a denied prompt, no Secret
	// Service on the bus — and telling them apart means matching on error
	// strings from three platforms, which would be wrong more often than it
	// was right. What a caller can act on is the same either way: the OS still
	// holds the token and Ark could not make it stop. The message names the
	// service and account so a person can finish the job by hand, which is the
	// state this command exists to spare them.
	if keyringErr != nil {
		msg := fmt.Sprintf("could not remove the token for %s from %s: it is still stored there, as service %q account %q",
			host, keyringName(), keychainService, host)
		if fileErr != nil {
			msg += fmt.Sprintf("; %s would not give it up either: %v", credentialsPath(), fileErr)
		}
		return rem, &records.Error{Kind: records.KindPermission, Message: msg, Err: keyringErr}
	}
	if fileErr != nil {
		return rem, &records.Error{Kind: records.KindPermission,
			Message: fmt.Sprintf("the token for %s is still in %s", host, credentialsPath()),
			Err:     fileErr}
	}
	return rem, nil
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
// itself once nothing is left in it. It reports whether an entry was there to
// drop, because `ark logout` names the stores it emptied and "removed" is a
// different sentence from "there was nothing stored".
//
// No file is the ordinary case and not an error. A file that will not parse
// is: it may hold this host's token in bytes neither this function nor
// resolution can read, so reporting it lets `ark logout` refuse to claim the
// credential is gone — and lets `ark login` say the copy it supersedes is
// still on disk.
func clearFileToken(host string) (bool, error) {
	path := credentialsPath()
	if path == "" {
		return false, nil
	}
	var c credentialsFile
	if _, err := toml.DecodeFile(path, &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if _, ok := c.Remotes[host]; !ok {
		return false, nil
	}
	delete(c.Remotes, host)
	if len(c.Remotes) == 0 {
		return true, os.Remove(path)
	}
	return true, writeCredentials(path, c)
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

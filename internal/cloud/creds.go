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

// ErrCredentialsUnreadable marks the one credential state a person has to
// repair rather than simply fill in: the fallback file is there and will not
// read, so whether it holds a token for the host is unknown. Every command on
// this path already words the two differently (§20); this makes the difference
// matchable, so a caller can act on it without reading the message. `ark
// status` is the caller that needed it — reporting the source as "none" made a
// damaged file look like a machine that had never logged in (#63).
var ErrCredentialsUnreadable = errors.New("credentials file could not be read")

// unreadableCredentials tags a resolution failure as ErrCredentialsUnreadable
// while leaving the decoder's own complaint — the part carrying the line
// number — as the message and the unwrap target. fmt.Errorf("%w: %w", …) would
// have prefixed the sentinel's words onto a sentence that already says all of
// this in the terms the user needs.
type unreadableCredentials struct{ err error }

func (u unreadableCredentials) Error() string        { return u.err.Error() }
func (u unreadableCredentials) Unwrap() error        { return u.err }
func (u unreadableCredentials) Is(target error) bool { return target == ErrCredentialsUnreadable }

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
	tok, err := fileToken(host)
	if err != nil {
		// A file that will not read is not the same state as a file with no
		// entry for this host, and reporting the second is a claim about a
		// store nobody managed to look inside. It also used to be an actively
		// dangerous claim: "no credentials, run `ark login`" pointed the user
		// at the command that then overwrote the file (#62). Naming the file
		// makes the state repairable rather than merely absent.
		return Credential{Source: SourceNone}, &records.Error{Kind: records.KindPermission,
			Message: fmt.Sprintf("no credentials for %s: %s could not be read, so whether it holds a token for that host is unknown — repair that file, or move it aside and run `ark login` again",
				host, credentialsPath()),
			Err: unreadableCredentials{err}}
	}
	if tok != "" {
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

// readCredentialsFile loads the fallback file, keeping apart the two ways it
// can yield no entries. They look alike at the call site and are opposites: a
// file that is not there is the ordinary state of a machine that has never
// stored a token outside the keyring, and starting from empty is correct. A
// file that is there and will not read is unknown territory — its bytes may be
// one host's token or every host's — and treating that as empty is the whole
// of #62, where a login rewrote the file with one entry and reported success.
//
// The two are distinguishable only because toml.DecodeFile opens the file
// itself, so an absent one surfaces as *fs.PathError around ENOENT rather than
// as a parse failure. Anything else — a syntax error, a mode that denies the
// read — is the unknown case, and unknown is never treated as empty here.
func readCredentialsFile(path string) (credentialsFile, error) {
	var c credentialsFile
	if _, err := toml.DecodeFile(path, &c); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return credentialsFile{}, nil
		}
		return credentialsFile{}, err
	}
	return c, nil
}

func fileToken(host string) (string, error) {
	path := credentialsPath()
	if path == "" {
		return "", nil
	}
	c, err := readCredentialsFile(path)
	if err != nil {
		return "", err
	}
	return c.Remotes[host].Token, nil
}

// writeFileToken stores one host's token in the fallback file and preserves
// every other host's, and refuses to write anything when it cannot read what
// is already there.
//
// The refusal is the point. The decode used to be best effort, so the single
// case where the read failed was the single case where the write replaced the
// file with one entry and destroyed every token it existed to keep — while
// `ark login` printed success (#62). A failed login costs a retry; an
// overwritten credentials file costs tokens that were only ever on this disk.
func writeFileToken(host, token string) error {
	path := credentialsPath()
	if path == "" {
		return fmt.Errorf("cannot determine home directory")
	}
	c, err := readCredentialsFile(path)
	if err != nil {
		// Permission (exit 5) is the code §20 already gives a store that will
		// not give a credential up, used here for a store that will not take
		// one. Every alternative says something untrue: 2 is for a command
		// line that is wrong and this one is not, 3 is "not found" for a file
		// that is emphatically present, and 7 would claim the command did what
		// it was asked. And `ark logout` already returns 5 on this identical
		// file, so a script wrapping both does not have to know which of them
		// it ran to know what 5 meant — the machine's credential store cannot
		// be used and a person has to open it.
		//
		// The message names the file and ends in the decoder's own complaint,
		// which carries the line number, because the repair is by hand: fix
		// the syntax, or move the file somewhere the login will not touch. The
		// tokens in it are plain text either way, so moving it aside loses
		// nothing, which is why that is the advice rather than "delete it".
		return &records.Error{Kind: records.KindPermission,
			Message: fmt.Sprintf("refusing to store the token for %s: %s exists and could not be read, and rewriting it would destroy the tokens it holds for every other host — repair that file, or move it aside and run `ark login` again",
				host, path),
			Err: err}
	}
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
// still on disk. That distinction is readCredentialsFile's now, shared with
// the read and write paths so the three cannot drift apart again.
func clearFileToken(host string) (bool, error) {
	path := credentialsPath()
	if path == "" {
		return false, nil
	}
	c, err := readCredentialsFile(path)
	if err != nil {
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

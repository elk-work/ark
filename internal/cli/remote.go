package cli

import (
	"bufio"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/config"
	"github.com/elk-work/ark/internal/records"
)

func newRemoteCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Configure the Ark sync service for this repository",
	}
	cmd.AddCommand(
		&cobra.Command{
			Use:   "set <url>",
			Short: "Set the sync service URL",
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := g.open(cmd)
				if err != nil {
					return err
				}
				defer a.Close()
				url := strings.TrimRight(args[0], "/")
				if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
					return records.Validationf("remote must be an http(s) URL")
				}
				a.Config.Remote = url
				if err := config.Save(a.ArkDir, a.Config); err != nil {
					return err
				}
				p := g.printer(cmd)
				return p.Result(map[string]string{"remote": url}, func() {
					p.Line("Remote set to %s", url)
					p.Line("Store a token with `ark login` if you have not already.")
				})
			},
		},
		&cobra.Command{
			Use:   "show",
			Short: "Show the configured remote",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, args []string) error {
				a, err := g.open(cmd)
				if err != nil {
					return err
				}
				defer a.Close()
				p := g.printer(cmd)
				return p.Result(map[string]string{"remote": a.Config.Remote}, func() {
					if a.Config.Remote == "" {
						p.Line("no remote configured")
					} else {
						p.Line("%s", a.Config.Remote)
					}
				})
			},
		},
	)
	return cmd
}

func newLoginCmd(g *globals) *cobra.Command {
	var remoteFlag string
	var noVerify bool

	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store the API token for a sync service",
		Long: `Store the API token for an Ark sync service.

The credential is per SERVICE, not per repository: one login covers every
repository pointing at the same remote, on this machine. Rotating the token
means one ` + "`ark login`" + `, not one per repository.

Run it anywhere with --remote, or inside a repository to use that
repository's configured remote.

The token goes to this machine's OS keyring: the macOS keychain, Windows
Credential Manager, or the Secret Service. If the keyring is unavailable Ark
says so on stderr and falls back to a per-user credentials file —
~/.ark/credentials.toml (mode 0600), or %USERPROFILE%\.ark\credentials.toml
with a current-user-only ACL on Windows. Set ARK_NO_KEYRING=1 to choose that
file deliberately and skip the keyring.

Tokens are never written inside the repository, and never passed to another
process as a command-line argument. Pass --token, or pipe the token on stdin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			remote, err := credentialRemote(g, cmd, remoteFlag)
			if err != nil {
				return err
			}

			token, _ := cmd.Flags().GetString("token")
			if token == "" {
				// Read one line from stdin (piped or typed).
				scanner := bufio.NewScanner(os.Stdin)
				if scanner.Scan() {
					token = strings.TrimSpace(scanner.Text())
				}
			}
			if token == "" {
				return records.Validationf("no token provided (use --token or pipe it on stdin)")
			}

			// Check before storing. Storing an unverified token trades an error
			// now for a confusing one later, at the next sync, in a different
			// repository, probably to a different person.
			if !noVerify {
				if err := cloud.VerifyToken(cmd.Context(), remote, token); err != nil {
					return err
				}
			}

			src, err := cloud.StoreToken(remote, token)
			if err != nil {
				return err
			}
			host := cloud.RemoteHost(remote)
			where := src.Description()
			p := g.printer(cmd)
			// storage is the machine-readable half of stored_in: agents branch
			// on "keyring" vs "file", humans read the keychain's name or a path.
			return p.Result(map[string]string{
				"stored_in": where,
				"storage":   string(src),
				"remote":    remote,
				"host":      host,
			}, func() {
				p.Line("Token stored in %s for %s", where, host)
				p.Line("Covers every repository on this machine whose remote is %s.", host)
			})
		},
	}
	cmd.Flags().String("token", "", "API token (prefer stdin to keep it out of shell history)")
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "sync service URL (lets you log in outside a repository)")
	cmd.Flags().BoolVar(&noVerify, "no-verify", false, "store without checking the token against the server")
	return cmd
}

// credentialRemote resolves the sync service a credential command acts on: the
// --remote flag, or the repository's configured remote. Shared by `ark login`
// and `ark logout` so the two cannot drift on what a credential is scoped to —
// a logout that resolved its host differently from the login would leave the
// credential in place and say it had not.
//
// A repository is opened only when the flag did not answer: neither command
// should require standing in one, since the credential is per service.
func credentialRemote(g *globals, cmd *cobra.Command, flag string) (string, error) {
	remote := strings.TrimRight(flag, "/")
	if remote == "" {
		a, err := g.open(cmd)
		if err != nil {
			return "", records.Validationf(
				"not in an Ark repository; pass --remote <url> (the credential is per service, so any URL works from anywhere)")
		}
		defer a.Close()
		if a.Config.Remote == "" {
			return "", records.Validationf("set a remote first: ark remote set <url>, or pass --remote <url>")
		}
		remote = a.Config.Remote
	}
	if !strings.HasPrefix(remote, "http://") && !strings.HasPrefix(remote, "https://") {
		return "", records.Validationf("remote must be an http(s) URL")
	}
	return remote, nil
}

// logoutReport is the --json shape. `removed` carries the same vocabulary
// `ark login` prints as `storage` — "keyring", "file" — so an agent branches on
// one set of names across the credential lifecycle, and `removed_from` is the
// parallel list of places a human would go and look.
type logoutReport struct {
	Host   string `json:"host"`
	Remote string `json:"remote"`
	// Never null: an empty logout is the ordinary idempotent case, and a
	// caller iterating this should not have to special-case the shape of
	// "nothing was stored".
	Removed        []string `json:"removed"`
	RemovedFrom    []string `json:"removed_from"`
	KeyringSkipped bool     `json:"keyring_skipped"`
	EnvToken       bool     `json:"env_token"`
}

func newLogoutCmd(g *globals) *cobra.Command {
	var remoteFlag string

	cmd := &cobra.Command{
		Use:   "logout",
		Short: "Remove this machine's stored token for a sync service",
		Long: `Remove this machine's stored API token for an Ark sync service.

Host-scoped, exactly as ` + "`ark login`" + ` is: one login covers every repository
pointing at the same remote, so one logout signs all of them out. Run it
anywhere with --remote, or inside a repository to use that repository's
configured remote.

Both stores are cleared — the OS keyring entry and any entry in the fallback
credentials file. The file copy is the one worth insisting on: the keyring
outranks it, so it can sit there unread and unnoticed long after you believe
you have logged out.

Logging out of a host that has nothing stored succeeds and says so. There is
nothing to repair, and a teardown script should not have to tell the two
cases apart.

ARK_TOKEN is not affected — no process can unset a variable in the shell that
started it. If it is set, this command says so and exits 7, because a token
still resolves for every remote and reporting a clean logout would be a lie
a script could not see through.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			remote, err := credentialRemote(g, cmd, remoteFlag)
			if err != nil {
				return err
			}

			rem, err := cloud.RemoveToken(remote)
			if err != nil {
				return err
			}

			report := logoutReport{
				Host:           rem.Host,
				Remote:         remote,
				Removed:        []string{},
				RemovedFrom:    []string{},
				KeyringSkipped: rem.KeyringSkipped,
				EnvToken:       rem.EnvToken,
			}
			for _, src := range rem.From {
				report.Removed = append(report.Removed, string(src))
				report.RemovedFrom = append(report.RemovedFrom, src.Description())
			}

			p := g.printer(cmd)
			if err := p.Result(report, func() {
				if len(report.RemovedFrom) == 0 {
					p.Line("No stored token for %s — nothing to remove.", rem.Host)
				} else {
					p.Line("Removed the token for %s from %s", rem.Host, strings.Join(report.RemovedFrom, " and "))
					p.Line("No repository on this machine can sync with %s until you `ark login` again.", rem.Host)
				}
				if rem.KeyringSkipped {
					p.Line("ARK_NO_KEYRING is set, so the %s was not consulted; unset it and run `ark logout` again to clear a token stored there.",
						keyringDescription())
				}
			}); err != nil {
				return err
			}

			// Reported after the result, and as an error rather than a warning:
			// the stores are empty and the machine still resolves a token, which
			// is precisely the divergence a caller cannot see in an exit code of
			// 0 (#46, #58). Spec §22 has a code for exactly this — 7, the command
			// did what it was asked and left something needing repair — and the
			// repair is one line in the shell Ark cannot reach.
			if rem.EnvToken {
				return records.Partialf(
					"ARK_TOKEN is set, so a token still resolves for every remote — run `unset ARK_TOKEN`, and remove it from your shell profile, to finish logging out")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "sync service URL (lets you log out outside a repository)")
	return cmd
}

// keyringDescription names this platform's keyring for a message. The keyring's
// own name is internal/cloud's to know; SourceKeyring.Description is the way
// out of the package, and going through it keeps one spelling of "macOS
// keychain" in the CLI rather than a second one that can drift.
func keyringDescription() string { return cloud.SourceKeyring.Description() }

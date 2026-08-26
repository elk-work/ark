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

The token goes to the macOS keychain when available, otherwise to a
per-user credentials file: ~/.ark/credentials.toml (mode 0600), or
%USERPROFILE%\.ark\credentials.toml with a current-user-only ACL on Windows.
Tokens are never written inside the repository. Pass --token, or pipe the
token on stdin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			remote := strings.TrimRight(remoteFlag, "/")

			// Only open a repository when we actually need it for the remote —
			// logging in should not require standing in one.
			if remote == "" {
				a, err := g.open(cmd)
				if err != nil {
					return records.Validationf(
						"not in an Ark repository; pass --remote <url> (the credential is per service, so any URL works from anywhere)")
				}
				defer a.Close()
				if a.Config.Remote == "" {
					return records.Validationf("set a remote first: ark remote set <url>, or pass --remote <url>")
				}
				remote = a.Config.Remote
			}
			if !strings.HasPrefix(remote, "http://") && !strings.HasPrefix(remote, "https://") {
				return records.Validationf("remote must be an http(s) URL")
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

			where, err := cloud.StoreToken(remote, token)
			if err != nil {
				return err
			}
			host := cloud.RemoteHost(remote)
			p := g.printer(cmd)
			return p.Result(map[string]string{"stored_in": where, "remote": remote, "host": host}, func() {
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

package cli

import (
	"bufio"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ijroth/ark/internal/cloud"
	"github.com/ijroth/ark/internal/config"
	"github.com/ijroth/ark/internal/records"
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
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store the API token for the configured remote",
		Long: `Store the API token for this repository's remote.

The token goes to the macOS keychain when available, otherwise to
~/.ark/credentials.toml (mode 0600). Tokens are never written inside the
repository. Pass --token, or pipe the token on stdin.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			if a.Config.Remote == "" {
				return records.Validationf("set a remote first: ark remote set <url>")
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
			where, err := cloud.StoreToken(a.Config.Remote, token)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(map[string]string{"stored_in": where}, func() {
				p.Line("Token stored in %s", where)
			})
		},
	}
	cmd.Flags().String("token", "", "API token (prefer stdin to keep it out of shell history)")
	return cmd
}

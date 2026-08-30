package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// `ark credential list` and `ark credential revoke`: the two halves of
// retiring a credential (docs/rfc-0003-elk-issued-credentials.md,
// Amendment 1; elk-work/ark#94).
//
// They are service-wide rather than per-repository, so they resolve a remote
// the way `ark principal create` does — this checkout's, or `--remote` — and
// never a repository id. `ark repo grant` is the per-repository neighbour and
// stays exactly where it is.
//
// Listing is here because revocation is by id, and the machine that held a
// credential is the machine that has gone missing. Without a way to recover
// the id, `revoke` would be a route nobody could address, which is the state
// elk-work/ark#94 was filed about.

// serviceClient builds a client for a service-wide command: a remote, and
// whatever credential is stored for it.
func serviceClient(g *globals, cmd *cobra.Command, remoteFlag string) (*cloud.Client, string, error) {
	remote, err := credentialRemote(g, cmd, remoteFlag)
	if err != nil {
		return nil, "", err
	}
	client, err := cloud.New(remote)
	if err != nil {
		return nil, "", err
	}
	return client, remote, nil
}

func newCredentialCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credential",
		Short: "List and revoke credentials on an Ark sync service",
		Long: `List and revoke credentials on an Ark sync service.

A credential is one ` + "`arkc_…`" + ` token. A principal may hold several — one per
machine is the ordinary shape — and retiring one leaves the others working,
which is the whole reason they are separate rows rather than one token each.

The service stores only a SHA-256 of a credential, so nothing recovers the
token itself. What ` + "`list`" + ` recovers is the **id**, which is what a revocation
names.`,
	}
	cmd.AddCommand(newCredentialListCmd(g), newCredentialRevokeCmd(g))
	return cmd
}

func newCredentialListCmd(g *globals) *cobra.Command {
	var remoteFlag string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List your own credentials on the sync service",
		Long: `List your own credentials on the sync service.

Yours and nobody else's — an operator sees everybody's with ` + "`ark principal list`" + `.

Each row is a credential you could be holding on some machine: when it was
issued, when it expires, the last day it was used, and whether it has been
revoked. ` + "`last used`" + ` is a date rather than a time, because usage is recorded
at day granularity so that observing it costs no write on every request.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, remote, err := serviceClient(g, cmd, remoteFlag)
			if err != nil {
				return err
			}
			resp, err := client.Credentials(cmd.Context())
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(resp, func() {
				if len(resp.Credentials) == 0 {
					p.Line("No credentials on %s for principal %s.", remote, resp.PrincipalID)
					return
				}
				p.Line("Credentials held by principal %s on %s:", resp.PrincipalID, remote)
				p.Line("")
				p.Table([]string{"ID", "LABEL", "ISSUED", "EXPIRES", "LAST USED", "STATE"},
					credentialRows(resp.Credentials, false))
				p.Line("")
				p.Line("Retire one with:  ark credential revoke <id> --remote %s", remote)
			})
		},
	}
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "sync service URL (lets you run this outside a repository)")
	return cmd
}

func newCredentialRevokeCmd(g *globals) *cobra.Command {
	var remoteFlag string
	cmd := &cobra.Command{
		Use:   "revoke <credential-id>",
		Short: "Retire one credential so the service stops accepting it",
		Long: `Retire one credential so the service stops accepting it.

You may always revoke your own — that is the lost-laptop case, and it should
not wait for anybody. An operator may revoke any credential, which is how a
departed colleague's access ends.

Revocation is eventually consistent, bounded at 60 seconds: the service caches
the credential store rather than reading it on every request, and that bound
is the accepted price. Revoking a credential that is already revoked is a
success — the state you asked for holds.

` + "`ark credential list`" + ` shows the ids you may pass here.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := strings.TrimSpace(args[0])
			if id == "" {
				return records.Validationf("a credential id is required: `ark credential list` shows yours")
			}
			client, remote, err := serviceClient(g, cmd, remoteFlag)
			if err != nil {
				return err
			}
			resp, err := client.RevokeCredential(cmd.Context(), id)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(resp, func() {
				if resp.AlreadyRevoked {
					p.Line("Credential %s was already revoked, on %s. Nothing changed.",
						resp.Credential.ID, resp.Credential.RevokedAt)
					return
				}
				p.Line("Credential %s is revoked on %s.", resp.Credential.ID, remote)
				// The bound is the useful half: somebody holding this token
				// may still be served for up to a minute, and saying so is
				// what keeps that from looking like the revocation failing.
				p.Line("Any request still carrying it is refused within 60 seconds.")
			})
		},
	}
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "sync service URL (lets you run this outside a repository)")
	return cmd
}

// credentialRows renders credentials for a table. `withPrincipal` adds the
// owning principal, which the operator roster needs and a self-list does not.
func credentialRows(creds []api.Credential, withPrincipal bool) [][]string {
	rows := make([][]string, 0, len(creds))
	for _, c := range creds {
		state := "active"
		if c.RevokedAt != "" {
			state = "revoked " + day(c.RevokedAt)
		}
		row := []string{c.ID, orDash(c.Label), day(c.CreatedAt), day(c.ExpiresAt),
			orDash(c.LastUsedOn), state}
		if withPrincipal {
			row = append([]string{c.PrincipalID}, row...)
		}
		rows = append(rows, row)
	}
	return rows
}

// day trims an RFC-3339 timestamp to its date. A credential's life is
// measured in months, so the clock time is noise in a table somebody is
// scanning for which machine a token is on.
func day(ts string) string {
	if ts == "" {
		return "—"
	}
	if i := strings.IndexByte(ts, 'T'); i > 0 {
		return ts[:i]
	}
	return ts
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

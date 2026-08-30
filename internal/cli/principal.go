package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

func newPrincipalCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "principal",
		Short: "Manage principals on an Ark sync service",
		Long: `Manage principals on an Ark sync service.

A principal is a person or an agent the service knows by name, holding
credentials it can revoke one at a time. It is what replaces the single shared
service token — see docs/rfc-0003-elk-issued-credentials.md.

An **operator** is a principal entitled to the two acts that are about the
service rather than about a repository: listing principals, and revoking any
credential. The first one is minted with the service's ARK_BOOTSTRAP_TOKEN;
every operator after that is added by an operator.`,
	}
	cmd.AddCommand(newPrincipalCreateCmd(g), newPrincipalListCmd(g))
	return cmd
}

func newPrincipalListCmd(g *globals) *cobra.Command {
	var remoteFlag string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every principal on the sync service and the credentials each holds",
		Long: `List every principal on the sync service and the credentials each holds.

This is an operator act, for the reason ` + "`ark repo grants`" + ` needs ` + "`--admin`" + `: the
list is a roster of who exists and of what each of them could present, and the
people who may read one are the people who may change it.

It is also what makes revocation usable. A credential is retired by id, and an
id printed once at issue is an id nobody has when it matters — so this is
where you find the one to pass to ` + "`ark credential revoke`" + `.

Your own credentials, without being an operator, are ` + "`ark credential list`" + `.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			client, remote, err := serviceClient(g, cmd, remoteFlag)
			if err != nil {
				return err
			}
			resp, err := client.Principals(cmd.Context())
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(resp, func() {
				if len(resp.Principals) == 0 {
					p.Line("No principals on %s.", remote)
					return
				}
				for i, rec := range resp.Principals {
					if i > 0 {
						p.Line("")
					}
					p.Line("%s", principalHeading(rec.Principal))
					if len(rec.Credentials) == 0 {
						p.Line("  no credentials")
						continue
					}
					p.Table([]string{"  ID", "LABEL", "ISSUED", "EXPIRES", "LAST USED", "STATE"},
						indentRows(credentialRows(rec.Credentials, false)))
				}
			})
		},
	}
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "sync service URL (lets you run this outside a repository)")
	return cmd
}

// principalHeading is one principal's line above its credentials. Operator
// and disabled are called out because they are the two facts that change what
// the rows below mean.
func principalHeading(p api.Principal) string {
	label := p.Email
	if label == "" {
		label = p.ID
	}
	line := label + "  (" + p.Kind + ", " + p.ID + ")"
	if p.Operator() {
		line += "  operator since " + day(p.OperatorSince)
	}
	if p.DisabledAt != "" {
		line += "  DISABLED " + day(p.DisabledAt)
	}
	return line
}

// indentRows shifts a credential table under its principal's heading.
func indentRows(rows [][]string) [][]string {
	for _, row := range rows {
		row[0] = "  " + row[0]
	}
	return rows
}

func newPrincipalCreateCmd(g *globals) *cobra.Command {
	var (
		remoteFlag  string
		email       string
		displayName string
		kind        string
		bootstrap   string
		operator    bool
	)

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Mint a principal and its first credential",
		Long: `Mint a principal on a sync service and print its credential once.

This is the bootstrap path, and it needs no identity provider: the service
operator sets ARK_BOOTSTRAP_TOKEN to a random string — exactly as they set
ARK_API_TOKEN — and this command exchanges it for a per-person credential.
From then on that person has a credential of their own, revocable on its own,
rather than a copy of the one string everybody shares.

The credential is shown once. The service stores only its SHA-256, so a lost
credential is reissued, never recovered. Store it with ` + "`ark login`" + `.

The bootstrap token is read from --bootstrap, else ARK_BOOTSTRAP_TOKEN, else
one line on stdin when stdin is not a terminal. Prefer the last two: an
argument lands in the process table and in shell history (spec §20).

With none of those, the command authenticates as **you** — the credential
already stored for this remote — which is how an operator mints a principal
for somebody else, and the only way to mint another operator.

` + "`--operator`" + ` makes the new principal one: entitled to list principals and
revoke any credential. Only an operator may ask for it. The bootstrap token
cannot, because the first principal on a service that has no operator becomes
one automatically — so a shared secret hands that authority over exactly once,
before anyone holds it, and never again.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			remote, err := credentialRemote(g, cmd, remoteFlag)
			if err != nil {
				return err
			}
			if strings.TrimSpace(email) == "" {
				return records.Validationf("--email is required: the email is the principal's identity")
			}
			if bootstrap == "" {
				bootstrap = os.Getenv("ARK_BOOTSTRAP_TOKEN")
			}
			if bootstrap == "" {
				// Not an unconditional read: on a terminal it would be
				// indistinguishable from a hang, and there is now a second
				// thing this command can do without a bootstrap token. Piping
				// and redirecting are unaffected, which is every scripted use.
				bootstrap = stdinToken(cmd)
			}
			// Falling back to the stored credential is what makes `--operator`
			// reachable at all: an operator authenticates as themselves, and
			// the service refuses anyone who is not one.
			as := bootstrap
			if as == "" {
				if as, err = cloud.ResolveToken(remote); err != nil || as == "" {
					return records.Validationf(
						"nothing to authenticate with: pass --bootstrap, set ARK_BOOTSTRAP_TOKEN, pipe it on "+
							"stdin, or `ark login --remote %s` as an operator", remote)
				}
			}

			resp, err := createPrincipal(cmd.Context(), remote, as, api.CreatePrincipalRequest{
				Email:       strings.TrimSpace(email),
				DisplayName: displayName,
				Kind:        kind,
				Operator:    operator,
			})
			if err != nil {
				return err
			}

			p := g.printer(cmd)
			return p.Result(resp, func() {
				verb := "Created principal"
				if !resp.Created {
					verb = "Reissued a credential for principal"
				}
				p.Line("%s %s (%s) on %s", verb, resp.Principal.ID, resp.Principal.Email, remote)
				if resp.Principal.Operator() {
					// Worth its own line: this is the only thing the command
					// can do that changes what somebody may do to the whole
					// service, and on a fresh deployment it happens without
					// anybody asking for it.
					p.Line("They are an operator, since %s: they may list principals and revoke any credential.",
						resp.Principal.OperatorSince)
				}
				p.Line("")
				p.Line("Credential — shown once, and the service keeps only its SHA-256:")
				p.Line("")
				p.Line("  %s", resp.Token)
				p.Line("")
				p.Line("Store it:  ark login --remote %s   (paste it, or pipe it in)", remote)
				p.Line("It expires on %s. Recovery from expiry is another `ark login`.", resp.ExpiresAt)
				p.Line("Retire it early:  ark credential revoke %s --remote %s", resp.CredentialID, remote)
			})
		},
	}
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "sync service URL (lets you run this outside a repository)")
	cmd.Flags().StringVar(&email, "email", "", "the principal's email — its identity on the service")
	cmd.Flags().StringVar(&displayName, "display-name", "", "name shown beside the principal")
	cmd.Flags().StringVar(&kind, "kind", "human", `"human" or "agent"`)
	cmd.Flags().StringVar(&bootstrap, "bootstrap", "",
		"the service's bootstrap token (prefer ARK_BOOTSTRAP_TOKEN or stdin to keep it out of the process table)")
	cmd.Flags().BoolVar(&operator, "operator", false,
		"make them an operator: may list principals and revoke any credential (only an operator may ask)")
	return cmd
}

// createPrincipal calls POST /v1/principals with whatever bearer the caller
// resolved — the service's bootstrap token, or an operator's own credential.
//
// It does not go through internal/cloud's Client, deliberately. That client
// resolves a stored credential and maps every 401 to "the stored credential
// was not accepted; it may have been rotated. Run `ark login`" — advice that is
// exactly backwards here, because this is the command a person runs when they
// have nothing to log in with yet. The mapping below is the same error
// contract (spec §22) told from that starting point.
func createPrincipal(ctx context.Context, remote, bootstrap string, req api.CreatePrincipalRequest) (*api.CreatePrincipalResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	url := strings.TrimSuffix(remote, "/") + "/v1/principals"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+bootstrap)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, records.Offlinef("cannot reach %s: %v", remote, err)
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var apiErr api.Error
		if json.Unmarshal(data, &apiErr) != nil || apiErr.Message == "" {
			apiErr.Message = strings.TrimSpace(string(data))
		}
		switch resp.StatusCode {
		case http.StatusForbidden:
			// The service authenticated the bearer and declined the act —
			// which since elk-work/ark#94 means "you are not an operator", and
			// the server's own message says which act and what to do. Adding
			// the bootstrap-token hint on top of it would be advice about a
			// different failure.
			return nil, records.Permissionf("%s", apiErr.Message)
		case http.StatusUnauthorized:
			return nil, records.Permissionf(
				"%s — this route takes the server's ARK_BOOTSTRAP_TOKEN, or an operator's own credential; "+
					"not the shared API token", apiErr.Message)
		case http.StatusNotFound:
			// Not "no such principal": the route itself is missing, which
			// means the service predates per-principal credentials.
			return nil, records.NotFoundf(
				"%s does not serve POST /v1/principals — it is older than per-principal credentials", remote)
		case http.StatusConflict:
			return nil, records.Conflictf("%s", apiErr.Message)
		case http.StatusBadRequest:
			return nil, records.Validationf("%s", apiErr.Message)
		default:
			return nil, records.Offlinef("server error: %s", apiErr.Message)
		}
	}
	var out api.CreatePrincipalResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, records.Offlinef("unreadable response from %s: %v", remote, err)
	}
	return &out, nil
}

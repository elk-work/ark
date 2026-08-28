package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

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
service token — see docs/rfc-0003-elk-issued-credentials.md.`,
	}
	cmd.AddCommand(newPrincipalCreateCmd(g))
	return cmd
}

func newPrincipalCreateCmd(g *globals) *cobra.Command {
	var (
		remoteFlag  string
		email       string
		displayName string
		kind        string
		bootstrap   string
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
one line on stdin. Prefer the last two: an argument lands in the process table
and in shell history (spec §20).`,
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
				scanner := bufio.NewScanner(os.Stdin)
				if scanner.Scan() {
					bootstrap = strings.TrimSpace(scanner.Text())
				}
			}
			if bootstrap == "" {
				return records.Validationf(
					"no bootstrap token provided (use --bootstrap, set ARK_BOOTSTRAP_TOKEN, or pipe it on stdin)")
			}

			resp, err := createPrincipal(cmd.Context(), remote, bootstrap, api.CreatePrincipalRequest{
				Email:       strings.TrimSpace(email),
				DisplayName: displayName,
				Kind:        kind,
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
				p.Line("")
				p.Line("Credential — shown once, and the service keeps only its SHA-256:")
				p.Line("")
				p.Line("  %s", resp.Token)
				p.Line("")
				p.Line("Store it:  ark login --remote %s   (paste it, or pipe it in)", remote)
				p.Line("It expires on %s. Recovery from expiry is another `ark login`.", resp.ExpiresAt)
			})
		},
	}
	cmd.Flags().StringVar(&remoteFlag, "remote", "", "sync service URL (lets you run this outside a repository)")
	cmd.Flags().StringVar(&email, "email", "", "the principal's email — its identity on the service")
	cmd.Flags().StringVar(&displayName, "display-name", "", "name shown beside the principal")
	cmd.Flags().StringVar(&kind, "kind", "human", `"human" or "agent"`)
	cmd.Flags().StringVar(&bootstrap, "bootstrap", "",
		"the service's bootstrap token (prefer ARK_BOOTSTRAP_TOKEN or stdin to keep it out of the process table)")
	return cmd
}

// createPrincipal calls POST /v1/principals.
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
		case http.StatusUnauthorized, http.StatusForbidden:
			return nil, records.Permissionf(
				"%s — the bootstrap token is the server's ARK_BOOTSTRAP_TOKEN, not your API token", apiErr.Message)
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

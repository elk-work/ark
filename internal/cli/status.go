package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/records"
)

type statusReport struct {
	RepositoryID     string `json:"repository_id"`
	Root             string `json:"root"`
	Branch           string `json:"branch,omitempty"`
	Head             string `json:"head,omitempty"`
	Actor            string `json:"actor"`
	ActorType        string `json:"actor_type"`
	OpenTasks        int64  `json:"open_tasks"`
	OpenPRs          int64  `json:"open_pull_requests"`
	PendingMutations int64  `json:"pending_mutations"`
	// RejectedMutations counts changes the server refused whose effect it
	// still does not hold — the repository's divergence from the service.
	//
	// pending_mutations cannot answer that on its own, and the way it fails
	// is the worst available: a rejection *removes* the mutation from the
	// queue, so the number goes to zero at the exact moment the two copies
	// stop agreeing. A client reported `0 pending mutations` about a server
	// that had just refused three writes, and nothing in this command said
	// otherwise (elk-work/ark#46). Whatever else status reports, it must not
	// be able to describe a diverged repository as a clean one.
	RejectedMutations int64  `json:"rejected_mutations"`
	Conflicts         int64  `json:"unresolved_conflicts"`
	Remote            string `json:"remote,omitempty"`
	// TokenSource is which store answered for the sync token — env, keyring,
	// file, or none — and is set only when a remote is configured. `ark login`
	// says where it wrote the token; resolution deserves to be as legible,
	// because reading one out of a plaintext file is a different state from
	// reading one out of the keychain. Never the token itself.
	TokenSource string `json:"token_source,omitempty"`
}

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show repository, actor, and sync state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()

			rep := statusReport{
				RepositoryID: a.Config.RepositoryID,
				Root:         a.Root,
				Actor:        a.Store.Actor.Name,
				ActorType:    string(a.Store.Actor.Type),
				Remote:       a.Config.Remote,
			}
			rep.Branch, _ = a.Git.CurrentBranch(ctx)
			rep.Head, _ = a.Git.Head(ctx)

			// Resolving here can warn on stderr that the keyring is locked or
			// absent, which is exactly where someone would want to hear it. A
			// missing credential is not a status failure — it is a state to
			// report — so the error is deliberately dropped for the source.
			tokenAt := cloud.SourceNone
			if rep.Remote != "" {
				if cred, err := cloud.ResolveCredential(rep.Remote); err == nil {
					tokenAt = cred.Source
				}
				rep.TokenSource = string(tokenAt)
			}

			count := func(query string, dest *int64, args ...any) error {
				if err := a.DB.QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
					return records.DBErr("count records", err)
				}
				return nil
			}
			if err := count(`SELECT COUNT(*) FROM tasks WHERE repository_id = ? AND status NOT IN ('done','closed') AND deleted_at IS NULL`, &rep.OpenTasks, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM pull_requests WHERE repository_id = ? AND status = 'open' AND deleted_at IS NULL`, &rep.OpenPRs, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM mutations WHERE repository_id = ? AND status = 'pending'`, &rep.PendingMutations, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM mutations WHERE repository_id = ? AND status = 'rejected' AND resolved_at IS NULL`, &rep.RejectedMutations, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM conflicts WHERE status = 'unresolved'`, &rep.Conflicts); err != nil {
				return err
			}

			p := g.printer(cmd)
			return p.Result(rep, func() {
				p.Line("repository  %s", rep.Root)
				p.Line("id          %s", rep.RepositoryID)
				if rep.Branch != "" {
					p.Line("branch      %s (%s)", rep.Branch, short(rep.Head))
				}
				p.Line("actor       %s (%s)", rep.Actor, rep.ActorType)
				p.Line("open        %d tasks, %d pull requests", rep.OpenTasks, rep.OpenPRs)
				// The queue count alone is the number that lied, so it never
				// stands alone again once anything has been rejected.
				queue := fmt.Sprintf("%d pending mutations", rep.PendingMutations)
				if rep.RejectedMutations > 0 {
					queue += fmt.Sprintf(", %d rejected", rep.RejectedMutations)
				}
				if rep.Remote == "" {
					p.Line("sync        no remote configured; %s recorded", queue)
				} else {
					p.Line("sync        %s (%s)", rep.Remote, queue)
					switch tokenAt {
					case cloud.SourceNone:
						p.Line("token       none — run `ark login`")
					case cloud.SourceFile:
						p.Line("token       %s (plaintext fallback)", tokenAt.Description())
					default:
						p.Line("token       %s", tokenAt.Description())
					}
				}
				// Spelled out rather than left as a number on the sync line:
				// a rejection is terminal — the mutation will not be retried
				// — so the local record keeps a change the service has never
				// accepted, and a reader has to be told that in words.
				if rep.RejectedMutations > 0 {
					p.Line("diverged    %d rejected change(s) kept locally and not on the server (`ark sync` names each)",
						rep.RejectedMutations)
				}
				if rep.Conflicts > 0 {
					p.Line("conflicts   %d unresolved (see `ark conflict list`)", rep.Conflicts)
				}
			})
		},
	}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

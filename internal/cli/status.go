package cli

import (
	"github.com/spf13/cobra"

	"github.com/elkproject/ark/internal/records"
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
	Conflicts        int64  `json:"unresolved_conflicts"`
	Remote           string `json:"remote,omitempty"`
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
				if rep.Remote == "" {
					p.Line("sync        no remote configured; %d local mutations recorded", rep.PendingMutations)
				} else {
					p.Line("sync        %s (%d pending mutations)", rep.Remote, rep.PendingMutations)
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

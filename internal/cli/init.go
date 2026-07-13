package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/ijroth/ark/internal/app"
)

func newInitCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize Ark inside the current Git repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := g.dir
			if dir == "" {
				var err error
				if dir, err = os.Getwd(); err != nil {
					return err
				}
			}
			repoID, _ := cmd.Flags().GetString("repository")
			res, err := app.Init(cmd.Context(), dir, repoID)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(res, func() {
				p.Line("Initialized Ark in %s/.ark", res.Root)
				p.Line("  repository  %s (%s)", res.Name, res.RepositoryID)
				p.Line("  branch      %s", res.DefaultBranch)
				if res.GitRemoteURL != "" {
					p.Line("  git remote  %s", res.GitRemoteURL)
				}
				p.Line("  actor       %s (%s)", res.ActorName, res.DefaultActorID)
			})
		},
	}
	cmd.Flags().String("repository", "",
		"join an existing Ark repository by ID (second client; then set remote, login, sync)")
	return cmd
}

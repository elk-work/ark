package cli

import (
	"os"

	"github.com/spf13/cobra"

	"github.com/elkproject/ark/internal/app"
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

			// Install the agent guidance by default. The repositories Ark
			// serves are built by agents, and the thing that actually goes
			// wrong is omission — sessions ending unrecorded, repositories
			// never given a remote. Guidance only helps if it arrives in the
			// agent's context without anyone remembering to fetch it.
			skillWritten, skillStaged := false, false
			skillPath := ""
			if withSkill, _ := cmd.Flags().GetBool("skill"); withSkill {
				skillWritten, skillPath, err = installSkill(res.Root, false)
				if err != nil {
					return err
				}
				// Writing it is not the job; being tracked is. An untracked
				// skill reaches no other clone, and the repository looks
				// adopted while every other session runs without guidance.
				if skillWritten {
					skillStaged = stageSkill(cmd.Context(), res.Root, skillPath)
				}
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
				if skillWritten {
					if skillStaged {
						p.Line("  skill       %s (staged)", SkillPath)
					} else {
						p.Line("  skill       %s (NOT staged)", SkillPath)
					}
				}
				// Say this every time. A repository with no remote records to
				// one machine, with no backup and no second reader, and that
				// is invisible until the day it matters.
				p.Line("")
				p.Line("Not yet syncing. Until a remote is set this records to this machine only:")
				p.Line("  ark remote set <sync-service-url> && ark login && ark sync")
				// Same reasoning, same failure shape: uncommitted, the skill
				// exists on this machine only and no other session gets it.
				if skillWritten {
					p.Line("")
					if skillStaged {
						p.Line("The skill is staged but not committed. Commit it — until you do, no")
						p.Line("other clone or sandbox has the guidance:")
						p.Line("  git commit -m 'chore(ark): add the Ark agent skill' -- %s", SkillPath)
					} else {
						p.Line("The skill could not be staged. Track it yourself — until it is committed,")
						p.Line("no other clone or sandbox has the guidance:")
						p.Line("  git add %s", SkillPath)
					}
				}
			})
		},
	}
	cmd.Flags().String("repository", "",
		"join an existing Ark repository by ID (second client; then set remote, login, sync)")
	cmd.Flags().Bool("skill", true,
		"install agent guidance at "+SkillPath+" (--skill=false to skip)")
	return cmd
}

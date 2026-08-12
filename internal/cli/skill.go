package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/elkproject/ark/internal/git"
	"github.com/elkproject/ark/internal/records"
	"github.com/elkproject/ark/skills"
)

// SkillPath is where the guidance lands in a repository. Claude Code and
// compatible harnesses load skills from this location automatically, which is
// the point: the instructions have to arrive in the agent's context without
// anyone remembering to go and read them.
const SkillPath = ".claude/skills/ark/SKILL.md"

// installSkill writes the bundled skill into the repository. It reports
// whether it wrote anything, so `ark init` can stay quiet when the file is
// already current.
//
// Ark's observed failure mode is not defect but omission — sessions that end
// without recording anything, and repositories that never get a remote. The
// projects Ark targets are built by agents, so the durable fix is to put the
// instructions where an agent will actually read them rather than in a
// runbook a human is assumed to have read.
func installSkill(root string, force bool) (wrote bool, path string, err error) {
	want, err := skills.FS.ReadFile(skills.Ark)
	if err != nil {
		return false, "", err
	}
	path = filepath.Join(root, filepath.FromSlash(SkillPath))

	existing, readErr := os.ReadFile(path)
	switch {
	case readErr == nil && string(existing) == string(want):
		return false, path, nil
	case readErr == nil && !force:
		// Someone edited their copy. Leave it alone rather than silently
		// reverting local guidance; `ark skill install --force` is explicit.
		return false, path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, path, records.DBErr("create skill directory", err)
	}
	if err := os.WriteFile(path, want, 0o644); err != nil {
		return false, path, records.DBErr("write skill", err)
	}
	return true, path, nil
}

// stageSkill adds the skill to the index if it is not tracked yet.
//
// Writing the file is not enough, and the difference is not cosmetic. The
// skill's whole design is that it reaches agents automatically because it is a
// *tracked* file — every clone, every worktree, every cloud sandbox. Left
// untracked it exists on exactly one machine, and the repository looks adopted
// while no other session ever sees the guidance. That is not hypothetical: two
// repositories in this project ran `ark init`, never committed `.claude/`, and
// carried Ark for weeks with no guidance at all.
//
// Staging rather than committing: Ark does not create commits in someone's
// repository unasked, and the working tree may be mid-change. Staging puts the
// file in `git status` where it cannot be missed, one step from done.
// Best-effort — a repository without Git, or a locked index, must not fail
// `ark init`.
func stageSkill(ctx context.Context, root, path string) bool {
	g := &git.Repo{Dir: root}
	if g.IsTracked(ctx, path) {
		return false
	}
	return g.Add(ctx, path) == nil
}

type skillReport struct {
	Path    string `json:"path"`
	Written bool   `json:"written"`
	Staged  bool   `json:"staged"`
}

func newSkillCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Manage the agent guidance Ark installs into a repository",
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Write .claude/skills/ark/SKILL.md into this repository",
		Long: `Install the Ark skill so coding agents working in this repository
know how to record their work — and can tell when the repository is recording
to one machine only.

` + "`ark init`" + ` does this for you. Run it directly to update an existing
repository after upgrading Ark, or with --force to overwrite a local edit.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			force, _ := cmd.Flags().GetBool("force")
			wrote, path, err := installSkill(a.Root, force)
			if err != nil {
				return err
			}
			staged := false
			if wrote {
				staged = stageSkill(cmd.Context(), a.Root, path)
			}
			rel, relErr := filepath.Rel(a.Root, path)
			if relErr != nil {
				rel = path
			}
			p := g.printer(cmd)
			return p.Result(skillReport{Path: path, Written: wrote, Staged: staged}, func() {
				if wrote {
					p.Line("Wrote %s", rel)
					if staged {
						p.Line("Staged it. Commit it, or no other clone gets the guidance.")
					}
					return
				}
				if _, statErr := os.Stat(path); statErr == nil {
					p.Line("%s is already present (use --force to overwrite local edits)", rel)
					return
				}
				p.Line("%s unchanged", rel)
			})
		},
	}
	install.Flags().Bool("force", false, "overwrite a locally modified copy")

	show := &cobra.Command{
		Use:   "show",
		Short: "Print the bundled skill to stdout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := skills.FS.ReadFile(skills.Ark)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), string(b))
			return err
		},
	}

	cmd.AddCommand(install, show)
	return cmd
}

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

// commitSkill tracks the skill: stage it, then commit that one path.
//
// Writing the file is not enough, and the difference is not cosmetic. The
// skill's whole design is that it reaches agents automatically because it is a
// *tracked* file — every clone, every worktree, every cloud sandbox. Left
// untracked it exists on exactly one machine, and the repository looks adopted
// while no other session ever sees the guidance. That is not hypothetical: two
// repositories in this project ran `ark init`, never committed `.claude/`, and
// carried Ark for weeks with no guidance at all.
//
// Committing rather than merely staging, because staging has the same defect
// one step further along: it still depends on a human noticing and finishing
// the job, and this failure is precisely that nobody ever does. Ark is being
// asked to set the repository up; carrying its own setup to a committed state
// is that task, not an overreach beyond it.
//
// Two guards keep it from being invasive. The commit names an explicit
// pathspec, so nothing else that happened to be staged is swept in. And the
// whole thing is best-effort — no Git, no identity configured, a locked index
// or a repository mid-merge must not fail `ark init`.
func commitSkill(ctx context.Context, root, path string) bool {
	g := &git.Repo{Dir: root}
	if g.IsTracked(ctx, path) {
		return false
	}
	if err := g.Add(ctx, path); err != nil {
		return false
	}
	return g.CommitPaths(ctx, "chore(ark): add the Ark agent skill\n\n"+
		"Installed by `ark init`. Tracked so every clone, worktree and cloud\n"+
		"sandbox gets the guidance — untracked it would exist on one machine.",
		path) == nil
}

type skillReport struct {
	Path      string `json:"path"`
	Written   bool   `json:"written"`
	Committed bool   `json:"committed"`
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
			committed := false
			if wrote {
				committed = commitSkill(cmd.Context(), a.Root, path)
			}
			rel, relErr := filepath.Rel(a.Root, path)
			if relErr != nil {
				rel = path
			}
			p := g.printer(cmd)
			return p.Result(skillReport{Path: path, Written: wrote, Committed: committed}, func() {
				if wrote {
					p.Line("Wrote %s", rel)
					if committed {
						p.Line("Committed it, so every clone gets the guidance.")
					} else {
						p.Line("NOT committed — track it yourself, or no other clone gets it:")
						p.Line("  git add %s && git commit -m 'chore(ark): add the Ark agent skill'", rel)
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

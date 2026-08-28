package cli

import (
	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/records"
	arksync "github.com/elk-work/ark/internal/sync"
	"github.com/elk-work/ark/pkg/api"
)

// `ark repo show` and `ark repo set`: the sync service's copy of a
// repository's name, default branch and Git remote.
//
// These are the only fields in Ark that could be set and never corrected.
// A sync registers them, and registration deliberately backfills rather than
// overwrites — the name a client sends is the basename of wherever it is
// checked out, so overwriting let any client rename a repository for
// everyone. Correcting therefore has to be its own act, and this is it.
//
// The repository is named by ULID, never inferred from the directory. That
// inference is the original bug: `ark repo set` defaults to the repository
// this checkout is *bound* to (`.ark/config.toml`), which is an identity a
// human chose, not a path that happens to be current.

// cliAgentName is the agent identity a write from this CLI registers under
// on the service. A remote write is always an agent acting under a human's
// authority (RFC-0004 Decision 2) — even when a person typed the command,
// what reached the service was this program.
const cliAgentName = "ark-cli"

// remoteWriter names the identity for a server-side write. An `--agent` run
// writes as that agent under whoever it already delegates from; a person
// writes as this CLI under themselves.
func (g *globals) remoteWriter(a *app.Context) api.Writer {
	actor := a.Store.Actor
	if actor.Type == records.ActorAgent {
		return api.Writer{AgentName: actor.AgentName, AgentVersion: actor.AgentVersion,
			DelegatedBy: actor.DelegatedBy}
	}
	return api.Writer{AgentName: cliAgentName, AgentVersion: g.version, DelegatedBy: actor.ID}
}

// targetRepo resolves which repository a command addresses: the one this
// checkout is bound to, or an explicit ULID for correcting another
// repository on the same service.
func targetRepo(a *app.Context, flag string) (string, error) {
	if flag == "" {
		return a.Config.RepositoryID, nil
	}
	if !records.ValidID(flag) {
		return "", records.Validationf("--repo takes a repository ULID, not %q", flag)
	}
	return flag, nil
}

func newRepoCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Inspect and correct this repository's record on the sync service",
	}
	cmd.AddCommand(newRepoShowCmd(g), newRepoSetCmd(g))
	return cmd
}

func newRepoShowCmd(g *globals) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show the repository record held by the sync service",
		Long: `Show the repository record the sync service holds.

This is the service's copy, not the local one: the name, default branch and
Git remote that appear when a repository is listed or recovered. ` + "`ark status`" + `
reports what this checkout knows; only this reaches the service.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			repoID, err := targetRepo(a, repoFlag)
			if err != nil {
				return err
			}
			client, err := arksync.Client(a)
			if err != nil {
				return err
			}
			meta, err := client.RepositoryMetadata(cmd.Context(), repoID)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(meta, func() { printRepoMetadata(g, cmd, *meta) })
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository ULID (defaults to this repository)")
	return cmd
}

func newRepoSetCmd(g *globals) *cobra.Command {
	var repoFlag, name, remote, branch string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Correct the repository record held by the sync service",
		Long: `Correct the name, default branch, or Git remote the sync service holds.

A sync can only ever fill in a field the service is missing, because the name
a client sends is the basename of wherever it is checked out — overwriting on
every sync let one client rename the repository for everyone. So a value that
was wrong when the repository was first registered stays wrong until it is
corrected here.

Only the flags you pass are asserted; everything else is left alone.
--git-remote "" is the one way to clear a field, for a repository that
genuinely has no remote.

The repository is addressed by ULID. Without --repo that is the repository
this checkout is bound to (.ark/config.toml), never the directory name.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Say so before opening anything: a command that asserts nothing
			// is a mistyped command, not a no-op write.
			flags := cmd.Flags()
			if !flags.Changed("name") && !flags.Changed("git-remote") && !flags.Changed("default-branch") {
				return records.Validationf("nothing to set (pass --name, --git-remote, or --default-branch)")
			}
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			repoID, err := targetRepo(a, repoFlag)
			if err != nil {
				return err
			}
			client, err := arksync.Client(a)
			if err != nil {
				return err
			}

			req := api.SetRepositoryMetadataRequest{Writer: g.remoteWriter(a)}
			if flags.Changed("name") {
				req.Name = &name
			}
			if flags.Changed("git-remote") {
				req.GitRemoteURL = &remote
			}
			if flags.Changed("default-branch") {
				req.DefaultBranch = &branch
			}
			resp, err := client.SetRepositoryMetadata(cmd.Context(), repoID, req)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(resp, func() {
				if resp.Changed {
					p.Line("Updated on %s", a.Config.Remote)
				} else {
					p.Line("Already set on %s; nothing changed.", a.Config.Remote)
				}
				printRepoMetadata(g, cmd, resp.Repository)
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository ULID (defaults to this repository)")
	cmd.Flags().StringVar(&name, "name", "", "repository name")
	cmd.Flags().StringVar(&remote, "git-remote", "", `Git remote URL (pass "" to clear it)`)
	cmd.Flags().StringVar(&branch, "default-branch", "", "default branch name")
	return cmd
}

func printRepoMetadata(g *globals, cmd *cobra.Command, meta api.RepositoryMetadata) {
	p := g.printer(cmd)
	p.Line("id          %s", meta.ID)
	p.Line("name        %s", meta.Name)
	p.Line("branch      %s", meta.DefaultBranch)
	if meta.GitRemoteURL == "" {
		p.Line("git remote  (none)")
	} else {
		p.Line("git remote  %s", meta.GitRemoteURL)
	}
	p.Line("revision    %d", meta.Revision)
}

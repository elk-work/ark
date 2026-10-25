package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
)

// `ark repo git-url`: where this repository's Git is served when the sync
// service hosts it (elk-work/ark#125).
//
// It prints a URL and changes nothing. Adding the remote is left to the
// person — `git remote add ark "$(ark repo git-url)"` — because a Git remote
// is the checkout's own configuration, and Ark has never written it.

// gitURL is the clone URL for a repository on a sync service.
func gitURL(remote, repoID string) string {
	return strings.TrimRight(remote, "/") + "/git/" + repoID
}

func newRepoGitURLCmd(g *globals) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "git-url",
		Short: "Print the Git clone URL for this repository on the sync service",
		Long: `Print the Git clone URL for this repository on the sync service.

The service hosts a bare Git repository per Ark repository, served over
smart HTTP. Git authenticates with your Ark credential: any username, the
credential as the password, which git's credential helper stores after the
first prompt. A read grant clones and fetches; a write grant pushes.

Nothing is changed. To use it:

  git remote add ark "$(ark repo git-url)"
  git push ark main`,
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
			if a.Config.Remote == "" {
				return records.Validationf("no sync service configured: run `ark remote set <sync-service-url>` first")
			}
			url := gitURL(a.Config.Remote, repoID)
			p := g.printer(cmd)
			return p.Result(map[string]string{"repository_id": repoID, "git_url": url}, func() {
				p.Line("%s", url)
			})
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository ULID (defaults to this repository)")
	return cmd
}

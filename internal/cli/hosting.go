package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	arksync "github.com/elk-work/ark/internal/sync"
	"github.com/elk-work/ark/pkg/api"
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

// `ark repo mirror set|show|clear`: where the service mirrors this
// repository's Git. The mirror is a backup — the service pushes `git push
// --mirror` to it after every push and on a ticker — and the service's copy
// stays the record.

func newRepoMirrorCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mirror",
		Short: "Set, show, or clear where the sync service mirrors this repository's Git",
		Long: `Set, show, or clear where the sync service mirrors this repository's Git.

The service hosts this repository's Git (see ` + "`ark repo git-url`" + `) and can
push all of it to a second host — GitHub, in practice — as a backup: after
every push that changes something, and every ARK_MIRROR_INTERVAL (10 minutes
by default) while the refs differ from the last successful mirror. It runs
` + "`git push --mirror`" + `, so the mirror ends up with exactly the service's refs;
point it at a repository that holds nothing else.

The mirror is pushed with the service's own credential (ARK_MIRROR_CREDENTIAL
on the server), never one in the URL. That is why setting or clearing it is
an operator act as well as an admin one.`,
	}
	cmd.AddCommand(newRepoMirrorSetCmd(g), newRepoMirrorShowCmd(g), newRepoMirrorClearCmd(g))
	return cmd
}

// setMirror writes the mirror URL ("" clears it) and prints the result.
func setMirror(g *globals, cmd *cobra.Command, repoFlag, mirror string) error {
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
	resp, err := client.SetRepositoryMetadata(cmd.Context(), repoID,
		api.SetRepositoryMetadataRequest{Writer: g.remoteWriter(a), MirrorURL: &mirror})
	if err != nil {
		return err
	}
	p := g.printer(cmd)
	return p.Result(resp, func() {
		switch {
		case mirror == "" && resp.Changed:
			p.Line("%s is no longer mirrored.", repoID)
		case mirror == "":
			p.Line("%s had no mirror; nothing changed.", repoID)
		case resp.Changed:
			p.Line("%s now mirrors to %s.", repoID, mirror)
			p.Line("The next push mirrors it; until then the service tries every ARK_MIRROR_INTERVAL.")
		default:
			p.Line("%s already mirrors to %s; nothing changed.", repoID, mirror)
		}
	})
}

func newRepoMirrorSetCmd(g *globals) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "set <https url>",
		Short: "Mirror this repository's Git to an https Git remote",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			mirror := strings.TrimSpace(args[0])
			if !strings.HasPrefix(mirror, "https://") {
				return records.Validationf("the mirror must be an https URL, such as https://github.com/<owner>/<repo>.git")
			}
			return setMirror(g, cmd, repoFlag, mirror)
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository ULID (defaults to this repository)")
	return cmd
}

func newRepoMirrorClearCmd(g *globals) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Stop mirroring this repository's Git",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return setMirror(g, cmd, repoFlag, "")
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository ULID (defaults to this repository)")
	return cmd
}

func newRepoMirrorShowCmd(g *globals) *cobra.Command {
	var repoFlag string
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show where this repository's Git is mirrored and how the last attempt went",
		Args:  cobra.NoArgs,
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
			return p.Result(meta.Mirror, func() { printMirror(p, meta.Mirror) })
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository ULID (defaults to this repository)")
	return cmd
}

// printMirror is the human view of a mirror's state.
func printMirror(p linePrinter, m api.MirrorStatus) {
	if m.URL == "" {
		p.Line("mirror      (none)")
		return
	}
	p.Line("mirror      %s", m.URL)
	switch {
	case m.LastAttemptAt == "":
		p.Line("            not attempted yet on this service instance")
	case m.LastError != "":
		p.Line("            last attempt %s FAILED: %s", m.LastAttemptAt, m.LastError)
		if m.LastOKAt != "" {
			p.Line("            last success %s", m.LastOKAt)
		}
	default:
		p.Line("            last success %s", m.LastOKAt)
	}
}

// linePrinter is the part of the printer printMirror needs.
type linePrinter interface {
	Line(format string, args ...any)
}

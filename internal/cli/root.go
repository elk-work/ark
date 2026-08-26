// Package cli implements the ark command tree.
package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/output"
	"github.com/elk-work/ark/internal/records"
)

// globals carries flag state shared by every command.
type globals struct {
	json    bool
	dir     string
	actorID string
	agent   string
	debug   bool
}

func (g *globals) printer(cmd *cobra.Command) *output.Printer {
	return &output.Printer{W: cmd.OutOrStdout(), JSON: g.json}
}

func (g *globals) options() app.Options {
	opts := app.Options{
		ActorID:      firstNonEmpty(g.actorID, os.Getenv("ARK_ACTOR_ID")),
		AgentName:    firstNonEmpty(g.agent, os.Getenv("ARK_AGENT_NAME")),
		AgentVersion: os.Getenv("ARK_AGENT_VERSION"),
		DelegatedBy:  os.Getenv("ARK_DELEGATED_BY"),
	}
	if g.debug {
		opts.Debug = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "debug: "+format+"\n", args...)
		}
	}
	return opts
}

// open loads the Ark context for the working directory.
func (g *globals) open(cmd *cobra.Command) (*app.Context, error) {
	dir := g.dir
	if dir == "" {
		var err error
		if dir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}
	return app.Open(cmd.Context(), dir, g.options())
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// New builds the root ark command.
func New(version string) *cobra.Command {
	g := &globals{}
	root := &cobra.Command{
		Use:   "ark",
		Short: "Ark keeps the history of the work around your code",
		Long: `Ark is a local-first, agent-native work record system that sits beside Git.

Git stores source history. Ark stores the work around it: tasks, comments,
agent threads and runs, pull requests, reviews, and artifacts. All records
live in .ark/ next to .git/ and sync to a shared service when configured.`,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().BoolVar(&g.json, "json", false, "machine-readable JSON output")
	root.PersistentFlags().StringVarP(&g.dir, "dir", "C", "", "run as if started in this directory")
	root.PersistentFlags().StringVar(&g.actorID, "actor", "", "act as this actor ID (or set ARK_ACTOR_ID)")
	root.PersistentFlags().StringVar(&g.agent, "agent", "", "act as this named agent (or set ARK_AGENT_NAME)")
	root.PersistentFlags().BoolVar(&g.debug, "debug", false, "log Git and database activity to stderr")

	root.AddCommand(
		newInitCmd(g),
		newStatusCmd(g),
		newSyncCmd(g),
		newRemoteCmd(g),
		newLoginCmd(g),
		newTaskCmd(g),
		newThreadCmd(g),
		newRunCmd(g),
		newReviewCmd(g),
		newPRCmd(g),
		newPromotionCmd(g),
		newArtifactCmd(g),
		newConflictCmd(g),
		newSearchCmd(g),
		newGHCmd(g),
		newElkCmd(g),
		newSkillCmd(g),
	)
	return root
}

// Execute runs the CLI and maps errors to the exit-code contract.
func Execute(version string) int {
	root := New(version)
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "ark: %v\n", err)
		return records.ExitCode(err)
	}
	return 0
}

// body reads --body, or --body-file (use "-" for stdin) when set.
func body(cmd *cobra.Command) (string, error) {
	b, _ := cmd.Flags().GetString("body")
	file, _ := cmd.Flags().GetString("body-file")
	if file == "" {
		return b, nil
	}
	if b != "" {
		return "", records.Validationf("pass --body or --body-file, not both")
	}
	if file == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", records.Validationf("read stdin: %v", err)
		}
		return string(data), nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return "", records.NotFoundf("cannot read %s: %v", file, err)
	}
	return string(data), nil
}

func addBodyFlags(cmd *cobra.Command) {
	cmd.Flags().StringP("body", "b", "", "body text")
	cmd.Flags().String("body-file", "", "read body from file (- for stdin)")
}

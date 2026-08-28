// Package cli implements the ark command tree.
package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

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
	// version is this binary's version string. It travels as the agent
	// version of a write this CLI makes against the sync service, so the
	// service can say which build asserted a change.
	version string
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
	g := &globals{version: version}
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
		newRepoCmd(g),
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
	// Do this last, so it covers every command just added.
	reportInputErrors(root)
	return root
}

// reportInputErrors makes Cobra's own parse failures obey the exit-code
// contract (spec §22: 2 for invalid input).
//
// Cobra reports a missing required flag, an unknown flag, a bad flag value and
// a wrong argument count as a plain error, which records.ExitCode can only
// score as 1 — a general failure. From outside, "unknown flag" and "invalid
// status" are the same class of mistake, and --json plus the exit codes are
// the interface agents script against, so the two have to agree.
func reportInputErrors(root *cobra.Command) {
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return records.Validationf("%v", err)
	})

	// Cobra validates required flags itself, but only after PersistentPreRunE
	// and as a plain error, so get there first. If Cobra ever renames the
	// annotation this check silently matches nothing and Cobra's own runs
	// instead, dropping the exit code back to 1 — which is exactly what
	// TestInputErrorsExitTwo asserts against.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		var missing []string
		cmd.Flags().VisitAll(func(f *pflag.Flag) {
			req := f.Annotations[cobra.BashCompOneRequiredFlag]
			if len(req) > 0 && req[0] == "true" && !f.Changed {
				missing = append(missing, strconv.Quote(f.Name))
			}
		})
		if len(missing) > 0 {
			return records.Validationf("required flag(s) %s not set", strings.Join(missing, ", "))
		}
		return nil
	}

	markInputErrors(root)
}

func markInputErrors(cmd *cobra.Command) {
	if cmd.Run == nil && cmd.RunE == nil && cmd.HasSubCommands() {
		// A command group is not Runnable, and Cobra returns "print the help"
		// for a non-runnable command BEFORE it ever validates arguments — so
		// `ark task lst` printed help and exited 0, reporting success for work
		// it did not do. Give the group a Run so the unknown subcommand is
		// reached at all, and non-nil Args so Find stops short-circuiting into
		// its own unknown-command error on the way past.
		cmd.Args = cobra.ArbitraryArgs
		cmd.RunE = func(c *cobra.Command, args []string) error {
			if len(args) > 0 {
				return records.Validationf("unknown command %q for %q", args[0], c.CommandPath())
			}
			return c.Help()
		}
	}
	if inner := cmd.Args; inner != nil {
		cmd.Args = func(c *cobra.Command, args []string) error {
			if err := inner(c, args); err != nil {
				return records.Validationf("%v", err)
			}
			return nil
		}
	}
	for _, sub := range cmd.Commands() {
		markInputErrors(sub)
	}
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

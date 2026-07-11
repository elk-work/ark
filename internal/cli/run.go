package cli

import (
	"github.com/spf13/cobra"

	"github.com/ijroth/ark/internal/records"
	"github.com/ijroth/ark/internal/store"
)

func newRunCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Record agent executions",
	}
	cmd.AddCommand(
		newRunStartCmd(g),
		newRunFinishCmd(g),
		newRunListCmd(g),
		newRunViewCmd(g),
	)
	return cmd
}

func newRunStartCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Record the start of an agent run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()

			r := &store.Run{}
			r.AgentName, _ = cmd.Flags().GetString("agent-name")
			if r.AgentName == "" {
				// The acting agent identity doubles as the run's agent.
				r.AgentName = a.Store.Actor.AgentName
			}
			r.AgentVersion, _ = cmd.Flags().GetString("agent-version")
			r.InputSummary, _ = cmd.Flags().GetString("input")
			r.MetadataJSON, _ = cmd.Flags().GetString("metadata")

			if taskRef, _ := cmd.Flags().GetString("task"); taskRef != "" {
				t, err := a.Store.ResolveTask(ctx, taskRef)
				if err != nil {
					return err
				}
				r.TaskID = t.ID
			}
			if threadRef, _ := cmd.Flags().GetString("thread"); threadRef != "" {
				th, err := a.Store.ResolveThread(ctx, threadRef)
				if err != nil {
					return err
				}
				r.ThreadID = th.ID
			}

			// Capture the Git position the run starts from.
			if branch, err := a.Git.CurrentBranch(ctx); err == nil {
				r.BranchName = branch
			}
			if head, err := a.Git.Head(ctx); err == nil {
				r.BaseCommitSHA = head
			}
			if v, _ := cmd.Flags().GetString("branch"); v != "" {
				r.BranchName = v
			}
			if v, _ := cmd.Flags().GetString("base-commit"); v != "" {
				r.BaseCommitSHA = v
			}

			r, err = a.Store.StartRun(ctx, r)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(r, func() {
				p.Line("Started run %s (%s on %s)", r.ID, r.AgentName, firstNonEmpty(r.BranchName, "detached HEAD"))
			})
		},
	}
	cmd.Flags().String("agent-name", "", "agent performing the run (defaults to --agent identity)")
	cmd.Flags().String("agent-version", "", "agent version")
	cmd.Flags().String("task", "", "task this run works on (number or id)")
	cmd.Flags().String("thread", "", "thread this run belongs to (id)")
	cmd.Flags().StringP("input", "i", "", "what the agent was asked to do")
	cmd.Flags().String("branch", "", "override recorded branch name")
	cmd.Flags().String("base-commit", "", "override recorded base commit SHA")
	cmd.Flags().String("metadata", "", "structured metadata as JSON")
	return cmd
}

func newRunFinishCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "finish <id>",
		Short: "Record the completion of an agent run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()

			fin := store.RunFinish{}
			fin.Status, _ = cmd.Flags().GetString("status")
			fin.ResultSummary, _ = cmd.Flags().GetString("result")
			fin.ResultCommitSHA, _ = cmd.Flags().GetString("commit")
			if cmd.Flags().Changed("exit-code") {
				v, _ := cmd.Flags().GetInt64("exit-code")
				fin.ExitCode = &v
			}
			if fin.ResultCommitSHA == "" {
				// Default the result commit to wherever HEAD is now.
				if head, err := a.Git.Head(ctx); err == nil {
					fin.ResultCommitSHA = head
				}
			}
			r, err := a.Store.FinishRun(ctx, args[0], fin)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(r, func() {
				p.Line("Finished run %s: %s", r.ID, r.Status)
			})
		},
	}
	cmd.Flags().StringP("status", "s", "succeeded", "terminal status (succeeded, failed, cancelled)")
	cmd.Flags().StringP("result", "r", "", "what the run produced")
	cmd.Flags().String("commit", "", "resulting commit SHA (defaults to current HEAD)")
	cmd.Flags().Int64("exit-code", 0, "process exit code")
	return cmd
}

func newRunListCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List agent runs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()
			taskRef, _ := cmd.Flags().GetString("task")
			var taskID string
			if taskRef != "" {
				t, err := a.Store.ResolveTask(ctx, taskRef)
				if err != nil {
					return err
				}
				taskID = t.ID
			}
			runs, err := a.Store.ListRuns(ctx, taskID)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if p.JSON {
				if runs == nil {
					runs = []*store.Run{}
				}
				return p.JSONValue(runs)
			}
			if len(runs) == 0 {
				p.Line("no runs")
				return nil
			}
			rows := make([][]string, len(runs))
			for i, r := range runs {
				rows[i] = []string{shortID(r.ID), r.Status, r.AgentName, r.BranchName,
					records.FormatTime(r.CreatedAt)}
			}
			p.Table([]string{"RUN", "STATUS", "AGENT", "BRANCH", "STARTED"}, rows)
			return nil
		},
	}
	cmd.Flags().String("task", "", "only runs for this task (number or id)")
	return cmd
}

func newRunViewCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "view <id>",
		Short: "Show one agent run",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			r, err := a.Store.ResolveRun(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(r, func() {
				p.Line("run      %s", r.ID)
				p.Line("agent    %s %s", r.AgentName, r.AgentVersion)
				p.Line("status   %s", r.Status)
				if r.TaskID != "" {
					p.Line("task     %s", r.TaskID)
				}
				if r.ThreadID != "" {
					p.Line("thread   %s", r.ThreadID)
				}
				if r.BranchName != "" {
					p.Line("branch   %s (base %s)", r.BranchName, short(r.BaseCommitSHA))
				}
				if r.ResultCommitSHA != "" {
					p.Line("result   %s", short(r.ResultCommitSHA))
				}
				if r.ExitCode != nil {
					p.Line("exit     %d", *r.ExitCode)
				}
				if r.InputSummary != "" {
					p.Line("input    %s", r.InputSummary)
				}
				if r.ResultSummary != "" {
					p.Line("output   %s", r.ResultSummary)
				}
				p.Line("timing   %s → %s", records.FormatTime(r.StartedAt),
					firstNonEmpty(records.FormatTime(r.FinishedAt), "running"))
			})
		},
	}
}

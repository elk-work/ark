package cli

import (
	"github.com/spf13/cobra"

	"github.com/ijroth/ark/internal/records"
	"github.com/ijroth/ark/internal/store"
)

func newThreadCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "thread",
		Short: "Record the conversations around work",
	}
	cmd.AddCommand(
		newThreadCreateCmd(g),
		newThreadListCmd(g),
		newThreadViewCmd(g),
		newThreadMessageCmd(g),
		newThreadCloseCmd(g),
	)
	return cmd
}

func newThreadCreateCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a thread, optionally linked to a task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()
			title, _ := cmd.Flags().GetString("title")
			taskRef, _ := cmd.Flags().GetString("task")
			var taskID string
			if taskRef != "" {
				t, err := a.Store.ResolveTask(ctx, taskRef)
				if err != nil {
					return err
				}
				taskID = t.ID
			}
			th, err := a.Store.CreateThread(ctx, taskID, title)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(th, func() {
				p.Line("Created thread %s: %s", th.ID, th.Title)
			})
		},
	}
	cmd.Flags().StringP("title", "t", "", "thread title (required)")
	cmd.Flags().String("task", "", "link to task (number or id)")
	cmd.MarkFlagRequired("title")
	return cmd
}

func newThreadListCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List threads",
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
			threads, err := a.Store.ListThreads(ctx, taskID)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if p.JSON {
				if threads == nil {
					threads = []*store.Thread{}
				}
				return p.JSONValue(threads)
			}
			if len(threads) == 0 {
				p.Line("no threads")
				return nil
			}
			rows := make([][]string, len(threads))
			for i, th := range threads {
				rows[i] = []string{shortID(th.ID), th.Status,
					records.Truncate(th.Title, 60), records.FormatTime(th.CreatedAt)}
			}
			p.Table([]string{"THREAD", "STATUS", "TITLE", "CREATED"}, rows)
			return nil
		},
	}
	cmd.Flags().String("task", "", "only threads for this task (number or id)")
	return cmd
}

type threadDetail struct {
	*store.Thread
	Messages []*store.Message `json:"messages"`
}

func newThreadViewCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "view <id>",
		Short: "Show a thread and its messages",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()
			th, err := a.Store.ResolveThread(ctx, args[0])
			if err != nil {
				return err
			}
			msgs, err := a.Store.ListMessages(ctx, th.ID)
			if err != nil {
				return err
			}
			if msgs == nil {
				msgs = []*store.Message{}
			}
			names, err := store.ActorNames(ctx, a.DB)
			if err != nil {
				return err
			}
			d := threadDetail{Thread: th, Messages: msgs}
			p := g.printer(cmd)
			return p.Result(d, func() {
				p.Line("thread %s  [%s]", th.ID, th.Status)
				p.Line("title  %s", th.Title)
				if th.TaskID != "" {
					p.Line("task   %s", th.TaskID)
				}
				for _, m := range msgs {
					p.Line("\n[%s] %s (%s) at %s", m.Role, actorName(names, m.CreatedBy),
						m.CreatedByType, records.FormatTime(m.CreatedAt))
					p.Line("%s", m.Body)
				}
			})
		},
	}
}

func newThreadMessageCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "message <id>",
		Short: "Append a message to a thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			b, err := body(cmd)
			if err != nil {
				return err
			}
			role, _ := cmd.Flags().GetString("role")
			meta, _ := cmd.Flags().GetString("metadata")
			m, err := a.Store.AddMessage(cmd.Context(), args[0], role, b, meta)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(m, func() {
				p.Line("Added %s message %s", m.Role, m.ID)
			})
		},
	}
	addBodyFlags(cmd)
	cmd.Flags().StringP("role", "r", "user", "message role (user, agent, system, tool)")
	cmd.Flags().String("metadata", "", "structured metadata as JSON")
	return cmd
}

func newThreadCloseCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "close <id>",
		Short: "Close a thread",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			th, err := a.Store.CloseThread(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(th, func() {
				p.Line("Closed thread %s: %s", th.ID, th.Title)
			})
		},
	}
}

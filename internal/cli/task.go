package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

func newTaskCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "task",
		Short: "Create and manage tasks",
	}
	cmd.AddCommand(
		newTaskCreateCmd(g),
		newTaskListCmd(g),
		newTaskViewCmd(g),
		newTaskEditCmd(g),
		newTaskCloseCmd(g),
		newTaskCommentCmd(g),
	)
	return cmd
}

// taskCreated is `ark task create`'s result: the task, plus the Elk parent
// the call posted as a marker comment. Additive over store.Task's JSON.
type taskCreated struct {
	*store.Task
	ElkRef string `json:"elk_ref,omitempty"`
}

func newTaskCreateCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a task",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			title, _ := cmd.Flags().GetString("title")
			b, err := body(cmd)
			if err != nil {
				return err
			}
			elkRef := ""
			if cmd.Flags().Changed("elk") {
				raw, _ := cmd.Flags().GetString("elk")
				if elkRef, err = validElkRef(raw); err != nil {
					return err
				}
			}
			// The refusal is this repository's own rule (ark config set
			// require-elk-parent true), checked before anything is written so a
			// refused create leaves no record behind.
			if elkRef == "" && a.Config.RequireElkParent {
				return records.Validationf("this repository requires an Elk parent for every task: " +
					"pass --elk <ref> (#35, 35, elk:35, or the Elk action id), " +
					"or have Elk create the task with push_to_work_record")
			}
			t, err := a.Store.CreateTask(cmd.Context(), title, b)
			if err != nil {
				return err
			}
			if elkRef != "" {
				if _, err := a.Store.AddComment(cmd.Context(), "task", t.ID, elkParentMarker(elkRef), ""); err != nil {
					return fmt.Errorf("task #%d (%s) was created, but posting its elk-parent marker failed: %w", t.Number, t.ID, err)
				}
			}
			p := g.printer(cmd)
			return p.Result(taskCreated{Task: t, ElkRef: elkRef}, func() {
				p.Line("Created task #%d: %s (%s)", t.Number, t.Title, t.ID)
				if elkRef != "" {
					p.Line("Elk parent: %s", elkRef)
				}
			})
		},
	}
	cmd.Flags().StringP("title", "t", "", "task title (required)")
	addBodyFlags(cmd)
	cmd.Flags().String("elk", "", "the Elk task this is a step of (#35, 35, elk:35, or the action id); posted as an elk-parent comment")
	cmd.MarkFlagRequired("title")
	return cmd
}

func newTaskListCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List tasks",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			status, _ := cmd.Flags().GetString("status")
			tasks, err := a.Store.ListTasks(cmd.Context(), status)
			if err != nil {
				return err
			}
			if cmd.Flags().Changed("elk") {
				raw, _ := cmd.Flags().GetString("elk")
				want, err := validElkRef(raw)
				if err != nil {
					return err
				}
				tasks, err = filterByElkParent(cmd.Context(), a.Store, tasks, want)
				if err != nil {
					return err
				}
			}
			p := g.printer(cmd)
			if p.JSON {
				if tasks == nil {
					tasks = []*store.Task{}
				}
				return p.JSONValue(tasks)
			}
			if len(tasks) == 0 {
				p.Line("no tasks")
				return nil
			}
			rows := make([][]string, len(tasks))
			for i, t := range tasks {
				rows[i] = []string{fmt.Sprintf("#%d", t.Number), t.Status,
					records.Truncate(t.Title, 60), records.FormatTime(t.UpdatedAt)}
			}
			p.Table([]string{"TASK", "STATUS", "TITLE", "UPDATED"}, rows)
			return nil
		},
	}
	cmd.Flags().StringP("status", "s", "", "filter by status (open, in_progress, blocked, done, closed, all)")
	cmd.Flags().String("elk", "", "only tasks whose latest elk-parent marker names this Elk task (#35, 35, elk:35, or the action id)")
	return cmd
}

// taskDetail is the composite returned by `ark task view`.
type taskDetail struct {
	*store.Task
	// ElkRef is the Elk parent named by the latest `elk-parent:` marker
	// comment, verbatim. Empty when the task has none.
	ElkRef   string           `json:"elk_ref,omitempty"`
	Comments []*store.Comment `json:"comments"`
	Threads  []*store.Thread  `json:"threads"`
	Runs     []*store.Run     `json:"runs"`
}

func newTaskViewCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "view <number|id>",
		Short: "Show a task with its comments, threads, and runs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()
			t, err := a.Store.ResolveTask(ctx, args[0])
			if err != nil {
				return err
			}
			d := taskDetail{Task: t}
			if d.Comments, err = a.Store.ListComments(ctx, "task", t.ID); err != nil {
				return err
			}
			d.ElkRef = latestElkParent(d.Comments)
			if d.Threads, err = a.Store.ListThreads(ctx, t.ID); err != nil {
				return err
			}
			if d.Runs, err = a.Store.ListRuns(ctx, t.ID); err != nil {
				return err
			}
			names, err := store.ActorNames(ctx, a.DB)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if d.Comments == nil {
				d.Comments = []*store.Comment{}
			}
			if d.Threads == nil {
				d.Threads = []*store.Thread{}
			}
			if d.Runs == nil {
				d.Runs = []*store.Run{}
			}
			return p.Result(d, func() {
				p.Line("task #%d  %s", t.Number, t.Title)
				p.Line("status    %s", t.Status)
				p.Line("id        %s", t.ID)
				if d.ElkRef != "" {
					p.Line("elk       %s", d.ElkRef)
				}
				p.Line("created   %s by %s (%s)", records.FormatTime(t.CreatedAt),
					actorName(names, t.CreatedBy), t.CreatedByType)
				if t.Body != "" {
					p.Line("\n%s", t.Body)
				}
				if len(d.Threads) > 0 {
					p.Line("\nthreads:")
					for _, th := range d.Threads {
						p.Line("  %s  [%s] %s", shortID(th.ID), th.Status, th.Title)
					}
				}
				if len(d.Runs) > 0 {
					p.Line("\nruns:")
					for _, r := range d.Runs {
						p.Line("  %s  [%s] %s — %s", shortID(r.ID), r.Status, r.AgentName,
							records.Truncate(firstNonEmpty(r.ResultSummary, r.InputSummary), 50))
					}
				}
				if len(d.Comments) > 0 {
					p.Line("\ncomments:")
					for _, c := range d.Comments {
						p.Line("  [%s, %s (%s)]", records.FormatTime(c.CreatedAt),
							actorName(names, c.CreatedBy), c.CreatedByType)
						p.Line("  %s", c.Body)
					}
				}
			})
		},
	}
}

func newTaskEditCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "edit <number|id>",
		Short: "Edit a task's title, body, or status",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			var edit store.TaskEdit
			if cmd.Flags().Changed("title") {
				v, _ := cmd.Flags().GetString("title")
				edit.Title = &v
			}
			if cmd.Flags().Changed("body") || cmd.Flags().Changed("body-file") {
				v, err := body(cmd)
				if err != nil {
					return err
				}
				edit.Body = &v
			}
			if cmd.Flags().Changed("status") {
				v, _ := cmd.Flags().GetString("status")
				edit.Status = &v
			}
			elkRef := ""
			if cmd.Flags().Changed("elk") {
				raw, _ := cmd.Flags().GetString("elk")
				if elkRef, err = validElkRef(raw); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			var t *store.Task
			if edit.Title != nil || edit.Body != nil || edit.Status != nil {
				if t, err = a.Store.UpdateTask(ctx, args[0], edit); err != nil {
					return err
				}
			} else if elkRef != "" {
				if t, err = a.Store.ResolveTask(ctx, args[0]); err != nil {
					return err
				}
			} else {
				return records.Validationf("nothing to change (pass --title, --body, --status, or --elk)")
			}
			// Re-parenting is a NEW marker; the old one stays as history, which
			// is the append-only rule every comment already follows.
			if elkRef != "" {
				if _, err := a.Store.AddComment(ctx, "task", t.ID, elkParentMarker(elkRef), ""); err != nil {
					return fmt.Errorf("task #%d (%s) was updated, but posting its elk-parent marker failed: %w", t.Number, t.ID, err)
				}
			}
			p := g.printer(cmd)
			return p.Result(taskCreated{Task: t, ElkRef: elkRef}, func() {
				p.Line("Updated task #%d: %s [%s]", t.Number, t.Title, t.Status)
				if elkRef != "" {
					p.Line("Elk parent: %s", elkRef)
				}
			})
		},
	}
	cmd.Flags().StringP("title", "t", "", "new title")
	addBodyFlags(cmd)
	cmd.Flags().StringP("status", "s", "", "new status (open, in_progress, blocked, done, closed)")
	cmd.Flags().String("elk", "", "move this task under another Elk task (#35, 35, elk:35, or the action id)")
	return cmd
}

func newTaskCloseCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "close <number|id>",
		Short: "Close a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			t, err := a.Store.CloseTask(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(t, func() {
				p.Line("Closed task #%d: %s", t.Number, t.Title)
			})
		},
	}
}

func newTaskCommentCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "comment <number|id>",
		Short: "Comment on a task",
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
			t, err := a.Store.ResolveTask(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			c, err := a.Store.AddComment(cmd.Context(), "task", t.ID, b, "")
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(c, func() {
				p.Line("Commented on task #%d (%s)", t.Number, c.ID)
			})
		},
	}
	addBodyFlags(cmd)
	return cmd
}

func actorName(names map[string]string, id string) string {
	if n, ok := names[id]; ok {
		return n
	}
	return shortID(id)
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

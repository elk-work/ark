package cli

import (
	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

// newGHCmd provides a limited GitHub CLI compatibility surface so agents
// that know `gh issue ...` and `gh pr ...` can use Ark without new habits.
// Mapping: GitHub issue -> Ark task, GitHub PR -> Ark pull request.
// See docs/v1-spec.md §14.
func newGHCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "gh",
		Short: "GitHub CLI compatibility (issues map to tasks)",
		Long: `A limited GitHub CLI compatibility surface.

GitHub issues map to Ark tasks; GitHub pull requests map to Ark pull
requests. Only the common subcommands are supported; this is a working
vocabulary for agents, not a full gh clone.`,
	}
	cmd.AddCommand(newGHIssueCmd(g), newGHPRCmd(g))
	return cmd
}

func newGHIssueCmd(g *globals) *cobra.Command {
	issue := &cobra.Command{Use: "issue", Short: "Work with issues (Ark tasks)"}

	create := &cobra.Command{
		Use:   "create",
		Short: "Create an issue",
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
			t, err := a.Store.CreateTask(cmd.Context(), title, b)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(t, func() {
				// gh prints the issue URL; Ark prints the task reference.
				p.Line("#%d", t.Number)
			})
		},
	}
	create.Flags().StringP("title", "t", "", "issue title")
	addBodyFlags(create)
	create.MarkFlagRequired("title")

	list := &cobra.Command{
		Use:   "list",
		Short: "List issues",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			state, _ := cmd.Flags().GetString("state")
			status, err := ghStateToTaskStatus(state)
			if err != nil {
				return err
			}
			tasks, err := a.Store.ListTasks(cmd.Context(), status)
			if err != nil {
				return err
			}
			// gh keeps closed issues out of the default list.
			if status == "" {
				open := tasks[:0]
				for _, t := range tasks {
					if t.Status != "closed" && t.Status != "done" {
						open = append(open, t)
					}
				}
				tasks = open
			}
			p := g.printer(cmd)
			if p.JSON {
				if tasks == nil {
					tasks = []*store.Task{}
				}
				return p.JSONValue(tasks)
			}
			for _, t := range tasks {
				p.Line("#%d\t%s\t%s", t.Number, t.Status, t.Title)
			}
			return nil
		},
	}
	list.Flags().StringP("state", "s", "open", "filter by state (open, closed, all)")

	view := &cobra.Command{
		Use:   "view <number>",
		Short: "View an issue",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Same composite view as `ark task view`.
			taskView := newTaskViewCmd(g)
			taskView.SetContext(cmd.Context())
			taskView.SetOut(cmd.OutOrStdout())
			return taskView.RunE(taskView, args)
		},
	}

	comment := &cobra.Command{
		Use:   "comment <number>",
		Short: "Comment on an issue",
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
				p.Line("Commented on #%d", t.Number)
			})
		},
	}
	addBodyFlags(comment)

	closeCmd := &cobra.Command{
		Use:   "close <number>",
		Short: "Close an issue",
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
				p.Line("Closed #%d", t.Number)
			})
		},
	}

	issue.AddCommand(create, list, view, comment, closeCmd)
	return issue
}

func newGHPRCmd(g *globals) *cobra.Command {
	pr := &cobra.Command{Use: "pr", Short: "Work with pull requests"}

	create := &cobra.Command{
		Use:   "create",
		Short: "Create a pull request",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Delegate to `ark pr create` semantics with gh flag names.
			inner := newPRCreateCmd(g)
			inner.SetContext(cmd.Context())
			inner.SetOut(cmd.OutOrStdout())
			title, _ := cmd.Flags().GetString("title")
			inner.Flags().Set("title", title)
			if b, _ := cmd.Flags().GetString("body"); b != "" {
				inner.Flags().Set("body", b)
			}
			if v, _ := cmd.Flags().GetString("base"); v != "" {
				inner.Flags().Set("base", v)
			}
			if v, _ := cmd.Flags().GetString("head"); v != "" {
				inner.Flags().Set("head", v)
			}
			return inner.RunE(inner, nil)
		},
	}
	create.Flags().StringP("title", "t", "", "pull request title")
	create.Flags().StringP("body", "b", "", "pull request body")
	create.Flags().StringP("base", "B", "", "base branch")
	create.Flags().StringP("head", "H", "", "head branch")
	create.MarkFlagRequired("title")

	list := &cobra.Command{
		Use:   "list",
		Short: "List pull requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			state, _ := cmd.Flags().GetString("state")
			if state == "" {
				state = "open"
			}
			prs, err := a.Store.ListPRs(cmd.Context(), state)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if p.JSON {
				if prs == nil {
					prs = []*store.PullRequest{}
				}
				return p.JSONValue(prs)
			}
			for _, x := range prs {
				p.Line("#%d\t%s\t%s\t%s", x.Number, x.Status, x.Title, x.HeadBranch)
			}
			return nil
		},
	}
	list.Flags().StringP("state", "s", "open", "filter by state (open, merged, closed, all)")

	view := &cobra.Command{
		Use:   "view <number>",
		Short: "View a pull request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inner := newPRViewCmd(g)
			inner.SetContext(cmd.Context())
			inner.SetOut(cmd.OutOrStdout())
			return inner.RunE(inner, args)
		},
	}

	comment := &cobra.Command{
		Use:   "comment <number>",
		Short: "Comment on a pull request",
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
			x, err := a.Store.ResolvePR(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			c, err := a.Store.AddComment(cmd.Context(), "pull_request", x.ID, b, "")
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(c, func() {
				p.Line("Commented on #%d", x.Number)
			})
		},
	}
	addBodyFlags(comment)

	review := &cobra.Command{
		Use:   "review <number>",
		Short: "Review a pull request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()
			approve, _ := cmd.Flags().GetBool("approve")
			requestChanges, _ := cmd.Flags().GetBool("request-changes")
			commentOnly, _ := cmd.Flags().GetBool("comment")
			state := "comment"
			switch {
			case approve && !requestChanges && !commentOnly:
				state = "approve"
			case requestChanges && !approve && !commentOnly:
				state = "request_changes"
			case commentOnly && !approve && !requestChanges:
				state = "comment"
			case !approve && !requestChanges && !commentOnly:
				return records.Validationf("pass --approve, --request-changes, or --comment")
			default:
				return records.Validationf("pass exactly one of --approve, --request-changes, --comment")
			}
			b, err := body(cmd)
			if err != nil {
				return err
			}
			x, err := a.Store.ResolvePR(ctx, args[0])
			if err != nil {
				return err
			}
			commitSHA := x.HeadCommitSHA
			if sha, err := a.Git.BranchSHA(ctx, x.HeadBranch); err == nil {
				commitSHA = sha
			}
			r, x, err := a.Store.SubmitReview(ctx, args[0], state, b, commitSHA)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(r, func() {
				p.Line("Reviewed #%d (%s)", x.Number, r.State)
			})
		},
	}
	addBodyFlags(review)
	review.Flags().BoolP("approve", "a", false, "approve the pull request")
	review.Flags().BoolP("request-changes", "r", false, "request changes")
	review.Flags().BoolP("comment", "c", false, "comment without approval")

	merge := &cobra.Command{
		Use:   "merge <number>",
		Short: "Merge a pull request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			squash, _ := cmd.Flags().GetBool("squash")
			strategy := "merge"
			if squash {
				strategy = "squash"
			}
			res, err := mergePR(cmd.Context(), a, args[0], strategy, false)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(res, func() {
				p.Line("Merged #%d (%s)", res.Number, short(res.MergeCommitSHA))
			})
		},
	}
	merge.Flags().BoolP("merge", "m", false, "merge commit strategy (default)")
	merge.Flags().BoolP("squash", "s", false, "squash strategy")

	closeCmd := &cobra.Command{
		Use:   "close <number>",
		Short: "Close a pull request",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			x, err := a.Store.ClosePR(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(x, func() {
				p.Line("Closed #%d", x.Number)
			})
		},
	}

	pr.AddCommand(create, list, view, comment, review, merge, closeCmd)
	return pr
}

func ghStateToTaskStatus(state string) (string, error) {
	switch state {
	case "", "open":
		return "", nil // caller filters out closed/done to match gh behavior
	case "closed":
		return "closed", nil
	case "all":
		return "all", nil
	default:
		return "", records.Validationf("invalid state %q (open, closed, all)", state)
	}
}

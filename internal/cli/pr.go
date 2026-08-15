package cli

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
	arksync "github.com/elk-work/ark/internal/sync"
	"github.com/elk-work/ark/pkg/api"
)

func newPRCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Create, review, and merge pull requests",
	}
	cmd.AddCommand(
		newPRCreateCmd(g),
		newPRListCmd(g),
		newPRViewCmd(g),
		newPRCommentCmd(g),
		newPRReviewCmd(g),
		newPRMergeCmd(g),
		newPRCloseCmd(g),
	)
	return cmd
}

func newPRCreateCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a pull request from an existing Git branch",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()

			pr := &store.PullRequest{}
			pr.Title, _ = cmd.Flags().GetString("title")
			if pr.Body, err = body(cmd); err != nil {
				return err
			}
			pr.BaseBranch, _ = cmd.Flags().GetString("base")
			pr.HeadBranch, _ = cmd.Flags().GetString("head")
			if pr.HeadBranch == "" {
				if pr.HeadBranch, err = a.Git.CurrentBranch(ctx); err != nil || pr.HeadBranch == "" {
					return records.Validationf("cannot determine head branch (detached HEAD?); pass --head")
				}
			}
			if pr.BaseBranch == "" {
				pr.BaseBranch = a.Git.DefaultBranch(ctx)
			}

			// Anchor the PR to real Git objects at creation time.
			headSHA, err := a.Git.BranchSHA(ctx, pr.HeadBranch)
			if err != nil {
				return err
			}
			baseSHA, err := a.Git.BranchSHA(ctx, pr.BaseBranch)
			if err != nil {
				return err
			}
			pr.HeadCommitSHA = headSHA
			if mb, err := a.Git.MergeBase(ctx, pr.BaseBranch, pr.HeadBranch); err == nil {
				pr.BaseCommitSHA = mb
			} else {
				pr.BaseCommitSHA = baseSHA
			}

			if taskRef, _ := cmd.Flags().GetString("task"); taskRef != "" {
				t, err := a.Store.ResolveTask(ctx, taskRef)
				if err != nil {
					return err
				}
				pr.TaskID = t.ID
			}

			pr, err = a.Store.CreatePR(ctx, pr)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(pr, func() {
				p.Line("Created pull request #%d: %s (%s)", pr.Number, pr.Title, pr.ID)
				p.Line("  %s <- %s (%s)", pr.BaseBranch, pr.HeadBranch, short(pr.HeadCommitSHA))
			})
		},
	}
	cmd.Flags().StringP("title", "t", "", "pull request title (required)")
	addBodyFlags(cmd)
	cmd.Flags().String("base", "", "base branch (defaults to the repository default branch)")
	cmd.Flags().String("head", "", "head branch (defaults to the current branch)")
	cmd.Flags().String("task", "", "link to task (number or id)")
	cmd.MarkFlagRequired("title")
	return cmd
}

func newPRListCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List pull requests",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			status, _ := cmd.Flags().GetString("status")
			prs, err := a.Store.ListPRs(cmd.Context(), status)
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
			if len(prs) == 0 {
				p.Line("no pull requests")
				return nil
			}
			rows := make([][]string, len(prs))
			for i, pr := range prs {
				rows[i] = []string{fmt.Sprintf("#%d", pr.Number), pr.Status,
					records.Truncate(pr.Title, 50),
					fmt.Sprintf("%s <- %s", pr.BaseBranch, pr.HeadBranch)}
			}
			p.Table([]string{"PR", "STATUS", "TITLE", "BRANCHES"}, rows)
			return nil
		},
	}
	cmd.Flags().StringP("status", "s", "open", "filter by status (open, merged, closed, all)")
	return cmd
}

type prDetail struct {
	*store.PullRequest
	Reviews  []*store.Review  `json:"reviews"`
	Comments []*store.Comment `json:"comments"`
}

func newPRViewCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "view <number|id>",
		Short: "Show a pull request with reviews and comments",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()
			pr, err := a.Store.ResolvePR(ctx, args[0])
			if err != nil {
				return err
			}
			d := prDetail{PullRequest: pr}
			if d.Reviews, err = a.Store.ListReviews(ctx, pr.ID); err != nil {
				return err
			}
			if d.Comments, err = a.Store.ListComments(ctx, "pull_request", pr.ID); err != nil {
				return err
			}
			if d.Reviews == nil {
				d.Reviews = []*store.Review{}
			}
			if d.Comments == nil {
				d.Comments = []*store.Comment{}
			}
			names, err := store.ActorNames(ctx, a.DB)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(d, func() {
				p.Line("pull request #%d  %s", pr.Number, pr.Title)
				p.Line("status    %s", pr.Status)
				p.Line("branches  %s <- %s", pr.BaseBranch, pr.HeadBranch)
				p.Line("head      %s", short(pr.HeadCommitSHA))
				if pr.MergeCommitSHA != "" {
					p.Line("merged    %s at %s", short(pr.MergeCommitSHA), records.FormatTime(pr.MergedAt))
				}
				if pr.Body != "" {
					p.Line("\n%s", pr.Body)
				}
				if len(d.Reviews) > 0 {
					p.Line("\nreviews:")
					for _, r := range d.Reviews {
						p.Line("  [%s] %s (%s) at %s", r.State, actorName(names, r.CreatedBy),
							r.CreatedByType, records.FormatTime(r.CreatedAt))
						if r.Body != "" {
							p.Line("  %s", r.Body)
						}
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

func newPRCommentCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "comment <number|id>",
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
			pr, err := a.Store.ResolvePR(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			c, err := a.Store.AddComment(cmd.Context(), "pull_request", pr.ID, b, "")
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(c, func() {
				p.Line("Commented on pull request #%d (%s)", pr.Number, c.ID)
			})
		},
	}
	addBodyFlags(cmd)
	return cmd
}

func newPRReviewCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review <number|id>",
		Short: "Submit a review (immutable once submitted)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()
			b, err := body(cmd)
			if err != nil {
				return err
			}
			approve, _ := cmd.Flags().GetBool("approve")
			requestChanges, _ := cmd.Flags().GetBool("request-changes")
			state := "comment"
			if approve && requestChanges {
				return records.Validationf("pass --approve or --request-changes, not both")
			}
			if approve {
				state = "approve"
			}
			if requestChanges {
				state = "request_changes"
			}

			// Record which commit the review judged.
			pr, err := a.Store.ResolvePR(ctx, args[0])
			if err != nil {
				return err
			}
			commitSHA := pr.HeadCommitSHA
			if sha, err := a.Git.BranchSHA(ctx, pr.HeadBranch); err == nil {
				commitSHA = sha
			}

			r, pr, err := a.Store.SubmitReview(ctx, args[0], state, b, commitSHA)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(r, func() {
				p.Line("Submitted %s review on pull request #%d (%s)", r.State, pr.Number, r.ID)
			})
		},
	}
	addBodyFlags(cmd)
	cmd.Flags().Bool("approve", false, "approve the change")
	cmd.Flags().Bool("request-changes", false, "request changes")
	return cmd
}

func newPRCloseCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "close <number|id>",
		Short: "Close a pull request without merging",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			pr, err := a.Store.ClosePR(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(pr, func() {
				p.Line("Closed pull request #%d: %s", pr.Number, pr.Title)
			})
		},
	}
}

type mergeResult struct {
	*store.PullRequest
	Strategy string `json:"strategy"`
	Pushed   bool   `json:"pushed"`
}

func newPRMergeCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "merge <number|id>",
		Short: "Merge a pull request with Git and record the result",
		Long: `Merge a pull request.

Verifies the PR is open and its head branch still points at the reviewed
commits, performs the Git merge (merge or squash), pushes when a Git remote
exists, and records the merge. When cloud sync is configured this will also
confirm the merge with the shared service (docs/v1-spec.md §12).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			strategy, _ := cmd.Flags().GetString("strategy")
			noPush, _ := cmd.Flags().GetBool("no-push")
			res, err := mergePR(cmd.Context(), a, args[0], strategy, noPush)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(res, func() {
				p.Line("Merged pull request #%d into %s (%s, %s)", res.Number,
					res.BaseBranch, res.Strategy, short(res.MergeCommitSHA))
				if res.Pushed {
					p.Line("Pushed %s to origin", res.BaseBranch)
				}
			})
		},
	}
	cmd.Flags().String("strategy", "merge", "merge strategy (merge, squash)")
	cmd.Flags().Bool("no-push", false, "skip pushing the result to the Git remote")
	return cmd
}

// mergePR implements the V1-local slice of the merge flow in
// docs/v1-spec.md §12: Git state verification, merge, optional push, and the
// merged record. Cloud confirmation joins in Phase 4.
//
// Ordering matters for recovery: the local Git merge is not recorded (and the
// base branch is reset) unless the push succeeds, so a rejected push leaves
// both Git and the record exactly as they were.
func mergePR(ctx context.Context, a *app.Context, ref, strategy string, noPush bool) (*mergeResult, error) {
	if strategy != "merge" && strategy != "squash" {
		return nil, records.Validationf("strategy must be merge or squash (got %q)", strategy)
	}
	pr, err := a.Store.ResolvePR(ctx, ref)
	if err != nil {
		return nil, err
	}
	if pr.Status != "open" {
		return nil, records.Validationf("pull request #%d is %s", pr.Number, pr.Status)
	}

	// With a remote configured, a merge must be confirmed online (spec §12):
	// push local mutations, reload the PR from the server, and verify it is
	// still open before touching Git.
	var cloudClient *cloud.Client
	if a.Config.Remote != "" {
		cloudClient, err = arksync.Client(a)
		if err != nil {
			return nil, err
		}
		var pushRes arksync.Result
		if err := arksync.Push(ctx, a, cloudClient, &pushRes); err != nil {
			return nil, err
		}
		rec, err := cloudClient.GetRecord(ctx, a.Config.RepositoryID, "pull_request", pr.ID)
		if err != nil && records.ExitCode(err) != 3 {
			return nil, err // unreachable remote blocks the merge (exit 6)
		}
		if rec != nil {
			var remote store.PullRequest
			if err := json.Unmarshal(rec.Data, &remote); err == nil && remote.Status != "open" {
				var syncRes arksync.Result
				arksync.Pull(ctx, a, cloudClient, &syncRes)
				return nil, records.Conflictf("pull request #%d is already %s on the server", pr.Number, remote.Status)
			}
		}
	}

	clean, err := a.Git.IsClean(ctx)
	if err != nil {
		return nil, err
	}
	if !clean {
		return nil, records.Validationf("work tree has uncommitted changes; commit or stash before merging")
	}
	if !a.Git.BranchExists(ctx, pr.HeadBranch) {
		return nil, records.NotFoundf("head branch %q no longer exists", pr.HeadBranch)
	}
	if !a.Git.BranchExists(ctx, pr.BaseBranch) {
		return nil, records.NotFoundf("base branch %q no longer exists", pr.BaseBranch)
	}

	// Refresh refs when a remote exists, so the merge sees current state.
	hasRemote := a.Git.HasRemote(ctx, "origin")
	if hasRemote {
		if err := a.Git.Fetch(ctx, "origin"); err != nil {
			return nil, records.Offlinef("cannot fetch origin before merge: %v", err)
		}
	}

	// Git owns branches: read the head from Git and let the record follow
	// (docs/v1-spec.md §10.5). Reviews record which commit they judged.
	headSHA, err := a.Git.BranchSHA(ctx, pr.HeadBranch)
	if err != nil {
		return nil, err
	}
	if err := a.Store.UpdatePRHead(ctx, pr, headSHA); err != nil {
		return nil, err
	}

	// A head already reachable from base was merged outside Ark (or by a
	// prior partially-recorded attempt); record that instead of failing.
	if a.Git.IsAncestor(ctx, headSHA, "refs/heads/"+pr.BaseBranch) {
		baseSHA, err := a.Git.BranchSHA(ctx, pr.BaseBranch)
		if err != nil {
			return nil, err
		}
		if err := recordMerge(ctx, a, cloudClient, pr, baseSHA, headSHA); err != nil {
			return nil, err
		}
		return &mergeResult{PullRequest: pr, Strategy: "recorded", Pushed: false}, nil
	}

	// Remember where we were so we can return.
	origBranch, err := a.Git.CurrentBranch(ctx)
	if err != nil {
		return nil, err
	}
	if origBranch != pr.BaseBranch {
		if err := a.Git.Checkout(ctx, pr.BaseBranch); err != nil {
			return nil, err
		}
		defer func() {
			if origBranch != "" {
				a.Git.Checkout(ctx, origBranch)
			}
		}()
	}

	// Bring the local base up to date with origin so the merge commit lands
	// on current history and the push can fast-forward.
	if remoteBase := a.Git.RefSHA(ctx, "refs/remotes/origin/"+pr.BaseBranch); hasRemote && remoteBase != "" {
		localBase, err := a.Git.BranchSHA(ctx, pr.BaseBranch)
		if err != nil {
			return nil, err
		}
		if localBase != remoteBase {
			if err := a.Git.FastForward(ctx, remoteBase); err != nil {
				return nil, records.Conflictf(
					"local %s has diverged from origin/%s; reconcile them (e.g. git pull --rebase) before merging",
					pr.BaseBranch, pr.BaseBranch)
			}
		}
	}
	preMergeSHA, err := a.Git.BranchSHA(ctx, pr.BaseBranch)
	if err != nil {
		return nil, err
	}

	message := fmt.Sprintf("Merge pull request #%d: %s", pr.Number, pr.Title)
	var mergeErr error
	if strategy == "squash" {
		mergeErr = a.Git.SquashMerge(ctx, pr.HeadBranch, message)
	} else {
		mergeErr = a.Git.Merge(ctx, pr.HeadBranch, message)
	}
	if mergeErr != nil {
		a.Git.AbortMerge(ctx)
		return nil, records.Conflictf(
			"Git merge failed (%v); resolve manually (git merge %s while on %s, fix conflicts, commit), then re-run `ark pr merge %d` to record it",
			mergeErr, pr.HeadBranch, pr.BaseBranch, pr.Number)
	}
	mergeSHA, err := a.Git.Head(ctx)
	if err != nil {
		return nil, err
	}

	pushed := false
	if hasRemote && !noPush {
		if err := a.Git.Push(ctx, "origin", pr.BaseBranch); err != nil {
			// Nothing has been recorded yet; undo the local merge so the
			// repository and the PR record still agree.
			if resetErr := a.Git.ResetHard(ctx, preMergeSHA); resetErr != nil {
				return nil, records.Partialf(
					"push to origin failed (%v) and resetting %s to %s also failed (%v); reset it manually, then re-run the merge",
					err, pr.BaseBranch, short(preMergeSHA), resetErr)
			}
			return nil, records.Offlinef("push to origin was rejected (%v); the local merge was undone — fetch, reconcile, and retry", err)
		}
		pushed = true
	}

	if err := recordMerge(ctx, a, cloudClient, pr, mergeSHA, headSHA); err != nil {
		// Git already has the merge (and possibly origin does); re-running
		// `ark pr merge` takes the already-merged path above and records it.
		return nil, records.Partialf("Git merge %s succeeded but recording it failed: %v; re-run `ark pr merge %d` to repair the record", short(mergeSHA), err, pr.Number)
	}
	return &mergeResult{PullRequest: pr, Strategy: strategy, Pushed: pushed}, nil
}

// recordMerge marks the PR merged: on the server first when a remote is
// configured (spec §12 step 12), then locally. If the Git work succeeded but
// the server call fails, the local record plus its queued mutation is the
// repair record — the next sync reconciles.
func recordMerge(ctx context.Context, a *app.Context, cloudClient *cloud.Client, pr *store.PullRequest, mergeSHA, headSHA string) error {
	if cloudClient != nil {
		resp, err := cloudClient.Merge(ctx, pr.ID, api.MergeRequest{
			RepositoryID:   a.Config.RepositoryID,
			HeadCommitSHA:  headSHA,
			MergeCommitSHA: mergeSHA,
			MergedBy:       a.Store.Actor.ID,
		})
		if err == nil {
			if applyErr := a.Store.ApplyServerRecord(ctx, resp.Record); applyErr == nil {
				pr.Status = "merged"
				pr.MergeCommitSHA = mergeSHA
				return nil
			}
			// Server confirmed; local write failed. Fall through to the
			// local mark so the mutation log carries the repair.
		} else if records.ExitCode(err) == 3 {
			// Server has never seen this PR (created offline and push was
			// skipped); the local mark queues it for the next sync.
		} else if records.ExitCode(err) != 6 {
			return err // e.g. merged by someone else: surface the conflict
		}
		// Offline (exit 6) also falls through: Git merge already happened,
		// so record locally and repair via the mutation log on next sync.
	}
	return a.Store.MarkPRMerged(ctx, pr, mergeSHA, headSHA)
}

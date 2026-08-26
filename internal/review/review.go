// Package review assembles a read-only view of what happened in a
// repository: agent runs, and everything the records already hang off them —
// the task, the thread, the pull request and its reviews, the artifacts, and
// the diff between the two commits the run recorded.
//
// It is a query and a renderer. It adds no record type, no storage, and no
// server surface, and it introduces no workspace, project, session, or
// milestone: a "session" here is a scope over runs, not a thing to keep.
// v1-spec §2 excludes those four primitives and principle 005 guards the
// rest. If something here starts wanting to be stored, that is the signal to
// stop rather than to add a table.
package review

import (
	"context"
	"sort"
	"time"

	"github.com/elk-work/ark/internal/git"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

// Scope kinds.
const (
	ScopeSinceLastReview = "since_last_review"
	ScopeSince           = "since"
	ScopeRun             = "run"
)

// Scope says which runs the review covers. Whatever the scope, runs that
// have not finished are always included: they are the ones a person most
// needs to see, and excluding them would make the liveness axis pointless.
type Scope struct {
	Kind  string `json:"kind"`
	Since string `json:"since,omitempty"` // RFC3339; runs finished after this
	RunID string `json:"run_id,omitempty"`
}

// Repository identifies what was reviewed.
type Repository struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
}

// Review is the whole document. Its JSON is a stable interface, like every
// other --json output in Ark: treat a field rename as a breaking change.
type Review struct {
	Repository  Repository `json:"repository"`
	GeneratedAt string     `json:"generated_at"`
	Scope       Scope      `json:"scope"`
	Totals      Totals     `json:"totals"`
	Runs        []*Run     `json:"runs"`
}

// Totals summarize the scope at a glance.
type Totals struct {
	Runs         int `json:"runs"`
	Waiting      int `json:"waiting"`
	Errored      int `json:"errored"`
	Working      int `json:"working"`
	Idle         int `json:"idle"`
	Settled      int `json:"settled"`
	Faulted      int `json:"faulted"`
	Unanswered   int `json:"unanswered"`
	Unclear      int `json:"unclear"`
	Insertions   int `json:"insertions"`
	Deletions    int `json:"deletions"`
	FilesTouched int `json:"files_touched"`
}

// Run is one agent run with everything hanging off it.
//
// Two independent axes describe it, because one status cannot answer both
// questions a person has. Liveness is about the run: is anything happening?
// Outcome is about the work: did it land? A run can be idle and settled, or
// idle and unanswered, and those are opposite situations.
type Run struct {
	*store.Run
	AgentDisplay string `json:"agent_display,omitempty"`

	Liveness string `json:"liveness"` // working|waiting|errored|idle
	Outcome  string `json:"outcome"`  // settled|faulted|unanswered|unclear
	// NeedBecause is set on every run that is not settled, and says in one
	// sentence what would move it.
	NeedBecause string `json:"need_because,omitempty"`

	Task        *store.Task       `json:"task,omitempty"`
	Thread      *Thread           `json:"thread,omitempty"`
	PullRequest *PullRequest      `json:"pull_request,omitempty"`
	Artifacts   []*store.Artifact `json:"artifacts"`
	Comments    []*store.Comment  `json:"comments"`
	Diff        *Diff             `json:"diff,omitempty"`
}

// Thread is the conversation around the run.
type Thread struct {
	*store.Thread
	Messages []*store.Message `json:"messages"`
}

// PullRequest is the proposed change and the judgments on it.
type PullRequest struct {
	*store.PullRequest
	Reviews []*store.Review `json:"reviews"`
}

// Options controls what a Collect covers.
type Options struct {
	Scope Scope
	// Now is the clock, injectable so the liveness axis is testable.
	Now time.Time
	// Git reads the diff. Nil means no diff is available, which the page
	// reports rather than hides.
	Git *git.Repo
	// Branch is the repository's current branch, for the header.
	Branch string
	// Name is the repository's display name.
	Name string
}

// staleAfter is how long a running run may go without evidence of activity
// before it reads as idle rather than working. Ark records no heartbeat, so
// this is inferred from what the run has produced.
const staleAfter = 15 * time.Minute

// Collect assembles the review. It only reads.
func Collect(ctx context.Context, s *store.Store, opt Options) (*Review, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	runs, err := selectRuns(ctx, s, opt.Scope)
	if err != nil {
		return nil, err
	}

	names, err := store.ActorNames(ctx, s.DB)
	if err != nil {
		return nil, err
	}
	prs, err := s.ListPRs(ctx, "")
	if err != nil {
		return nil, err
	}

	rev := &Review{
		Repository:  Repository{ID: s.RepoID, Name: opt.Name, Branch: opt.Branch},
		GeneratedAt: records.Now(),
		Scope:       opt.Scope,
		Runs:        []*Run{},
	}

	for _, r := range runs {
		item := &Run{Run: r, AgentDisplay: names[r.CreatedBy],
			Artifacts: []*store.Artifact{}, Comments: []*store.Comment{}}
		if item.AgentDisplay == "" {
			item.AgentDisplay = r.AgentName
		}
		if r.TaskID != "" {
			if t, err := s.ResolveTask(ctx, r.TaskID); err == nil {
				item.Task = t
			}
		}
		if r.ThreadID != "" {
			if th, err := s.ResolveThread(ctx, r.ThreadID); err == nil {
				msgs, err := s.ListMessages(ctx, th.ID)
				if err != nil {
					return nil, err
				}
				if msgs == nil {
					msgs = []*store.Message{}
				}
				item.Thread = &Thread{Thread: th, Messages: msgs}
			}
		}
		if pr := matchPR(prs, r); pr != nil {
			reviews, err := s.ListReviews(ctx, pr.ID)
			if err != nil {
				return nil, err
			}
			if reviews == nil {
				reviews = []*store.Review{}
			}
			item.PullRequest = &PullRequest{PullRequest: pr, Reviews: reviews}
		}
		if arts, err := s.ListArtifacts(ctx, "agent_run", r.ID); err == nil && arts != nil {
			item.Artifacts = arts
		}
		if cs, err := s.ListComments(ctx, "agent_run", r.ID); err == nil && cs != nil {
			item.Comments = cs
		}
		item.Diff = collectDiff(ctx, opt.Git, r.BaseCommitSHA, r.ResultCommitSHA)

		classify(item, opt.Now)
		rev.Runs = append(rev.Runs, item)
	}

	sortRuns(rev.Runs)
	rev.Totals = totals(rev.Runs)
	return rev, nil
}

// selectRuns resolves the scope to a set of runs.
func selectRuns(ctx context.Context, s *store.Store, sc Scope) ([]*store.Run, error) {
	if sc.Kind == ScopeRun {
		r, err := s.ResolveRun(ctx, sc.RunID)
		if err != nil {
			return nil, err
		}
		return []*store.Run{r}, nil
	}
	all, err := s.ListRuns(ctx, "")
	if err != nil {
		return nil, err
	}
	var out []*store.Run
	for _, r := range all {
		if !finished(r) {
			// Always in scope: an unfinished run is the thing most likely
			// to need a person, and it has no finish time to filter on.
			out = append(out, r)
			continue
		}
		if sc.Since == "" || records.TimeAfter(r.FinishedAt, sc.Since) {
			out = append(out, r)
		}
	}
	return out, nil
}

// matchPR finds the pull request a run produced. Ark does not link the two
// directly — a PR belongs to a task — so the run's own branch is the second
// and more specific key.
func matchPR(prs []*store.PullRequest, r *store.Run) *store.PullRequest {
	var byTask *store.PullRequest
	for _, pr := range prs {
		if r.BranchName != "" && pr.HeadBranch == r.BranchName {
			return pr
		}
		if byTask == nil && r.TaskID != "" && pr.TaskID == r.TaskID {
			byTask = pr
		}
	}
	return byTask
}

func finished(r *store.Run) bool {
	return r.Status != "running" && r.Status != "queued"
}

func sortRuns(runs []*Run) {
	// waiting first, then errored: a person is the only thing that will
	// move a waiting run, while an errored one may still be an agent's job.
	rank := map[string]int{LivenessWaiting: 0, LivenessErrored: 1, LivenessWorking: 2, LivenessIdle: 3}
	sort.SliceStable(runs, func(i, j int) bool {
		if rank[runs[i].Liveness] != rank[runs[j].Liveness] {
			return rank[runs[i].Liveness] < rank[runs[j].Liveness]
		}
		return records.TimeAfter(lastActivity(runs[i]), lastActivity(runs[j]))
	})
}

func totals(runs []*Run) Totals {
	var t Totals
	files := map[string]bool{}
	for _, r := range runs {
		t.Runs++
		switch r.Liveness {
		case LivenessWaiting:
			t.Waiting++
		case LivenessErrored:
			t.Errored++
		case LivenessWorking:
			t.Working++
		case LivenessIdle:
			t.Idle++
		}
		switch r.Outcome {
		case OutcomeSettled:
			t.Settled++
		case OutcomeFaulted:
			t.Faulted++
		case OutcomeUnanswered:
			t.Unanswered++
		case OutcomeUnclear:
			t.Unclear++
		}
		if r.Diff != nil {
			t.Insertions += r.Diff.Insertions
			t.Deletions += r.Diff.Deletions
			for _, f := range r.Diff.Files {
				files[f.Path] = true
			}
		}
	}
	t.FilesTouched = len(files)
	return t
}

package workrecord

import (
	"context"
	"sort"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

// Collect walks a repository's records and returns every work-record event
// they imply, ordered by when the thing happened.
//
// The walk is over current state, not a change feed, and it is deliberately
// stateless: emitting the same event twice is safe because Elk deduplicates
// on the (provider, external_id) unique index over external_refs. That is
// the same index that absorbs GitHub webhook redeliveries, and reusing it
// here means a client needs no cursor of its own, an interrupted run needs
// no repair, and a repository that has never synced can still be backfilled
// from nothing.
//
// Callers that want a window pass Options.Since.
func Collect(ctx context.Context, s *store.Store, r Repo, opt Options) ([]Event, error) {
	actorList, err := s.AllActors(ctx)
	if err != nil {
		return nil, err
	}
	as := NewActors(actorList)

	var out []Event
	tasks, err := s.ListTasks(ctx, "")
	if err != nil {
		return nil, err
	}
	taskByID := make(map[string]*store.Task, len(tasks))
	for _, t := range tasks {
		taskByID[t.ID] = t
		out = append(out, Task(r, as, t)...)
	}

	runs, err := s.ListRuns(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, run := range runs {
		arts, err := s.ListArtifacts(ctx, "agent_run", run.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, Run(r, as, run, arts)...)
	}

	prs, err := s.ListPRs(ctx, "")
	if err != nil {
		return nil, err
	}
	prByID := make(map[string]*store.PullRequest, len(prs))
	for _, pr := range prs {
		prByID[pr.ID] = pr
		out = append(out, PR(r, as, pr)...)
	}
	for _, pr := range prs {
		reviews, err := s.ListReviews(ctx, pr.ID)
		if err != nil {
			return nil, err
		}
		for _, rev := range reviews {
			out = append(out, Review(r, as, rev, pr)...)
		}
	}

	proms, err := s.ListPromotions(ctx, "", false)
	if err != nil {
		return nil, err
	}
	for _, p := range proms {
		out = append(out, Promotion(r, as, p)...)
	}

	threads, err := s.ListThreads(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, t := range threads {
		msgs, err := s.ListMessages(ctx, t.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, Thread(r, as, t, len(msgs))...)
	}

	if opt.IncludeComments {
		// Comments hang off several parent types; walk the parents we know.
		for _, t := range tasks {
			cs, err := s.ListComments(ctx, "task", t.ID)
			if err != nil {
				return nil, err
			}
			for _, c := range cs {
				out = append(out, Comment(r, as, c, t.Title, t.Number)...)
			}
		}
		for _, pr := range prs {
			cs, err := s.ListComments(ctx, "pull_request", pr.ID)
			if err != nil {
				return nil, err
			}
			for _, c := range cs {
				out = append(out, Comment(r, as, c, pr.Title, pr.Number)...)
			}
		}
	}

	// Stable order: by when it happened, then by key so equal timestamps
	// (Ark writes a whole batch in one transaction) stay deterministic.
	sort.SliceStable(out, func(i, j int) bool {
		if c := records.TimeCompare(out[i].OccurredAt, out[j].OccurredAt); c != 0 {
			return c < 0
		}
		return out[i].ExternalID < out[j].ExternalID
	})

	if opt.Since != "" {
		filtered := out[:0:0]
		for _, e := range out {
			if records.TimeAfter(e.OccurredAt, opt.Since) {
				filtered = append(filtered, e)
			}
		}
		out = filtered
	}
	return out, nil
}

// Options tunes a collection.
type Options struct {
	// Since drops events at or before this RFC3339 timestamp.
	Since string
	// IncludeComments turns on comment events. Off by default: Elk gates
	// them behind fencing_config.comments for the same reason — they are
	// the highest-volume, lowest-signal record type.
	IncludeComments bool
}

package review

import (
	"fmt"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
)

// Two independent axes, borrowed from agentglass (SirAllap/agentglass, MIT).
//
// A single status field answers one question and people ask two. "Is anything
// happening?" and "did the work land?" have different answers and different
// remedies: a finished, successful run whose pull request nobody has reviewed
// is quiet *and* stuck, and collapsing that into one word loses whichever
// half you did not pick.
//
// Liveness is about the run. Outcome is about the work. Every run that is
// not settled carries a sentence saying what would move it, because a status
// word tells a person the state and not the next action.

// Liveness values.
const (
	LivenessWorking = "working"
	LivenessWaiting = "waiting"
	LivenessErrored = "errored"
	LivenessIdle    = "idle"
)

// Outcome values.
const (
	OutcomeSettled    = "settled"
	OutcomeFaulted    = "faulted"
	OutcomeUnanswered = "unanswered"
	OutcomeUnclear    = "unclear"
)

// classify fills both axes and the needBecause sentence.
func classify(r *Run, now time.Time) {
	pending, pendingWhy := waitingOnAPerson(r)
	failed, failedWhy := faulted(r)
	done := finished(r.Run)

	switch {
	case !done:
		r.Outcome = OutcomeUnclear
	case failed:
		r.Outcome = OutcomeFaulted
	case pending:
		r.Outcome = OutcomeUnanswered
	case landed(r):
		r.Outcome = OutcomeSettled
	default:
		r.Outcome = OutcomeUnclear
	}

	live, liveWhy := liveness(r, now, done, failed, pending)
	r.Liveness = live

	if r.Outcome == OutcomeSettled {
		r.NeedBecause = ""
		return
	}
	switch {
	case failed && pending:
		// Both are true and both matter: the fault is why it is here, and
		// the pending review is the other thing nobody has looked at.
		r.NeedBecause = failedWhy + ". Also: " + pendingWhy
	case failed:
		r.NeedBecause = failedWhy
	case pending:
		r.NeedBecause = pendingWhy
	case liveWhy != "":
		r.NeedBecause = liveWhy
	default:
		r.NeedBecause = unclearWhy(r)
	}
}

// waitingOnAPerson reports work that no agent can advance.
func waitingOnAPerson(r *Run) (bool, string) {
	if pr := r.PullRequest; pr != nil {
		if state, at := latestReview(pr); state == "request_changes" {
			return true, fmt.Sprintf("pull request #%d has changes requested (%s) — someone has to answer the review",
				pr.Number, records.FormatTime(at))
		}
		if pr.Status == "open" && len(pr.Reviews) == 0 {
			return true, fmt.Sprintf("pull request #%d is open with no review — a person is the only thing that will move it",
				pr.Number)
		}
		if pr.Status == "open" {
			return true, fmt.Sprintf("pull request #%d is reviewed but still open — it needs merging", pr.Number)
		}
	}
	if t := r.Task; t != nil && t.Status == "blocked" {
		return true, fmt.Sprintf("task #%d is blocked: %s", t.Number, records.Truncate(t.Title, 60))
	}
	return false, ""
}

// latestReview returns the state and time of the most recent review.
func latestReview(pr *PullRequest) (string, string) {
	var state, at string
	for _, rv := range pr.Reviews {
		if !records.TimeBefore(rv.CreatedAt, at) {
			state, at = rv.State, rv.CreatedAt
		}
	}
	return state, at
}

// faulted reports a run that ended badly, and says how.
func faulted(r *Run) (bool, string) {
	switch r.Status {
	case "failed":
		if r.ExitCode != nil && *r.ExitCode != 0 {
			return true, fmt.Sprintf("failed with exit code %d%s", *r.ExitCode, summaryTail(r))
		}
		return true, "failed" + summaryTail(r)
	case "cancelled":
		return true, "cancelled before it finished" + summaryTail(r)
	}
	if r.ExitCode != nil && *r.ExitCode != 0 {
		return true, fmt.Sprintf("reported success but exited %d%s", *r.ExitCode, summaryTail(r))
	}
	return false, ""
}

func summaryTail(r *Run) string {
	if s := strings.TrimSpace(r.ResultSummary); s != "" {
		return ": " + records.Truncate(s, 80)
	}
	return ", and recorded no result summary"
}

// landed reports work that visibly reached its destination.
func landed(r *Run) bool {
	if pr := r.PullRequest; pr != nil {
		return pr.Status == "merged"
	}
	if t := r.Task; t != nil {
		return t.Status == "done" || t.Status == "closed"
	}
	// No task and no pull request: the run's own record is all there is, so
	// a result summary plus something it produced has to serve as evidence.
	return strings.TrimSpace(r.ResultSummary) != "" &&
		(r.ResultCommitSHA != "" || len(r.Artifacts) > 0)
}

// liveness answers "is anything happening?", and returns a sentence when the
// answer is one a person should act on.
func liveness(r *Run, now time.Time, done, failed, pending bool) (string, string) {
	if done {
		switch {
		case failed:
			// A run that broke reads as errored even when something also
			// waits on a person: the fault is the first fact about it, and
			// the sentence carries the rest.
			return LivenessErrored, ""
		case pending:
			return LivenessWaiting, ""
		default:
			return LivenessIdle, ""
		}
	}

	// Still running. Ark records no heartbeat, so the only honest evidence
	// is what the run has produced. agentglass's rule applies: having
	// nothing to check is not the same as having checked and found nothing,
	// and the page must say which one it is.
	last, sources := evidenceOfLife(r)
	ran := sinceText(r.StartedAt, now)
	if sources == 0 {
		return LivenessWorking, fmt.Sprintf(
			"running for %s; no evidence source readable (not the same as nothing happening)", ran)
	}
	age := ageOf(last, now)
	if age >= 0 && age < staleAfter {
		return LivenessWorking, ""
	}
	return LivenessIdle, fmt.Sprintf("running for %s with no activity in the last %s",
		ran, humanDuration(age))
}

// evidenceOfLife returns the most recent timestamp anything hanging off the
// run produced, and how many sources were readable at all.
func evidenceOfLife(r *Run) (string, int) {
	last, sources := "", 0
	consider := func(ts string) {
		sources++
		if records.TimeAfter(ts, last) {
			last = ts
		}
	}
	if r.Thread != nil {
		for _, m := range r.Thread.Messages {
			consider(m.CreatedAt)
		}
	}
	for _, a := range r.Artifacts {
		consider(a.CreatedAt)
	}
	for _, c := range r.Comments {
		consider(c.CreatedAt)
	}
	return last, sources
}

// unclearWhy explains a run nothing can be concluded about.
func unclearWhy(r *Run) string {
	if !finished(r.Run) {
		return "still " + r.Status
	}
	var missing []string
	if strings.TrimSpace(r.ResultSummary) == "" {
		missing = append(missing, "no result summary")
	}
	if r.ResultCommitSHA == "" {
		missing = append(missing, "no result commit")
	}
	if len(r.Artifacts) == 0 {
		missing = append(missing, "no artifacts")
	}
	if r.Task == nil && r.PullRequest == nil {
		missing = append(missing, "no task or pull request")
	}
	if len(missing) == 0 {
		if t := r.Task; t != nil {
			return fmt.Sprintf("finished, but task #%d is still %s", t.Number, t.Status)
		}
		return "finished, but nothing records where the work went"
	}
	return "succeeded with " + strings.Join(missing, ", ") + ", so there is nothing to judge it by"
}

// lastActivity is the most recent timestamp the run touched. Used only for
// ordering within a liveness bucket.
func lastActivity(r *Run) string {
	last, _ := evidenceOfLife(r)
	for _, ts := range []string{r.FinishedAt, r.StartedAt, r.CreatedAt} {
		if records.TimeAfter(ts, last) {
			last = ts
		}
	}
	return last
}

// ageOf returns how long ago an RFC3339 timestamp was, or -1 if unreadable.
func ageOf(ts string, now time.Time) time.Duration {
	if ts == "" {
		return -1
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return -1
	}
	return now.Sub(t)
}

func sinceText(ts string, now time.Time) string {
	d := ageOf(ts, now)
	if d < 0 {
		return "an unknown time"
	}
	return humanDuration(d)
}

// humanDuration renders a duration the way a person would say it.
func humanDuration(d time.Duration) string {
	if d < 0 {
		return "an unknown time"
	}
	switch {
	case d < time.Minute:
		return "less than a minute"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	}
}

package review

import (
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/store"
)

var now = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func at(d time.Duration) string { return now.Add(-d).Format(time.RFC3339Nano) }

func exit(n int64) *int64 { return &n }

// run builds a finished, successful run with nothing hanging off it.
func run(mut ...func(*Run)) *Run {
	r := &Run{Run: &store.Run{
		ID:              "01RUN0000000000000000000AA",
		AgentName:       "claude-code",
		Status:          "succeeded",
		StartedAt:       at(30 * time.Minute),
		FinishedAt:      at(20 * time.Minute),
		ResultSummary:   "did the thing",
		BaseCommitSHA:   "aaaa",
		ResultCommitSHA: "bbbb",
	}}
	for _, m := range mut {
		m(r)
	}
	return r
}

func withTask(status string) func(*Run) {
	return func(r *Run) { r.Task = &store.Task{Number: 7, Title: "A task", Status: status} }
}

func withPR(status string, reviews ...*store.Review) func(*Run) {
	return func(r *Run) {
		r.PullRequest = &PullRequest{
			PullRequest: &store.PullRequest{Number: 14, Status: status},
			Reviews:     reviews,
		}
	}
}

func review(state string, ago time.Duration) *store.Review {
	return &store.Review{State: state, CreatedAt: at(ago)}
}

// The two axes are independent, and this is the table that says so: the same
// liveness appears against three different outcomes, and vice versa.
func TestClassify(t *testing.T) {
	cases := []struct {
		name         string
		run          *Run
		wantLiveness string
		wantOutcome  string
		needMentions string // "" means no needBecause at all
		alsoMentions string
	}{
		{
			name:         "merged pull request is settled and quiet",
			run:          run(withTask("done"), withPR("merged", review("approve", time.Hour))),
			wantLiveness: LivenessIdle, wantOutcome: OutcomeSettled,
		},
		{
			name:         "open pull request with no review waits on a person",
			run:          run(withTask("open"), withPR("open")),
			wantLiveness: LivenessWaiting, wantOutcome: OutcomeUnanswered,
			needMentions: "a person is the only thing that will move it",
		},
		{
			name:         "changes requested waits on a person too",
			run:          run(withPR("open", review("approve", 2*time.Hour), review("request_changes", time.Hour))),
			wantLiveness: LivenessWaiting, wantOutcome: OutcomeUnanswered,
			needMentions: "changes requested",
		},
		{
			name:         "an approved but unmerged pull request still needs merging",
			run:          run(withPR("open", review("approve", time.Hour))),
			wantLiveness: LivenessWaiting, wantOutcome: OutcomeUnanswered,
			needMentions: "needs merging",
		},
		{
			name:         "a blocked task waits even with nothing else pending",
			run:          run(withTask("blocked")),
			wantLiveness: LivenessWaiting, wantOutcome: OutcomeUnanswered,
			needMentions: "task #7 is blocked",
		},
		{
			name: "a failed run is errored and faulted",
			run: run(func(r *Run) {
				r.Status = "failed"
				r.ExitCode = exit(2)
			}),
			wantLiveness: LivenessErrored, wantOutcome: OutcomeFaulted,
			needMentions: "exit code 2",
		},
		{
			name:         "success with a non-zero exit is still a fault, and says so",
			run:          run(func(r *Run) { r.ExitCode = exit(1) }),
			wantLiveness: LivenessErrored, wantOutcome: OutcomeFaulted,
			needMentions: "reported success but exited 1",
		},
		{
			name:         "cancelled is a fault",
			run:          run(func(r *Run) { r.Status = "cancelled" }),
			wantLiveness: LivenessErrored, wantOutcome: OutcomeFaulted,
			needMentions: "cancelled",
		},
		{
			name: "nothing to judge it by is unclear, not settled",
			run: run(func(r *Run) {
				r.ResultSummary = ""
				r.ResultCommitSHA = ""
			}),
			wantLiveness: LivenessIdle, wantOutcome: OutcomeUnclear,
			needMentions: "nothing to judge it by",
		},
		{
			name: "a fault outranks a pending review, and the sentence keeps both",
			run: run(withPR("open"), func(r *Run) {
				r.Status = "failed"
				r.ExitCode = exit(3)
			}),
			wantLiveness: LivenessErrored, wantOutcome: OutcomeFaulted,
			needMentions: "exit code 3",
			alsoMentions: "pull request #14",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			classify(c.run, now)
			if c.run.Liveness != c.wantLiveness {
				t.Errorf("liveness %q, want %q", c.run.Liveness, c.wantLiveness)
			}
			if c.run.Outcome != c.wantOutcome {
				t.Errorf("outcome %q, want %q", c.run.Outcome, c.wantOutcome)
			}
			if c.needMentions == "" {
				if c.run.NeedBecause != "" {
					t.Errorf("settled run carries a reason: %q", c.run.NeedBecause)
				}
				return
			}
			if !strings.Contains(c.run.NeedBecause, c.needMentions) {
				t.Errorf("need_because %q, want it to mention %q",
					c.run.NeedBecause, c.needMentions)
			}
			if c.alsoMentions != "" && !strings.Contains(c.run.NeedBecause, c.alsoMentions) {
				t.Errorf("need_because %q dropped %q", c.run.NeedBecause, c.alsoMentions)
			}
		})
	}
}

// A running run has no heartbeat to read, so liveness is inferred from what
// it has produced — and "nothing to check" must not read as "nothing
// happening".
func TestLivenessOfARunningRun(t *testing.T) {
	inflight := func(mut ...func(*Run)) *Run {
		r := run(func(r *Run) {
			r.Status = "running"
			r.FinishedAt = ""
			r.ResultSummary = ""
			r.ResultCommitSHA = ""
		})
		for _, m := range mut {
			m(r)
		}
		return r
	}
	msg := func(ago time.Duration) func(*Run) {
		return func(r *Run) {
			r.Thread = &Thread{Thread: &store.Thread{Title: "t"},
				Messages: []*store.Message{{Role: "agent", Body: "x", CreatedAt: at(ago)}}}
		}
	}

	t.Run("recent activity is working, with nothing to say", func(t *testing.T) {
		r := inflight(msg(2 * time.Minute))
		classify(r, now)
		if r.Liveness != LivenessWorking {
			t.Errorf("liveness %q, want working", r.Liveness)
		}
		if r.Outcome != OutcomeUnclear {
			t.Errorf("outcome %q, want unclear while it is still running", r.Outcome)
		}
	})

	t.Run("stale activity is idle and says how long", func(t *testing.T) {
		r := inflight(msg(3 * time.Hour))
		classify(r, now)
		if r.Liveness != LivenessIdle {
			t.Errorf("liveness %q, want idle", r.Liveness)
		}
		if !strings.Contains(r.NeedBecause, "no activity in the last") {
			t.Errorf("need_because %q does not say how stale it is", r.NeedBecause)
		}
	})

	t.Run("no readable evidence is admitted, not guessed", func(t *testing.T) {
		r := inflight()
		classify(r, now)
		if r.Liveness != LivenessWorking {
			t.Errorf("liveness %q, want working: the record says it is running", r.Liveness)
		}
		if !strings.Contains(r.NeedBecause, "not the same as nothing happening") {
			t.Errorf("need_because %q does not distinguish "+
				"'nothing to check' from 'checked and nothing moved'", r.NeedBecause)
		}
	})
}

// Waiting sorts above errored: a person is the only thing that will move it.
func TestSortRuns(t *testing.T) {
	mk := func(id, liveness string, ago time.Duration) *Run {
		return &Run{Run: &store.Run{ID: id, CreatedAt: at(ago), FinishedAt: at(ago)},
			Liveness: liveness}
	}
	runs := []*Run{
		mk("idle-new", LivenessIdle, time.Minute),
		mk("errored", LivenessErrored, time.Hour),
		mk("working", LivenessWorking, time.Hour),
		mk("waiting", LivenessWaiting, 4*time.Hour),
		mk("idle-old", LivenessIdle, 5*time.Hour),
	}
	sortRuns(runs)
	var got []string
	for _, r := range runs {
		got = append(got, r.ID)
	}
	want := []string{"waiting", "errored", "working", "idle-new", "idle-old"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "less than a minute"},
		{42 * time.Minute, "42m"},
		{90 * time.Minute, "1h 30m"},
		{50 * time.Hour, "2d 2h"},
		{-1, "an unknown time"},
	}
	for _, c := range cases {
		if got := humanDuration(c.d); got != c.want {
			t.Errorf("humanDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

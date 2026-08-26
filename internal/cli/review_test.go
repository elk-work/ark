package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// reviewJSON is the shape `ark review --json` promises. Declared here rather
// than reusing review.Review so a field rename shows up as a test failure —
// --json is a stable interface (CLAUDE.md), and this is what enforces it.
type reviewJSON struct {
	Repository struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Branch string `json:"branch"`
	} `json:"repository"`
	GeneratedAt string `json:"generated_at"`
	Scope       struct {
		Kind  string `json:"kind"`
		Since string `json:"since"`
		RunID string `json:"run_id"`
	} `json:"scope"`
	Totals struct {
		Runs         int `json:"runs"`
		Waiting      int `json:"waiting"`
		Errored      int `json:"errored"`
		Settled      int `json:"settled"`
		Faulted      int `json:"faulted"`
		Insertions   int `json:"insertions"`
		Deletions    int `json:"deletions"`
		FilesTouched int `json:"files_touched"`
	} `json:"totals"`
	Runs []struct {
		ID          string `json:"id"`
		Status      string `json:"status"`
		Liveness    string `json:"liveness"`
		Outcome     string `json:"outcome"`
		NeedBecause string `json:"need_because"`
		Task        *struct {
			Number int64  `json:"number"`
			Status string `json:"status"`
		} `json:"task"`
		PullRequest *struct {
			Number  int64 `json:"number"`
			Reviews []any `json:"reviews"`
		} `json:"pull_request"`
		Artifacts []struct {
			Name string `json:"name"`
		} `json:"artifacts"`
		Diff *struct {
			Unavailable string `json:"unavailable"`
			Insertions  int    `json:"insertions"`
			Deletions   int    `json:"deletions"`
			Files       []struct {
				Path  string `json:"path"`
				Hunks []struct {
					Lines []struct {
						Kind string `json:"kind"`
						Text string `json:"text"`
					} `json:"lines"`
				} `json:"hunks"`
			} `json:"files"`
		} `json:"diff"`
	} `json:"runs"`
}

// reviewRepo builds a repository with one finished run that changed a file,
// and returns the directory and the run's ID.
func reviewRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "task", "create", "-t", "Raise the retry budget")

	var run struct {
		ID string `json:"id"`
	}
	arkJSON(t, dir, &run, "--agent", "claude-code", "run", "start",
		"--task", "1", "-i", "Raise the retry budget from 3 to 5")
	commitFile(t, dir, "README.md", "retries: 5\n", "docs: raise the retry budget")
	ark(t, dir, "--agent", "claude-code", "run", "finish", run.ID,
		"-s", "succeeded", "-r", "Raised it.", "--no-review")
	return dir, run.ID
}

func TestReviewJSON(t *testing.T) {
	dir, runID := reviewRepo(t)

	var rev reviewJSON
	arkJSON(t, dir, &rev, "review", "--no-advance")

	if len(rev.Runs) != 1 {
		t.Fatalf("reviewed %d runs, want 1: %+v", len(rev.Runs), rev.Runs)
	}
	r := rev.Runs[0]
	if r.ID != runID {
		t.Errorf("run id %q, want %q", r.ID, runID)
	}
	if r.Liveness == "" || r.Outcome == "" {
		t.Errorf("both axes must always be set: %+v", r)
	}
	if r.Task == nil || r.Task.Number != 1 {
		t.Errorf("the run's task did not come along: %+v", r.Task)
	}
	if rev.Repository.ID == "" || rev.Repository.Branch != "main" {
		t.Errorf("repository: %+v", rev.Repository)
	}
	if rev.Scope.Kind != "since_last_review" {
		t.Errorf("scope %q, want since_last_review", rev.Scope.Kind)
	}
	if rev.Totals.Runs != 1 {
		t.Errorf("totals: %+v", rev.Totals)
	}

	// The diff came from Git, between the two SHAs the run recorded.
	if r.Diff == nil || r.Diff.Unavailable != "" {
		t.Fatalf("no diff: %+v", r.Diff)
	}
	if r.Diff.Insertions == 0 || len(r.Diff.Files) == 0 {
		t.Fatalf("empty diff: %+v", r.Diff)
	}
	if r.Diff.Files[0].Path != "README.md" {
		t.Errorf("changed file %q, want README.md", r.Diff.Files[0].Path)
	}
	if rev.Totals.Insertions != r.Diff.Insertions || rev.Totals.FilesTouched != 1 {
		t.Errorf("totals do not agree with the run: %+v vs %+v", rev.Totals, r.Diff)
	}
	var sawAdd bool
	for _, h := range r.Diff.Files[0].Hunks {
		for _, l := range h.Lines {
			if l.Kind == "add" && strings.Contains(l.Text, "retries: 5") {
				sawAdd = true
			}
		}
	}
	if !sawAdd {
		t.Errorf("the added line is not in the hunks: %+v", r.Diff.Files[0].Hunks)
	}
}

// The cursor is what makes "since the last review" mean anything, and it is
// a file rather than a record on purpose.
func TestReviewCursor(t *testing.T) {
	dir, _ := reviewRepo(t)
	cursor := filepath.Join(dir, ".ark", "review-cursor")

	if _, err := os.Stat(cursor); !os.IsNotExist(err) {
		t.Fatal("a cursor existed before any review")
	}
	var first reviewJSON
	arkJSON(t, dir, &first, "review", "--no-advance")
	if _, err := os.Stat(cursor); !os.IsNotExist(err) {
		t.Error("--no-advance still moved the cursor")
	}

	arkJSON(t, dir, &first, "review")
	if _, err := os.Stat(cursor); err != nil {
		t.Fatalf("no cursor after a review: %v", err)
	}
	if len(first.Runs) != 1 {
		t.Fatalf("first review covered %d runs, want 1", len(first.Runs))
	}

	var second reviewJSON
	arkJSON(t, dir, &second, "review")
	if len(second.Runs) != 0 {
		t.Errorf("the second review repeated %d runs already reviewed", len(second.Runs))
	}

	// A window of its own ignores the cursor entirely.
	var windowed reviewJSON
	arkJSON(t, dir, &windowed, "review", "--since", "24h")
	if len(windowed.Runs) != 1 {
		t.Errorf("--since covered %d runs, want 1", len(windowed.Runs))
	}
	if windowed.Scope.Kind != "since" || windowed.Scope.Since == "" {
		t.Errorf("scope: %+v", windowed.Scope)
	}
}

// A run that has not finished is in scope whatever the window: it is the one
// most likely to need a person, and it has no finish time to filter on.
func TestReviewAlwaysIncludesRunsInFlight(t *testing.T) {
	dir, _ := reviewRepo(t)
	arkJSON(t, dir, &reviewJSON{}, "review") // advance past the finished run

	ark(t, dir, "--agent", "claude-code", "run", "start", "-i", "Still going")

	var rev reviewJSON
	arkJSON(t, dir, &rev, "review", "--no-advance")
	if len(rev.Runs) != 1 || rev.Runs[0].Status != "running" {
		t.Fatalf("in-flight run not in scope: %+v", rev.Runs)
	}
	if rev.Runs[0].Outcome != "unclear" {
		t.Errorf("outcome %q, want unclear while it is still running", rev.Runs[0].Outcome)
	}
	if rev.Runs[0].NeedBecause == "" {
		t.Error("an unsettled run carries no reason")
	}
}

func TestReviewScopeFlags(t *testing.T) {
	dir, runID := reviewRepo(t)

	var one reviewJSON
	arkJSON(t, dir, &one, "review", "--run", runID)
	if len(one.Runs) != 1 || one.Scope.Kind != "run" || one.Scope.RunID != runID {
		t.Errorf("--run: %+v %+v", one.Scope, one.Runs)
	}

	if _, err := arkErr(t, dir, "review", "--run", runID, "--since", "1h"); records.ExitCode(err) != 2 {
		t.Errorf("--run with --since: exit %d, want 2", records.ExitCode(err))
	}
	if _, err := arkErr(t, dir, "review", "--since", "nonsense"); records.ExitCode(err) != 2 {
		t.Errorf("bad --since: exit %d, want 2", records.ExitCode(err))
	}
	if _, err := arkErr(t, dir, "review", "--run", "01NOSUCHRUN000000000000000"); records.ExitCode(err) != 3 {
		t.Errorf("unknown run: exit %d, want 3", records.ExitCode(err))
	}
}

func TestReviewHTMLSink(t *testing.T) {
	dir, _ := reviewRepo(t)
	out := filepath.Join(t.TempDir(), "review.html")
	ark(t, dir, "review", "--no-advance", "--out", out)

	page, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("no page written: %v", err)
	}
	html := string(page)
	if !strings.HasPrefix(html, "<!DOCTYPE html>") {
		t.Errorf("not a page: %.60q", html)
	}
	for _, want := range []string{"Raise the retry budget", "README.md", "<style>", "--accent"} {
		if !strings.Contains(html, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	// Self-contained: an artifact must not depend on a network.
	if strings.Contains(html, "http://") || strings.Contains(html, "https://") {
		t.Error("the page reaches outside itself")
	}

	// With no sink, the page is the output.
	stdout := ark(t, dir, "review", "--no-advance", "--html")
	if !strings.HasPrefix(stdout, "<!DOCTYPE html>") {
		t.Errorf("--html alone did not write the page to stdout: %.60q", stdout)
	}
}

func TestReviewArtifactSink(t *testing.T) {
	dir, runID := reviewRepo(t)
	ark(t, dir, "review", "--no-advance", "--run", runID, "--artifact")

	var arts []struct {
		Name       string `json:"name"`
		ParentType string `json:"parent_type"`
		ParentID   string `json:"parent_id"`
		MediaType  string `json:"media_type"`
	}
	arkJSON(t, dir, &arts, "artifact", "list", "--parent", "run:"+runID)
	if len(arts) != 1 || arts[0].Name != "review.html" || arts[0].ParentID != runID {
		t.Fatalf("artifact not attached to the run: %+v", arts)
	}

	// A multi-run review has no single run to attach to, and says so rather
	// than picking one.
	ark(t, dir, "--agent", "claude-code", "run", "start", "-i", "Another")
	_, err := arkErr(t, dir, "review", "--no-advance", "--artifact")
	if records.ExitCode(err) != 2 {
		t.Errorf("--artifact over several runs: exit %d, want 2", records.ExitCode(err))
	}
}

// Saving the review as part of the saved session.
func TestRunFinishAttachesAReview(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var run struct {
		ID string `json:"id"`
	}
	arkJSON(t, dir, &run, "--agent", "claude-code", "run", "start", "-i", "Do a thing")
	ark(t, dir, "--agent", "claude-code", "run", "finish", run.ID, "-s", "succeeded", "-r", "Done.")

	var arts []struct {
		Name string `json:"name"`
	}
	arkJSON(t, dir, &arts, "artifact", "list", "--parent", "run:"+run.ID)
	if len(arts) != 1 || arts[0].Name != "review.html" {
		t.Fatalf("run finish attached %+v, want one review.html", arts)
	}

	// And the opt-out really opts out.
	var quiet struct {
		ID string `json:"id"`
	}
	arkJSON(t, dir, &quiet, "--agent", "claude-code", "run", "start", "-i", "Quietly")
	ark(t, dir, "--agent", "claude-code", "run", "finish", quiet.ID, "-s", "succeeded", "--no-review")
	arkJSON(t, dir, &arts, "artifact", "list", "--parent", "run:"+quiet.ID)
	if len(arts) != 0 {
		t.Errorf("--no-review still attached %+v", arts)
	}
}

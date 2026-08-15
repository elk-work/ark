package cli

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/servertest"
)

// startSyncServer boots the real server over temp-dir SQLite repository
// databases and local blob storage, returning its URL.
func startSyncServer(t *testing.T) string {
	t.Helper()
	s := servertest.NewServer(t)
	blobs := s.Blobs.(*server.LocalBlobStore)
	ts := httptest.NewServer(s.Handler())
	blobs.BaseURL = ts.URL
	t.Cleanup(ts.Close)
	t.Setenv("ARK_TOKEN", servertest.Token)
	return ts.URL
}

func repoIDOf(t *testing.T, dir string) string {
	t.Helper()
	var status struct {
		RepositoryID string `json:"repository_id"`
	}
	arkJSON(t, dir, &status, "status")
	return status.RepositoryID
}

// TestTwoClientSync is the final piece of the spec §23.4 end-to-end path:
// a second client pulls the same records and the full history is visible.
func TestTwoClientSync(t *testing.T) {
	url := startSyncServer(t)

	// Client A: full local workflow, then sync.
	a := gitRepo(t)
	ark(t, a, "init")
	ark(t, a, "remote", "set", url)
	var task struct {
		ID string `json:"id"`
	}
	arkJSON(t, a, &task, "task", "create", "-t", "Shared task", "-b", "with details")
	ark(t, a, "task", "comment", "1", "-b", "first comment")
	var thread struct {
		ID string `json:"id"`
	}
	arkJSON(t, a, &thread, "thread", "create", "--task", "1", "-t", "Discussion")
	ark(t, a, "--agent", "claude-code", "thread", "message", thread.ID, "-r", "agent", "-b", "working on it")
	var run struct {
		ID string `json:"id"`
	}
	arkJSON(t, a, &run, "--agent", "claude-code", "run", "start", "--task", "1", "-i", "do the work")
	ark(t, a, "--agent", "claude-code", "run", "finish", run.ID, "-s", "succeeded", "-r", "done")

	evidence := filepath.Join(a, "evidence.txt")
	os.WriteFile(evidence, []byte("proof\n"), 0o644)
	var artifact struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
	}
	arkJSON(t, a, &artifact, "artifact", "add", evidence, "--parent", "run:"+run.ID)
	os.Remove(evidence)

	gitOut(t, a, "checkout", "-b", "feature")
	commitFile(t, a, "f.txt", "f\n", "feature work")
	gitOut(t, a, "checkout", "main")
	ark(t, a, "pr", "create", "-t", "Feature PR", "--head", "feature", "--base", "main")
	ark(t, a, "pr", "review", "1", "--approve", "-b", "ship it")

	out := ark(t, a, "sync")
	if !strings.Contains(out, "pushed") || !strings.Contains(out, "uploaded  1 artifact") {
		t.Fatalf("first sync: %s", out)
	}

	// Cloud-confirmed merge: remote is configured, so the server owns the
	// open->merged transition.
	ark(t, a, "pr", "merge", "1")
	ark(t, a, "sync")

	var statusA struct {
		PendingMutations int64 `json:"pending_mutations"`
	}
	arkJSON(t, a, &statusA, "status")
	if statusA.PendingMutations != 0 {
		t.Errorf("client A pending mutations = %d, want 0", statusA.PendingMutations)
	}

	// Client B: clone the Git repo, join the Ark repository, sync.
	repoID := repoIDOf(t, a)
	b := filepath.Join(t.TempDir(), "clone")
	cmd := exec.Command("git", "clone", a, b)
	if outp, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, outp)
	}
	gitOut(t, b, "config", "user.name", "Second Human")
	gitOut(t, b, "config", "user.email", "second@example.com")
	ark(t, b, "init", "--repository", repoID)
	ark(t, b, "remote", "set", url)
	out = ark(t, b, "sync")
	if !strings.Contains(out, "pulled") {
		t.Fatalf("client B sync: %s", out)
	}

	// The full history is visible on B (spec §26).
	var tasks []struct {
		Number int64  `json:"number"`
		Title  string `json:"title"`
		Status string `json:"status"`
	}
	arkJSON(t, b, &tasks, "task", "list", "-s", "all")
	if len(tasks) != 1 || tasks[0].Title != "Shared task" {
		t.Fatalf("client B tasks: %+v", tasks)
	}
	var detail struct {
		Comments []struct{ Body string }   `json:"comments"`
		Threads  []struct{ Title string }  `json:"threads"`
		Runs     []struct{ Status string } `json:"runs"`
	}
	arkJSON(t, b, &detail, "task", "view", "1")
	if len(detail.Comments) != 1 || len(detail.Threads) != 1 || len(detail.Runs) != 1 {
		t.Fatalf("client B task detail: %+v", detail)
	}
	var prs []struct {
		Status string `json:"status"`
	}
	arkJSON(t, b, &prs, "pr", "list", "-s", "all")
	if len(prs) != 1 || prs[0].Status != "merged" {
		t.Fatalf("client B PRs: %+v", prs)
	}
	var prDetail struct {
		Reviews []struct{ State string } `json:"reviews"`
	}
	arkJSON(t, b, &prDetail, "pr", "view", "1")
	if len(prDetail.Reviews) != 1 || prDetail.Reviews[0].State != "approve" {
		t.Fatalf("client B reviews: %+v", prDetail)
	}

	// Artifact blob downloads on demand from the server's object store.
	dest := filepath.Join(t.TempDir(), "evidence.txt")
	ark(t, b, "artifact", "get", artifact.ID, "-o", dest)
	got, _ := os.ReadFile(dest)
	if string(got) != "proof\n" {
		t.Errorf("artifact content on B: %q", got)
	}

	// B comments; A sees it after the next sync round-trip.
	ark(t, b, "task", "comment", "1", "-b", "hello from B")
	ark(t, b, "sync")
	ark(t, a, "sync")
	var detailA struct {
		Comments []struct{ Body string } `json:"comments"`
	}
	arkJSON(t, a, &detailA, "task", "view", "1")
	if len(detailA.Comments) != 2 {
		t.Fatalf("client A comments after round trip: %+v", detailA.Comments)
	}
}

func TestSyncConflictFlow(t *testing.T) {
	url := startSyncServer(t)

	a := gitRepo(t)
	ark(t, a, "init")
	ark(t, a, "remote", "set", url)
	ark(t, a, "task", "create", "-t", "Contested", "-b", "original")
	ark(t, a, "sync")

	repoID := repoIDOf(t, a)
	b := filepath.Join(t.TempDir(), "clone")
	exec.Command("git", "clone", a, b).Run()
	gitOut(t, b, "config", "user.name", "B")
	gitOut(t, b, "config", "user.email", "b@example.com")
	ark(t, b, "init", "--repository", repoID)
	ark(t, b, "remote", "set", url)
	ark(t, b, "sync")

	// Both edit the title offline. A syncs first and wins; B conflicts.
	ark(t, a, "task", "edit", "1", "-t", "Title from A")
	ark(t, b, "task", "edit", "1", "-t", "Title from B")
	ark(t, a, "sync")
	out := ark(t, b, "sync")
	if !strings.Contains(out, "conflict: task") {
		t.Fatalf("B sync should conflict: %s", out)
	}

	// B's local record now shows the server truth; the conflict holds B's edit.
	var taskB struct {
		Title string `json:"title"`
	}
	arkJSON(t, b, &taskB, "task", "view", "1")
	if taskB.Title != "Title from A" {
		t.Errorf("B title after conflict = %q, want server's", taskB.Title)
	}
	var conflicts []struct {
		ID string `json:"id"`
	}
	arkJSON(t, b, &conflicts, "conflict", "list")
	if len(conflicts) != 1 {
		t.Fatalf("B conflicts: %+v", conflicts)
	}

	// Keeping B's version requeues it against the current revision.
	ark(t, b, "conflict", "resolve", conflicts[0].ID, "--keep", "local")
	ark(t, b, "sync")
	ark(t, a, "sync")
	var taskA struct {
		Title string `json:"title"`
	}
	arkJSON(t, a, &taskA, "task", "view", "1")
	if taskA.Title != "Title from B" {
		t.Errorf("A title after resolution = %q, want B's", taskA.Title)
	}

	// Concurrent unrelated edits merge without conflict.
	ark(t, a, "task", "edit", "1", "-s", "in_progress")
	ark(t, b, "task", "edit", "1", "-b", "new body from B")
	ark(t, a, "sync")
	out = ark(t, b, "sync")
	if strings.Contains(out, "conflict: task") {
		t.Fatalf("unrelated edits conflicted: %s", out)
	}
	ark(t, a, "sync")
	var merged struct {
		Status string `json:"status"`
		Body   string `json:"body"`
	}
	arkJSON(t, a, &merged, "task", "view", "1")
	if merged.Status != "in_progress" || merged.Body != "new body from B" {
		t.Errorf("field merge result: %+v", merged)
	}
}

// TestOfflineNumberReconciliation: both clients create task #2 offline; the
// server renumbers the later one and both converge.
func TestOfflineNumberReconciliation(t *testing.T) {
	url := startSyncServer(t)

	a := gitRepo(t)
	ark(t, a, "init")
	ark(t, a, "remote", "set", url)
	ark(t, a, "task", "create", "-t", "Base")
	ark(t, a, "sync")

	repoID := repoIDOf(t, a)
	b := filepath.Join(t.TempDir(), "clone")
	exec.Command("git", "clone", a, b).Run()
	gitOut(t, b, "config", "user.name", "B")
	gitOut(t, b, "config", "user.email", "b@example.com")
	ark(t, b, "init", "--repository", repoID)
	ark(t, b, "remote", "set", url)
	ark(t, b, "sync")

	ark(t, a, "task", "create", "-t", "From A") // #2 on A
	ark(t, b, "task", "create", "-t", "From B") // #2 on B too
	ark(t, a, "sync")
	ark(t, b, "sync")
	ark(t, a, "sync") // A learns about B's renumbered task

	for _, dir := range []string{a, b} {
		var tasks []struct {
			Number int64  `json:"number"`
			Title  string `json:"title"`
		}
		arkJSON(t, dir, &tasks, "task", "list", "-s", "all")
		if len(tasks) != 3 {
			t.Fatalf("tasks in %s: %+v", dir, tasks)
		}
		seen := map[int64]string{}
		for _, task := range tasks {
			if prev, dup := seen[task.Number]; dup {
				t.Errorf("duplicate number %d (%s and %s) in %s", task.Number, prev, task.Title, dir)
			}
			seen[task.Number] = task.Title
		}
	}
}

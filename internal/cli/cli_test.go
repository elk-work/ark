package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elkproject/ark/internal/records"
)

// gitRepo creates a Git repository with one commit on main.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	git("config", "user.name", "Test Human")
	git("config", "user.email", "t@example.com")
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644)
	git("add", ".")
	git("commit", "-m", "init")
	return dir
}

// ark runs the CLI with args against dir and returns stdout.
func ark(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := arkErr(t, dir, args...)
	if err != nil {
		t.Fatalf("ark %v: %v\n%s", args, err, out)
	}
	return out
}

// arkErr runs the CLI and returns output and error without failing the test.
func arkErr(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	root := New("test")
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(append([]string{"-C", dir}, args...))
	err := root.Execute()
	return buf.String(), err
}

// arkJSON runs the CLI with --json and decodes the result into v.
func arkJSON(t *testing.T, dir string, v any, args ...string) {
	t.Helper()
	out := ark(t, dir, append([]string{"--json"}, args...)...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		t.Fatalf("bad JSON from ark %v: %v\n%s", args, err, out)
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "-m", msg)
}

// TestEndToEnd walks the required V1 path from docs/v1-spec.md §23.4
// (excluding the cloud sync steps, which are Phase 4).
func TestEndToEnd(t *testing.T) {
	dir := gitRepo(t)

	// 1. initialize repository
	out := ark(t, dir, "init")
	if !strings.Contains(out, "Initialized Ark") {
		t.Fatalf("init output: %s", out)
	}
	// init is idempotent-hostile by design: a second init fails cleanly.
	if _, err := arkErr(t, dir, "init"); err == nil {
		t.Fatal("second init should fail")
	}
	// Ark deliberately writes no repository-level .ark/ rule — .ark/.gitignore
	// contains `*`, so Ark state ignores itself on every branch. A repo-level
	// rule added nothing and, left untracked, read as an un-ignored database.
	if gi, err := os.ReadFile(filepath.Join(dir, ".ark", ".gitignore")); err != nil {
		t.Errorf("read .ark/.gitignore: %v", err)
	} else if strings.TrimSpace(string(gi)) != "*" {
		t.Errorf(".ark/.gitignore = %q, want %q", gi, "*")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("init should leave no repository-level .gitignore, got err = %v", err)
	}

	// 2. create task
	var task struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
	}
	arkJSON(t, dir, &task, "task", "create", "-t", "Build the feature", "-b", "Full details")
	if task.Number != 1 {
		t.Fatalf("task number = %d", task.Number)
	}

	// 3. create thread
	var thread struct {
		ID string `json:"id"`
	}
	arkJSON(t, dir, &thread, "thread", "create", "--task", "1", "-t", "Feature discussion")
	ark(t, dir, "thread", "message", thread.ID, "-r", "user", "-b", "please build it")
	ark(t, dir, "--agent", "claude-code", "thread", "message", thread.ID, "-r", "agent", "-b", "on it")

	// 4. start agent run
	var run struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	arkJSON(t, dir, &run, "--agent", "claude-code", "run", "start",
		"--task", "1", "--thread", thread.ID, "-i", "build the feature")
	if run.Status != "running" {
		t.Fatalf("run status = %s", run.Status)
	}

	// 5-6. create branch, commit change
	gitOut(t, dir, "checkout", "-b", "feature")
	commitFile(t, dir, "feature.txt", "built\n", "Build the feature")

	// 7. finish run
	ark(t, dir, "--agent", "claude-code", "run", "finish", run.ID, "-s", "succeeded", "-r", "feature built")

	// attach an artifact produced by the run
	evidence := filepath.Join(dir, "bench.txt")
	os.WriteFile(evidence, []byte("fast\n"), 0o644)
	gitOut(t, dir, "checkout", "main") // keep the work tree story simple
	var artifact struct {
		ID     string `json:"id"`
		SHA256 string `json:"sha256"`
	}
	arkJSON(t, dir, &artifact, "artifact", "add", evidence, "--parent", "run:"+run.ID)
	os.Remove(evidence)

	// 8. create PR
	var pr struct {
		ID     string `json:"id"`
		Number int64  `json:"number"`
		Status string `json:"status"`
	}
	arkJSON(t, dir, &pr, "pr", "create", "-t", "Feature", "-b", "Adds the feature",
		"--task", "1", "--head", "feature", "--base", "main")
	if pr.Number != 1 || pr.Status != "open" {
		t.Fatalf("pr: %+v", pr)
	}

	// 9. review PR (agent reviews are first-class)
	ark(t, dir, "--agent", "claude-code", "pr", "review", "1", "--approve", "-b", "verified")

	// 10. merge PR
	out = ark(t, dir, "pr", "merge", "1")
	if !strings.Contains(out, "Merged pull request #1") {
		t.Fatalf("merge output: %s", out)
	}
	if !strings.Contains(gitOut(t, dir, "log", "--oneline", "main"), "Merge pull request #1") {
		t.Error("merge commit missing from Git history")
	}

	// 12. verify full history is visible
	var detail struct {
		Status   string `json:"status"`
		Comments []any  `json:"comments"`
		Threads  []any  `json:"threads"`
		Runs     []any  `json:"runs"`
	}
	arkJSON(t, dir, &detail, "task", "view", "1")
	if len(detail.Threads) != 1 || len(detail.Runs) != 1 {
		t.Errorf("task detail: %+v", detail)
	}
	var prDetail struct {
		Status  string `json:"status"`
		Reviews []any  `json:"reviews"`
	}
	arkJSON(t, dir, &prDetail, "pr", "view", "1")
	if prDetail.Status != "merged" || len(prDetail.Reviews) != 1 {
		t.Errorf("pr detail: %+v", prDetail)
	}

	// close the loop on the task
	ark(t, dir, "task", "close", "1")

	// search sees the whole story
	var hits []map[string]any
	arkJSON(t, dir, &hits, "search", "feature")
	if len(hits) < 2 {
		t.Errorf("search hits = %d, want >= 2", len(hits))
	}

	// artifact round trip
	dest := filepath.Join(t.TempDir(), "bench.txt")
	ark(t, dir, "artifact", "get", artifact.ID, "-o", dest)
	got, _ := os.ReadFile(dest)
	if string(got) != "fast\n" {
		t.Errorf("artifact content: %q", got)
	}

	// status reflects the finished state
	var status struct {
		OpenTasks        int64 `json:"open_tasks"`
		OpenPRs          int64 `json:"open_pull_requests"`
		PendingMutations int64 `json:"pending_mutations"`
	}
	arkJSON(t, dir, &status, "status")
	if status.OpenTasks != 0 || status.OpenPRs != 0 {
		t.Errorf("status: %+v", status)
	}
	if status.PendingMutations == 0 {
		t.Error("expected pending mutations recorded for future sync")
	}
}

func TestGHCompatibility(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	// gh issue -> task
	out := ark(t, dir, "gh", "issue", "create", "--title", "Compat issue", "--body", "via gh")
	if !strings.Contains(out, "#1") {
		t.Fatalf("gh issue create: %s", out)
	}
	ark(t, dir, "gh", "issue", "comment", "1", "-b", "a comment")
	out = ark(t, dir, "gh", "issue", "list")
	if !strings.Contains(out, "Compat issue") {
		t.Fatalf("gh issue list: %s", out)
	}
	ark(t, dir, "gh", "issue", "close", "1")
	out = ark(t, dir, "gh", "issue", "list")
	if strings.Contains(out, "Compat issue") {
		t.Errorf("closed issue still listed: %s", out)
	}

	// gh pr -> pull request, squash merge
	gitOut(t, dir, "checkout", "-b", "change")
	commitFile(t, dir, "x.txt", "x\n", "change")
	gitOut(t, dir, "checkout", "main")
	ark(t, dir, "gh", "pr", "create", "--title", "Compat PR", "-B", "main", "-H", "change")
	ark(t, dir, "gh", "pr", "review", "1", "--approve", "-b", "ship it")
	out = ark(t, dir, "gh", "pr", "merge", "1", "--squash")
	if !strings.Contains(out, "Merged #1") {
		t.Fatalf("gh pr merge: %s", out)
	}
}

func TestExitCodes(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	// not found -> 3
	_, err := arkErr(t, dir, "task", "view", "99")
	if records.ExitCode(err) != 3 {
		t.Errorf("missing task: exit %d, want 3", records.ExitCode(err))
	}
	// validation -> 2
	_, err = arkErr(t, dir, "task", "create", "-t", "")
	if records.ExitCode(err) != 2 {
		t.Errorf("empty title: exit %d, want 2", records.ExitCode(err))
	}
	// offline sync -> 6
	_, err = arkErr(t, dir, "sync")
	if records.ExitCode(err) != 6 {
		t.Errorf("sync without remote: exit %d, want 6", records.ExitCode(err))
	}
	// no ark dir -> 3
	_, err = arkErr(t, t.TempDir(), "task", "list")
	if records.ExitCode(err) != 3 {
		t.Errorf("no .ark: exit %d, want 3", records.ExitCode(err))
	}
}

func TestMergeGuards(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	gitOut(t, dir, "checkout", "-b", "guard")
	commitFile(t, dir, "g.txt", "g\n", "guard work")
	gitOut(t, dir, "checkout", "main")
	ark(t, dir, "pr", "create", "-t", "Guarded", "--head", "guard", "--base", "main")

	// Dirty work tree (modified tracked file) -> validation (exit 2).
	// Untracked files deliberately do not block merges.
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("edited\n"), 0o644)
	_, err := arkErr(t, dir, "pr", "merge", "1")
	if records.ExitCode(err) != 2 {
		t.Errorf("dirty tree: exit %d, want 2 (%v)", records.ExitCode(err), err)
	}
	gitOut(t, dir, "checkout", "--", "README.md")

	// A head that moved after PR creation merges fine: Git owns branches
	// and the record follows (spec §10.5).
	gitOut(t, dir, "checkout", "guard")
	commitFile(t, dir, "g2.txt", "g2\n", "more work")
	newHead := gitOut(t, dir, "rev-parse", "HEAD")
	gitOut(t, dir, "checkout", "main")
	var merged struct {
		Status        string `json:"status"`
		HeadCommitSHA string `json:"head_commit_sha"`
	}
	arkJSON(t, dir, &merged, "pr", "merge", "1")
	if merged.Status != "merged" || merged.HeadCommitSHA != newHead {
		t.Errorf("moved-head merge: %+v (want head %s)", merged, newHead)
	}

	// Closed PR cannot merge.
	gitOut(t, dir, "checkout", "-b", "guard2")
	commitFile(t, dir, "g3.txt", "g3\n", "guard2 work")
	gitOut(t, dir, "checkout", "main")
	ark(t, dir, "pr", "create", "-t", "Guarded 2", "--head", "guard2", "--base", "main")
	ark(t, dir, "pr", "close", "2")
	_, err = arkErr(t, dir, "pr", "merge", "2")
	if err == nil {
		t.Error("merge of closed PR should fail")
	}
}

// conflictingPR sets up main and a feature branch that both edit the same
// line, plus an open PR for the feature branch.
func conflictingPR(t *testing.T, strategy string) (string, error) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	gitOut(t, dir, "checkout", "-b", "feature")
	commitFile(t, dir, "shared.txt", "feature version\n", "feature edit")
	gitOut(t, dir, "checkout", "main")
	commitFile(t, dir, "shared.txt", "main version\n", "main edit")
	ark(t, dir, "pr", "create", "-t", "Conflicting", "--head", "feature", "--base", "main")
	_, err := arkErr(t, dir, "pr", "merge", "1", "--strategy", strategy)
	return dir, err
}

func TestMergeConflictLeavesTreeClean(t *testing.T) {
	for _, strategy := range []string{"merge", "squash"} {
		t.Run(strategy, func(t *testing.T) {
			dir, err := conflictingPR(t, strategy)
			if records.ExitCode(err) != 4 {
				t.Fatalf("conflicting merge: exit %d, want 4 (%v)", records.ExitCode(err), err)
			}
			// The failed merge must not leave conflict markers or strand
			// the user on the base branch.
			if status := gitOut(t, dir, "status", "--porcelain"); status != "" {
				t.Errorf("work tree dirty after aborted %s merge:\n%s", strategy, status)
			}
			if branch := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); branch != "main" {
				t.Errorf("stranded on %q", branch)
			}
			var prs []struct {
				Status string `json:"status"`
			}
			arkJSON(t, dir, &prs, "pr", "list", "-s", "all")
			if len(prs) != 1 || prs[0].Status != "open" {
				t.Errorf("PR after failed merge: %+v", prs)
			}
		})
	}
}

func TestMergeWithRemote(t *testing.T) {
	// origin is a bare repo; a second clone advances origin/main so the
	// merging clone's local main is stale.
	origin := t.TempDir()
	gitOut(t, origin, "init", "--bare", "-b", "main")

	dir := gitRepo(t)
	gitOut(t, dir, "remote", "add", "origin", origin)
	// init before the first push: `ark init` commits the agent skill, so it
	// belongs in the initial push like any other commit. Pushing first and
	// initializing after would leave local main ahead of origin before the
	// scenario even starts.
	ark(t, dir, "init")
	gitOut(t, dir, "push", "-u", "origin", "main")

	other := t.TempDir()
	gitOut(t, filepath.Dir(other), "clone", origin, other)
	gitOut(t, other, "config", "user.name", "Other")
	gitOut(t, other, "config", "user.email", "o@example.com")
	commitFile(t, other, "upstream.txt", "u\n", "upstream work")
	gitOut(t, other, "push", "origin", "main")

	// Local feature branch, PR, merge: local main is behind origin/main and
	// must fast-forward before the merge so the push lands.
	gitOut(t, dir, "checkout", "-b", "feature")
	commitFile(t, dir, "f.txt", "f\n", "feature work")
	gitOut(t, dir, "checkout", "main")
	ark(t, dir, "pr", "create", "-t", "Remote merge", "--head", "feature", "--base", "main")
	var res struct {
		Status         string `json:"status"`
		Pushed         bool   `json:"pushed"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	arkJSON(t, dir, &res, "pr", "merge", "1")
	if res.Status != "merged" || !res.Pushed {
		t.Fatalf("remote merge: %+v", res)
	}
	// The recorded merge commit is on origin, and includes the upstream work.
	if originMain := gitOut(t, origin, "rev-parse", "main"); originMain != res.MergeCommitSHA {
		t.Errorf("origin/main = %s, recorded merge = %s", originMain, res.MergeCommitSHA)
	}
	gitOut(t, dir, "log", "--oneline", "main") // sanity: no error
	if out := gitOut(t, dir, "log", "--oneline", "main"); !strings.Contains(out, "upstream work") {
		t.Errorf("upstream commit missing from merged history:\n%s", out)
	}

	// Diverged local base -> conflict, nothing recorded.
	gitOut(t, other, "pull", "--ff-only", "origin", "main") // catch up with the merge
	commitFile(t, other, "upstream2.txt", "u2\n", "more upstream")
	gitOut(t, other, "push", "origin", "main")
	gitOut(t, dir, "commit", "--allow-empty", "-m", "local divergence")
	gitOut(t, dir, "checkout", "-b", "feature2")
	commitFile(t, dir, "f2.txt", "f2\n", "feature2 work")
	gitOut(t, dir, "checkout", "main")
	ark(t, dir, "pr", "create", "-t", "Diverged", "--head", "feature2", "--base", "main")
	_, err := arkErr(t, dir, "pr", "merge", "2")
	if records.ExitCode(err) != 4 {
		t.Errorf("diverged base: exit %d, want 4 (%v)", records.ExitCode(err), err)
	}
	var prs []struct {
		Number int64  `json:"number"`
		Status string `json:"status"`
	}
	arkJSON(t, dir, &prs, "pr", "list", "-s", "open")
	if len(prs) != 1 || prs[0].Number != 2 {
		t.Errorf("PR 2 should stay open: %+v", prs)
	}
}

func TestManualResolutionThenRecord(t *testing.T) {
	dir, err := conflictingPR(t, "merge")
	if err == nil {
		t.Fatal("expected conflict")
	}
	// Follow the error's advice: merge manually, resolve, commit...
	cmd := exec.Command("git", "merge", "feature")
	cmd.Dir = dir
	cmd.CombinedOutput() // conflicts, as expected
	os.WriteFile(filepath.Join(dir, "shared.txt"), []byte("resolved\n"), 0o644)
	gitOut(t, dir, "add", "shared.txt")
	gitOut(t, dir, "commit", "-m", "Merge feature (resolved)")

	// ...then re-run the merge to record it.
	var res struct {
		Status   string `json:"status"`
		Strategy string `json:"strategy"`
	}
	arkJSON(t, dir, &res, "pr", "merge", "1")
	if res.Status != "merged" || res.Strategy != "recorded" {
		t.Errorf("manual resolution: %+v", res)
	}
}

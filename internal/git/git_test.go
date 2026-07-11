package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// newTestRepo creates a Git repository with one commit on main.
func newTestRepo(t *testing.T) *Repo {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	run("config", "user.name", "Test")
	run("config", "user.email", "t@example.com")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644)
	run("add", ".")
	run("commit", "-m", "init")
	return &Repo{Dir: dir}
}

func TestTopLevel(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	sub := filepath.Join(r.Dir, "nested", "deep")
	os.MkdirAll(sub, 0o755)
	top, err := TopLevel(ctx, sub)
	if err != nil {
		t.Fatalf("toplevel: %v", err)
	}
	// Resolve symlinks (macOS /tmp) before comparing.
	want, _ := filepath.EvalSymlinks(r.Dir)
	got, _ := filepath.EvalSymlinks(top)
	if got != want {
		t.Errorf("toplevel = %q, want %q", got, want)
	}
	if _, err := TopLevel(ctx, t.TempDir()); err == nil {
		t.Error("toplevel outside a repo should fail")
	}
}

func TestBranchesAndMerge(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	head, err := r.Head(ctx)
	if err != nil || len(head) != 40 {
		t.Fatalf("head: %q %v", head, err)
	}
	branch, _ := r.CurrentBranch(ctx)
	if branch != "main" {
		t.Fatalf("branch = %q", branch)
	}
	if r.DefaultBranch(ctx) != "main" {
		t.Errorf("default branch = %q", r.DefaultBranch(ctx))
	}

	// Feature branch with one commit.
	if _, err := r.Run(ctx, "checkout", "-b", "feature"); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(r.Dir, "b.txt"), []byte("b\n"), 0o644)
	r.Run(ctx, "add", ".")
	if _, err := r.Run(ctx, "commit", "-m", "feature work"); err != nil {
		t.Fatal(err)
	}
	featureSHA, _ := r.BranchSHA(ctx, "feature")
	if !r.BranchExists(ctx, "feature") || featureSHA == head {
		t.Fatal("feature branch not created properly")
	}

	mb, err := r.MergeBase(ctx, "main", "feature")
	if err != nil || mb != head {
		t.Errorf("merge base = %q, want %q", mb, head)
	}
	if r.IsAncestor(ctx, featureSHA, "refs/heads/main") {
		t.Error("feature should not be merged yet")
	}

	clean, err := r.IsClean(ctx)
	if err != nil || !clean {
		t.Errorf("work tree should be clean")
	}

	// Merge --no-ff back into main.
	if err := r.Checkout(ctx, "main"); err != nil {
		t.Fatal(err)
	}
	if err := r.Merge(ctx, "feature", "Merge feature"); err != nil {
		t.Fatalf("merge: %v", err)
	}
	if !r.IsAncestor(ctx, featureSHA, "refs/heads/main") {
		t.Error("feature should be merged now")
	}
	mergeSHA, _ := r.Head(ctx)
	if mergeSHA == featureSHA {
		t.Error("expected a merge commit, got fast-forward")
	}
}

func TestSquashMerge(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()

	r.Run(ctx, "checkout", "-b", "feature")
	for _, name := range []string{"c1.txt", "c2.txt"} {
		os.WriteFile(filepath.Join(r.Dir, name), []byte(name), 0o644)
		r.Run(ctx, "add", ".")
		r.Run(ctx, "commit", "-m", "add "+name)
	}
	r.Checkout(ctx, "main")
	if err := r.SquashMerge(ctx, "feature", "Squash feature"); err != nil {
		t.Fatalf("squash: %v", err)
	}
	res, _ := r.Run(ctx, "log", "--oneline")
	// init + one squash commit
	if lines := countLines(res.Stdout); lines != 2 {
		t.Errorf("log lines = %d, want 2:\n%s", lines, res.Stdout)
	}
}

func TestRemoteURLMissing(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	if url := r.RemoteURL(ctx, "origin"); url != "" {
		t.Errorf("unexpected remote %q", url)
	}
	if r.HasRemote(ctx, "origin") {
		t.Error("HasRemote should be false")
	}
}

func countLines(s string) int {
	n := 0
	for _, c := range s {
		if c == '\n' {
			n++
		}
	}
	return n
}

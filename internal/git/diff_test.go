package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// commit writes files and records them, returning the resulting SHA.
func commit(t *testing.T, r *Repo, message string, files map[string]string, removed ...string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(r.Dir, name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range removed {
		os.Remove(filepath.Join(r.Dir, name))
	}
	git(t, r, "add", "-A")
	git(t, r, "commit", "-m", message)
	return strings.TrimSpace(git(t, r, "rev-parse", "HEAD"))
}

func git(t *testing.T, r *Repo, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestHasCommit(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	head := r.RefSHA(ctx, "HEAD")

	if !r.HasCommit(ctx, head) {
		t.Error("HEAD is not present?")
	}
	// "Not here" must be distinguishable from "nothing changed", so a
	// well-formed but absent SHA has to answer false rather than error.
	if r.HasCommit(ctx, "0123456789012345678901234567890123456789") {
		t.Error("an absent commit reported as present")
	}
	if r.HasCommit(ctx, "") {
		t.Error("the empty SHA reported as present")
	}
	// A tree is not a commit.
	tree := strings.TrimSpace(git(t, r, "rev-parse", "HEAD^{tree}"))
	if r.HasCommit(ctx, tree) {
		t.Error("a tree object reported as a commit")
	}
}

func TestDiffStat(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	base := r.RefSHA(ctx, "HEAD")

	head := commit(t, r, "change several things", map[string]string{
		"a.txt":           "a\nb\nc\n",
		"added.txt":       "new\n",
		"dir/spaced name": "x\n",
		"bin.dat":         "\x00\x01\x02binary\x00",
	})

	stats, err := r.DiffStat(ctx, base, head)
	if err != nil {
		t.Fatalf("diff stat: %v", err)
	}
	byPath := map[string]FileStat{}
	for _, s := range stats {
		byPath[s.Path] = s
	}
	if len(byPath) != 4 {
		t.Fatalf("stats for %d files, want 4: %+v", len(byPath), stats)
	}
	if got := byPath["a.txt"]; got.Insertions != 2 || got.Deletions != 0 {
		t.Errorf("a.txt: +%d -%d, want +2 -0", got.Insertions, got.Deletions)
	}
	// A path with a space is exactly what -z is for.
	if _, ok := byPath["dir/spaced name"]; !ok {
		t.Errorf("a path with a space was lost: %+v", stats)
	}
	if !byPath["bin.dat"].Binary {
		t.Errorf("bin.dat not reported as binary: %+v", byPath["bin.dat"])
	}
}

func TestDiffStatDetectsRenames(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	body := strings.Repeat("a line that is long enough to match on\n", 20)
	base := commit(t, r, "add a file worth renaming", map[string]string{"old.txt": body})
	head := commit(t, r, "rename it", map[string]string{"new.txt": body}, "old.txt")

	stats, err := r.DiffStat(ctx, base, head)
	if err != nil {
		t.Fatalf("diff stat: %v", err)
	}
	if len(stats) != 1 {
		t.Fatalf("stats: %+v, want one rename", stats)
	}
	if stats[0].Path != "new.txt" || stats[0].OldPath != "old.txt" {
		t.Errorf("rename read as %q -> %q, want old.txt -> new.txt",
			stats[0].OldPath, stats[0].Path)
	}
}

func TestDiffUnifiedAndCommitsBetween(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	base := r.RefSHA(ctx, "HEAD")
	commit(t, r, "first step", map[string]string{"a.txt": "a\nb\n"})
	head := commit(t, r, "second step", map[string]string{"a.txt": "a\nb\nc\n"})

	patch, err := r.DiffUnified(ctx, base, head, 3)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	for _, want := range []string{"diff --git", "@@", "+b", "+c"} {
		if !strings.Contains(patch, want) {
			t.Errorf("patch is missing %q:\n%s", want, patch)
		}
	}

	commits, err := r.CommitsBetween(ctx, base, head)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(commits) != 2 {
		t.Fatalf("listed %d commits, want 2: %+v", len(commits), commits)
	}
	// git log is newest first.
	if commits[0].Subject != "second step" || commits[1].Subject != "first step" {
		t.Errorf("subjects: %q then %q", commits[0].Subject, commits[1].Subject)
	}
	if commits[0].Author != "Test" || commits[0].Date == "" || len(commits[0].SHA) != 40 {
		t.Errorf("commit metadata: %+v", commits[0])
	}
}

// A subject containing the field separator or a newline must not corrupt the
// parse — the delimiter is chosen so it cannot appear in the payload.
func TestCommitsBetweenSurvivesAwkwardSubjects(t *testing.T) {
	r := newTestRepo(t)
	ctx := context.Background()
	base := r.RefSHA(ctx, "HEAD")
	commit(t, r, "fix: handle a | pipe, a \"quote\" and a\ttab", map[string]string{"a.txt": "a\nb\n"})
	head := r.RefSHA(ctx, "HEAD")

	commits, err := r.CommitsBetween(ctx, base, head)
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if len(commits) != 1 {
		t.Fatalf("listed %d commits, want 1: %+v", len(commits), commits)
	}
	if !strings.Contains(commits[0].Subject, "a | pipe") ||
		!strings.Contains(commits[0].Subject, "tab") {
		t.Errorf("subject mangled: %q", commits[0].Subject)
	}
}

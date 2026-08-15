// Package git wraps the installed Git CLI. Ark never reimplements Git; it
// shells out with machine-readable flags and an explicit working directory.
package git

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
)

// Repo runs Git commands against one working directory.
type Repo struct {
	Dir string
	// Debug, when set, receives one line per Git invocation.
	Debug func(format string, args ...any)
}

// Result captures one Git invocation.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// Run executes git with args in the repository directory.
func (r *Repo) Run(ctx context.Context, args ...string) (*Result, error) {
	start := time.Now()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	res := &Result{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}
	if r.Debug != nil {
		r.Debug("git %s (exit %d, %s)", strings.Join(args, " "), res.ExitCode, res.Duration.Round(time.Millisecond))
	}
	if err != nil {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = err.Error()
		}
		return res, records.GitErr(fmt.Sprintf("git %s: %s", strings.Join(args, " "), msg), nil)
	}
	return res, nil
}

// out runs git and returns trimmed stdout.
func (r *Repo) out(ctx context.Context, args ...string) (string, error) {
	res, err := r.Run(ctx, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}

// TopLevel returns the repository root, or a typed error when dir is not
// inside a Git work tree.
func TopLevel(ctx context.Context, dir string) (string, error) {
	r := &Repo{Dir: dir}
	top, err := r.out(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", records.NotFoundf("not inside a Git repository (Ark lives beside Git; run from a Git work tree)")
	}
	return top, nil
}

// Head returns the current HEAD commit SHA.
func (r *Repo) Head(ctx context.Context) (string, error) {
	return r.out(ctx, "rev-parse", "HEAD")
}

// CurrentBranch returns the checked-out branch name, or "" when detached.
func (r *Repo) CurrentBranch(ctx context.Context) (string, error) {
	out, err := r.out(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if out == "HEAD" {
		return "", nil
	}
	return out, nil
}

// BranchSHA resolves a branch name to a commit SHA.
func (r *Repo) BranchSHA(ctx context.Context, branch string) (string, error) {
	out, err := r.out(ctx, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		return "", records.NotFoundf("branch %q not found", branch)
	}
	return out, nil
}

// BranchExists reports whether a local branch exists.
func (r *Repo) BranchExists(ctx context.Context, branch string) bool {
	_, err := r.BranchSHA(ctx, branch)
	return err == nil
}

// RemoteURL returns the URL for the named remote, or "" if unset.
func (r *Repo) RemoteURL(ctx context.Context, remote string) string {
	out, err := r.out(ctx, "remote", "get-url", remote)
	if err != nil {
		return ""
	}
	return out
}

// DefaultBranch guesses the default branch: origin/HEAD if set, otherwise
// main or master if they exist, otherwise the current branch.
func (r *Repo) DefaultBranch(ctx context.Context) string {
	if out, err := r.out(ctx, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimPrefix(out, "origin/")
	}
	for _, b := range []string{"main", "master"} {
		if r.BranchExists(ctx, b) {
			return b
		}
	}
	if cur, err := r.CurrentBranch(ctx); err == nil && cur != "" {
		return cur
	}
	return "main"
}

// MergeBase returns the merge base of two revisions.
func (r *Repo) MergeBase(ctx context.Context, a, b string) (string, error) {
	return r.out(ctx, "merge-base", a, b)
}

// IsAncestor reports whether a is an ancestor of b.
func (r *Repo) IsAncestor(ctx context.Context, a, b string) bool {
	res, _ := r.Run(ctx, "merge-base", "--is-ancestor", a, b)
	return res != nil && res.ExitCode == 0
}

// IsClean reports whether the work tree has no uncommitted changes to
// tracked files. Untracked files do not block a merge; Git itself refuses
// if one would be overwritten.
func (r *Repo) IsClean(ctx context.Context) (bool, error) {
	out, err := r.out(ctx, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return out == "", nil
}

// Checkout switches branches.
func (r *Repo) Checkout(ctx context.Context, branch string) error {
	_, err := r.Run(ctx, "checkout", branch)
	return err
}

// Add stages paths. Used for files Ark itself writes into the working tree:
// a file Ark created but left untracked reaches no other clone, which for the
// agent skill means the guidance silently does not exist for anyone else.
func (r *Repo) Add(ctx context.Context, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	_, err := r.Run(ctx, append([]string{"add", "--"}, paths...)...)
	return err
}

// IsTracked reports whether path is known to the index.
func (r *Repo) IsTracked(ctx context.Context, path string) bool {
	res, err := r.Run(ctx, "ls-files", "--error-unmatch", "--", path)
	return err == nil && res.ExitCode == 0
}

// CommitPaths commits only the named paths, leaving the rest of the index
// untouched. The pathspec matters: Ark commits files it wrote itself, and must
// never sweep up whatever else the working tree happened to have staged.
func (r *Repo) CommitPaths(ctx context.Context, message string, paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"commit", "-m", message, "--"}, paths...)
	_, err := r.Run(ctx, args...)
	return err
}

// Fetch fetches from the named remote. A missing remote is not an error for
// local-only repositories.
func (r *Repo) Fetch(ctx context.Context, remote string) error {
	_, err := r.Run(ctx, "fetch", remote, "--prune")
	return err
}

// HasRemote reports whether the repository has the named remote configured.
func (r *Repo) HasRemote(ctx context.Context, remote string) bool {
	return r.RemoteURL(ctx, remote) != ""
}

// Merge merges head into the current branch with --no-ff, using message.
func (r *Repo) Merge(ctx context.Context, head, message string) error {
	_, err := r.Run(ctx, "merge", "--no-ff", "-m", message, head)
	return err
}

// SquashMerge squashes head onto the current branch and commits with message.
func (r *Repo) SquashMerge(ctx context.Context, head, message string) error {
	if _, err := r.Run(ctx, "merge", "--squash", head); err != nil {
		return err
	}
	_, err := r.Run(ctx, "commit", "-m", message)
	return err
}

// AbortMerge restores the work tree after a failed merge. A conflicted
// `merge --squash` leaves no MERGE_HEAD, so `merge --abort` refuses; `reset
// --merge` is the documented abort for that state.
func (r *Repo) AbortMerge(ctx context.Context) {
	if _, err := r.Run(ctx, "merge", "--abort"); err != nil {
		r.Run(ctx, "reset", "--merge")
	}
}

// ResetHard moves the current branch (and work tree) to rev.
func (r *Repo) ResetHard(ctx context.Context, rev string) error {
	_, err := r.Run(ctx, "reset", "--hard", rev)
	return err
}

// FastForward advances the current branch to rev, refusing non-fast-forward.
func (r *Repo) FastForward(ctx context.Context, rev string) error {
	_, err := r.Run(ctx, "merge", "--ff-only", rev)
	return err
}

// RefSHA resolves any ref (e.g. origin/main) to a commit SHA, "" if absent.
func (r *Repo) RefSHA(ctx context.Context, ref string) string {
	out, err := r.out(ctx, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return out
}

// Push pushes branch to remote with --force-with-lease semantics disabled
// for plain fast-forward pushes.
func (r *Repo) Push(ctx context.Context, remote, branch string) error {
	_, err := r.Run(ctx, "push", remote, branch)
	return err
}

package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/skills"
)

// The skill is the durable answer to Ark's real failure mode — sessions that
// end without recording anything, and repositories that never get a remote.
// It only works if it lands where an agent's harness loads it automatically,
// so the path is a contract, not an implementation detail.
func TestSkillInstallsWhereAgentsLoadIt(t *testing.T) {
	root := t.TempDir()

	wrote, path, err := installSkill(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first install should write the file")
	}
	if want := filepath.Join(root, ".claude", "skills", "ark", "SKILL.md"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want, err := skills.FS.ReadFile(skills.Ark)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Error("installed copy differs from the bundled skill")
	}
}

// Re-running init must not churn the file, or every initialization would show
// up as a spurious diff.
func TestSkillInstallIsIdempotent(t *testing.T) {
	root := t.TempDir()
	if _, _, err := installSkill(root, false); err != nil {
		t.Fatal(err)
	}
	wrote, _, err := installSkill(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("second install rewrote an already-current file")
	}
}

// A project that has adapted the guidance to its own conventions must not have
// that silently reverted by an unrelated `ark init`. --force is the explicit
// way to take the update.
func TestSkillInstallPreservesLocalEditsUnlessForced(t *testing.T) {
	root := t.TempDir()
	if _, path, err := installSkill(root, false); err != nil {
		t.Fatal(err)
	} else if err := os.WriteFile(path, []byte("# our own house rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	wrote, path, err := installSkill(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Error("a locally edited skill was overwritten without --force")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "# our own house rules\n" {
		t.Errorf("local edit was not preserved: %q", got)
	}

	if wrote, _, err = installSkill(root, true); err != nil {
		t.Fatal(err)
	} else if !wrote {
		t.Error("--force should overwrite a local edit")
	}
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "ark run finish") {
		t.Error("--force did not restore the bundled skill")
	}
}

// The skill's whole job is to name the moments an agent must act on. If any of
// these disappears, the guidance has quietly stopped covering the failure it
// exists to prevent.
func TestSkillCoversTheMomentsThatActuallyGetMissed(t *testing.T) {
	b, err := skills.FS.ReadFile(skills.Ark)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)

	for _, want := range []string{
		"ark status",     // is this repo even recording?
		"ark remote set", // the step that never happens
		"ark run start",  // the start of a session
		"ark run finish", // the end of one
		"ark sync",       // durability
		"ARK_AGENT_NAME", // attribution, so delegation resolves
		"--json",         // the machine interface
	} {
		if !strings.Contains(body, want) {
			t.Errorf("skill no longer mentions %q", want)
		}
	}

	// Frontmatter is what makes a harness load it at the right moment.
	if !strings.HasPrefix(body, "---\n") {
		t.Error("skill is missing its frontmatter block")
	}
	for _, want := range []string{"name: ark", "description:"} {
		if !strings.Contains(body, want) {
			t.Errorf("frontmatter is missing %q", want)
		}
	}
}

// Writing the skill is not the guarantee — being *tracked* is. An untracked
// skill exists on one machine, so every other clone, worktree and cloud
// sandbox runs with no guidance while the repository looks fully adopted.
// Two repositories in this project failed exactly this way: `ark init` wrote
// .claude/, nobody committed it, and they carried Ark for weeks with no skill.
func TestSkillIsCommittedSoOtherClonesGetIt(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	runGit("init", "-q", ".")
	runGit("config", "user.email", "t@example.com")
	runGit("config", "user.name", "t")

	wrote, path, err := installSkill(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("first install should write the file")
	}

	if committed := commitSkill(context.Background(), root, path); !committed {
		t.Fatal("commitSkill reported it did not commit a freshly written skill")
	}

	// The contract: it is committed, so a clone of HEAD carries the guidance.
	if out := runGit("ls-files", "--", ".claude/skills/ark/SKILL.md"); !strings.Contains(out, "SKILL.md") {
		t.Errorf("skill is not tracked; ls-files = %q", out)
	}
	if out := runGit("status", "--porcelain"); strings.Contains(out, "SKILL.md") {
		t.Errorf("skill should be committed, not left pending; status = %q", out)
	}

	// Idempotent: already tracked means nothing more to do.
	if committed := commitSkill(context.Background(), root, path); committed {
		t.Error("commitSkill should report no work once the skill is tracked")
	}
}

// A directory that is not a Git repository must not break `ark init`.
func TestSkillTrackingIsBestEffortOutsideGit(t *testing.T) {
	root := t.TempDir()
	_, path, err := installSkill(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if committed := commitSkill(context.Background(), root, path); committed {
		t.Error("tracking cannot succeed outside a Git repository")
	}
}

// Ark commits a file it wrote itself; it must never sweep up whatever else the
// working tree had staged. The explicit pathspec is the guard, so pin it.
func TestSkillCommitDoesNotSweepUpOtherStagedWork(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	runGit("init", "-q", ".")
	runGit("config", "user.email", "t@example.com")
	runGit("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "seed.txt")
	runGit("commit", "-q", "-m", "seed")

	// Someone is mid-change with unrelated work staged.
	if err := os.WriteFile(filepath.Join(root, "wip.txt"), []byte("half done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "wip.txt")

	_, path, err := installSkill(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if !commitSkill(context.Background(), root, path) {
		t.Fatal("commitSkill should have committed the skill")
	}

	// The skill landed...
	if out := runGit("show", "--name-only", "--format=", "HEAD"); !strings.Contains(out, "SKILL.md") {
		t.Errorf("skill not in HEAD commit: %q", out)
	}
	// ...and the unrelated staged file did NOT ride along.
	if out := runGit("show", "--name-only", "--format=", "HEAD"); strings.Contains(out, "wip.txt") {
		t.Error("commit swept up unrelated staged work")
	}
	if out := runGit("status", "--porcelain"); !strings.Contains(out, "wip.txt") {
		t.Errorf("unrelated work should still be staged and uncommitted; status = %q", out)
	}
}

// Ark no longer writes a repository-level .ark/ rule: .ark/.gitignore contains
// `*`, so Ark state is self-ignoring on every branch. The rule was redundant,
// and left untracked it read as an un-ignored database. Pin that .ark/ stays
// invisible to Git with no repository-level rule at all.
func TestArkStateIsSelfIgnoringWithoutARepoGitignore(t *testing.T) {
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	runGit("init", "-q", ".")
	runGit("config", "user.email", "t@example.com")
	runGit("config", "user.name", "t")

	// Simulate what app.Init writes for Ark state.
	if err := os.MkdirAll(filepath.Join(root, ".ark"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".gitignore": "*\n", "ark.db": "not really sqlite"} {
		if err := os.WriteFile(filepath.Join(root, ".ark", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := os.Stat(filepath.Join(root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("test precondition: there must be no repository-level .gitignore")
	}
	if out := runGit("status", "--porcelain"); strings.Contains(out, ".ark") {
		t.Errorf(".ark/ must be invisible to Git without a repo-level rule; status = %q", out)
	}
}

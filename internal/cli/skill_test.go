package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elkproject/ark/skills"
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
func TestSkillIsStagedSoOtherClonesGetIt(t *testing.T) {
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

	if staged := stageSkill(context.Background(), root, path); !staged {
		t.Fatal("stageSkill reported it did not stage a freshly written skill")
	}

	// The contract: git now knows about it, so a commit carries it to clones.
	if out := runGit("diff", "--cached", "--name-only"); !strings.Contains(out, "SKILL.md") {
		t.Errorf("skill is not in the index; staged output = %q", out)
	}

	// Idempotent: already tracked means nothing more to do.
	runGit("commit", "-q", "-m", "add skill")
	if staged := stageSkill(context.Background(), root, path); staged {
		t.Error("stageSkill should report no work once the skill is tracked")
	}
}

// A directory that is not a Git repository must not break `ark init`.
func TestSkillStagingIsBestEffortOutsideGit(t *testing.T) {
	root := t.TempDir()
	_, path, err := installSkill(root, false)
	if err != nil {
		t.Fatal(err)
	}
	if staged := stageSkill(context.Background(), root, path); staged {
		t.Error("staging cannot succeed outside a Git repository")
	}
}

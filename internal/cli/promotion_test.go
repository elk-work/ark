package cli

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

type promotionJSON struct {
	ID             string `json:"id"`
	Environment    string `json:"environment"`
	Service        string `json:"service"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	ArtifactSHA256 string `json:"artifact_sha256"`
	PullRequestID  string `json:"pull_request_id"`
	ActivatedAt    string `json:"activated_at"`
	EndedAt        string `json:"ended_at"`
}

func TestPromotionCLI(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	head := gitOut(t, dir, "rev-parse", "HEAD")

	// create defaults --commit to the current HEAD SHA.
	var p1 promotionJSON
	arkJSON(t, dir, &p1, "promotion", "create", "--env", "production")
	if p1.Environment != "production" || p1.MergeCommitSHA != head || p1.ActivatedAt == "" {
		t.Fatalf("promotion: %+v", p1)
	}

	// An explicit artifact promotion in the same environment supersedes p1.
	var p2 promotionJSON
	arkJSON(t, dir, &p2, "promotion", "create", "--env", "production",
		"--artifact-sha256", "deadbeef")
	if p2.MergeCommitSHA != "" || p2.ArtifactSHA256 != "deadbeef" {
		t.Fatalf("artifact promotion: %+v", p2)
	}

	var all []promotionJSON
	arkJSON(t, dir, &all, "promotion", "list")
	if len(all) != 2 || all[0].ID != p1.ID || all[0].EndedAt != p2.ActivatedAt {
		t.Fatalf("list: %+v", all)
	}
	var active []promotionJSON
	arkJSON(t, dir, &active, "promotion", "list", "--env", "production", "--active")
	if len(active) != 1 || active[0].ID != p2.ID {
		t.Fatalf("active list: %+v", active)
	}

	// end closes the active promotion.
	var ended promotionJSON
	arkJSON(t, dir, &ended, "promotion", "end", p2.ID)
	if ended.EndedAt == "" {
		t.Fatalf("ended: %+v", ended)
	}
	arkJSON(t, dir, &active, "promotion", "list", "--active")
	if len(active) != 0 {
		t.Fatalf("active after end: %+v", active)
	}
}

func TestPromotionExitCodes(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	// validation -> 2 (missing environment)
	if _, err := arkErr(t, dir, "promotion", "create"); records.ExitCode(err) != 2 {
		t.Errorf("missing env: exit %d, want 2", records.ExitCode(err))
	}
	// not found -> 3
	if _, err := arkErr(t, dir, "promotion", "end", "01ZZZZZZZZ"); records.ExitCode(err) != 3 {
		t.Errorf("unknown promotion: exit %d, want 3", records.ExitCode(err))
	}
}

// TestPromotionSync pushes a promotion create and its ended_at update through
// the sync engine and pulls them into a second store.
func TestPromotionSync(t *testing.T) {
	url := startSyncServer(t)

	a := gitRepo(t)
	ark(t, a, "init")
	ark(t, a, "remote", "set", url)
	gitOut(t, a, "checkout", "-b", "release")
	commitFile(t, a, "r.txt", "r\n", "release work")
	gitOut(t, a, "checkout", "main")
	head := gitOut(t, a, "rev-parse", "HEAD")

	var pr struct {
		ID string `json:"id"`
	}
	arkJSON(t, a, &pr, "pr", "create", "-t", "Ship it", "--head", "release", "--base", "main")
	var p1 promotionJSON
	arkJSON(t, a, &p1, "promotion", "create", "--env", "production", "--pr", "1")
	if p1.PullRequestID != pr.ID {
		t.Fatalf("pr link: %+v", p1)
	}
	ark(t, a, "sync")

	// A second promotion supersedes the first after it is already synced, so
	// the ended_at update travels as its own mutation.
	var p2 promotionJSON
	arkJSON(t, a, &p2, "promotion", "create", "--env", "production", "--artifact-sha256", "deadbeef")
	ark(t, a, "sync")

	// Client B joins and pulls; both promotions materialize.
	repoID := repoIDOf(t, a)
	b := filepath.Join(t.TempDir(), "clone")
	if out, err := exec.Command("git", "clone", a, b).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v\n%s", err, out)
	}
	gitOut(t, b, "config", "user.name", "B")
	gitOut(t, b, "config", "user.email", "b@example.com")
	ark(t, b, "init", "--repository", repoID)
	ark(t, b, "remote", "set", url)
	ark(t, b, "sync")

	var proms []promotionJSON
	arkJSON(t, b, &proms, "promotion", "list", "--env", "production")
	if len(proms) != 2 {
		t.Fatalf("promotions on B: %+v", proms)
	}
	if proms[0].ID != p1.ID || proms[0].MergeCommitSHA != head ||
		proms[0].PullRequestID != pr.ID || proms[0].EndedAt != p2.ActivatedAt {
		t.Errorf("superseded promotion on B: %+v", proms[0])
	}
	if proms[1].ID != p2.ID || proms[1].ArtifactSHA256 != "deadbeef" || proms[1].EndedAt != "" {
		t.Errorf("active promotion on B: %+v", proms[1])
	}

	// B ends the active promotion; A sees it after a sync round trip.
	ark(t, b, "promotion", "end", p2.ID)
	ark(t, b, "sync")
	ark(t, a, "sync")
	var activeA []promotionJSON
	arkJSON(t, a, &activeA, "promotion", "list", "--active")
	if len(activeA) != 0 {
		t.Errorf("A still sees active promotions: %+v", activeA)
	}
}

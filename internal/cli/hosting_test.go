package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/servertest"
)

// isolatedGit runs git with no system or global configuration and a HOME of
// its own, so a credential sent over HTTP is never offered to — or stored by
// — the machine's credential helper.
func isolatedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+global, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// `ark repo git-url` prints the service's clone URL for the bound
// repository, and that URL is one Git can push to and clone from with the
// same credential the CLI syncs with (elk-work/ark#125).
func TestRepoGitURLIsWhereGitPushesAndClones(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
	dir, url := syncedRepo(t)

	var out struct {
		RepositoryID string `json:"repository_id"`
		GitURL       string `json:"git_url"`
	}
	arkJSON(t, dir, &out, "repo", "git-url")
	if want := url + "/git/" + repoIDOf(t, dir); out.GitURL != want {
		t.Fatalf("git-url = %q, want %q", out.GitURL, want)
	}

	auth := "http.extraHeader=Authorization: Bearer " + servertest.Token
	isolatedGit(t, dir, "-c", auth, "push", "--quiet", out.GitURL, "main")
	parent := t.TempDir()
	isolatedGit(t, parent, "-c", auth, "clone", "--quiet", out.GitURL, "clone")
	if got, want := isolatedGit(t, filepath.Join(parent, "clone"), "rev-parse", "HEAD"),
		gitOut(t, dir, "rev-parse", "HEAD"); got != want {
		t.Errorf("clone HEAD = %s, want %s", got, want)
	}

	// No remote, no URL: it says what to do rather than printing half of one.
	bare := gitRepo(t)
	ark(t, bare, "init")
	if _, err := arkErr(t, bare, "repo", "git-url"); err == nil ||
		!strings.Contains(err.Error(), "ark remote set") {
		t.Errorf("git-url with no remote: %v", err)
	}
}

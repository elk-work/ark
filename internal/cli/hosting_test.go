package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/servertest"
	"github.com/elk-work/ark/pkg/api"
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

	// No remote, no URL: it says what to do rather than printing half of
	// one.
	bare := gitRepo(t)
	ark(t, bare, "init")
	if _, err := arkErr(t, bare, "repo", "git-url"); err == nil ||
		!strings.Contains(err.Error(), "ark remote set") {
		t.Errorf("git-url with no remote: %v", err)
	}
}

// `ark repo mirror set|show|clear` against a real service: refused for a
// caller who is not an operator (here, the shared service token), and the
// whole round trip for one who is.
func TestRepoMirrorSetShowClear(t *testing.T) {
	s := servertest.NewServer(t)
	s.BootstrapToken = "boot"
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	t.Setenv("ARK_TOKEN", servertest.Token)

	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", ts.URL)
	ark(t, dir, "sync")

	const mirror = "https://github.com/elk-work/example.git"
	_, err := arkErr(t, dir, "repo", "mirror", "set", mirror)
	if code := records.ExitCode(err); code != 5 || !strings.Contains(err.Error(), "operator") {
		t.Fatalf("mirror set by a non-operator: exit %d, %v", code, err)
	}
	if _, err := arkErr(t, dir, "repo", "mirror", "set", "git@github.com:elk-work/example.git"); records.ExitCode(err) != 2 {
		t.Errorf("an ssh mirror URL: %v, want a validation error", err)
	}

	// The first principal minted on a service with no operator is one.
	req, _ := http.NewRequest("POST", ts.URL+"/v1/principals",
		strings.NewReader(`{"email":"op@example.com","display_name":"Op"}`))
	req.Header.Set("Authorization", "Bearer boot")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var minted api.CreatePrincipalResponse
	if err := json.NewDecoder(resp.Body).Decode(&minted); err != nil || resp.StatusCode != 200 {
		t.Fatalf("mint operator: %d %v", resp.StatusCode, err)
	}
	resp.Body.Close()
	ark(t, dir, "repo", "grant", "op@example.com", "--admin")
	t.Setenv("ARK_TOKEN", minted.Token)

	var set api.RepositoryResponse
	arkJSON(t, dir, &set, "repo", "mirror", "set", mirror)
	if !set.Changed || set.Repository.Mirror.URL != mirror {
		t.Fatalf("mirror set: %+v", set)
	}
	var shown api.MirrorStatus
	arkJSON(t, dir, &shown, "repo", "mirror", "show")
	if shown.URL != mirror {
		t.Errorf("mirror show: %+v", shown)
	}
	if out := ark(t, dir, "repo", "show"); !strings.Contains(out, mirror) {
		t.Errorf("repo show does not mention the mirror:\n%s", out)
	}
	ark(t, dir, "repo", "mirror", "clear")
	arkJSON(t, dir, &shown, "repo", "mirror", "show")
	if shown.URL != "" {
		t.Errorf("after clear: %+v", shown)
	}
}

package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

// gitHarness is a real ark-server on a real port, a registered repository,
// and a Git environment that cannot reach anything on the machine running
// the test: no system or global configuration (so no osxkeychain storing
// test credentials, no credential prompt), and a HOME of its own.
type gitHarness struct {
	*authServer
	ts  *httptest.Server
	env []string
}

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not on PATH")
	}
}

// isolatedGitEnv is the environment every Git process in these tests runs
// with, client and server alike.
func isolatedGitEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	global := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(global, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(strings.ToUpper(name), "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"HOME="+home, "USERPROFILE="+home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+global, "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com",
	)
}

func newGitHarness(t *testing.T) *gitHarness {
	t.Helper()
	requireGit(t)
	a := newAuthServer(t)
	a.GitDataDir = t.TempDir()
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	ts := httptest.NewServer(a.Handler())
	t.Cleanup(ts.Close)
	return &gitHarness{authServer: a, ts: ts, env: isolatedGitEnv(t)}
}

// remote is the clone URL for a repository with a credential in it, the way
// a person would paste one; user is arbitrary, which is the point.
func (h *gitHarness) remote(id, credential string) string {
	u, _ := url.Parse(h.ts.URL + "/git/" + id)
	if credential != "" {
		u.User = url.UserPassword("anyone", credential)
	}
	return u.String()
}

// git runs a Git command, returning its combined output and error.
func (h *gitHarness) git(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = h.env
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *gitHarness) mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := h.git(t, dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

// workTree is a fresh repository with one commit on main.
func (h *gitHarness) workTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	h.mustGit(t, dir, "init", "--quiet", "--initial-branch=main", ".")
	h.commit(t, dir, "first", files)
	return dir
}

func (h *gitHarness) commit(t *testing.T, dir, msg string, files map[string]string) string {
	t.Helper()
	for name, body := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h.mustGit(t, dir, "add", "-A")
	h.mustGit(t, dir, "commit", "--quiet", "-m", msg)
	return strings.TrimSpace(h.mustGit(t, dir, "rev-parse", "HEAD"))
}

// credentials mints a writer and a reader on the harness repository. The
// first principal minted on a fresh service is its operator.
func (h *gitHarness) credentials(t *testing.T) (writer, reader api.CreatePrincipalResponse) {
	t.Helper()
	writer = mintCredentialFor(t, h.authServer, "writer@example.com")
	reader = mintCredentialFor(t, h.authServer, "reader@example.com")
	grantTo(t, h.authServer, repoID, "writer@example.com", api.GrantWrite)
	grantTo(t, h.authServer, repoID, "reader@example.com", api.GrantRead)
	return writer, reader
}

// The issue's "done when", first clause: a push creates the bare repository,
// a fresh clone with an Ark credential gets it back — on the default branch —
// and a fetch with a read grant works while a push with one is refused.
func TestGitPushCreatesTheRepositoryAndCloneReturnsIt(t *testing.T) {
	h := newGitHarness(t)
	writer, reader := h.credentials(t)

	bare := h.bareRepoDir(repoID)
	if _, err := os.Stat(bare); !os.IsNotExist(err) {
		t.Fatalf("bare repository exists before anything touched it: %v", err)
	}

	src := h.workTree(t, map[string]string{"README.md": "hello\n"})
	head := strings.TrimSpace(h.mustGit(t, src, "rev-parse", "HEAD"))
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")

	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Fatalf("push did not create the bare repository: %v", err)
	}
	if got := strings.TrimSpace(h.mustGit(t, bare, "rev-parse", "refs/heads/main")); got != head {
		t.Errorf("bare main = %s, want %s", got, head)
	}

	// A reader clones, and lands on main with the file in it.
	parent := t.TempDir()
	h.mustGit(t, parent, "clone", "--quiet", h.remote(repoID, reader.Token), "clone")
	clone := filepath.Join(parent, "clone")
	body, err := os.ReadFile(filepath.Join(clone, "README.md"))
	if err != nil || string(body) != "hello\n" {
		t.Fatalf("clone has README.md = %q, %v", body, err)
	}
	if branch := strings.TrimSpace(h.mustGit(t, clone, "rev-parse", "--abbrev-ref", "HEAD")); branch != "main" {
		t.Errorf("clone checked out %q, want main", branch)
	}

	// The reader cannot push, and the refusal is the grant's, not a crash.
	h.commit(t, clone, "from a reader", map[string]string{"README.md": "changed\n"})
	out, err := h.git(t, clone, "push", "--quiet", h.remote(repoID, reader.Token), "main")
	if err == nil {
		t.Fatalf("a read grant pushed:\n%s", out)
	}
	if !strings.Contains(out, "403") {
		t.Errorf("push with a read grant failed for some other reason:\n%s", out)
	}
	if got := strings.TrimSpace(h.mustGit(t, bare, "rev-parse", "refs/heads/main")); got != head {
		t.Errorf("a refused push moved main to %s", got)
	}

	// And a writer's follow-up push lands, through a Bearer header this time
	// rather than Basic.
	next := h.commit(t, src, "second", map[string]string{"b.txt": "b\n"})
	h.mustGit(t, src, "-c", "http.extraHeader=Authorization: Bearer "+writer.Token,
		"push", "--quiet", h.remote(repoID, ""), "main")
	if got := strings.TrimSpace(h.mustGit(t, bare, "rev-parse", "refs/heads/main")); got != next {
		t.Errorf("bearer push: bare main = %s, want %s", got, next)
	}
}

// Without a credential, Git is asked to authenticate — 401 with a Basic
// challenge, which is what makes it consult a credential helper — and a
// clone with nothing to answer it fails.
func TestGitRefusesWithoutACredential(t *testing.T) {
	h := newGitHarness(t)
	h.credentials(t)

	for _, service := range []string{serviceUploadPack, serviceReceivePack} {
		resp, err := http.Get(h.ts.URL + "/git/" + repoID + "/info/refs?service=" + service)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Errorf("%s with no credential: %d, want 401", service, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic") {
			t.Errorf("%s: WWW-Authenticate = %q, want a Basic challenge", service, got)
		}
	}

	out, err := h.git(t, t.TempDir(), "clone", "--quiet", h.remote(repoID, ""), "c")
	if err == nil {
		t.Fatalf("clone with no credential succeeded:\n%s", out)
	}
	out, err = h.git(t, t.TempDir(), "clone", "--quiet", h.remote(repoID, "arkc_notacredential"), "c")
	if err == nil {
		t.Fatalf("clone with a made-up credential succeeded:\n%s", out)
	}
}

// The ref advertisement for a push is where Git learns whether it may; a
// read grant is refused there, before any pack is sent. And a principal with
// no grant reads nothing.
func TestGitGrantsAreEnforcedOnTheAdvertisement(t *testing.T) {
	h := newGitHarness(t)
	_, reader := h.credentials(t)
	stranger := mintCredentialFor(t, h.authServer, "stranger@example.com")

	get := func(cred, service string) int {
		req, _ := http.NewRequest("GET", h.ts.URL+"/git/"+repoID+".git/info/refs?service="+service, nil)
		req.SetBasicAuth("x", cred)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get(reader.Token, serviceUploadPack); got != 200 {
		t.Errorf("reader, upload-pack: %d, want 200", got)
	}
	if got := get(reader.Token, serviceReceivePack); got != 403 {
		t.Errorf("reader, receive-pack: %d, want 403", got)
	}
	if got := get(stranger.Token, serviceUploadPack); got != 403 {
		t.Errorf("no grant, upload-pack: %d, want 403", got)
	}
	// Dumb HTTP is not served.
	req, _ := http.NewRequest("GET", h.ts.URL+"/git/"+repoID+"/info/refs", nil)
	req.SetBasicAuth("x", reader.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Error("info/refs without ?service= was served")
	}
}

// A repository the service has not registered is a 404, and pushing to one
// creates nothing — not a bare repository, not a registration.
func TestGitUnregisteredRepositoryIs404(t *testing.T) {
	h := newGitHarness(t)
	writer, _ := h.credentials(t)

	req, _ := http.NewRequest("GET", h.ts.URL+"/git/"+secondRepoID+"/info/refs?service="+serviceReceivePack, nil)
	req.SetBasicAuth("x", writer.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unregistered repository: %d, want 404", resp.StatusCode)
	}

	src := h.workTree(t, map[string]string{"a": "a"})
	if out, err := h.git(t, src, "push", h.remote(secondRepoID, writer.Token), "main"); err == nil {
		t.Fatalf("push to an unregistered repository succeeded:\n%s", out)
	}
	if _, err := os.Stat(h.bareRepoDir(secondRepoID)); !os.IsNotExist(err) {
		t.Errorf("a refused push left a bare repository behind: %v", err)
	}
	// Not a ULID at all.
	req, _ = http.NewRequest("GET", h.ts.URL+"/git/..%2f..%2fetc/info/refs?service="+serviceUploadPack, nil)
	req.SetBasicAuth("x", writer.Token)
	if resp, err = http.DefaultClient.Do(req); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("a path that is not a ULID: %d, want 404", resp.StatusCode)
	}
}

// Git compresses a large request body and says so in Content-Encoding. The
// service inflates it before upload-pack sees it.
func TestGitAcceptsAGzippedRequestBody(t *testing.T) {
	h := newGitHarness(t)
	writer, _ := h.credentials(t)
	src := h.workTree(t, map[string]string{"a": "a"})
	head := strings.TrimSpace(h.mustGit(t, src, "rev-parse", "HEAD"))
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")

	var raw bytes.Buffer
	raw.WriteString(pktLine("want " + head + " no-progress\n"))
	raw.WriteString("0000")
	raw.WriteString(pktLine("done\n"))
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write(raw.Bytes())
	zw.Close()

	req, _ := http.NewRequest("POST", h.ts.URL+"/git/"+repoID+"/git-upload-pack", &gz)
	req.SetBasicAuth("x", writer.Token)
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("gzipped upload-pack: %d %s", resp.StatusCode, body)
	}
	if !bytes.HasPrefix(body, []byte("0008NAK\n")) || !bytes.Contains(body, []byte("PACK")) {
		t.Errorf("gzipped upload-pack did not answer with a pack: %q", body[:min(len(body), 64)])
	}
}

func TestDiffRefs(t *testing.T) {
	a, b, c := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	zero := strings.Repeat("0", 40)
	got := diffRefs(
		map[string]string{"refs/heads/main": a, "refs/heads/gone": b, "refs/tags/v1": c},
		map[string]string{"refs/heads/main": b, "refs/heads/new": c, "refs/tags/v1": c},
	)
	want := []refUpdate{
		{Ref: "refs/heads/gone", Before: b, After: zero},
		{Ref: "refs/heads/main", Before: a, After: b},
		{Ref: "refs/heads/new", Before: zero, After: c},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("diffRefs = %v, want %v", got, want)
	}
}

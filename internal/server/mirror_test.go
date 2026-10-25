package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

// mirrorHarness is a git harness whose writer is also the service's operator
// and the repository's admin — the one principal allowed to set a mirror —
// with the human actor a metadata write delegates from.
func mirrorHarness(t *testing.T) (*gitHarness, api.CreatePrincipalResponse) {
	t.Helper()
	h := newGitHarness(t)
	h.allowInsecureMirrors = true
	writer, _ := h.credentials(t) // minted first, so the operator
	grantTo(t, h.authServer, repoID, "writer@example.com", api.GrantAdmin)
	seedHuman(t, h.Server)
	return h, writer
}

func setMirrorAs(t *testing.T, h *gitHarness, bearer, mirror string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{%s,"mirror_url":%q}`, writerFragment, mirror)
	rec := doRequestAs(t, h.Server, bearer, "POST", metaPath, body)
	return rec.Code, rec.Body.String()
}

// fileURL is a file:// URL Git accepts on every platform, Windows drive
// letters included.
func fileURL(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file://" + p
}

// bareTarget is an empty bare repository standing in for GitHub.
func (h *gitHarness) bareTarget(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "github.git")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.mustGit(t, dir, "init", "--bare", "--quiet", ".")
	return dir
}

func (h *gitHarness) refsOf(t *testing.T, dir string) string {
	t.Helper()
	out, _ := h.git(t, dir, "for-each-ref", "--format=%(objectname) %(refname)")
	return strings.TrimSpace(out)
}

func (h *gitHarness) mirrorStatus(t *testing.T) api.MirrorStatus {
	t.Helper()
	rec := doRequestAs(t, h.Server, h.Token, "GET", "/v1/repositories/"+repoID, "")
	if rec.Code != 200 {
		t.Fatalf("get repository: %d %s", rec.Code, rec.Body.String())
	}
	var meta api.RepositoryMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	return meta.Mirror
}

// A push to the service is pushed on to the mirror, all of it — every
// branch and tag, and a deleted branch deleted there too — and the outcome
// is on GET /v1/repositories/{repo}.
func TestGitPushIsMirrored(t *testing.T) {
	h, writer := mirrorHarness(t)
	target := h.bareTarget(t)
	if code, body := setMirrorAs(t, h, writer.Token, fileURL(target)); code != 200 {
		t.Fatalf("set mirror: %d %s", code, body)
	}

	src := h.workTree(t, map[string]string{"README.md": "hi\n"})
	h.mustGit(t, src, "branch", "feature")
	h.mustGit(t, src, "tag", "v1")
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main", "feature", "v1")
	h.WaitBackground()

	bare := h.bareRepoDir(repoID)
	if got, want := h.refsOf(t, target), h.refsOf(t, bare); got != want || want == "" {
		t.Fatalf("mirror refs:\n%s\nwant:\n%s", got, want)
	}
	st := h.mirrorStatus(t)
	if st.URL != fileURL(target) || st.LastOKAt == "" || st.LastError != "" {
		t.Errorf("mirror status after a good push: %+v", st)
	}

	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), ":feature")
	h.WaitBackground()
	if refs := h.refsOf(t, target); strings.Contains(refs, "refs/heads/feature") {
		t.Errorf("a branch deleted on the service survived on the mirror:\n%s", refs)
	}
}

// The ticker mirrors a repository whose refs have moved since its last
// successful mirror — here, one that was pushed before any mirror was set —
// and leaves alone one that has not.
func TestMirrorTickerCatchesUp(t *testing.T) {
	h, writer := mirrorHarness(t)
	src := h.workTree(t, map[string]string{"a": "a"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()

	target := h.bareTarget(t)
	if code, body := setMirrorAs(t, h, writer.Token, fileURL(target)); code != 200 {
		t.Fatalf("set mirror: %d %s", code, body)
	}
	h.mirrorAll(t.Context())
	if got, want := h.refsOf(t, target), h.refsOf(t, h.bareRepoDir(repoID)); got != want {
		t.Fatalf("after a tick, mirror refs:\n%s\nwant:\n%s", got, want)
	}

	// Nothing moved, so the next tick pushes nothing: a ref removed at the
	// far end by hand stays removed until the service has something new.
	h.mustGit(t, target, "update-ref", "-d", "refs/heads/main")
	h.mirrorAll(t.Context())
	if refs := h.refsOf(t, target); refs != "" {
		t.Errorf("a tick with nothing new pushed anyway:\n%s", refs)
	}
}

// A mirror that cannot be pushed to is a recorded error and nothing more:
// the push to the service lands, and the error is readable.
func TestMirrorFailureNeverFailsThePush(t *testing.T) {
	h, writer := mirrorHarness(t)
	missing := filepath.Join(t.TempDir(), "nowhere.git")
	if code, body := setMirrorAs(t, h, writer.Token, fileURL(missing)); code != 200 {
		t.Fatalf("set mirror: %d %s", code, body)
	}
	src := h.workTree(t, map[string]string{"a": "a"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()

	if got := h.refsOf(t, h.bareRepoDir(repoID)); !strings.Contains(got, "refs/heads/main") {
		t.Fatalf("the push did not land: %q", got)
	}
	st := h.mirrorStatus(t)
	if st.LastError == "" || st.LastAttemptAt == "" || st.LastOKAt != "" {
		t.Errorf("mirror status after a failed mirror: %+v", st)
	}
}

// The credential reaches the mirror host, and only through the helper: a
// second ark-server stands in for GitHub and requires its own credential,
// which the first service holds as ARK_MIRROR_CREDENTIAL.
func TestMirrorPushesWithTheServiceCredential(t *testing.T) {
	h, writer := mirrorHarness(t)
	github := newGitHarness(t)
	githubWriter, _ := github.credentials(t)
	h.MirrorCredential = "mirror-bot:" + githubWriter.Token

	target := github.ts.URL + "/git/" + repoID
	if code, body := setMirrorAs(t, h, writer.Token, target); code != 200 {
		t.Fatalf("set mirror: %d %s", code, body)
	}
	src := h.workTree(t, map[string]string{"a": "a"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()

	if st := h.mirrorStatus(t); st.LastError != "" || st.LastOKAt == "" {
		t.Fatalf("mirror with a credential: %+v", st)
	}
	if got, want := github.refsOf(t, github.bareRepoDir(repoID)), h.refsOf(t, h.bareRepoDir(repoID)); got != want {
		t.Fatalf("the second service holds:\n%s\nwant:\n%s", got, want)
	}

	// And without the credential the same mirror fails, saying so, and the
	// error carries no secret.
	h.MirrorCredential = "mirror-bot:arkc_wrong"
	h.commit(t, src, "second", map[string]string{"b": "b"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()
	st := h.mirrorStatus(t)
	if st.LastError == "" {
		t.Fatalf("a mirror with a wrong credential reported success: %+v", st)
	}
	if strings.Contains(st.LastError, "arkc_wrong") {
		t.Errorf("the mirror error leaked the credential: %s", st.LastError)
	}
}

// Setting a mirror is an operator act on top of admin, the URL is https with
// no credential in it, and "" clears it.
func TestMirrorSettingRules(t *testing.T) {
	h, operator := mirrorHarness(t)
	h.allowInsecureMirrors = false

	admin := mintCredentialFor(t, h.authServer, "admin@example.com")
	grantTo(t, h.authServer, repoID, "admin@example.com", api.GrantAdmin)
	if code, body := setMirrorAs(t, h, admin.Token, "https://github.com/elk-work/scout.git"); code != 403 {
		t.Errorf("an admin who is not an operator set a mirror: %d %s", code, body)
	}
	for _, bad := range []string{
		"http://github.com/elk-work/scout.git",
		"https://user:token@github.com/elk-work/scout.git",
		"git@github.com:elk-work/scout.git",
		"file:///srv/git/other.git",
		"https://github.com/elk-work/scout.git?x=1",
	} {
		if code, body := setMirrorAs(t, h, operator.Token, bad); code != 400 {
			t.Errorf("mirror %q: %d %s, want 400", bad, code, body)
		}
	}
	if code, body := setMirrorAs(t, h, operator.Token, "https://github.com/elk-work/scout.git"); code != 200 {
		t.Fatalf("operator set: %d %s", code, body)
	}
	if st := h.mirrorStatus(t); st.URL != "https://github.com/elk-work/scout.git" {
		t.Errorf("after set: %+v", st)
	}
	if code, body := setMirrorAs(t, h, operator.Token, ""); code != 200 {
		t.Fatalf("operator clear: %d %s", code, body)
	}
	if st := h.mirrorStatus(t); st != (api.MirrorStatus{}) {
		t.Errorf("after clear: %+v", st)
	}
}

func TestMirrorCredentialParts(t *testing.T) {
	for _, c := range []struct{ in, user, pass string }{
		{"", "", ""},
		{"ghp_abc", "x-access-token", "ghp_abc"},
		{"bot:ghp_abc", "bot", "ghp_abc"},
		{":ghp_abc", "x-access-token", ":ghp_abc"},
	} {
		if u, p := mirrorCredentialParts(c.in); u != c.user || p != c.pass {
			t.Errorf("mirrorCredentialParts(%q) = %q, %q", c.in, u, p)
		}
	}
}

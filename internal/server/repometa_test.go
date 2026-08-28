package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

const metaPath = "/v1/repositories/" + repoID + "/metadata"

// writer is the request fragment naming the seeded agent, which every write
// route resolves before it changes anything.
var writerFragment = fmt.Sprintf(`"writer":{"agent_name":%q,"delegated_by":%q}`, agent, humanID)

// registerRepoWith registers the repository with metadata a client would
// have sent on its first sync, which is the only moment these fields are
// writable without this route.
func registerRepoWith(t *testing.T, s *Server, name, branch, remote string) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"name":%q,"default_branch":%q,"git_remote_url":%q}`,
		repoID, name, branch, remote)
	if rec := doRequest(t, s, "POST", "/v1/repositories", body); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
}

// metaServer is a repository registered with the wrong metadata, and a human
// actor the writer can delegate from.
func metaServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(t)
	registerRepoWith(t, s, "weirdly-named-dir", "main", "https://github.com/someone/gone.git")
	seedHuman(t, s)
	return s
}

func getRepo(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, s, "GET", "/v1/repositories/"+id, "")
}

func decodeMeta(t *testing.T, rec *httptest.ResponseRecorder) api.RepositoryMetadata {
	t.Helper()
	var meta api.RepositoryMetadata
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("decode metadata %q: %v", rec.Body.String(), err)
	}
	return meta
}

func decodeRepoResp(t *testing.T, rec *httptest.ResponseRecorder) api.RepositoryResponse {
	t.Helper()
	var resp api.RepositoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp
}

// setMeta posts a metadata correction, failing the test unless it is served.
func setMeta(t *testing.T, s *Server, key, fields string) api.RepositoryResponse {
	t.Helper()
	rec := writeReq(t, s, metaPath, key, `{`+writerFragment+`,`+fields+`}`)
	if rec.Code != 200 {
		t.Fatalf("set metadata: %d %s", rec.Code, rec.Body.String())
	}
	return decodeRepoResp(t, rec)
}

// TestRepositoryMetadataIsCorrectable is the issue in one test: a repository
// registered under the name of the directory someone happened to be standing
// in, corrected afterwards, with the correction readable and ordered by a
// revision.
func TestRepositoryMetadataIsCorrectable(t *testing.T) {
	s := metaServer(t)

	before := decodeMeta(t, getRepo(t, s, repoID))
	if before.Name != "weirdly-named-dir" || before.ID != repoID {
		t.Fatalf("registered metadata: %+v", before)
	}

	baseRev := revision(t, s)
	resp := setMeta(t, s, "", `"name":"scout","git_remote_url":"https://github.com/elk-work/scout.git"`)
	if !resp.Changed {
		t.Error("a real correction reported changed = false")
	}
	if resp.ServerRevision <= baseRev {
		t.Errorf("revision %d did not advance past %d", resp.ServerRevision, baseRev)
	}
	if resp.Repository.Revision != resp.ServerRevision {
		t.Errorf("record revision %d, response revision %d",
			resp.Repository.Revision, resp.ServerRevision)
	}

	after := decodeMeta(t, getRepo(t, s, repoID))
	if after.Name != "scout" || after.GitRemoteURL != "https://github.com/elk-work/scout.git" {
		t.Errorf("after correction: %+v", after)
	}
	// The field nobody asserted is untouched, and so is the identity.
	if after.DefaultBranch != "main" || after.ID != repoID {
		t.Errorf("unasserted fields changed: %+v", after)
	}

	// A later sync cannot undo it: registration backfills only, so the
	// client's own basename loses to the corrected value.
	registerRepoWith(t, s, "weirdly-named-dir", "main", "")
	if again := decodeMeta(t, getRepo(t, s, repoID)); again.Name != "scout" {
		t.Errorf("a sync renamed the repository back to %q", again.Name)
	}
}

// A repeat of a correction already applied is a correct answer, not a write:
// no revision, and changed = false so a caller can tell which happened.
func TestSetRepositoryMetadataNoOp(t *testing.T) {
	s := metaServer(t)
	setMeta(t, s, "", `"name":"scout"`)
	settled := revision(t, s)

	resp := setMeta(t, s, "", `"name":"scout"`)
	if resp.Changed {
		t.Error("a no-op reported changed = true")
	}
	if got := revision(t, s); got != settled {
		t.Errorf("a no-op bumped the revision %d -> %d", settled, got)
	}
	if resp.ServerRevision != settled {
		t.Errorf("no-op reported revision %d, want %d", resp.ServerRevision, settled)
	}
	// Whitespace around a value it already holds is the same assertion.
	if resp := setMeta(t, s, "", `"name":"  scout  "`); resp.Changed {
		t.Error("a value differing only in surrounding whitespace was treated as a change")
	}
}

// git_remote_url is the one field an explicit empty value clears, for a
// repository that genuinely has no remote. The others cannot be emptied.
func TestSetRepositoryMetadataClearsTheRemote(t *testing.T) {
	s := metaServer(t)
	resp := setMeta(t, s, "", `"git_remote_url":""`)
	if !resp.Changed || resp.Repository.GitRemoteURL != "" {
		t.Errorf("clearing the remote: %+v", resp)
	}
	if got := decodeMeta(t, getRepo(t, s, repoID)).GitRemoteURL; got != "" {
		t.Errorf("remote after clearing = %q", got)
	}
}

func TestSetRepositoryMetadataValidation(t *testing.T) {
	cases := []struct {
		name     string
		fields   string
		wantCode int
	}{
		{"asserts nothing", `"unrelated":1`, 400},
		{"empty name", `"name":""`, 400},
		{"whitespace name", `"name":"   "`, 400},
		{"newline in name", `"name":"scout\nwatch"`, 400},
		{"empty branch", `"default_branch":""`, 400},
		{"branch with a space", `"default_branch":"my branch"`, 400},
		{"branch with ..", `"default_branch":"a..b"`, 400},
		{"branch ending in .lock", `"default_branch":"main.lock"`, 400},
		{"branch beginning with a slash", `"default_branch":"/main"`, 400},
		{"remote with a space", `"git_remote_url":"https://host/a repo.git"`, 400},
		{"remote with no host", `"git_remote_url":"https:///scout.git"`, 400},
		{"remote that is a bare word", `"git_remote_url":"scout"`, 400},
		{"unsupported scheme", `"git_remote_url":"javascript:alert(1)"`, 400},
		{"https remote", `"git_remote_url":"https://github.com/elk-work/scout.git"`, 200},
		{"scp-like remote", `"git_remote_url":"git@github.com:elk-work/scout.git"`, 200},
		{"ssh url remote", `"git_remote_url":"ssh://git@github.com/elk-work/scout.git"`, 200},
		{"local path remote", `"git_remote_url":"/srv/git/scout.git"`, 200},
		{"a branch Git accepts", `"default_branch":"release/2026-08"`, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := metaServer(t)
			rec := writeReq(t, s, metaPath, "", `{`+writerFragment+`,`+c.fields+`}`)
			if rec.Code != c.wantCode {
				t.Fatalf("code %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
			if c.wantCode != 400 {
				return
			}
			if got := errCode(t, rec); got != "validation" {
				t.Errorf("error code %q, want validation", got)
			}
			// A rejected request must leave nothing behind — the fault rolls
			// the transaction back rather than committing a partial write.
			if got := decodeMeta(t, getRepo(t, s, repoID)); got.Name != "weirdly-named-dir" {
				t.Errorf("a rejected request changed the metadata: %+v", got)
			}
		})
	}
}

// The writer rule is the same one every write route runs, and it is where
// RFC-0003's per-repository grant check will go.
func TestSetRepositoryMetadataResolvesTheWriter(t *testing.T) {
	cases := []struct {
		name     string
		writer   string
		wantCode int
	}{
		{"no agent name", `{"delegated_by":"` + humanID + `"}`, 400},
		{"new agent without delegation", `{"agent_name":"drifter"}`, 400},
		{"delegated to nobody", `{"agent_name":"drifter","delegated_by":"01NOBODY0000000000000000000"}`, 400},
		{"registered agent", `{"agent_name":"drifter","delegated_by":"` + humanID + `"}`, 200},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := metaServer(t)
			rec := writeReq(t, s, metaPath, "", `{"writer":`+c.writer+`,"name":"scout"}`)
			if rec.Code != c.wantCode {
				t.Fatalf("code %d, want %d (%s)", rec.Code, c.wantCode, rec.Body.String())
			}
		})
	}
}

// A key is honoured though not required: the replay returns the first
// outcome verbatim and mints nothing.
func TestSetRepositoryMetadataIdempotency(t *testing.T) {
	s := metaServer(t)
	first := setMeta(t, s, "fix-the-name", `"name":"scout"`)
	settled := revision(t, s)

	// A replay of the same key, with a body that would otherwise change
	// something, returns what the key already answered.
	rec := writeReq(t, s, metaPath, "fix-the-name",
		`{`+writerFragment+`,"name":"something-else"}`)
	if rec.Code != 200 {
		t.Fatalf("replay: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay did not announce itself: %v", rec.Header())
	}
	replay := decodeRepoResp(t, rec)
	if replay.Repository.Name != first.Repository.Name || replay.ServerRevision != first.ServerRevision {
		t.Errorf("replay = %+v, want %+v", replay, first)
	}
	if got := revision(t, s); got != settled {
		t.Errorf("replay bumped the revision %d -> %d", settled, got)
	}
	if got := decodeMeta(t, getRepo(t, s, repoID)).Name; got != "scout" {
		t.Errorf("replay applied the second body: name = %q", got)
	}
}

func TestRepositoryMetadataOnAnUnregisteredRepository(t *testing.T) {
	s := newTestServer(t)
	const absent = "01NOSUCHREPO00000000000000"
	if rec := getRepo(t, s, absent); rec.Code != 404 {
		t.Errorf("show: %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
	rec := writeReq(t, s, "/v1/repositories/"+absent+"/metadata", "",
		`{`+writerFragment+`,"name":"scout"}`)
	if rec.Code != 404 {
		t.Errorf("set: %d, want 404 (%s)", rec.Code, rec.Body.String())
	}
}

func TestRepositoryMetadataRoutesRequireAuth(t *testing.T) {
	s := metaServer(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/repositories/" + repoID},
		{"POST", metaPath},
	} {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("%s %s without a token: %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

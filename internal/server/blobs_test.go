package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func blobKey(digest string) string { return "sha256/" + digest[:2] + "/" + digest }

// live starts the real handler so signed URLs are reachable, and points the
// blob store's BaseURL at it.
func live(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := newTestServer(t)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	s.Blobs.(*LocalBlobStore).BaseURL = ts.URL
	return s, ts
}

func putBlob(t *testing.T, url string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// uploadURL asks the service for a signed PUT target.
func uploadURL(t *testing.T, s *Server, digest string, size int64) api.UploadURLResponse {
	t.Helper()
	body, _ := json.Marshal(api.UploadURLRequest{RepositoryID: repoID, SHA256: digest, SizeBytes: size})
	rec := doRequest(t, s, "POST", "/v1/artifacts/upload-url", string(body))
	if rec.Code != 200 {
		t.Fatalf("upload-url: %d %s", rec.Code, rec.Body.String())
	}
	var out api.UploadURLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func confirm(t *testing.T, s *Server, digest string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(api.UploadURLRequest{RepositoryID: repoID, SHA256: digest})
	return doRequest(t, s, "POST", "/v1/artifacts/confirm", string(body))
}

// The whole point of a content-addressed store: the bytes at a hash must
// actually hash to it. Spec §6.9 calls artifacts immutable by checksum, and
// confirm is what stamps storage_key onto every artifact record carrying that
// hash — so if confirm only checked existence, whoever reached the upload URL
// first would decide what "that artifact" contains, forever.
func TestConfirmRejectsContentThatDoesNotMatchItsChecksum(t *testing.T) {
	s, _ := live(t)
	registerRepo(t, s)

	honest := []byte("the real benchmark output")
	digest := sha256Hex(honest)
	up := uploadURL(t, s, digest, int64(len(honest)))

	// Store different bytes at the honest content's address.
	if code := putBlob(t, up.URL, []byte("poisoned")); code != http.StatusOK {
		t.Fatalf("put: %d", code)
	}

	rec := confirm(t, s, digest)
	if rec.Code != http.StatusConflict {
		t.Fatalf("confirm accepted mismatched content: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "does not match its checksum") {
		t.Errorf("error should name the cause: %s", rec.Body.String())
	}

	// The bad object must not survive: left in place it would satisfy a later
	// confirm, and it is unreachable by any correct client anyway.
	if ok, _ := s.Blobs.Exists(context.Background(), blobKey(digest)); ok {
		t.Error("mismatched blob was left in storage")
	}
}

// The honest path still works end to end — verification must not become a
// wall that correct clients hit.
func TestConfirmAcceptsContentThatMatches(t *testing.T) {
	s, _ := live(t)
	registerRepo(t, s)

	content := []byte("bench: 41ms p95")
	digest := sha256Hex(content)
	up := uploadURL(t, s, digest, int64(len(content)))

	if code := putBlob(t, up.URL, content); code != http.StatusOK {
		t.Fatalf("put: %d", code)
	}
	if rec := confirm(t, s, digest); rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}

	// And it can be read back through a signed GET.
	body, _ := json.Marshal(api.DownloadURLRequest{RepositoryID: repoID, StorageKey: blobKey(digest)})
	rec := doRequest(t, s, "POST", "/v1/artifacts/download-url", string(body))
	if rec.Code != 200 {
		t.Fatalf("download-url: %d %s", rec.Code, rec.Body.String())
	}
	var dl api.DownloadURLResponse
	json.Unmarshal(rec.Body.Bytes(), &dl)

	resp, err := http.Get(dl.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("signed GET: %d", resp.StatusCode)
	}
}

// In DATA_DIR mode the /blobs/ routes cannot sit behind the bearer middleware,
// because clients treat them as pre-signed URLs. So the signature is the only
// thing protecting artifact content from anyone who can reach the service.
func TestUnsignedBlobRequestsAreRefused(t *testing.T) {
	s, ts := live(t)
	registerRepo(t, s)

	content := []byte("private evidence")
	digest := sha256Hex(content)
	up := uploadURL(t, s, digest, int64(len(content)))
	if code := putBlob(t, up.URL, content); code != http.StatusOK {
		t.Fatalf("put: %d", code)
	}

	bare := ts.URL + "/blobs/" + blobKey(digest)

	t.Run("unsigned read", func(t *testing.T) {
		resp, err := http.Get(bare)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("unsigned GET = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("unsigned write", func(t *testing.T) {
		if code := putBlob(t, bare, []byte("anything")); code != http.StatusForbidden {
			t.Errorf("unsigned PUT = %d, want 403", code)
		}
	})

	t.Run("forged signature", func(t *testing.T) {
		forged := fmt.Sprintf("%s?exp=%d&sig=%s", bare, 1<<40, strings.Repeat("a", 64))
		resp, err := http.Get(forged)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("forged GET = %d, want 403", resp.StatusCode)
		}
	})

	t.Run("expired signature", func(t *testing.T) {
		local := s.Blobs.(*LocalBlobStore)
		key := blobKey(digest)
		past := int64(1)
		url := fmt.Sprintf("%s/blobs/%s?exp=%d&sig=%s", ts.URL, key, past,
			signBlob(local.Secret, http.MethodGet, key, past))
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expired GET = %d, want 403", resp.StatusCode)
		}
	})
}

// A read capability must not become a write capability. The signature covers
// the method for exactly this reason.
func TestAReadSignatureCannotBeReplayedAsAWrite(t *testing.T) {
	s, ts := live(t)
	registerRepo(t, s)

	content := []byte("evidence")
	digest := sha256Hex(content)
	up := uploadURL(t, s, digest, int64(len(content)))
	putBlob(t, up.URL, content)
	if rec := confirm(t, s, digest); rec.Code != 200 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body.String())
	}

	getURL, err := s.Blobs.SignedGetURL(context.Background(), blobKey(digest))
	if err != nil {
		t.Fatal(err)
	}
	if code := putBlob(t, getURL, []byte("overwritten")); code != http.StatusForbidden {
		t.Errorf("a GET signature was accepted for PUT: %d", code)
	}
	_ = ts
}

// Without a secret the store must refuse to mint URLs rather than fall back
// to unsigned ones — a silent downgrade is how this class of bug survives.
func TestUnconfiguredLocalStoreWillNotMintURLs(t *testing.T) {
	l := &LocalBlobStore{Dir: t.TempDir(), BaseURL: "http://x"}
	if _, err := l.SignedPutURL(context.Background(), "sha256/aa/aaaa", ""); err == nil {
		t.Error("an unsigned store should refuse to produce a PUT URL")
	}
}

// Deleting a record that is already gone is idempotent — a repair run may
// replay it — but it must not mint a revision. Every client would then pull an
// empty change set, and repeated repairs would inflate the counter for nothing.
func TestDeletingAMissingRecordDoesNotMintARevision(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	before := push(t, s, api.Mutation{
		ID: "01MUT0000000000000000000A", RecordType: "task", RecordID: "01TASK000000000000000000A",
		Operation: "create", CreatedAt: "2026-07-01T00:00:00Z",
		Payload: []byte(`{"id":"01TASK000000000000000000A","title":"t","status":"open"}`),
	}).ServerRevision

	// Delete something that never existed.
	resp := push(t, s, api.Mutation{
		ID: "01MUT0000000000000000000B", RecordType: "task", RecordID: "01NEVEREXISTED0000000000",
		Operation: "delete", CreatedAt: "2026-07-02T00:00:00Z", Payload: []byte(`{}`),
	})
	if len(resp.Applied) != 1 {
		t.Fatalf("an idempotent delete should be applied, got %+v", resp)
	}
	if resp.ServerRevision != before {
		t.Errorf("revision moved %d -> %d for a no-op delete", before, resp.ServerRevision)
	}
}

// Registration runs on every sync and the name a client sends is only the
// basename of wherever it is checked out. If that overwrote the stored value,
// any client could silently rename the repository for everyone — and clear
// its Git remote, if that checkout happened not to have one. Observed against
// a live repository before this was fixed.
func TestRegisterBackfillsButNeverRenames(t *testing.T) {
	s := newTestServer(t)

	reg := func(name, branch, remote string) {
		t.Helper()
		body := fmt.Sprintf(`{"id":%q,"name":%q,"default_branch":%q,"git_remote_url":%q}`,
			repoID, name, branch, remote)
		if rec := doRequest(t, s, "POST", "/v1/repositories", body); rec.Code != 200 {
			t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
		}
	}
	meta := func() (name, branch, remote string) {
		t.Helper()
		if err := s.Repos.View(context.Background(), repoID, func(db *sql.DB) error {
			return db.QueryRow(`SELECT name, default_branch, git_remote_url FROM meta WHERE id = 1`).
				Scan(&name, &branch, &remote)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}

	reg("scout", "main", "https://github.com/elk-work/scout.git")
	if n, b, r := meta(); n != "scout" || b != "main" || r == "" {
		t.Fatalf("first registration should set everything: %q %q %q", n, b, r)
	}

	// A second client, checked out somewhere else, with no Git remote.
	reg("weirdly-named-dir", "master", "")
	n, b, r := meta()
	if n != "scout" {
		t.Errorf("name was overwritten to %q — a joining client renamed the repository", n)
	}
	if b != "main" {
		t.Errorf("default_branch was overwritten to %q", b)
	}
	if r != "https://github.com/elk-work/scout.git" {
		t.Errorf("git_remote_url was cleared to %q", r)
	}

	// But a field the server is missing can still be filled in.
	s2 := newTestServer(t)
	body := fmt.Sprintf(`{"id":%q,"name":"x","default_branch":"main","git_remote_url":""}`, repoID)
	if rec := doRequest(t, s2, "POST", "/v1/repositories", body); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	body = fmt.Sprintf(`{"id":%q,"name":"x","default_branch":"main","git_remote_url":"https://example.com/x.git"}`, repoID)
	if rec := doRequest(t, s2, "POST", "/v1/repositories", body); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var got string
	if err := s2.Repos.View(context.Background(), repoID, func(db *sql.DB) error {
		return db.QueryRow(`SELECT git_remote_url FROM meta WHERE id = 1`).Scan(&got)
	}); err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com/x.git" {
		t.Errorf("an empty field should be backfillable, got %q", got)
	}
}

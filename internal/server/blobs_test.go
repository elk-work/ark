package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elkproject/ark/pkg/api"
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

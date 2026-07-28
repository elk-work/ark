package cloud

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elkproject/ark/internal/records"
	"github.com/elkproject/ark/pkg/api"
)

// testClient builds a Client directly, bypassing token resolution.
func testClient(url string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(url, "/"),
		Token:   "test-token",
		HTTP:    &http.Client{Timeout: 10 * time.Second},
	}
}

// TestRequestsCarrySpecPathsAndBearerToken: every client method hits the
// endpoint the spec names (§19) with the bearer token set, and JSON bodies
// are marked as such. These paths are the wire contract with the server.
func TestRequestsCarrySpecPathsAndBearerToken(t *testing.T) {
	var got struct {
		method, path, auth, contentType string
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.auth = r.Header.Get("Authorization")
		got.contentType = r.Header.Get("Content-Type")
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	c := testClient(ts.URL)
	ctx := context.Background()

	calls := []struct {
		name       string
		wantMethod string
		wantPath   string
		hasBody    bool
		call       func() error
	}{
		{"RegisterRepo", "POST", "/v1/repositories", true, func() error {
			return c.RegisterRepo(ctx, api.RegisterRepositoryRequest{ID: "r1", Name: "n"})
		}},
		{"Push", "POST", "/v1/sync/push", true, func() error {
			_, err := c.Push(ctx, api.PushRequest{RepositoryID: "r1"})
			return err
		}},
		{"Pull", "POST", "/v1/sync/pull", true, func() error {
			_, err := c.Pull(ctx, api.PullRequest{RepositoryID: "r1"})
			return err
		}},
		{"GetRecord", "GET", "/v1/repositories/r1/records/task/t1", false, func() error {
			_, err := c.GetRecord(ctx, "r1", "task", "t1")
			return err
		}},
		{"Merge", "POST", "/v1/pull-requests/pr1/merge", true, func() error {
			_, err := c.Merge(ctx, "pr1", api.MergeRequest{RepositoryID: "r1"})
			return err
		}},
		{"UploadURL", "POST", "/v1/artifacts/upload-url", true, func() error {
			_, err := c.UploadURL(ctx, api.UploadURLRequest{RepositoryID: "r1"})
			return err
		}},
		{"ConfirmUpload", "POST", "/v1/artifacts/confirm", true, func() error {
			return c.ConfirmUpload(ctx, api.UploadURLRequest{RepositoryID: "r1"})
		}},
		{"DownloadURL", "POST", "/v1/artifacts/download-url", true, func() error {
			_, err := c.DownloadURL(ctx, api.DownloadURLRequest{RepositoryID: "r1"})
			return err
		}},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if got.method != tc.wantMethod || got.path != tc.wantPath {
				t.Errorf("request = %s %s, want %s %s", got.method, got.path, tc.wantMethod, tc.wantPath)
			}
			if got.auth != "Bearer test-token" {
				t.Errorf("Authorization = %q, want Bearer test-token", got.auth)
			}
			wantCT := ""
			if tc.hasBody {
				wantCT = "application/json"
			}
			if got.contentType != wantCT {
				t.Errorf("Content-Type = %q, want %q", got.contentType, wantCT)
			}
		})
	}
}

// TestNonOKStatusMapsToRecordsErrorKinds: each api.Error status the server
// can return maps to the records.Error kind that drives the CLI exit-code
// contract (spec §22): validation→2, not_found→3, conflict→4, permission→5;
// 5xx is retryable and reads as offline→6. Non-JSON bodies still surface.
func TestNonOKStatusMapsToRecordsErrorKinds(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		wantKind records.Kind
		wantMsg  string
		wantExit int
	}{
		{"validation", 400, `{"code":"validation","message":"title required"}`,
			records.KindValidation, "title required", 2},
		{"unauthorized", 401, `{"code":"permission","message":"invalid or missing token"}`,
			records.KindPermission, "invalid or missing token", 5},
		{"forbidden", 403, `{"code":"permission","message":"no access"}`,
			records.KindPermission, "no access", 5},
		{"not found", 404, `{"code":"not_found","message":"repository not registered"}`,
			records.KindNotFound, "repository not registered", 3},
		{"conflict", 409, `{"code":"conflict","message":"repository is being updated concurrently"}`,
			records.KindConflict, "updated concurrently", 4},
		{"internal", 500, `{"code":"internal","message":"push failed"}`,
			records.KindOffline, "push failed", 6},
		{"plain text body", 400, "malformed request",
			records.KindValidation, "malformed request", 2},
		{"empty body", 404, "",
			records.KindNotFound, http.StatusText(http.StatusNotFound), 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				w.Write([]byte(tc.body))
			}))
			t.Cleanup(ts.Close)

			_, err := testClient(ts.URL).Pull(context.Background(), api.PullRequest{RepositoryID: "r1"})
			if err == nil {
				t.Fatalf("status %d returned no error", tc.status)
			}
			var re *records.Error
			if !errors.As(err, &re) || re.Kind != tc.wantKind {
				t.Fatalf("error = %v, want records.Error kind %v", err, tc.wantKind)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("message %q missing %q", err.Error(), tc.wantMsg)
			}
			if code := records.ExitCode(err); code != tc.wantExit {
				t.Errorf("exit code = %d, want %d", code, tc.wantExit)
			}
		})
	}
}

// TestUnreachableRemoteIsOffline: a connection failure (not an HTTP error)
// is the offline condition, exit code 6 (spec §22) — retryable, no verdict.
func TestUnreachableRemoteIsOffline(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := ts.URL
	ts.Close() // nothing listens here anymore

	_, err := testClient(url).Pull(context.Background(), api.PullRequest{RepositoryID: "r1"})
	var re *records.Error
	if !errors.As(err, &re) || re.Kind != records.KindOffline {
		t.Fatalf("error = %v, want kind offline", err)
	}
	if code := records.ExitCode(err); code != 6 {
		t.Errorf("exit code = %d, want 6", code)
	}
}

// TestNewNormalizesBaseURLAndResolvesEnvToken: New trims the trailing slash
// (so path joins cannot double up) and resolves the token, with ARK_TOKEN
// first in the order (spec §20).
func TestNewNormalizesBaseURLAndResolvesEnvToken(t *testing.T) {
	t.Setenv("ARK_TOKEN", "env-token")
	c, err := New("https://ark.example.com/")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BaseURL != "https://ark.example.com" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", c.BaseURL)
	}
	if c.Token != "env-token" {
		t.Errorf("Token = %q, want env-token", c.Token)
	}
}

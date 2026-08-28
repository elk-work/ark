package cloud

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
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
// 5xx is retryable and reads as offline→6, unless its code says otherwise:
// repository_corrupt→8, the one condition that will still be true on the next
// attempt. Non-JSON bodies still surface.
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
		// The one 5xx that is read by its code rather than its status. A
		// service whose stored copy of a repository will not open answers 500
		// like any other server-side fault, and unlike any other it will
		// answer 500 again on every retry — so it must not land on 6, which is
		// the code a retry loop keys on (elk-work/ark#65).
		{"repository corrupt", 500,
			`{"code":"repository_corrupt","message":"repository 01R: its stored database will not open"}`,
			records.KindRemoteCorrupt, "its stored database will not open", 8},
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

// A credential is per service, not per repository, so `ark login` should be
// able to tell you the token is wrong while you are still holding it — rather
// than at the next sync, in a different repository, probably for someone else.
//
// The probe asks an authenticated route about a repository that cannot exist:
// a live token earns a not-found, a dead one earns a 401.
func TestVerifyTokenSeparatesADeadTokenFromAnUnknownRepository(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"server rejects the token", http.StatusUnauthorized,
			`{"code":"permission","message":"invalid or missing token"}`, true},
		{"token fine, throwaway repo absent", http.StatusNotFound,
			`{"code":"not_found","message":"repository not registered"}`, false},
		{"token fine, repo somehow present", http.StatusOK,
			`{"records":[],"tombstones":[],"server_revision":0}`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var sawAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				sawAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			err := VerifyToken(context.Background(), srv.URL, "candidate-token")
			if tc.wantErr && err == nil {
				t.Fatal("a rejected token must not verify — that is the whole point")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("token should have verified: %v", err)
			}
			if sawAuth != "Bearer candidate-token" {
				t.Errorf("probe sent %q; it must present the candidate, not a stored credential", sawAuth)
			}
		})
	}
}

// An unreachable server is not a bad token, and saying so would send someone
// hunting for a credential problem they do not have.
func TestVerifyTokenReportsUnreachableAsOfflineNotPermission(t *testing.T) {
	err := VerifyToken(context.Background(), "http://127.0.0.1:1", "tok")
	if err == nil {
		t.Fatal("an unreachable server should be an error")
	}
	var arkErr *records.Error
	if !errors.As(err, &arkErr) || arkErr.Kind != records.KindOffline {
		t.Errorf("got %v, want an offline error", err)
	}
}

// The 401 a client sees at sync time must point at the fix. The server's own
// wording ("invalid or missing token") cannot distinguish a stale credential
// from an absent one, and by that point the client definitely has one.
func TestRejectedCredentialErrorNamesTheRemedy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"code":"permission","message":"invalid or missing token"}`)
	}))
	defer srv.Close()

	t.Setenv("ARK_TOKEN", "stale")
	c, err := New(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	err = c.RegisterRepo(context.Background(), api.RegisterRepositoryRequest{ID: "r", Name: "r"})
	if err == nil {
		t.Fatal("expected a permission error")
	}
	if !strings.Contains(err.Error(), "ark login") {
		t.Errorf("error should tell the reader what to do, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rotated") {
		t.Errorf("error should name the likely cause, got: %v", err)
	}
}

package cloud

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// probe answers every request with one status and body, which is all
// VerifyToken's contract depends on.
func probe(t *testing.T, status int, body any) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if body != nil {
			_ = json.NewEncoder(w).Encode(body)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestVerifyTokenReadsAForbiddenAsAuthenticated is elk-work/ark#106.
//
// VerifyToken exists to tell you a credential is wrong while you are still
// holding it, and it establishes exactly one thing: that the service accepts
// the bearer. A 403 is the service saying it accepted the bearer and declined
// the request, so it is a pass, not a failure.
//
// It said the opposite. The probe id `00000000000000000000000000` is a
// well-formed ULID and so registrable, and after #52 a principal with no grant
// on a repository that exists gets a 403 — at which point `ark login` reported
// "the server rejected this token" about a credential that had just worked.
func TestVerifyTokenReadsAForbiddenAsAuthenticated(t *testing.T) {
	for _, tc := range []struct {
		name string
		body any
	}{
		{"the service names what was missing", map[string]string{
			"code":    "permission",
			"message": "b@example.com has no grant on repository 00000000000000000000000000: read access is required",
		}},
		{"and when it says nothing at all", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyToken(context.Background(), probe(t, http.StatusForbidden, tc.body), "arkc_good"); err != nil {
				t.Fatalf("a 403 was reported as a bad credential: %v", err)
			}
		})
	}
}

// The half that must not move: a bearer the service does not recognise is
// still a failed verification, which is the whole point of the command.
func TestVerifyTokenStillRefusesAnUnrecognisedBearer(t *testing.T) {
	err := VerifyToken(context.Background(), probe(t, http.StatusUnauthorized,
		map[string]string{"code": "permission", "message": "invalid or missing token"}), "arkc_stale")
	if err == nil {
		t.Fatal("a 401 verified successfully")
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code %d, want 5", code)
	}
}

// A repository the service does not hold is the ordinary answer for the
// sentinel id, and the one every first login gets.
func TestVerifyTokenAcceptsNotFound(t *testing.T) {
	if err := VerifyToken(context.Background(), probe(t, http.StatusNotFound,
		map[string]string{"code": "not_found", "message": "repository not registered"}), "arkc_good"); err != nil {
		t.Fatalf("a 404 was reported as a bad credential: %v", err)
	}
}

// The tag must not change what a 403 is to everything else. It stays a
// *records.Error of kind permission at exit 5, and keeps the service's own
// sentence — otherwise fixing VerifyToken would have broken #95's message and
// every caller that switches on Kind.
func TestTaggingAForbiddenChangesNothingElseAboutIt(t *testing.T) {
	err := statusError(http.StatusForbidden, api.Error{
		Code: "permission", Message: "no grant on repository X"})

	if !errors.Is(err, ErrAuthorizationRefused) {
		t.Error("a 403 is not matchable as an authorization refusal")
	}
	var arkErr *records.Error
	if !errors.As(err, &arkErr) {
		t.Fatal("errors.As no longer finds the records.Error inside a 403")
	}
	if arkErr.Kind != records.KindPermission {
		t.Errorf("kind %v, want permission", arkErr.Kind)
	}
	if code := records.ExitCode(err); code != 5 {
		t.Errorf("exit code %d, want 5", code)
	}
	if got := err.Error(); got != arkErr.Error() {
		t.Errorf("the wrapper changed the message:\n got %q\nwant %q", got, arkErr.Error())
	}

	// And a 401 is deliberately not tagged: it is the case where logging in
	// again is the remedy.
	if unauth := statusError(http.StatusUnauthorized, api.Error{Message: "invalid or missing token"}); errors.Is(unauth, ErrAuthorizationRefused) {
		t.Error("a 401 is being reported as an authorization refusal")
	}
}

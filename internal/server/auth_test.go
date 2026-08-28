package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

const testBootstrap = "test-bootstrap-token"

// authServer is newTestServer plus the directories the credential store lives
// in, so a test can look at auth.db the way an operator would — it is a file
// in a bucket (RFC-0001) — and can stand a second instance beside it.
type authServer struct {
	*Server
	backendDir string
	cacheDir   string
}

func newAuthServer(t *testing.T) *authServer {
	t.Helper()
	backendDir, cacheDir := t.TempDir(), t.TempDir()
	return &authServer{
		Server: &Server{
			Repos:          repodb.NewManager(&repodb.LocalBackend{Dir: backendDir}, cacheDir),
			Token:          "test-token",
			BootstrapToken: testBootstrap,
			Blobs:          &LocalBlobStore{Dir: t.TempDir(), BaseURL: "http://unused"},
		},
		backendDir: backendDir,
		cacheDir:   cacheDir,
	}
}

// authDBPath is where the reserved key lands on a LocalBackend.
func (a *authServer) authDBPath() string {
	return filepath.Join(a.backendDir, authDBKey+".db")
}

// otherInstance is a second ark-server's view of the same auth.db: its own
// cache, its own TTL. Revoking through it is how a test reproduces "somebody
// else revoked this credential a moment ago".
func (a *authServer) otherInstance(t *testing.T) *authStore {
	t.Helper()
	return newAuthStore(a.Repos.Backend, t.TempDir(), nil)
}

// doRequestAs is doRequest with a bearer of the caller's choosing.
func doRequestAs(t *testing.T, s *Server, bearer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// mintCredentialFor runs the bootstrap route and returns what it handed back.
func mintCredentialFor(t *testing.T, a *authServer, email string) api.CreatePrincipalResponse {
	t.Helper()
	rec := doRequestAs(t, a.Server, testBootstrap, "POST", "/v1/principals",
		fmt.Sprintf(`{"email":%q,"display_name":"Test Person"}`, email))
	if rec.Code != 200 {
		t.Fatalf("create principal: %d %s", rec.Code, rec.Body.String())
	}
	var out api.CreatePrincipalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("create principal response: %v", err)
	}
	return out
}

// readAuthDB opens the stored auth.db read-only, for assertions about what the
// service actually persisted.
func readAuthDB(t *testing.T, a *authServer, fn func(db *sql.DB)) {
	t.Helper()
	db, err := openAuthDB(a.authDBPath())
	if err != nil {
		t.Fatalf("open auth.db: %v", err)
	}
	defer db.Close()
	fn(db)
}

// The bar for RFC-0003 Stage 1 is that existing clients cannot tell the
// difference. With a service token set and no principals in existence, the
// service must behave exactly as it did before auth.db existed — including
// never going near auth.db, because the credential store is a new single point
// of contention and the path that already worked must not acquire one.
func TestLegacyBearerIsUnchangedAndNeverTouchesAuthDB(t *testing.T) {
	var logged bytes.Buffer
	a := newAuthServer(t)
	a.Log = slog.New(slog.NewJSONHandler(&logged, nil))

	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register with the service token: %d %s", rec.Code, rec.Body.String())
	}
	for _, bearer := range []string{"", "wrong", "arkc_not-a-real-credential"} {
		rec := doRequestAs(t, a.Server, bearer, "POST", "/v1/sync/pull", `{}`)
		if rec.Code != 401 {
			t.Errorf("bearer %q: %d, want 401", bearer, rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "invalid or missing token") {
			t.Errorf("bearer %q: message changed: %s", bearer, rec.Body.String())
		}
	}

	// No principals were minted, so nothing should have written auth.db.
	if _, err := os.Stat(a.authDBPath()); !os.IsNotExist(err) {
		t.Errorf("auth.db exists on a deployment that never bootstrapped (err = %v)", err)
	}

	// And the legacy path is logged, which is the entire answer to #54's "who
	// has not moved yet?" for a token nobody can attribute any other way.
	if !strings.Contains(logged.String(), `"principal":"legacy"`) {
		t.Errorf("the legacy path is not visible in the logs:\n%s", logged.String())
	}
}

// The bootstrap route is what makes per-principal credentials work with no
// identity provider anywhere: one random string the operator sets, exchanged
// for a credential of your own. What it must never do is keep the credential.
func TestBootstrapMintsACredentialAndStoresOnlyItsHash(t *testing.T) {
	a := newAuthServer(t)
	got := mintCredentialFor(t, a, "me@example.com")

	if !got.Created {
		t.Error("first principal for an email should report created")
	}
	if !strings.HasPrefix(got.Token, "arkc_") {
		t.Errorf("token = %q, want an arkc_ prefix", got.Token)
	}
	if got.Principal.ID == "" || got.Principal.Email != "me@example.com" || got.Principal.Kind != "human" {
		t.Errorf("principal = %+v", got.Principal)
	}
	if got.ExpiresAt == "" {
		t.Error("a credential with no expiry is not what RFC-0003 specifies")
	}

	readAuthDB(t, a, func(db *sql.DB) {
		var stored, principalID string
		if err := db.QueryRow(`SELECT token_sha256, principal_id FROM credentials`).
			Scan(&stored, &principalID); err != nil {
			t.Fatalf("read credential: %v", err)
		}
		if stored != hashCredential(got.Token) {
			t.Errorf("stored digest does not match the issued credential")
		}
		if principalID != got.Principal.ID {
			t.Errorf("credential is bound to %q, not to the principal returned", principalID)
		}
	})

	// The strongest form of "never stored in the clear": the bytes are not in
	// the file at all.
	raw, err := os.ReadFile(a.authDBPath())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(got.Token)) {
		t.Error("the credential itself is in auth.db")
	}

	// Reissuing for a known email is the break-glass, and it must reuse the
	// principal rather than mint a second one for the same person.
	again := mintCredentialFor(t, a, "me@example.com")
	if again.Created {
		t.Error("a second credential for a known email should not report a new principal")
	}
	if again.Principal.ID != got.Principal.ID {
		t.Errorf("email %q resolved to two principals: %s and %s",
			"me@example.com", got.Principal.ID, again.Principal.ID)
	}
	if again.Token == got.Token || again.CredentialID == got.CredentialID {
		t.Error("reissue returned the same credential")
	}
	for _, tok := range []string{got.Token, again.Token} {
		if rec := doRequestAs(t, a.Server, tok, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
			t.Errorf("credential rejected: %s", rec.Body.String())
		}
	}
}

// ARK_BOOTSTRAP_TOKEN is accepted on POST /v1/principals and nowhere else, and
// nothing else is accepted there. Both halves matter: the first keeps a
// bootstrap secret from becoming a second service token, the second keeps
// anyone holding a credential from minting more of them (that is a grant, and
// grants are elk-work/ark#52).
func TestBootstrapTokenIsAcceptedOnNoOtherRouteAndNothingElseOnThatOne(t *testing.T) {
	a := newAuthServer(t)
	cred := mintCredentialFor(t, a, "me@example.com")

	for _, path := range []string{"/v1/sync/pull", "/v1/sync/push", "/v1/repositories"} {
		if rec := doRequestAs(t, a.Server, testBootstrap, "POST", path, `{}`); rec.Code != 401 {
			t.Errorf("the bootstrap token authenticated %s: %d", path, rec.Code)
		}
	}
	for name, bearer := range map[string]string{
		"the service token": a.Token,
		"a credential":      cred.Token,
		"nothing":           "",
	} {
		rec := doRequestAs(t, a.Server, bearer, "POST", "/v1/principals", `{"email":"x@example.com"}`)
		if rec.Code != 401 {
			t.Errorf("%s minted a principal: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

// A deployment that has not opted in must not have a mint route at all.
func TestBootstrapIsRefusedWhenUnconfigured(t *testing.T) {
	a := newAuthServer(t)
	a.BootstrapToken = ""
	rec := doRequestAs(t, a.Server, "anything", "POST", "/v1/principals", `{"email":"x@example.com"}`)
	if rec.Code != 401 {
		t.Fatalf("code = %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ARK_BOOTSTRAP_TOKEN") {
		t.Errorf("the refusal should name what is unset: %s", rec.Body.String())
	}
}

func TestCreatePrincipalValidation(t *testing.T) {
	a := newAuthServer(t)
	for name, body := range map[string]string{
		"no email":      `{"display_name":"X"}`,
		"blank email":   `{"email":"   "}`,
		"unknown kind":  `{"email":"x@example.com","kind":"robot"}`,
		"not even JSON": `{`,
	} {
		rec := doRequestAs(t, a.Server, testBootstrap, "POST", "/v1/principals", body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
}

// The acceptance bullet, stated as an equivalence rather than as a list of
// status codes: a credential must reach every route the legacy token reaches,
// and get the same answer there.
//
// Since grants landed (elk-work/ark#52) the equivalence needs a level to hold
// at: the legacy token carries implicit admin everywhere, so the credential it
// is compared against is one holding `admin` on this repository. Without a
// grant the answer is 403 on every one of these routes, which is
// TestAPrincipalWithNoGrantIsRefusedEverywhere.
func TestACredentialAuthenticatesEveryRouteTheLegacyTokenDoes(t *testing.T) {
	a := newAuthServer(t)
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	cred := mintCredentialFor(t, a, "me@example.com")
	grantTo(t, a, repoID, "me@example.com", api.GrantAdmin)

	routes := []struct{ method, path, body string }{
		{"POST", "/v1/repositories", fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)},
		{"POST", "/v1/sync/push", fmt.Sprintf(`{"repository_id":%q,"client_id":"c1"}`, repoID)},
		{"POST", "/v1/sync/pull", fmt.Sprintf(`{"repository_id":%q}`, repoID)},
		{"GET", "/v1/repositories/" + repoID, ""},
		{"POST", "/v1/repositories/" + repoID + "/metadata", `{"name":"test"}`},
		{"GET", "/v1/repositories/" + repoID + "/records/task/01NOSUCHTASK00000000000000", ""},
		{"POST", "/v1/repositories/" + repoID + "/tasks", `{"title":"t"}`},
		{"POST", "/v1/repositories/" + repoID + "/comments", `{"body":"b"}`},
		{"POST", "/v1/repositories/" + repoID + "/tasks/1/status", `{"status":"done"}`},
		{"POST", "/v1/pull-requests/01NOSUCHPR000000000000000/merge", fmt.Sprintf(`{"repository_id":%q}`, repoID)},
		{"POST", "/v1/artifacts/upload-url", fmt.Sprintf(`{"repository_id":%q}`, repoID)},
		{"POST", "/v1/artifacts/confirm", fmt.Sprintf(`{"repository_id":%q}`, repoID)},
		{"POST", "/v1/artifacts/download-url", fmt.Sprintf(`{"repository_id":%q}`, repoID)},
	}
	for _, r := range routes {
		legacy := doRequestAs(t, a.Server, a.Token, r.method, r.path, r.body)
		credentialed := doRequestAs(t, a.Server, cred.Token, r.method, r.path, r.body)
		if credentialed.Code == 401 {
			t.Errorf("%s %s: credential refused where the service token was not: %s",
				r.method, r.path, credentialed.Body.String())
			continue
		}
		if legacy.Code != credentialed.Code {
			t.Errorf("%s %s: service token %d, credential %d",
				r.method, r.path, legacy.Code, credentialed.Code)
		}
	}
}

// Revocation is eventually consistent, bounded at 60 seconds — RFC-0003 says
// so and accepts it. That is only a defensible cost if the bound holds, so
// this pins both halves: still valid inside the window, refused after it,
// with no restart and no request to the instance that did the revoking.
func TestRevokedExpiredAndDisabledAreRefusedWithinTheTTL(t *testing.T) {
	cases := map[string]struct {
		change func(t *testing.T, other *authStore, cred api.CreatePrincipalResponse)
		want   string
	}{
		"revoked": {
			change: func(t *testing.T, other *authStore, cred api.CreatePrincipalResponse) {
				mustUpdateAuth(t, other, `UPDATE credentials SET revoked_at = ? WHERE id = ?`,
					records.Now(), cred.CredentialID)
			},
			want: "credential revoked",
		},
		"principal disabled": {
			change: func(t *testing.T, other *authStore, cred api.CreatePrincipalResponse) {
				mustUpdateAuth(t, other, `UPDATE principals SET disabled_at = ? WHERE id = ?`,
					records.Now(), cred.Principal.ID)
			},
			want: "principal disabled",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a := newAuthServer(t)
			cred := mintCredentialFor(t, a, "me@example.com")

			at := time.Now()
			store := a.authStore()
			store.now = func() time.Time { return at }
			// Load the cache under the pinned clock.
			if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
				t.Fatalf("credential refused before anything changed: %s", rec.Body.String())
			}

			tc.change(t, a.otherInstance(t), cred)

			at = at.Add(authTTL - time.Second)
			if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
				t.Errorf("inside the TTL the cached answer should still stand: %s", rec.Body.String())
			}

			at = at.Add(2 * time.Second)
			rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", `{}`)
			if rec.Code != 401 {
				t.Fatalf("still accepted %v after the change: %d", authTTL, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("message %s does not name the cause %q", rec.Body.String(), tc.want)
			}
		})
	}

	// Expiry needs no second instance: the credential simply outlives its own
	// year, which is also the only case the holder can fix alone.
	t.Run("expired", func(t *testing.T) {
		a := newAuthServer(t)
		cred := mintCredentialFor(t, a, "me@example.com")

		at := time.Now()
		store := a.authStore()
		store.now = func() time.Time { return at }
		if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
			t.Fatalf("credential refused while valid: %s", rec.Body.String())
		}

		at = at.Add(credentialLifetime + time.Hour)
		rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", `{}`)
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), "credential expired") {
			t.Fatalf("an expired credential was accepted: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// mustUpdateAuth runs one statement against auth.db through a store's own CAS
// path, so the write is exactly what another instance's write would be.
func mustUpdateAuth(t *testing.T, store *authStore, query string, args ...any) {
	t.Helper()
	err := store.update(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	})
	if err != nil {
		t.Fatalf("update auth.db: %v", err)
	}
}

// auth.db is written with the same compare-and-swap as a repository database,
// so a lost race must be survivable by replay rather than by locking. The
// competing write happens *inside* the closure, which is precisely when the
// generation the caller fetched goes stale.
func TestAuthDBSurvivesALostCASRace(t *testing.T) {
	a := newAuthServer(t)
	mintCredentialFor(t, a, "first@example.com") // so auth.db exists
	mine, theirs := a.authStore(), a.otherInstance(t)

	ctx := context.Background()
	raced := false
	attempts := 0
	err := mine.update(ctx, func(tx *sql.Tx) error {
		attempts++
		if !raced {
			raced = true
			// Distinct mtimes are what LocalBackend uses as generations.
			time.Sleep(2 * time.Millisecond)
			mustUpdateAuth(t, theirs, `INSERT INTO principals (id, kind, email, created_at)
				VALUES ('01THEIRS0000000000000000A', 'human', 'theirs@example.com', ?)`, records.Now())
		}
		_, err := tx.Exec(`INSERT INTO principals (id, kind, email, created_at)
			VALUES ('01MINE00000000000000000AA', 'human', 'mine@example.com', ?)
			ON CONFLICT (id) DO NOTHING`, records.Now())
		return err
	})
	if err != nil {
		t.Fatalf("update did not survive the race: %v", err)
	}
	if attempts < 2 {
		t.Fatalf("the closure ran %d time(s); the race did not happen", attempts)
	}

	readAuthDB(t, a, func(db *sql.DB) {
		for _, email := range []string{"first@example.com", "theirs@example.com", "mine@example.com"} {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM principals WHERE email = ?`, email).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 1 {
				t.Errorf("principal %s appears %d times; the replay lost or duplicated a write", email, n)
			}
		}
	})
}

// last_used_on is what tells elk-work/ark#54 who has moved to a credential
// before the legacy token is switched off. It is deliberately coarse — a day,
// batched — because the alternative is a write on every request.
func TestLastUsedOnIsRecordedByDayAndNotOnEveryRequest(t *testing.T) {
	a := newAuthServer(t)
	cred := mintCredentialFor(t, a, "me@example.com")
	store := a.authStore()

	if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
		t.Fatalf("credential refused: %s", rec.Body.String())
	}
	if len(store.usage) != 1 {
		t.Fatalf("a first use queued %d updates, want 1", len(store.usage))
	}
	if err := store.flushUsage(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	today := time.Now().UTC().Format(time.DateOnly)
	readAuthDB(t, a, func(db *sql.DB) {
		var got string
		if err := db.QueryRow(`SELECT COALESCE(last_used_on, '') FROM credentials WHERE id = ?`,
			cred.CredentialID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != today {
			t.Errorf("last_used_on = %q, want %q", got, today)
		}
	})

	// A second use on the same day must queue nothing at all: the point of day
	// granularity is that a busy client writes once, not once per request.
	for i := 0; i < 3; i++ {
		doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", `{}`)
	}
	if len(store.usage) != 0 {
		t.Errorf("same-day requests queued %d writes, want 0", len(store.usage))
	}
}

// Handlers downstream have to be able to ask who is calling — that is what
// elk-work/ark#52 will consult for grants, and what makes the legacy token's
// implicit authority explicit rather than merely absent.
func TestThePrincipalReachesTheHandler(t *testing.T) {
	a := newAuthServer(t)
	cred := mintCredentialFor(t, a, "me@example.com")

	var seen *authenticated
	handler := a.auth(func(w http.ResponseWriter, r *http.Request) {
		who, ok := principalFrom(r.Context())
		if !ok {
			t.Error("no principal on the request context")
		}
		seen = who
	})

	handler(httptest.NewRecorder(), authedRequest(a.Token))
	if seen == nil || !seen.Legacy || seen.ID != legacyPrincipalID {
		t.Fatalf("service token resolved to %+v, want the legacy principal", seen)
	}

	handler(httptest.NewRecorder(), authedRequest(cred.Token))
	if seen == nil || seen.Legacy || seen.ID != cred.Principal.ID || seen.Email != "me@example.com" {
		t.Fatalf("credential resolved to %+v, want principal %s", seen, cred.Principal.ID)
	}
	if seen.CredentialID != cred.CredentialID {
		t.Errorf("credential id = %q, want %q", seen.CredentialID, cred.CredentialID)
	}
}

func authedRequest(bearer string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/sync/pull", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+bearer)
	return req
}

// brokenBackend fails every operation, standing in for object storage the
// service cannot reach.
type brokenBackend struct{}

var errBackendDown = errors.New("backend unreachable")

func (brokenBackend) Fetch(context.Context, string, string) (int64, error) {
	return 0, errBackendDown
}
func (brokenBackend) Store(context.Context, string, string, int64) (int64, error) {
	return 0, errBackendDown
}

// auth.db is a new failure domain, and RFC-0003 accepts that a corrupt one
// locks credential holders out. What it must not do is lock out the legacy
// bearer — the whole fleet — or report a storage outage as a bad credential,
// which would send everyone off to rotate a token that is fine.
func TestAnUnreachableAuthDBDoesNotBreakTheLegacyBearer(t *testing.T) {
	s := &Server{
		Repos: repodb.NewManager(brokenBackend{}, t.TempDir()),
		Token: "test-token",
		Blobs: &LocalBlobStore{Dir: t.TempDir(), BaseURL: "http://unused"},
	}
	// The legacy path never reads auth.db, so a dead backend is invisible to
	// it. (Pull still fails on the repository database, which is the point:
	// it fails as a repository error, not as an authentication one.)
	rec := doRequestAs(t, s, s.Token, "POST", "/v1/sync/pull", `{}`)
	if rec.Code == 401 {
		t.Errorf("a dead credential store rejected the service token: %s", rec.Body.String())
	}

	rec = doRequestAs(t, s, "arkc_something", "POST", "/v1/sync/pull", `{}`)
	if rec.Code != 500 {
		t.Errorf("a credential store outage was reported as %d; it must not read as a bad credential", rec.Code)
	}
}

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

// RFC-0003 Stage 4 (elk-work/ark#54): ARK_API_TOKEN unset.
//
// The acceptance line is "with ARK_API_TOKEN unset the service starts, serves,
// and rejects the old token", and each clause is a test here. "Starts" is
// cmd/ark-server's half (main_test.go); a Server with an empty Token is what it
// then builds, and this file is what that server does.

// retiredToken is the string the fleet held before the cutover — the Token a
// newAuthServer is built with — and so the one a forgotten machine will still
// present after it.
const retiredToken = "test-token"

// retiredServer is a service that has cut over: no service token, a bootstrap
// token, and one principal who registered a repository and so administers it.
// The setup never touches the legacy path, because on a retired service there
// is none to touch.
func retiredServer(t *testing.T) (*authServer, api.CreatePrincipalResponse, *bytes.Buffer) {
	t.Helper()
	a := newAuthServer(t)
	if a.Token != retiredToken {
		t.Fatalf("newAuthServer's token is %q; retiredToken must name it", a.Token)
	}
	a.Token = ""
	logged := &bytes.Buffer{}
	a.Log = slog.New(slog.NewJSONHandler(logged, nil))

	cred := mintCredentialFor(t, a, "me@example.com")
	if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register as the first principal: %d %s", rec.Code, rec.Body.String())
	}
	return a, cred, logged
}

// "Serves": everything a principal did before the cutover still works after
// it, and the unauthenticated routes a load balancer and `ark login` depend on
// are still there.
func TestNoServiceTokenServesPrincipals(t *testing.T) {
	a, cred, logged := retiredServer(t)

	for _, path := range []string{"/", "/health"} {
		if rec := doRequestAs(t, a.Server, "", "GET", path, ""); rec.Code != 200 {
			t.Errorf("GET %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	pushBody, err := json.Marshal(api.PushRequest{RepositoryID: repoID, ClientID: "c1",
		Actors: []api.Actor{{ID: humanID, Type: "human", Name: "Me", Email: "me@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []struct{ name, method, path, body string }{
		{"re-register", "POST", "/v1/repositories", fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)},
		{"push", "POST", "/v1/sync/push", string(pushBody)},
		{"pull", "POST", "/v1/sync/pull", `{"repository_id":"` + repoID + `"}`},
		{"get repository", "GET", "/v1/repositories/" + repoID, ""},
		{"list grants", "GET", "/v1/repositories/" + repoID + "/grants", ""},
		// The first principal on a service with no operator became one, so
		// the service-wide acts are reachable without the shared token too.
		{"list principals", "GET", "/v1/principals", ""},
	} {
		if rec := doRequestAs(t, a.Server, cred.Token, call.method, call.path, call.body); rec.Code != 200 {
			t.Errorf("%s as a principal: %d %s", call.name, rec.Code, rec.Body.String())
		}
	}

	if strings.Contains(logged.String(), `"principal":"legacy"`) {
		t.Errorf("a service with no token authenticated somebody as legacy:\n%s", logged.String())
	}
}

// "Rejects the old token": not refused as a legacy bearer, but not recognised
// at all — the same 401 and the same words as a string nobody ever configured,
// on every route. Anything else would mean a comparison is still being made
// against something.
func TestNoServiceTokenRejectsTheOldToken(t *testing.T) {
	a, _, logged := retiredServer(t)

	routes := append([]struct{ name, method, path, body, needs string }{},
		everyRepositoryRoute...)
	routes = append(routes,
		struct{ name, method, path, body, needs string }{"register", "POST", "/v1/repositories",
			fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID), ""},
		struct{ name, method, path, body, needs string }{"list principals", "GET", "/v1/principals", "", ""},
		struct{ name, method, path, body, needs string }{"list credentials", "GET", "/v1/credentials", "", ""},
	)
	for _, route := range routes {
		rec := doRequestAs(t, a.Server, retiredToken, route.method, route.path, route.body)
		if rec.Code != 401 {
			t.Errorf("%s with the retired token: %d %s, want 401", route.name, rec.Code, rec.Body.String())
			continue
		}
		if got := errCode(t, rec); got != "permission" {
			t.Errorf("%s: error code %q, want permission", route.name, got)
		}
		if !strings.Contains(rec.Body.String(), "invalid or missing token") {
			t.Errorf("%s: the retired token is answered differently from any other bad bearer: %s",
				route.name, rec.Body.String())
		}
	}

	// The bootstrap route has its own token and never took the service token;
	// it must not start now.
	if rec := doRequestAs(t, a.Server, retiredToken, "POST", "/v1/principals",
		`{"email":"someone@example.com"}`); rec.Code == 200 {
		t.Errorf("the retired token minted a principal: %s", rec.Body.String())
	}

	if strings.Contains(logged.String(), `"principal":"legacy"`) {
		t.Errorf("the retired token was logged as the legacy principal:\n%s", logged.String())
	}
}

// The trap an empty Token sets: if the legacy comparison ran against "", a
// request carrying `Authorization: Bearer ` — or none at all — would compare
// equal to it and be handed implicit admin on every repository. That is the
// one way making the token optional could open the service rather than close
// it, so it is tested directly and on the write it would matter most for.
func TestNoServiceTokenIsNotAnEmptyBearer(t *testing.T) {
	a, _, _ := retiredServer(t)

	for _, header := range []string{"", "Bearer ", "Bearer", " "} {
		req := httptest.NewRequest("POST", "/v1/sync/push",
			strings.NewReader(`{"repository_id":"`+repoID+`","client_id":"c1","mutations":[]}`))
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		rec := httptest.NewRecorder()
		a.Handler().ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Errorf("Authorization %q: %d %s, want 401", header, rec.Code, rec.Body.String())
		}
	}
}

// With no token there is no dial: whatever the struct carries, the mode every
// decision reads is `off`. cmd/ark-server refuses to start with an explicit
// `full` or `readonly` and no token (ResolveLegacyMode), so this is the
// struct-literal half — the one every test and embedder builds.
func TestNoServiceTokenIsLegacyModeOff(t *testing.T) {
	for _, configured := range []string{"", LegacyModeFull, LegacyModeReadonly, LegacyModeOff} {
		s := &Server{LegacyMode: configured}
		if got := s.legacyMode(); got != LegacyModeOff {
			t.Errorf("LegacyMode %q with no Token: mode %q, want off", configured, got)
		}
		if s.legacyAccepted() {
			t.Errorf("LegacyMode %q with no Token: the legacy branch is registered", configured)
		}
	}
	// And a service that still has one is untouched by this.
	if got := (&Server{Token: "t"}).legacyMode(); got != LegacyModeFull {
		t.Errorf("a service with a token and no mode: %q, want full", got)
	}
}

func TestResolveLegacyMode(t *testing.T) {
	for _, ok := range []struct {
		in       string
		tokenSet bool
		want     string
	}{
		// With a token it is exactly ParseLegacyMode.
		{"", true, LegacyModeFull},
		{LegacyModeFull, true, LegacyModeFull},
		{LegacyModeReadonly, true, LegacyModeReadonly},
		{LegacyModeOff, true, LegacyModeOff},
		// Without one the only position is off.
		{"", false, LegacyModeOff},
		{LegacyModeOff, false, LegacyModeOff},
	} {
		got, err := ResolveLegacyMode(ok.in, ok.tokenSet)
		if err != nil || got != ok.want {
			t.Errorf("ResolveLegacyMode(%q, %v) = %q, %v; want %q", ok.in, ok.tokenSet, got, err, ok.want)
		}
	}
	// An explicit position the service cannot honour is refused, in words
	// that name both variables — the likeliest cause is a dropped secret, and
	// the message has to lead there.
	for _, mode := range []string{LegacyModeFull, LegacyModeReadonly} {
		got, err := ResolveLegacyMode(mode, false)
		if err == nil {
			t.Errorf("ResolveLegacyMode(%q, no token) = %q; want an error", mode, got)
			continue
		}
		for _, name := range []string{"ARK_LEGACY_TOKEN", "ARK_API_TOKEN"} {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("ResolveLegacyMode(%q, no token): error does not name %s: %v", mode, name, err)
			}
		}
	}
	// A typo is still a typo, token or not.
	if _, err := ResolveLegacyMode("read-only", false); err == nil {
		t.Error("ResolveLegacyMode(read-only, no token) accepted a value that is none of the three")
	}
}

// ARK_SIGNING_KEY taking over the token's other job, end to end: with no
// service token, local-mode artifact URLs are signed and verified by the
// signing key alone, uploaded and read back by a principal, and a signature
// made with the retired token does not verify. Without the key there is
// nothing to sign with, and the routes refuse rather than open.
func TestNoServiceTokenBlobURLsUseTheSigningKey(t *testing.T) {
	serve := func(t *testing.T, signingKey string) (*authServer, api.CreatePrincipalResponse, string) {
		t.Helper()
		a, cred, _ := retiredServer(t)
		a.SigningKey = signingKey
		ts := httptest.NewServer(a.Handler())
		t.Cleanup(ts.Close)
		a.Blobs.(*LocalBlobStore).BaseURL = ts.URL
		return a, cred, ts.URL
	}

	t.Run("ARK_SIGNING_KEY set", func(t *testing.T) {
		const signingKey = "a-signing-key-with-no-service-token-beside-it"
		a, cred, base := serve(t, signingKey)

		content := []byte("p95 41ms")
		digest := sha256Hex(content)
		rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/artifacts/upload-url",
			fmt.Sprintf(`{"repository_id":%q,"sha256":%q,"size_bytes":%d}`, repoID, digest, len(content)))
		if rec.Code != 200 {
			t.Fatalf("upload-url as a principal: %d %s", rec.Code, rec.Body.String())
		}
		var up api.UploadURLResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &up); err != nil {
			t.Fatal(err)
		}
		if code := putBlob(t, up.URL, content); code != http.StatusOK {
			t.Fatalf("put to the signed URL: %d", code)
		}
		if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/artifacts/confirm",
			fmt.Sprintf(`{"repository_id":%q,"sha256":%q}`, repoID, digest)); rec.Code != 200 {
			t.Fatalf("confirm as a principal: %d %s", rec.Code, rec.Body.String())
		}
		getURL, err := a.Blobs.SignedGetURL(context.Background(), blobKey(digest))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(getURL)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
			t.Fatalf("signed GET: %d %q", resp.StatusCode, got)
		}

		if code := getStatus(t, signedGet(base, signingKey, blobKey(digest))); code != http.StatusOK {
			t.Errorf("a signing-key signature was refused: %d", code)
		}
		if code := getStatus(t, signedGet(base, retiredToken, blobKey(digest))); code != http.StatusForbidden {
			t.Errorf("the retired token still signs valid URLs: %d, want 403", code)
		}
	})

	t.Run("neither, and the routes refuse rather than open", func(t *testing.T) {
		a, cred, base := serve(t, "")
		if got := a.signingKey(); got != "" {
			t.Fatalf("signing key with neither configured = %q, want empty", got)
		}
		rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/artifacts/upload-url",
			fmt.Sprintf(`{"repository_id":%q,"sha256":%q,"size_bytes":1}`, repoID, strings.Repeat("a", 64)))
		if rec.Code == 200 {
			t.Errorf("minted an upload URL with no key to sign it: %s", rec.Body.String())
		}
		// An HMAC under the empty key is something anyone can compute. It
		// must be refused, or "no key" would mean "no lock".
		key := "sha256/aa/" + strings.Repeat("a", 64)
		if code := getStatus(t, signedGet(base, "", key)); code != http.StatusForbidden {
			t.Errorf("an empty-key signature was served: %d, want 403", code)
		}
	})
}

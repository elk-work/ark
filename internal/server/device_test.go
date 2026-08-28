package server

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

const (
	testApprovalURL = "https://idp.example.com/ark-auth"
	testIDPKey      = "test-idp-key"
)

// newDeviceServer is newAuthServer with an identity provider configured — the
// deployment that offers a device login. The approver in these tests is the
// approve route itself, called in process: no external service and no network,
// which is what keeps this slice landable on its own.
func newDeviceServer(t *testing.T) *authServer {
	t.Helper()
	a := newAuthServer(t)
	a.IDPApprovalURL = testApprovalURL
	a.IDPKey = testIDPKey
	return a
}

// pinClock fixes the service's clock so a test can step it deliberately. Both
// numbers this flow turns on are times — a five-second poll interval and a
// fifteen-minute code — and waiting either out for real would put them into
// the suite's runtime.
func pinClock(t *testing.T, a *authServer) *time.Time {
	t.Helper()
	at := time.Now()
	a.authStore().now = func() time.Time { return at }
	return &at
}

func requestDeviceCode(t *testing.T, a *authServer) api.DeviceCodeResponse {
	t.Helper()
	rec := doRequestAs(t, a.Server, "", "POST", "/v1/device/code", `{}`)
	if rec.Code != 200 {
		t.Fatalf("device code: %d %s", rec.Code, rec.Body.String())
	}
	var out api.DeviceCodeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("device code response: %v", err)
	}
	return out
}

func approve(t *testing.T, a *authServer, bearer string, req api.DeviceApproveRequest) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return doRequestAs(t, a.Server, bearer, "POST", "/v1/device/approve", string(body))
}

func poll(t *testing.T, a *authServer, deviceCode string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestAs(t, a.Server, "", "POST", "/v1/device/token",
		fmt.Sprintf(`{"device_code":%q}`, deviceCode))
}

// errorCode reads the `code` beside a device-flow status. The status carries
// the answer and the code names it; a client keys on either.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e api.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("error body %q: %v", rec.Body.String(), err)
	}
	return e.Code
}

// The flow end to end, and the properties that make it safe to return a
// credential over an unauthenticated route: the device code is stored as a
// digest, and the pending row is gone once it has been redeemed.
func TestDeviceLoginMintsAWorkingCredential(t *testing.T) {
	a := newDeviceServer(t)
	at := pinClock(t, a)

	code := requestDeviceCode(t, a)
	if code.ExpiresIn != int(deviceCodeTTL.Seconds()) || code.Interval != int(devicePollInterval.Seconds()) {
		t.Errorf("expires_in = %d, interval = %d; want %d and %d",
			code.ExpiresIn, code.Interval, int(deviceCodeTTL.Seconds()), int(devicePollInterval.Seconds()))
	}
	if code.VerificationURI != testApprovalURL {
		t.Errorf("verification_uri = %q, want the configured approval URL", code.VerificationURI)
	}
	if !strings.Contains(code.VerificationURIComplete, "user_code=") {
		t.Errorf("verification_uri_complete = %q carries no code", code.VerificationURIComplete)
	}

	// Stored hashed, like the credential itself: a leaked pending-code store
	// must not be redeemable.
	readAuthDB(t, a, func(db *sql.DB) {
		var stored, userCode string
		if err := db.QueryRow(`SELECT device_code_sha256, user_code FROM device_codes`).
			Scan(&stored, &userCode); err != nil {
			t.Fatal(err)
		}
		if stored == code.DeviceCode {
			t.Error("the device code is stored in the clear")
		}
		if stored != hashCredential(code.DeviceCode) {
			t.Errorf("stored digest %q is not the device code's SHA-256", stored)
		}
		if displayUserCode(userCode) != code.UserCode {
			t.Errorf("stored user code %q does not render as %q", userCode, code.UserCode)
		}
	})

	if rec := poll(t, a, code.DeviceCode); rec.Code != 428 || errorCode(t, rec) != api.DeviceCodePending {
		t.Fatalf("an unapproved code answered %d %s", rec.Code, rec.Body.String())
	}

	if rec := approve(t, a, testIDPKey, api.DeviceApproveRequest{
		UserCode:    code.UserCode,
		Subject:     "idp-subject-1",
		Email:       "me@example.com",
		DisplayName: "Me",
	}); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}

	*at = at.Add(devicePollInterval)
	rec := poll(t, a, code.DeviceCode)
	if rec.Code != 200 {
		t.Fatalf("poll after approval: %d %s", rec.Code, rec.Body.String())
	}
	var out api.DeviceTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.Token, credentialPrefix) {
		t.Errorf("token %q is not an Ark credential", out.Token)
	}
	if out.Principal.Email != "me@example.com" || out.Principal.DisplayName != "Me" ||
		out.Principal.Kind != "human" || out.Principal.ID == "" {
		t.Errorf("principal = %+v", out.Principal)
	}

	// The credential is a credential like any other — that is the whole
	// claim: nothing downstream had to learn a new shape.
	if rec := doRequestAs(t, a.Server, out.Token, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
		t.Errorf("the minted credential was refused: %s", rec.Body.String())
	}

	readAuthDB(t, a, func(db *sql.DB) {
		var pending int
		if err := db.QueryRow(`SELECT count(*) FROM device_codes`).Scan(&pending); err != nil {
			t.Fatal(err)
		}
		if pending != 0 {
			t.Errorf("%d pending rows survived redemption", pending)
		}
		var label, issuer, subject string
		if err := db.QueryRow(`SELECT c.label, p.issuer, p.subject FROM credentials c
			JOIN principals p ON p.id = c.principal_id WHERE p.email = ?`,
			"me@example.com").Scan(&label, &issuer, &subject); err != nil {
			t.Fatal(err)
		}
		if label != deviceCredentialLabel {
			t.Errorf("credential label = %q, want %q", label, deviceCredentialLabel)
		}
		if issuer != "idp.example.com" || subject != "idp-subject-1" {
			t.Errorf("issuer = %q, subject = %q", issuer, subject)
		}
	})
}

// Redemption is one-shot, and that is what makes possession of the device
// code sufficient authentication: the 200 deletes the pending row in the same
// transaction, so the second holder of a replayed poll gets nothing.
func TestAReplayedPollGetsExpiredRatherThanASecondCredential(t *testing.T) {
	a := newDeviceServer(t)
	at := pinClock(t, a)

	code := requestDeviceCode(t, a)
	if rec := approve(t, a, testIDPKey, api.DeviceApproveRequest{
		UserCode: code.UserCode, Email: "me@example.com",
	}); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	first := poll(t, a, code.DeviceCode)
	if first.Code != 200 {
		t.Fatalf("first poll: %d %s", first.Code, first.Body.String())
	}

	*at = at.Add(devicePollInterval)
	second := poll(t, a, code.DeviceCode)
	if second.Code != 410 || errorCode(t, second) != api.DeviceCodeExpired {
		t.Fatalf("replayed poll answered %d %s, want 410 expired", second.Code, second.Body.String())
	}
	if strings.Contains(second.Body.String(), credentialPrefix) {
		t.Error("the replayed poll returned a second copy of the credential")
	}

	readAuthDB(t, a, func(db *sql.DB) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM credentials`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%d credentials exist; one login minted more than one", n)
		}
	})
}

// The poll limit exists to keep a client in a tight loop away from the store,
// so it has to refuse before the store is read — and it has to stop refusing
// once the interval has passed, or a client that polled twice quickly would be
// locked out of its own login.
func TestPollingFasterThanTheIntervalIsRefused(t *testing.T) {
	a := newDeviceServer(t)
	at := pinClock(t, a)
	code := requestDeviceCode(t, a)

	if rec := poll(t, a, code.DeviceCode); rec.Code != 428 {
		t.Fatalf("first poll: %d %s", rec.Code, rec.Body.String())
	}
	*at = at.Add(devicePollInterval - time.Second)
	rec := poll(t, a, code.DeviceCode)
	if rec.Code != 429 || errorCode(t, rec) != api.DeviceCodeSlowDown {
		t.Fatalf("a fast poll answered %d %s, want 429 slow_down", rec.Code, rec.Body.String())
	}
	// A refusal does not reset the clock: the client that ignored the
	// interval must still get in on the beat it was told to keep.
	*at = at.Add(time.Second)
	if rec := poll(t, a, code.DeviceCode); rec.Code != 428 {
		t.Fatalf("poll on the interval answered %d %s, want 428 pending", rec.Code, rec.Body.String())
	}
}

// Approval asserts who somebody is, which is not something any client of this
// service may do — not the service token, not a credential, only the identity
// provider's own key.
func TestApproveIsRefusedWithoutTheIdentityProviderKey(t *testing.T) {
	a := newDeviceServer(t)
	cred := mintCredentialFor(t, a, "someone@example.com")
	code := requestDeviceCode(t, a)

	req := api.DeviceApproveRequest{UserCode: code.UserCode, Email: "me@example.com"}
	for name, bearer := range map[string]string{
		"no bearer":         "",
		"wrong key":         "not-the-idp-key",
		"the service token": a.Token,
		"a credential":      cred.Token,
		"the bootstrap":     testBootstrap,
	} {
		t.Run(name, func(t *testing.T) {
			rec := approve(t, a, bearer, req)
			if rec.Code != 401 || errorCode(t, rec) != "permission" {
				t.Fatalf("approve accepted %s: %d %s", name, rec.Code, rec.Body.String())
			}
		})
	}

	// And nothing was approved by any of them.
	if rec := poll(t, a, code.DeviceCode); rec.Code != 428 {
		t.Fatalf("the code was approved after all: %d %s", rec.Code, rec.Body.String())
	}
}

// A service with an approval URL but no key cannot verify who is calling the
// approve route, so it must not serve it at all.
func TestApproveIsRefusedWhenNoKeyIsConfigured(t *testing.T) {
	a := newDeviceServer(t)
	a.IDPKey = ""
	code := requestDeviceCode(t, a)

	rec := approve(t, a, "", api.DeviceApproveRequest{UserCode: code.UserCode, Email: "me@example.com"})
	if rec.Code != 401 {
		t.Fatalf("approve without a configured key: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ARK_IDP_KEY") {
		t.Errorf("the message does not name the setting: %s", rec.Body.String())
	}
}

// Seeding is the identity provider's one chance to say what a principal may
// read, and RFC-0003's resolved decision 2 pins both halves of what that may
// do: it runs on every login, so a repository bound after someone paired
// becomes visible; and it only ever adds `read`, so a membership change at the
// identity provider can never quietly take access away.
func TestSeedingIsReappliedEveryLoginAndOnlyEverAddsRead(t *testing.T) {
	const (
		seeded    = "01SEEDEDREPO00000000000000"
		bound     = "01BOUNDLATERREPO0000000000"
		promoted  = "01PROMOTEDREPO000000000000"
		unrelated = "01UNRELATEDREPO0000000000A"
	)
	a := newDeviceServer(t)
	at := pinClock(t, a)

	login := func(repos ...string) api.DeviceTokenResponse {
		t.Helper()
		code := requestDeviceCode(t, a)
		if rec := approve(t, a, testIDPKey, api.DeviceApproveRequest{
			UserCode: code.UserCode, Email: "me@example.com", RepositoryIDs: repos,
		}); rec.Code != 200 {
			t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
		}
		*at = at.Add(devicePollInterval)
		rec := poll(t, a, code.DeviceCode)
		if rec.Code != 200 {
			t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
		}
		var out api.DeviceTokenResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	first := login(seeded, promoted)
	// An admin raises one grant to write and issues another the identity
	// provider never asserts. Neither is seeding's to touch.
	other := a.otherInstance(t)
	mustUpdateAuth(t, other, `UPDATE grants SET level = 'write' WHERE repository_id = ?`, promoted)
	mustUpdateAuth(t, other, `INSERT INTO grants (repository_id, principal_id, level, granted_by, granted_at)
		VALUES (?, ?, 'admin', 'an-admin', ?)`, unrelated, first.Principal.ID, records.Now())

	// The second login carries a repository bound since the first, and drops
	// one the identity provider previously asserted.
	second := login(seeded, bound)
	if second.Principal.ID != first.Principal.ID {
		t.Fatalf("a second login minted a second principal: %s then %s",
			first.Principal.ID, second.Principal.ID)
	}

	want := map[string]string{
		seeded:    "read",  // seeded twice, still read
		bound:     "read",  // bound after pairing, seeded on the later login
		promoted:  "write", // raised by an admin, and seeding left it alone
		unrelated: "admin", // never asserted, never touched
	}
	readAuthDB(t, a, func(db *sql.DB) {
		rows, err := db.Query(`SELECT repository_id, level, granted_by FROM grants WHERE principal_id = ?`,
			first.Principal.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		got := map[string]string{}
		for rows.Next() {
			var repo, level, by string
			if err := rows.Scan(&repo, &level, &by); err != nil {
				t.Fatal(err)
			}
			got[repo] = level
			if level == "read" && by != seededGrantGrantedBy && repo != unrelated {
				t.Errorf("grant on %s says it was granted by %q", repo, by)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("grants = %v, want %v", got, want)
		}
		for repo, level := range want {
			if got[repo] != level {
				t.Errorf("grant on %s = %q, want %q", repo, got[repo], level)
			}
		}
	})
}

// A code nobody approves in time is not redeemable, and neither is one the
// identity provider gets to late. Fifteen minutes is a bound, not a
// suggestion.
func TestAnExpiredCodeIsNeitherApprovableNorRedeemable(t *testing.T) {
	a := newDeviceServer(t)
	at := pinClock(t, a)
	code := requestDeviceCode(t, a)

	*at = at.Add(deviceCodeTTL + time.Second)
	rec := approve(t, a, testIDPKey, api.DeviceApproveRequest{
		UserCode: code.UserCode, Email: "me@example.com",
	})
	if rec.Code != 410 || errorCode(t, rec) != api.DeviceCodeExpired {
		t.Fatalf("approve on an expired code: %d %s", rec.Code, rec.Body.String())
	}
	if rec := poll(t, a, code.DeviceCode); rec.Code != 410 || errorCode(t, rec) != api.DeviceCodeExpired {
		t.Fatalf("poll on an expired code: %d %s", rec.Code, rec.Body.String())
	}

	// The next login sweeps the dead row away rather than leaving it to hold
	// its user code hostage.
	requestDeviceCode(t, a)
	readAuthDB(t, a, func(db *sql.DB) {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM device_codes WHERE user_code = ?`,
			strings.ReplaceAll(code.UserCode, "-", "")).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("the expired row survived a later login")
		}
	})
}

// Disabling a principal is the one act that stops them outright. A login that
// minted a fresh credential for a disabled principal would undo it.
func TestADisabledPrincipalCannotLogIn(t *testing.T) {
	a := newDeviceServer(t)
	at := pinClock(t, a)
	cred := mintCredentialFor(t, a, "me@example.com")
	mustUpdateAuth(t, a.otherInstance(t), `UPDATE principals SET disabled_at = ? WHERE id = ?`,
		records.Now(), cred.Principal.ID)

	code := requestDeviceCode(t, a)
	rec := approve(t, a, testIDPKey, api.DeviceApproveRequest{
		UserCode: code.UserCode, Email: "me@example.com",
	})
	if rec.Code != 409 {
		t.Fatalf("approve for a disabled principal: %d %s", rec.Code, rec.Body.String())
	}
	*at = at.Add(devicePollInterval)
	if rec := poll(t, a, code.DeviceCode); rec.Code != 428 {
		t.Fatalf("the refused approval was recorded anyway: %d %s", rec.Code, rec.Body.String())
	}
}

// Discovery is the whole of how a client decides how to log in, so the banner
// has to answer both ways without the client configuring anything.
func TestTheBannerReportsWhetherThereIsADeviceFlow(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		a := newDeviceServer(t)
		rec := doRequestAs(t, a.Server, "", "GET", "/", "")
		var banner api.ServiceBanner
		if err := json.Unmarshal(rec.Body.Bytes(), &banner); err != nil {
			t.Fatal(err)
		}
		if banner.Service != "ark-sync" || banner.API != "v1" {
			t.Errorf("banner = %+v; the existing fields must not move", banner)
		}
		if banner.Auth == nil || !banner.Auth.DeviceFlow || banner.Auth.ApprovalURL != testApprovalURL {
			t.Errorf("auth = %+v, want the device flow and its approval URL", banner.Auth)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		a := newAuthServer(t)
		rec := doRequestAs(t, a.Server, "", "GET", "/", "")
		var banner api.ServiceBanner
		if err := json.Unmarshal(rec.Body.Bytes(), &banner); err != nil {
			t.Fatal(err)
		}
		if banner.Auth == nil || banner.Auth.DeviceFlow || banner.Auth.ApprovalURL != "" {
			t.Errorf("auth = %+v, want a plain no", banner.Auth)
		}
		// And the routes are not there either: a service with no identity
		// provider must not hand out a code nobody can approve.
		for _, path := range []string{"/v1/device/code", "/v1/device/token", "/v1/device/approve"} {
			rec := doRequestAs(t, a.Server, "", "POST", path, `{}`)
			if rec.Code != 404 {
				t.Errorf("POST %s answered %d, want 404", path, rec.Code)
			}
			if !strings.Contains(rec.Body.String(), "ARK_IDP_APPROVAL_URL") {
				t.Errorf("POST %s does not name the setting: %s", path, rec.Body.String())
			}
		}
	})
}

// The user code is read off one screen and typed into another, which is the
// whole reason for the alphabet: no vowels to spell a word with, and none of
// Crockford's confusable letters.
func TestUserCodeShape(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := newUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != userCodeLength {
			t.Fatalf("code %q is %d symbols, want %d", code, len(code), userCodeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(userCodeAlphabet, r) {
				t.Fatalf("code %q carries %q, which is not in the alphabet", code, r)
			}
		}
		if strings.ContainsAny(code, "AEIOU") {
			t.Fatalf("code %q contains a vowel", code)
		}
		if display := displayUserCode(code); display != code[:4]+"-"+code[4:] {
			t.Fatalf("display %q does not render %q as XXXX-XXXX", display, code)
		}
	}
}

func TestNormalizeUserCode(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		valid bool
	}{
		{"BCDF-GHJK", "BCDFGHJK", true},
		{"bcdf-ghjk", "BCDFGHJK", true},
		{"BCDFGHJK", "BCDFGHJK", true},
		{"  bcdf ghjk  ", "BCDFGHJK", true},
		// Crockford's own reading of the letters the alphabet leaves out.
		{"1BCD-EFGH", "", false}, // E is a vowel and not in the alphabet
		{"IBCD-FGHJ", "1BCDFGHJ", true},
		{"LBCD-FGHJ", "1BCDFGHJ", true},
		{"OBCD-FGHJ", "0BCDFGHJ", true},
		{"BCDF-GHJ", "", false},   // seven symbols
		{"BCDF-GHJKM", "", false}, // nine
		{"", "", false},
		{"BCDF-GH!K", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeUserCode(tc.in)
		if ok != tc.valid || got != tc.want {
			t.Errorf("normalizeUserCode(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.valid)
		}
	}
}

// The code a person types is not the code they were shown, character for
// character — they retype it, and the flow has to survive that.
func TestApproveAcceptsARetypedCode(t *testing.T) {
	a := newDeviceServer(t)
	at := pinClock(t, a)
	code := requestDeviceCode(t, a)

	retyped := strings.ToLower(strings.ReplaceAll(code.UserCode, "-", " "))
	if rec := approve(t, a, testIDPKey, api.DeviceApproveRequest{
		UserCode: retyped, Email: "me@example.com",
	}); rec.Code != 200 {
		t.Fatalf("approve with %q: %d %s", retyped, rec.Code, rec.Body.String())
	}
	*at = at.Add(devicePollInterval)
	if rec := poll(t, a, code.DeviceCode); rec.Code != 200 {
		t.Fatalf("poll after a retyped approval: %d %s", rec.Code, rec.Body.String())
	}
}

// An unknown device code is answered exactly as an expired one is: telling
// them apart would say whether a guessed code had ever existed.
func TestAnUnknownDeviceCodeIsExpired(t *testing.T) {
	a := newDeviceServer(t)
	rec := poll(t, a, "not-a-device-code-anybody-issued")
	if rec.Code != 410 || errorCode(t, rec) != api.DeviceCodeExpired {
		t.Fatalf("unknown code answered %d %s, want 410 expired", rec.Code, rec.Body.String())
	}
}

package server

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/pkg/api"
)

// The service-wide acts elk-work/ark#94 was filed about, and D116's answer to
// "who may perform one": an operator, which is a principal, which has a name
// and a credential somebody can revoke.

// mintOperatorAndOrdinary returns a service whose first principal is the
// operator (the bootstrap rule) and a second that is not.
func mintOperatorAndOrdinary(t *testing.T) (*authServer, api.CreatePrincipalResponse, api.CreatePrincipalResponse) {
	t.Helper()
	a := newAuthServer(t)
	operator := mintCredentialFor(t, a, "operator@example.com")
	ordinary := mintCredentialFor(t, a, "member@example.com")
	return a, operator, ordinary
}

// The bootstrap rule, stated as the two halves that make ARK_BOOTSTRAP_TOKEN
// something other than an operator identity: it hands the authority over once,
// into a service that has none, and never again.
func TestBootstrapSeedsOnlyTheFirstOperator(t *testing.T) {
	a, operator, ordinary := mintOperatorAndOrdinary(t)
	if !operator.Principal.Operator() {
		t.Fatalf("the first principal did not become an operator: %+v", operator.Principal)
	}
	if operator.Principal.OperatorSince == "" {
		t.Errorf("operator_since should be set and reported: %+v", operator.Principal)
	}
	if ordinary.Principal.Operator() {
		t.Fatalf("a second bootstrap mint became an operator: %+v", ordinary.Principal)
	}

	// And the bootstrap token cannot ask for one either. Refused rather than
	// ignored, so nobody believes they made an operator and finds out later.
	rec := doRequestAs(t, a.Server, testBootstrap, "POST", "/v1/principals",
		`{"email":"third@example.com","operator":true}`)
	if rec.Code != 403 {
		t.Fatalf("the bootstrap token appointed an operator: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "operator") {
		t.Errorf("the refusal should say what it refused: %s", rec.Body.String())
	}
}

// An operator makes another operator; nobody else can, and the refusal says so.
func TestOnlyAnOperatorMakesAnOperator(t *testing.T) {
	a, operator, ordinary := mintOperatorAndOrdinary(t)

	rec := doRequestAs(t, a.Server, ordinary.Token, "POST", "/v1/principals",
		`{"email":"nope@example.com","operator":true}`)
	if rec.Code != 403 {
		t.Fatalf("an ordinary credential minted a principal: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not an operator") {
		t.Errorf("the refusal should name the reason: %s", rec.Body.String())
	}

	rec = doRequestAs(t, a.Server, operator.Token, "POST", "/v1/principals",
		`{"email":"second-operator@example.com","operator":true}`)
	if rec.Code != 200 {
		t.Fatalf("an operator could not make an operator: %d %s", rec.Code, rec.Body.String())
	}
	var made api.CreatePrincipalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &made); err != nil {
		t.Fatalf("response: %v", err)
	}
	if !made.Principal.Operator() {
		t.Fatalf("--operator did not make one: %+v", made.Principal)
	}
	// And the new operator can do the thing that makes them one.
	if rec := doRequestAs(t, a.Server, made.Token, "GET", "/v1/principals", ""); rec.Code != 200 {
		t.Fatalf("the new operator cannot list principals: %d %s", rec.Code, rec.Body.String())
	}
}

// The roster is an operator act, for the reason `ark repo grants` is an admin
// one: it names everybody the service knows and what each could present.
func TestListingPrincipalsIsOperatorOnly(t *testing.T) {
	a, operator, ordinary := mintOperatorAndOrdinary(t)

	rec := doRequestAs(t, a.Server, ordinary.Token, "GET", "/v1/principals", "")
	if rec.Code != 403 {
		t.Fatalf("a non-operator read the roster: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "member@example.com") {
		t.Errorf("the refusal should name who was refused: %s", rec.Body.String())
	}

	// The legacy service token reaches every repository and no service-wide
	// act. That is the whole point of D116 and it has to be pinned, because
	// "implicit admin everywhere" makes the refusal look like a bug.
	rec = doRequestAs(t, a.Server, a.Token, "GET", "/v1/principals", "")
	if rec.Code != 403 {
		t.Fatalf("the shared service token read the roster: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ARK_BOOTSTRAP_TOKEN") {
		t.Errorf("the refusal should say how to get an operator: %s", rec.Body.String())
	}

	rec = doRequestAs(t, a.Server, operator.Token, "GET", "/v1/principals", "")
	if rec.Code != 200 {
		t.Fatalf("the operator was refused the roster: %d %s", rec.Code, rec.Body.String())
	}
	var list api.PrincipalListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("response: %v", err)
	}
	if len(list.Principals) != 2 {
		t.Fatalf("roster has %d principals, want 2: %+v", len(list.Principals), list.Principals)
	}
	// Email order, which is the order a person reads.
	if list.Principals[0].Principal.Email != "member@example.com" {
		t.Errorf("roster is not in email order: %+v", list.Principals)
	}
	if strings.Contains(rec.Body.String(), "arkc_") || strings.Contains(rec.Body.String(), "token_sha256") {
		t.Errorf("the roster leaked credential material: %s", rec.Body.String())
	}
}

// The failure elk-work/ark#94 named: "`ark principal create` prints a
// credential id once … so a credential id nobody wrote down cannot be named
// later — which makes revocation unusable even once it exists."
func TestTheRosterRecoversACredentialIdNobodyWroteDown(t *testing.T) {
	a, operator, ordinary := mintOperatorAndOrdinary(t)

	rec := doRequestAs(t, a.Server, operator.Token, "GET", "/v1/principals", "")
	var list api.PrincipalListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("response: %v", err)
	}
	found := ""
	for _, p := range list.Principals {
		if p.Principal.Email != "member@example.com" {
			continue
		}
		for _, c := range p.Credentials {
			found = c.ID
			if c.Label != "bootstrap" {
				t.Errorf("credential label = %q, want bootstrap", c.Label)
			}
			if c.RevokedAt != "" {
				t.Errorf("a fresh credential is not revoked: %+v", c)
			}
		}
	}
	if found != ordinary.CredentialID {
		t.Fatalf("roster credential id = %q, want %q", found, ordinary.CredentialID)
	}

	// And the same for the holder, without an operator: `GET /v1/credentials`
	// is what makes the self-service half reachable at all.
	rec = doRequestAs(t, a.Server, ordinary.Token, "GET", "/v1/credentials", "")
	if rec.Code != 200 {
		t.Fatalf("a principal could not list its own credentials: %d %s", rec.Code, rec.Body.String())
	}
	var mine api.CredentialListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &mine); err != nil {
		t.Fatalf("response: %v", err)
	}
	if mine.PrincipalID != ordinary.Principal.ID {
		t.Errorf("principal_id = %q, want %q", mine.PrincipalID, ordinary.Principal.ID)
	}
	if len(mine.Credentials) != 1 || mine.Credentials[0].ID != ordinary.CredentialID {
		t.Fatalf("own credentials = %+v, want just %s", mine.Credentials, ordinary.CredentialID)
	}
}

// "I lost a laptop" has to work on a Saturday, without finding an operator
// first. It is option 3 of elk-work/ark#94, kept as a subset of option 2.
func TestAPrincipalRevokesItsOwnCredentialAndNobodyElses(t *testing.T) {
	a, operator, ordinary := mintOperatorAndOrdinary(t)
	second := doRequestAs(t, a.Server, operator.Token, "POST", "/v1/principals",
		`{"email":"member@example.com"}`)
	if second.Code != 200 {
		t.Fatalf("reissue: %d %s", second.Code, second.Body.String())
	}
	var laptop api.CreatePrincipalResponse
	if err := json.Unmarshal(second.Body.Bytes(), &laptop); err != nil {
		t.Fatalf("response: %v", err)
	}

	// Somebody else's credential is not findable, let alone revocable: a
	// non-operator gets the same answer for "not yours" and "does not exist".
	for name, id := range map[string]string{
		"an operator's credential":       operator.CredentialID,
		"a credential that is not there": "01NOSUCHCREDENTIAL00000000",
	} {
		rec := doRequestAs(t, a.Server, ordinary.Token, "POST", "/v1/credentials/"+id+"/revoke", "")
		if rec.Code != 404 {
			t.Errorf("%s: %d %s, want 404", name, rec.Code, rec.Body.String())
		}
	}

	// Its own, though, it may retire — the laptop credential, while still
	// holding the other one.
	rec := doRequestAs(t, a.Server, ordinary.Token,
		"POST", "/v1/credentials/"+laptop.CredentialID+"/revoke", "")
	if rec.Code != 200 {
		t.Fatalf("self-revoke refused: %d %s", rec.Code, rec.Body.String())
	}
	var out api.RevokeCredentialResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response: %v", err)
	}
	if out.AlreadyRevoked {
		t.Errorf("a first revocation reported itself as a repeat: %+v", out)
	}
	if out.Credential.RevokedAt == "" || out.Credential.RevokedBy != ordinary.Principal.ID {
		t.Errorf("revocation not attributed to the holder: %+v", out.Credential)
	}

	// Revoking twice is a success that changes nothing, and says which.
	rec = doRequestAs(t, a.Server, ordinary.Token,
		"POST", "/v1/credentials/"+laptop.CredentialID+"/revoke", "")
	if rec.Code != 200 {
		t.Fatalf("a repeat revocation failed: %d %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response: %v", err)
	}
	if !out.AlreadyRevoked {
		t.Errorf("a repeat revocation did not report itself as one: %+v", out)
	}
}

// The operator half: retiring a departed colleague's credential, which is the
// case self-service cannot cover and the one the issue was filed about.
//
// It extends elk-work/ark#43's verifier test through the route rather than
// through a hand-edited auth.db: what #43 proved the verifier honours is now
// something a person can actually cause.
func TestAnOperatorRevokesAndTheCredentialIsRefusedOnTheNextRequest(t *testing.T) {
	var logged bytes.Buffer
	a, operator, ordinary := mintOperatorAndOrdinary(t)
	a.Log = slog.New(slog.NewJSONHandler(&logged, nil))

	at := time.Now()
	store := a.authStore()
	store.now = func() time.Time { return at }

	// The credential works before anything happens to it.
	if rec := doRequestAs(t, a.Server, ordinary.Token, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
		t.Fatalf("credential refused before it was revoked: %s", rec.Body.String())
	}

	rec := doRequestAs(t, a.Server, operator.Token,
		"POST", "/v1/credentials/"+ordinary.CredentialID+"/revoke", "")
	if rec.Code != 200 {
		t.Fatalf("operator revoke: %d %s", rec.Code, rec.Body.String())
	}

	// Revoking through this instance drops its cache, so the refusal is
	// immediate here; the 60-second bound is what another instance pays, and
	// TestRevokedExpiredAndDisabledAreRefusedWithinTheTTL pins that.
	rec = doRequestAs(t, a.Server, ordinary.Token, "POST", "/v1/sync/pull", `{}`)
	if rec.Code != 401 {
		t.Fatalf("a revoked credential still authenticated: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "credential revoked") {
		t.Errorf("the refusal should name the cause: %s", rec.Body.String())
	}

	// Attribution: the named operator, never a shared secret, and never the
	// credential itself (spec §21).
	log := logged.String()
	if !strings.Contains(log, `"credential revoked"`) {
		t.Fatalf("the revocation was not logged: %s", log)
	}
	for _, want := range []string{operator.Principal.ID, "operator@example.com", ordinary.CredentialID} {
		if !strings.Contains(log, want) {
			t.Errorf("the log does not name %q: %s", want, log)
		}
	}
	if strings.Contains(log, ordinary.Token) || strings.Contains(log, "arkc_") {
		t.Errorf("the log leaked a credential: %s", log)
	}

	// And it is durable, not only a log line: RFC-0003's non-goals rule out an
	// audit-log export, so `revoked_by` beside `revoked_at` is the record.
	readAuthDB(t, a, func(db *sql.DB) {
		var by string
		if err := db.QueryRow(`SELECT COALESCE(revoked_by, '') FROM credentials WHERE id = ?`,
			ordinary.CredentialID).Scan(&by); err != nil {
			t.Fatalf("read revoked_by: %v", err)
		}
		if by != operator.Principal.ID {
			t.Errorf("revoked_by = %q, want %q", by, operator.Principal.ID)
		}
	})
}

// An auth.db written before elk-work/ark#94 has neither new column, and every
// auth.db in existence is one. The upgrade has to happen on the next read or
// write — there is no migration command for this store and no deploy step —
// so this stands up the old shape by hand and then asks the service to use it.
func TestAnOlderAuthDBGainsTheOperatorColumns(t *testing.T) {
	a := newAuthServer(t)
	// A store with a principal in it, as every live deployment has.
	old := mintCredentialFor(t, a, "old@example.com")

	// Now make it the shape it had before this change, by dropping the two
	// columns off the stored file directly. `openAuthDB` would put them back,
	// so the drop happens on a raw handle that never applies the schema.
	db, err := sql.Open("sqlite", "file:"+a.authDBPath())
	if err != nil {
		t.Fatalf("open auth.db: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE principals DROP COLUMN operator_since`,
		`ALTER TABLE credentials DROP COLUMN revoked_by`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	db.Close()
	// Drop the cache too: this instance still remembers the upgraded read.
	a.authStore().snap = nil

	// The service must read the old shape, upgrade it, and go on working —
	// with no migration command, because auth.db has none, and no deploy step.
	if rec := doRequestAs(t, a.Server, old.Token, "POST", "/v1/sync/pull", `{}`); rec.Code == 401 {
		t.Fatalf("an existing credential stopped working on the old shape: %s", rec.Body.String())
	}
	cred := mintCredentialFor(t, a, "new@example.com")
	if !cred.Principal.Operator() {
		t.Fatalf("the first operator was not seeded on an upgraded store: %+v", cred.Principal)
	}
	if rec := doRequestAs(t, a.Server, cred.Token, "GET", "/v1/principals", ""); rec.Code != 200 {
		t.Fatalf("operator routes do not work on an upgraded store: %d %s", rec.Code, rec.Body.String())
	}
	rec := doRequestAs(t, a.Server, cred.Token,
		"POST", "/v1/credentials/"+cred.CredentialID+"/revoke", "")
	if rec.Code != 200 {
		t.Fatalf("revoke on an upgraded store: %d %s", rec.Code, rec.Body.String())
	}
}

// A service with no bootstrap token and no operator has no way in, and the
// route says so rather than reporting a bad credential.
func TestOperatorRoutesOnAServiceWithNoOperator(t *testing.T) {
	a := newAuthServer(t)
	a.BootstrapToken = ""
	rec := doRequestAs(t, a.Server, a.Token, "GET", "/v1/principals", "")
	if rec.Code != 403 {
		t.Fatalf("the service token listed principals: %d %s", rec.Code, rec.Body.String())
	}
	rec = doRequestAs(t, a.Server, a.Token, "GET", "/v1/credentials", "")
	if rec.Code != 403 {
		t.Fatalf("the service token listed credentials: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not a principal") {
		t.Errorf("the refusal should say why the service token holds none: %s", rec.Body.String())
	}
}

// A disabled principal's credentials are still revocable by an operator: the
// disabling refuses the request, and retiring the credential is the separate
// act that makes the token itself worthless.
func TestAnOperatorRevokesForADisabledPrincipal(t *testing.T) {
	a, operator, ordinary := mintOperatorAndOrdinary(t)
	mustUpdateAuth(t, a.authStore(), `UPDATE principals SET disabled_at = ? WHERE id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), ordinary.Principal.ID)

	rec := doRequestAs(t, a.Server, operator.Token,
		"POST", "/v1/credentials/"+ordinary.CredentialID+"/revoke", "")
	if rec.Code != 200 {
		t.Fatalf("revoke for a disabled principal: %d %s", rec.Code, rec.Body.String())
	}
	// And the holder can no longer reach the self-service route at all, which
	// is what "disabled" means.
	if rec := doRequestAs(t, a.Server, ordinary.Token, "GET", "/v1/credentials", ""); rec.Code != 401 {
		t.Errorf("a disabled principal authenticated: %d %s", rec.Code, rec.Body.String())
	}
}

// The roster reports what an operator needs to act on, including the state
// that changes what every other row means.
func TestTheRosterReportsOperatorAndDisabled(t *testing.T) {
	a, operator, ordinary := mintOperatorAndOrdinary(t)
	mustUpdateAuth(t, a.authStore(), `UPDATE principals SET disabled_at = ? WHERE id = ?`,
		"2026-08-30T00:00:00Z", ordinary.Principal.ID)

	rec := doRequestAs(t, a.Server, operator.Token, "GET", "/v1/principals", "")
	if rec.Code != 200 {
		t.Fatalf("roster: %d %s", rec.Code, rec.Body.String())
	}
	var list api.PrincipalListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("response: %v", err)
	}
	seen := map[string]api.Principal{}
	for _, p := range list.Principals {
		seen[p.Principal.Email] = p.Principal
	}
	if got := seen["member@example.com"]; got.DisabledAt == "" {
		t.Errorf("a disabled principal is not reported as one: %+v", got)
	}
	if got := seen["operator@example.com"]; !got.Operator() {
		t.Errorf("the operator is not reported as one: %+v", got)
	}
	if fmt.Sprint(seen["operator@example.com"].Kind) != "human" {
		// The whole reason operator is its own column: promoting somebody
		// must not erase what is holding the credential (RFC-0003 Decision 5).
		t.Errorf("promotion overwrote kind: %+v", seen["operator@example.com"])
	}
}

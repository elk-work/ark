package server

// The device-authorization flow (docs/rfc-0003-elk-issued-credentials.md,
// Decision 3; docs/v1-spec.md §20.1), which is how a person logs in without
// already holding a token.
//
// Three routes. Two are unauthenticated because they have to be — the caller
// has nothing to authenticate with yet, which is the whole problem — and one
// is authenticated with ARK_IDP_KEY because the identity provider is a server,
// not a browser. What makes returning a credential over an unauthenticated
// route safe is that redemption is one-shot and the device code is a secret
// only the requesting process ever saw: possession of it *is* the
// authentication, and the row is deleted in the transaction that hands the
// credential over, so a replayed poll gets `expired` rather than a second copy.
//
// Nothing here knows the identity provider's name. `ARK_IDP_APPROVAL_URL` is
// a URL this service prints and never calls; `ARK_IDP_KEY` is a shared secret
// it compares. A self-hoster who sets neither has no device flow, and every
// other path — the service token, `ark principal create`, `ark login --token`
// — works exactly as before (RFC-0003 Decision 6).

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// deviceCodeTTL and devicePollInterval are RFC-0003's numbers: fifteen
// minutes to walk to a browser, five seconds between polls.
const (
	deviceCodeTTL      = 15 * time.Minute
	devicePollInterval = 5 * time.Second
)

// userCodeAlphabet is Crockford base32 with the vowels dropped. Crockford
// already excludes I, L, O and U — the confusable letters — and dropping A
// and E as well leaves 30 symbols that cannot spell a word at anybody. Ark
// already speaks this alphabet: records.NewID mints Crockford-encoded ULIDs.
const userCodeAlphabet = "0123456789BCDFGHJKMNPQRSTVWXYZ"

// userCodeLength is the number of symbols in a user code, rendered XXXX-XXXX.
// 30^8 is about 6.5e11, and a code lives fifteen minutes.
const userCodeLength = 8

// deviceCredentialLabel marks a credential minted by the device flow, so
// `label` on the credentials row says how someone logged in.
const deviceCredentialLabel = "device"

// seededGrantLevel is the only level seeding ever writes, and
// seededGrantGrantedBy is who it records as the granter. RFC-0003's resolved
// decision 2: seeding adds `read` and nothing else, so losing a membership at
// the identity provider never silently revokes Ark access — revocation stays
// an explicit act.
const (
	seededGrantLevel     = "read"
	seededGrantGrantedBy = "idp"
)

// deviceSchema is the pending-code table. It is applied beside authSchema
// rather than inside it: this is one table added by one slice, and keeping the
// statements apart is what lets the slices land in either order.
//
// A row holds the identity provider's assertion, never a credential. The
// credential is minted at redemption, in the transaction that deletes the row
// — so a leaked pending-code store yields nothing redeemable, which is the
// same property that makes `device_code_sha256` a digest rather than a value.
const deviceSchema = `
CREATE TABLE IF NOT EXISTS device_codes (
	device_code_sha256 TEXT PRIMARY KEY,
	user_code          TEXT NOT NULL UNIQUE,
	created_at         TEXT NOT NULL,
	expires_at         TEXT NOT NULL,
	approved_at        TEXT NOT NULL DEFAULT '',
	subject            TEXT NOT NULL DEFAULT '',
	email              TEXT NOT NULL DEFAULT '',
	display_name       TEXT NOT NULL DEFAULT '',
	repository_ids     TEXT NOT NULL DEFAULT ''
);
`

// Why a poll or an approval was refused.
var (
	errDevicePending = errors.New("device code not approved yet")
	// errDeviceExpired is deliberately one answer for three states — past
	// its TTL, already redeemed, never issued — because telling them apart
	// would say whether a guessed code had ever existed.
	errDeviceExpired = errors.New("device code expired")
	errUserCodeTaken = errors.New("user code already in use")
)

// deviceStamp formats a timestamp for the device table. Fixed-width RFC3339
// seconds, not the RFC3339Nano the rest of auth.db uses, because these values
// are compared in SQL: Nano trims trailing zeros from the fraction, so two
// stamps a moment apart can sort the wrong way round as strings. A
// fifteen-minute code does not need sub-second precision.
func deviceStamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// deviceStore holds pending device codes. It borrows auth.db and its
// compare-and-swap from authStore rather than opening a second store: pending
// codes are credential state, they are written by the same transactions that
// mint credentials, and one CAS object is one failure domain instead of two.
type deviceStore struct {
	auth   *authStore
	issuer string // recorded on a principal; the approval URL's host

	// ttl and interval are fields rather than constants so a test can pin a
	// code's lifetime instead of waiting fifteen minutes for it.
	ttl      time.Duration
	interval time.Duration

	// lastPoll is the slow_down clock, and it is in memory on purpose. Its
	// job is to keep a client that polls in a tight loop away from the
	// store, so recording each poll *in* the store would spend exactly what
	// it is protecting. The cost is that the limit is per instance; at
	// --max-instances 1 (the deployment RFC-0001 describes) that is the
	// whole fleet, and a client that evades it by reaching two instances
	// still cannot redeem a code twice.
	mu       sync.Mutex
	lastPoll map[string]time.Time
}

func newDeviceStore(auth *authStore, issuer string) *deviceStore {
	return &deviceStore{
		auth:     auth,
		issuer:   issuer,
		ttl:      deviceCodeTTL,
		interval: devicePollInterval,
		lastPoll: map[string]time.Time{},
	}
}

func (d *deviceStore) now() time.Time { return d.auth.now() }

// deviceStore returns the pending-code store, opening the credential store
// behind it on first use. Server is a struct literal everywhere, so there is
// no constructor to do this in — the same reason authStore is lazy.
func (s *Server) deviceStore() *deviceStore {
	s.deviceOnce.Do(func() {
		s.devices = newDeviceStore(s.authStore(), approvalHost(s.IDPApprovalURL))
	})
	return s.devices
}

// deviceFlowEnabled reports whether this service offers a device login at
// all. Unset ARK_IDP_APPROVAL_URL is the ordinary configuration — it is what
// every self-hoster with no identity provider runs — and it must be a plain
// "no" rather than a code nobody can approve.
func (s *Server) deviceFlowEnabled() bool { return s.IDPApprovalURL != "" }

// approvalHost names the identity provider by the only thing this service
// knows about it: the host of the URL it sends people to. It is recorded on a
// principal as `issuer`, so a deployment that later adds a second identity
// provider can tell which one asserted whom.
func approvalHost(approvalURL string) string {
	u, err := url.Parse(approvalURL)
	if err != nil || u.Host == "" {
		return approvalURL
	}
	return u.Host
}

// verificationURIComplete is the approval URL with the code already in it,
// for a client that can render a link. The code travels in the query string,
// which is where RFC 8628 puts it.
func verificationURIComplete(approvalURL, userCode string) string {
	u, err := url.Parse(approvalURL)
	if err != nil {
		return approvalURL
	}
	q := u.Query()
	q.Set("user_code", userCode)
	u.RawQuery = q.Encode()
	return u.String()
}

// newUserCode mints eight symbols from the vowel-free Crockford alphabet.
//
// The rejection above `limit` is what keeps the distribution flat: 256 is not
// a multiple of 30, so taking every byte modulo the alphabet would make the
// first sixteen symbols slightly likelier than the rest.
func newUserCode() (string, error) {
	const limit = 256 - (256 % len(userCodeAlphabet))
	out := make([]byte, 0, userCodeLength)
	buf := make([]byte, userCodeLength)
	for len(out) < userCodeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("mint user code: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, userCodeAlphabet[int(b)%len(userCodeAlphabet)])
			if len(out) == userCodeLength {
				break
			}
		}
	}
	return string(out), nil
}

// normalizeUserCode turns what a person typed into the eight symbols stored.
//
// It accepts the hyphen the code is displayed with, spaces, any case, and
// Crockford's own decoding of the letters the alphabet leaves out — I and L
// read as 1, O reads as 0 — because the code is read off one screen and typed
// into another, and refusing a transcription that is unambiguous would be a
// support ticket rather than a security property.
func normalizeUserCode(s string) (string, bool) {
	var b strings.Builder
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		switch r {
		case '-', ' ', '\t':
			continue
		case 'I', 'L':
			r = '1'
		case 'O':
			r = '0'
		}
		if !strings.ContainsRune(userCodeAlphabet, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	code := b.String()
	if len(code) != userCodeLength {
		return "", false
	}
	return code, true
}

// displayUserCode renders a stored code the way a person reads it.
func displayUserCode(code string) string {
	if len(code) != userCodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// issuedDevice is one pending code, as handed to the client that asked.
type issuedDevice struct {
	DeviceCode string // plaintext, returned once and never stored
	UserCode   string // canonical, without the display hyphen
	ExpiresAt  time.Time
}

// issue writes a pending row and returns the pair of codes.
func (d *deviceStore) issue(ctx context.Context) (*issuedDevice, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("mint device code: %w", err)
	}
	deviceCode := base64.RawURLEncoding.EncodeToString(buf)
	userCode, err := newUserCode()
	if err != nil {
		return nil, err
	}
	// Minted outside the closure, which reruns on a lost CAS race: a replay
	// that minted a second pair would hand back codes the store does not hold.
	var (
		now     = d.now()
		expires = now.Add(d.ttl)
		hash    = hashCredential(deviceCode)
	)

	err = d.auth.update(ctx, func(tx *sql.Tx) error {
		// Housekeeping, on the one transaction per login that is already
		// being written: an expired pending row is worthless, and leaving it
		// there would hold its user code hostage.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM device_codes WHERE expires_at <= ?`, deviceStamp(now)); err != nil {
			return err
		}
		var taken int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM device_codes WHERE user_code = ?`, userCode).Scan(&taken); err != nil {
			return err
		}
		if taken > 0 {
			return errUserCodeTaken
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO device_codes
			(device_code_sha256, user_code, created_at, expires_at)
			VALUES (?, ?, ?, ?) ON CONFLICT (device_code_sha256) DO NOTHING`,
			hash, userCode, deviceStamp(now), deviceStamp(expires))
		return err
	})
	if err != nil {
		return nil, err
	}
	d.prune(now)
	return &issuedDevice{DeviceCode: deviceCode, UserCode: userCode, ExpiresAt: expires}, nil
}

// prune drops poll clocks for codes that can no longer be redeemed. Called on
// issue, which is the once-per-login moment the map grows.
func (d *deviceStore) prune(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for hash, at := range d.lastPoll {
		if now.Sub(at) > d.ttl {
			delete(d.lastPoll, hash)
		}
	}
}

// allowPoll reports whether this poll may reach the store, and records it if
// so. A refused poll does not reset the clock: a client polling in a tight
// loop would otherwise never be let through again.
func (d *deviceStore) allowPoll(hash string) bool {
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if last, ok := d.lastPoll[hash]; ok && now.Sub(last) < d.interval {
		return false
	}
	d.lastPoll[hash] = now
	return true
}

// approve records the identity provider's assertion against a pending code.
//
// It does not mint anything. Everything that follows from an approval — the
// principal, the credential, the seeded grants — happens at redemption, in
// one transaction with the delete, so an approval nobody redeems leaves no
// trace and a leaked pending row is not a credential.
func (d *deviceStore) approve(ctx context.Context, userCode string, req api.DeviceApproveRequest) error {
	ids, err := json.Marshal(cleanRepositoryIDs(req.RepositoryIDs))
	if err != nil {
		return err
	}
	now := deviceStamp(d.now())
	return d.auth.update(ctx, func(tx *sql.Tx) error {
		// A disabled principal is refused here as well as at redemption, so
		// the person sees it in the browser rather than as a puzzling failure
		// on the machine they are trying to log in from.
		var disabled string
		err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(disabled_at, '') FROM principals WHERE email = ?`, req.Email).Scan(&disabled)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if disabled != "" {
			return errPrincipalDisabled
		}
		// An absolute UPDATE, so a replay after a lost CAS race lands the
		// same assertion rather than a second one.
		res, err := tx.ExecContext(ctx, `UPDATE device_codes
			SET approved_at = ?, subject = ?, email = ?, display_name = ?, repository_ids = ?
			WHERE user_code = ? AND expires_at > ?`,
			now, req.Subject, req.Email, req.DisplayName, string(ids), userCode, now)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errDeviceExpired
		}
		return nil
	})
}

// cleanRepositoryIDs trims, drops blanks, and de-duplicates the asserted list.
// It does not check that a repository exists: a grant for one that does not is
// inert, and refusing the assertion would make seeding depend on the order in
// which someone pairs and someone else registers.
func cleanRepositoryIDs(ids []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// deviceRedemption is what a successful poll hands back.
type deviceRedemption struct {
	Token        string
	Principal    authPrincipal
	CredentialID string
	ExpiresAt    string
	// Seeded is the repositories this login added a `read` grant for. It is
	// reported for the log, not to the client: what a principal may read is
	// answered by the grant check, not by what one login happened to add.
	Seeded []string
}

// redeem turns an approved pending code into a credential, exactly once.
//
// One transaction does all of it: resolve or create the principal, mint the
// credential, seed the grants, and delete the pending row. That is what makes
// the replayed poll answer `expired` — there is no window in which the row is
// gone and the credential is not yet stored, or the reverse.
//
// A poll that finds nothing to redeem costs a fetch and no write: the closure
// returns its sentinel, the transaction rolls back, and update never reaches
// the compare-and-swap. So the ordinary state of a login in progress — polling
// every five seconds for up to fifteen minutes — does not contend for auth.db,
// which is the one object every request depends on.
func (d *deviceStore) redeem(ctx context.Context, hash string) (*deviceRedemption, error) {
	token, tokenHash, err := mintCredential()
	if err != nil {
		return nil, err
	}
	// Minted outside the closure for the same reason createPrincipal does it:
	// a replay must not hand back a token the store does not hold.
	var (
		now            = d.now()
		newPrincipalID = records.NewID()
		credentialID   = records.NewID()
		createdAt      = now.UTC().Format(time.RFC3339Nano)
		expiresAt      = now.UTC().Add(credentialLifetime).Format(time.RFC3339Nano)
	)

	var out deviceRedemption
	err = d.auth.update(ctx, func(tx *sql.Tx) error {
		out = deviceRedemption{} // reset: this closure may be replayed

		var approvedAt, subject, email, displayName, repoIDs, expiresAtRow string
		err := tx.QueryRowContext(ctx, `SELECT approved_at, subject, email, display_name,
			repository_ids, expires_at FROM device_codes WHERE device_code_sha256 = ?`, hash).
			Scan(&approvedAt, &subject, &email, &displayName, &repoIDs, &expiresAtRow)
		if errors.Is(err, sql.ErrNoRows) {
			return errDeviceExpired
		}
		if err != nil {
			return err
		}
		exp, perr := time.Parse(time.RFC3339, expiresAtRow)
		if perr != nil || !now.Before(exp) {
			// An expiry nobody can read is not one anybody should honour.
			return errDeviceExpired
		}
		if approvedAt == "" {
			return errDevicePending
		}

		p := authPrincipal{
			ID:          newPrincipalID,
			Kind:        "human",
			Email:       email,
			DisplayName: displayName,
			CreatedAt:   createdAt,
		}
		var storedSubject string
		err = tx.QueryRowContext(ctx, `SELECT id, kind, subject, display_name, created_at,
			COALESCE(disabled_at, '') FROM principals WHERE email = ?`, email).
			Scan(&p.ID, &p.Kind, &storedSubject, &p.DisplayName, &p.CreatedAt, &p.DisabledAt)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO principals
				(id, kind, issuer, subject, email, display_name, created_at)
				VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
				p.ID, p.Kind, d.issuer, subject, p.Email, p.DisplayName, p.CreatedAt); err != nil {
				return err
			}
		case err != nil:
			return err
		case p.DisabledAt != "":
			// Minting here would undo the disabling, which is the one act
			// that stops a principal outright.
			return errPrincipalDisabled
		default:
			// Backfill what the principal does not have — a principal minted
			// by ARK_BOOTSTRAP_TOKEN has no issuer and no subject — and take
			// the display name from the assertion, which is the only place it
			// is ever refreshed. The subject is never overwritten: the email
			// is the identity (RFC-0003 Decision 4), so a changed subject is
			// a fact to record on first sight, not a rebinding to perform.
			if _, err := tx.ExecContext(ctx, `UPDATE principals SET
				issuer = CASE WHEN issuer = '' THEN ? ELSE issuer END,
				subject = CASE WHEN subject = '' THEN ? ELSE subject END,
				display_name = CASE WHEN ? <> '' THEN ? ELSE display_name END
				WHERE id = ?`,
				d.issuer, subject, displayName, displayName, p.ID); err != nil {
				return err
			}
			if displayName != "" {
				p.DisplayName = displayName
			}
		}

		if _, err := tx.ExecContext(ctx, `INSERT INTO credentials
			(id, principal_id, token_sha256, label, created_at, expires_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (id) DO NOTHING`,
			credentialID, p.ID, tokenHash, deviceCredentialLabel, createdAt, expiresAt); err != nil {
			return err
		}

		// This is the login RFC-0003 Decision 4 means when it says a grant
		// keyed on an email "resolves to a principal id at that person's
		// first login": every grant an admin issued to this address before
		// anybody held it becomes a grant this principal holds.
		//
		// It runs **before** seeding, and the order is load-bearing. Seeding
		// adds `read` and leaves an existing row alone, so claiming second
		// would let a seeded `read` sit where an admin had written `write`
		// and silently keep it there.
		if err := claimPendingGrants(ctx, tx, p.ID, p.Email); err != nil {
			return err
		}

		var seeded []string
		if repoIDs != "" {
			if err := json.Unmarshal([]byte(repoIDs), &seeded); err != nil {
				return err
			}
		}
		for _, repoID := range seeded {
			// addGrant writes only where nothing exists, which is the whole
			// of "only ever adds read": an existing grant — `read`, `write`
			// or `admin` — is left exactly as it stands, so seeding can
			// neither downgrade someone nor resurrect a level an admin took
			// away. See grants.go, which is where that rule now lives.
			if err := addGrant(ctx, tx, repoID, p.ID,
				seededGrantLevel, seededGrantGrantedBy, createdAt); err != nil {
				return err
			}
		}

		if _, err := tx.ExecContext(ctx,
			`DELETE FROM device_codes WHERE device_code_sha256 = ?`, hash); err != nil {
			return err
		}

		out = deviceRedemption{
			Token:        token,
			Principal:    p,
			CredentialID: credentialID,
			ExpiresAt:    expiresAt,
			Seeded:       seeded,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// handleDeviceCode issues a pending code. Unauthenticated: the caller has
// nothing to authenticate with, which is why it is here.
func (s *Server) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	if !s.deviceFlowEnabled() {
		s.deviceUnavailable(w)
		return
	}
	d := s.deviceStore()
	// The body is `{}` and is ignored, so a client that sends nothing is
	// fine. Retry only for a user-code collision: at 30^8 over a fifteen
	// minute window that is a lottery win, and refusing it would be a login
	// that failed for no reason a person could act on.
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		var issued *issuedDevice
		issued, err = d.issue(r.Context())
		if errors.Is(err, errUserCodeTaken) {
			continue
		}
		if err != nil {
			s.internal(w, "issue device code", err)
			return
		}
		if s.Log != nil {
			// Neither code is logged. The device code is the credential the
			// poll presents (spec §21), and the user code is what somebody
			// with the approval page open would need to approve a login they
			// did not start.
			s.Log.Info("device code issued", "expires_at", deviceStamp(issued.ExpiresAt))
		}
		display := displayUserCode(issued.UserCode)
		writeJSON(w, api.DeviceCodeResponse{
			DeviceCode:              issued.DeviceCode,
			UserCode:                display,
			VerificationURI:         s.IDPApprovalURL,
			VerificationURIComplete: verificationURIComplete(s.IDPApprovalURL, display),
			ExpiresIn:               int(d.ttl.Seconds()),
			Interval:                int(d.interval.Seconds()),
		})
		return
	}
	s.internal(w, "issue device code", err)
}

// handleDeviceToken polls for the credential, and redeems it exactly once.
func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	if !s.deviceFlowEnabled() {
		s.deviceUnavailable(w)
		return
	}
	req, ok := decode[api.DeviceTokenRequest](w, r)
	if !ok {
		return
	}
	if strings.TrimSpace(req.DeviceCode) == "" {
		writeErr(w, http.StatusBadRequest, "validation", "device_code is required")
		return
	}
	d := s.deviceStore()
	// The same digest the credential store keeps, and for the same reason: a
	// leaked pending-code store must not be redeemable.
	hash := hashCredential(req.DeviceCode)
	if !d.allowPoll(hash) {
		writeErr(w, http.StatusTooManyRequests, api.DeviceCodeSlowDown,
			fmt.Sprintf("polled faster than the interval; wait %d seconds between polls",
				int(d.interval.Seconds())))
		return
	}

	out, err := d.redeem(r.Context(), hash)
	switch {
	case errors.Is(err, errDevicePending):
		writeErr(w, http.StatusPreconditionRequired, api.DeviceCodePending,
			"nobody has approved this code yet")
		return
	case errors.Is(err, errDeviceExpired):
		writeErr(w, http.StatusGone, api.DeviceCodeExpired,
			"this code has expired or has already been redeemed; start again")
		return
	case errors.Is(err, errPrincipalDisabled):
		writeErr(w, http.StatusUnauthorized, "permission", "principal disabled")
		return
	case err != nil:
		s.internal(w, "redeem device code", err)
		return
	}
	if s.Log != nil {
		// The principal and the credential id, never the credential (§21).
		s.Log.Info("device login", "principal", out.Principal.ID,
			"credential", out.CredentialID, "seeded_grants", len(out.Seeded))
	}
	writeJSON(w, api.DeviceTokenResponse{
		Token: out.Token,
		Principal: api.Principal{
			ID:          out.Principal.ID,
			Kind:        out.Principal.Kind,
			Email:       out.Principal.Email,
			DisplayName: out.Principal.DisplayName,
			CreatedAt:   out.Principal.CreatedAt,
		},
	})
}

// handleDeviceApprove records the identity provider's assertion. It is
// authenticated by ARK_IDP_KEY and by nothing else: neither the service token
// nor a credential reaches it, because asserting who someone is is not
// something any client of this service may do.
func (s *Server) handleDeviceApprove(w http.ResponseWriter, r *http.Request) {
	if !s.deviceFlowEnabled() {
		s.deviceUnavailable(w)
		return
	}
	if s.IDPKey == "" {
		writeErr(w, http.StatusUnauthorized, "permission",
			"device approval is not enabled on this service (ARK_IDP_KEY is unset)")
		return
	}
	presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(s.IDPKey)) != 1 {
		writeErr(w, http.StatusUnauthorized, "permission",
			"invalid or missing identity-provider key")
		return
	}

	req, ok := decode[api.DeviceApproveRequest](w, r)
	if !ok {
		return
	}
	userCode, valid := normalizeUserCode(req.UserCode)
	if !valid {
		writeErr(w, http.StatusBadRequest, "validation",
			"user_code must be eight characters of Crockford base32 without vowels, as XXXX-XXXX")
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" {
		writeErr(w, http.StatusBadRequest, "validation",
			"email is required: it is the principal's identity")
		return
	}

	err := s.deviceStore().approve(r.Context(), userCode, req)
	switch {
	case errors.Is(err, errDeviceExpired):
		writeErr(w, http.StatusGone, api.DeviceCodeExpired,
			"no pending login is waiting on that code; it may have expired")
		return
	case errors.Is(err, errPrincipalDisabled):
		writeErr(w, http.StatusConflict, "conflict",
			"a disabled principal holds that email; re-enable it before logging in")
		return
	case err != nil:
		s.internal(w, "approve device code", err)
		return
	}
	if s.Log != nil {
		// The email, not the code: this line says who was asserted, and the
		// redemption line that follows says which principal it resolved to.
		s.Log.Info("device code approved", "email", req.Email,
			"repositories", len(cleanRepositoryIDs(req.RepositoryIDs)))
	}
	writeJSON(w, struct{}{})
}

// deviceUnavailable is the answer of a service with no identity provider
// configured. It is `not_found` rather than `permission` because the route
// genuinely is not there — which is also what a client gets from a service
// too old to have these routes at all, so one branch covers both.
func (s *Server) deviceUnavailable(w http.ResponseWriter) {
	writeErr(w, http.StatusNotFound, "not_found",
		"this service does not offer a device login (ARK_IDP_APPROVAL_URL is unset)")
}

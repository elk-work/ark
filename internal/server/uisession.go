package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
)

const uiCookie = "__Host-ark_ui"
const uiLifetime = 12 * time.Hour

type uiSession struct{ CredentialID, CreatedAt, ExpiresAt string }

func loadUISessions(ctx context.Context, db *sql.DB, snap *authSnapshot) error {
	rows, err := db.QueryContext(ctx, `SELECT token_sha256, credential_id, created_at, expires_at FROM ui_sessions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var hash string
		var session uiSession
		if err := rows.Scan(&hash, &session.CredentialID, &session.CreatedAt, &session.ExpiresAt); err != nil {
			return err
		}
		snap.sessions[hash] = session
	}
	return rows.Err()
}

func (a *authStore) verifyUISession(ctx context.Context, token string) (*authenticated, error) {
	if len(token) != 43 {
		return nil, errNoCredential
	}
	snap, err := a.snapshot(ctx)
	if err != nil {
		return nil, err
	}
	session, ok := snap.sessions[hashCredential(token)]
	if !ok {
		return nil, errNoCredential
	}
	expiry, err := time.Parse(time.RFC3339, session.ExpiresAt)
	if err != nil || !a.now().Before(expiry) {
		return nil, errCredentialExpired
	}
	for _, cred := range snap.credentials {
		if cred.ID == session.CredentialID {
			return a.verifyCredential(snap, cred)
		}
	}
	return nil, errNoCredential
}

func sessionToken(r *http.Request) string {
	c, err := r.Cookie(uiCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) authenticateUI(r *http.Request) (*authenticated, error) {
	if _, present := r.Header["Authorization"]; present {
		return s.authenticate(r)
	}
	return s.authStore().verifyUISession(r.Context(), sessionToken(r))
}

// Cookies are deliberately not wired into auth(): existing writes stay Bearer-only.
func (s *Server) uiAuth(next http.HandlerFunc, html bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		who, err := s.authenticateUI(r)
		if err == nil && who.Legacy {
			err = errNoCredential
		}
		if err != nil {
			if !isAuthRejection(err) {
				s.internal(w, "authenticate browser", err)
				return
			}
			if html {
				s.renderUI(w, uiPage{SignIn: true})
				return
			}
			writeErr(w, http.StatusUnauthorized, "permission", rejectionMessage(err))
			return
		}
		if s.Log != nil {
			s.Log.Info("authenticated", "principal", who.ID, "kind", who.Kind,
				"method", r.Method, "path", r.URL.Path)
		}
		next(w, r.WithContext(withPrincipal(r.Context(), who)))
	}
}

// A trusted TLS-terminating proxy must overwrite X-Forwarded-Proto, not append
// untrusted client input. Direct browser use must be HTTPS as well.
func sameUIOrigin(r *http.Request) bool {
	origin, err := url.Parse(r.Header.Get("Origin"))
	secure := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	return err == nil && secure && origin.Scheme == "https" && origin.Host == r.Host &&
		origin.User == nil && origin.Path == "" && origin.RawQuery == "" && origin.Fragment == ""
}

func setUICookie(w http.ResponseWriter, token string, expires time.Time, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: uiCookie, Value: token, Path: "/", HttpOnly: true,
		Secure: true, SameSite: http.SameSiteStrictMode, Expires: expires, MaxAge: maxAge})
}

func (s *Server) handleUISession(w http.ResponseWriter, r *http.Request) {
	if !sameUIOrigin(r) {
		writeErr(w, 403, "permission", "sign in from this service's HTTPS page")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	if err := r.ParseForm(); err != nil {
		writeErr(w, 400, "validation", "invalid sign-in form")
		return
	}
	credential := strings.TrimSpace(r.PostForm.Get("credential"))
	if !strings.HasPrefix(credential, credentialPrefix) {
		writeErr(w, 401, "permission", "an arkc_ credential is required")
		return
	}
	a := s.authStore()
	who, err := a.verify(r.Context(), credential)
	if err != nil {
		if isAuthRejection(err) {
			writeErr(w, 401, "permission", rejectionMessage(err))
		} else {
			s.internal(w, "sign in", err)
		}
		return
	}
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		s.internal(w, "create session", err)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(bytes)
	now := a.now().UTC()
	expires := now.Add(uiLifetime)
	err = a.update(r.Context(), func(tx *sql.Tx) error {
		// Re-read the fresh CAS copy. A revoke racing login must never be bypassed.
		var active int
		if err := tx.QueryRowContext(r.Context(), `SELECT count(*) FROM credentials c JOIN principals p ON p.id=c.principal_id
   WHERE c.id=? AND c.revoked_at IS NULL AND p.disabled_at IS NULL
   AND (c.expires_at IS NULL OR c.expires_at>?)`, who.CredentialID, now.Format(time.RFC3339)).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return errNoCredential
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM ui_sessions WHERE expires_at<=? OR token_sha256=?`, now.Format(time.RFC3339), hashCredential(sessionToken(r))); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.Context(), `INSERT INTO ui_sessions(token_sha256, credential_id, created_at, expires_at) VALUES(?,?,?,?)`,
			hashCredential(token), who.CredentialID, now.Format(time.RFC3339), expires.Format(time.RFC3339))
		return err
	})
	if err != nil {
		if isAuthRejection(err) {
			writeErr(w, 401, "permission", rejectionMessage(err))
		} else {
			s.internal(w, "save session", err)
		}
		return
	}
	setUICookie(w, token, expires, int(uiLifetime.Seconds()))
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

func (s *Server) handleUILogout(w http.ResponseWriter, r *http.Request) {
	if !sameUIOrigin(r) {
		writeErr(w, 403, "permission", "sign out from this service's HTTPS page")
		return
	}
	if token := sessionToken(r); token != "" {
		err := s.authStore().update(r.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(r.Context(), `DELETE FROM ui_sessions WHERE token_sha256=?`, hashCredential(token))
			return err
		})
		if err != nil {
			s.internal(w, "sign out", err)
			return
		}
	}
	setUICookie(w, "", time.Unix(1, 0), -1)
	http.Redirect(w, r, "/ui/", http.StatusSeeOther)
}

// Board reads deliberately require resolved grants, even under the legacy
// blanket-read default. The switcher and direct URLs expose the same set.
func (s *Server) allowUI(w http.ResponseWriter, r *http.Request, repo string) bool {
	if !records.ValidID(repo) {
		writeErr(w, 400, "validation", "repository must be a full ULID")
		return false
	}
	who, ok := principalFrom(r.Context())
	if !ok || who.Legacy {
		writeErr(w, 401, "permission", "a principal credential is required")
		return false
	}
	snap, err := s.authStore().snapshot(r.Context())
	if err != nil {
		s.internal(w, "read board grants", err)
		return false
	}
	if !atLeast(snap.grants[grantKey(repo, who.ID)].Level, "read") {
		writeErr(w, 403, "permission", refusal(who, repo, "", "read"))
		return false
	}
	return true
}

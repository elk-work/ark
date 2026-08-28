package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/server/repodb"
)

// TestRegistrationRequiresAULID is elk-work/ark#84.
//
// A repository id becomes an object name in storage the service owns, and
// registration is the only route that mints one. The check that ran before
// this was repodb.validRepoID, which rejects the separators and the dot —
// path safety, not identity — so a caller holding a valid credential could
// register "spooky", "admin", or six hundred characters of anything, and the
// service would create and serve repos/<that>.db. Clients mint ULIDs
// (records.NewID), so the service can require one.
func TestRegistrationRequiresAULID(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"a word", "spooky"},
		{"a name a later feature might want", "admin"},
		{"a single character", "x"},
		// Long enough to be nothing like a ULID, short enough to still be a
		// legal filename — so the refusal is provably the ULID check rather
		// than the filesystem refusing the name for us.
		{"far too long", strings.Repeat("A", 100)},
		{"right length, wrong alphabet", "01KX9B83TF2FV51C6K04563FQU"},
		{"one character short", "01KX9B83TF2FV51C6K04563FQ"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := registerBody(t, s, fmt.Sprintf(`{"id":%q,"name":"scout"}`, tc.id))
			if rec.Code != 400 {
				t.Fatalf("register %q: %d %s, want 400", tc.id, rec.Code, rec.Body.String())
			}
			if code := decodeErr(t, rec).Code; code != "validation" {
				t.Errorf("error code %q, want validation", code)
			}
			// The point of refusing is that no object is left behind.
			err := s.Repos.View(context.Background(), tc.id, func(*sql.DB) error { return nil })
			if !errors.Is(err, repodb.ErrNotFound) {
				t.Errorf("view %q after refusal: %v, want ErrNotFound", tc.id, err)
			}
		})
	}
}

// records.ValidID upper-cases before parsing, so a lowercase ULID is still a
// ULID. Worth pinning: tightening a check that runs on every sync must not
// turn into a migration for anyone whose id is not in canonical case.
func TestRegistrationAcceptsAULIDInEitherCase(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
	}{
		{"canonical", repoID},
		{"lowercase", strings.ToLower(repoID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(t)
			rec := registerBody(t, s, fmt.Sprintf(`{"id":%q,"name":"scout"}`, tc.id))
			if rec.Code != 200 {
				t.Fatalf("register %q: %d %s", tc.id, rec.Code, rec.Body.String())
			}
		})
	}
}

// auth.db lives at the reserved key `ark.auth` (elk-work/ark#43). Nothing may
// register a repository that collides with it. That already held, because the
// dot is rejected for path safety — this pins it against the rule that now
// carries it, so the reservation cannot be quietly undermined by loosening
// either check.
func TestRegistrationCannotClaimTheAuthStore(t *testing.T) {
	s := newTestServer(t)
	for _, id := range []string{"ark.auth", "ark_auth", "arkauth"} {
		rec := registerBody(t, s, fmt.Sprintf(`{"id":%q,"name":"scout"}`, id))
		if rec.Code != 400 {
			t.Errorf("register %q: %d %s, want 400", id, rec.Code, rec.Body.String())
		}
	}
}

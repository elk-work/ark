package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

// The #54 acceptance test: ARK_LEGACY_TOKEN, RFC-0003 Stage 3.
//
// The stage exists because retiring the string the whole fleet holds is a
// migration rather than a deploy, so the tests here are about the dial being
// *knowable* as much as about it working: unset must be indistinguishable from
// before, `readonly` must refuse a write in words that name the cutover and
// leave a log line naming the caller, `off` must be the ordinary bad-token
// path, and a typo must not silently mean `full`.

// legacyServer is a registered repository with a human actor in it, running
// in the given mode, and a buffer holding everything the service logged after
// the mode was set.
//
// The setup runs as `full` and the mode is set afterwards, because that is
// what the cutover is: a service that has been serving a fleet rolls a new
// revision with the variable set. Registering a repository under `readonly`
// is itself refused, which is its own test below.
func legacyServer(t *testing.T, mode string) (*authServer, *bytes.Buffer) {
	t.Helper()
	a := newAuthServer(t)
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	seedHuman(t, a.Server)

	logged := &bytes.Buffer{}
	a.Log = slog.New(slog.NewJSONHandler(logged, nil))
	a.LegacyMode = mode
	return a, logged
}

// Unset and `full` are the same thing, and that thing is what the fleet is
// running today. This is the bar the change has to clear before any of the
// rest of it matters: deploying it with the variable unset must change
// nothing, so that turning the dial is the only act with consequences and it
// can be reverted by removing one environment variable.
func TestLegacyModeFullIsTodaysBehaviour(t *testing.T) {
	for _, mode := range []string{"", LegacyModeFull} {
		t.Run("mode="+mode, func(t *testing.T) {
			a, logged := legacyServer(t, mode)
			for _, route := range everyRepositoryRoute {
				rec := doRequestAs(t, a.Server, a.Token, route.method, route.path, route.body)
				if rec.Code == 403 {
					t.Errorf("%s: %d %s, want anything but a refusal", route.name, rec.Code, rec.Body.String())
				}
			}
			// A route may still answer 404 for a record nobody created; what
			// it may not do is refuse, and nothing may be logged as refused.
			if strings.Contains(logged.String(), "legacy token refused") {
				t.Errorf("a refusal was logged on a service that refuses nothing:\n%s", logged.String())
			}
		})
	}
}

// The stage itself: "legacy bearers may pull, not push" (RFC-0003 Stage 3).
func TestLegacyReadonlyReadsAndRefusesEveryWrite(t *testing.T) {
	a, logged := legacyServer(t, LegacyModeReadonly)
	before := decodeMeta(t, getRepo(t, a.Server, repoID))
	beforeRev := revision(t, a.Server)

	for _, route := range everyRepositoryRoute {
		t.Run(route.name, func(t *testing.T) {
			rec := doRequestAs(t, a.Server, a.Token, route.method, route.path, route.body)
			if route.needs == api.GrantRead {
				if rec.Code == 403 {
					t.Fatalf("%s is a read and was refused: %d %s", route.name, rec.Code, rec.Body.String())
				}
				return
			}
			// Everything above `read` — the pushes and record writes, and the
			// two `admin` acts, one of which (the grant roster) is a read the
			// model treats as administration because whoever may see it may
			// change it.
			if rec.Code != 403 {
				t.Fatalf("%s: %d %s, want 403", route.name, rec.Code, rec.Body.String())
			}
			if got := errCode(t, rec); got != "permission" {
				t.Errorf("error code %q, want permission (spec §22 → client exit 5)", got)
			}
			// The refusal has to send the reader to the cutover, not to a
			// grant: `ark repo grant legacy --write` would name a principal
			// that is not a row anywhere and would undo the migration if it
			// worked.
			body := rec.Body.String()
			if !strings.Contains(body, "read-only") || !strings.Contains(body, "RFC-0003") {
				t.Errorf("refusal does not name the cutover: %s", body)
			}
			if strings.Contains(body, "ark repo grant principal legacy") {
				t.Errorf("refusal sends the reader to a grant on the shared token: %s", body)
			}
		})
	}

	// Nothing was written on the way to being refused.
	if after := decodeMeta(t, getRepo(t, a.Server, repoID)); after != before {
		t.Errorf("a refused caller changed the repository: %+v -> %+v", before, after)
	}
	if after := revision(t, a.Server); after != beforeRev {
		t.Errorf("a refused caller minted revisions: %d -> %d", beforeRev, after)
	}

	// And "who has not moved yet?" is answerable from the logs, which is the
	// whole reason the stage is run for two weeks rather than skipped.
	log := logged.String()
	for _, want := range []string{`"principal":"legacy"`, `"mode":"readonly"`, `"op":"POST /v1/sync/push"`} {
		if !strings.Contains(log, want) {
			t.Errorf("refusal log is missing %s:\n%s", want, log)
		}
	}
}

// Registration is the one write that is not refused by the level check, and
// the seam matters in both directions: a legacy bearer's pulls begin with it,
// so refusing it wholesale would make `readonly` indistinguishable from `off`;
// creating a repository is a write, so allowing it wholesale would leave the
// token able to mint objects during the cutover.
func TestLegacyReadonlyRefusesCreationButKeepsTheSyncHandshake(t *testing.T) {
	a, _ := legacyServer(t, LegacyModeReadonly)

	// The call every sync makes, against a repository that already exists.
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("re-registering an existing repository: %d %s, want 200", rec.Code, rec.Body.String())
	}
	// And a pull, which is the thing that handshake exists to precede.
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q}`, repoID)); rec.Code != 200 {
		t.Fatalf("pull: %d %s, want 200", rec.Code, rec.Body.String())
	}

	rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"new"}`, secondRepoID))
	if rec.Code != 403 {
		t.Fatalf("creating a repository: %d %s, want 403", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "permission" {
		t.Errorf("error code %q, want permission", got)
	}
	if !strings.Contains(rec.Body.String(), "read-only") {
		t.Errorf("refusal does not name the cutover: %s", rec.Body.String())
	}
	// The transaction rolled back, so the repository is absent rather than
	// standing there empty.
	if rec := getRepo(t, a.Server, secondRepoID); rec.Code != 404 {
		t.Errorf("a refused creation left something behind: %d %s", rec.Code, rec.Body.String())
	}
}

// `off` is not a refusal, it is a branch that is not registered: the service
// token becomes a bearer nobody minted and takes the same path as any other
// bad token — 401, and the wording clients have been reading since V1.
func TestLegacyOffMakesTheServiceTokenAnUnknownCredential(t *testing.T) {
	a, _ := legacyServer(t, LegacyModeOff)

	for _, route := range everyRepositoryRoute {
		rec := doRequestAs(t, a.Server, a.Token, route.method, route.path, route.body)
		if rec.Code != 401 {
			t.Errorf("%s: %d %s, want 401", route.name, rec.Code, rec.Body.String())
		}
		if got := errCode(t, rec); got != "permission" {
			t.Errorf("%s: error code %q, want permission", route.name, got)
		}
		if !strings.Contains(rec.Body.String(), "invalid or missing token") {
			t.Errorf("%s: the service token is answered differently from any other bad bearer: %s",
				route.name, rec.Body.String())
		}
	}
	// Which is the same answer a string nobody has ever configured gets.
	rec := doRequestAs(t, a.Server, "wrong", "POST", "/v1/sync/pull", `{}`)
	if rec.Code != 401 || !strings.Contains(rec.Body.String(), "invalid or missing token") {
		t.Errorf("an unknown bearer: %d %s", rec.Code, rec.Body.String())
	}
}

// The other half of the acceptance test, and the half the cutover depends on:
// narrowing the shared token must not touch the credentials people have moved
// to. If it did, `readonly` would be an outage rather than a warning.
func TestPrincipalCredentialsAreUnaffectedInEveryMode(t *testing.T) {
	for _, mode := range []string{LegacyModeFull, LegacyModeReadonly, LegacyModeOff} {
		t.Run("mode="+mode, func(t *testing.T) {
			a, cred := grantedServer(t, api.GrantAdmin)
			a.LegacyMode = mode

			for _, route := range everyRepositoryRoute {
				rec := doRequestAs(t, a.Server, cred.Token, route.method, route.path, route.body)
				if rec.Code == 403 || rec.Code == 401 {
					t.Errorf("%s: %d %s, want anything but a refusal", route.name, rec.Code, rec.Body.String())
				}
			}
			// Not only "not refused" — a write that lands.
			body, err := json.Marshal(api.PushRequest{RepositoryID: repoID, ClientID: "c1",
				Actors: []api.Actor{{ID: humanID, Type: "human", Name: "Alice", Email: "alice@example.com"}}})
			if err != nil {
				t.Fatal(err)
			}
			if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/push", string(body)); rec.Code != 200 {
				t.Fatalf("push as a principal: %d %s", rec.Code, rec.Body.String())
			}
			// And a repository of its own can still be created, which is the
			// write the legacy bearer is refused above.
			if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/repositories",
				fmt.Sprintf(`{"id":%q,"name":"new"}`, secondRepoID)); rec.Code != 200 {
				t.Fatalf("register as a principal: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// A misconfiguration must not silently mean `full`. The failure it prevents is
// specific and has already nearly happened: `gcloud run services update …
// --update-env-vars ARK_LEGACY_TOKEN=…` rolls a new revision either way, so a
// value the service ignored would report the riskiest step of the cutover as
// done while changing no authorization behaviour at all.
func TestParseLegacyModeRefusesAnythingButTheThree(t *testing.T) {
	for _, ok := range []struct{ in, want string }{
		{"", LegacyModeFull},
		{LegacyModeFull, LegacyModeFull},
		{LegacyModeReadonly, LegacyModeReadonly},
		{LegacyModeOff, LegacyModeOff},
	} {
		got, err := ParseLegacyMode(ok.in)
		if err != nil || got != ok.want {
			t.Errorf("ParseLegacyMode(%q) = %q, %v; want %q, nil", ok.in, got, err, ok.want)
		}
	}
	// The near misses, which are what an operator actually types.
	for _, bad := range []string{"read-only", "readonly ", "Readonly", "READONLY", "FULL", "none", "true", "1", "yes"} {
		got, err := ParseLegacyMode(bad)
		if err == nil {
			t.Errorf("ParseLegacyMode(%q) = %q, nil; want an error", bad, got)
			continue
		}
		if !strings.Contains(err.Error(), "ARK_LEGACY_TOKEN") {
			t.Errorf("ParseLegacyMode(%q) does not name the variable: %v", bad, err)
		}
		for _, mode := range LegacyModeValues {
			if !strings.Contains(err.Error(), mode) {
				t.Errorf("ParseLegacyMode(%q) does not say %q is accepted: %v", bad, mode, err)
			}
		}
	}
}

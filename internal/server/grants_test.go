package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

// secondRepoID is a repository nobody in these tests creates by accident. It
// is a real ULID, because registration checks (elk-work/ark#84).
const secondRepoID = "01TESTREPTW00000000000000A"

// grantPath addresses one repository's grants.
func grantPath(repoID string) string { return "/v1/repositories/" + repoID + "/grants" }

// grantTo issues a grant using the service token, which carries implicit
// admin everywhere — the break-glass every deployment still has while
// elk-work/ark#54 is open, and the way a founder backfills grants onto
// repositories that predate them.
func grantTo(t *testing.T, a *authServer, repoID, email, level string) api.Grant {
	t.Helper()
	rec := doRequestAs(t, a.Server, a.Token, "POST", grantPath(repoID),
		fmt.Sprintf(`{"email":%q,"level":%q}`, email, level))
	if rec.Code != 200 {
		t.Fatalf("grant %s to %s: %d %s", level, email, rec.Code, rec.Body.String())
	}
	var resp api.GrantResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode grant %q: %v", rec.Body.String(), err)
	}
	return resp.Grant
}

// grantedServer is a registered repository, a human actor in it, and a
// credential holding `level` on it.
func grantedServer(t *testing.T, level string) (*authServer, api.CreatePrincipalResponse) {
	t.Helper()
	a := newAuthServer(t)
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	seedHuman(t, a.Server)
	cred := mintCredentialFor(t, a, "me@example.com")
	if level != "" {
		grantTo(t, a, repoID, "me@example.com", level)
	}
	return a, cred
}

// everyRepositoryRoute is what a principal can attempt against one
// repository, with the level each one requires. The bodies are deliberately
// well-formed: a route that answered 400 before it answered 403 would make
// this test pass for the wrong reason.
var everyRepositoryRoute = []struct {
	name, method, path, body, needs string
}{
	{"pull", "POST", "/v1/sync/pull", `{"repository_id":"` + repoID + `"}`, api.GrantRead},
	{"get repository", "GET", "/v1/repositories/" + repoID, "", api.GrantRead},
	{"get record", "GET", "/v1/repositories/" + repoID + "/records/task/01TESTTASK0000000000000000", "", api.GrantRead},
	{"download url", "POST", "/v1/artifacts/download-url", `{"repository_id":"` + repoID + `"}`, api.GrantRead},
	{"dangling", "GET", "/v1/repositories/" + repoID + "/dangling", "", api.GrantRead},
	{"push", "POST", "/v1/sync/push", `{"repository_id":"` + repoID + `","client_id":"c1","mutations":[]}`, api.GrantWrite},
	{"create task", "POST", "/v1/repositories/" + repoID + "/tasks",
		`{"writer":{"agent_name":"` + agent + `","delegated_by":"` + humanID + `"},"title":"t"}`, api.GrantWrite},
	{"create comment", "POST", "/v1/repositories/" + repoID + "/comments",
		`{"writer":{"agent_name":"` + agent + `","delegated_by":"` + humanID + `"},"parent_type":"task","parent_id":"01TESTTASK0000000000000000","body":"b"}`, api.GrantWrite},
	{"task status", "POST", "/v1/repositories/" + repoID + "/tasks/01TESTTASK0000000000000000/status",
		`{"writer":{"agent_name":"` + agent + `","delegated_by":"` + humanID + `"},"status":"done"}`, api.GrantWrite},
	{"upload url", "POST", "/v1/artifacts/upload-url",
		fmt.Sprintf(`{"repository_id":%q,"sha256":%q,"size_bytes":1}`, repoID, strings.Repeat("a", 64)), api.GrantWrite},
	{"confirm upload", "POST", "/v1/artifacts/confirm",
		fmt.Sprintf(`{"repository_id":%q,"sha256":%q}`, repoID, strings.Repeat("a", 64)), api.GrantWrite},
	{"merge", "POST", "/v1/pull-requests/01TESTPR0000000000000000AA/merge",
		fmt.Sprintf(`{"repository_id":%q,"head_commit_sha":"abc","merge_commit_sha":"def"}`, repoID), api.GrantWrite},
	{"set metadata", "POST", metaPath,
		`{"writer":{"agent_name":"` + agent + `","delegated_by":"` + humanID + `"},"name":"renamed"}`, api.GrantAdmin},
	{"list grants", "GET", grantPath(repoID), "", api.GrantAdmin},
	{"set grant", "POST", grantPath(repoID), `{"email":"someone@example.com","level":"read"}`, api.GrantAdmin},
}

// The first acceptance bullet: a principal with no grant on a repository
// reaches none of it. Not a 404 and not a 500 — `permission`, which the
// client maps to exit 5 (spec §22) without having learned anything new.
func TestAPrincipalWithNoGrantIsRefusedEverywhere(t *testing.T) {
	a, cred := grantedServer(t, "")
	before := decodeMeta(t, getRepo(t, a.Server, repoID))

	for _, route := range everyRepositoryRoute {
		t.Run(route.name, func(t *testing.T) {
			rec := doRequestAs(t, a.Server, cred.Token, route.method, route.path, route.body)
			if rec.Code != 403 {
				t.Fatalf("%s %s: %d %s, want 403", route.method, route.path, rec.Code, rec.Body.String())
			}
			if got := errCode(t, rec); got != "permission" {
				t.Errorf("error code %q, want permission", got)
			}
			// The refusal has to leave the reader somewhere to go: they
			// cannot fix this themselves, so it names who they are, what
			// they are missing, and the command that issues it.
			if body := rec.Body.String(); !strings.Contains(body, "me@example.com") ||
				!strings.Contains(body, "ark repo grant") {
				t.Errorf("refusal does not say what to do about it: %s", body)
			}
		})
	}

	// And nothing was written on the way to being refused: not the name a
	// rename asked for, and not a revision, which every client would pull.
	if after := decodeMeta(t, getRepo(t, a.Server, repoID)); after != before {
		t.Errorf("a refused caller changed the repository: %+v -> %+v", before, after)
	}
}

// The second acceptance bullet, as the levels themselves: read pulls and
// cannot push, write pushes and cannot grant, admin renames.
func TestGrantLevelsAreEnforced(t *testing.T) {
	for _, level := range []string{api.GrantRead, api.GrantWrite, api.GrantAdmin} {
		t.Run(level, func(t *testing.T) {
			a, cred := grantedServer(t, level)
			for _, route := range everyRepositoryRoute {
				rec := doRequestAs(t, a.Server, cred.Token, route.method, route.path, route.body)
				allowed := atLeast(level, route.needs)
				if allowed && rec.Code == 403 {
					t.Errorf("%s holder refused %s: %s", level, route.name, rec.Body.String())
				}
				if !allowed && rec.Code != 403 {
					t.Errorf("%s holder reached %s (needs %s): %d %s",
						level, route.name, route.needs, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// Renaming a repository is the clearest admin-level act on this service, and
// it is what the marker in repometa.go promised this check to. Worth its own
// test rather than a row in a table, because both halves have to be true: the
// writer with `write` is refused, and the same request from an admin lands.
func TestOnlyAnAdminRenamesARepository(t *testing.T) {
	a, cred := grantedServer(t, api.GrantWrite)
	body := `{"writer":{"agent_name":"` + agent + `","delegated_by":"` + humanID + `"},"name":"scout"}`

	rec := doRequestAs(t, a.Server, cred.Token, "POST", metaPath, body)
	if rec.Code != 403 {
		t.Fatalf("a writer renamed the repository: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeMeta(t, getRepo(t, a.Server, repoID)); got.Name != "test" {
		t.Fatalf("the refused rename landed anyway: %+v", got)
	}

	grantTo(t, a, repoID, "me@example.com", api.GrantAdmin)
	if rec := doRequestAs(t, a.Server, cred.Token, "POST", metaPath, body); rec.Code != 200 {
		t.Fatalf("an admin was refused the rename: %d %s", rec.Code, rec.Body.String())
	}
	if got := decodeMeta(t, getRepo(t, a.Server, repoID)); got.Name != "scout" {
		t.Errorf("after the rename: %+v", got)
	}
}

// The bootstrap rule: the principal whose registration brings a repository
// into existence administers it, and nobody else has anything.
func TestFirstWriterRegisters(t *testing.T) {
	a := newAuthServer(t)
	founder := mintCredentialFor(t, a, "founder@example.com")
	other := mintCredentialFor(t, a, "other@example.com")

	body := fmt.Sprintf(`{"id":%q,"name":"scout"}`, secondRepoID)
	if rec := doRequestAs(t, a.Server, founder.Token, "POST", "/v1/repositories", body); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	// Admin, which is the level that can prove itself: only an admin lists.
	rec := doRequestAs(t, a.Server, founder.Token, "GET", grantPath(secondRepoID), "")
	if rec.Code != 200 {
		t.Fatalf("the founder does not administer what it created: %d %s", rec.Code, rec.Body.String())
	}
	var list api.GrantListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Grants) != 1 || list.Grants[0].Level != api.GrantAdmin ||
		list.Grants[0].PrincipalID != founder.Principal.ID {
		t.Fatalf("grants after creation: %+v", list.Grants)
	}

	// Everyone else has no access until granted — including to the
	// registration call itself, which every sync makes.
	for _, route := range []struct{ method, path, body string }{
		{"POST", "/v1/repositories", body},
		{"POST", "/v1/sync/pull", fmt.Sprintf(`{"repository_id":%q}`, secondRepoID)},
	} {
		rec := doRequestAs(t, a.Server, other.Token, route.method, route.path, route.body)
		if rec.Code != 403 {
			t.Errorf("%s %s reached a repository it was never granted: %d %s",
				route.method, route.path, rec.Code, rec.Body.String())
		}
	}

	// A reader may register, because registration is the call every sync
	// makes before it pulls — refusing it would be refusing the pull.
	grantTo(t, a, secondRepoID, "other@example.com", api.GrantRead)
	if rec := doRequestAs(t, a.Server, other.Token, "POST", "/v1/repositories", body); rec.Code != 200 {
		t.Errorf("a reader could not register before pulling: %d %s", rec.Code, rec.Body.String())
	}
}

// A grant is issued to an email, and it is waiting when that address first
// logs in. This is what makes the outside-contributor story work: the grant
// exists before the contributor does, and no credential is passed
// person-to-person (RFC-0003 Decision 4).
func TestAGrantIsIssuedBeforeItsGranteeExists(t *testing.T) {
	a := newAuthServer(t)
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}

	// Nobody holds this address yet, so the grant waits — and says so.
	pending := grantTo(t, a, repoID, "Newcomer@Example.com", api.GrantWrite)
	if !pending.Pending || pending.PrincipalID != "" {
		t.Fatalf("a grant to an unknown address resolved to somebody: %+v", pending)
	}

	// The login a service with no identity provider has. Case differs from
	// the address the grant was issued to, on purpose: one was typed by a
	// human and one is asserted by whatever issues identities.
	cred := mintCredentialFor(t, a, "newcomer@example.com")
	rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q}`, repoID))
	if rec.Code != 200 {
		t.Fatalf("the grant did not resolve at first login: %d %s", rec.Code, rec.Body.String())
	}

	// And it resolved into a real grant rather than staying a promise.
	for _, g := range listGrants(t, a, repoID) {
		if g.Email == "newcomer@example.com" {
			if g.Pending || g.PrincipalID != cred.Principal.ID || g.Level != api.GrantWrite {
				t.Errorf("resolved grant: %+v", g)
			}
			return
		}
	}
	t.Errorf("the grant vanished on resolution: %+v", listGrants(t, a, repoID))
}

// The other first login, and the one RFC-0003 actually describes: the device
// flow. A grant issued to an address before anybody held it has to resolve
// there too, or `ark repo grant` would work for a self-hoster and silently do
// nothing for everyone whose credential comes from an identity provider.
//
// The ordering is the substance. Seeding adds `read` and leaves an existing
// row alone, so if it ran before the claim, an admin's `write` would arrive to
// find a seeded `read` already sitting in its place — and stay lost.
func TestADeviceLoginResolvesAGrantIssuedToTheEmail(t *testing.T) {
	a := newDeviceServer(t)
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"test"}`, repoID)); rec.Code != 200 {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if pending := grantTo(t, a, repoID, "newcomer@example.com", api.GrantWrite); !pending.Pending {
		t.Fatalf("nobody holds that address yet: %+v", pending)
	}

	// Log in, with the identity provider seeding `read` on the very same
	// repository — the case where the two rules meet.
	code := requestDeviceCode(t, a)
	if rec := approve(t, a, testIDPKey, api.DeviceApproveRequest{
		UserCode: code.UserCode, Subject: "sub-newcomer",
		Email: "newcomer@example.com", RepositoryIDs: []string{repoID},
	}); rec.Code != 200 {
		t.Fatalf("approve: %d %s", rec.Code, rec.Body.String())
	}
	rec := poll(t, a, code.DeviceCode)
	if rec.Code != 200 {
		t.Fatalf("poll: %d %s", rec.Code, rec.Body.String())
	}
	var out api.DeviceTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	// The admin's `write` survived the seed, and it is what the credential
	// carries: a push, not only a pull.
	if rec := doRequestAs(t, a.Server, out.Token, "POST", "/v1/sync/push",
		fmt.Sprintf(`{"repository_id":%q,"client_id":"c1","mutations":[]}`, repoID)); rec.Code != 200 {
		t.Fatalf("the issued grant did not survive the login: %d %s", rec.Code, rec.Body.String())
	}
	got := listGrants(t, a, repoID)
	if len(got) != 1 || got[0].Level != api.GrantWrite || got[0].Pending ||
		got[0].PrincipalID != out.Principal.ID {
		t.Errorf("grants after the login: %+v", got)
	}
}

// listGrants reads a repository's grants with the service token.
func listGrants(t *testing.T, a *authServer, repoID string) []api.Grant {
	t.Helper()
	rec := doRequestAs(t, a.Server, a.Token, "GET", grantPath(repoID), "")
	if rec.Code != 200 {
		t.Fatalf("list grants: %d %s", rec.Code, rec.Body.String())
	}
	var list api.GrantListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode grants %q: %v", rec.Body.String(), err)
	}
	return list.Grants
}

// Revoking is the other half of granting: an access model that cannot take
// something back is not one. It is idempotent, the way `ark logout` is on a
// machine that never logged in (spec §20).
func TestAGrantCanBeRevoked(t *testing.T) {
	a, cred := grantedServer(t, api.GrantWrite)
	pull := fmt.Sprintf(`{"repository_id":%q}`, repoID)

	if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", pull); rec.Code != 200 {
		t.Fatalf("granted principal refused: %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 2; i++ {
		rec := doRequestAs(t, a.Server, a.Token, "POST", grantPath(repoID),
			`{"email":"me@example.com","revoke":true}`)
		if rec.Code != 200 {
			t.Fatalf("revoke %d: %d %s", i, rec.Code, rec.Body.String())
		}
	}
	if rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull", pull); rec.Code != 403 {
		t.Errorf("a revoked grant still pulls: %d %s", rec.Code, rec.Body.String())
	}
	if got := listGrants(t, a, repoID); len(got) != 0 {
		t.Errorf("grants after revocation: %+v", got)
	}
}

// ARK_DEFAULT_GRANT is a deliberate setting rather than a discovered one
// (RFC-0003, resolved decision 2). `read` makes every authenticated principal
// a reader of everything; it never confers write, and the default — `seeded`,
// with nothing seeding — is deny.
func TestDefaultGrant(t *testing.T) {
	cases := map[string]struct{ pull, push int }{
		"":                 {403, 403},
		DefaultGrantSeeded: {403, 403},
		DefaultGrantNone:   {403, 403},
		DefaultGrantRead:   {200, 403},
	}
	for policy, want := range cases {
		t.Run("policy="+policy, func(t *testing.T) {
			a, cred := grantedServer(t, "")
			a.DefaultGrant = policy
			pull := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull",
				fmt.Sprintf(`{"repository_id":%q}`, repoID))
			if pull.Code != want.pull {
				t.Errorf("pull: %d, want %d (%s)", pull.Code, want.pull, pull.Body.String())
			}
			push := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/push",
				fmt.Sprintf(`{"repository_id":%q,"client_id":"c1","mutations":[]}`, repoID))
			if push.Code != want.push {
				t.Errorf("push: %d, want %d (%s)", push.Code, want.push, push.Body.String())
			}
		})
	}
}

// The bullet that protects the live fleet: six repositories sync against this
// service on the shared token today, none of them has a grant, and none of
// them may notice that grants exist. The legacy bearer carries implicit admin
// everywhere until elk-work/ark#54 retires it.
func TestTheLegacyServiceTokenIsUnaffectedByGrants(t *testing.T) {
	a, _ := grantedServer(t, "")
	for _, route := range everyRepositoryRoute {
		rec := doRequestAs(t, a.Server, a.Token, route.method, route.path, route.body)
		if rec.Code == 403 {
			t.Errorf("the service token was refused %s: %s", route.name, rec.Body.String())
		}
	}
}

// A repository this service does not hold is not an authorization question.
// Answering it as one would break two things at once: `ark login` verifies a
// credential by pulling an id that cannot exist and reading the 404, and a
// repository the service has *lost* is detected by that same 404 (§19,
// elk-work/ark#66).
func TestAnAbsentRepositoryIsNotFoundRatherThanForbidden(t *testing.T) {
	a, cred := grantedServer(t, "")
	rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q}`, secondRepoID))
	if rec.Code != 404 {
		t.Fatalf("pull of an unknown repository: %d %s, want 404", rec.Code, rec.Body.String())
	}
}

func TestGrantRouteValidation(t *testing.T) {
	a, _ := grantedServer(t, "")
	for name, body := range map[string]string{
		"no email":      `{"level":"read"}`,
		"blank email":   `{"email":"  ","level":"read"}`,
		"no level":      `{"email":"x@example.com"}`,
		"unknown level": `{"email":"x@example.com","level":"owner"}`,
		"not even JSON": `{`,
	} {
		rec := doRequestAs(t, a.Server, a.Token, "POST", grantPath(repoID), body)
		if rec.Code != 400 {
			t.Errorf("%s: %d %s, want 400", name, rec.Code, rec.Body.String())
		}
	}
	// A grant on a repository that does not exist is a typo in a ULID, and
	// answering it with a grant on nothing would look exactly like success.
	rec := doRequestAs(t, a.Server, a.Token, "POST", grantPath(secondRepoID),
		`{"email":"x@example.com","level":"read"}`)
	if rec.Code != 404 {
		t.Errorf("granting on an unregistered repository: %d %s, want 404", rec.Code, rec.Body.String())
	}
}

// A grant is scoped to one repository and says nothing about any other.
func TestAGrantDoesNotTravelBetweenRepositories(t *testing.T) {
	a, cred := grantedServer(t, api.GrantAdmin)
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"other"}`, secondRepoID)); rec.Code != 200 {
		t.Fatalf("register the second repository: %d %s", rec.Code, rec.Body.String())
	}
	rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/pull",
		fmt.Sprintf(`{"repository_id":%q}`, secondRepoID))
	if rec.Code != 403 {
		t.Errorf("admin on one repository read another: %d %s", rec.Code, rec.Body.String())
	}
}

// Blobs are addressed by content hash in a store shared across every
// repository, so `read` on one repository must not sign a URL for another
// one's artifact. This was unreachable while one token reached everything; a
// grant is what makes it reachable, so it is this slice's to close.
func TestADownloadURLIsScopedToTheRepositoryThatHoldsTheBlob(t *testing.T) {
	a, cred := grantedServer(t, api.GrantRead)
	sha := strings.Repeat("a", 64)
	key := "sha256/" + sha[:2] + "/" + sha

	// The blob belongs to a second repository, which this principal has no
	// grant on at all.
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories",
		fmt.Sprintf(`{"id":%q,"name":"other"}`, secondRepoID)); rec.Code != 200 {
		t.Fatalf("register the second repository: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRequestAs(t, a.Server, a.Token, "POST", "/v1/artifacts/upload-url",
		fmt.Sprintf(`{"repository_id":%q,"sha256":%q,"size_bytes":1}`, secondRepoID, sha)); rec.Code != 200 {
		t.Fatalf("register the blob: %d %s", rec.Code, rec.Body.String())
	}

	// Asking for it against the repository this principal *can* read must not
	// hand over a URL: the repository it names holds no such artifact.
	rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/artifacts/download-url",
		fmt.Sprintf(`{"repository_id":%q,"storage_key":%q}`, repoID, key))
	if rec.Code != 404 {
		t.Fatalf("a reader signed a URL for another repository's blob: %d %s", rec.Code, rec.Body.String())
	}
	// And naming the repository that does hold it is refused for the ordinary
	// reason.
	rec = doRequestAs(t, a.Server, cred.Token, "POST", "/v1/artifacts/download-url",
		fmt.Sprintf(`{"repository_id":%q,"storage_key":%q}`, secondRepoID, key))
	if rec.Code != 403 {
		t.Errorf("naming the holding repository: %d %s, want 403", rec.Code, rec.Body.String())
	}
	// The service token gets past the scoping check on the repository that
	// does hold it. (It still fails to sign, because no content was ever
	// PUT to the blob store in this test — a different refusal, which is the
	// point: the repository is not what refused it.)
	rec = doRequestAs(t, a.Server, a.Token, "POST", "/v1/artifacts/download-url",
		fmt.Sprintf(`{"repository_id":%q,"storage_key":%q}`, secondRepoID, key))
	if strings.Contains(rec.Body.String(), "holds no artifact") {
		t.Errorf("the repository that stored the blob denied holding it: %s", rec.Body.String())
	}
}

// Re-granting is a correction, not a second grant: one row per principal per
// repository, at whatever level was issued last.
func TestRegrantingCorrectsTheLevel(t *testing.T) {
	a, cred := grantedServer(t, api.GrantAdmin)
	grantTo(t, a, repoID, "me@example.com", api.GrantRead)

	if got := listGrants(t, a, repoID); len(got) != 1 || got[0].Level != api.GrantRead {
		t.Fatalf("after re-granting: %+v", got)
	}
	rec := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/sync/push",
		fmt.Sprintf(`{"repository_id":%q,"client_id":"c1","mutations":[]}`, repoID))
	if rec.Code != 403 {
		t.Errorf("a lowered grant still pushes: %d %s", rec.Code, rec.Body.String())
	}
}

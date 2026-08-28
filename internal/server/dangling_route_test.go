package server

// GET /v1/repositories/{repo}/dangling — elk-work/ark#74.
//
// #77 wrote the ledger and nothing read it, so the outstanding set was
// defined by a query in the spec that an operator could only run against a
// copy of `repos/<id>.db`. What follows covers what the route answers and the
// three ways it is allowed to be wrong: reporting a reference that has since
// resolved as a defect, hiding a defect behind a truncated listing, and
// letting somebody read a repository they hold no grant on.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/elk-work/ark/pkg/api"
)

func danglingPath(repoID string) string { return "/v1/repositories/" + repoID + "/dangling" }

// getDangling calls the route with the service token and decodes the answer.
func getDangling(t *testing.T, s *Server, query string) api.DanglingResponse {
	t.Helper()
	rec := doRequest(t, s, "GET", danglingPath(repoID)+query, "")
	if rec.Code != 200 {
		t.Fatalf("GET dangling%s: %d %s", query, rec.Code, rec.Body.String())
	}
	var resp api.DanglingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode dangling %q: %v", rec.Body.String(), err)
	}
	return resp
}

// TestDanglingRouteServesTheOutstandingSet is the issue's own reproduction
// read back over HTTP: the push that leaves exactly one outstanding entry,
// and a route that names it without anybody holding the `.db` file.
func TestDanglingRouteServesTheOutstandingSet(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)
	commentOnAbsentTask(t, s)

	resp := getDangling(t, s, "")
	if resp.RepositoryID != repoID {
		t.Errorf("repository_id = %q, want %q", resp.RepositoryID, repoID)
	}
	if resp.Outstanding != 1 || resp.Recorded != 1 {
		t.Errorf("outstanding = %d, recorded = %d, want 1 and 1", resp.Outstanding, resp.Recorded)
	}
	if len(resp.References) != 1 {
		t.Fatalf("references = %+v, want one", resp.References)
	}
	got := resp.References[0]
	if got.FirstSeenAt == "" {
		t.Error("first_seen_at is empty: the age of an entry is what separates skew from loss")
	}
	// Every column the ledger holds reaches the reader. The mutation id in
	// particular: without it an operator cannot find the push that carried
	// the orphan, which is the thread back to the client that sent it.
	want := api.DanglingReference{RecordType: "comment", RecordID: "c1", Field: "parent_id",
		ParentType: "task", ParentID: absentTask, MutationID: "m2", FirstSeenAt: got.FirstSeenAt}
	if got != want {
		t.Errorf("reference = %+v, want %+v", got, want)
	}
	if resp.ServerRevision == 0 {
		t.Error("server_revision = 0: the answer is a comparison made at a moment and should date itself")
	}
	if resp.Truncated {
		t.Error("one entry reported as truncated")
	}
}

// The set is defined by comparison, not by a stamp (§9.1), so a reference
// whose parent has since arrived leaves the outstanding set the moment it
// does — and stays in the ledger, because how often a repository sees this
// skew is worth knowing. Both halves are the route's job to tell apart.
func TestDanglingRouteExcludesAReferenceWhoseParentArrived(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)
	commentOnAbsentTask(t, s)
	push(t, s, mut("m3", "task", absentTask, "create", 0,
		fmt.Sprintf(`{"id":%q,"number":1,"title":"A","status":"open"}`, absentTask)))

	resp := getDangling(t, s, "")
	if resp.Outstanding != 0 {
		t.Errorf("outstanding = %d after the parent landed, want 0", resp.Outstanding)
	}
	if len(resp.References) != 0 {
		t.Errorf("the default listing still names a resolved reference: %+v", resp.References)
	}
	// The entry itself survives, and `all` is how it is read.
	if resp.Recorded != 1 {
		t.Errorf("recorded = %d, want the entry kept as history", resp.Recorded)
	}

	all := getDangling(t, s, "?all=true")
	if len(all.References) != 1 {
		t.Fatalf("--all listing = %+v, want the resolved entry", all.References)
	}
	if !all.References[0].Resolved {
		t.Error("a resolved entry is not marked resolved, so a reader cannot tell it from a defect")
	}
	if all.Outstanding != 0 || all.Recorded != 1 {
		t.Errorf("--all moved the counts: outstanding = %d, recorded = %d", all.Outstanding, all.Recorded)
	}
}

// twoDangling leaves two outstanding entries, in a known order: the listing
// is oldest-first and both land inside the same second, so the record key
// breaks the tie — `agent_thread` before `comment`.
func twoDangling(t *testing.T, s *Server) {
	t.Helper()
	push(t, s,
		mut("m1", "comment", "c1", "create", 0,
			`{"id":"c1","parent_type":"task","parent_id":"absent-task","body":"b"}`),
		mut("m2", "agent_thread", "th1", "create", 0,
			`{"id":"th1","task_id":"absent-task","title":"T","status":"open"}`),
	)
}

// A truncated listing must not be able to make a repository look healthier
// than it is: the counts describe the repository, `limit` describes the page.
func TestDanglingRouteTruncatesTheListingAndNotTheCount(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)
	twoDangling(t, s)

	resp := getDangling(t, s, "?limit=1")
	if !resp.Truncated {
		t.Error("a cut listing did not report itself as truncated")
	}
	if resp.Outstanding != 2 || resp.Recorded != 2 {
		t.Errorf("limit moved the counts: outstanding = %d, recorded = %d", resp.Outstanding, resp.Recorded)
	}
	if len(resp.References) != 1 {
		t.Fatalf("limit=1 returned %d entries", len(resp.References))
	}
	// Oldest first, and the record key inside a second — so which entry
	// survives truncation is defined rather than whatever SQLite returned.
	if got := resp.References[0].RecordType; got != "agent_thread" {
		t.Errorf("first entry is %q, want the listing ordered deterministically", got)
	}

	full := getDangling(t, s, "?limit=2")
	if full.Truncated || len(full.References) != 2 {
		t.Errorf("limit=2 over two entries: truncated=%v, %d entries", full.Truncated, len(full.References))
	}
}

// A mistyped parameter is refused rather than read as a default. `?all=ture`
// silently listing the outstanding set instead of everything is the failure
// this is here to stop, and it would look exactly like success.
func TestDanglingRouteValidatesItsParameters(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	for _, query := range []string{"?all=ture", "?limit=0", "?limit=-1", "?limit=nine", "?limit=100000"} {
		rec := doRequest(t, s, "GET", danglingPath(repoID)+query, "")
		if rec.Code != 400 {
			t.Errorf("GET dangling%s: %d %s, want 400", query, rec.Code, rec.Body.String())
		}
		if got := errCode(t, rec); got != "validation" {
			t.Errorf("GET dangling%s: error code %q, want validation", query, got)
		}
	}
	// `?all` with no value is what a person types by hand, and it means yes.
	if rec := doRequest(t, s, "GET", danglingPath(repoID)+"?all", ""); rec.Code != 200 {
		t.Errorf("GET dangling?all: %d %s", rec.Code, rec.Body.String())
	}
}

// A repository this service does not hold answers `not_found`, like every
// other route addressed by repository id. Answering a permission error there
// would hide a lost repository behind a plausible refusal (§19, #66).
func TestDanglingRouteOnAnUnknownRepositoryIsNotFound(t *testing.T) {
	s := newTestServer(t)
	registerRepo(t, s)

	rec := doRequest(t, s, "GET", danglingPath(secondRepoID), "")
	if rec.Code != 404 {
		t.Fatalf("dangling on an unregistered repository: %d %s, want 404", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "not_found" {
		t.Errorf("error code %q, want not_found", got)
	}
}

// The grant check, on its own rather than only as a row in
// everyRepositoryRoute: a dangling reference names record ids in a
// repository, so reading it is reading the repository. `read` is the level
// that pulls records, and nobody without one gets here.
func TestDanglingRouteNeedsReadOnTheRepository(t *testing.T) {
	a, cred := grantedServer(t, "")

	rec := doRequestAs(t, a.Server, cred.Token, "GET", danglingPath(repoID), "")
	if rec.Code != 403 {
		t.Fatalf("a principal with no grant read the ledger: %d %s", rec.Code, rec.Body.String())
	}
	if got := errCode(t, rec); got != "permission" {
		t.Errorf("error code %q, want permission", got)
	}
	// The refusal names who they are and the command that fixes it, because
	// they cannot fix it themselves.
	if body := rec.Body.String(); !strings.Contains(body, "me@example.com") ||
		!strings.Contains(body, "ark repo grant") {
		t.Errorf("refusal does not say what to do about it: %s", body)
	}

	// And `read` is enough — not `admin`. `ark repo grants` is admin because
	// that list is a roster of email addresses; this is repository content,
	// and putting it behind admin would be the locked room again in a
	// smaller size.
	grantTo(t, a, repoID, "me@example.com", api.GrantRead)
	if rec := doRequestAs(t, a.Server, cred.Token, "GET", danglingPath(repoID), ""); rec.Code != 200 {
		t.Fatalf("a reader was refused the ledger: %d %s", rec.Code, rec.Body.String())
	}

	// The legacy service token carries implicit admin everywhere until #54
	// retires it, and the whole live fleet is still on it.
	if rec := doRequestAs(t, a.Server, a.Token, "GET", danglingPath(repoID), ""); rec.Code != 200 {
		t.Errorf("the service token was refused the ledger: %d %s", rec.Code, rec.Body.String())
	}
}

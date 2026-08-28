package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// `ark repo dangling` — elk-work/ark#74.
//
// The substance of the answer is the service's and is tested there
// (internal/server/dangling_route_test.go), against a real push that leaves a
// real orphan. What is under test here is the surface: that the command
// reaches the route it claims to, renders what comes back in a way that says
// what it means, and degrades the way a command that needs a service has to
// when there is not one.

// danglingStub serves the one route, records the query it was asked, and
// answers with a canned response.
func danglingStub(t *testing.T, resp api.DanglingResponse) (base string, asked *url.Values) {
	t.Helper()
	var seen url.Values
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/dangling") || r.Method != http.MethodGet {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		seen = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(ts.Close)
	t.Setenv("ARK_TOKEN", "test-token")
	return ts.URL, &seen
}

// stubbedRepo is a checkout pointed at a stub rather than at a service.
func stubbedRepo(t *testing.T, resp api.DanglingResponse) (dir string, asked *url.Values) {
	t.Helper()
	base, asked := danglingStub(t, resp)
	dir = gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", base)
	return dir, asked
}

// oneOutstanding is the shape of the issue's own reproduction: a comment the
// service is serving on a task it does not hold.
func oneOutstanding() api.DanglingResponse {
	return api.DanglingResponse{
		RepositoryID: "01TESTREP00000000000000000",
		References: []api.DanglingReference{{
			RecordType: "comment", RecordID: "01K3COMMENT0000000000000AA",
			Field:      "parent_id",
			ParentType: "task", ParentID: "01K3TASK00000000000000000A",
			MutationID:  "01K3MUTATION000000000000AA",
			FirstSeenAt: records.Now(),
		}},
		Outstanding: 1, Recorded: 1, ServerRevision: 12,
	}
}

// TestRepoDanglingRendersTheDefect is the whole point of the command: a
// person reads it and knows what is wrong, what it means, and that they very
// likely cannot fix it — the missing record is on a machine they do not have.
func TestRepoDanglingRendersTheDefect(t *testing.T) {
	dir, asked := stubbedRepo(t, oneOutstanding())

	out := ark(t, dir, "repo", "dangling")
	for _, want := range []string{
		"01K3COMMENT0000000000000AA", // what was accepted
		"parent_id",                  // the field that pointed
		"01K3TASK00000000000000000A", // what it named and does not resolve to
		"1 outstanding",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not name %q:\n%s", want, out)
		}
	}
	// Said in words, not left as a row in a table. An outstanding entry is a
	// defect (§9.1) and the reader is owed what it means.
	if !strings.Contains(out, "no client can render it") {
		t.Errorf("output does not say what an entry means:\n%s", out)
	}
	// And it is one story with the client-side count, not a second vocabulary
	// for the same skew.
	if !strings.Contains(out, "held records") {
		t.Errorf("output does not connect to `ark status`:\n%s", out)
	}
	// The default asks for the outstanding set, which is the question worth
	// asking: resolved entries are history.
	if asked.Has("all") {
		t.Errorf("the default listing asked for everything: %v", *asked)
	}
}

// The `--json` shape is a stable interface for agents (CONTRIBUTING.md), so
// it is asserted field by field rather than through the command's own types.
func TestRepoDanglingJSONShape(t *testing.T) {
	dir, _ := stubbedRepo(t, oneOutstanding())

	var got struct {
		RepositoryID string `json:"repository_id"`
		Outstanding  int    `json:"outstanding"`
		Recorded     int    `json:"recorded"`
		Revision     int64  `json:"server_revision"`
		References   []struct {
			RecordType  string `json:"record_type"`
			RecordID    string `json:"record_id"`
			Field       string `json:"field"`
			ParentType  string `json:"parent_type"`
			ParentID    string `json:"parent_id"`
			MutationID  string `json:"mutation_id"`
			FirstSeenAt string `json:"first_seen_at"`
			Resolved    bool   `json:"resolved"`
		} `json:"references"`
	}
	arkJSON(t, dir, &got, "repo", "dangling")
	if got.Outstanding != 1 || got.Recorded != 1 || got.Revision != 12 {
		t.Errorf("counts: %+v", got)
	}
	if len(got.References) != 1 {
		t.Fatalf("references = %+v", got.References)
	}
	ref := got.References[0]
	if ref.RecordType != "comment" || ref.Field != "parent_id" || ref.ParentType != "task" ||
		ref.MutationID == "" || ref.FirstSeenAt == "" || ref.Resolved {
		t.Errorf("reference = %+v", ref)
	}
}

// Nothing wrong is an answer, and it has to look like one. An empty table
// looks the same as a command that failed quietly.
func TestRepoDanglingSaysWhenNothingIsWrong(t *testing.T) {
	dir, _ := stubbedRepo(t, api.DanglingResponse{
		RepositoryID:   "01TESTREP00000000000000000",
		References:     []api.DanglingReference{},
		ServerRevision: 4,
	})
	out := ark(t, dir, "repo", "dangling")
	if !strings.Contains(out, "Nothing dangling") {
		t.Errorf("a clean repository reported nothing readable:\n%s", out)
	}
}

// --all reaches the service as the parameter the route reads, and a resolved
// entry is rendered as history rather than as a defect.
func TestRepoDanglingAllListsResolvedEntries(t *testing.T) {
	resp := oneOutstanding()
	resp.References[0].Resolved = true
	resp.Outstanding, resp.Recorded = 0, 1
	dir, asked := stubbedRepo(t, resp)

	out := ark(t, dir, "repo", "dangling", "--all")
	if asked.Get("all") != "true" {
		t.Errorf("--all did not reach the service: %v", *asked)
	}
	if !strings.Contains(out, "resolved") {
		t.Errorf("a resolved entry is not marked as one:\n%s", out)
	}
	if strings.Contains(out, "no client can render it") {
		t.Errorf("a repository with nothing outstanding was told it has a defect:\n%s", out)
	}
}

// --limit reaches the service, and a listing the service cut short says so
// rather than letting a reader take it for the whole set.
func TestRepoDanglingReportsATruncatedListing(t *testing.T) {
	resp := oneOutstanding()
	resp.Outstanding, resp.Recorded, resp.Truncated = 40, 40, true
	dir, asked := stubbedRepo(t, resp)

	out := ark(t, dir, "repo", "dangling", "--limit", "1")
	if asked.Get("limit") != "1" {
		t.Errorf("--limit did not reach the service: %v", *asked)
	}
	if !strings.Contains(out, "40 outstanding") {
		t.Errorf("the count of the whole repository is missing:\n%s", out)
	}
	if !strings.Contains(out, "--limit") {
		t.Errorf("a truncated listing did not say how to see the rest:\n%s", out)
	}
}

// A limit the service would refuse is refused here, before a round trip:
// exit 2, the validation code (spec §22).
func TestRepoDanglingRefusesAnImpossibleLimit(t *testing.T) {
	dir, _ := stubbedRepo(t, oneOutstanding())
	out, err := arkErr(t, dir, "repo", "dangling", "--limit", "100000")
	if err == nil {
		t.Fatalf("an out-of-range limit was accepted:\n%s", out)
	}
	if code := records.ExitCode(err); code != 2 {
		t.Errorf("exit code = %d, want 2 (validation): %v", code, err)
	}
}

// Degrading: this command needs a service, and the two ways there is not one
// are both exit 6 (offline). Neither may be answered with a zero, which would
// report a repository as clean on no evidence at all.
func TestRepoDanglingWithoutAReachableService(t *testing.T) {
	t.Run("no remote", func(t *testing.T) {
		dir := gitRepo(t)
		ark(t, dir, "init")
		_, err := arkErr(t, dir, "repo", "dangling")
		if err == nil {
			t.Fatal("dangling succeeded with no remote configured")
		}
		if code := records.ExitCode(err); code != 6 {
			t.Errorf("exit code = %d, want 6 (offline): %v", code, err)
		}
		if !strings.Contains(err.Error(), "remote") {
			t.Errorf("the error does not name what is missing: %v", err)
		}
	})

	t.Run("unreachable service", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		base := ts.URL
		ts.Close()
		t.Setenv("ARK_TOKEN", "test-token")
		dir := gitRepo(t)
		ark(t, dir, "init")
		ark(t, dir, "remote", "set", base)

		_, err := arkErr(t, dir, "repo", "dangling")
		if err == nil {
			t.Fatal("dangling succeeded against a service that is not there")
		}
		if code := records.ExitCode(err); code != 6 {
			t.Errorf("exit code = %d, want 6 (offline): %v", code, err)
		}
	})
}

// TestRepoDanglingAgainstTheRealService proves the wire rather than the
// rendering: the path, the bearer, the query and the response shape all agree
// with the handler, which a stub written from the same head cannot show.
func TestRepoDanglingAgainstTheRealService(t *testing.T) {
	dir, _ := syncedRepo(t)

	var got api.DanglingResponse
	arkJSON(t, dir, &got, "repo", "dangling")
	if got.RepositoryID != repoIDOf(t, dir) {
		t.Errorf("repository_id = %q, want this checkout's %q", got.RepositoryID, repoIDOf(t, dir))
	}
	if got.Outstanding != 0 || got.Recorded != 0 {
		t.Errorf("a freshly synced repository has dangling references: %+v", got)
	}
	if got.ServerRevision == 0 {
		t.Error("server_revision = 0 from a repository that has been pushed to")
	}
	if out := ark(t, dir, "repo", "dangling"); !strings.Contains(out, "Nothing dangling") {
		t.Errorf("human rendering against the real service:\n%s", out)
	}
}

// The two halves of one skew, joined where somebody is already looking.
//
// `ark status` counts the records this checkout is holding back (#89); only
// the service can say whether the records they are waiting for exist
// anywhere, which is the difference between waiting and waiting forever. So
// status names the command that asks — and does not ask, because status
// answers about a checkout and must not acquire a network round trip.
func TestStatusPointsAtTheServiceSideOfAHeldRecord(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	holdARecord(t, dir)

	// Nothing to ask, so nothing offered.
	if out := ark(t, dir, "status"); strings.Contains(out, "repo dangling") {
		t.Errorf("a checkout with no remote was pointed at the service:\n%s", out)
	}

	reached := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	ark(t, dir, "remote", "set", ts.URL)

	out := ark(t, dir, "status")
	if !strings.Contains(out, "held") {
		t.Fatalf("status stopped reporting held records:\n%s", out)
	}
	if !strings.Contains(out, "ark repo dangling") {
		t.Errorf("status does not name where the other half of this is:\n%s", out)
	}
	if reached {
		t.Error("`ark status` made a network call; it answers about a checkout and must not")
	}
}

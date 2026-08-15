package workrecord

import (
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

var testRepo = Repo{ID: "01REPO", Name: "signal"}

// testActors builds the identity shape every Ark repository actually has:
// one human, and one agent acting under that human's authority.
func testActors() *Actors {
	return NewActors([]api.Actor{
		{ID: "01HUMAN", Type: "human", Name: "ijroth", Email: "ijroth@example.com"},
		{ID: "01AGENT", Type: "agent", Name: "claude", AgentName: "claude", DelegatedBy: "01HUMAN"},
		{ID: "01ORPHAN", Type: "agent", Name: "ghost", AgentName: "ghost"},
	})
}

// Identity is keyed on email, not the actor ULID, because actors are minted
// per repository at `ark init` — a ULID key would need one mapping row per
// person per repository. See RFC-0002 "Identity".
func TestActorLoginPrefersEmailOverName(t *testing.T) {
	as := NewActors([]api.Actor{
		{ID: "a", Type: "human", Name: "named", Email: "mail@example.com"},
		{ID: "b", Type: "human", Name: "nameonly"},
	})
	if got := as.Resolve("a").ProviderLogin; got != "mail@example.com" {
		t.Errorf("email should win: got %q", got)
	}
	if got := as.Resolve("b").ProviderLogin; got != "nameonly" {
		t.Errorf("name is the fallback: got %q", got)
	}
}

// An agent's work belongs to the human who authorized it. Ark records
// delegation and GitHub does not; dropping it would leave a stream in which
// every record is unattributed, which is what the trial repositories look
// like (100% of records agent-authored).
func TestAgentResolvesToDelegatingHuman(t *testing.T) {
	got := testActors().Resolve("01AGENT")
	if got.ProviderLogin != "ijroth@example.com" {
		t.Errorf("agent should map to the delegating human, got %q", got.ProviderLogin)
	}
	if got.DelegatedByLogin != "ijroth@example.com" {
		t.Errorf("delegation should be preserved, got %q", got.DelegatedByLogin)
	}
	if got.ActorType != "agent" {
		t.Errorf("the acting type stays agent, got %q", got.ActorType)
	}
	if got.DisplayName != "ijroth (via claude)" {
		t.Errorf("display should name both, got %q", got.DisplayName)
	}
}

// An agent with no recorded delegation must not be silently attributed to
// anyone; it keeps its own identity and Elk falls back to the binding owner.
func TestAgentWithoutDelegationKeepsItsOwnIdentity(t *testing.T) {
	got := testActors().Resolve("01ORPHAN")
	if got.ProviderLogin != "ghost" {
		t.Errorf("got %q", got.ProviderLogin)
	}
	if got.DelegatedByLogin != "" {
		t.Errorf("nothing to delegate to, got %q", got.DelegatedByLogin)
	}
}

// An actor we have no record for still needs a stable key so the event is
// routable rather than dropped.
func TestUnknownActorDegradesToItsID(t *testing.T) {
	got := testActors().Resolve("01MISSING")
	if got.ProviderLogin != "01MISSING" || got.ActorType != "unknown" {
		t.Errorf("got %+v", got)
	}
}

// Keys are built from ULIDs, never display numbers. The server renumbers
// colliding display numbers (CLAUDE.md: "Task/PR numbers are display
// aliases; ULIDs are authoritative"), so a number-keyed ref would silently
// rebind to a different record after a sync.
func TestEventKeysUseULIDsNotDisplayNumbers(t *testing.T) {
	task := &store.Task{ID: "01TASK", Number: 7, Title: "t", Status: "open",
		CreatedBy: "01HUMAN", CreatedAt: "2026-07-01T00:00:00Z"}
	ev := Task(testRepo, testActors(), task)[0]

	if !strings.Contains(ev.ExternalID, "01TASK") {
		t.Errorf("external_id must carry the ULID: %q", ev.ExternalID)
	}
	if strings.Contains(ev.ExternalID, "#task/7") {
		t.Errorf("external_id must not key on the display number: %q", ev.ExternalID)
	}
	if want := "ark:01REPO#task/01TASK"; ev.ObjectKey != want {
		t.Errorf("object key = %q, want %q", ev.ObjectKey, want)
	}
	// The display number is fine in the URL, which is for humans.
	if want := "ark://01REPO/task/7"; ev.URL != want {
		t.Errorf("url = %q, want %q", ev.URL, want)
	}
}

// A task's status lives in the event key, so external_refs' unique index on
// (provider, external_id) does state-transition dedup with no projection
// table. See RFC-0002 "Deriving events from a state feed".
func TestTaskStatusTransitionsAreDistinctIdempotentKeys(t *testing.T) {
	tests := []struct {
		status    string
		wantKinds []string
		wantKey   string
	}{
		{"open", []string{KindTaskOpened}, ""},
		{"in_progress", []string{KindTaskOpened, KindTaskStatus}, "status/in_progress"},
		{"blocked", []string{KindTaskOpened, KindTaskStatus}, "status/blocked"},
		{"done", []string{KindTaskOpened, KindTaskClosed}, "status/done"},
		{"closed", []string{KindTaskOpened, KindTaskClosed}, "status/closed"},
	}
	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			task := &store.Task{ID: "01TASK", Number: 1, Title: "x", Status: tc.status,
				CreatedBy: "01HUMAN", CreatedAt: "2026-07-01T00:00:00Z",
				UpdatedAt: "2026-07-02T00:00:00Z"}
			evs := Task(testRepo, testActors(), task)
			if len(evs) != len(tc.wantKinds) {
				t.Fatalf("got %d events, want %d", len(evs), len(tc.wantKinds))
			}
			for i, want := range tc.wantKinds {
				if evs[i].Kind != want {
					t.Errorf("event %d kind = %q, want %q", i, evs[i].Kind, want)
				}
			}
			if tc.wantKey != "" {
				if !strings.HasSuffix(evs[1].ExternalID, tc.wantKey) {
					t.Errorf("transition key = %q, want suffix %q", evs[1].ExternalID, tc.wantKey)
				}
				// Both events describe the same object, so an action bound to
				// the task resolves on closure.
				if evs[0].ObjectKey != evs[1].ObjectKey {
					t.Errorf("object keys diverged: %q vs %q", evs[0].ObjectKey, evs[1].ObjectKey)
				}
			}
		})
	}
}

// Mapping is a pure function of state, so the same record always produces
// the same keys. This is what makes re-emission safe and lets a client stay
// stateless.
func TestMappingIsDeterministic(t *testing.T) {
	task := &store.Task{ID: "01TASK", Number: 3, Title: "t", Status: "done",
		CreatedBy: "01AGENT", CreatedAt: "2026-07-01T00:00:00Z", UpdatedAt: "2026-07-02T00:00:00Z"}
	a := Task(testRepo, testActors(), task)
	b := Task(testRepo, testActors(), task)
	if len(a) != len(b) {
		t.Fatalf("length differs")
	}
	for i := range a {
		if a[i].ExternalID != b[i].ExternalID || a[i].Body != b[i].Body {
			t.Errorf("event %d not deterministic", i)
		}
	}
}

// Only a finished run tells a story; a start carries no result and would be
// pure noise in a stream.
func TestOnlyFinishedRunsProduceEvents(t *testing.T) {
	running := &store.Run{ID: "01RUN", Status: "running", CreatedBy: "01AGENT"}
	if evs := Run(testRepo, testActors(), running, nil); len(evs) != 0 {
		t.Errorf("a running run should emit nothing, got %d", len(evs))
	}
	// Status set but no finish timestamp is still in flight.
	partial := &store.Run{ID: "01RUN", Status: "succeeded", CreatedBy: "01AGENT"}
	if evs := Run(testRepo, testActors(), partial, nil); len(evs) != 0 {
		t.Errorf("no finished_at means not finished, got %d", len(evs))
	}
}

// The run capture is the reason this adapter exists: input, result, and the
// commit it produced, linked in one memory. Git holds the commit; only Ark
// holds why it happened.
func TestFinishedRunCarriesTheReasoning(t *testing.T) {
	run := &store.Run{
		ID: "01RUN", Status: "succeeded", AgentName: "claude", CreatedBy: "01AGENT",
		InputSummary: "Build the android adapter", ResultSummary: "13 match, 0 drift",
		BranchName: "android-adapter", ResultCommitSHA: "e6aac2975d96c579",
		FinishedAt: "2026-07-03T00:00:00Z",
	}
	arts := []*store.Artifact{{Name: "android.json"}, {Name: "screenshot.png"}}
	evs := Run(testRepo, testActors(), run, arts)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	e := evs[0]
	if e.Kind != KindRunFinished {
		t.Errorf("kind = %q", e.Kind)
	}
	for _, want := range []string{"Build the android adapter", "13 match, 0 drift",
		"android-adapter", "e6aac297", "android.json", "screenshot.png"} {
		if !strings.Contains(e.Body, want) {
			t.Errorf("body is missing %q:\n%s", want, e.Body)
		}
	}
	if e.OccurredAt != run.FinishedAt {
		t.Errorf("a run happened when it finished, got %q", e.OccurredAt)
	}
	if len(e.Refs.ArtifactNames) != 2 {
		t.Errorf("artifacts should be referenced, got %v", e.Refs.ArtifactNames)
	}
}

// A merged PR is both a memory and a resolution trigger; Elk's
// `Closes elk:<id>` parser reads the body, so the body must survive mapping
// intact.
func TestMergedPRPreservesBodyForClosesResolution(t *testing.T) {
	pr := &store.PullRequest{
		ID: "01PR", Number: 4, Title: "Ship it", Status: "merged",
		Body:      "Adds the thing.\n\nCloses elk:abc123",
		CreatedBy: "01HUMAN", CreatedAt: "2026-07-01T00:00:00Z",
		MergedAt: "2026-07-05T00:00:00Z", MergeCommitSHA: "deadbeefcafe",
	}
	evs := PR(testRepo, testActors(), pr)
	if len(evs) != 2 {
		t.Fatalf("open + merged expected, got %d", len(evs))
	}
	merged := evs[1]
	if merged.Kind != KindPRMerged {
		t.Errorf("kind = %q", merged.Kind)
	}
	if !strings.Contains(merged.Body, "Closes elk:abc123") {
		t.Errorf("the resolution directive must survive: %q", merged.Body)
	}
	if merged.OccurredAt != pr.MergedAt {
		t.Errorf("occurred_at = %q, want the merge time", merged.OccurredAt)
	}
	if merged.ObjectKey != evs[0].ObjectKey {
		t.Errorf("both events describe the same PR")
	}
}

// An unmerged PR must not emit a merge event, or a stream would announce
// work that never landed.
func TestOpenPRDoesNotEmitMerged(t *testing.T) {
	pr := &store.PullRequest{ID: "01PR", Number: 1, Title: "wip", Status: "open",
		CreatedBy: "01HUMAN", CreatedAt: "2026-07-01T00:00:00Z"}
	evs := PR(testRepo, testActors(), pr)
	if len(evs) != 1 || evs[0].Kind != KindPROpened {
		t.Fatalf("got %d events, first kind %q", len(evs), evs[0].Kind)
	}
}

// A review binds to its pull request's object key so approval and merge meet
// on the same external_refs row.
func TestReviewBindsToItsPullRequest(t *testing.T) {
	pr := &store.PullRequest{ID: "01PR", Number: 9, Title: "Ship"}
	rev := &store.Review{ID: "01REV", PullRequestID: "01PR", State: "changes_requested",
		Body: "needs work", CreatedBy: "01HUMAN", CreatedAt: "2026-07-04T00:00:00Z"}
	e := Review(testRepo, testActors(), rev, pr)[0]

	if want := "ark:01REPO#pr/01PR"; e.ObjectKey != want {
		t.Errorf("object key = %q, want %q", e.ObjectKey, want)
	}
	if !strings.Contains(e.ExternalID, "01REV") {
		t.Errorf("the event key is the review's own: %q", e.ExternalID)
	}
	if e.Refs.ReviewState != "changes_requested" {
		t.Errorf("review state lost")
	}
	if !strings.Contains(e.Title, "changes requested") {
		t.Errorf("title should read naturally: %q", e.Title)
	}
}

// Only closed threads become a memory: an open conversation is still being
// had, and forty individual messages are noise, not one memory.
func TestOnlyClosedThreadsProduceOneEvent(t *testing.T) {
	open := &store.Thread{ID: "01TH", Title: "design", Status: "open", CreatedBy: "01HUMAN"}
	if evs := Thread(testRepo, testActors(), open, 12); len(evs) != 0 {
		t.Errorf("open thread should emit nothing, got %d", len(evs))
	}
	closed := &store.Thread{ID: "01TH", Title: "design", Status: "closed",
		CreatedBy: "01HUMAN", CreatedAt: "2026-07-01T00:00:00Z", ClosedAt: "2026-07-06T00:00:00Z"}
	evs := Thread(testRepo, testActors(), closed, 12)
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	if !strings.Contains(evs[0].Body, "12 messages") {
		t.Errorf("body = %q", evs[0].Body)
	}
	if evs[0].OccurredAt != closed.ClosedAt {
		t.Errorf("a thread closes when it closes")
	}
}

// Comments are append-only (spec §6.3); a correction arrives as a new
// comment with supersedes_id, which is a distinct ULID and so a distinct
// event — correct, and self-deduplicating.
func TestCommentBindsToParentObjectAndKeysOnItself(t *testing.T) {
	c := &store.Comment{ID: "01CMT", ParentType: "task", ParentID: "01TASK",
		Body: "started", CreatedBy: "01AGENT", CreatedAt: "2026-07-02T00:00:00Z"}
	e := Comment(testRepo, testActors(), c, "Ship the widget", 5)[0]

	if want := "ark:01REPO#task/01TASK"; e.ObjectKey != want {
		t.Errorf("object key = %q, want %q", e.ObjectKey, want)
	}
	if !strings.Contains(e.ExternalID, "01CMT") {
		t.Errorf("event key must be the comment's own: %q", e.ExternalID)
	}

	correction := &store.Comment{ID: "01CMT2", ParentType: "task", ParentID: "01TASK",
		Body: "actually, stopped", SupersedesID: "01CMT", CreatedBy: "01AGENT",
		CreatedAt: "2026-07-02T01:00:00Z"}
	e2 := Comment(testRepo, testActors(), correction, "Ship the widget", 5)[0]
	if e.ExternalID == e2.ExternalID {
		t.Errorf("a correction is a distinct event")
	}
}

// Comments on a pull request must not land in the task namespace, or two
// different objects would share one key.
func TestCommentParentTypeSelectsTheObjectNamespace(t *testing.T) {
	tests := map[string]string{
		"task":         "ark:01REPO#task/01P",
		"pull_request": "ark:01REPO#pr/01P",
		"agent_run":    "ark:01REPO#run/01P",
		"review":       "ark:01REPO#review/01P",
	}
	for parentType, want := range tests {
		c := &store.Comment{ID: "01C", ParentType: parentType, ParentID: "01P",
			CreatedBy: "01HUMAN", CreatedAt: "2026-07-01T00:00:00Z"}
		if got := Comment(testRepo, testActors(), c, "", 0)[0].ObjectKey; got != want {
			t.Errorf("%s: object key = %q, want %q", parentType, got, want)
		}
	}
}

// Two repositories may hold identically-numbered tasks; keys must never
// collide across them.
func TestKeysAreNamespacedByRepository(t *testing.T) {
	task := &store.Task{ID: "01TASK", Number: 1, Title: "t", Status: "open",
		CreatedBy: "01HUMAN", CreatedAt: "2026-07-01T00:00:00Z"}
	a := Task(Repo{ID: "01REPOA", Name: "a"}, testActors(), task)[0]
	b := Task(Repo{ID: "01REPOB", Name: "b"}, testActors(), task)[0]
	if a.ExternalID == b.ExternalID {
		t.Errorf("keys collided across repositories: %q", a.ExternalID)
	}
	if a.RepoFullName != "01REPOA" {
		t.Errorf("routing key should be the repository ULID, got %q", a.RepoFullName)
	}
}

// Every event must carry the provider Elk's external_refs CHECK constraint
// allows, and a non-empty dedup key — an event without one cannot be made
// idempotent.
func TestEveryEventIsRoutableAndDeduplicable(t *testing.T) {
	as := testActors()
	var all []Event
	all = append(all, Task(testRepo, as, &store.Task{ID: "01T", Number: 1, Title: "t",
		Status: "done", CreatedBy: "01AGENT", CreatedAt: "t0", UpdatedAt: "t1"})...)
	all = append(all, Run(testRepo, as, &store.Run{ID: "01R", Status: "failed",
		CreatedBy: "01AGENT", FinishedAt: "t2"}, nil)...)
	all = append(all, PR(testRepo, as, &store.PullRequest{ID: "01P", Number: 1,
		Title: "p", Status: "merged", CreatedBy: "01HUMAN", CreatedAt: "t0", MergedAt: "t3"})...)
	all = append(all, Review(testRepo, as, &store.Review{ID: "01V", PullRequestID: "01P",
		State: "approved", CreatedBy: "01HUMAN", CreatedAt: "t4"}, nil)...)
	all = append(all, Promotion(testRepo, as, &store.Promotion{ID: "01M",
		Environment: "prod", CreatedBy: "01HUMAN", ActivatedAt: "t5"})...)
	all = append(all, Thread(testRepo, as, &store.Thread{ID: "01H", Title: "th",
		Status: "closed", CreatedBy: "01HUMAN", ClosedAt: "t6"}, 3)...)
	all = append(all, Comment(testRepo, as, &store.Comment{ID: "01C", ParentType: "task",
		ParentID: "01T", CreatedBy: "01AGENT", CreatedAt: "t7"}, "t", 1)...)

	if len(all) < 8 {
		t.Fatalf("expected every record type to map, got %d events", len(all))
	}
	seen := map[string]bool{}
	for _, e := range all {
		if e.Provider != Provider {
			t.Errorf("%s: provider = %q", e.Kind, e.Provider)
		}
		if e.ExternalID == "" || e.ObjectKey == "" {
			t.Errorf("%s: missing key (%q / %q)", e.Kind, e.ExternalID, e.ObjectKey)
		}
		if e.OccurredAt == "" {
			t.Errorf("%s: missing occurred_at", e.Kind)
		}
		if e.RepoFullName == "" {
			t.Errorf("%s: missing routing key", e.Kind)
		}
		if seen[e.ExternalID] {
			t.Errorf("duplicate event key %q", e.ExternalID)
		}
		seen[e.ExternalID] = true
	}
}

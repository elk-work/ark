package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// The queries in this package say "in creation order". They cannot get that
// from created_at: it is stored as RFC3339Nano text, time.RFC3339Nano trims
// trailing zeros from the fractional second, and SQLite compares TEXT byte by
// byte — so the shorter, earlier string sorts after the longer, later one.
// records.TimeCompare carries the full explanation for the Go side; in SQL
// there is nothing to route through, so these queries order by the ULID.
//
// The trap is real but not dense enough to hit by writing rows quickly and
// hoping, so these tests construct it.

// trapStamps returns n timestamps that increase chronologically and decrease
// lexically. Each extends the one before it with another '9', so the earlier
// value is always a strict prefix of the later, and the byte that follows the
// shared prefix is 'Z' (0x5A) in the earlier against '9' (0x39) in the later.
// Every one of them is a value records.Now() could have produced: a clock
// reading of .1724 seconds formats as ".1724Z", not ".172400Z".
func trapStamps(n int) []string {
	const base = "2026-08-26T07:00:00.1724" // four fractional digits, five to spare
	if n > 6 {
		panic("trapStamps: RFC3339 allows nine fractional digits")
	}
	out := make([]string, n)
	for i := range out {
		out[i] = base + strings.Repeat("9", i) + "Z"
	}
	return out
}

func TestTrapStampsReverseUnderByteOrdering(t *testing.T) {
	stamps := trapStamps(3)
	for i := 1; i < len(stamps); i++ {
		if !records.TimeBefore(stamps[i-1], stamps[i]) {
			t.Fatalf("%s is not chronologically before %s", stamps[i-1], stamps[i])
		}
		if stamps[i-1] <= stamps[i] {
			t.Fatalf("%q does not sort after %q as text; the trap has gone stale",
				stamps[i-1], stamps[i])
		}
	}
}

// insertionOrder reports the ids of a table's rows in the order SQLite
// inserted them. It reads the implicit rowid, so it is independent of both
// the ULID and the timestamp — it is the ground truth the queries under test
// are trying to reproduce.
func insertionOrder(t *testing.T, s *Store, table string) []string {
	t.Helper()
	rows, err := s.DB.Query(fmt.Sprintf(`SELECT id FROM %s ORDER BY rowid`, table))
	if err != nil {
		t.Fatalf("read %s insertion order: %v", table, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan %s id: %v", table, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s insertion order: %v", table, err)
	}
	return out
}

// setTrap rewrites one timestamp column so the rows named — in creation order
// — carry instants that advance while their stored text runs backwards. It
// also asserts the ids themselves are ascending, which is what makes ORDER BY
// id the right answer and is guaranteed by records.NewID()'s monotonic
// entropy.
func setTrap(t *testing.T, s *Store, table, column string, ids []string) {
	t.Helper()
	stamps := trapStamps(len(ids))
	for i, id := range ids {
		if i > 0 && ids[i-1] >= id {
			t.Fatalf("%s ids are not ascending in creation order: %s then %s", table, ids[i-1], id)
		}
		res, err := s.DB.Exec(
			fmt.Sprintf(`UPDATE %s SET %s = ? WHERE id = ?`, table, column), stamps[i], id)
		if err != nil {
			t.Fatalf("set trap on %s.%s: %v", table, column, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("set trap on %s.%s: %d rows matched %s, want 1", table, column, n, id)
		}
	}
}

func assertOrder(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s order = %v, want %v", what, got, want)
	}
}

// TestMutationQueueReplaysInWriteOrder is the one that matters most: the
// mutation log is what the server replays, so an order that is merely usually
// right is not right. Ordering by ULID also settles the case created_at
// cannot — two mutations logged inside a single transaction, which is what a
// promotion superseding its predecessor produces.
func TestMutationQueueReplaysInWriteOrder(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	p1, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "abc123"})
	if err != nil {
		t.Fatalf("first promotion: %v", err)
	}
	// The second create supersedes the first: one transaction logging an
	// update of p1 and then a create of p2, microseconds apart.
	p2, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "def456"})
	if err != nil {
		t.Fatalf("second promotion: %v", err)
	}

	want := insertionOrder(t, s, "mutations")
	if len(want) != 3 {
		t.Fatalf("mutations = %d, want 3", len(want))
	}
	setTrap(t, s, "mutations", "created_at", want)

	muts, err := s.PendingMutationRows(ctx)
	if err != nil {
		t.Fatalf("pending mutations: %v", err)
	}
	got := make([]string, len(muts))
	for i, m := range muts {
		got[i] = m.ID
	}
	assertOrder(t, "mutation queue", got, want)

	// And the queue still tells the story it is meant to tell.
	type step struct{ record, op string }
	steps := make([]step, len(muts))
	for i, m := range muts {
		steps[i] = step{m.RecordID, m.Operation}
	}
	wantSteps := []step{{p1.ID, "create"}, {p1.ID, "update"}, {p2.ID, "create"}}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Fatalf("replay = %v, want %v", steps, wantSteps)
	}
}

func TestListsOrderByULIDNotTimestampText(t *testing.T) {
	ctx := context.Background()

	t.Run("comments", func(t *testing.T) {
		s, _ := newTestStore(t)
		task, err := s.CreateTask(ctx, "T", "")
		if err != nil {
			t.Fatal(err)
		}
		c1, err := s.AddComment(ctx, "task", task.ID, "first", "")
		if err != nil {
			t.Fatal(err)
		}
		c2, err := s.AddComment(ctx, "task", task.ID, "second", "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{c1.ID, c2.ID}
		setTrap(t, s, "comments", "created_at", want)

		got, err := s.ListComments(ctx, "task", task.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		bodies := make([]string, len(got))
		for i, c := range got {
			ids[i], bodies[i] = c.ID, c.Body
		}
		assertOrder(t, "comments", ids, want)
		assertOrder(t, "comment bodies", bodies, []string{"first", "second"})
	})

	t.Run("threads", func(t *testing.T) {
		s, _ := newTestStore(t)
		t1, err := s.CreateThread(ctx, "", "first thread")
		if err != nil {
			t.Fatal(err)
		}
		t2, err := s.CreateThread(ctx, "", "second thread")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{t1.ID, t2.ID}
		setTrap(t, s, "agent_threads", "created_at", want)

		got, err := s.ListThreads(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		for i, th := range got {
			ids[i] = th.ID
		}
		assertOrder(t, "threads", ids, want)
	})

	t.Run("thread messages", func(t *testing.T) {
		s, _ := newTestStore(t)
		th, err := s.CreateThread(ctx, "", "Design talk")
		if err != nil {
			t.Fatal(err)
		}
		m1, err := s.AddMessage(ctx, th.ID, "user", "question", "")
		if err != nil {
			t.Fatal(err)
		}
		m2, err := s.AddMessage(ctx, th.ID, "agent", "answer", "")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{m1.ID, m2.ID}
		setTrap(t, s, "thread_messages", "created_at", want)

		got, err := s.ListMessages(ctx, th.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		bodies := make([]string, len(got))
		for i, m := range got {
			ids[i], bodies[i] = m.ID, m.Body
		}
		assertOrder(t, "messages", ids, want)
		// A transcript read back out of order is a different conversation.
		assertOrder(t, "message bodies", bodies, []string{"question", "answer"})
	})

	t.Run("runs", func(t *testing.T) {
		s, _ := newTestStore(t)
		r1, err := s.StartRun(ctx, &Run{AgentName: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		r2, err := s.StartRun(ctx, &Run{AgentName: "claude"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{r1.ID, r2.ID}
		setTrap(t, s, "agent_runs", "created_at", want)

		got, err := s.ListRuns(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		for i, r := range got {
			ids[i] = r.ID
		}
		assertOrder(t, "runs", ids, want)
	})

	t.Run("artifacts", func(t *testing.T) {
		s, dir := newTestStore(t)
		arkDir := filepath.Join(dir, ".ark")
		if err := os.MkdirAll(arkDir, 0o755); err != nil {
			t.Fatal(err)
		}
		task, err := s.CreateTask(ctx, "T", "")
		if err != nil {
			t.Fatal(err)
		}
		src := filepath.Join(dir, "report.txt")
		if err := os.WriteFile(src, []byte("evidence"), 0o644); err != nil {
			t.Fatal(err)
		}
		a1, err := s.AddArtifact(ctx, arkDir, src, "task", task.ID, "first.txt")
		if err != nil {
			t.Fatal(err)
		}
		a2, err := s.AddArtifact(ctx, arkDir, src, "task", task.ID, "second.txt")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{a1.ID, a2.ID}
		setTrap(t, s, "artifacts", "created_at", want)

		got, err := s.ListArtifacts(ctx, "task", task.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		for i, a := range got {
			ids[i] = a.ID
		}
		assertOrder(t, "artifacts", ids, want)
	})

	t.Run("reviews", func(t *testing.T) {
		s, _ := newTestStore(t)
		pr, err := s.CreatePR(ctx, &PullRequest{Title: "Change", BaseBranch: "main",
			HeadBranch: "feature", HeadCommitSHA: "abc"})
		if err != nil {
			t.Fatal(err)
		}
		rev1, _, err := s.SubmitReview(ctx, "1", "request_changes", "not yet", "abc")
		if err != nil {
			t.Fatal(err)
		}
		rev2, _, err := s.SubmitReview(ctx, "1", "approve", "", "abc")
		if err != nil {
			t.Fatal(err)
		}
		want := []string{rev1.ID, rev2.ID}
		setTrap(t, s, "reviews", "created_at", want)

		got, err := s.ListReviews(ctx, pr.ID)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		states := make([]string, len(got))
		for i, r := range got {
			ids[i], states[i] = r.ID, r.State
		}
		assertOrder(t, "reviews", ids, want)
		// Reversed, the PR reads as having been blocked after approval.
		assertOrder(t, "review states", states, []string{"request_changes", "approve"})
	})

	t.Run("promotions", func(t *testing.T) {
		s, _ := newTestStore(t)
		p1, err := s.CreatePromotion(ctx, &Promotion{Environment: "staging", MergeCommitSHA: "abc123"})
		if err != nil {
			t.Fatal(err)
		}
		p2, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "def456"})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{p1.ID, p2.ID}
		// activated_at, not created_at: CreatePromotion writes both from one
		// records.Now() reading and never takes either from a caller.
		setTrap(t, s, "promotions", "activated_at", want)

		got, err := s.ListPromotions(ctx, "", false)
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(got))
		for i, p := range got {
			ids[i] = p.ID
		}
		assertOrder(t, "promotions", ids, want)
	})
}

// TestSupersedeEndsEveryPriorPromotion covers the other reader of the
// promotions ordering: activePromotions, which feeds CreatePromotion's
// supersede loop.
//
// Unlike every other test here this one passes either way, and that is the
// finding. The loop ends *every* row it is handed and stamps them all with
// the same ended_at, so the order it sees does not change the outcome — the
// ORDER BY is there for a deterministic mutation-log sequence, not for
// correctness. The test pins the invariant that actually matters, so the
// claim survives a future edit to that loop.
func TestSupersedeEndsEveryPriorPromotion(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	p1, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "one"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "two"})
	if err != nil {
		t.Fatal(err)
	}
	// Two rows active at once for the same environment, as a pull of a
	// concurrently created promotion can leave behind.
	if _, err := s.DB.Exec(`UPDATE promotions SET ended_at = NULL WHERE id = ?`, p1.ID); err != nil {
		t.Fatal(err)
	}
	setTrap(t, s, "promotions", "activated_at", []string{p1.ID, p2.ID})

	p3, err := s.CreatePromotion(ctx, &Promotion{Environment: "production", MergeCommitSHA: "three"})
	if err != nil {
		t.Fatal(err)
	}
	active, err := s.ListPromotions(ctx, "production", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != p3.ID {
		ids := make([]string, len(active))
		for i, p := range active {
			ids[i] = p.ID
		}
		t.Fatalf("active promotions = %v, want only %s", ids, p3.ID)
	}
}

// The LIMIT 1 lookups are the sharp end: a misorder does not shuffle a list,
// it returns a different record.

func TestResolveTaskByNumberPicksTheOldestRecord(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	first, err := s.CreateTask(ctx, "the real #1", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateTask(ctx, "a duplicate of #1", "")
	if err != nil {
		t.Fatal(err)
	}
	// Numbers duplicate transiently mid-sync; the oldest record wins.
	if _, err := s.DB.Exec(`UPDATE tasks SET number = ? WHERE id = ?`, first.Number, second.ID); err != nil {
		t.Fatal(err)
	}
	setTrap(t, s, "tasks", "created_at", []string{first.ID, second.ID})

	got, err := s.ResolveTask(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("task #1 resolved to %s (%q), want %s (%q)",
			got.ID, got.Title, first.ID, first.Title)
	}
}

func TestResolvePRByNumberPicksTheOldestRecord(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	first, err := s.CreatePR(ctx, &PullRequest{Title: "the real #1", BaseBranch: "main",
		HeadBranch: "feature", HeadCommitSHA: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreatePR(ctx, &PullRequest{Title: "a duplicate of #1", BaseBranch: "main",
		HeadBranch: "other", HeadCommitSHA: "def"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`UPDATE pull_requests SET number = ? WHERE id = ?`,
		first.Number, second.ID); err != nil {
		t.Fatal(err)
	}
	setTrap(t, s, "pull_requests", "created_at", []string{first.ID, second.ID})

	got, err := s.ResolvePR(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("pr #1 resolved to %s (%q), want %s (%q)",
			got.ID, got.Title, first.ID, first.Title)
	}
}

func TestFindAgentActorPicksTheFirstRegistration(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	first := &Actor{Type: records.ActorAgent, Name: "claude-code", AgentName: "claude-code"}
	if err := CreateActor(ctx, s.DB, first); err != nil {
		t.Fatal(err)
	}
	second := &Actor{Type: records.ActorAgent, Name: "claude-code", AgentName: "claude-code"}
	if err := CreateActor(ctx, s.DB, second); err != nil {
		t.Fatal(err)
	}
	setTrap(t, s, "actors", "created_at", []string{first.ID, second.ID})

	got, err := FindAgentActor(ctx, s.DB, "claude-code", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("agent actor = %s, want the first registration %s", got.ID, first.ID)
	}
}

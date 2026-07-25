package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/elkproject/ark/internal/db"
	"github.com/elkproject/ark/internal/records"
)

// newTestStore opens a fresh database with one repository and one human actor.
func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "ark.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	ctx := context.Background()
	actor := &Actor{Type: records.ActorHuman, Name: "Test Human"}
	if err := CreateActor(ctx, d, actor); err != nil {
		t.Fatalf("create actor: %v", err)
	}
	repoID := records.NewID()
	if _, err := d.Exec(`INSERT INTO repositories (id, name, path, created_at)
		VALUES (?, 'test', ?, ?)`, repoID, dir, records.Now()); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	return &Store{DB: d, RepoID: repoID, Actor: *actor}, dir
}

func mutationCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM mutations WHERE status = 'pending'`).Scan(&n); err != nil {
		t.Fatalf("count mutations: %v", err)
	}
	return n
}

func TestTaskLifecycle(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	task, err := s.CreateTask(ctx, "First task", "the body")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.Number != 1 {
		t.Errorf("first task number = %d, want 1", task.Number)
	}
	task2, _ := s.CreateTask(ctx, "Second task", "")
	if task2.Number != 2 {
		t.Errorf("second task number = %d, want 2", task2.Number)
	}

	// Resolve by number and by ULID prefix.
	byNum, err := s.ResolveTask(ctx, "1")
	if err != nil || byNum.ID != task.ID {
		t.Fatalf("resolve by number: %v", err)
	}
	// Shortest prefix that distinguishes task from task2 (monotonic ULIDs
	// minted in the same millisecond diverge only near the end).
	n := 0
	for n < len(task.ID) && n < len(task2.ID) && task.ID[n] == task2.ID[n] {
		n++
	}
	byPrefix, err := s.ResolveTask(ctx, task.ID[:n+1])
	if err != nil || byPrefix.ID != task.ID {
		t.Fatalf("resolve by prefix: %v", err)
	}

	// Edit: status transition and version bump.
	st := "in_progress"
	edited, err := s.UpdateTask(ctx, "1", TaskEdit{Status: &st})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if edited.Version != 2 || edited.Status != "in_progress" {
		t.Errorf("after edit: version=%d status=%s", edited.Version, edited.Status)
	}

	// Invalid status rejected.
	bad := "bogus"
	if _, err := s.UpdateTask(ctx, "1", TaskEdit{Status: &bad}); err == nil {
		t.Error("bogus status accepted")
	}

	closed, err := s.CloseTask(ctx, "1")
	if err != nil || closed.Status != "closed" {
		t.Fatalf("close: %v (status %s)", err, closed.Status)
	}

	// Every write logged an intent: 2 creates + 2 updates.
	if n := mutationCount(t, s); n != 4 {
		t.Errorf("mutations = %d, want 4", n)
	}

	// List filters.
	open, _ := s.ListTasks(ctx, "open")
	if len(open) != 1 || open[0].Number != 2 {
		t.Errorf("open tasks: %+v", open)
	}
	all, _ := s.ListTasks(ctx, "all")
	if len(all) != 2 {
		t.Errorf("all tasks: %d, want 2", len(all))
	}
}

func TestCommentsAppendOnly(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "T", "")

	c1, err := s.AddComment(ctx, "task", task.ID, "first", "")
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	// A correction references the original rather than editing it.
	c2, err := s.AddComment(ctx, "task", task.ID, "first (corrected)", c1.ID)
	if err != nil {
		t.Fatalf("superseding comment: %v", err)
	}
	if c2.SupersedesID != c1.ID {
		t.Errorf("supersedes = %q, want %q", c2.SupersedesID, c1.ID)
	}
	list, _ := s.ListComments(ctx, "task", task.ID)
	if len(list) != 2 {
		t.Fatalf("comments = %d, want 2", len(list))
	}
	if _, err := s.AddComment(ctx, "nonsense", task.ID, "x", ""); err == nil {
		t.Error("invalid parent type accepted")
	}
	if _, err := s.AddComment(ctx, "task", task.ID, "  ", ""); err == nil {
		t.Error("blank body accepted")
	}
}

func TestThreadAndMessages(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "T", "")

	th, err := s.CreateThread(ctx, task.ID, "Design talk")
	if err != nil {
		t.Fatalf("thread: %v", err)
	}
	if _, err := s.AddMessage(ctx, th.ID, "user", "hello", ""); err != nil {
		t.Fatalf("message: %v", err)
	}
	if _, err := s.AddMessage(ctx, th.ID, "robot", "hi", ""); err == nil {
		t.Error("invalid role accepted")
	}
	msgs, _ := s.ListMessages(ctx, th.ID)
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("messages: %+v", msgs)
	}

	closed, err := s.CloseThread(ctx, th.ID[:10])
	if err != nil || closed.Status != "closed" {
		t.Fatalf("close thread: %v", err)
	}
	// Closed threads refuse new messages.
	if _, err := s.AddMessage(ctx, th.ID, "user", "more", ""); err == nil {
		t.Error("message accepted on closed thread")
	}
	// Threads filter by task.
	byTask, _ := s.ListThreads(ctx, task.ID)
	if len(byTask) != 1 {
		t.Errorf("threads for task = %d, want 1", len(byTask))
	}
}

func TestRunLifecycle(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "T", "")

	r, err := s.StartRun(ctx, &Run{TaskID: task.ID, AgentName: "claude-code",
		InputSummary: "do the thing", BranchName: "work", BaseCommitSHA: "abc123"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if r.Status != "running" || r.StartedAt == "" {
		t.Errorf("started run: %+v", r)
	}

	exit := int64(0)
	fin, err := s.FinishRun(ctx, r.ID[:8], RunFinish{Status: "succeeded",
		ResultSummary: "done", ExitCode: &exit, ResultCommitSHA: "def456"})
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if fin.Status != "succeeded" || fin.FinishedAt == "" {
		t.Errorf("finished run: %+v", fin)
	}
	// Double finish rejected.
	if _, err := s.FinishRun(ctx, r.ID, RunFinish{Status: "failed"}); err == nil {
		t.Error("double finish accepted")
	}
	// Bad terminal status rejected.
	r2, _ := s.StartRun(ctx, &Run{AgentName: "a"})
	if _, err := s.FinishRun(ctx, r2.ID, RunFinish{Status: "running"}); err == nil {
		t.Error("non-terminal finish status accepted")
	}
}

func TestReviewImmutableAndPRStates(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	pr, err := s.CreatePR(ctx, &PullRequest{Title: "Change", BaseBranch: "main",
		HeadBranch: "feature", HeadCommitSHA: "abc"})
	if err != nil {
		t.Fatalf("create pr: %v", err)
	}
	if pr.Number != 1 {
		t.Errorf("pr number = %d", pr.Number)
	}
	if _, err := s.CreatePR(ctx, &PullRequest{Title: "Bad", BaseBranch: "main", HeadBranch: "main"}); err == nil {
		t.Error("same base/head accepted")
	}

	rev, _, err := s.SubmitReview(ctx, "1", "approve", "", "abc")
	if err != nil {
		t.Fatalf("review: %v", err)
	}
	if rev.State != "approve" {
		t.Errorf("review state %s", rev.State)
	}
	// request_changes needs a body.
	if _, _, err := s.SubmitReview(ctx, "1", "request_changes", "", "abc"); err == nil {
		t.Error("empty request_changes accepted")
	}

	// Close, then reviews are refused (submitted reviews stay put).
	closed, err := s.ClosePR(ctx, "1")
	if err != nil || closed.Status != "closed" {
		t.Fatalf("close: %v", err)
	}
	if _, _, err := s.SubmitReview(ctx, "1", "approve", "", "abc"); err == nil {
		t.Error("review accepted on closed PR")
	}
	if _, err := s.ClosePR(ctx, "1"); err == nil {
		t.Error("double close accepted")
	}
	reviews, _ := s.ListReviews(ctx, pr.ID)
	if len(reviews) != 1 {
		t.Errorf("reviews = %d, want 1", len(reviews))
	}
}

func TestArtifactContentAddressing(t *testing.T) {
	s, dir := newTestStore(t)
	ctx := context.Background()
	arkDir := filepath.Join(dir, ".ark")
	os.MkdirAll(arkDir, 0o755)
	task, _ := s.CreateTask(ctx, "T", "")

	src := filepath.Join(dir, "report.txt")
	os.WriteFile(src, []byte("evidence"), 0o644)

	a, err := s.AddArtifact(ctx, arkDir, src, "task", task.ID, "")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if a.Name != "report.txt" || a.SizeBytes != 8 {
		t.Errorf("artifact: %+v", a)
	}
	// Object is stored under its hash.
	obj := filepath.Join(arkDir, "objects", "sha256", a.SHA256[:2], a.SHA256)
	if _, err := os.Stat(obj); err != nil {
		t.Fatalf("object missing: %v", err)
	}

	// Same content -> same object, distinct record.
	b, err := s.AddArtifact(ctx, arkDir, src, "task", task.ID, "copy.txt")
	if err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if b.SHA256 != a.SHA256 || b.ID == a.ID {
		t.Errorf("dedup: %+v vs %+v", a, b)
	}

	// Round trip with checksum verification.
	dest := filepath.Join(dir, "out.txt")
	if err := s.CopyArtifact(a, arkDir, dest); err != nil {
		t.Fatalf("copy: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "evidence" {
		t.Errorf("round trip: %q", got)
	}
}

func TestSearch(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	s.CreateTask(ctx, "Fix the flux capacitor", "it sparks")
	task2, _ := s.CreateTask(ctx, "Other work", "unrelated")
	s.AddComment(ctx, "task", task2.ID, "the capacitor is fine", "")

	hits, err := s.Search(ctx, "capacitor", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (task + comment): %+v", len(hits), hits)
	}

	// Edits keep the index current.
	newTitle := "Fix the warp coil"
	s.UpdateTask(ctx, "1", TaskEdit{Title: &newTitle})
	hits, _ = s.Search(ctx, "capacitor", 10)
	if len(hits) != 1 {
		t.Errorf("after edit, capacitor hits = %d, want 1", len(hits))
	}
	hits, _ = s.Search(ctx, "warp", 10)
	if len(hits) != 1 {
		t.Errorf("warp hits = %d, want 1", len(hits))
	}
}

func TestResolveAmbiguousPrefix(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	a, _ := s.CreateTask(ctx, "A", "")
	b, _ := s.CreateTask(ctx, "B", "")
	// IDs minted within the same test share at least their timestamp prefix.
	prefix := a.ID[:10]
	if !strings.HasPrefix(b.ID, prefix) {
		t.Skip("IDs diverged within the timestamp prefix")
	}
	_, err := s.ResolveTask(ctx, prefix)
	if err == nil {
		t.Fatal("ambiguous prefix accepted")
	}
	var ae *records.Error
	if !errors.As(err, &ae) || ae.Kind != records.KindValidation {
		t.Fatalf("want validation error for ambiguous prefix, got %v", err)
	}
}

func TestAgentActorIdentity(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()

	a1, err := FindAgentActor(ctx, s.DB, "claude-code", "1.0", s.Actor.ID)
	if err != nil {
		t.Fatalf("find agent: %v", err)
	}
	if a1.Type != records.ActorAgent || a1.DelegatedBy != s.Actor.ID {
		t.Errorf("agent actor: %+v", a1)
	}
	// Same name returns the same identity.
	a2, _ := FindAgentActor(ctx, s.DB, "claude-code", "", "")
	if a2.ID != a1.ID {
		t.Errorf("agent identity not stable: %s vs %s", a1.ID, a2.ID)
	}

	// Records created by the agent carry its identity.
	agentStore := &Store{DB: s.DB, RepoID: s.RepoID, Actor: *a1}
	task, _ := agentStore.CreateTask(ctx, "By agent", "")
	if task.CreatedByType != "agent" || task.CreatedBy != a1.ID {
		t.Errorf("agent task attribution: %+v", task)
	}
}

func TestMutationPayloads(t *testing.T) {
	s, _ := newTestStore(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, "T", "")
	st := "blocked"
	s.UpdateTask(ctx, task.ID, TaskEdit{Status: &st})

	rows, err := s.DB.Query(`SELECT record_type, operation, payload_json FROM mutations ORDER BY created_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type mut struct{ rt, op, payload string }
	var muts []mut
	for rows.Next() {
		var m mut
		rows.Scan(&m.rt, &m.op, &m.payload)
		muts = append(muts, m)
	}
	if len(muts) != 2 {
		t.Fatalf("mutations = %d, want 2", len(muts))
	}
	if muts[0].op != "create" || muts[1].op != "update" {
		t.Errorf("operations: %+v", muts)
	}
	// The update payload carries only the changed field.
	if muts[1].payload != `{"status":"blocked"}` {
		t.Errorf("update payload = %s", muts[1].payload)
	}
}

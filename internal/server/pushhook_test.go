package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/ark/pkg/api"
)

// hookReceiver records what the push webhook delivers. failFirst makes the
// first n deliveries answer 500.
type hookReceiver struct {
	mu        sync.Mutex
	events    []api.PushEvent
	auth      []string
	delivery  []string
	attempts  int
	failFirst int
}

func newHookReceiver(t *testing.T, h *gitHarness, failFirst int) *hookReceiver {
	t.Helper()
	rcv := &hookReceiver{failFirst: failFirst}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rcv.mu.Lock()
		defer rcv.mu.Unlock()
		rcv.attempts++
		rcv.delivery = append(rcv.delivery, r.Header.Get("X-Ark-Delivery"))
		if rcv.attempts <= rcv.failFirst {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		var ev api.PushEvent
		if err := json.Unmarshal(body, &ev); err != nil {
			t.Errorf("webhook body is not a push event: %v\n%s", err, body)
		}
		rcv.events = append(rcv.events, ev)
		rcv.auth = append(rcv.auth, r.Header.Get("Authorization"))
	}))
	t.Cleanup(ts.Close)
	h.PushWebhookURL = ts.URL + "/hooks/ark"
	h.PushWebhookToken = "hook-secret"
	h.webhookBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	return rcv
}

func (r *hookReceiver) take() []api.PushEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// One POST per updated ref, carrying everything the issue names: the
// repository, the ref and its two ends, the pusher, the paths that changed,
// and ci/suites.yml at the new tip when it is there.
func TestPushWebhookFiresOncePerRef(t *testing.T) {
	h := newGitHarness(t)
	writer, _ := h.credentials(t)
	rcv := newHookReceiver(t, h, 0)

	suites := "suites:\n  - name: unit\n"
	src := h.workTree(t, map[string]string{"README.md": "hi\n", "ci/suites.yml": suites})
	main1 := strings.TrimSpace(h.mustGit(t, src, "rev-parse", "HEAD"))
	h.mustGit(t, src, "checkout", "--quiet", "-b", "feature")
	feature := h.commit(t, src, "feature work", map[string]string{"src/app.go": "package app\n"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main", "feature")
	h.WaitBackground()

	events := rcv.take()
	if len(events) != 2 {
		t.Fatalf("got %d events for a push of two refs, want 2: %+v", len(events), events)
	}
	zero := strings.Repeat("0", len(main1))
	f, m := events[0], events[1]
	if f.Ref != "refs/heads/feature" || m.Ref != "refs/heads/main" {
		t.Fatalf("refs = %s, %s", f.Ref, m.Ref)
	}
	for _, ev := range events {
		if ev.RepositoryID != repoID || ev.Before != zero {
			t.Errorf("%s: repository %q before %q", ev.Ref, ev.RepositoryID, ev.Before)
		}
		if ev.Pusher.PrincipalID != writer.Principal.ID || ev.Pusher.Email != "writer@example.com" {
			t.Errorf("%s: pusher = %+v, want %s", ev.Ref, ev.Pusher, writer.Principal.ID)
		}
		if ev.Manifests["ci/suites.yml"] != suites {
			t.Errorf("%s: manifests = %v", ev.Ref, ev.Manifests)
		}
	}
	if m.After != main1 || f.After != feature {
		t.Errorf("afters: main %s feature %s", m.After, f.After)
	}
	// A created ref reports what its tip commit changed: everything, for the
	// root commit on main; only the new file, for feature.
	if got := strings.Join(m.ChangedPaths, ","); got != "README.md,ci/suites.yml" {
		t.Errorf("main changed_paths = %q", got)
	}
	if got := strings.Join(f.ChangedPaths, ","); got != "src/app.go" {
		t.Errorf("feature changed_paths = %q", got)
	}
	rcv.mu.Lock()
	for _, a := range rcv.auth {
		if a != "Bearer hook-secret" {
			t.Errorf("Authorization = %q", a)
		}
	}
	rcv.mu.Unlock()

	// An update reports the diff between its two ends; a manifest that is
	// gone at the new tip is absent; a deletion carries no paths.
	h.mustGit(t, src, "checkout", "--quiet", "main")
	h.mustGit(t, src, "rm", "--quiet", "ci/suites.yml")
	main2 := h.commit(t, src, "drop suites", map[string]string{"README.md": "bye\n"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main", ":feature")
	h.WaitBackground()
	events = rcv.take()
	if len(events) != 2 {
		t.Fatalf("got %d events for an update and a delete, want 2: %+v", len(events), events)
	}
	del, upd := events[0], events[1]
	if del.Ref != "refs/heads/feature" || del.Before != feature || del.After != zero ||
		len(del.ChangedPaths) != 0 || len(del.Manifests) != 0 {
		t.Errorf("deletion event: %+v", del)
	}
	if upd.Before != main1 || upd.After != main2 {
		t.Errorf("update event ends: %s..%s", upd.Before, upd.After)
	}
	if got := strings.Join(upd.ChangedPaths, ","); got != "README.md,ci/suites.yml" {
		t.Errorf("update changed_paths = %q", got)
	}
	if _, ok := upd.Manifests["ci/suites.yml"]; ok {
		t.Errorf("a manifest deleted at the new tip was still sent: %v", upd.Manifests)
	}

	// A push that changes nothing sends nothing.
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()
	if events := rcv.take(); len(events) != 0 {
		t.Errorf("a no-op push sent %d events", len(events))
	}
}

// A receiver that fails is retried three times, with one delivery id across
// the attempts, and a push never waits on it.
func TestPushWebhookRetries(t *testing.T) {
	h := newGitHarness(t)
	writer, _ := h.credentials(t)

	rcv := newHookReceiver(t, h, 3)
	src := h.workTree(t, map[string]string{"a": "a"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()
	if events := rcv.take(); len(events) != 1 || rcv.attempts != 4 {
		t.Fatalf("after three failures: %d events in %d attempts, want 1 in 4", len(events), rcv.attempts)
	}
	for _, d := range rcv.delivery {
		if d == "" || d != rcv.delivery[0] {
			t.Errorf("delivery ids across the attempts of one event: %v", rcv.delivery)
			break
		}
	}

	// Four failures is the end of it: nothing delivered, nothing more tried.
	rcv = newHookReceiver(t, h, 100)
	h.commit(t, src, "b", map[string]string{"b": "b"})
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()
	if events := rcv.take(); len(events) != 0 || rcv.attempts != 4 {
		t.Fatalf("a receiver that always fails: %d events in %d attempts, want 0 in 4", len(events), rcv.attempts)
	}
}

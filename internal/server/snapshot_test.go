package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/server/repodb"
)

// memSnapshots is a GitSnapshotStore in memory, with the generation
// compare-and-swap GCS gives.
type memSnapshots struct {
	mu      sync.Mutex
	objects map[string][]byte
	gens    map[string]int64
	next    int64
	failGet bool
}

func newMemSnapshots() *memSnapshots {
	return &memSnapshots{objects: map[string][]byte{}, gens: map[string]int64{}}
}

func (m *memSnapshots) Open(ctx context.Context, repoID string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failGet {
		return nil, 0, errors.New("bucket unreachable")
	}
	b, ok := m.objects[repoID]
	if !ok {
		return nil, 0, repodb.ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), m.gens[repoID], nil
}

func (m *memSnapshots) Store(ctx context.Context, repoID string, r io.Reader, ifGen int64) (int64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gens[repoID] != ifGen {
		return 0, repodb.ErrConcurrentWrite
	}
	m.next++
	m.objects[repoID], m.gens[repoID] = b, m.next
	return m.next, nil
}

func (m *memSnapshots) generation(repoID string) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.gens[repoID]
}

// instance is another ark-server over the same repository databases and the
// same bucket, with a GIT_DATA_DIR of its own — what a restart, or a new
// Cloud Run instance after a scale to zero, looks like.
func (h *gitHarness) instance(t *testing.T, snaps GitSnapshotStore) (*Server, *httptest.Server) {
	t.Helper()
	s := &Server{
		Repos:          repodb.NewManager(h.Repos.Backend, t.TempDir()),
		Token:          h.Token,
		BootstrapToken: h.BootstrapToken,
		Blobs:          h.Blobs,
		GitDataDir:     t.TempDir(),
		GitSnapshots:   snaps,
		SnapshotDelay:  time.Hour,
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func remoteOn(ts *httptest.Server, credential string) string {
	return strings.Replace(ts.URL, "http://", "http://anyone:"+credential+"@", 1) + "/git/" + repoID
}

// The object-storage lifecycle: a push is snapshotted once pushes go quiet,
// an instance that starts with an empty GIT_DATA_DIR restores from the
// snapshot on first touch, and its own pushes snapshot on from the
// generation it restored.
func TestGitSnapshotSurvivesAnInstance(t *testing.T) {
	h := newGitHarness(t)
	writer, reader := h.credentials(t)
	snaps := newMemSnapshots()
	h.GitSnapshots = snaps
	h.SnapshotDelay = 20 * time.Millisecond

	src := h.workTree(t, map[string]string{"README.md": "one\n"})
	first := strings.TrimSpace(h.mustGit(t, src, "rev-parse", "HEAD"))
	h.mustGit(t, src, "push", "--quiet", h.remote(repoID, writer.Token), "main")
	deadline := time.Now().Add(10 * time.Second)
	for snaps.generation(repoID) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no snapshot was written after the push went quiet")
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.WaitBackground()

	// A second instance has never seen the repository on disk.
	next, ts := h.instance(t, snaps)
	parent := t.TempDir()
	h.mustGit(t, parent, "clone", "--quiet", remoteOn(ts, reader.Token), "clone")
	if got := strings.TrimSpace(h.mustGit(t, filepath.Join(parent, "clone"), "rev-parse", "HEAD")); got != first {
		t.Fatalf("a clone from the restored instance has HEAD %s, want %s", got, first)
	}

	// Its pushes snapshot from the generation it restored. FlushSnapshots is
	// the shutdown path: the delay here is an hour, so only a flush writes.
	before := snaps.generation(repoID)
	second := h.commit(t, src, "two", map[string]string{"README.md": "two\n"})
	h.mustGit(t, src, "push", "--quiet", remoteOn(ts, writer.Token), "main")
	next.WaitBackground()
	if snaps.generation(repoID) != before {
		t.Fatal("a snapshot was written before its delay")
	}
	next.FlushSnapshots(t.Context())
	if snaps.generation(repoID) == before {
		t.Fatal("FlushSnapshots wrote nothing for a pending push")
	}

	// And a third instance sees the second push.
	_, third := h.instance(t, snaps)
	parent = t.TempDir()
	h.mustGit(t, parent, "clone", "--quiet", remoteOn(third, reader.Token), "clone")
	if got := strings.TrimSpace(h.mustGit(t, filepath.Join(parent, "clone"), "rev-parse", "HEAD")); got != second {
		t.Fatalf("third instance HEAD %s, want %s", got, second)
	}

	// The first instance, still running with an old generation, cannot
	// overwrite what the second wrote.
	h.SnapshotDelay = time.Hour
	h.commit(t, src, "stale", map[string]string{"stale": "x"})
	h.mustGit(t, src, "push", "--quiet", "--force", h.remote(repoID, writer.Token), "main")
	h.WaitBackground()
	gen := snaps.generation(repoID)
	if err := h.snapshotNow(t.Context(), repoID); err == nil || !strings.Contains(err.Error(), "NOT stored") {
		t.Errorf("a stale instance's snapshot: %v, want a refusal", err)
	}
	if snaps.generation(repoID) != gen {
		t.Error("a stale instance overwrote the snapshot")
	}
}

// A snapshot that cannot be read is a 503, and no empty repository takes its
// place.
func TestGitSnapshotUnreadableIsNotEmpty(t *testing.T) {
	h := newGitHarness(t)
	_, reader := h.credentials(t)
	snaps := newMemSnapshots()
	snaps.failGet = true
	_, ts := h.instance(t, snaps)

	out, err := h.git(t, t.TempDir(), "clone", "--quiet", remoteOn(ts, reader.Token), "c")
	if err == nil {
		t.Fatalf("clone succeeded against an unreadable snapshot:\n%s", out)
	}
	if !strings.Contains(out, "503") {
		t.Errorf("clone failed for some other reason:\n%s", out)
	}
}

func TestUntarRefusesEntriesOutsideTheRepository(t *testing.T) {
	for _, name := range []string{"../escape", "/abs", "a/../../escape", `..\escape`} {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Size: 1, Mode: 0o644})
		tw.Write([]byte("x"))
		tw.Close()
		zw.Close()
		dir := filepath.Join(t.TempDir(), "repo")
		if err := untarGz(&buf, dir); err == nil {
			t.Errorf("entry %q was extracted", name)
		}
	}
	// And a round trip keeps a tree intact.
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "objects", "ab"), 0o755)
	os.WriteFile(filepath.Join(src, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
	os.WriteFile(filepath.Join(src, "objects", "ab", "cdef"), []byte("blob"), 0o444)
	var buf bytes.Buffer
	if err := tarGz(src, &buf); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "repo")
	if err := untarGz(&buf, dst); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"HEAD": "ref: refs/heads/main\n", "objects/ab/cdef": "blob"} {
		got, err := os.ReadFile(filepath.Join(dst, filepath.FromSlash(name)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q, %v", name, got, err)
		}
	}
}

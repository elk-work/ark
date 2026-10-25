package server

// Snapshots of hosted Git in object-storage mode (elk-work/ark#125).
//
// A bare repository is a directory Git reads and writes, so it needs a
// filesystem whatever the storage mode. In local mode that filesystem is the
// disk under GIT_DATA_DIR and nothing more is needed. In object-storage mode
// (GCS_BUCKET) the filesystem is scratch — on Cloud Run it is memory, and it
// is gone when the instance is — so the bucket keeps a snapshot of each bare
// repository, `git/<id>.tar.gz`, beside its database:
//
//   - On first touch, an instance restores the repository from its snapshot
//     before serving it. A snapshot that cannot be read is a 503, never an
//     empty repository: initializing one over a transient failure would
//     present an empty history, and its first snapshot would bury the real
//     one.
//   - After a push lands, a fresh snapshot is written once pushes have been
//     quiet for SnapshotDelay (30 s by default), and on shutdown for any
//     still pending. The write is conditional on the generation this
//     instance restored or last wrote, the same compare-and-swap repodb uses
//     for databases, so an instance that is not the one the bucket has heard
//     from last cannot overwrite it; it logs that loudly instead.
//
// This is the single-instance design the reference deployment already runs
// (docs/self-hosting.md § Concurrency): one instance holds each repository's
// working copy and its push lock, and the generation check is the safety net
// for an overlapping restart, not a protocol for two writers. A deployment
// that scales out needs a shared filesystem for GIT_DATA_DIR, not this.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"

	"github.com/elk-work/ark/internal/server/repodb"
)

// DefaultSnapshotDelay is how long pushes to a repository must be quiet
// before its snapshot is written.
const DefaultSnapshotDelay = 30 * time.Second

// snapshotTimeout bounds one restore or one snapshot.
const snapshotTimeout = 10 * time.Minute

// GitSnapshotStore keeps one snapshot object per hosted repository.
type GitSnapshotStore interface {
	// Open streams a repository's snapshot and reports its generation.
	// repodb.ErrNotFound when there is none.
	Open(ctx context.Context, repoID string) (io.ReadCloser, int64, error)
	// Store writes a new snapshot from r, only if the stored generation is
	// still ifGeneration (0: only if there is none), and returns the new
	// generation. repodb.ErrConcurrentWrite when it is not.
	Store(ctx context.Context, repoID string, r io.Reader, ifGeneration int64) (int64, error)
}

// GCSSnapshotStore keeps snapshots as git/<id>.tar.gz in a bucket.
type GCSSnapshotStore struct {
	Client *storage.Client
	Bucket string
}

func (g *GCSSnapshotStore) object(repoID string) *storage.ObjectHandle {
	return g.Client.Bucket(g.Bucket).Object("git/" + repoID + ".tar.gz")
}

func (g *GCSSnapshotStore) Open(ctx context.Context, repoID string) (io.ReadCloser, int64, error) {
	r, err := g.object(repoID).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, 0, repodb.ErrNotFound
	}
	if err != nil {
		return nil, 0, fmt.Errorf("open git snapshot: %w", err)
	}
	return r, r.Attrs.Generation, nil
}

func (g *GCSSnapshotStore) Store(ctx context.Context, repoID string, r io.Reader, ifGeneration int64) (int64, error) {
	obj := g.object(repoID)
	if ifGeneration == 0 {
		obj = obj.If(storage.Conditions{DoesNotExist: true})
	} else {
		obj = obj.If(storage.Conditions{GenerationMatch: ifGeneration})
	}
	w := obj.NewWriter(ctx)
	w.ContentType = "application/gzip"
	if _, err := io.Copy(w, r); err != nil {
		w.CloseWithError(err)
		return 0, fmt.Errorf("upload git snapshot: %w", err)
	}
	if err := w.Close(); err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == http.StatusPreconditionFailed {
			return 0, repodb.ErrConcurrentWrite
		}
		return 0, fmt.Errorf("store git snapshot: %w", err)
	}
	return w.Attrs().Generation, nil
}

// restoreBareRepo fills dir from the repository's snapshot. It reports
// whether there was one; (false, nil) means the repository has never been
// snapshotted and may be initialized fresh.
func (s *Server) restoreBareRepo(ctx context.Context, repoID, dir string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	rc, gen, err := s.GitSnapshots.Open(ctx, repoID)
	if errors.Is(err, repodb.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer rc.Close()
	if err := untarGz(rc, dir); err != nil {
		return false, fmt.Errorf("restore git snapshot of %s: %w", repoID, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false, fmt.Errorf("restore git snapshot of %s: it holds no bare repository", repoID)
	}
	st := s.gitState(repoID)
	st.snapMu.Lock()
	st.snapGen = gen
	st.snapMu.Unlock()
	if s.Log != nil {
		s.Log.Info("restored bare repository from snapshot", "repository_id", repoID, "generation", gen)
	}
	return true, nil
}

// scheduleSnapshot arranges a snapshot once pushes to a repository have been
// quiet for SnapshotDelay. Each push restarts the wait.
func (s *Server) scheduleSnapshot(repoID string) {
	if s.GitSnapshots == nil {
		return
	}
	delay := s.SnapshotDelay
	if delay <= 0 {
		delay = DefaultSnapshotDelay
	}
	st := s.gitState(repoID)
	st.snapMu.Lock()
	defer st.snapMu.Unlock()
	st.snapSeq++
	seq := st.snapSeq
	if st.snapTimer != nil {
		st.snapTimer.Stop()
	}
	st.snapPending = true
	st.snapTimer = time.AfterFunc(delay, func() { s.runScheduledSnapshot(repoID, seq) })
}

func (s *Server) runScheduledSnapshot(repoID string, seq uint64) {
	st := s.gitState(repoID)
	st.snapMu.Lock()
	if seq != st.snapSeq || !st.snapPending {
		st.snapMu.Unlock()
		return
	}
	st.snapPending, st.snapTimer = false, nil
	st.snapMu.Unlock()
	if err := s.snapshotNow(context.Background(), repoID); err != nil {
		s.logError("snapshot bare repository", err)
	}
}

// FlushSnapshots writes every snapshot still waiting out its delay, now. The
// shutdown path calls it, so a push moments before an instance stops is not
// lost with the instance.
func (s *Server) FlushSnapshots(ctx context.Context) {
	if s.GitSnapshots == nil {
		return
	}
	s.gitMu.Lock()
	var pending []string
	for id, st := range s.gitRepos {
		st.snapMu.Lock()
		if st.snapPending {
			st.snapPending = false
			if st.snapTimer != nil {
				st.snapTimer.Stop()
				st.snapTimer = nil
			}
			pending = append(pending, id)
		}
		st.snapMu.Unlock()
	}
	s.gitMu.Unlock()
	for _, id := range pending {
		if err := s.snapshotNow(ctx, id); err != nil {
			s.logError("snapshot bare repository at shutdown", err)
		}
	}
}

// snapshotNow writes a repository's snapshot. It holds the push lock while
// it reads the directory, so the tarball is a state Git actually reached.
func (s *Server) snapshotNow(ctx context.Context, repoID string) error {
	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	st := s.gitState(repoID)
	st.push.Lock()
	defer st.push.Unlock()
	dir := s.bareRepoDir(repoID)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return nil
	}
	st.snapMu.Lock()
	gen := st.snapGen
	st.snapMu.Unlock()

	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(tarGz(dir, pw)) }()
	started := time.Now()
	newGen, err := s.GitSnapshots.Store(ctx, repoID, pr, gen)
	pr.CloseWithError(errors.New("snapshot upload ended"))
	if errors.Is(err, repodb.ErrConcurrentWrite) {
		// Something other than this instance wrote the snapshot since this
		// instance restored or last wrote it — an overlapping restart, or a
		// second instance. Overwriting would discard pushes this instance
		// never saw, so it does not; until somebody reconciles the two, the
		// bucket holds the other copy.
		return fmt.Errorf("the stored git snapshot of %s changed since this instance read it (generation %d): "+
			"another instance has written it, and this instance's copy was NOT stored. Reconcile by hand: restore "+
			"the snapshot elsewhere and fetch from both", repoID, gen)
	}
	if err != nil {
		return err
	}
	st.snapMu.Lock()
	st.snapGen = newGen
	st.snapMu.Unlock()
	if s.Log != nil {
		s.Log.Info("snapshot bare repository", "repository_id", repoID, "generation", newGen,
			"duration_ms", time.Since(started).Milliseconds())
	}
	return nil
}

// tarGz writes a directory as a gzipped tarball of its regular files and
// directories, paths relative to it. A file that disappears mid-walk is
// skipped: the push lock keeps Git from writing, but a stray process could.
func tarGz(dir string, w io.Writer) error {
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil || rel == "." {
			return err
		}
		name := filepath.ToSlash(rel)
		if strings.HasSuffix(name, ".lock") {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: 0o755,
				ModTime: info.ModTime()})
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			defer f.Close()
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644,
				Size: info.Size(), ModTime: info.ModTime()}); err != nil {
				return err
			}
			_, err = io.CopyN(tw, f, info.Size())
			return err
		default:
			return nil // no symlinks or devices in a bare repository
		}
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return zw.Close()
}

// untarGz extracts a gzipped tarball into dir, which must not exist yet.
// Only directories and regular files are written, and only beneath dir.
func untarGz(r io.Reader, dir string) error {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		clean := path.Clean(h.Name)
		if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") ||
			strings.Contains(clean, ":") || strings.Contains(clean, `\`) {
			return fmt.Errorf("snapshot entry %q is outside the repository", h.Name)
		}
		target := filepath.Join(dir, filepath.FromSlash(clean))
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
			_ = os.Chtimes(target, h.ModTime, h.ModTime)
		}
	}
}

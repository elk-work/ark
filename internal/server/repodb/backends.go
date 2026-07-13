package repodb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
)

// GCSBackend stores repository databases as objects under repos/<id>.db.
// Object generations provide the compare-and-swap.
type GCSBackend struct {
	Client *storage.Client
	Bucket string
}

func (g *GCSBackend) object(repoID string) *storage.ObjectHandle {
	return g.Client.Bucket(g.Bucket).Object("repos/" + repoID + ".db")
}

func (g *GCSBackend) Fetch(ctx context.Context, repoID, destPath string) (int64, error) {
	r, err := g.object(repoID).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("fetch repository db: %w", err)
	}
	defer r.Close()
	f, err := os.Create(destPath)
	if err != nil {
		return 0, err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(destPath)
		return 0, fmt.Errorf("download repository db: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	return r.Attrs.Generation, nil
}

func (g *GCSBackend) Store(ctx context.Context, repoID, srcPath string, ifGeneration int64) (int64, error) {
	f, err := os.Open(srcPath)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	obj := g.object(repoID)
	if ifGeneration == 0 {
		obj = obj.If(storage.Conditions{DoesNotExist: true})
	} else {
		obj = obj.If(storage.Conditions{GenerationMatch: ifGeneration})
	}
	w := obj.NewWriter(ctx)
	w.ContentType = "application/vnd.sqlite3"
	if _, err := io.Copy(w, f); err != nil {
		w.Close()
		return 0, fmt.Errorf("upload repository db: %w", err)
	}
	if err := w.Close(); err != nil {
		var gerr *googleapi.Error
		if errors.As(err, &gerr) && gerr.Code == http.StatusPreconditionFailed {
			return 0, ErrConcurrentWrite
		}
		return 0, fmt.Errorf("store repository db: %w", err)
	}
	return w.Attrs().Generation, nil
}

// LocalBackend stores repository databases in a directory, tracking a
// generation counter in an xattr-free sidecar-less way: the file mtime
// nanoseconds serve as generation. Single-process development and tests.
type LocalBackend struct {
	Dir string
}

func (l *LocalBackend) path(repoID string) string {
	return filepath.Join(l.Dir, repoID+".db")
}

// PathFor exposes where a repository database lives, for tests and tooling
// that treat the file as the snapshot artifact it is.
func (l *LocalBackend) PathFor(repoID string) string { return l.path(repoID) }

func (l *LocalBackend) Fetch(ctx context.Context, repoID, destPath string) (int64, error) {
	src := l.path(repoID)
	info, err := os.Stat(src)
	if os.IsNotExist(err) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if err := copyFile(src, destPath); err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

func (l *LocalBackend) Store(ctx context.Context, repoID, srcPath string, ifGeneration int64) (int64, error) {
	dst := l.path(repoID)
	info, err := os.Stat(dst)
	switch {
	case os.IsNotExist(err):
		if ifGeneration != 0 {
			return 0, ErrConcurrentWrite
		}
	case err != nil:
		return 0, err
	default:
		if info.ModTime().UnixNano() != ifGeneration {
			return 0, ErrConcurrentWrite
		}
	}
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return 0, err
	}
	tmp := dst + ".tmp"
	if err := copyFile(srcPath, tmp); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	info, err = os.Stat(dst)
	if err != nil {
		return 0, err
	}
	return info.ModTime().UnixNano(), nil
}

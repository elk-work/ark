package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

// BlobStore hands out URLs for artifact content, keyed by content hash.
type BlobStore interface {
	SignedPutURL(ctx context.Context, key, mediaType string) (string, error)
	SignedGetURL(ctx context.Context, key string) (string, error)
	Exists(ctx context.Context, key string) (bool, error)
}

// GCSBlobStore signs V4 URLs against a Google Cloud Storage bucket. On
// Cloud Run the default service account signs via the IAM credentials API.
type GCSBlobStore struct {
	Client *storage.Client
	Bucket string
}

func (g *GCSBlobStore) SignedPutURL(ctx context.Context, key, mediaType string) (string, error) {
	return g.Client.Bucket(g.Bucket).SignedURL(key, &storage.SignedURLOptions{
		Method:      http.MethodPut,
		Expires:     time.Now().Add(time.Hour),
		Scheme:      storage.SigningSchemeV4,
		ContentType: mediaType,
	})
}

func (g *GCSBlobStore) SignedGetURL(ctx context.Context, key string) (string, error) {
	return g.Client.Bucket(g.Bucket).SignedURL(key, &storage.SignedURLOptions{
		Method:  http.MethodGet,
		Expires: time.Now().Add(time.Hour),
		Scheme:  storage.SigningSchemeV4,
	})
}

func (g *GCSBlobStore) Exists(ctx context.Context, key string) (bool, error) {
	_, err := g.Client.Bucket(g.Bucket).Object(key).Attrs(ctx)
	if err == storage.ErrObjectNotExist {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// LocalBlobStore keeps blobs on the server's own disk and serves them over
// the API's /blobs/ routes. A development and test stand-in for GCS.
type LocalBlobStore struct {
	Dir     string
	BaseURL string // externally reachable base, e.g. http://host:port
}

func (l *LocalBlobStore) path(key string) (string, error) {
	clean := filepath.Clean(key)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	return filepath.Join(l.Dir, clean), nil
}

func (l *LocalBlobStore) SignedPutURL(ctx context.Context, key, mediaType string) (string, error) {
	return l.BaseURL + "/blobs/" + key, nil
}

func (l *LocalBlobStore) SignedGetURL(ctx context.Context, key string) (string, error) {
	if ok, _ := l.Exists(ctx, key); !ok {
		return "", fmt.Errorf("blob %s not stored", key)
	}
	return l.BaseURL + "/blobs/" + key, nil
}

func (l *LocalBlobStore) Exists(ctx context.Context, key string) (bool, error) {
	p, err := l.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// Handler serves PUT/GET /blobs/<key> for LocalBlobStore deployments.
func (l *LocalBlobStore) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/blobs/")
		p, err := l.path(key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPut:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f, err := os.CreateTemp(filepath.Dir(p), ".upload-*")
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			tmp := f.Name()
			if _, err := io.Copy(f, r.Body); err != nil {
				f.Close()
				os.Remove(tmp)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			f.Close()
			if err := os.Rename(tmp, p); err != nil {
				os.Remove(tmp)
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			http.ServeFile(w, r, p)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

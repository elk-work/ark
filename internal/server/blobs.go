package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/storage"
)

// BlobStore hands out URLs for artifact content, keyed by content hash.
//
// Open and Delete exist so the service can verify that stored bytes actually
// hash to the key they were stored under. Without that check the spec's
// "artifacts are immutable by checksum" is a claim the server never enforces:
// whoever can reach the upload URL decides what lives at a given hash.
type BlobStore interface {
	SignedPutURL(ctx context.Context, key, mediaType string) (string, error)
	SignedGetURL(ctx context.Context, key string) (string, error)
	Exists(ctx context.Context, key string) (bool, error)
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	Delete(ctx context.Context, key string) error
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

func (g *GCSBlobStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	return g.Client.Bucket(g.Bucket).Object(key).NewReader(ctx)
}

func (g *GCSBlobStore) Delete(ctx context.Context, key string) error {
	err := g.Client.Bucket(g.Bucket).Object(key).Delete(ctx)
	if err == storage.ErrObjectNotExist {
		return nil
	}
	return err
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
//
// Its URLs are signed, like GCS's. They have to be: the /blobs/ routes cannot
// sit behind the bearer middleware, because a client treats these as
// pre-signed URLs and sends no Authorization header. Without a signature the
// routes would be an unauthenticated read *and write* of artifact content —
// which is not what "signed URL" means anywhere else in this service.
type LocalBlobStore struct {
	Dir     string
	BaseURL string // externally reachable base, e.g. http://host:port

	// Secret signs URLs. Server.Handler sets it from the service token, so
	// there is no separate key to configure or forget. Empty means unsigned,
	// which Handler refuses to serve.
	Secret string
}

// blobURLTTL bounds how long a signed local URL stays usable. An hour matches
// the GCS signer.
const blobURLTTL = time.Hour

// signBlob authenticates one method+key+expiry triple.
func signBlob(secret, method, key string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	fmt.Fprintf(mac, "%s\n%s\n%d", method, key, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

func (l *LocalBlobStore) signedURL(method, key string) (string, error) {
	if l.Secret == "" {
		return "", fmt.Errorf("local blob store has no signing secret")
	}
	exp := time.Now().Add(blobURLTTL).Unix()
	return fmt.Sprintf("%s/blobs/%s?exp=%d&sig=%s",
		l.BaseURL, key, exp, signBlob(l.Secret, method, key, exp)), nil
}

// verify checks a request's signature against the key it is asking for. A
// signature is bound to the method, so a read URL cannot be replayed as a
// write.
func (l *LocalBlobStore) verify(r *http.Request, key string) bool {
	if l.Secret == "" {
		return false
	}
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	want := signBlob(l.Secret, r.Method, key, exp)
	return hmac.Equal([]byte(r.URL.Query().Get("sig")), []byte(want))
}

func (l *LocalBlobStore) path(key string) (string, error) {
	clean := filepath.Clean(key)
	if strings.Contains(clean, "..") || filepath.IsAbs(clean) {
		return "", fmt.Errorf("invalid blob key %q", key)
	}
	return filepath.Join(l.Dir, clean), nil
}

func (l *LocalBlobStore) SignedPutURL(ctx context.Context, key, mediaType string) (string, error) {
	return l.signedURL(http.MethodPut, key)
}

func (l *LocalBlobStore) SignedGetURL(ctx context.Context, key string) (string, error) {
	if ok, _ := l.Exists(ctx, key); !ok {
		return "", fmt.Errorf("blob %s not stored", key)
	}
	return l.signedURL(http.MethodGet, key)
}

func (l *LocalBlobStore) Open(ctx context.Context, key string) (io.ReadCloser, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	return os.Open(p)
}

func (l *LocalBlobStore) Delete(ctx context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
		// The signature is the only thing standing between this route and
		// anyone on the network; check it before touching the filesystem.
		if !l.verify(r, key) {
			http.Error(w, "invalid or expired blob signature", http.StatusForbidden)
			return
		}
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

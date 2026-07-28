// Command ark-server is the Ark sync service. Repository metadata lives in
// one SQLite database per repository, persisted to GCS in production or a
// local directory in development; artifact blobs go to object storage.
//
// Configuration (environment):
//
//	ARK_API_TOKEN  bearer token clients must present (required)
//	GCS_BUCKET     bucket for repo databases and artifact blobs (production)
//	DATA_DIR       local directory for repo databases + blobs (used without
//	               GCS_BUCKET; default ./data)
//	BASE_URL       externally reachable base URL (required without GCS_BUCKET)
//	CACHE_DIR      scratch space for working copies (default os temp dir)
//	PORT           listen port (default 8080)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"cloud.google.com/go/storage"

	"github.com/elkproject/ark/internal/buildinfo"
	"github.com/elkproject/ark/internal/server"
	"github.com/elkproject/ark/internal/server/repodb"
)

// version is set at build time via -ldflags "-X main.version=...". Left
// unset, buildinfo.Resolve falls back to the module version or the VCS
// stamp Go embeds on its own.
var version = buildinfo.Dev

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ark-server:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	ver := buildinfo.Resolve(version)
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("version", ver)

	token := os.Getenv("ARK_API_TOKEN")
	if token == "" {
		return fmt.Errorf("ARK_API_TOKEN is required")
	}
	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "ark-repos")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}

	var backend repodb.Backend
	var blobs server.BlobStore
	if bucket := os.Getenv("GCS_BUCKET"); bucket != "" {
		client, err := storage.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("gcs client: %w", err)
		}
		backend = &repodb.GCSBackend{Client: client, Bucket: bucket}
		blobs = &server.GCSBlobStore{Client: client, Bucket: bucket}
	} else {
		base := os.Getenv("BASE_URL")
		if base == "" {
			return fmt.Errorf("BASE_URL is required when GCS_BUCKET is unset")
		}
		dir := os.Getenv("DATA_DIR")
		if dir == "" {
			dir = "data"
		}
		backend = &repodb.LocalBackend{Dir: filepath.Join(dir, "repos")}
		blobs = &server.LocalBlobStore{Dir: filepath.Join(dir, "blobs"), BaseURL: base}
	}

	s := &server.Server{
		Repos:   repodb.NewManager(backend, cacheDir),
		Token:   token,
		Blobs:   blobs,
		Log:     log,
		Version: ver,
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Info("listening", "port", port)
	return httpServer.ListenAndServe()
}

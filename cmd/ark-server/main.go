// Command ark-server is the Ark sync service: Cloud Run + Cloud SQL + GCS
// in production, or any Postgres plus local blob storage in development.
//
// Configuration (environment):
//
//	DATABASE_URL   Postgres DSN (required)
//	ARK_API_TOKEN  bearer token clients must present (required)
//	GCS_BUCKET     bucket for artifact blobs (omit to store blobs on disk)
//	BLOB_DIR       local blob directory (default ./blobs; used without GCS_BUCKET)
//	BASE_URL       externally reachable base URL (required without GCS_BUCKET)
//	PORT           listen port (default 8080)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"cloud.google.com/go/storage"

	"github.com/ijroth/ark/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ark-server:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}
	token := os.Getenv("ARK_API_TOKEN")
	if token == "" {
		return fmt.Errorf("ARK_API_TOKEN is required")
	}

	db, err := server.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer db.Close()

	var blobs server.BlobStore
	if bucket := os.Getenv("GCS_BUCKET"); bucket != "" {
		client, err := storage.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("gcs client: %w", err)
		}
		blobs = &server.GCSBlobStore{Client: client, Bucket: bucket}
	} else {
		base := os.Getenv("BASE_URL")
		if base == "" {
			return fmt.Errorf("BASE_URL is required when GCS_BUCKET is unset")
		}
		dir := os.Getenv("BLOB_DIR")
		if dir == "" {
			dir = "blobs"
		}
		blobs = &server.LocalBlobStore{Dir: dir, BaseURL: base}
	}

	s := &server.Server{DB: db, Token: token, Blobs: blobs, Log: log}
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

// Package servertest builds fully local sync servers for tests: SQLite
// repository databases and blob storage in temp directories. No external
// services are required.
package servertest

import (
	"testing"

	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/server/repodb"
)

// Token is the bearer token test servers accept.
const Token = "test-token"

// NewServer returns a sync server backed by temp-dir storage.
func NewServer(t *testing.T) *server.Server {
	t.Helper()
	return &server.Server{
		Repos: repodb.NewManager(&repodb.LocalBackend{Dir: t.TempDir()}, t.TempDir()),
		Token: Token,
		Blobs: &server.LocalBlobStore{Dir: t.TempDir()},
	}
}

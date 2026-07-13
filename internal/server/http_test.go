package server

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// doRequest exercises the real handler with auth.
func doRequest(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+s.Token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest("POST", "/v1/sync/pull", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no token: %d, want 401", rec.Code)
	}
	req = httptest.NewRequest("POST", "/v1/sync/pull", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong token: %d, want 401", rec.Code)
	}
}

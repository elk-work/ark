package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

func uiRequest(t *testing.T, s *Server, method, path, body, origin string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, "https://board.test"+path, strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func signIntoUI(t *testing.T, s *Server, credential string, old *http.Cookie) *http.Cookie {
	t.Helper()
	w := uiRequest(t, s, "POST", "/ui/session", url.Values{"credential": {credential}}.Encode(), "https://board.test", old)
	if w.Code != 303 {
		t.Fatalf("sign in: %d %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies: %v", cookies)
	}
	c := cookies[0]
	if c.Name != uiCookie || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" || c.Domain != "" || c.MaxAge != 43200 {
		t.Fatalf("insecure cookie: %+v", c)
	}
	if strings.Contains(w.Body.String(), credential) {
		t.Fatal("credential echoed")
	}
	return c
}

func TestUISessionBoundaries(t *testing.T) {
	a, cred := grantedServer(t, "read")
	c := signIntoUI(t, a.Server, cred.Token, nil)
	path := "/v1/repositories/" + repoID + "/tasks"
	for _, p := range []string{"/ui/", "/ui/" + repoID, path} {
		w := uiRequest(t, a.Server, "GET", p, "", "", c)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Security-Policy") == "" || w.Header().Get("Referrer-Policy") != "same-origin" {
			t.Fatal("missing private page headers")
		}
	}
	// No session token is a bearer credential; no cookie reaches old writes.
	if w := doRequestAs(t, a.Server, c.Value, "GET", path, ""); w.Code != 401 {
		t.Fatalf("session accepted as bearer: %d", w.Code)
	}
	if w := uiRequest(t, a.Server, "POST", "/v1/sync/push", `{}`, "https://board.test", c); w.Code != 401 {
		t.Fatalf("cookie reached write API: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "https://board.test"+path, nil)
	r.AddCookie(c)
	r.Header.Set("Authorization", "Bearer bad")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatalf("bad bearer fell back to cookie: %d", w.Code)
	}
	for _, origin := range []string{"", "https://evil.test", "http://board.test", "https://board.test/path"} {
		for _, p := range []string{"/ui/session", "/ui/logout"} {
			w := uiRequest(t, a.Server, "POST", p, url.Values{"credential": {cred.Token}}.Encode(), origin, c)
			if w.Code != 403 {
				t.Fatalf("origin %q %s: %d", origin, p, w.Code)
			}
		}
	}
	for _, token := range []string{a.Token, testBootstrap, "arkc_unknown"} {
		w := uiRequest(t, a.Server, "POST", "/ui/session", url.Values{"credential": {token}}.Encode(), "https://board.test", nil)
		if w.Code != 401 {
			t.Fatalf("bad credential accepted: %d", w.Code)
		}
	}
	// Rotation deletes the old row, and logout removes the new one.
	next := signIntoUI(t, a.Server, cred.Token, c)
	if next.Value == c.Value {
		t.Fatal("session not rotated")
	}
	if w := uiRequest(t, a.Server, "GET", path, "", "", c); w.Code != 401 {
		t.Fatalf("old session survived rotation: %d", w.Code)
	}
	if w := uiRequest(t, a.Server, "POST", "/ui/logout", "", "https://board.test", next); w.Code != 303 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := uiRequest(t, a.Server, "GET", path, "", "", next); w.Code != 401 {
		t.Fatalf("session survived logout: %d", w.Code)
	}
}

func TestUISessionRevocationBoundAndExpiry(t *testing.T) {
	for _, kind := range []string{"revoke", "disable", "credential expiry", "session expiry"} {
		t.Run(kind, func(t *testing.T) {
			a, cred := grantedServer(t, "read")
			at := time.Now().UTC()
			a.authStore().now = func() time.Time { return at }
			c := signIntoUI(t, a.Server, cred.Token, nil)
			path := "/v1/repositories/" + repoID + "/tasks"
			if w := uiRequest(t, a.Server, "GET", path, "", "", c); w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			other := a.otherInstance(t)
			switch kind {
			case "revoke":
				mustUpdateAuth(t, other, `UPDATE credentials SET revoked_at=? WHERE id=?`, records.Now(), cred.CredentialID)
			case "disable":
				mustUpdateAuth(t, other, `UPDATE principals SET disabled_at=? WHERE id=?`, records.Now(), cred.Principal.ID)
			case "credential expiry":
				mustUpdateAuth(t, other, `UPDATE credentials SET expires_at=? WHERE id=?`, at.Add(-time.Hour).Format(time.RFC3339), cred.CredentialID)
			case "session expiry":
				at = at.Add(uiLifetime)
			}
			if kind != "session expiry" {
				at = at.Add(authTTL - time.Second)
				if w := uiRequest(t, a.Server, "GET", path, "", "", c); w.Code != 200 {
					t.Fatalf("cache contract: %d", w.Code)
				}
				at = at.Add(time.Second)
			}
			if w := uiRequest(t, a.Server, "GET", path, "", "", c); w.Code != 401 {
				t.Fatalf("%s still valid after bound: %d %s", kind, w.Code, w.Body.String())
			}
		})
	}
}

func TestUIGrantsGateSwitcherAndDirectReads(t *testing.T) {
	a, cred := grantedServer(t, "read")
	if w := doRequestAs(t, a.Server, a.Token, "POST", "/v1/repositories", fmt.Sprintf(`{"id":%q,"name":"Secret repository"}`, secondRepoID)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a.DefaultGrant = "read" // This does not widen the board's explicit-grant set.
	c := signIntoUI(t, a.Server, cred.Token, nil)
	w := uiRequest(t, a.Server, "GET", "/ui/", "", "", c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), repoID) || strings.Contains(w.Body.String(), secondRepoID) || strings.Contains(w.Body.String(), "Secret repository") {
		t.Fatalf("switcher leaks/omits repository: %s", w.Body.String())
	}
	for _, p := range []string{"/ui/" + secondRepoID, "/v1/repositories/" + secondRepoID + "/tasks", "/v1/repositories/" + secondRepoID + "/tasks/" + records.NewID()} {
		if w := uiRequest(t, a.Server, "GET", p, "", "", c); w.Code != 403 {
			t.Fatalf("ungranted %s: %d", p, w.Code)
		}
	}
	// This principal is an operator, yet still only sees its explicit grants.
	for _, level := range []string{"read", "write", "admin"} {
		grantTo(t, a, secondRepoID, cred.Principal.Email, level)
		w := uiRequest(t, a.Server, "GET", "/ui/", "", "", c)
		if !strings.Contains(w.Body.String(), secondRepoID) {
			t.Fatalf("%s grant missing", level)
		}
	}
	if w := doRequestAs(t, a.Server, a.Token, "POST", grantPath(secondRepoID), fmt.Sprintf(`{"email":%q,"revoke":true}`, cred.Principal.Email)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := uiRequest(t, a.Server, "GET", "/v1/repositories/"+secondRepoID+"/tasks", "", "", c); w.Code != 403 {
		t.Fatalf("revoked grant still works: %d", w.Code)
	}
}

// Like servertest.NewServer, all data lives in temporary SQLite files over the
// local backend. Seed sync-format documents to test relationships independently
// of the task-create route, including records synced in arbitrary order.
func seedBoardRecord(t *testing.T, s *Server, kind, id string, fields map[string]any, deleted bool) {
	t.Helper()
	fields["id"] = id
	if _, ok := fields["created_at"]; !ok {
		fields["created_at"] = "2026-09-24T20:00:00Z"
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Repos.Update(context.Background(), repoID, false, func(tx *sql.Tx) error {
		var deletion any
		if deleted {
			deletion = records.Now()
		}
		_, err := tx.Exec(`INSERT INTO records(record_type,record_id,data,server_revision,deleted_at,created_at,updated_at) VALUES(?,?,?,1,?,?,?)`, kind, id, string(raw), deletion, records.Now(), records.Now())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestBoardReadsFiltersDetailAndEscaping(t *testing.T) {
	a, cred := grantedServer(t, "read")
	ids := []string{}
	for i, status := range records.TaskStatuses {
		id := records.NewID()
		ids = append(ids, id)
		actor := humanID
		if i == 1 {
			actor = "missing-actor"
		}
		seedBoardRecord(t, a.Server, "task", id, map[string]any{"number": i + 1, "title": "<script>alert(1)</script>", "status": status, "body": "Parent elk:old", "created_by": actor, "created_by_type": "human", "updated_at": "2026-09-24T21:00:00Z"}, false)
	}
	task := ids[0]
	seedBoardRecord(t, a.Server, "task", records.NewID(), map[string]any{"number": 6, "title": "Deleted secret", "status": "open"}, true)
	for i, body := range []string{"elk-parent: #34", "elk-parent: elk:35\nhttps://elk.work/open/example", "Later mention elk:unrelated"} {
		seedBoardRecord(t, a.Server, "comment", records.NewID(), map[string]any{"parent_type": "task", "parent_id": task, "body": body, "created_at": fmt.Sprintf("2026-09-24T20:00:0%dZ", i), "supersedes_id": "older-comment"}, false)
	}
	seedBoardRecord(t, a.Server, "comment", records.NewID(), map[string]any{"parent_type": "task", "parent_id": task, "body": "elk-parent: deleted"}, true)
	run, pr, review := records.NewID(), records.NewID(), records.NewID()
	seedBoardRecord(t, a.Server, "agent_run", run, map[string]any{"task_id": task, "agent_name": "Builder", "status": "succeeded", "result_summary": "All done"}, false)
	seedBoardRecord(t, a.Server, "pull_request", pr, map[string]any{"task_id": task, "number": 42, "title": "Linked PR", "status": "open"}, false)
	seedBoardRecord(t, a.Server, "review", review, map[string]any{"pull_request_id": pr}, false)
	for _, parent := range [][2]string{{"task", task}, {"agent_run", run}, {"pull_request", pr}, {"review", review}, {"task", ids[1]}} {
		seedBoardRecord(t, a.Server, "artifact", records.NewID(), map[string]any{"parent_type": parent[0], "parent_id": parent[1], "name": "Evidence", "sha256": "abc", "size_bytes": 10}, false)
	}
	path := "/v1/repositories/" + repoID + "/tasks"
	for _, tc := range []struct {
		query string
		want  int
	}{{"", 5}, {"?status=all", 5}, {"?status=open", 1}, {"?status=closed", 1}, {"?status=in_progress", 1}, {"?status=blocked", 1}, {"?status=done", 1}, {"?actor=" + humanID, 4}, {"?actor=missing-actor", 1}, {"?elk=%2335", 1}, {"?elk=elk:35", 1}, {"?elk=elk%2335", 1}, {"?elk=missing", 0}, {"?status=closed&elk=35", 0}} {
		w := doRequestAs(t, a.Server, cred.Token, "GET", path+tc.query, "")
		if w.Code != 200 {
			t.Fatalf("list %s: %d %s", tc.query, w.Code, w.Body.String())
		}
		var out api.TaskListResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Tasks) != tc.want {
			t.Fatalf("list %s: %d want %d", tc.query, len(out.Tasks), tc.want)
		}
		if out.Tasks == nil {
			t.Fatal("nil tasks array")
		}
	}
	if w := doRequestAs(t, a.Server, cred.Token, "GET", path+"?status=working", ""); w.Code != 400 {
		t.Fatalf("invalid status: %d", w.Code)
	}
	w := doRequestAs(t, a.Server, cred.Token, "GET", path+"/"+task, "")
	var out api.TaskDetailResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(out.Comments) != 3 || len(out.Runs) != 1 || len(out.PullRequests) != 1 || len(out.Artifacts) != 4 {
		t.Fatalf("detail: %d %s", w.Code, w.Body.String())
	}
	if out.Task.ElkRef != "#35" || out.Task.ElkURL != "https://elk.work/open/example" || out.Task.Actor.Name != "Alice" || out.Task.Body == "" {
		t.Fatalf("task projection: %+v", out.Task)
	}
	if !strings.Contains(string(out.Comments[0]), "#34") || !strings.Contains(string(out.Comments[2]), "Later mention") {
		t.Fatal("comments not oldest-first")
	}
	for _, id := range []string{records.NewID(), "14"} {
		want := 404
		if id == "14" {
			want = 400
		}
		if w := doRequestAs(t, a.Server, cred.Token, "GET", path+"/"+id, ""); w.Code != want {
			t.Fatalf("missing task %s: %d", id, w.Code)
		}
	}
	c := signIntoUI(t, a.Server, cred.Token, nil)
	w = uiRequest(t, a.Server, "GET", "/ui/"+repoID+"?task="+task, "", "", c)
	if w.Code != 200 {
		t.Fatalf("HTML: %d %s", w.Code, w.Body.String())
	}
	html := w.Body.String()
	for _, text := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "Linked PR", "Evidence", "All done", "Corrects older-comment", "Comments", "Artifacts"} {
		if !strings.Contains(html, text) {
			t.Errorf("missing HTML %q", text)
		}
	}
	if strings.Contains(html, "<script>alert") || strings.Contains(html, "Deleted secret") {
		t.Fatal("unsafe/deleted HTML")
	}
}

func TestUIOffHidesOnlyTheNewSurface(t *testing.T) {
	a, cred := grantedServer(t, "read")
	a.UIMode = "off"
	for _, tc := range [][2]string{{"GET", "/ui/"}, {"GET", "/ui/" + repoID}, {"POST", "/ui/session"}, {"POST", "/ui/logout"}, {"GET", "/ui/assets/board.js"}, {"GET", "/v1/repositories/" + repoID + "/tasks"}, {"GET", "/v1/repositories/" + repoID + "/tasks/" + records.NewID()}} {
		if w := doRequestAs(t, a.Server, cred.Token, tc[0], tc[1], ""); w.Code != 404 {
			t.Errorf("off %s %s: %d", tc[0], tc[1], w.Code)
		}
	}
	if w := doRequestAs(t, a.Server, cred.Token, "GET", "/v1/repositories/"+repoID, ""); w.Code != 200 {
		t.Fatal("existing read disabled")
	}
	if w := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/repositories/"+repoID+"/tasks", `{}`); w.Code == 404 {
		t.Fatal("existing task write removed")
	}
}

func TestBoardParentConservativeFallback(t *testing.T) {
	for _, tc := range []struct{ body, want string }{{"Fix #115", ""}, {"Parent elk#35", "#35"}, {"Parent Elk #35", "#35"}, {"Parent elk:35", "#35"}, {"Parent elk:ab12-cd34", "ab12-cd34"}} {
		ref, link := boardParent(tc.body, nil)
		if ref != tc.want || link != "" {
			t.Errorf("%q = %q %q", tc.body, ref, link)
		}
	}
	ref, link := boardParent("elk:35 https://evil.test/open/workspace", nil)
	if ref != "#35" || link != "" {
		t.Fatalf("unsafe parent URL: %q", link)
	}
}

func TestUISessionStoredHashedAndRevokedThroughAPI(t *testing.T) {
	a, cred := grantedServer(t, "read")
	c := signIntoUI(t, a.Server, cred.Token, nil)
	db, err := openAuthDB(a.authDBPath())
	if err != nil {
		t.Fatal(err)
	}
	var hash, id string
	err = db.QueryRow(`SELECT token_sha256, credential_id FROM ui_sessions`).Scan(&hash, &id)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if hash != hashCredential(c.Value) || id != cred.CredentialID || hash == c.Value || hash == cred.Token {
		t.Fatal("session not stored as a credential-linked hash")
	}
	w := doRequestAs(t, a.Server, cred.Token, "POST", "/v1/credentials/"+cred.CredentialID+"/revoke", `{}`)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w := uiRequest(t, a.Server, "GET", "/v1/repositories/"+repoID+"/tasks", "", "", c); w.Code != 401 {
		t.Fatalf("revoked credential session: %d", w.Code)
	}
}

func TestUIEmptyStateAndSafeParentLinks(t *testing.T) {
	a := newAuthServer(t)
	cred := mintCredentialFor(t, a, "no-grants@example.com")
	c := signIntoUI(t, a.Server, cred.Token, nil)
	w := uiRequest(t, a.Server, "GET", "/ui/", "", "", c)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "No repositories have been shared") {
		t.Fatalf("empty switcher: %s", w.Body.String())
	}
	for _, unsafe := range []string{"https://elk.work.evil.test/open/foo", "https://elk.work/open/foo/extra", "https://elk.work/open/foo?token=bad", "https://evil.test/https://elk.work/open/foo", "https://user@elk.work/open/foo"} {
		if got := parentWorkspace(unsafe); got != "" {
			t.Errorf("unsafe workspace %q -> %q", unsafe, got)
		}
	}
}

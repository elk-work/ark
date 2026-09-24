package server

import (
	"bytes"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

//go:embed ui/*
var uiFiles embed.FS

var uiTemplate = template.Must(template.New("board.html").Funcs(template.FuncMap{
	"label": func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	"age": func(s string) string {
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return s
		}
		d := time.Since(t)
		if d < time.Hour {
			return "less than an hour ago"
		}
		if d < 24*time.Hour {
			return fmt.Sprintf("%dh ago", int(d.Hours()))
		}
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	},
}).ParseFS(uiFiles, "ui/board.html"))

type uiColumn struct {
	Status string
	Tasks  []api.BoardTask
}
type uiDetail struct {
	Task                           api.BoardTask
	Comments, Runs, PRs, Artifacts []boardRecord
}
type uiPage struct {
	SignIn             bool
	Repos              []api.RepositoryMetadata
	Repo               api.RepositoryMetadata
	Columns            []uiColumn
	Actors             []api.BoardActor
	Status, Actor, Elk string
	Detail             *uiDetail
}

func ParseUIMode(value string) (string, error) {
	switch value {
	case "", "on":
		return "on", nil
	case "off":
		return "off", nil
	default:
		return "", fmt.Errorf("ARK_UI is %q; it takes on, off", value)
	}
}

func uiHeaders(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next(w, r)
	}
}

func (s *Server) registerUI(mux *http.ServeMux) {
	// Existing task POST routes remain intact. Handler hides all /ui methods.
	if s.UIMode == "off" {
		off := func(w http.ResponseWriter, r *http.Request) { writeErr(w, 404, "not_found", "unknown route") }
		mux.HandleFunc("GET /v1/repositories/{repo}/tasks", off)
		mux.HandleFunc("GET /v1/repositories/{repo}/tasks/{id}", off)
		return
	}
	mux.HandleFunc("GET /ui/{$}", uiHeaders(s.uiAuth(s.handleUI, true)))
	mux.HandleFunc("GET /ui/{repo}", uiHeaders(s.uiAuth(s.handleUI, true)))
	mux.HandleFunc("POST /ui/session", uiHeaders(s.handleUISession))
	mux.HandleFunc("POST /ui/logout", uiHeaders(s.handleUILogout))
	for _, asset := range []string{"board.css", "board.js"} {
		mux.HandleFunc("GET /ui/assets/"+asset, uiHeaders(func(w http.ResponseWriter, r *http.Request) {
			data, err := uiFiles.ReadFile("ui/" + asset)
			if err != nil {
				s.internal(w, "read board asset", err)
				return
			}
			if strings.HasSuffix(asset, ".css") {
				w.Header().Set("Content-Type", "text/css; charset=utf-8")
			} else {
				w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			}
			_, _ = w.Write(data)
		}))
	}
	mux.HandleFunc("GET /v1/repositories/{repo}/tasks", uiHeaders(s.uiAuth(s.handleTaskList, false)))
	mux.HandleFunc("GET /v1/repositories/{repo}/tasks/{id}", uiHeaders(s.uiAuth(s.handleTaskDetail, false)))
}

func (s *Server) renderUI(w http.ResponseWriter, p uiPage) {
	var buf bytes.Buffer
	if err := uiTemplate.Execute(&buf, p); err != nil {
		s.internal(w, "render board", err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	repo := r.PathValue("repo")
	if repo != "" && !s.allowUI(w, r, repo) {
		return
	}
	who, _ := principalFrom(r.Context())
	snap, err := s.authStore().snapshot(r.Context())
	if err != nil {
		s.internal(w, "list repositories", err)
		return
	}
	p := uiPage{Status: r.URL.Query().Get("status"), Actor: r.URL.Query().Get("actor"), Elk: r.URL.Query().Get("elk")}
	if p.Status == "" {
		p.Status = "all"
	}
	for _, g := range snap.grants {
		if g.PrincipalID != who.ID || !atLeast(g.Level, "read") {
			continue
		}
		var meta api.RepositoryMetadata
		err := s.Repos.View(r.Context(), g.RepositoryID, func(db *sql.DB) error { return loadMetadata(db.QueryRowContext(r.Context(), metadataQuery), &meta) })
		if errors.Is(err, repodb.ErrNotFound) {
			continue
		}
		if err != nil {
			s.respond(w, "list repositories", err)
			return
		}
		p.Repos = append(p.Repos, meta)
	}
	sort.Slice(p.Repos, func(i, j int) bool {
		if p.Repos[i].Name != p.Repos[j].Name {
			return p.Repos[i].Name < p.Repos[j].Name
		}
		return p.Repos[i].ID < p.Repos[j].ID
	})
	if repo == "" {
		s.renderUI(w, p)
		return
	}
	b, err := s.readBoard(r.Context(), repo)
	if err != nil {
		s.respond(w, "read board", err)
		return
	}
	p.Repo = b.Repo
	list, err := b.list(r.URL.Query())
	if err != nil {
		writeErr(w, 400, "validation", err.Error())
		return
	}
	for _, status := range records.TaskStatuses {
		col := uiColumn{Status: status}
		for _, t := range list.Tasks {
			if t.Status == status {
				col.Tasks = append(col.Tasks, t)
			}
		}
		p.Columns = append(p.Columns, col)
	}
	seen := map[string]bool{}
	for _, record := range b.Records["task"] {
		actor := b.task(record, false).Actor
		if !seen[actor.ID] {
			seen[actor.ID] = true
			p.Actors = append(p.Actors, actor)
		}
	}
	sort.Slice(p.Actors, func(i, j int) bool {
		if p.Actors[i].Name != p.Actors[j].Name {
			return p.Actors[i].Name < p.Actors[j].Name
		}
		return p.Actors[i].ID < p.Actors[j].ID
	})
	if id := r.URL.Query().Get("task"); id != "" {
		if !records.ValidID(id) {
			writeErr(w, 400, "validation", "task must be a full ULID")
			return
		}
		detail, err := b.detail(id)
		if err != nil {
			s.finish(w, "read task", err, nil, 200)
			return
		}
		p.Detail = &uiDetail{Task: detail.Task}
		groups := []struct {
			raw  []json.RawMessage
			dest *[]boardRecord
		}{{detail.Comments, &p.Detail.Comments}, {detail.Runs, &p.Detail.Runs}, {detail.PullRequests, &p.Detail.PRs}, {detail.Artifacts, &p.Detail.Artifacts}}
		for _, g := range groups {
			for _, raw := range g.raw {
				var rec boardRecord
				if err := json.Unmarshal(raw, &rec); err != nil {
					s.internal(w, "render task", err)
					return
				}
				*g.dest = append(*g.dest, rec)
			}
		}
	}
	s.renderUI(w, p)
}

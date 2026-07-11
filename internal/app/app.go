// Package app wires a command invocation to its repository: it finds .ark,
// opens the database, loads configuration, and resolves the acting identity.
package app

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"

	"github.com/ijroth/ark/internal/config"
	"github.com/ijroth/ark/internal/db"
	"github.com/ijroth/ark/internal/git"
	"github.com/ijroth/ark/internal/records"
	"github.com/ijroth/ark/internal/store"
)

// Context is everything a command needs to operate on one repository.
type Context struct {
	Root   string // repository root (contains .git and .ark)
	ArkDir string
	Config *config.Config
	DB     *sql.DB
	Git    *git.Repo
	Store  *store.Store
}

func (c *Context) Close() {
	if c.DB != nil {
		c.DB.Close()
	}
}

// Options carries identity overrides from global CLI flags and environment.
type Options struct {
	ActorID      string // --actor / ARK_ACTOR_ID
	AgentName    string // --agent / ARK_AGENT_NAME
	AgentVersion string // ARK_AGENT_VERSION
	DelegatedBy  string // ARK_DELEGATED_BY
	Debug        func(format string, args ...any)
}

// FindRoot walks up from dir looking for a directory containing .ark.
func FindRoot(dir string) (string, error) {
	d, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if fi, err := os.Stat(filepath.Join(d, ".ark")); err == nil && fi.IsDir() {
			return d, nil
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", records.NotFoundf("no .ark directory found (run `ark init` inside a Git repository)")
		}
		d = parent
	}
}

// Open loads the Ark context for the repository containing dir.
func Open(ctx context.Context, dir string, opts Options) (*Context, error) {
	root, err := FindRoot(dir)
	if err != nil {
		return nil, err
	}
	arkDir := filepath.Join(root, ".ark")
	cfg, err := config.Load(arkDir)
	if err != nil {
		return nil, err
	}
	d, err := db.Open(filepath.Join(arkDir, "ark.db"))
	if err != nil {
		return nil, err
	}
	actor, err := resolveActor(ctx, d, cfg, opts)
	if err != nil {
		d.Close()
		return nil, err
	}
	return &Context{
		Root:   root,
		ArkDir: arkDir,
		Config: cfg,
		DB:     d,
		Git:    &git.Repo{Dir: root, Debug: opts.Debug},
		Store:  &store.Store{DB: d, RepoID: cfg.RepositoryID, Actor: *actor},
	}, nil
}

// resolveActor picks the acting identity: an explicit actor ID, a named
// agent (created on first use, delegated by the default human), or the
// repository's default actor.
func resolveActor(ctx context.Context, d *sql.DB, cfg *config.Config, opts Options) (*store.Actor, error) {
	if opts.ActorID != "" {
		return store.GetActor(ctx, d, opts.ActorID)
	}
	if opts.AgentName != "" {
		delegatedBy := opts.DelegatedBy
		if delegatedBy == "" {
			delegatedBy = cfg.DefaultActorID
		}
		return store.FindAgentActor(ctx, d, opts.AgentName, opts.AgentVersion, delegatedBy)
	}
	return store.GetActor(ctx, d, cfg.DefaultActorID)
}

// InitResult reports what `ark init` created.
type InitResult struct {
	RepositoryID   string `json:"repository_id"`
	Root           string `json:"root"`
	Name           string `json:"name"`
	DefaultBranch  string `json:"default_branch"`
	GitRemoteURL   string `json:"git_remote_url,omitempty"`
	DefaultActorID string `json:"default_actor_id"`
	ActorName      string `json:"actor_name"`
}

// Init creates .ark inside the Git repository containing dir.
func Init(ctx context.Context, dir string) (*InitResult, error) {
	root, err := git.TopLevel(ctx, dir)
	if err != nil {
		return nil, err
	}
	arkDir := filepath.Join(root, ".ark")
	if _, err := os.Stat(filepath.Join(arkDir, "config.toml")); err == nil {
		return nil, records.Validationf("Ark is already initialized at %s", arkDir)
	}
	for _, sub := range []string{"", "objects", "tmp"} {
		if err := os.MkdirAll(filepath.Join(arkDir, sub), 0o755); err != nil {
			return nil, &records.Error{Kind: records.KindGeneral, Message: "create .ark", Err: err}
		}
	}

	g := &git.Repo{Dir: root}
	name := filepath.Base(root)
	remote := g.RemoteURL(ctx, "origin")
	branch := g.DefaultBranch(ctx)

	d, err := db.Open(filepath.Join(arkDir, "ark.db"))
	if err != nil {
		return nil, err
	}
	defer d.Close()

	// Default human actor from Git identity.
	userName, _ := gitConfig(ctx, g, "user.name")
	userEmail, _ := gitConfig(ctx, g, "user.email")
	if userName == "" {
		userName = "unknown"
	}
	actor := &store.Actor{Type: records.ActorHuman, Name: userName, Email: userEmail}
	if err := store.CreateActor(ctx, d, actor); err != nil {
		return nil, err
	}

	repoID := records.NewID()
	if _, err := d.ExecContext(ctx, `INSERT INTO repositories
		(id, name, path, git_remote_url, default_branch, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		repoID, name, root, remote, branch, records.Now()); err != nil {
		return nil, records.DBErr("create repository record", err)
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO sync_state (repository_id, client_id, last_revision)
		VALUES (?, ?, 0)`, repoID, records.NewID()); err != nil {
		return nil, records.DBErr("create sync state", err)
	}

	cfg := &config.Config{
		Version:          1,
		RepositoryID:     repoID,
		DefaultActorID:   actor.ID,
		DefaultActorType: string(records.ActorHuman),
	}
	if err := config.Save(arkDir, cfg); err != nil {
		return nil, err
	}

	ensureGitignore(root)

	return &InitResult{
		RepositoryID:   repoID,
		Root:           root,
		Name:           name,
		DefaultBranch:  branch,
		GitRemoteURL:   remote,
		DefaultActorID: actor.ID,
		ActorName:      userName,
	}, nil
}

func gitConfig(ctx context.Context, g *git.Repo, key string) (string, error) {
	res, err := g.Run(ctx, "config", "--get", key)
	if err != nil {
		return "", err
	}
	return trimNL(res.Stdout), nil
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

// ensureGitignore adds .ark/ to the repository's .gitignore so Ark state
// never lands in source history. Best effort; init succeeds without it.
func ensureGitignore(root string) {
	path := filepath.Join(root, ".gitignore")
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return
	}
	for _, line := range splitLines(string(body)) {
		if line == ".ark/" || line == ".ark" {
			return
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	prefix := ""
	if len(body) > 0 && body[len(body)-1] != '\n' {
		prefix = "\n"
	}
	f.WriteString(prefix + ".ark/\n")
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			out = append(out, line)
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

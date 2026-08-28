// Package app wires a command invocation to its repository: it finds .ark,
// opens the database, loads configuration, and resolves the acting identity.
package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"

	"github.com/elk-work/ark/internal/config"
	"github.com/elk-work/ark/internal/db"
	"github.com/elk-work/ark/internal/git"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
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
	s := &store.Store{DB: d, RepoID: cfg.RepositoryID, Actor: *actor}
	if push := newElkFilingPush(root, arkDir, opts.Debug); push != nil {
		s.AfterMutation = push.launch
	}
	return &Context{
		Root:   root,
		ArkDir: arkDir,
		Config: cfg,
		DB:     d,
		Git:    &git.Repo{Dir: root, Debug: opts.Debug},
		Store:  s,
	}, nil
}

// resolveActor picks the acting identity: an explicit actor ID, a named
// agent (created on first use, delegated by the default human), or the
// repository's default actor.
//
// A named agent's identity is per (agent name, delegating human) — see
// store.FindAgentActor — so which human this invocation delegates from is
// what decides which actor it writes as, and no longer a detail that only
// matters the first time a name is used. Two settings supply it.
func resolveActor(ctx context.Context, d *sql.DB, cfg *config.Config, opts Options) (*store.Actor, error) {
	if opts.ActorID != "" {
		return store.GetActor(ctx, d, opts.ActorID)
	}
	if opts.AgentName != "" {
		delegatedBy, source := opts.DelegatedBy, "ARK_DELEGATED_BY"
		if delegatedBy == "" {
			delegatedBy, source = cfg.DefaultActorID, "default_actor_id in .ark/config.toml"
		}
		actor, err := store.FindAgentActor(ctx, d, opts.AgentName, opts.AgentVersion, delegatedBy)
		if err != nil {
			return nil, delegationSource(err, source)
		}
		return actor, nil
	}
	return store.GetActor(ctx, d, cfg.DefaultActorID)
}

// delegationSource names the setting that supplied a delegation the store
// refused. The store validates the value; only this layer knows where it came
// from, and which of the two knobs to turn is the whole of what the reader
// needs. Anything that is not a rejected delegation passes through untouched.
func delegationSource(err error, source string) error {
	var e *records.Error
	if errors.As(err, &e) && e.Kind == records.KindValidation {
		return records.Validationf("%s (from %s)", e.Message, source)
	}
	return err
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

// Init creates .ark inside the Git repository containing dir. A non-empty
// repoID joins an existing Ark repository (a second client of the same
// project) instead of minting a new identity; the first sync then pulls the
// shared history.
func Init(ctx context.Context, dir, repoID string) (*InitResult, error) {
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
	// .ark ignores itself so `git add .` can never sweep Ark state into a
	// commit, even on branches where the repository .gitignore is absent.
	if err := os.WriteFile(filepath.Join(arkDir, ".gitignore"), []byte("*\n"), 0o644); err != nil {
		return nil, &records.Error{Kind: records.KindGeneral, Message: "create .ark/.gitignore", Err: err}
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

	if repoID == "" {
		repoID = records.NewID()
	} else if !records.ValidID(repoID) {
		return nil, records.Validationf("invalid repository id %q", repoID)
	}
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

// Ark deliberately does NOT add a `.ark/` rule to the repository's .gitignore.
// `.ark/.gitignore` contains `*` (written above), so Ark state ignores itself
// on every branch, including ones where the repository .gitignore is absent —
// a repository-level rule adds nothing. It did add harm: nothing committed the
// rule, so every `ark init` left a stray untracked .gitignore that reads as an
// un-ignored SQLite database and sends people hunting a hazard that does not
// exist. Verified: with no repository-level rule at all, `.ark/` does not
// appear in `git status`.

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

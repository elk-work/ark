package server

// Git hosting: the smart HTTP transport (elk-work/ark#125).
//
// The service holds one bare Git repository per Ark repository and serves it
// to ordinary Git clients, so `git clone`, `git fetch` and `git push` work
// against `<service>/git/<repository id>` with no client of Ark's in the
// path. Principle 001 ("Do Not Rebuild Git") is the whole design: every byte
// of the protocol is spoken by `git upload-pack` and `git receive-pack`
// themselves, run with `--stateless-rpc` against a bare repository the
// service owns. This file authenticates the request, finds the repository,
// and moves bytes between the client and those two programs. It adds nothing
// to Git's model — no refs of its own, no hooks, no object store.
//
// Three routes, keyed by the repository ULID:
//
//	GET  /git/{repo}/info/refs?service=git-upload-pack|git-receive-pack
//	POST /git/{repo}/git-upload-pack
//	POST /git/{repo}/git-receive-pack
//
// Dumb HTTP is not served. A trailing ".git" on {repo} is accepted, because
// that is how people write clone URLs.
//
// Authentication is the credential every other route takes, presented the
// way Git presents one: HTTP Basic, any username, the `arkc_` credential as
// the password — or a Bearer header, for a client configured with
// `http.extraHeader`. A missing or refused credential is a 401 carrying
// `WWW-Authenticate: Basic`, which is what makes git consult its credential
// helper or prompt. The grant is the repository's own (grants.go): `read`
// fetches, `write` pushes. A repository the service has not registered is a
// 404, and is never created here.

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

// Git services this transport runs, by the name the protocol uses.
const (
	serviceUploadPack  = "git-upload-pack"
	serviceReceivePack = "git-receive-pack"
)

// receivePackTimeout bounds one push. It is deliberately not the request's
// own context: a receive-pack killed half-way through its ref transaction
// leaves `.lock` files that refuse every later push until somebody removes
// them by hand. A client that goes away is noticed through the pipes instead
// — receive-pack reads EOF or writes into a closed pipe and exits the way Git
// exits, cleaning up after itself.
const receivePackTimeout = time.Hour

// gitRepoState serializes the pushes into one bare repository, which is what
// lets the service compare its refs before and after a receive-pack and know
// exactly which updates that push applied.
type gitRepoState struct {
	push sync.Mutex
}

// registerGit adds the smart HTTP routes. Called from Handler, and inert
// beyond answering 404 when the service has no GitDataDir.
func (s *Server) registerGit(mux *http.ServeMux) {
	mux.HandleFunc("GET /git/{repo}/info/refs", s.handleGitInfoRefs)
	mux.HandleFunc("POST /git/{repo}/git-upload-pack", func(w http.ResponseWriter, r *http.Request) {
		s.serveGitRPC(w, r, serviceUploadPack)
	})
	mux.HandleFunc("POST /git/{repo}/git-receive-pack", func(w http.ResponseWriter, r *http.Request) {
		s.serveGitRPC(w, r, serviceReceivePack)
	})
}

// gitLevel is the grant a Git service needs.
func gitLevel(service string) string {
	if service == serviceReceivePack {
		return api.GrantWrite
	}
	return api.GrantRead
}

// gitErr answers a Git client in plain text. Git prints the status line and
// not the body, but a person debugging with curl reads the body.
func gitErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	w.WriteHeader(status)
	fmt.Fprintln(w, msg)
}

// gitChallenge is the 401 that makes a Git client authenticate. Without the
// header git reports "Authentication failed" and never asks its credential
// helper, which is why the JSON routes' 401 cannot be reused here.
func gitChallenge(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", `Basic realm="ark"`)
	gitErr(w, http.StatusUnauthorized, msg+" — present an Ark credential (`ark login`) as the password")
}

// gitCredential takes the credential out of a Git client's request: the
// Basic password, or the username when the password is empty (some tools put
// a token there), or a Bearer header.
func gitCredential(r *http.Request) string {
	if user, pass, ok := r.BasicAuth(); ok {
		if pass == "" {
			return user
		}
		return pass
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// gitRepoID is the repository a Git route names, with the ".git" a clone URL
// usually carries removed. "" when it is not a ULID.
func gitRepoID(r *http.Request) string {
	id := strings.TrimSuffix(r.PathValue("repo"), ".git")
	if !records.ValidID(id) {
		return ""
	}
	return id
}

// gitRequest is everything a Git route decides before Git runs, in the order
// that leaks nothing: who is asking (401), whether the repository exists
// (404), whether they may (403). The existence check has to be explicit:
// authorize() deliberately lets a request about an unregistered repository
// through (it is how `ark login` verifies a credential), and here that would
// mean any principal could create a bare repository under any ULID by pushing
// to it. It writes its own refusal and returns the request carrying the
// principal, the repository id and the bare repository's directory.
func (s *Server) gitRequest(w http.ResponseWriter, r *http.Request, service string) (*http.Request, string, string, bool) {
	if s.GitDataDir == "" {
		gitErr(w, http.StatusNotFound, "this service does not host Git repositories")
		return nil, "", "", false
	}
	tok := gitCredential(r)
	if tok == "" {
		gitChallenge(w, "authentication required")
		return nil, "", "", false
	}
	who, err := s.authenticateToken(r.Context(), tok)
	if err != nil {
		if !isAuthRejection(err) {
			s.logError("authenticate", err)
			gitErr(w, http.StatusInternalServerError, "authenticate failed")
			return nil, "", "", false
		}
		gitChallenge(w, rejectionMessage(err))
		return nil, "", "", false
	}
	if s.Log != nil {
		s.Log.Info("authenticated", "principal", who.ID, "kind", who.Kind,
			"method", r.Method, "path", r.URL.Path)
	}
	r = r.WithContext(withPrincipal(r.Context(), who))

	repoID := gitRepoID(r)
	if repoID == "" {
		gitErr(w, http.StatusNotFound, "not a repository: Git is served at /git/<repository ULID>")
		return nil, "", "", false
	}
	var defaultBranch string
	err = s.Repos.View(r.Context(), repoID, func(db *sql.DB) error {
		return db.QueryRowContext(r.Context(), `SELECT default_branch FROM meta WHERE id = 1`).Scan(&defaultBranch)
	})
	if errors.Is(err, repodb.ErrNotFound) {
		gitErr(w, http.StatusNotFound, "repository "+repoID+" is not registered with this service")
		return nil, "", "", false
	}
	if err != nil {
		s.logError("load repository for git", err)
		gitErr(w, http.StatusInternalServerError, "load repository failed")
		return nil, "", "", false
	}
	if fault := s.authorize(r, repoID, gitLevel(service)); fault != nil {
		if fault.code == "internal" {
			s.logError("authorize", errors.New(fault.msg))
			gitErr(w, http.StatusInternalServerError, "authorize failed")
			return nil, "", "", false
		}
		gitErr(w, fault.status, fault.msg)
		return nil, "", "", false
	}
	dir, err := s.ensureBareRepo(r.Context(), repoID, defaultBranch)
	if err != nil {
		s.logError("open bare repository", err)
		gitErr(w, http.StatusServiceUnavailable, "the repository's Git storage is unavailable; retry")
		return nil, "", "", false
	}
	return r, repoID, dir, true
}

func (s *Server) logError(what string, err error) {
	if s.Log != nil {
		s.Log.Error(what, "error", err)
	}
}

// handleGitInfoRefs is the ref advertisement that begins every fetch and
// push.
func (s *Server) handleGitInfoRefs(w http.ResponseWriter, r *http.Request) {
	service := r.URL.Query().Get("service")
	if service != serviceUploadPack && service != serviceReceivePack {
		// A client that asks without a service is speaking dumb HTTP, which
		// would mean serving the repository's files directly. Not served.
		gitErr(w, http.StatusForbidden, "only smart HTTP is served: use a Git client that sends ?service=")
		return
	}
	r, repoID, dir, ok := s.gitRequest(w, r, service)
	if !ok {
		return
	}
	// The Git-Protocol header is deliberately not passed on, so every client
	// speaks protocol v0/v1, which both services fall back to cleanly. v2 is
	// a second protocol to get right and to test, for nothing a single
	// repository host needs.
	cmd := s.gitCommand(r.Context(), dir, strings.TrimPrefix(service, "git-"),
		"--stateless-rpc", "--advertise-refs", ".")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		s.logGitFailure(repoID, service, err, stderr.String())
		gitErr(w, http.StatusInternalServerError, "advertise refs failed")
		return
	}
	w.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	io.WriteString(w, pktLine("# service="+service+"\n"))
	io.WriteString(w, "0000")
	w.Write(out)
}

// pktLine frames one line of the Git protocol.
func pktLine(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }

// serveGitRPC runs one upload-pack or receive-pack exchange.
func (s *Server) serveGitRPC(w http.ResponseWriter, r *http.Request, service string) {
	r, repoID, dir, ok := s.gitRequest(w, r, service)
	if !ok {
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-"+service+"-request" {
		gitErr(w, http.StatusUnsupportedMediaType, "expected Content-Type application/x-"+service+"-request")
		return
	}
	var body io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip", "x-gzip":
		// Git compresses a large request body — a long negotiation, or a
		// push — and says so here rather than in Transfer-Encoding.
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			gitErr(w, http.StatusBadRequest, "request body is not valid gzip")
			return
		}
		defer zr.Close()
		body = zr
	default:
		gitErr(w, http.StatusUnsupportedMediaType, "unsupported Content-Encoding")
		return
	}
	// Go's HTTP/1 server discards whatever is left of the request body the
	// moment a handler first writes its response. Git's services may write
	// (progress, a NAK) before they have read everything, so the body has to
	// stay readable while the response streams. It is a no-op error on
	// HTTP/2, where the two directions are independent already.
	rc := http.NewResponseController(w)
	_ = rc.EnableFullDuplex()

	ctx := r.Context()
	var state *gitRepoState
	var before map[string]string
	if service == serviceReceivePack {
		state = s.gitState(repoID)
		state.push.Lock()
		defer state.push.Unlock()
		var err error
		if before, err = s.gitRefs(ctx, dir); err != nil {
			s.logError("read refs before push", err)
			gitErr(w, http.StatusInternalServerError, "read refs failed")
			return
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), receivePackTimeout)
		defer cancel()
	}

	w.Header().Set("Content-Type", "application/x-"+service+"-result")
	w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
	out := &streamWriter{w: w, rc: rc}
	var stderr boundedBuffer
	cmd := s.gitCommand(ctx, dir, strings.TrimPrefix(service, "git-"), "--stateless-rpc", ".")
	cmd.Stdin = body
	cmd.Stdout = out
	cmd.Stderr = &stderr
	started := time.Now()
	err := cmd.Run()
	if err != nil {
		s.logGitFailure(repoID, service, err, stderr.String())
		if !out.wrote {
			gitErr(w, http.StatusInternalServerError, strings.TrimPrefix(service, "git-")+" failed")
		}
		// Whatever it wrote is the client's answer — Git reports its own
		// failures in-band — and a receive-pack that failed may still have
		// applied some of its updates, so the refs are compared regardless.
	}
	if service != serviceReceivePack {
		return
	}
	after, rerr := s.gitRefs(context.WithoutCancel(ctx), dir)
	if rerr != nil {
		s.logError("read refs after push", rerr)
		return
	}
	updates := diffRefs(before, after)
	if s.Log != nil {
		s.Log.Info("git push", "repository_id", repoID, "principal", principalOf(r).ID,
			"updated_refs", len(updates), "duration_ms", time.Since(started).Milliseconds(),
			"ok", err == nil)
	}
	if len(updates) > 0 {
		s.afterPush(repoID, dir, principalOf(r), updates)
	}
}

// streamWriter flushes after every write, so a client sees progress and the
// pack as Git produces them rather than when the buffer fills, and records
// whether anything reached the client — after which the status is fixed.
type streamWriter struct {
	w     http.ResponseWriter
	rc    *http.ResponseController
	wrote bool
}

func (s *streamWriter) Write(p []byte) (int, error) {
	s.wrote = true
	n, err := s.w.Write(p)
	if err == nil {
		_ = s.rc.Flush()
	}
	return n, err
}

// boundedBuffer keeps the first 64 KiB of a Git process's stderr, for the
// log line a failure earns.
type boundedBuffer struct{ bytes.Buffer }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := 64<<10 - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

func (s *Server) logGitFailure(repoID, service string, err error, stderr string) {
	if s.Log != nil {
		s.Log.Warn("git service failed", "repository_id", repoID, "service", service,
			"error", err, "stderr", strings.TrimSpace(stderr))
	}
}

// refUpdate is one ref a push changed. A zero object id on either side is
// Git's own notation for "did not exist": a created ref has a zero Before, a
// deleted one a zero After.
type refUpdate struct {
	Ref    string
	Before string
	After  string
}

// gitRefs reads every ref in a bare repository.
func (s *Server) gitRefs(ctx context.Context, dir string) (map[string]string, error) {
	cmd := s.gitCommand(ctx, dir, "for-each-ref", "--format=%(objectname) %(refname)")
	out, err := cmd.Output()
	if err != nil {
		return nil, gitError("for-each-ref", err)
	}
	refs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if sha, ref, ok := strings.Cut(line, " "); ok {
			refs[ref] = sha
		}
	}
	return refs, nil
}

// diffRefs is what changed between two ref listings, in ref order. It is the
// whole of how the service learns what a push did: comparing the repository
// before and after, under the push lock, reports the updates Git actually
// applied — rejected ones excluded, deletions included — which parsing the
// client's commands would not.
func diffRefs(before, after map[string]string) []refUpdate {
	var out []refUpdate
	zeroLike := func(sha string) string { return strings.Repeat("0", len(sha)) }
	for ref, b := range before {
		a, ok := after[ref]
		switch {
		case !ok:
			out = append(out, refUpdate{Ref: ref, Before: b, After: zeroLike(b)})
		case a != b:
			out = append(out, refUpdate{Ref: ref, Before: b, After: a})
		}
	}
	for ref, a := range after {
		if _, ok := before[ref]; !ok {
			out = append(out, refUpdate{Ref: ref, Before: zeroLike(a), After: a})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out
}

// isZeroID reports Git's "no object" id.
func isZeroID(sha string) bool { return strings.Trim(sha, "0") == "" }

// gitState is the per-repository lock set, created on first use.
func (s *Server) gitState(repoID string) *gitRepoState {
	s.gitMu.Lock()
	defer s.gitMu.Unlock()
	if s.gitRepos == nil {
		s.gitRepos = map[string]*gitRepoState{}
	}
	st, ok := s.gitRepos[repoID]
	if !ok {
		st = &gitRepoState{}
		s.gitRepos[repoID] = st
	}
	return st
}

// bareRepoDir is where a repository's bare Git repository lives.
func (s *Server) bareRepoDir(repoID string) string {
	return filepath.Join(s.GitDataDir, repoID+".git")
}

// ensureBareRepo returns the bare repository for a registered Ark
// repository, creating it the first time anything touches it. Creating on a
// fetch as well as a push is deliberate: a clone of a repository nobody has
// pushed to yet is an empty clone, which Git reports in its own words, and
// not a 404 that reads as "this repository does not exist".
//
// HEAD is pointed at the repository's default branch, or a clone of a
// `main`-only repository would check out nothing.
func (s *Server) ensureBareRepo(ctx context.Context, repoID, defaultBranch string) (string, error) {
	dir := s.bareRepoDir(repoID)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	s.gitInitMu.Lock()
	defer s.gitInitMu.Unlock()
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(s.GitDataDir, 0o755); err != nil {
		return "", err
	}
	// Built beside its final name and renamed into place, so a crash half way
	// through leaves a directory nothing serves rather than a repository with
	// no HEAD.
	tmp, err := os.MkdirTemp(s.GitDataDir, repoID+".init-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	if out, err := s.gitCommand(ctx, tmp, "init", "--bare", "--quiet", ".").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git init --bare: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if out, err := s.gitCommand(ctx, tmp, "symbolic-ref", "HEAD", "refs/heads/"+defaultBranch).CombinedOutput(); err != nil {
		// A branch name Git refuses is the repository's metadata to fix
		// (`ark repo set --default-branch`), not a reason to refuse a push.
		s.logError("point HEAD at the default branch", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out))))
	}
	// The reflog is Git's own record of every ref change, off by default in
	// a bare repository. The service is the record of this code; a force push
	// should leave a trace of what it replaced.
	if out, err := s.gitCommand(ctx, tmp, "config", "core.logAllRefUpdates", "always").CombinedOutput(); err != nil {
		return "", fmt.Errorf("git config: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	if s.Log != nil {
		s.Log.Info("created bare repository", "repository_id", repoID, "default_branch", defaultBranch)
	}
	return dir, nil
}

// gitCommand builds a Git invocation the way spec §11 says Git is run:
// exec.CommandContext, an explicit working directory, machine-readable
// arguments.
//
// The environment is built rather than inherited. The service's own
// environment holds its secrets (every ARK_* value) and possibly a GIT_DIR or
// GIT_WORK_TREE from whatever started it; neither belongs in a Git process.
// System and global Git configuration are switched off too, so a credential
// helper or url.insteadOf on the host cannot change what the service does —
// GIT_CONFIG_GLOBAL points at an empty file the service owns rather than at
// /dev/null, which Windows does not have.
func (s *Server) gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = s.gitEnv()
	// SIGTERM rather than SIGKILL when a context ends: Git removes its lock
	// files on a signal it can catch, and a killed one leaves them behind.
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

func (s *Server) gitEnv() []string {
	s.gitEnvOnce.Do(func() {
		var env []string
		for _, kv := range os.Environ() {
			name, _, _ := strings.Cut(kv, "=")
			upper := strings.ToUpper(name)
			if strings.HasPrefix(upper, "ARK_") || strings.HasPrefix(upper, "GIT_") {
				continue
			}
			env = append(env, kv)
		}
		global := filepath.Join(s.GitDataDir, ".gitconfig-empty")
		if err := os.MkdirAll(s.GitDataDir, 0o755); err == nil {
			if f, err := os.OpenFile(global, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				f.Close()
			}
		}
		env = append(env,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_CONFIG_GLOBAL="+global,
			"GIT_TERMINAL_PROMPT=0",
		)
		s.gitEnvBase = env
	})
	return append([]string(nil), s.gitEnvBase...)
}

// gitError wraps a failed Git invocation with its stderr, which exec puts on
// an ExitError when the caller collected stdout with Output.
func gitError(what string, err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		return fmt.Errorf("git %s: %w: %s", what, err, strings.TrimSpace(string(exit.Stderr)))
	}
	return fmt.Errorf("git %s: %w", what, err)
}

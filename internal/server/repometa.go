package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	"github.com/elk-work/ark/pkg/api"
)

// Repository metadata: reading the service's copy of a repository's name,
// default branch and Git remote, and correcting it.
//
// Registration cannot correct any of them, and must not. `handleRegisterRepo`
// runs on every sync with a name that is only ever the basename of wherever
// the client happens to be checked out, so overwriting there let any client
// rename the repository for everyone and blank the remote of a scratch
// checkout — both observed live, both fixed by making registration
// backfill-only. That left the fields writable exactly once, at creation, and
// wrong forever after (elk-work/ark#11).
//
// So correction is its own deliberate act, addressed by repository ID rather
// than inferred from a working directory — the inference is what caused the
// original bug. What it borrows from the RFC-0004 write routes is everything
// else: the fault and no-op machinery in write.go, an Idempotency-Key, the
// writer check, and a revision minted only when something actually changes.
//
// Metadata is not a record. It has no created_by, nothing pulls it, and it
// lives in the single-row meta table rather than in `records` — so there is
// no document to write and no field_revisions to stamp. The revision bump is
// still owed: it is the repository's monotonic clock, every client reads it
// on the next pull, and a change nothing ordered would be a side-channel
// write of exactly the kind this route exists to replace.

const (
	maxRepoName  = 200
	maxBranchLen = 255
	maxRemoteLen = 2048
)

// handleGetRepository serves the service's copy of the repository metadata.
// Without it there is no way to see the values this route corrects: they are
// not records, so no pull carries them, and `ark status` reports the local
// copy that `ark init` wrote.
func (s *Server) handleGetRepository(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	if !s.allow(w, r, repoID, api.GrantRead) {
		return
	}
	var meta api.RepositoryMetadata
	err := s.Repos.View(r.Context(), repoID, func(db *sql.DB) error {
		return loadMetadata(db.QueryRowContext(r.Context(), metadataQuery), &meta)
	})
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "repository not registered")
		return
	}
	if err != nil {
		s.respond(w, "load repository", err)
		return
	}
	writeJSON(w, meta)
}

const metadataQuery = `SELECT repository_id, name, default_branch, git_remote_url, revision, created_at
	FROM meta WHERE id = 1`

type scanner interface{ Scan(dest ...any) error }

func loadMetadata(row scanner, meta *api.RepositoryMetadata) error {
	return row.Scan(&meta.ID, &meta.Name, &meta.DefaultBranch,
		&meta.GitRemoteURL, &meta.Revision, &meta.CreatedAt)
}

// handleSetRepositoryMetadata corrects one or more metadata fields.
func (s *Server) handleSetRepositoryMetadata(w http.ResponseWriter, r *http.Request) {
	repoID := r.PathValue("repo")
	// `admin`, and this is the clearest admin-level act on the service:
	// renaming a repository changes what it is called for everyone, and
	// nothing about it is recoverable from a client. The check is here rather
	// than beside the writer resolution below because it needs no repository
	// database — a caller with no authority should not cost a fetch.
	if !s.allow(w, r, repoID, api.GrantAdmin) {
		return
	}
	req, ok := decode[api.SetRepositoryMetadataRequest](w, r)
	if !ok {
		return
	}
	// A key is honoured but not required, for the reason the status route
	// gives: this is an assertion about state, not an increment, so a repeat
	// is answerable from the stored metadata itself.
	key := idempotencyKey(r)
	if len(key) > maxIdempotencyKey {
		writeErr(w, http.StatusBadRequest, "validation", "Idempotency-Key is too long")
		return
	}
	if fault := validateMetadata(req); fault != nil {
		writeErr(w, fault.status, fault.code, fault.msg)
		return
	}

	ctx := r.Context()
	var resp api.RepositoryResponse
	err := s.Repos.Update(ctx, repoID, false, func(tx *sql.Tx) error {
		// The closure reruns on a lost CAS; reset the accumulator so a retry
		// cannot report a revision the committed database never had.
		resp = api.RepositoryResponse{}
		if err := replayedResponse(ctx, tx, key, http.StatusOK, &api.RepositoryResponse{}); err != nil {
			return err
		}
		var meta api.RepositoryMetadata
		err := loadMetadata(tx.QueryRowContext(ctx, metadataQuery), &meta)
		if errors.Is(err, sql.ErrNoRows) {
			// The database exists but carries no meta row, which registration
			// always writes. Not a repository this service knows about.
			return faultNotFound("repository " + repoID + " is not registered")
		}
		if err != nil {
			return err
		}

		next := meta
		if req.Name != nil {
			next.Name = strings.TrimSpace(*req.Name)
		}
		if req.DefaultBranch != nil {
			next.DefaultBranch = strings.TrimSpace(*req.DefaultBranch)
		}
		if req.GitRemoteURL != nil {
			next.GitRemoteURL = strings.TrimSpace(*req.GitRemoteURL)
		}
		if next == meta {
			// Setting a field to what it already holds is a correct answer,
			// not a write. Minting a revision for it would make every client
			// pull an empty change set — applyDelete's reasoning, and the
			// status route's.
			return unchanged{status: http.StatusOK, resp: api.RepositoryResponse{
				Repository: meta, ServerRevision: meta.Revision}}
		}

		// Resolved for the authorization rule rather than for attribution:
		// metadata carries no created_by. Running the same writer check on
		// every write route keeps the rule in one place. The per-repository
		// grant this act needs — `admin` — was checked before the handler
		// touched storage; this is the second half of the same rule, about
		// the identity the write is made under rather than the caller.
		if _, err := resolveWriter(ctx, tx, req.Writer, principalOf(r)); err != nil {
			return err
		}
		rev, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE meta
			SET name = ?, default_branch = ?, git_remote_url = ? WHERE id = 1`,
			next.Name, next.DefaultBranch, next.GitRemoteURL); err != nil {
			return err
		}
		next.Revision = rev
		resp = api.RepositoryResponse{Repository: next, Changed: true, ServerRevision: rev}
		return rememberResponse(ctx, tx, key, resp, rev)
	})
	s.finish(w, "set repository metadata", err, resp, http.StatusOK)
}

// validateMetadata rejects a request that asserts nothing, and any value that
// would leave the repository harder to identify than the wrong one it
// replaces. Checks run before storage is touched so a bad request costs no
// fetch of the repository database.
func validateMetadata(req api.SetRepositoryMetadataRequest) *writeFault {
	if req.Name == nil && req.DefaultBranch == nil && req.GitRemoteURL == nil {
		return faultValidation("at least one of name, default_branch, git_remote_url is required")
	}
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return faultValidation("name cannot be empty")
		}
		if len(name) > maxRepoName {
			return faultValidation("name is too long")
		}
		if hasControl(name) {
			return faultValidation("name cannot contain control characters")
		}
	}
	if req.DefaultBranch != nil {
		if fault := validBranchName(strings.TrimSpace(*req.DefaultBranch)); fault != nil {
			return fault
		}
	}
	if req.GitRemoteURL != nil {
		if fault := plausibleRemote(strings.TrimSpace(*req.GitRemoteURL)); fault != nil {
			return fault
		}
	}
	return nil
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) })
}

// validBranchName applies the parts of git-check-ref-format that a branch
// name must satisfy. Ark shells out to the Git CLI for everything real, so
// this is the cheap client-independent half: it stops a value Git would
// refuse from being stored as the branch every clone is told to use.
func validBranchName(branch string) *writeFault {
	reject := func(why string) *writeFault {
		return faultValidation("default_branch " + branch + " is not a valid branch name: " + why)
	}
	switch {
	case branch == "":
		return faultValidation("default_branch cannot be empty")
	case len(branch) > maxBranchLen:
		return reject("too long")
	case hasControl(branch), strings.ContainsAny(branch, " ~^:?*[\\"):
		return reject("Git refuses space, control characters, and ~^:?*[\\")
	case strings.Contains(branch, ".."), strings.Contains(branch, "@{"), branch == "@":
		return reject(`Git refuses "..", "@{" and a bare "@"`)
	case strings.HasPrefix(branch, "/"), strings.HasSuffix(branch, "/"), strings.Contains(branch, "//"):
		return reject("a path component cannot be empty")
	case strings.HasPrefix(branch, "-"), strings.HasPrefix(branch, "."):
		return reject("cannot begin with a dash or a dot")
	case strings.HasSuffix(branch, "."), strings.HasSuffix(branch, ".lock"):
		return reject(`cannot end with "." or ".lock"`)
	}
	return nil
}

// plausibleRemote reports whether a value looks like something `git clone`
// could take. It is a shape check and deliberately not a reachability one:
// the service cannot reach a customer's Git host, and a remote that is
// unreachable today is not thereby wrong. An empty value is accepted and
// clears the field — a repository can genuinely have no remote, and the
// alternative is a wrong non-empty URL nothing can remove.
func plausibleRemote(remote string) *writeFault {
	reject := func(why string) *writeFault {
		return faultValidation("git_remote_url " + remote + " does not look like a Git remote: " + why)
	}
	if remote == "" {
		return nil
	}
	if len(remote) > maxRemoteLen {
		return reject("too long")
	}
	if hasControl(remote) || strings.ContainsFunc(remote, unicode.IsSpace) {
		return reject("it contains whitespace or control characters")
	}
	if u, err := url.Parse(remote); err == nil && u.Scheme != "" {
		switch u.Scheme {
		case "http", "https", "ssh", "git":
			if u.Host == "" {
				return reject("no host")
			}
			return nil
		case "file":
			if u.Path == "" {
				return reject("no path")
			}
			return nil
		default:
			return reject("unsupported scheme " + u.Scheme)
		}
	}
	// scp-like syntax, which is what most `git remote -v` output holds:
	// [user@]host:path, with no scheme and no slash before the colon.
	if host, path, ok := strings.Cut(remote, ":"); ok {
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		if host != "" && path != "" && !strings.Contains(host, "/") {
			return nil
		}
	}
	// A local clone source is a remote too.
	if strings.HasPrefix(remote, "/") || strings.HasPrefix(remote, "./") || strings.HasPrefix(remote, "../") {
		return nil
	}
	return reject("expected a URL, [user@]host:path, or an absolute path")
}

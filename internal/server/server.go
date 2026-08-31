package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

// Server is the Ark sync service.
type Server struct {
	Repos *repodb.Manager
	Token string // single service token (spec §20: V1 begins with one)
	// LegacyMode is ARK_LEGACY_TOKEN: what the service token above may still
	// do. Empty means `full` — the behaviour every deployment configured
	// before this field existed is running — and the other two positions are
	// `readonly` and `off`. It is the dial RFC-0003 Stage 3 narrows the token
	// with (elk-work/ark#54); see legacy.go, which owns every decision it
	// makes.
	LegacyMode string
	// SigningKey signs local-mode blob URLs. Empty falls back to Token,
	// which is what every deployment configured before ARK_SIGNING_KEY
	// existed relies on. See signingKey.
	SigningKey string
	// BootstrapToken mints the first principal on POST /v1/principals and is
	// accepted on no other route (RFC-0003 Decision 6). Empty disables that
	// route entirely, which is every deployment that has not opted in.
	BootstrapToken string
	// IDPApprovalURL is where `ark login` sends a person to approve a device
	// code, and IDPKey is the shared secret the identity provider presents on
	// POST /v1/device/approve. Empty IDPApprovalURL means this service offers
	// no device login at all, which GET / reports and `ark login` reads. See
	// device.go.
	IDPApprovalURL string
	IDPKey         string
	// DefaultGrant is ARK_DEFAULT_GRANT: what a principal holds on a
	// repository nobody has granted it. Empty means the default, `seeded` —
	// grants arrive from the identity provider at approval (device.go) and
	// nowhere else, so a service with no identity provider seeds nothing and
	// is deny. See grants.go.
	DefaultGrant string
	Blobs        BlobStore
	Log          *slog.Logger
	Version      string // build stamp, reported unauthenticated on GET /

	// auths is the credential store, opened on first use. See authStore.
	authOnce sync.Once
	auths    *authStore

	// devices holds pending device codes, over the same auth.db. See
	// device.go.
	deviceOnce sync.Once
	devices    *deviceStore
}

// signingKey is the HMAC key for local-mode blob URLs: ARK_SIGNING_KEY when
// the deployment sets one, otherwise the service token.
//
// The fallback is not a convenience. The service token has been the signing
// key since local mode existed, so every deployment and every already-issued
// URL assumes it; making the key mandatory would break them all on upgrade.
// It is also why the key needs a name of its own *before* RFC-0003 retires the
// service token as a bearer (elk-work/ark#54): the day ARK_API_TOKEN goes
// away, a signing key with no home takes local-mode artifact URLs with it, and
// a signature that no longer verifies looks like a bad URL rather than like a
// missing setting.
func (s *Server) signingKey() string {
	if s.SigningKey != "" {
		return s.SigningKey
	}
	return s.Token
}

// Handler builds the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleRoot)
	// Not /healthz: Google Frontend intercepts that path on run.app
	// hostnames and serves its own 404 before the request reaches the
	// container (verified empirically 2026-07-13).
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	// Bootstrap: the one route authenticated by ARK_BOOTSTRAP_TOKEN rather
	// than by a bearer, so a deployment can mint its first credential without
	// already holding one. It also takes an operator's own credential, which
	// is how operators are made after the first. See auth.go.
	mux.HandleFunc("POST /v1/principals", s.handleCreatePrincipal)
	// The two service-wide acts (elk-work/ark#94, D116). Listing principals
	// is operator-only; revoking is an operator, or the holder retiring their
	// own credential. `GET /v1/credentials` is the self-service half of the
	// second: a credential is revoked by an id, and this is where its holder
	// finds it. See operators.go.
	mux.HandleFunc("GET /v1/principals", s.auth(s.handleListPrincipals))
	mux.HandleFunc("GET /v1/credentials", s.auth(s.handleListCredentials))
	mux.HandleFunc("POST /v1/credentials/{id}/revoke", s.auth(s.handleRevokeCredential))
	// Device login (RFC-0003 Decision 3, spec §20.1). The first two are
	// unauthenticated because the caller holds nothing to authenticate with
	// yet; the third has its own key, ARK_IDP_KEY. See device.go.
	mux.HandleFunc("POST /v1/device/code", s.handleDeviceCode)
	mux.HandleFunc("POST /v1/device/token", s.handleDeviceToken)
	mux.HandleFunc("POST /v1/device/approve", s.handleDeviceApprove)
	mux.HandleFunc("POST /v1/repositories", s.auth(s.handleRegisterRepo))
	mux.HandleFunc("POST /v1/sync/push", s.auth(s.handlePush))
	mux.HandleFunc("POST /v1/sync/pull", s.auth(s.handlePull))
	mux.HandleFunc("GET /v1/repositories/{repo}/records/{type}/{id}", s.auth(s.handleGetRecord))
	// Repository metadata: readable, and correctable after registration —
	// which registration itself deliberately cannot do. See repometa.go.
	mux.HandleFunc("GET /v1/repositories/{repo}", s.auth(s.handleGetRepository))
	mux.HandleFunc("POST /v1/repositories/{repo}/metadata", s.auth(s.handleSetRepositoryMetadata))
	// Per-repository grants: who may read, write, or administer this
	// repository (RFC-0003 Decision 4). Both are `admin` acts. See grants.go.
	mux.HandleFunc("GET /v1/repositories/{repo}/grants", s.auth(s.handleListGrants))
	mux.HandleFunc("POST /v1/repositories/{repo}/grants", s.auth(s.handleSetGrant))
	// The ledger of references this service accepted while holding nothing
	// at the other end (§9.1). A `read` act: it names record ids in this
	// repository and nothing else. See dangling.go.
	mux.HandleFunc("GET /v1/repositories/{repo}/dangling", s.auth(s.handleDangling))
	// Work-record write routes (docs/rfc-0004-work-record-write-api.md):
	// what a program uses instead of speaking the mutation protocol.
	mux.HandleFunc("POST /v1/repositories/{repo}/tasks", s.auth(s.handleCreateTask))
	mux.HandleFunc("POST /v1/repositories/{repo}/comments", s.auth(s.handleCreateComment))
	mux.HandleFunc("POST /v1/repositories/{repo}/tasks/{id}/status", s.auth(s.handleTaskStatus))
	mux.HandleFunc("POST /v1/pull-requests/{id}/merge", s.auth(s.handleMerge))
	mux.HandleFunc("POST /v1/artifacts/upload-url", s.auth(s.handleUploadURL))
	mux.HandleFunc("POST /v1/artifacts/confirm", s.auth(s.handleConfirmUpload))
	mux.HandleFunc("POST /v1/artifacts/download-url", s.auth(s.handleDownloadURL))
	if local, ok := s.Blobs.(*LocalBlobStore); ok {
		// These routes carry their own signature rather than the bearer
		// token, because clients treat them as pre-signed URLs and send no
		// Authorization header. The key is ARK_SIGNING_KEY, defaulting to the
		// service token — so there is still nothing to configure and still no
		// way to leave the routes open, but the key now has a name that
		// outlives the service token being a bearer.
		local.Secret = s.signingKey()
		mux.Handle("GET /blobs/", local.Handler())
		mux.Handle("PUT /blobs/", local.Handler())
	}
	return mux
}

// handleRoot is the unauthenticated service banner.
//
// It carries an `auth` object, and that object is the whole of how a client
// discovers how to log in: `ark login` reads it and either runs the device
// flow or says the service has none and asks for a token. There is no client
// configuration behind it, which is the point — a person who can reach the
// service can find out how to authenticate to it.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
		return
	}
	banner := api.ServiceBanner{
		Service: "ark-sync",
		API:     "v1",
		Version: s.Version,
		Auth:    &api.ServiceAuth{DeviceFlow: s.deviceFlowEnabled()},
	}
	if banner.Auth.DeviceFlow {
		banner.Auth.ApprovalURL = s.IDPApprovalURL
	}
	writeJSON(w, banner)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(api.Error{Code: code, Message: msg})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func decode[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&v); err != nil {
		writeErr(w, http.StatusBadRequest, "validation", "invalid JSON body: "+err.Error())
		return v, false
	}
	return v, true
}

// respond maps repodb errors onto the API error contract.
func (s *Server) respond(w http.ResponseWriter, what string, err error) {
	var corrupt *repodb.CorruptError
	switch {
	case err == nil:
	case errors.Is(err, repodb.ErrNotFound):
		// One status and one code for both shapes of the loss, because the
		// client's answer is the same: the object is absent, or it is there
		// and holds no repository row, which is what a zero-length
		// `repos/<id>.db` reads as. The message says which, because the
		// operator's next move is not the same — one object was removed, the
		// other had zero bytes written over it (elk-work/ark#85).
		msg := "repository not registered"
		if errors.Is(err, repodb.ErrNoRepositoryRow) {
			if s.Log != nil {
				s.Log.Warn(what, "error", err)
			}
			msg += ": a stored database is present for it and holds no repository, which is what a zero-length repos/<id>.db reads as — restore it from a copy"
		}
		writeErr(w, http.StatusNotFound, "not_found", msg)
	case errors.Is(err, repodb.ErrConcurrentWrite):
		writeErr(w, http.StatusConflict, "conflict", "repository is being updated concurrently; retry")
	case errors.As(err, &corrupt):
		// Still a 500 — the request was fine and the service cannot serve it —
		// but not an anonymous one. Every other 5xx here is a reason to try
		// again; this one will hold until an operator restores the stored
		// database, and saying "pull failed" left that fact in the service's
		// logs and nowhere else (elk-work/ark#65). The message names the
		// repository and what is wrong with its stored copy, which is the
		// operator's own storage and not a secret; the client keys on the code
		// beside it, because the status cannot carry the distinction.
		if s.Log != nil {
			s.Log.Error(what, "error", err)
		}
		writeErr(w, http.StatusInternalServerError, api.ErrorCodeRepositoryCorrupt,
			corrupt.Error()+" — restore this repository's stored database from a copy; retrying will not clear it")
	default:
		s.internal(w, what, err)
	}
}

func (s *Server) handleRegisterRepo(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.RegisterRepositoryRequest](w, r)
	if !ok {
		return
	}
	if req.ID == "" || req.Name == "" {
		writeErr(w, http.StatusBadRequest, "validation", "id and name are required")
		return
	}
	// A repository id becomes an object name in storage the service owns, and
	// registration is the only route that mints one. `repodb.validRepoID` runs
	// on every read and write, but it is a path-safety check — it rejects the
	// separators and the dot, and accepts "spooky" or 600 characters of
	// anything else. Identity is a different question and belongs here, once,
	// where the name is chosen: clients mint ULIDs (`records.NewID`), so the
	// service can require one rather than inherit whatever a caller sends
	// (elk-work/ark#84).
	//
	// This also makes auth.db's collision-immunity explicit. It lives at the
	// reserved key `ark.auth`, which no client can register because the dot is
	// rejected — correct, but a stronger guarantee than it looked like, resting
	// on a rule written for path safety. A ULID cannot contain a dot either,
	// so the reservation now holds for a reason you can read.
	if !records.ValidID(req.ID) {
		writeErr(w, http.StatusBadRequest, "validation", "id must be a ULID")
		return
	}
	if req.DefaultBranch == "" {
		req.DefaultBranch = "main"
	}
	ctx := r.Context()
	// Registration is how a repository comes into existence, and it is also
	// the one call every sync makes unconditionally — so it is the call that
	// can stand a lost repository back up empty, and until it learned the
	// client's cursor it could not tell the two apart. A client above
	// revision 0 is asserting it holds history this service issued; if the
	// database is missing, it was lost rather than never created, and making
	// a fresh one destroys the only clean signal of that. Pull and push both
	// answer 404 while a repository is absent, and both answer normally the
	// moment a client re-creates it, which is how a repository sat missing
	// for six weeks with every client reporting itself in sync
	// (elk-work/ark#66, elk-work/ark#58). So create only for a client at 0.
	//
	// The second shape of the same condition — a stored database with no
	// repository row in it, which is what a zero-length object reads as — used
	// to be caught here, by a sentinel that never escaped this handler. It
	// belongs to repodb now: the rule is about the repository rather than
	// about whether some file exists, and pull and push were falling through
	// to `500 internal` for it while this route said 404 (elk-work/ark#85). So
	// `create` carries the whole decision, and both shapes come back as
	// repodb.ErrNotFound.
	created := false
	// Registration is where authorization begins, so it is the one route that
	// cannot simply check a level and proceed: creating a repository is open
	// to any authenticated principal and *confers* admin on it
	// (first-writer-registers, RFC-0003 Decision 4), while registering against
	// one that already exists is the idempotent call every sync makes and
	// needs only `read` — a reader whose every sync began with a 403 could
	// never pull at all.
	existingFault := s.authorize(r, req.ID, api.GrantRead)
	// Which is also why this route is the one write ARK_LEGACY_TOKEN=readonly
	// has to refuse by hand: `read` is enough to be let through above, and
	// that is correct for the re-registration every pull begins with — the
	// handshake a narrowed legacy bearer must keep, or `readonly` would stop
	// pulls as well as pushes and be no different from `off`. Bringing a
	// repository into existence is not that call, so it is refused, and only
	// once the transaction has established that it is what is happening.
	legacyCreate := s.legacyReadonly(principalOf(r))
	err := s.Repos.Update(ctx, req.ID, req.LastRevision == 0, func(tx *sql.Tx) error {
		// The whole closure reruns on a lost CAS race; reset the accumulator
		// so a replay cannot report a creation the first attempt made.
		created = false
		var registered int
		if err := tx.QueryRow(`SELECT count(*) FROM meta WHERE id = 1`).Scan(&registered); err != nil {
			return err
		}
		created = registered == 0
		if !created && existingFault != nil {
			return existingFault
		}
		if created && legacyCreate {
			return legacyReadonlyCreate
		}

		// Registration runs on every sync, and the name a client sends is just
		// the basename of wherever it happens to be checked out. Overwriting on
		// conflict therefore let any client silently rename the repository for
		// everyone — observed: joining an existing repository from a directory
		// called "weirdly-named-dir" renamed it to that, and cleared the Git
		// remote because the scratch checkout had none.
		//
		// So registration only ever *backfills*: a value already on the server
		// wins, and a client can fill a field the server is missing. Renaming
		// is not something a sync should do — it is a deliberate act, and it
		// has its own route: POST /v1/repositories/{repo}/metadata, in
		// repometa.go, which is the only path that overwrites these fields.
		_, err := tx.Exec(`INSERT INTO meta (id, repository_id, name, default_branch, git_remote_url, created_at)
			VALUES (1, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				name           = CASE WHEN meta.name           = '' THEN excluded.name           ELSE meta.name           END,
				default_branch = CASE WHEN meta.default_branch = '' THEN excluded.default_branch ELSE meta.default_branch END,
				git_remote_url = CASE WHEN meta.git_remote_url = '' THEN excluded.git_remote_url ELSE meta.git_remote_url END`,
			req.ID, req.Name, req.DefaultBranch, req.GitRemoteURL, records.Now())
		if err != nil {
			return err
		}
		// Actors are upserted here as well as on push, because this is the
		// call every sync makes whether or not it has anything to send. A
		// repository that registered and never pushed used to hold no actor
		// records, which left every write route unable to resolve the writer
		// its own client would delegate from (elk-work/ark#47). upsertActor
		// mints a revision only for an actor it has not seen, so repeat
		// registrations stay quiet and pulls stay empty.
		//
		// They carry the same identity rules a push does — this is a real
		// introduction of an actor, and it is the one that runs
		// unconditionally, so exempting it would leave the rule enforced on
		// the surface a client can choose not to use.
		return introduceActors(ctx, tx, req.Actors, principalOf(r))
	})
	if errors.Is(err, repodb.ErrNotFound) {
		// not_found, and deliberately: it is the same answer pull and push
		// give for this exact condition, so all three routes say the same
		// thing about a repository that is gone, and the client lands on spec
		// §22's exit 3 rather than on a validation fault about a field nobody
		// typed. The message names what happened, because the operator reading
		// it has a missing database, not a typo — and it says which shape,
		// because a live object holding nothing and no object at all are the
		// same loss with different causes.
		if s.Log != nil {
			s.Log.Warn("refused to create a repository a client has already synced",
				"repository_id", req.ID, "name", req.Name,
				"client_last_revision", req.LastRevision,
				"stored_object_present", errors.Is(err, repodb.ErrNoRepositoryRow))
		}
		writeErr(w, http.StatusNotFound, "not_found", fmt.Sprintf(
			"this service has no database for repository %s, and this client has already synced it to revision %d — it is missing, not new. Registration will not stand an empty repository up in its place; restore it, or point this checkout at the service that holds it.",
			req.ID, req.LastRevision))
		return
	}
	if errors.Is(err, legacyReadonlyCreate) {
		// Logged here rather than in the closure above, which reruns on a lost
		// compare-and-swap and would count one refusal several times.
		s.logLegacyRefusal(r)
	}
	if err != nil {
		s.finish(w, "register repository", err, nil, http.StatusOK)
		return
	}
	if created {
		// First-writer-registers: the principal that brought the repository
		// into existence administers it, and everyone else has no access
		// until granted. It cannot ride in the transaction above — auth.db is
		// a different object under its own compare-and-swap — so a failure
		// here leaves a repository nobody administers. That is worth a 500
		// rather than a quiet log: the caller retries, and until then the
		// service token still carries implicit admin everywhere and can issue
		// the grant by hand (#54 removes that fallback and must replace it).
		if binder := binderOf(principalOf(r)); binder != "" {
			if err := s.authStore().grantOnCreate(ctx, req.ID, binder); err != nil {
				s.internal(w, "grant the repository's creator admin", err)
				return
			}
		}
	}
	if created && s.Log != nil {
		// The rare and interesting half of this route. Every other
		// registration is the idempotent no-op every sync of every repository
		// makes, and while the two were indistinguishable in the log a
		// repository could come back from the dead without leaving a trace of
		// having done so (elk-work/ark#66).
		s.Log.Info("registration created a repository",
			"repository_id", req.ID, "name", req.Name, "actors", len(req.Actors))
	}
	writeJSON(w, map[string]string{"id": req.ID})
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.PushRequest](w, r)
	if !ok {
		return
	}
	if !s.allow(w, r, req.RepositoryID, api.GrantWrite) {
		return
	}
	ctx := r.Context()
	who := principalOf(r)
	var resp api.PushResponse

	err := s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		// The whole fn reruns on a lost CAS race; reset accumulators so a
		// retry doesn't double-report outcomes.
		resp = api.PushResponse{
			Applied:   []api.MutationOutcome{},
			Rejected:  []api.MutationOutcome{},
			Conflicts: []api.MutationOutcome{},
		}
		// Actors are upserted as records so every client can render names,
		// and a new one is bound to the principal introducing it.
		if err := introduceActors(ctx, tx, req.Actors, who); err != nil {
			return err
		}
		// Then the identity every mutation claims. This runs before any of
		// them is applied, so a push written as somebody else's actor leaves
		// nothing behind — the fault rolls the whole transaction back.
		if err := authorizeMutations(ctx, tx, req.Mutations, who); err != nil {
			return err
		}
		// Mutations apply in creation order; savepoint handling inside
		// processMutation keeps one bad mutation from poisoning the batch.
		session := sessionRevisions{}
		for _, m := range req.Mutations {
			out := processMutation(ctx, tx, m, session)
			mo := api.MutationOutcome{MutationID: m.ID, Error: out.err,
				Remote: out.remote, ServerRevision: out.revision}
			switch out.status {
			case statusApplied:
				resp.Applied = append(resp.Applied, mo)
			case statusConflict:
				resp.Conflicts = append(resp.Conflicts, mo)
			default:
				resp.Rejected = append(resp.Rejected, mo)
			}
		}
		return tx.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).
			Scan(&resp.ServerRevision)
	})
	if err != nil {
		s.finish(w, "push", err, nil, http.StatusOK)
		return
	}
	writeJSON(w, resp)
}

// upsertActor stores an actor as a record, bumping the revision only for
// new actors so pulls stay quiet on repeat pushes. It reports whether the
// actor was new, which is what decides whether the identity rules in
// grantsactors.go have anything to judge: re-sending an actor the service
// already holds is a no-op, and must stay one.
func upsertActor(ctx context.Context, tx *sql.Tx, a api.Actor) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `SELECT true FROM records
		WHERE record_type = 'actor' AND record_id = ?`, a.ID).Scan(&exists)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	data, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	rev, err := nextRevision(ctx, tx)
	if err != nil {
		return false, err
	}
	now := records.Now()
	_, err = tx.ExecContext(ctx, `INSERT INTO records
		(record_type, record_id, data, server_revision, created_at, updated_at)
		VALUES ('actor', ?, ?, ?, ?, ?)`, a.ID, string(data), rev, now, now)
	return err == nil, err
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.PullRequest](w, r)
	if !ok {
		return
	}
	if !s.allow(w, r, req.RepositoryID, api.GrantRead) {
		return
	}
	ctx := r.Context()
	resp := api.PullResponse{Records: []api.Record{}, Tombstones: []api.Tombstone{}}

	err := s.Repos.View(ctx, req.RepositoryID, func(db *sql.DB) error {
		if err := db.QueryRowContext(ctx, `SELECT revision FROM meta WHERE id = 1`).
			Scan(&resp.ServerRevision); err != nil {
			return err
		}
		rows, err := db.QueryContext(ctx, `SELECT record_type, record_id, data, server_revision, deleted_at
			FROM records WHERE server_revision > ? ORDER BY server_revision`, req.AfterRevision)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var rec api.Record
			var data string
			var deletedAt sql.NullString
			if err := rows.Scan(&rec.RecordType, &rec.RecordID, &data, &rec.ServerRevision, &deletedAt); err != nil {
				return err
			}
			rec.Data = json.RawMessage(data)
			if deletedAt.Valid {
				resp.Tombstones = append(resp.Tombstones, api.Tombstone{
					RecordType: rec.RecordType, RecordID: rec.RecordID,
					DeletedAt: deletedAt.String, ServerRevision: rec.ServerRevision})
			} else {
				resp.Records = append(resp.Records, rec)
			}
		}
		return rows.Err()
	})
	if err != nil {
		s.respond(w, "pull", err)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handleGetRecord(w http.ResponseWriter, r *http.Request) {
	repoID, recordType, recordID := r.PathValue("repo"), r.PathValue("type"), r.PathValue("id")
	if !s.allow(w, r, repoID, api.GrantRead) {
		return
	}
	var rec api.Record
	rec.RecordType, rec.RecordID = recordType, recordID
	found := false
	err := s.Repos.View(r.Context(), repoID, func(db *sql.DB) error {
		var data string
		err := db.QueryRowContext(r.Context(), `SELECT data, server_revision FROM records
			WHERE record_type = ? AND record_id = ? AND deleted_at IS NULL`,
			recordType, recordID).Scan(&data, &rec.ServerRevision)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rec.Data = json.RawMessage(data)
		found = true
		return nil
	})
	if err != nil {
		s.respond(w, "load record", err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "not_found", "record not found")
		return
	}
	writeJSON(w, rec)
}

// handleMerge is the authoritative PR state transition (spec §12 step 12):
// open -> merged exactly once, serialized by the repository database.
func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	prID := r.PathValue("id")
	req, ok := decode[api.MergeRequest](w, r)
	if !ok {
		return
	}
	if !s.allow(w, r, req.RepositoryID, api.GrantWrite) {
		return
	}
	if req.MergeCommitSHA == "" || req.HeadCommitSHA == "" {
		writeErr(w, http.StatusBadRequest, "validation", "head_commit_sha and merge_commit_sha are required")
		return
	}
	ctx := r.Context()
	var resp api.MergeResponse
	var conflictMsg string

	err := s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		conflictMsg = ""
		var data string
		err := tx.QueryRowContext(ctx, `SELECT data FROM records
			WHERE record_type = 'pull_request' AND record_id = ? AND deleted_at IS NULL`, prID).Scan(&data)
		if errors.Is(err, sql.ErrNoRows) {
			conflictMsg = "pull request not found"
			return nil
		}
		if err != nil {
			return err
		}
		var pr map[string]json.RawMessage
		if err := json.Unmarshal([]byte(data), &pr); err != nil {
			return err
		}
		var status string
		json.Unmarshal(pr["status"], &status)
		if status != "open" {
			conflictMsg = "pull request is " + status
			return nil
		}
		if req.ExpectedHeadSHA != "" {
			var head string
			json.Unmarshal(pr["head_commit_sha"], &head)
			if head != "" && head != req.ExpectedHeadSHA {
				conflictMsg = "pull request head is " + head + ", expected " + req.ExpectedHeadSHA
				return nil
			}
		}

		now := records.Now()
		set := func(key, val string) {
			b, _ := json.Marshal(val)
			pr[key] = b
		}
		set("status", "merged")
		set("merge_commit_sha", req.MergeCommitSHA)
		set("head_commit_sha", req.HeadCommitSHA)
		set("merged_at", now)
		set("updated_at", now)
		merged, _ := json.Marshal(pr)

		rev, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE records SET data = ?, server_revision = ?, updated_at = ?
			WHERE record_type = 'pull_request' AND record_id = ?`,
			string(merged), rev, now, prID); err != nil {
			return err
		}
		if err := setFieldRevisions(ctx, tx, "pull_request", prID,
			[]string{"status", "merge_commit_sha", "head_commit_sha", "merged_at"}, rev); err != nil {
			return err
		}
		resp = api.MergeResponse{
			Record:         api.Record{RecordType: "pull_request", RecordID: prID, Data: merged, ServerRevision: rev},
			ServerRevision: rev,
		}
		return nil
	})
	if err != nil {
		s.respond(w, "merge", err)
		return
	}
	if conflictMsg == "pull request not found" {
		writeErr(w, http.StatusNotFound, "not_found", conflictMsg)
		return
	}
	if conflictMsg != "" {
		writeErr(w, http.StatusConflict, "conflict", conflictMsg)
		return
	}
	writeJSON(w, resp)
}

func (s *Server) handleUploadURL(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.UploadURLRequest](w, r)
	if !ok {
		return
	}
	if !s.allow(w, r, req.RepositoryID, api.GrantWrite) {
		return
	}
	if len(req.SHA256) != 64 || req.SizeBytes < 0 {
		writeErr(w, http.StatusBadRequest, "validation", "sha256 and size_bytes are required")
		return
	}
	ctx := r.Context()
	key := "sha256/" + req.SHA256[:2] + "/" + req.SHA256

	var alreadyStored bool
	err := s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		var stored bool
		err := tx.QueryRowContext(ctx, `SELECT stored FROM blobs WHERE sha256 = ?`, req.SHA256).Scan(&stored)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		alreadyStored = stored
		if stored {
			return nil
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO blobs (sha256, size_bytes, storage_key, created_at)
			VALUES (?, ?, ?, ?) ON CONFLICT (sha256) DO NOTHING`,
			req.SHA256, req.SizeBytes, key, records.Now())
		return err
	})
	if err != nil {
		s.respond(w, "record blob", err)
		return
	}
	if alreadyStored {
		writeJSON(w, api.UploadURLResponse{StorageKey: key, AlreadyStored: true})
		return
	}
	url, err := s.Blobs.SignedPutURL(ctx, key, req.MediaType)
	if err != nil {
		s.internal(w, "sign upload url", err)
		return
	}
	writeJSON(w, api.UploadURLResponse{URL: url, StorageKey: key})
}

// handleConfirmUpload verifies the blob landed in object storage, marks it
// stored, and stamps storage_key onto every artifact record with that hash
// (bumping their revisions so all clients learn the cloud copy exists).
func (s *Server) handleConfirmUpload(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.UploadURLRequest](w, r)
	if !ok {
		return
	}
	if !s.allow(w, r, req.RepositoryID, api.GrantWrite) {
		return
	}
	if len(req.SHA256) != 64 {
		writeErr(w, http.StatusBadRequest, "validation", "sha256 is required")
		return
	}
	key := "sha256/" + req.SHA256[:2] + "/" + req.SHA256
	exists, err := s.Blobs.Exists(r.Context(), key)
	if err != nil {
		s.internal(w, "check blob", err)
		return
	}
	if !exists {
		writeErr(w, http.StatusConflict, "conflict", "blob not found in object storage")
		return
	}
	// Existence is not enough. Whoever holds the upload URL chooses the bytes,
	// and this handler is what stamps storage_key onto every artifact record
	// carrying this hash — so without verifying, "artifacts are immutable by
	// checksum" (spec §6.9) would be a property nothing enforces, and a blob
	// could be poisoned at a hash before its real content arrived.
	if err := s.verifyBlobDigest(r.Context(), key, req.SHA256); err != nil {
		var mismatch digestMismatch
		if errors.As(err, &mismatch) {
			// Content that does not hash to its key is unreachable by any
			// correct client and would otherwise satisfy a later confirm.
			if delErr := s.Blobs.Delete(r.Context(), key); delErr != nil && s.Log != nil {
				s.Log.Error("delete mismatched blob", "key", key, "error", delErr)
			}
			writeErr(w, http.StatusConflict, "conflict", mismatch.Error())
			return
		}
		s.internal(w, "verify blob", err)
		return
	}
	ctx := r.Context()
	err = s.Repos.Update(ctx, req.RepositoryID, false, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET stored = 1 WHERE sha256 = ?`, req.SHA256); err != nil {
			return err
		}
		var needsStamp bool
		err := tx.QueryRowContext(ctx, `SELECT true FROM records
			WHERE record_type = 'artifact' AND data->>'sha256' = ?
			AND COALESCE(data->>'storage_key', '') = '' LIMIT 1`, req.SHA256).Scan(&needsStamp)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		rev, err := nextRevision(ctx, tx)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE records
			SET data = json_set(data, '$.storage_key', ?), server_revision = ?, updated_at = ?
			WHERE record_type = 'artifact' AND data->>'sha256' = ?
			AND COALESCE(data->>'storage_key', '') = ''`,
			key, rev, records.Now(), req.SHA256)
		return err
	})
	if err != nil {
		s.respond(w, "confirm upload", err)
		return
	}
	writeJSON(w, api.UploadURLResponse{StorageKey: key, AlreadyStored: true})
}

func (s *Server) handleDownloadURL(w http.ResponseWriter, r *http.Request) {
	req, ok := decode[api.DownloadURLRequest](w, r)
	if !ok {
		return
	}
	// `read`, not `write`: fetching an artifact somebody else stored is
	// reading the repository, and a reader that could not open an artifact
	// would be reading half of it.
	if !s.allow(w, r, req.RepositoryID, api.GrantRead) {
		return
	}
	// And the blob has to be one of *this* repository's. Blobs are addressed
	// by content hash in a store shared across every repository, so the
	// repository id in the request is the only thing scoping this call — and
	// without checking it, `read` on any repository would sign a URL for any
	// blob on the service, which is the one place a per-repository grant
	// could be walked around. It was unreachable while one token reached
	// everything; it is reachable the moment a grant means something.
	//
	// Either witness will do. A blobs row is what an upload into this
	// repository leaves behind, and an artifact record naming the key is what
	// survives a client replaying its history into a restored repository —
	// which would otherwise stop being able to fetch its own artifacts.
	held := false
	err := s.Repos.View(r.Context(), req.RepositoryID, func(db *sql.DB) error {
		return db.QueryRowContext(r.Context(), `SELECT
			EXISTS(SELECT 1 FROM blobs WHERE storage_key = ?)
			OR EXISTS(SELECT 1 FROM records WHERE record_type = 'artifact'
				AND data->>'storage_key' = ?)`,
			req.StorageKey, req.StorageKey).Scan(&held)
	})
	if err != nil {
		s.respond(w, "load repository", err)
		return
	}
	if !held {
		writeErr(w, http.StatusNotFound, "not_found",
			"blob not available: repository "+req.RepositoryID+" holds no artifact stored at "+req.StorageKey)
		return
	}
	url, err := s.Blobs.SignedGetURL(r.Context(), req.StorageKey)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "blob not available: "+err.Error())
		return
	}
	writeJSON(w, api.DownloadURLResponse{URL: url})
}

func (s *Server) internal(w http.ResponseWriter, what string, err error) {
	if s.Log != nil {
		s.Log.Error(what, "error", err)
	}
	writeErr(w, http.StatusInternalServerError, "internal", what+" failed")
}

// maxVerifyBytes bounds how much content the service will hash for one
// confirm. It is a denial-of-service guard, not a size limit on artifacts:
// anything larger is refused rather than silently trusted, because trusting
// unverified content is exactly the failure this check exists to prevent.
const maxVerifyBytes = 1 << 30 // 1 GiB

// digestMismatch reports stored content that does not hash to its key.
type digestMismatch struct{ want, got string }

func (d digestMismatch) Error() string {
	return "stored content does not match its checksum (expected " + d.want + ", got " + d.got + ")"
}

// verifyBlobDigest streams the stored object and checks it hashes to the
// value the client claimed.
func (s *Server) verifyBlobDigest(ctx context.Context, key, want string) error {
	rc, err := s.Blobs.Open(ctx, key)
	if err != nil {
		return err
	}
	defer rc.Close()

	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(rc, maxVerifyBytes+1))
	if err != nil {
		return err
	}
	if n > maxVerifyBytes {
		return fmt.Errorf("blob exceeds the %d byte verification limit", int64(maxVerifyBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
		return digestMismatch{want: want, got: got}
	}
	return nil
}

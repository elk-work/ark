package server

// The Git mirror (elk-work/ark#125): a repository the service hosts can be
// pushed, whole, to a second Git host — GitHub, in practice — as a backup.
//
// The mirror is a copy and never the record. The service's bare repository
// is what clones and pushes read and write; the mirror is `git push --mirror`
// run against it after every push that changed something and on a ticker
// (ARK_MIRROR_INTERVAL, default 10m) whenever the refs differ from what the
// last successful mirror pushed. A failure is logged and reported on
// `GET /v1/repositories/{repo}` as `mirror.last_error`, and is never a
// failure of the push that triggered it.
//
// What makes this safe to offer is in three rules, each of which closes a way
// to abuse a service-wide credential that `git push --mirror` — which
// force-updates and deletes every ref at the far end — would otherwise hand
// out:
//
//   - Setting the mirror is an operator act, not only an `admin` one
//     (repometa.go). Any principal administers the repositories it
//     registers; pointing the service's credential at a host, or at
//     somebody else's GitHub repository, is a decision about that credential.
//   - The URL is https and carries no credential. ARK_MIRROR_CREDENTIAL is
//     the only credential, injected by a credential helper scoped to the
//     mirror's own scheme and host, so it is offered to nothing else — not
//     to a host a redirect lands on (redirects are off as well), and never
//     through argv, the stored URL, or a log line.
//   - An empty repository is never mirrored. `--mirror` would make the far
//     end match it, which is deleting the backup.
//
// The status is the service's own observation and lives with the bare
// repository (ark-mirror.json inside it, which Git ignores), not in the
// repository database: every write there rewrites an object in the bucket,
// and a mirror that fails every ten minutes should not.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/server/repodb"
	"github.com/elk-work/ark/pkg/api"
)

// DefaultMirrorInterval is how often the ticker looks for repositories whose
// refs have moved since their last successful mirror.
const DefaultMirrorInterval = 10 * time.Minute

// mirrorTimeout bounds one `git push --mirror`.
const mirrorTimeout = 10 * time.Minute

// mirrorStatusFile is the sidecar inside a bare repository.
const mirrorStatusFile = "ark-mirror.json"

// mirrorState is one repository's mirror bookkeeping.
type mirrorState struct {
	// run serializes mirror pushes for one repository; a push that lands
	// while one is running is picked up by the next.
	run sync.Mutex

	mu     sync.Mutex // guards the fields below
	loaded bool
	saved  mirrorRecord
}

// mirrorRecord is what ark-mirror.json holds.
type mirrorRecord struct {
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastOKAt      string `json:"last_ok_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	// OKURL and OKRefs are the mirror URL and the refs fingerprint the last
	// successful mirror pushed. The ticker compares against them, so a
	// repository nobody pushed to costs one for-each-ref per interval and no
	// network.
	OKURL  string `json:"ok_url,omitempty"`
	OKRefs string `json:"ok_refs,omitempty"`
}

func (s *Server) mirrorState(repoID string) *mirrorState {
	s.mirrorMu.Lock()
	defer s.mirrorMu.Unlock()
	if s.mirrors == nil {
		s.mirrors = map[string]*mirrorState{}
	}
	st, ok := s.mirrors[repoID]
	if !ok {
		st = &mirrorState{}
		s.mirrors[repoID] = st
	}
	return st
}

// record returns the repository's mirror record, reading the sidecar the
// first time.
func (s *Server) mirrorRecordOf(repoID string, st *mirrorState) mirrorRecord {
	st.mu.Lock()
	defer st.mu.Unlock()
	if !st.loaded {
		st.loaded = true
		if b, err := os.ReadFile(filepath.Join(s.bareRepoDir(repoID), mirrorStatusFile)); err == nil {
			_ = json.Unmarshal(b, &st.saved)
		}
	}
	return st.saved
}

func (s *Server) saveMirrorRecord(repoID string, st *mirrorState, rec mirrorRecord) {
	st.mu.Lock()
	st.loaded, st.saved = true, rec
	st.mu.Unlock()
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(s.bareRepoDir(repoID), mirrorStatusFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		if err := os.Rename(tmp, path); err != nil {
			s.logError("save mirror status", err)
		}
	}
}

// fillMirrorStatus adds the service's last observation to a repository's
// mirror URL, for GET /v1/repositories/{repo}. The observation belongs to
// the URL it was made against: after the URL changes, an old error is not
// this mirror's.
func (s *Server) fillMirrorStatus(repoID string, m *api.MirrorStatus) {
	if s.GitDataDir == "" {
		return
	}
	rec := s.mirrorRecordOf(repoID, s.mirrorState(repoID))
	m.LastAttemptAt = rec.LastAttemptAt
	m.LastError = rec.LastError
	if rec.OKURL == m.URL {
		m.LastOKAt = rec.LastOKAt
	}
}

// validMirrorURL is the shape a mirror URL must have. "" clears the mirror.
func (s *Server) validMirrorURL(raw string) *writeFault {
	if raw == "" {
		return nil
	}
	reject := func(why string) *writeFault {
		return faultValidation("mirror_url is not usable: " + why)
	}
	if len(raw) > maxRemoteLen {
		return reject("too long")
	}
	if strings.ContainsFunc(raw, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) }) {
		return reject("it contains whitespace or control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return reject(err.Error())
	}
	switch {
	case u.Scheme == "https":
	case s.allowInsecureMirrors && (u.Scheme == "http" || u.Scheme == "file"):
		// Tests only: a second local repository or service stands in for
		// GitHub. Nothing outside this package can set the field.
	default:
		return reject("it must be an https URL (for example https://github.com/<owner>/<repo>.git)")
	}
	if u.User != nil {
		return reject("it carries a credential; the mirror is pushed with the service's ARK_MIRROR_CREDENTIAL, " +
			"which never lives in the URL")
	}
	if u.Scheme != "file" && u.Host == "" {
		return reject("no host")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return reject("a Git remote has no query or fragment")
	}
	return nil
}

// mirrorURL reads a repository's mirror URL. "" when none is set.
func (s *Server) mirrorURL(ctx context.Context, repoID string) (string, error) {
	var u string
	err := s.Repos.View(ctx, repoID, func(db *sql.DB) error {
		return db.QueryRowContext(ctx,
			`SELECT COALESCE((SELECT url FROM git_mirror WHERE id = 1), '')`).Scan(&u)
	})
	return u, err
}

// refsFingerprint is a stable digest of a ref listing.
func refsFingerprint(refs map[string]string) string {
	names := make([]string, 0, len(refs))
	for name := range refs {
		names = append(names, name)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, name := range names {
		h.Write([]byte(refs[name] + " " + name + "\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// mirrorRepo pushes one repository to its mirror if there is one and its refs
// have moved since the last successful mirror. It never returns an error: the
// outcome is the status it records.
func (s *Server) mirrorRepo(ctx context.Context, repoID string) {
	dir := s.bareRepoDir(repoID)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return
	}
	target, err := s.mirrorURL(ctx, repoID)
	if errors.Is(err, repodb.ErrNotFound) || (err == nil && target == "") {
		return
	}
	if err != nil {
		s.logError("read mirror url", err)
		return
	}
	st := s.mirrorState(repoID)
	st.run.Lock()
	defer st.run.Unlock()

	refs, err := s.gitRefs(ctx, dir)
	if err != nil {
		s.logError("read refs for mirror", err)
		return
	}
	if len(refs) == 0 {
		return
	}
	fingerprint := refsFingerprint(refs)
	rec := s.mirrorRecordOf(repoID, st)
	if rec.OKURL == target && rec.OKRefs == fingerprint {
		return
	}

	ctx, cancel := context.WithTimeout(ctx, mirrorTimeout)
	defer cancel()
	started := time.Now()
	pushErr := s.pushMirror(ctx, dir, target)
	rec.LastAttemptAt = records.Now()
	if pushErr != nil {
		rec.LastError = pushErr.Error()
	} else {
		rec.LastError = ""
		rec.LastOKAt = rec.LastAttemptAt
		rec.OKURL, rec.OKRefs = target, fingerprint
	}
	s.saveMirrorRecord(repoID, st, rec)
	if s.Log != nil {
		if pushErr != nil {
			s.Log.Warn("git mirror failed", "repository_id", repoID, "mirror", target,
				"error", rec.LastError, "duration_ms", time.Since(started).Milliseconds())
		} else {
			s.Log.Info("git mirror", "repository_id", repoID, "mirror", target,
				"refs", len(refs), "duration_ms", time.Since(started).Milliseconds())
		}
	}
}

// pushMirror runs `git push --mirror`. The credential reaches Git through a
// credential helper configured for the mirror's own scheme and host, reading
// two variables set only in this child's environment — so the secret is in
// neither argv nor the URL, and Git offers it to no other host.
func (s *Server) pushMirror(ctx context.Context, dir, target string) error {
	args := []string{
		// Clear every helper the environment might configure, then add ours.
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
	}
	env := s.gitEnv()
	user, pass := mirrorCredentialParts(s.MirrorCredential)
	if pass != "" {
		if u, err := url.Parse(target); err == nil && (u.Scheme == "https" || u.Scheme == "http") {
			args = append(args, "-c", "credential."+u.Scheme+"://"+u.Host+".helper="+
				`!f() { test "$1" = get && printf 'username=%s\npassword=%s\n' "$ARK_MIRROR_USERNAME" "$ARK_MIRROR_PASSWORD"; }; f`)
			env = append(env, "ARK_MIRROR_USERNAME="+user, "ARK_MIRROR_PASSWORD="+pass)
		}
	}
	args = append(args, "push", "--mirror", "--porcelain", target)
	cmd := s.gitCommand(ctx, dir, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if pass != "" {
		msg = strings.ReplaceAll(msg, pass, "[redacted]")
	}
	if len(msg) > 2000 {
		msg = msg[:2000] + "…"
	}
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(msg)
}

// mirrorCredentialParts splits ARK_MIRROR_CREDENTIAL. A bare token gets the
// username GitHub documents for token authentication, which it otherwise
// ignores.
func mirrorCredentialParts(cred string) (user, pass string) {
	if cred == "" {
		return "", ""
	}
	if u, p, ok := strings.Cut(cred, ":"); ok && u != "" && p != "" {
		return u, p
	}
	return "x-access-token", cred
}

// mirrorAll is one tick: every bare repository on this instance whose refs
// have moved since its last successful mirror is mirrored. Sequential,
// because the mirror is a backup and not a race.
func (s *Server) mirrorAll(ctx context.Context) {
	entries, err := os.ReadDir(s.GitDataDir)
	if err != nil {
		if !os.IsNotExist(err) {
			s.logError("list bare repositories", err)
		}
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".git")
		if !ok || !e.IsDir() || !records.ValidID(id) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		s.mirrorRepo(ctx, id)
	}
}

// RunMirrorLoop runs the mirror ticker until ctx ends. cmd/ark-server starts
// it; nothing in Handler does, because tests build a handler per request.
func (s *Server) RunMirrorLoop(ctx context.Context) {
	if s.GitDataDir == "" {
		return
	}
	every := s.MirrorInterval
	if every <= 0 {
		every = DefaultMirrorInterval
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.mirrorAll(ctx)
		}
	}
}

// background runs work that follows a request — the mirror and the push
// webhook — on the service's clock rather than the client's. WaitBackground
// is how a test, or a shutdown, waits for it.
func (s *Server) background(fn func(ctx context.Context)) {
	s.bgWG.Add(1)
	go func() {
		defer s.bgWG.Done()
		fn(context.Background())
	}()
}

// WaitBackground blocks until every background job started so far is done.
func (s *Server) WaitBackground() { s.bgWG.Wait() }

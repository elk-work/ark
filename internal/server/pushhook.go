package server

// The push webhook (elk-work/ark#125): after a push to the service's Git
// lands, one POST per updated ref to ARK_PUSH_WEBHOOK_URL, bearing
// ARK_PUSH_WEBHOOK_TOKEN, with the body api.PushEvent describes.
//
// Ark knows nothing about checks. The receiver — Elk's connector — decides
// what a push means; the event carries what it needs to decide without
// cloning: which ref moved from where to where, who pushed it, which paths
// changed, and the contents of the manifest files it reads at the new tip
// (`ci/suites.yml`), so the common case costs it no Git at all.
//
// Delivery is at least once and in ref order within a push, with three
// retries on backoff, then a log line. Every delivery of one event carries
// the same X-Ark-Delivery id, so a receiver that saw an attempt it timed out
// on can tell the retry for what it is.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// pushManifests are the files a push event carries the contents of, when
// they exist at the pushed commit.
var pushManifests = []string{"ci/suites.yml"}

// Bounds on one event, so a push of a vendored tree cannot make a body no
// receiver will accept.
const (
	maxChangedPaths  = 10000
	maxManifestBytes = 1 << 20
)

// defaultWebhookBackoff is the wait before each of the three retries.
var defaultWebhookBackoff = []time.Duration{time.Second, 4 * time.Second, 16 * time.Second}

// afterPush is what follows a push that changed something: the webhook and
// the mirror, each on its own, after the client has its answer.
func (s *Server) afterPush(repoID, dir string, who *authenticated, updates []refUpdate) {
	if s.PushWebhookURL != "" {
		s.background(func(ctx context.Context) { s.deliverPushEvents(ctx, repoID, dir, who, updates) })
	}
	s.background(func(ctx context.Context) { s.mirrorRepo(ctx, repoID) })
}

// deliverPushEvents sends one event per updated ref, in ref order.
func (s *Server) deliverPushEvents(ctx context.Context, repoID, dir string, who *authenticated, updates []refUpdate) {
	pusher := api.PushPusher{}
	if who != nil {
		pusher = api.PushPusher{PrincipalID: who.ID, Email: who.Email}
	}
	for _, u := range updates {
		ev := api.PushEvent{
			RepositoryID: repoID,
			Ref:          u.Ref,
			Before:       u.Before,
			After:        u.After,
			Pusher:       pusher,
			ChangedPaths: []string{},
			Manifests:    map[string]string{},
		}
		if !isZeroID(u.After) {
			paths, err := s.changedPaths(ctx, dir, u)
			if err != nil {
				s.logError("changed paths for push event", err)
			}
			if len(paths) > maxChangedPaths {
				paths, ev.ChangedPathsTruncated = paths[:maxChangedPaths], true
			}
			if paths != nil {
				ev.ChangedPaths = paths
			}
			for _, path := range pushManifests {
				if body, ok := s.fileAt(ctx, dir, u.After, path); ok {
					ev.Manifests[path] = body
				}
			}
		}
		s.postPushEvent(ctx, ev)
	}
}

// changedPaths is what a ref update touched: `git diff --name-only` between
// the two commits, or for a created ref the paths its tip commit changed —
// every path, when that commit has no parent.
func (s *Server) changedPaths(ctx context.Context, dir string, u refUpdate) ([]string, error) {
	after, err := s.peelCommit(ctx, dir, u.After)
	if err != nil || after == "" {
		// A ref to a tree or a blob — a tag can name one — changed no paths
		// a commit diff can describe.
		return nil, err
	}
	var args []string
	switch before, _ := s.peelCommit(ctx, dir, u.Before); {
	case !isZeroID(u.Before) && before != "":
		args = []string{"diff", "--name-only", "-z", before, after}
	default:
		if parent, _ := s.peelCommit(ctx, dir, after+"^1"); parent != "" {
			args = []string{"diff", "--name-only", "-z", parent, after}
		} else {
			args = []string{"ls-tree", "-r", "--name-only", "-z", after}
		}
	}
	out, err := s.gitCommand(ctx, dir, args...).Output()
	if err != nil {
		return nil, gitError(args[0], err)
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// peelCommit resolves a revision to the commit it names, or "" when it names
// none.
func (s *Server) peelCommit(ctx context.Context, dir, rev string) (string, error) {
	if isZeroID(rev) {
		return "", nil
	}
	out, err := s.gitCommand(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}").Output()
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// fileAt reads one file at a commit, if it is there and not too large.
func (s *Server) fileAt(ctx context.Context, dir, rev, path string) (string, bool) {
	spec := rev + ":" + path
	if err := s.gitCommand(ctx, dir, "cat-file", "-e", spec).Run(); err != nil {
		return "", false
	}
	out, err := s.gitCommand(ctx, dir, "cat-file", "-s", spec).Output()
	if err != nil {
		return "", false
	}
	var size int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &size); err != nil || size > maxManifestBytes {
		return "", false
	}
	body, err := s.gitCommand(ctx, dir, "cat-file", "blob", spec).Output()
	if err != nil {
		return "", false
	}
	return string(body), true
}

// postPushEvent delivers one event: the first attempt and three retries.
func (s *Server) postPushEvent(ctx context.Context, ev api.PushEvent) {
	body, err := json.Marshal(ev)
	if err != nil {
		s.logError("encode push event", err)
		return
	}
	backoff := s.webhookBackoff
	if backoff == nil {
		backoff = defaultWebhookBackoff
	}
	client := s.webhookClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	delivery := records.NewID()
	var lastErr string
	for attempt := 0; attempt <= len(backoff); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff[attempt-1]):
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", s.PushWebhookURL, bytes.NewReader(body))
		if err != nil {
			s.logError("build push webhook request", err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "ark-server/"+s.Version)
		req.Header.Set("X-Ark-Event", "push")
		req.Header.Set("X-Ark-Delivery", delivery)
		if s.PushWebhookToken != "" {
			req.Header.Set("Authorization", "Bearer "+s.PushWebhookToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if s.Log != nil {
				s.Log.Info("push webhook delivered", "repository_id", ev.RepositoryID, "ref", ev.Ref,
					"delivery", delivery, "attempts", attempt+1)
			}
			return
		}
		lastErr = resp.Status
	}
	if s.Log != nil {
		// The URL is the operator's own and not a secret; the token never
		// appears here.
		s.Log.Error("push webhook not delivered", "repository_id", ev.RepositoryID, "ref", ev.Ref,
			"after", ev.After, "delivery", delivery, "attempts", len(backoff)+1, "error", lastErr)
	}
}

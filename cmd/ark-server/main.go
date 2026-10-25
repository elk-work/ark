// Command ark-server is the Ark sync service. Repository metadata lives in
// one SQLite database per repository, persisted to GCS in production or a
// local directory in development; artifact blobs go to object storage.
//
// Configuration (environment):
//
//	ARK_API_TOKEN    the legacy shared bearer token (optional). Unset, the
//	                 legacy branch is not registered and every client
//	                 authenticates with a per-principal credential.
//	                 RFC-0003 Stage 4
//	ARK_LEGACY_TOKEN what ARK_API_TOKEN may still do as a bearer:
//	                 full | readonly | off (default full, which is the
//	                 behaviour before this setting existed). readonly lets it
//	                 pull and read and refuses every write; off stops
//	                 accepting it as a bearer at all. RFC-0003 Stage 3. With
//	                 ARK_API_TOKEN unset only off (or unset) is accepted
//	ARK_UI          on | off (default on); hides the board and task GET routes
//	ARK_SIGNING_KEY  HMAC key signing local-mode /blobs/ URLs (default:
//	                 ARK_API_TOKEN; required in local mode without it)
//	ARK_BOOTSTRAP_TOKEN
//	                 accepted on POST /v1/principals only, to mint the first
//	                 per-principal credential; unset disables that route
//	ARK_IDP_APPROVAL_URL
//	                 where `ark login` sends a person to approve a device
//	                 code; unset means this service offers no device login
//	ARK_IDP_KEY      shared secret the identity provider presents on
//	                 POST /v1/device/approve (required with the above)
//	ARK_DEFAULT_GRANT
//	                 what a principal holds on a repository nobody granted it:
//	                 none | read | seeded (default seeded, which grants
//	                 nothing without an identity provider to seed from)
//	GCS_BUCKET       bucket for repo databases and artifact blobs (production)
//	DATA_DIR         local directory for repo databases + blobs (used without
//	                 GCS_BUCKET; default ./data)
//	BASE_URL         externally reachable base URL (required without GCS_BUCKET)
//	CACHE_DIR        scratch space for working copies (default os temp dir)
//	GIT_DATA_DIR     where the bare Git repositories served at /git/<id> live
//	                 (default <DATA_DIR>/git). Needs the git binary on PATH.
//	                 elk-work/ark#125, internal/server/smarthttp.go
//	ARK_MIRROR_INTERVAL
//	                 how often repositories whose refs moved are pushed to
//	                 their mirror, as a Go duration (default 10m)
//	ARK_MIRROR_CREDENTIAL
//	                 token, or user:token, the mirror is pushed with; offered
//	                 only to the mirror's own host, never logged
//	ARK_PUSH_WEBHOOK_URL
//	                 where a push event is POSTed, one per updated ref
//	                 (unset: no webhook)
//	ARK_PUSH_WEBHOOK_TOKEN
//	                 bearer the push webhook carries
//	PORT             listen port (default 8080)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/storage"

	"github.com/elk-work/ark/internal/buildinfo"
	"github.com/elk-work/ark/internal/server"
	"github.com/elk-work/ark/internal/server/repodb"
)

// version is set at build time via -ldflags "-X main.version=...". Left
// unset, buildinfo.Resolve falls back to the module version or the VCS
// stamp Go embeds on its own.
var version = buildinfo.Dev

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ark-server:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()
	ver := buildinfo.Resolve(version)
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("version", ver)

	// Optional since RFC-0003 Stage 4 (elk-work/ark#54). Unset is the
	// retired configuration, not a missing one: the legacy branch in s.auth is
	// not registered, so the shared string is answered like any unknown
	// bearer, and principals and their credentials are the only way in.
	token := os.Getenv("ARK_API_TOKEN")
	signingKey := os.Getenv("ARK_SIGNING_KEY")
	// The device flow needs both halves or neither: an approval URL with no
	// key is a service that sends people to a page whose approvals it cannot
	// verify, and it would fail at the last step of a login rather than at
	// startup. Refuse at startup, as ARK_API_TOKEN and BASE_URL do.
	approvalURL := os.Getenv("ARK_IDP_APPROVAL_URL")
	idpKey := os.Getenv("ARK_IDP_KEY")
	if approvalURL != "" && idpKey == "" {
		return fmt.Errorf("ARK_IDP_KEY is required when ARK_IDP_APPROVAL_URL is set")
	}
	// Validated at startup for the same reason, and not at the first refused
	// request: a typo in an authorization default is the kind of mistake that
	// reads as an outage hours later, and it costs one comparison to refuse
	// to start instead.
	defaultGrant := os.Getenv("ARK_DEFAULT_GRANT")
	if defaultGrant != "" && !slices.Contains(server.DefaultGrantValues, defaultGrant) {
		return fmt.Errorf("ARK_DEFAULT_GRANT is %q; it takes %s",
			defaultGrant, strings.Join(server.DefaultGrantValues, ", "))
	}
	// And for the same reason again, more sharply: this one narrows what the
	// token the whole fleet holds may do, so a value that is none of the three
	// must not be read as "carry on as before" — a service that answered
	// ARK_LEGACY_TOKEN=read-only by accepting every write would report the
	// riskiest step of the cutover as done while changing nothing. With no
	// ARK_API_TOKEN the only position left is off, and an explicit full or
	// readonly is refused rather than reinterpreted (see ResolveLegacyMode).
	legacyMode, err := server.ResolveLegacyMode(os.Getenv("ARK_LEGACY_TOKEN"), token != "")
	if err != nil {
		return err
	}
	uiMode, err := server.ParseUIMode(os.Getenv("ARK_UI"))
	if err != nil {
		return err
	}
	cacheDir := os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = filepath.Join(os.TempDir(), "ark-repos")
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return err
	}

	// Git hosting needs a filesystem whatever the storage mode: a bare
	// repository is a directory Git itself reads and writes. In local mode
	// it sits beside the repository databases under DATA_DIR; in
	// object-storage mode the same default is scratch space, and
	// docs/self-hosting.md says what that means for a restart.
	dataDir := os.Getenv("DATA_DIR")
	if dataDir == "" {
		dataDir = "data"
	}
	gitDataDir := os.Getenv("GIT_DATA_DIR")
	if gitDataDir == "" {
		gitDataDir = filepath.Join(dataDir, "git")
	}
	if err := os.MkdirAll(gitDataDir, 0o755); err != nil {
		return fmt.Errorf("GIT_DATA_DIR %s: %w", gitDataDir, err)
	}
	// Validated at startup like every other setting here: a typo in an
	// interval or a webhook URL would otherwise surface as a backup that
	// never runs, or checks that never start, noticed days later.
	mirrorInterval := server.DefaultMirrorInterval
	if v := os.Getenv("ARK_MIRROR_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			return fmt.Errorf("ARK_MIRROR_INTERVAL is %q; it takes a Go duration of at least 1m, such as 10m", v)
		}
		mirrorInterval = d
	}
	webhookURL := os.Getenv("ARK_PUSH_WEBHOOK_URL")
	if webhookURL != "" {
		u, err := url.Parse(webhookURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("ARK_PUSH_WEBHOOK_URL is not an http(s) URL")
		}
	}

	var backend repodb.Backend
	var blobs server.BlobStore
	var gitSnapshots server.GitSnapshotStore
	if bucket := os.Getenv("GCS_BUCKET"); bucket != "" {
		client, err := storage.NewClient(ctx)
		if err != nil {
			return fmt.Errorf("gcs client: %w", err)
		}
		backend = &repodb.GCSBackend{Client: client, Bucket: bucket}
		blobs = &server.GCSBlobStore{Client: client, Bucket: bucket}
		// GIT_DATA_DIR is scratch in this mode, so each bare repository is
		// restored from git/<id>.tar.gz on first touch and snapshotted back
		// after pushes (internal/server/snapshot.go).
		gitSnapshots = &server.GCSSnapshotStore{Client: client, Bucket: bucket}
	} else {
		base := os.Getenv("BASE_URL")
		if base == "" {
			return fmt.Errorf("BASE_URL is required when GCS_BUCKET is unset")
		}
		// Local mode signs /blobs/ URLs itself, and the key has always
		// defaulted to the service token. Without either the store would
		// refuse every URL it was asked to mint — artifacts broken while
		// records sync, which reads as a bad URL rather than as a missing
		// setting. Refuse at startup instead. Object-storage mode is
		// unaffected: GCS signs its own URLs.
		if token == "" && signingKey == "" {
			return fmt.Errorf("ARK_SIGNING_KEY is required when GCS_BUCKET and ARK_API_TOKEN are both unset: " +
				"it signs local-mode /blobs/ URLs, and the service token it used to default to is not configured")
		}
		backend = &repodb.LocalBackend{Dir: filepath.Join(dataDir, "repos")}
		blobs = &server.LocalBlobStore{Dir: filepath.Join(dataDir, "blobs"), BaseURL: base}
	}

	s := &server.Server{
		Repos: repodb.NewManager(backend, cacheDir),
		Token: token,
		// `full` unless an operator narrowed it, which is what every
		// deployment configured before ARK_LEGACY_TOKEN existed is running.
		LegacyMode: legacyMode,
		UIMode:     uiMode,
		// Unset is the supported configuration while ARK_API_TOKEN is set,
		// not an oversight: the signing key falls back to the service token,
		// which is what it has always been. Setting it is how a deployment
		// stops depending on that, and local mode requires it once the token
		// is gone (checked above).
		SigningKey: signingKey,
		// Unset is also the supported configuration here, and the safer one:
		// no bootstrap token means no route that mints principals at all.
		BootstrapToken: os.Getenv("ARK_BOOTSTRAP_TOKEN"),
		// Unset means no device login, which is every deployment with no
		// identity provider. `ark login --token` and `ark principal create`
		// are unaffected; see internal/server/device.go.
		IDPApprovalURL: approvalURL,
		IDPKey:         idpKey,
		// Unset means `seeded`, which is deny until something seeds a grant.
		DefaultGrant: defaultGrant,
		Blobs:        blobs,
		Log:          log,
		Version:      ver,
		GitDataDir:   gitDataDir,
		// The mirror and the push webhook (elk-work/ark#125). Both secrets
		// stay in these fields: never logged, never on a command line.
		MirrorInterval:   mirrorInterval,
		MirrorCredential: os.Getenv("ARK_MIRROR_CREDENTIAL"),
		PushWebhookURL:   webhookURL,
		PushWebhookToken: os.Getenv("ARK_PUSH_WEBHOOK_TOKEN"),
		GitSnapshots:     gitSnapshots,
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	// HTTP/2 without TLS (h2c, prior knowledge) beside HTTP/1. Cloud Run caps
	// an HTTP/1 request body at 32 MiB, which a first `git push` of a real
	// repository exceeds; deployed with --use-http2 it speaks h2c to the
	// container and the cap does not apply. Serving both costs nothing where
	// nothing sends h2c.
	httpServer.Protocols = new(http.Protocols)
	httpServer.Protocols.SetHTTP1(true)
	httpServer.Protocols.SetUnencryptedHTTP2(true)
	// Once, at startup, and unconditionally — including for `full`. Which
	// position the dial is in is the first question anybody debugging a
	// refused push will ask, and the second is whether the revision that was
	// rolled to change it actually came up with the new value.
	// The "configured" half answers the Stage 4 question the same way: a
	// revision rolled to retire the token logs mode=off configured=false,
	// and nothing else does.
	log.Info("legacy service token", "mode", legacyMode, "configured", token != "")
	if token == "" && s.BootstrapToken == "" && approvalURL == "" {
		// Not an error — a deployment that has minted its principals and then
		// dropped the bootstrap token is the tightest configuration there is.
		// But on a fresh data directory it is one nobody can get into, and
		// that should be the first thing the log says rather than something
		// deduced from a wall of 401s.
		log.Warn("no ARK_API_TOKEN, ARK_BOOTSTRAP_TOKEN or identity provider: only credentials already issued can reach this service")
	}
	if _, err := exec.LookPath("git"); err != nil {
		// Not fatal: every other route works without it. But a service whose
		// /git/ routes all fail should say why once, at startup, rather than
		// in a 500 per clone.
		log.Warn("git is not on PATH: the /git/ routes will fail", "error", err)
	}
	// Which of the Git settings are on, never their values.
	log.Info("git hosting", "git_data_dir", gitDataDir, "mirror_interval", mirrorInterval.String(),
		"mirror_credential", s.MirrorCredential != "", "push_webhook", webhookURL != "",
		"push_webhook_token", s.PushWebhookToken != "")
	go s.RunMirrorLoop(ctx)
	log.Info("listening", "port", port)
	// A stop signal drains requests and then writes every Git snapshot
	// still waiting out its delay, inside the ten seconds Cloud Run allows
	// between SIGTERM and SIGKILL. Without it, a push in the last half
	// minute of an instance's life would leave with the instance.
	stopCtx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpServer.ListenAndServe() }()
	select {
	case err := <-serveErr:
		return err
	case <-stopCtx.Done():
	}
	log.Info("stopping")
	drain, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(drain)
	flush, cancelFlush := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelFlush()
	s.FlushSnapshots(flush)
	return nil
}

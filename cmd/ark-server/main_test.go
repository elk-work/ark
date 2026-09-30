package main

import (
	"strings"
	"testing"
)

// A misconfigured ARK_LEGACY_TOKEN must stop the service coming up, not be
// read as `full` (RFC-0003 Stage 3, elk-work/ark#54).
//
// This is the whole reason the value is validated at startup rather than at
// the first refused request: `gcloud run services update … --update-env-vars
// ARK_LEGACY_TOKEN=…` rolls a new revision whatever the value is, so a typo
// that the service ignored would present as a completed cutover step and a
// fleet still writing with the shared token. A container that will not start
// keeps the previous revision serving, which is the safe half of the mistake.
//
// run() reads the environment in order and returns before it opens a backend
// or listens, so this exercises the real startup path with no service and no
// network.
func TestRunRefusesAnUnknownLegacyMode(t *testing.T) {
	t.Setenv("ARK_API_TOKEN", "test-token")
	t.Setenv("ARK_IDP_APPROVAL_URL", "")
	t.Setenv("ARK_IDP_KEY", "")
	t.Setenv("ARK_DEFAULT_GRANT", "")

	for _, bad := range []string{"read-only", "readonly ", "Readonly", "true", "no"} {
		t.Setenv("ARK_LEGACY_TOKEN", bad)
		err := run()
		if err == nil {
			t.Fatalf("ARK_LEGACY_TOKEN=%q started the service", bad)
		}
		if !strings.Contains(err.Error(), "ARK_LEGACY_TOKEN") {
			t.Errorf("ARK_LEGACY_TOKEN=%q: error does not name the variable: %v", bad, err)
		}
		for _, mode := range []string{"full", "readonly", "off"} {
			if !strings.Contains(err.Error(), mode) {
				t.Errorf("ARK_LEGACY_TOKEN=%q: error does not say %q is accepted: %v", bad, mode, err)
			}
		}
	}
}

func TestRunRefusesUnknownUIMode(t *testing.T) {
	t.Setenv("ARK_API_TOKEN", "test-token")
	t.Setenv("ARK_IDP_APPROVAL_URL", "")
	t.Setenv("ARK_DEFAULT_GRANT", "")
	t.Setenv("ARK_LEGACY_TOKEN", "")
	t.Setenv("ARK_UI", "disabled")
	if err := run(); err == nil || !strings.Contains(err.Error(), "ARK_UI") {
		t.Fatalf("invalid ARK_UI should fail startup: %v", err)
	}
}

// startupEnv sets every variable run() reads — to the given value, or empty —
// so a test states its whole configuration and inherits nothing from the
// machine it runs on.
func startupEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, name := range []string{
		"ARK_API_TOKEN", "ARK_LEGACY_TOKEN", "ARK_UI", "ARK_SIGNING_KEY", "ARK_BOOTSTRAP_TOKEN",
		"ARK_IDP_APPROVAL_URL", "ARK_IDP_KEY", "ARK_DEFAULT_GRANT",
		"GCS_BUCKET", "BASE_URL", "DATA_DIR", "CACHE_DIR", "PORT",
	} {
		t.Setenv(name, env[name])
	}
}

// unlistenable is a PORT nothing can bind. run() validates its configuration,
// builds the backend and the server, and only then listens — so with this
// port a configuration it accepts comes back as a listen error, and one it
// refuses comes back naming the variable. That is the whole startup path with
// no service left running.
const unlistenable = "not-a-port"

// localMode is a local-mode configuration that run() would otherwise accept,
// plus the given overrides.
func localMode(t *testing.T, overrides map[string]string) map[string]string {
	t.Helper()
	env := map[string]string{
		"BASE_URL":  "http://localhost:8080",
		"DATA_DIR":  t.TempDir(),
		"CACHE_DIR": t.TempDir(),
		"PORT":      unlistenable,
	}
	for k, v := range overrides {
		env[k] = v
	}
	return env
}

// RFC-0003 Stage 4 (elk-work/ark#54): ARK_API_TOKEN is optional, and "the
// service starts" is the first clause of the acceptance line. It starts with
// the token unset, and it still starts exactly as before with it set.
func TestRunStartsWithoutTheServiceToken(t *testing.T) {
	for _, c := range []struct {
		name string
		env  map[string]string
	}{
		{"no token, a signing key", map[string]string{"ARK_SIGNING_KEY": "k"}},
		{"no token, a signing key, ARK_LEGACY_TOKEN=off", map[string]string{"ARK_SIGNING_KEY": "k", "ARK_LEGACY_TOKEN": "off"}},
		{"no token, a signing key and a bootstrap token", map[string]string{"ARK_SIGNING_KEY": "k", "ARK_BOOTSTRAP_TOKEN": "b"}},
		// Before Stage 4, unchanged: the token alone, and the token narrowed.
		{"the token, no signing key", map[string]string{"ARK_API_TOKEN": "t"}},
		{"the token, readonly", map[string]string{"ARK_API_TOKEN": "t", "ARK_LEGACY_TOKEN": "readonly"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			startupEnv(t, localMode(t, c.env))
			err := run()
			if err == nil || !strings.Contains(err.Error(), "listen") {
				t.Fatalf("configuration was refused before the service listened: %v", err)
			}
		})
	}
}

// And the two configurations Stage 4 makes newly possible to get wrong both
// stop the service coming up, naming what is missing.
func TestRunRefusesWhatStage4CannotHonour(t *testing.T) {
	for _, c := range []struct {
		name  string
		env   map[string]string
		names []string
	}{
		// Local mode signs its own blob URLs, and the key used to default to
		// the token. With neither, artifacts would break while records
		// synced.
		{"local mode, no token, no signing key", map[string]string{}, []string{"ARK_SIGNING_KEY"}},
		// A dial set to a position that needs a token nobody configured. The
		// likeliest cause is a redeploy that dropped the secret; a container
		// that will not start keeps the previous revision serving.
		{"readonly with no token", map[string]string{"ARK_SIGNING_KEY": "k", "ARK_LEGACY_TOKEN": "readonly"},
			[]string{"ARK_LEGACY_TOKEN", "ARK_API_TOKEN"}},
		{"full with no token", map[string]string{"ARK_SIGNING_KEY": "k", "ARK_LEGACY_TOKEN": "full"},
			[]string{"ARK_LEGACY_TOKEN", "ARK_API_TOKEN"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			startupEnv(t, localMode(t, c.env))
			err := run()
			if err == nil || strings.Contains(err.Error(), "listen") {
				t.Fatalf("the service reached listen: %v", err)
			}
			for _, name := range c.names {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error does not name %s: %v", name, err)
				}
			}
		})
	}
}

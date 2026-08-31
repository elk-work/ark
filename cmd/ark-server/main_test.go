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

package cli

import (
	"strings"
	"testing"
)

// TestStatusReportsWhereTheTokenCameFrom: `ark login` says where it wrote the
// token, so resolution has to be equally legible — "sync is authenticated" and
// "sync is authenticated out of a plaintext file" are different states and
// only one of them is fine (spec §20). The keyring case is not reachable from
// here: TestMain opts this package out of the real OS keyring, and the keyring
// branches are exercised against a fake in internal/cloud.
func TestStatusReportsWhereTheTokenCameFrom(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var before struct {
		TokenSource string `json:"token_source"`
	}
	arkJSON(t, dir, &before, "status")
	if before.TokenSource != "" {
		t.Errorf("token_source = %q with no remote configured, want it omitted", before.TokenSource)
	}

	ark(t, dir, "remote", "set", "https://ark-status-test.invalid")

	var none struct {
		TokenSource string `json:"token_source"`
	}
	arkJSON(t, dir, &none, "status")
	if none.TokenSource != "none" {
		t.Errorf("token_source = %q with no credentials, want none", none.TokenSource)
	}

	t.Setenv("ARK_TOKEN", "from-env")
	var env struct {
		TokenSource string `json:"token_source"`
	}
	arkJSON(t, dir, &env, "status")
	if env.TokenSource != "env" {
		t.Errorf("token_source = %q with ARK_TOKEN set, want env", env.TokenSource)
	}

	// And the human rendering names the source without printing the token.
	out := ark(t, dir, "status")
	if !strings.Contains(out, "token       ARK_TOKEN") {
		t.Errorf("status output does not name the token source:\n%s", out)
	}
	if strings.Contains(out, "from-env") {
		t.Errorf("status printed the token itself:\n%s", out)
	}
}

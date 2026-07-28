package buildinfo

import (
	"strings"
	"testing"
)

func TestResolveExplicitStampWins(t *testing.T) {
	if got := Resolve("v0.1.0"); got != "v0.1.0" {
		t.Errorf("Resolve(%q) = %q, want it returned untouched", "v0.1.0", got)
	}
}

func TestResolveFallsBackToBuildInfo(t *testing.T) {
	// Built by "go test" from this repository, so there is no module
	// version: the result starts at "dev" and may carry VCS detail.
	for _, stamped := range []string{"", Dev} {
		got := Resolve(stamped)
		if !strings.HasPrefix(got, Dev) {
			t.Errorf("Resolve(%q) = %q, want it to start with %q", stamped, got, Dev)
		}
	}
}

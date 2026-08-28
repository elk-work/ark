package cli

import (
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// TestRepairPushRefusesAHealthyRepository: the gate at the surface a person
// meets it. `ark repair push` re-asserts one checkout's whole history at the
// service, so it has to be unavailable — exit 2, spec §22 — anywhere a sync
// has not first found the service serving a history this checkout is ahead of.
func TestRepairPushRefusesAHealthyRepository(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "task", "create", "-t", "local work")

	out, err := arkErr(t, dir, "repair", "push", "--confirm")
	if err == nil {
		t.Fatalf("repair push ran against a healthy repository:\n%s", out)
	}
	if code := records.ExitCode(err); code != 2 {
		t.Errorf("exit %d, want 2: %v", code, err)
	}
	if !strings.Contains(err.Error(), "history reset") {
		t.Errorf("the refusal does not name the gate: %v", err)
	}
	// Nothing about the repository moved on the way to being refused.
	out = ark(t, dir, "status")
	if strings.Contains(out, "history") {
		t.Errorf("the refused repair recorded a history line in status:\n%s", out)
	}
}

// TestRepairPushIsNotRunnableByAccident: the second gate. A bare `ark repair
// push` is a preview, and `ark repair` on its own is a group that prints help
// rather than doing anything — because the one thing this command must never
// become is something an agent runs on a loop.
func TestRepairPushIsNotRunnableByAccident(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	out := ark(t, dir, "repair")
	if !strings.Contains(out, "push") {
		t.Errorf("`ark repair` does not name its subcommand:\n%s", out)
	}
	// A group invoked with something it does not know is an input error, not
	// a help screen with a success code (spec §22).
	if _, err := arkErr(t, dir, "repair", "shove"); err == nil {
		t.Error("`ark repair shove` succeeded")
	} else if code := records.ExitCode(err); code != 2 {
		t.Errorf("unknown subcommand exits %d, want 2", code)
	}
	// And --confirm is a real flag rather than something the help invents.
	if !strings.Contains(ark(t, dir, "repair", "push", "--help"), "--confirm") {
		t.Error("`ark repair push --help` does not document --confirm")
	}
}

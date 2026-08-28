package cli

import (
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// A named agent's identity is per (agent name, delegating human), so the
// delegation is now resolved on every `--agent` invocation rather than only on
// the first one that used the name (elk-work/ark#93). These are the two ends
// of that: the ordinary run, where the repository's own human supplies it, and
// the run that cannot be attributed, where the message has to name the setting
// that supplied the value.

func TestAgentRunActsAsTheNamedAgent(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var st statusReport
	arkJSON(t, dir, &st, "--agent", "claude-code", "status")
	if st.ActorType != string(records.ActorAgent) || st.Actor != "claude-code" {
		t.Fatalf("acting identity = %s (%s), want claude-code (agent)", st.Actor, st.ActorType)
	}
}

func TestAgentRunRefusesAnUnattributableDelegation(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	t.Setenv("ARK_DELEGATED_BY", records.NewID())
	out, err := arkErr(t, dir, "--agent", "claude-code", "status")
	if err == nil {
		t.Fatalf("a delegation naming nobody was accepted:\n%s", out)
	}
	if code := records.ExitCode(err); code != 2 {
		t.Errorf("exit code = %d, want 2 (invalid input): %v", code, err)
	}
	// Which of the two settings supplied the value is the whole of what the
	// reader needs to fix it, and the store cannot know.
	if !strings.Contains(err.Error(), "ARK_DELEGATED_BY") {
		t.Errorf("error does not name the setting that supplied the delegation: %v", err)
	}
}

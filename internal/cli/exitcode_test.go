package cli

import (
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// TestInputErrorsExitTwo: every way of mistyping a command is invalid input,
// and spec §22 gives invalid input exit code 2. Cobra raises most of these
// itself and reports them as plain errors worth 1, so this is the test that
// keeps the two halves of the CLI's error surface agreeing — the half Ark
// validates and the half Cobra does.
//
// It is also the drift detector for the required-flag check in
// reportInputErrors: that check reads a Cobra annotation by name, and if Cobra
// ever renames it the check quietly matches nothing, Cobra's own validation
// runs instead, and the "missing required flag" case below drops back to 1.
func TestInputErrorsExitTwo(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "task", "create", "-t", "a task to aim the not-found cases past")

	cases := []struct {
		name string
		args []string
	}{
		{"missing required flag", []string{"task", "create"}},
		{"missing required flag on pr", []string{"pr", "create"}},
		{"missing required flag on thread", []string{"thread", "create"}},
		{"unknown flag", []string{"task", "list", "--bogus"}},
		{"bad flag value", []string{"task", "list", "--json=maybe"}},
		{"too few args", []string{"task", "view"}},
		{"too many args", []string{"task", "view", "1", "2"}},
		{"unknown command", []string{"frobnicate"}},
		{"unknown subcommand", []string{"task", "lst"}},
		{"unknown subcommand on pr", []string{"pr", "lst"}},
		{"unknown subcommand on remote", []string{"remote", "lst"}},
		// Ark's own validation, which already agreed — here so a refactor
		// cannot fix one half by breaking the other.
		{"nothing to change", []string{"task", "edit", "1"}},
		{"invalid status value", []string{"pr", "list", "-s", "banana"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := arkErr(t, dir, tc.args...)
			if err == nil {
				t.Fatalf("ark %v should have failed, got:\n%s", tc.args, out)
			}
			if code := records.ExitCode(err); code != 2 {
				t.Errorf("ark %v: exit code = %d, want 2 (invalid input): %v", tc.args, code, err)
			}
		})
	}
}

// TestGroupWithoutSubcommandStillPrintsHelp: making command groups runnable so
// they can reject an unknown subcommand must not turn `ark task` itself into
// an error. It prints help and succeeds, as it always did.
func TestGroupWithoutSubcommandStillPrintsHelp(t *testing.T) {
	dir := gitRepo(t)
	for _, group := range []string{"task", "pr", "thread", "run", "artifact", "conflict", "promotion", "remote", "skill"} {
		out, err := arkErr(t, dir, group)
		if err != nil {
			t.Errorf("ark %s: %v", group, err)
		}
		if out == "" {
			t.Errorf("ark %s printed nothing; expected the group's help", group)
		}
	}
}

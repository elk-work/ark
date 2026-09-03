package cli

import (
	"testing"

	"github.com/elk-work/ark/internal/records"
)

// A gh-shaped create is still an Ark task, so it posts the same marker and
// obeys the same repository rule as `ark task create`.
func TestGHIssueCreateCarriesTheElkParent(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var created elkTaskJSON
	arkJSON(t, dir, &created, "gh", "issue", "create", "--title", "Via gh", "--elk", "#35")
	if created.ElkRef != "#35" {
		t.Fatalf("elk_ref on gh issue create = %q, want #35", created.ElkRef)
	}
	var view elkViewJSON
	arkJSON(t, dir, &view, "task", "view", created.ID)
	if view.ElkRef != "#35" {
		t.Fatalf("view elk_ref = %q, want #35", view.ElkRef)
	}
	if len(view.Comments) != 1 || view.Comments[0].Body != "elk-parent: #35" {
		t.Fatalf("comments after gh issue create = %+v, want one marker", view.Comments)
	}
}

func TestGHIssueCreateObeysRequireElkParent(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "config", "set", "require-elk-parent", "true")

	var before []elkTaskJSON
	arkJSON(t, dir, &before, "task", "list", "--status", "all")

	out, err := arkErr(t, dir, "gh", "issue", "create", "--title", "Refused by gh")
	if err == nil || records.ExitCode(err) != 2 {
		t.Fatalf("gh issue create without --elk: err = %v (exit %d)\n%s", err, records.ExitCode(err), out)
	}

	// Refused before anything was written: no half-made task behind it.
	var after []elkTaskJSON
	arkJSON(t, dir, &after, "task", "list", "--status", "all")
	if len(after) != len(before) {
		t.Fatalf("a refused gh issue create wrote %d task(s): %+v", len(after)-len(before), after)
	}

	var created elkTaskJSON
	arkJSON(t, dir, &created, "gh", "issue", "create", "--title", "Allowed by gh", "--elk", "elk:36")
	if created.ElkRef != "elk:36" {
		t.Fatalf("gh issue create with --elk: elk_ref = %q", created.ElkRef)
	}
}

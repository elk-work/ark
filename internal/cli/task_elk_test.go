package cli

import (
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
)

type elkTaskJSON struct {
	ID     string `json:"id"`
	Number int64  `json:"number"`
	ElkRef string `json:"elk_ref"`
}

type elkViewJSON struct {
	ID       string `json:"id"`
	ElkRef   string `json:"elk_ref"`
	Comments []struct {
		Body string `json:"body"`
	} `json:"comments"`
}

func TestTaskCreateWithElkPostsTheMarker(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var created elkTaskJSON
	arkJSON(t, dir, &created, "task", "create", "-t", "Wire the thing", "--elk", "#35")
	if created.ElkRef != "#35" {
		t.Fatalf("elk_ref on create = %q, want #35", created.ElkRef)
	}

	var view elkViewJSON
	arkJSON(t, dir, &view, "task", "view", created.ID)
	if len(view.Comments) != 1 || view.Comments[0].Body != "elk-parent: #35" {
		t.Fatalf("comments after create = %+v, want one marker", view.Comments)
	}

	out := ark(t, dir, "task", "create", "-t", "Plain", "--elk", "elk:36")
	if !strings.Contains(out, "Elk parent: elk:36") {
		t.Fatalf("human output should name the parent: %s", out)
	}
}

func TestTaskCreateWithoutElkIsFineUntilTheRepositoryRequiresIt(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")

	var created elkTaskJSON
	arkJSON(t, dir, &created, "task", "create", "-t", "No parent yet")
	if created.ElkRef != "" {
		t.Fatalf("elk_ref without --elk = %q", created.ElkRef)
	}
	var view elkViewJSON
	arkJSON(t, dir, &view, "task", "view", created.ID)
	if len(view.Comments) != 0 {
		t.Fatalf("no marker should be posted without --elk, got %+v", view.Comments)
	}

	ark(t, dir, "config", "set", "require-elk-parent", "true")
	out, err := arkErr(t, dir, "task", "create", "-t", "Refused")
	if err == nil || records.ExitCode(err) != 2 {
		t.Fatalf("create without --elk under require-elk-parent: err = %v (exit %d)\n%s", err, records.ExitCode(err), out)
	}
	msg := err.Error()
	for _, want := range []string{"--elk", "push_to_work_record"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q should name %s", msg, want)
		}
	}
	arkJSON(t, dir, &created, "task", "create", "-t", "Allowed", "--elk", "35")
	if created.ElkRef != "35" {
		t.Fatalf("create with --elk under the rule: elk_ref = %q", created.ElkRef)
	}
	// An empty --elk is a mistyped flag, not a parent.
	if _, err := arkErr(t, dir, "task", "create", "-t", "Blank", "--elk", "  "); err == nil || records.ExitCode(err) != 2 {
		t.Errorf("blank --elk: err = %v, want exit 2", err)
	}
}

func TestTaskViewShowsTheLatestParent(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	var created elkTaskJSON
	arkJSON(t, dir, &created, "task", "create", "-t", "Step", "--elk", "#35")

	var view elkViewJSON
	arkJSON(t, dir, &view, "task", "view", created.ID)
	if view.ElkRef != "#35" {
		t.Fatalf("view elk_ref = %q, want #35", view.ElkRef)
	}
	out := ark(t, dir, "task", "view", created.ID)
	if !strings.Contains(out, "elk       #35") {
		t.Fatalf("human view should print the parent line:\n%s", out)
	}

	// A later marker supersedes; a plain comment does not.
	ark(t, dir, "task", "comment", created.ID, "-b", "elk-parent: #36")
	ark(t, dir, "task", "comment", created.ID, "-b", "unrelated note")
	arkJSON(t, dir, &view, "task", "view", created.ID)
	if view.ElkRef != "#36" {
		t.Fatalf("view elk_ref after a second marker = %q, want #36", view.ElkRef)
	}

	// No marker, no line, no field.
	arkJSON(t, dir, &created, "task", "create", "-t", "Orphan")
	out = ark(t, dir, "task", "view", created.ID)
	if strings.Contains(out, "elk       ") {
		t.Fatalf("an unparented task should print no elk line:\n%s", out)
	}
}

func TestTaskEditElkPostsANewMarkerAndTheLatestWins(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	var created elkTaskJSON
	arkJSON(t, dir, &created, "task", "create", "-t", "Step", "--elk", "#35")

	var edited elkTaskJSON
	arkJSON(t, dir, &edited, "task", "edit", created.ID, "--elk", "#36")
	if edited.ElkRef != "#36" {
		t.Fatalf("edit elk_ref = %q, want #36", edited.ElkRef)
	}
	var view elkViewJSON
	arkJSON(t, dir, &view, "task", "view", created.ID)
	if view.ElkRef != "#36" || len(view.Comments) != 2 {
		t.Fatalf("after edit: elk_ref=%q comments=%d, want #36 and 2 (the old marker is kept, not edited)", view.ElkRef, len(view.Comments))
	}

	// --elk together with an ordinary edit applies both.
	arkJSON(t, dir, &edited, "task", "edit", created.ID, "-t", "Renamed", "--elk", "37")
	arkJSON(t, dir, &view, "task", "view", created.ID)
	if view.ElkRef != "37" {
		t.Fatalf("elk_ref after combined edit = %q", view.ElkRef)
	}
	out := ark(t, dir, "task", "view", created.ID)
	if !strings.Contains(out, "Renamed") {
		t.Fatalf("title edit lost:\n%s", out)
	}

	// Nothing at all is still nothing to change.
	if _, err := arkErr(t, dir, "task", "edit", created.ID); err == nil || records.ExitCode(err) != 2 {
		t.Errorf("empty edit: err = %v, want exit 2", err)
	}
}

func TestTaskListFiltersByElkParent(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	var a, b, c elkTaskJSON
	arkJSON(t, dir, &a, "task", "create", "-t", "A", "--elk", "#35")
	arkJSON(t, dir, &b, "task", "create", "-t", "B", "--elk", "elk:35")
	arkJSON(t, dir, &c, "task", "create", "-t", "C", "--elk", "#36")
	arkJSON(t, dir, &c, "task", "create", "-t", "D")
	// B moved on; the latest marker is what the filter reads.
	ark(t, dir, "task", "edit", b.ID, "--elk", "#99")

	var listed []elkTaskJSON
	arkJSON(t, dir, &listed, "task", "list", "--elk", "35")
	if len(listed) != 1 || listed[0].ID != a.ID {
		t.Fatalf("list --elk 35 = %+v, want only A", listed)
	}
	arkJSON(t, dir, &listed, "task", "list", "--elk", "#99")
	if len(listed) != 1 || listed[0].ID != b.ID {
		t.Fatalf("list --elk #99 = %+v, want only B", listed)
	}
	arkJSON(t, dir, &listed, "task", "list", "--elk", "elk:36")
	if len(listed) != 1 || listed[0].Number != 3 {
		t.Fatalf("list --elk elk:36 = %+v, want only C", listed)
	}
	arkJSON(t, dir, &listed, "task", "list", "--elk", "#1000")
	if len(listed) != 0 {
		t.Fatalf("list --elk #1000 = %+v, want none", listed)
	}
	out := ark(t, dir, "task", "list", "--elk", "#1000")
	if !strings.Contains(out, "no tasks") {
		t.Fatalf("empty filter should say so: %s", out)
	}
}

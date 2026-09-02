package cli

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

// The Elk parent of a task is a comment whose first line reads
// `elk-parent: <ref>`. It is a marker, not a field: Ark stores nothing new,
// the sync service needs no route for it, and Elk's puller reads the latest
// one off the task's comments. The ref is whatever the person typed (#35, 35,
// elk:35, or the Elk action id); resolving it is Elk's job, never Ark's.
//
// This shape is a contract with Elk's connector (scout: gh-connector/ark_pull.ts,
// which reads it off the comment stream). Changing it changes both sides. The
// client-side producer in internal/workrecord (ark elk push --comments) does not
// read it; the puller is the path that carries the parent to Elk.
const elkParentPrefix = "elk-parent:"

// elkParentMarker is the body of the comment `--elk` posts.
func elkParentMarker(ref string) string {
	return elkParentPrefix + " " + ref
}

// parseElkParent returns the ref a marker comment names. Only the first line
// counts; anything after it is ignored, and a marker that is not on the first
// line is not a marker.
func parseElkParent(body string) (string, bool) {
	first := body
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		first = body[:i]
	}
	first = strings.TrimSpace(first)
	if len(first) < len(elkParentPrefix) || !strings.EqualFold(first[:len(elkParentPrefix)], elkParentPrefix) {
		return "", false
	}
	ref := strings.TrimSpace(first[len(elkParentPrefix):])
	if ref == "" {
		return "", false
	}
	return ref, true
}

// latestElkParent is the task's current parent: the last marker in creation
// order (ListComments orders by ULID). Empty when no comment is a marker.
func latestElkParent(comments []*store.Comment) string {
	ref := ""
	for _, c := range comments {
		if r, ok := parseElkParent(c.Body); ok {
			ref = r
		}
	}
	return ref
}

// elkRefKey folds the accepted grammars together for COMPARISON only, so
// `ark task list --elk 35` finds a task whose marker says `#35` or `elk:#35`.
// The stored marker keeps the person's own spelling.
func elkRefKey(ref string) string {
	r := strings.TrimSpace(ref)
	if len(r) >= 4 && strings.EqualFold(r[:4], "elk:") {
		r = strings.TrimSpace(r[4:])
	}
	r = strings.TrimPrefix(r, "#")
	return strings.ToLower(r)
}

// validElkRef is what --elk accepts: one non-empty line, kept verbatim.
func validElkRef(ref string) (string, error) {
	r := strings.TrimSpace(ref)
	if r == "" {
		return "", records.Validationf("--elk needs a reference: #35, 35, elk:35, or the Elk action id")
	}
	if strings.ContainsAny(r, "\r\n") {
		return "", records.Validationf("--elk takes a single line")
	}
	return r, nil
}

// filterByElkParent keeps the tasks whose current parent is `want`. Markers
// are read for every task in one query and folded newest-last, so the filter
// costs one round trip however many tasks the repository holds.
func filterByElkParent(ctx context.Context, s *store.Store, tasks []*store.Task, want string) ([]*store.Task, error) {
	comments, err := s.ListCommentsOfType(ctx, "task")
	if err != nil {
		return nil, err
	}
	parent := map[string]string{}
	for _, c := range comments {
		if ref, ok := parseElkParent(c.Body); ok {
			parent[c.ParentID] = ref
		}
	}
	key := elkRefKey(want)
	var out []*store.Task
	for _, t := range tasks {
		if ref, ok := parent[t.ID]; ok && elkRefKey(ref) == key {
			out = append(out, t)
		}
	}
	return out, nil
}

// elkFlagRef reads and validates `--elk` off a command. Empty when the flag
// was not given.
func elkFlagRef(cmd *cobra.Command) (string, error) {
	if !cmd.Flags().Changed("elk") {
		return "", nil
	}
	raw, _ := cmd.Flags().GetString("elk")
	return validElkRef(raw)
}

// requireElkParent is this repository's own rule (ark config set
// require-elk-parent true). It is checked before anything is written, so a
// refused create leaves no record behind.
func requireElkParent(a *app.Context, elkRef string) error {
	if elkRef != "" || !a.Config.RequireElkParent {
		return nil
	}
	return records.Validationf("this repository requires an Elk parent for every task: " +
		"pass --elk <ref> (#35, 35, elk:35, or the Elk action id), " +
		"or have Elk create the task with push_to_work_record")
}

// postElkParent posts the marker that re-parents a task. The task is already
// written by the time this runs, so a failure here is a partial write (exit 7)
// naming the command that repairs it.
func postElkParent(ctx context.Context, a *app.Context, t *store.Task, elkRef string) error {
	if _, err := a.Store.AddComment(ctx, "task", t.ID, elkParentMarker(elkRef), ""); err != nil {
		return records.Partialf(
			"task #%d (%s) exists, but posting its elk-parent marker failed: %v; "+
				"run `ark task edit %d --elk %s` to repair",
			t.Number, t.ID, err, t.Number, elkRef)
	}
	return nil
}

// createTaskWithElk is what `ark task create` and `ark gh issue create` both
// do: refuse when the repository requires a parent and none was named, write
// the task, then post its marker. One implementation so the two commands
// cannot drift apart.
func createTaskWithElk(ctx context.Context, a *app.Context, title, body, elkRef string) (*store.Task, error) {
	if err := requireElkParent(a, elkRef); err != nil {
		return nil, err
	}
	t, err := a.Store.CreateTask(ctx, title, body)
	if err != nil {
		return nil, err
	}
	if elkRef != "" {
		if err := postElkParent(ctx, a, t, elkRef); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// currentElkParent is the task's parent as it stands now: the latest marker on
// the task, read back after any write. `create`, `edit` and `view` all report
// the same thing by reading it the same way.
func currentElkParent(ctx context.Context, a *app.Context, taskID string) (string, error) {
	comments, err := a.Store.ListComments(ctx, "task", taskID)
	if err != nil {
		return "", err
	}
	return latestElkParent(comments), nil
}

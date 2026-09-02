package cli

import (
	"strings"

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

package review

import (
	"os"
	"path/filepath"
	"strings"
)

// The review cursor is a timestamp in a plain file under .ark/, and that is
// deliberately all it is.
//
// "Since the last review" needs a watermark somewhere, and the one place it
// must not go is the record model: it is not work, nobody else needs it, and
// a table for it would be the first plank of a primitive v1-spec §2 excludes.
// A file is not a record — it is not synced, not migrated, not in a mutation,
// and deleting it costs nothing but a wider next review. Same class as
// .ark/elk-push.log.
const cursorFile = "review-cursor"

// ReadCursor returns the timestamp of the last review, or "" if there has
// never been one — which correctly means "everything".
func ReadCursor(arkDir string) string {
	b, err := os.ReadFile(filepath.Join(arkDir, cursorFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// WriteCursor records that a review covered everything up to ts. A failure
// is not worth failing a read command over: the next review simply repeats
// itself, which is the harmless direction to be wrong in.
func WriteCursor(arkDir, ts string) error {
	if arkDir == "" || strings.TrimSpace(ts) == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(arkDir, cursorFile), []byte(ts+"\n"), 0o644)
}

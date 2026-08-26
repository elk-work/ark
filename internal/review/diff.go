package review

import (
	"context"
	"strconv"
	"strings"
	"unicode"

	"github.com/elk-work/ark/internal/git"
)

// Diff is what a run did to the source, between the two SHAs its record
// already carries. Git holds the change; this only reads it back.
type Diff struct {
	BaseCommitSHA   string `json:"base_commit_sha,omitempty"`
	ResultCommitSHA string `json:"result_commit_sha,omitempty"`
	// Unavailable says why there is no diff, when there is none. A missing
	// commit and an empty change are different answers and the page says
	// which — a record can outlive the objects it points at.
	Unavailable string       `json:"unavailable,omitempty"`
	Insertions  int          `json:"insertions"`
	Deletions   int          `json:"deletions"`
	Files       []DiffFile   `json:"files"`
	Commits     []git.Commit `json:"commits,omitempty"`
	Truncated   bool         `json:"truncated,omitempty"`
}

// DiffFile is one path's change.
type DiffFile struct {
	Path       string     `json:"path"`
	OldPath    string     `json:"old_path,omitempty"`
	Insertions int        `json:"insertions"`
	Deletions  int        `json:"deletions"`
	Binary     bool       `json:"binary,omitempty"`
	Truncated  bool       `json:"truncated,omitempty"`
	Hunks      []DiffHunk `json:"hunks,omitempty"`
}

// DiffHunk is one contiguous region of a file's change.
type DiffHunk struct {
	Header string     `json:"header"`
	Lines  []DiffLine `json:"lines"`
}

// Line kinds.
const (
	LineContext = "context"
	LineAdd     = "add"
	LineDel     = "del"
)

// DiffLine is one rendered line. SpanStart and SpanEnd bound the run of
// bytes inside Text that actually differs from the line it replaced, so a
// renderer can saturate only that and leave the rest pale. Zero-zero means
// no usable span: the whole line is the change.
type DiffLine struct {
	Kind      string `json:"kind"`
	Text      string `json:"text"`
	OldNum    int    `json:"old_num,omitempty"`
	NewNum    int    `json:"new_num,omitempty"`
	SpanStart int    `json:"span_start,omitempty"`
	SpanEnd   int    `json:"span_end,omitempty"`
}

// Caps. A review page is meant to be read; a 40,000-line diff is not, and
// an artifact of one is worse. Truncation is reported rather than silent.
const (
	maxFiles        = 80
	maxLinesPerFile = 500
	diffContext     = 3
)

// collectDiff reads the change a run produced. Every failure mode answers
// with a reason rather than an empty diff.
func collectDiff(ctx context.Context, repo *git.Repo, base, result string) *Diff {
	d := &Diff{BaseCommitSHA: base, ResultCommitSHA: result, Files: []DiffFile{}}
	// What the record itself says comes first: those answers do not depend
	// on having a repository to look in, and they are the more specific
	// explanation when both are true.
	switch {
	case base == "" && result == "":
		d.Unavailable = "the run recorded no commits"
		return d
	case base == "":
		d.Unavailable = "the run recorded no base commit, so there is nothing to compare against"
		return d
	case result == "":
		d.Unavailable = "the run recorded no result commit — it may have produced no source change"
		return d
	case base == result:
		d.Unavailable = "the run ended on the commit it started from: no source change"
		return d
	case repo == nil:
		d.Unavailable = "no Git repository available to read the change from"
		return d
	case !repo.HasCommit(ctx, base):
		d.Unavailable = "base commit " + short(base) + " is not in this repository (not the same as nothing changed)"
		return d
	case !repo.HasCommit(ctx, result):
		d.Unavailable = "result commit " + short(result) + " is not in this repository (not the same as nothing changed)"
		return d
	}

	stats, err := repo.DiffStat(ctx, base, result)
	if err != nil {
		d.Unavailable = "could not read the diff: " + err.Error()
		return d
	}
	if len(stats) == 0 {
		d.Unavailable = "the two commits have identical trees: no source change"
		return d
	}
	if commits, err := repo.CommitsBetween(ctx, base, result); err == nil {
		d.Commits = commits
	}

	patch, err := repo.DiffUnified(ctx, base, result, diffContext)
	if err != nil {
		// The stats survived, so report those rather than nothing.
		d.Unavailable = "could not read the patch text: " + err.Error()
	}
	hunks := parseUnified(patch)

	for i, st := range stats {
		if i >= maxFiles {
			d.Truncated = true
			break
		}
		f := DiffFile{Path: st.Path, OldPath: st.OldPath, Binary: st.Binary}
		if !st.Binary {
			f.Insertions, f.Deletions = st.Insertions, st.Deletions
			d.Insertions += st.Insertions
			d.Deletions += st.Deletions
		}
		f.Hunks, f.Truncated = capHunks(hunks[st.Path])
		for hi := range f.Hunks {
			markWordSpans(f.Hunks[hi].Lines)
		}
		d.Files = append(d.Files, f)
	}
	return d
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// capHunks trims a file's rendered lines to the per-file cap.
func capHunks(in []DiffHunk) ([]DiffHunk, bool) {
	total := 0
	var out []DiffHunk
	for _, h := range in {
		if total >= maxLinesPerFile {
			return out, true
		}
		if total+len(h.Lines) > maxLinesPerFile {
			h.Lines = h.Lines[:maxLinesPerFile-total]
			out = append(out, h)
			return out, true
		}
		total += len(h.Lines)
		out = append(out, h)
	}
	return out, false
}

// parseUnified turns `git diff --unified=N` output into hunks per path. The
// new path is the key; a rename's old path travels on the FileStat.
func parseUnified(patch string) map[string][]DiffHunk {
	out := map[string][]DiffHunk{}
	var path string
	var hunk *DiffHunk
	var oldNum, newNum int

	flush := func() {
		if hunk != nil && path != "" {
			out[path] = append(out[path], *hunk)
		}
		hunk = nil
	}

	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			path = ""
		// The file headers are only headers outside a hunk. Inside one, an
		// added line reading "++ x" arrives as "+++ x" and a deleted "-- x"
		// as "--- x" — diffing a patch is the case that finds this.
		case hunk == nil && strings.HasPrefix(line, "+++ "):
			p := strings.TrimPrefix(line, "+++ ")
			if p == "/dev/null" {
				// A deletion: the surviving name is on the --- side.
				continue
			}
			path = trimDiffPath(p)
		case hunk == nil && strings.HasPrefix(line, "--- "):
			p := strings.TrimPrefix(line, "--- ")
			if p != "/dev/null" && path == "" {
				path = trimDiffPath(p)
			}
		case strings.HasPrefix(line, "@@"):
			flush()
			oldNum, newNum = parseHunkHeader(line)
			hunk = &DiffHunk{Header: line}
		case hunk == nil:
			// Header noise between files (index, mode, similarity).
		case strings.HasPrefix(line, "+"):
			hunk.Lines = append(hunk.Lines, DiffLine{Kind: LineAdd, Text: line[1:], NewNum: newNum})
			newNum++
		case strings.HasPrefix(line, "-"):
			hunk.Lines = append(hunk.Lines, DiffLine{Kind: LineDel, Text: line[1:], OldNum: oldNum})
			oldNum++
		case strings.HasPrefix(line, " "):
			hunk.Lines = append(hunk.Lines, DiffLine{Kind: LineContext, Text: line[1:],
				OldNum: oldNum, NewNum: newNum})
			oldNum++
			newNum++
		case line == "":
			// Trailing newline of the patch, or an empty context line that
			// Git emitted without its leading space.
		case strings.HasPrefix(line, `\`):
			// "\ No newline at end of file".
		}
	}
	flush()
	return out
}

// trimDiffPath strips git's a/ or b/ prefix and any trailing tab metadata.
func trimDiffPath(p string) string {
	if i := strings.IndexByte(p, '\t'); i >= 0 {
		p = p[:i]
	}
	if len(p) > 2 && (strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/")) {
		return p[2:]
	}
	return p
}

// parseHunkHeader reads the starting line numbers from "@@ -a,b +c,d @@".
func parseHunkHeader(line string) (oldNum, newNum int) {
	oldNum, newNum = 1, 1
	fields := strings.Fields(line)
	for _, f := range fields {
		if len(f) < 2 {
			continue
		}
		n := f[1:]
		if i := strings.IndexByte(n, ','); i >= 0 {
			n = n[:i]
		}
		v, err := strconv.Atoi(n)
		if err != nil {
			continue
		}
		switch f[0] {
		case '-':
			oldNum = v
		case '+':
			newNum = v
		}
	}
	return oldNum, newNum
}

// markWordSpans pairs each run of deleted lines with the run of added lines
// immediately following it and, where the runs are the same length, marks
// the bytes that actually differ on each pair. Colour is never the only
// signal, so a missed span costs legibility and nothing else — which is why
// the cheap heuristic is the right one here.
func markWordSpans(lines []DiffLine) {
	for i := 0; i < len(lines); {
		if lines[i].Kind != LineDel {
			i++
			continue
		}
		delStart := i
		for i < len(lines) && lines[i].Kind == LineDel {
			i++
		}
		addStart := i
		for i < len(lines) && lines[i].Kind == LineAdd {
			i++
		}
		dels, adds := lines[delStart:addStart], lines[addStart:i]
		if len(dels) == 0 || len(dels) != len(adds) {
			continue
		}
		for j := range dels {
			ds, de, as, ae := changedSpan(dels[j].Text, adds[j].Text)
			dels[j].SpanStart, dels[j].SpanEnd = ds, de
			adds[j].SpanStart, adds[j].SpanEnd = as, ae
		}
	}
}

// changedSpan returns the byte range in each of two lines that differs,
// snapped outward to word boundaries. It returns zeroes when the whole line
// changed, or when the pair is unrelated enough that a span would mislead.
func changedSpan(del, add string) (ds, de, as, ae int) {
	if del == "" || add == "" || del == add {
		return 0, 0, 0, 0
	}
	prefix := 0
	for prefix < len(del) && prefix < len(add) && del[prefix] == add[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(del)-prefix && suffix < len(add)-prefix &&
		del[len(del)-1-suffix] == add[len(add)-1-suffix] {
		suffix++
	}
	prefix = snapBack(del, prefix)
	if p := snapBack(add, prefix); p < prefix {
		prefix = p
	}
	ds, de = prefix, len(del)-snapForward(del, suffix)
	as, ae = prefix, len(add)-snapForward(add, suffix)
	if ds >= de || as >= ae {
		return 0, 0, 0, 0
	}
	// A span covering essentially the whole line teaches nothing; the line
	// background already says it changed.
	if de-ds >= len(del)-1 && ae-as >= len(add)-1 {
		return 0, 0, 0, 0
	}
	return ds, de, as, ae
}

// snapBack moves an offset back to the start of the word it lands inside.
func snapBack(s string, i int) int {
	if i > len(s) {
		i = len(s)
	}
	for i > 0 && isWordByte(s[i-1]) && i < len(s) && isWordByte(s[i]) {
		i--
	}
	return i
}

// snapForward shortens a common suffix until it begins at a word boundary,
// so a change inside a token highlights the whole token rather than the two
// characters that happen to differ. Shortening the suffix grows the span —
// growing it would eat the change instead.
func snapForward(s string, suffix int) int {
	for suffix > 0 {
		i := len(s) - suffix
		if i > 0 && isWordByte(s[i-1]) && i < len(s) && isWordByte(s[i]) {
			suffix--
			continue
		}
		break
	}
	return suffix
}

func isWordByte(b byte) bool {
	r := rune(b)
	return b >= 0x80 || unicode.IsLetter(r) || unicode.IsDigit(r) || b == '_'
}

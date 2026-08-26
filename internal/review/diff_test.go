package review

import (
	"strings"
	"testing"
)

func TestParseUnified(t *testing.T) {
	// Built line by line because a blank *context* line in real git output
	// is a single space, and a literal here would lose it to any editor
	// that trims trailing whitespace.
	patch := strings.Join([]string{
		"diff --git a/app.go b/app.go",
		"index 1111111..2222222 100644",
		"--- a/app.go",
		"+++ b/app.go",
		"@@ -1,5 +1,6 @@",
		" package app",
		" ",
		"-func Serve() {}",
		`+import "os"`,
		"+",
		"+func Serve(addr string) {}",
		" ",
		" // end",
		"diff --git a/gone.txt b/gone.txt",
		"deleted file mode 100644",
		"index 3333333..0000000",
		"--- a/gone.txt",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"-one",
		"-two",
		"",
	}, "\n")
	hunks := parseUnified(patch)
	if len(hunks) != 2 {
		t.Fatalf("parsed %d files, want 2: %v", len(hunks), keysOf(hunks))
	}

	app := hunks["app.go"]
	if len(app) != 1 {
		t.Fatalf("app.go: %d hunks, want 1", len(app))
	}
	var adds, dels, ctxLines int
	for _, l := range app[0].Lines {
		switch l.Kind {
		case LineAdd:
			adds++
		case LineDel:
			dels++
		case LineContext:
			ctxLines++
		}
	}
	if adds != 3 || dels != 1 || ctxLines != 4 {
		t.Errorf("app.go lines: %d add, %d del, %d context; want 3/1/4", adds, dels, ctxLines)
	}
	// Numbering restarts from the hunk header, and the two sides advance
	// independently — the whole reason the header is parsed at all.
	first := app[0].Lines[0]
	if first.OldNum != 1 || first.NewNum != 1 {
		t.Errorf("first line numbered %d/%d, want 1/1", first.OldNum, first.NewNum)
	}

	// A deletion keeps its name from the --- side, since +++ is /dev/null.
	if len(hunks["gone.txt"]) != 1 {
		t.Errorf("deleted file lost its name: %v", keysOf(hunks))
	}
}

// A patch of a patch is the case that catches naive header detection: an
// added line reading "++ x" arrives on the wire as "+++ x".
func TestParseUnifiedDoesNotTreatContentAsAHeader(t *testing.T) {
	patch := `diff --git a/p.diff b/p.diff
--- a/p.diff
+++ b/p.diff
@@ -1,2 +1,2 @@
-- old line
++ new line
`
	hunks := parseUnified(patch)
	lines := hunks["p.diff"]
	if len(lines) != 1 || len(lines[0].Lines) != 2 {
		t.Fatalf("parsed %+v, want one hunk of two lines", hunks)
	}
	if lines[0].Lines[0].Text != "- old line" || lines[0].Lines[1].Text != "+ new line" {
		t.Errorf("content mangled: %q then %q",
			lines[0].Lines[0].Text, lines[0].Lines[1].Text)
	}
}

func keysOf(m map[string][]DiffHunk) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestChangedSpan(t *testing.T) {
	cases := []struct {
		name          string
		del, add      string
		wantDel, want string // the marked substring on each side, "" for none
	}{
		{"one word in the middle",
			"timeout is 30 seconds", "timeout is 45 seconds", "30", "45"},
		{"a suffix change",
			"backoff from 100ms", "backoff from 250ms", "100ms", "250ms"},
		{"identical lines have no span", "same", "same", "", ""},
		{"an empty side has no span", "", "added", "", ""},
		{"a wholly different line is not worth a span",
			"alpha", "omega beta", "", ""},
		{"the span snaps out to whole words, not mid-token",
			"call(alpha)", "call(alphabet)", "alpha", "alphabet"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ds, de, as, ae := changedSpan(c.del, c.add)
			gotDel, gotAdd := "", ""
			if ds < de {
				gotDel = c.del[ds:de]
			}
			if as < ae {
				gotAdd = c.add[as:ae]
			}
			if gotDel != c.wantDel || gotAdd != c.want {
				t.Errorf("spans %q/%q, want %q/%q", gotDel, gotAdd, c.wantDel, c.want)
			}
		})
	}
}

// Spans are only marked where a run of deletions pairs one-for-one with the
// run of additions that follows. An unequal pairing would guess.
func TestMarkWordSpansOnlyPairsEqualRuns(t *testing.T) {
	equal := []DiffLine{
		{Kind: LineDel, Text: "timeout is 30 seconds"},
		{Kind: LineAdd, Text: "timeout is 45 seconds"},
	}
	markWordSpans(equal)
	if equal[0].SpanEnd == 0 || equal[1].SpanEnd == 0 {
		t.Errorf("equal runs were not marked: %+v", equal)
	}

	unequal := []DiffLine{
		{Kind: LineDel, Text: "timeout is 30 seconds"},
		{Kind: LineAdd, Text: "timeout is 45 seconds"},
		{Kind: LineAdd, Text: "retries are 5"},
	}
	markWordSpans(unequal)
	for i, l := range unequal {
		if l.SpanEnd != 0 {
			t.Errorf("line %d was marked from an unequal pairing: %+v", i, l)
		}
	}
}

func TestCapHunks(t *testing.T) {
	big := DiffHunk{Header: "@@"}
	for i := 0; i < maxLinesPerFile+40; i++ {
		big.Lines = append(big.Lines, DiffLine{Kind: LineContext, Text: "x"})
	}
	out, truncated := capHunks([]DiffHunk{big})
	if !truncated {
		t.Error("an over-long file was not reported as truncated")
	}
	total := 0
	for _, h := range out {
		total += len(h.Lines)
	}
	if total != maxLinesPerFile {
		t.Errorf("kept %d lines, want the cap of %d", total, maxLinesPerFile)
	}
}

func TestCollectDiffSaysWhyItHasNothing(t *testing.T) {
	cases := []struct {
		name, base, result string
		want               string
	}{
		{"no commits at all", "", "", "recorded no commits"},
		{"no base", "", "abc", "no base commit"},
		{"no result", "abc", "", "no result commit"},
		{"unmoved", "abc", "abc", "commit it started from"},
		{"no repository to look in", "abc", "def", "no Git repository"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Nil repository throughout: what the record itself says has to
			// be answerable without one, and has to win when both are true.
			d := collectDiff(t.Context(), nil, c.base, c.result)
			if !strings.Contains(d.Unavailable, c.want) {
				t.Errorf("reason %q, want it to mention %q", d.Unavailable, c.want)
			}
			if len(d.Files) != 0 {
				t.Errorf("files reported alongside %q: %+v", d.Unavailable, d.Files)
			}
		})
	}
}

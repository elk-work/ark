package review

import (
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/store"
)

func renderOne(t *testing.T, r *Run) string {
	t.Helper()
	classify(r, now)
	out, err := RenderHTML(&Review{
		Repository: Repository{ID: "01REPO", Name: "demo", Branch: "main"},
		Scope:      Scope{Kind: ScopeRun, RunID: r.ID},
		Runs:       []*Run{r},
		Totals:     totals([]*Run{r}),
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return string(out)
}

// The page has to survive being an artifact: one file, opened years later,
// possibly with no network. Nothing in it may reach outside itself.
func TestRenderedPageIsSelfContained(t *testing.T) {
	html := renderOne(t, run())
	for _, forbidden := range []string{
		"http://", "https://", "//fonts.", "<link", "<img", "src=", "@import",
	} {
		if strings.Contains(html, forbidden) {
			t.Errorf("page reaches outside itself: found %q", forbidden)
		}
	}
	if !strings.Contains(html, "<style>") {
		t.Error("stylesheet is not inlined")
	}
}

// Every colour in the page comes from the six palette tokens or the four
// diff colours, and dark is a remap of the same names rather than a fork.
func TestPaletteIsSixTokensRemapped(t *testing.T) {
	html := renderOne(t, run())
	for _, token := range []string{"--bg", "--fg", "--muted", "--line", "--accent", "--code-bg"} {
		if strings.Count(html, token+":") < 3 {
			t.Errorf("%s is not defined in light, dark, and the explicit toggle", token)
		}
	}
	if !strings.Contains(html, "prefers-color-scheme: dark") {
		t.Error("no dark theme")
	}
	if !strings.Contains(html, `[data-theme="dark"]`) || !strings.Contains(html, `[data-theme="light"]`) {
		t.Error("the toggle does not win in both directions")
	}
}

// Colour is never the only signal.
func TestDiffIsRedundantlyEncoded(t *testing.T) {
	r := run()
	r.Diff = &Diff{
		Insertions: 1, Deletions: 1,
		Files: []DiffFile{{Path: "a.txt", Insertions: 1, Deletions: 1, Hunks: []DiffHunk{{
			Header: "@@ -1,1 +1,1 @@",
			Lines: []DiffLine{
				{Kind: LineDel, Text: "old & <dangerous>", OldNum: 1},
				{Kind: LineAdd, Text: "new & <dangerous>", NewNum: 1},
			},
		}}}},
	}
	html := renderOne(t, r)

	// html/template writes + as the numeric entity; the browser shows a +.
	if !strings.Contains(html, `class="mark" aria-hidden="true">&#43;`) {
		t.Error("added lines carry no + glyph")
	}
	if !strings.Contains(html, `class="mark" aria-hidden="true">−`) {
		t.Error("deleted lines carry no − glyph")
	}
	if glyph(LineAdd) != "+" || glyph(LineDel) != "−" || glyph(LineContext) != " " {
		t.Errorf("glyphs: %q %q %q", glyph(LineAdd), glyph(LineDel), glyph(LineContext))
	}
	if !strings.Contains(html, "box-shadow: inset 2px 0 0 var(--add-ink)") {
		t.Error("added lines carry no stripe")
	}
	// And the content is escaped, not interpreted.
	if strings.Contains(html, "<dangerous>") {
		t.Error("diff content was not escaped")
	}
	if !strings.Contains(html, "old &amp; &lt;dangerous&gt;") {
		t.Errorf("escaped text missing from the page")
	}
}

func TestLineHTMLMarksTheSpan(t *testing.T) {
	got := string(lineHTML(DiffLine{Text: "timeout is 30 seconds", SpanStart: 11, SpanEnd: 13}))
	want := "timeout is <mark>30</mark> seconds"
	if got != want {
		t.Errorf("lineHTML = %q, want %q", got, want)
	}
	// A span that would run off the end is ignored rather than panicking.
	if strings.Contains(string(lineHTML(DiffLine{Text: "ab", SpanStart: 1, SpanEnd: 99})), "<mark>") {
		t.Error("an out-of-range span was applied")
	}
}

// Tool calls collapse together; a subagent turn never does; and an error
// inside a closed strip is announced on the summary, because otherwise the
// closed state hides the one thing worth opening it for.
func TestToolStripsCollapseAndSurfaceErrors(t *testing.T) {
	strips := buildStrips([]*store.Message{
		{Role: "user", Body: "do it"},
		{Role: "tool", Body: "Read app.go"},
		{Role: "tool", Body: "Bash: go build\nerror: undefined: os"},
		{Role: "tool", Body: "Edit app.go"},
		{Role: "agent", Body: "Build is green."},
		{Role: "tool", Body: "Bash: go test (ok)"},
	})

	if len(strips) != 4 {
		t.Fatalf("grouped into %d strips, want 4 (user, 3 tools, agent, 1 tool)", len(strips))
	}
	if strips[0].IsToolStrip || strips[3].IsToolStrip == false {
		t.Errorf("grouping: %+v", strips)
	}
	if strips[1].Label != "3 tool calls" || strips[1].Errors != 1 {
		t.Errorf("first strip: label %q, errors %d; want \"3 tool calls\" and 1",
			strips[1].Label, strips[1].Errors)
	}
	if strips[2].IsToolStrip {
		t.Error("a subagent turn was collapsed into a tool strip")
	}
	if strips[3].Errors != 0 {
		t.Errorf("a clean strip raised %d errors", strips[3].Errors)
	}

	// And the badge reaches the page.
	r := run()
	r.Thread = &Thread{Thread: &store.Thread{Title: "Port defaulting"},
		Messages: []*store.Message{
			{Role: "tool", Body: "Bash: go build\nexit code 1"},
			{Role: "agent", Body: "Fixing the import."},
		}}
	html := renderOne(t, r)
	if !strings.Contains(html, `class="errbadge">1 error`) {
		t.Error("a closed tool strip hides its error")
	}
	if !strings.Contains(html, `class="msg msg-agent"`) {
		t.Error("the subagent turn is missing from the page")
	}
}

func TestLooksLikeError(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{"Read app.go (18 lines)", false},
		{"Bash: go build\nerror: undefined: os", true},
		{"exit code 1", true},
		{"Grep: no matches", false},
		{"Traceback (most recent call last):", true},
	}
	for _, c := range cases {
		if got := looksLikeError(&store.Message{Body: c.body}); got != c.want {
			t.Errorf("looksLikeError(%q) = %v, want %v", c.body, got, c.want)
		}
	}
}

// A run with nothing to show still renders a page that says so, rather than
// an empty one that reads as a bug.
func TestEmptyReviewSaysSo(t *testing.T) {
	out, err := RenderHTML(&Review{
		Repository: Repository{ID: "01REPO", Name: "demo"},
		Scope:      Scope{Kind: ScopeSinceLastReview},
		Runs:       []*Run{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "No agent runs in this scope") {
		t.Error("an empty review does not explain itself")
	}
}

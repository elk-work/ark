// Renderer for `ark review --html`.
//
// The visual system is borrowed, with thanks and under its licence, from
// pretty-cc — MIT (c) 2026 Eric Nguyen — specifically: the six-token palette
// (--bg --fg --muted --line --accent --code-bg) with the dark theme remapping
// those same six rather than forking the stylesheet; the diff palette of
// blue-add and brick-delete, chosen not to collide with the accent, two-tone
// with a pale line wash and a saturated word span, and redundantly encoded so
// colour is never the only signal; the priority-clamp grid where the rail
// compresses first and the measure never grows; and collapsed tool strips
// that surface errors in the closed summary while subagent turns never
// collapse.
//
// The two-axis run model — liveness and outcome as independent facts, with a
// human-readable reason on anything unsettled and `waiting` sorted above
// `errored` — is borrowed from agentglass (SirAllap/agentglass, MIT).
//
// The page is self-contained: one file, no requests, no webfont. It is meant
// to be attachable to a run as an artifact and still legible years later on a
// machine with no network.
package review

import (
	"bytes"
	_ "embed"
	"fmt"
	"html"
	"html/template"
	"strconv"
	"strings"
	"time"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

//go:embed review.css
var reviewCSS string

//go:embed review.html.tmpl
var reviewTemplate string

// RenderHTML turns a collected review into one self-contained page.
func RenderHTML(rev *Review) ([]byte, error) {
	t, err := template.New("review").Parse(reviewTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, buildPage(rev)); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// The view model. Template logic stays thin on purpose: everything that
// needs a decision is decided here, in Go, where it is testable.

type page struct {
	Title       string
	CSS         template.CSS
	Dark        bool
	Repo        Repository
	ScopeText   string
	RunCount    string
	GeneratedAt string
	Totals      Totals
	Runs        []runView
}

type runView struct {
	Anchor        string
	ShortID       string
	Headline      string
	Status        string
	Timing        string
	Liveness      string
	Outcome       string
	OutcomeGlyph  string
	NeedBecause   string
	InputSummary  string
	ResultSummary string
	Links         []linkView
	Diff          *diffView
	Artifacts     []artifactView
	ThreadTitle   string
	Thread        []strip
	Comments      []messageView
}

type linkView struct{ Kind, Text string }

type artifactView struct{ Name, Meta string }

type diffView struct {
	Unavailable string
	Truncated   bool
	Files       []fileView
	Commits     []commitView
}

type fileView struct {
	Path       string
	OldPath    string
	Insertions int
	Deletions  int
	Binary     bool
	Truncated  bool
	Open       bool
	Hunks      []hunkView
}

type hunkView struct {
	Header string
	Lines  []lineView
}

type lineView struct {
	Kind  string
	Old   string
	New   string
	Glyph string
	HTML  template.HTML
}

type commitView struct{ Short, Subject string }

// strip is a run of thread messages rendered together. A tool strip
// collapses; everything else does not.
type strip struct {
	IsToolStrip bool
	Label       string
	Errors      int
	Messages    []messageView
}

type messageView struct {
	Role string
	Who  string
	Body string
}

func buildPage(rev *Review) page {
	p := page{
		Title:       rev.Repository.Name + " — ark review",
		CSS:         template.CSS(reviewCSS),
		Repo:        rev.Repository,
		ScopeText:   scopeText(rev.Scope),
		RunCount:    plural(len(rev.Runs), "run", "runs"),
		GeneratedAt: records.FormatTime(rev.GeneratedAt),
		Totals:      rev.Totals,
	}
	for _, r := range rev.Runs {
		p.Runs = append(p.Runs, buildRun(r))
	}
	return p
}

func scopeText(sc Scope) string {
	switch sc.Kind {
	case ScopeRun:
		return "one run"
	case ScopeSince:
		return "finished since " + records.FormatTime(sc.Since) + ", plus anything still running"
	default:
		if sc.Since == "" {
			return "every run recorded here"
		}
		return "since the last review, " + records.FormatTime(sc.Since) + ", plus anything still running"
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func buildRun(r *Run) runView {
	v := runView{
		Anchor:        "run-" + strings.ToLower(shortID(r.ID)),
		ShortID:       shortID(r.ID),
		Headline:      headline(r),
		Status:        r.Status,
		Timing:        timing(r),
		Liveness:      r.Liveness,
		Outcome:       r.Outcome,
		OutcomeGlyph:  outcomeGlyph(r.Outcome),
		NeedBecause:   r.NeedBecause,
		InputSummary:  strings.TrimSpace(r.InputSummary),
		ResultSummary: strings.TrimSpace(r.ResultSummary),
		Links:         links(r),
		Diff:          buildDiff(r.Diff),
	}
	for _, a := range r.Artifacts {
		v.Artifacts = append(v.Artifacts, artifactView{
			Name: a.Name,
			Meta: strings.TrimSpace(a.MediaType + " · " + humanBytes(a.SizeBytes)),
		})
	}
	if r.Thread != nil {
		title := strings.TrimSpace(r.Thread.Title)
		if title == "" {
			title = "Thread"
		}
		v.ThreadTitle = title
		v.Thread = buildStrips(r.Thread.Messages)
	}
	for _, c := range r.Comments {
		v.Comments = append(v.Comments, messageView{
			Role: "comment", Who: records.FormatTime(c.CreatedAt), Body: c.Body})
	}
	return v
}

func headline(r *Run) string {
	if r.Task != nil && strings.TrimSpace(r.Task.Title) != "" {
		return fmt.Sprintf("#%d %s", r.Task.Number, records.Truncate(r.Task.Title, 70))
	}
	if s := strings.TrimSpace(r.InputSummary); s != "" {
		return records.Truncate(s, 70)
	}
	if r.AgentDisplay != "" {
		return r.AgentDisplay + " run"
	}
	return "run " + shortID(r.ID)
}

func timing(r *Run) string {
	if r.FinishedAt == "" {
		if r.StartedAt == "" {
			return ""
		}
		return "started " + records.FormatTime(r.StartedAt)
	}
	out := records.FormatTime(r.FinishedAt)
	if d, ok := span(r.StartedAt, r.FinishedAt); ok {
		out += " · took " + humanDuration(d)
	}
	return out
}

// span is the elapsed time between two stored timestamps, if both parse.
func span(from, to string) (time.Duration, bool) {
	a, errA := time.Parse(time.RFC3339Nano, from)
	b, errB := time.Parse(time.RFC3339Nano, to)
	if errA != nil || errB != nil || b.Before(a) {
		return 0, false
	}
	return b.Sub(a), true
}

func outcomeGlyph(outcome string) string {
	switch outcome {
	case OutcomeSettled:
		return "✓"
	case OutcomeFaulted:
		return "✗"
	case OutcomeUnanswered:
		return "?"
	default:
		return "~"
	}
}

func links(r *Run) []linkView {
	var out []linkView
	if t := r.Task; t != nil {
		out = append(out, linkView{"task", fmt.Sprintf("#%d (%s)", t.Number, t.Status)})
	}
	if pr := r.PullRequest; pr != nil {
		text := fmt.Sprintf("#%d (%s)", pr.Number, pr.Status)
		if n := len(pr.Reviews); n > 0 {
			state, _ := latestReview(pr)
			text += fmt.Sprintf(" · %s, latest %s", plural(n, "review", "reviews"), state)
		} else {
			text += " · no reviews"
		}
		out = append(out, linkView{"pull request", text})
	}
	if r.BranchName != "" {
		out = append(out, linkView{"branch", r.BranchName})
	}
	if r.AgentDisplay != "" {
		out = append(out, linkView{"agent", r.AgentDisplay})
	}
	return out
}

func buildDiff(d *Diff) *diffView {
	if d == nil {
		return &diffView{Unavailable: "no diff was collected for this run"}
	}
	v := &diffView{Unavailable: d.Unavailable, Truncated: d.Truncated}
	for _, c := range d.Commits {
		v.Commits = append(v.Commits, commitView{Short: short(c.SHA), Subject: c.Subject})
	}
	// Open the small files. A page that arrives with everything shut hides
	// the thing it exists to show; one that opens a 500-line file buries it.
	for _, f := range d.Files {
		fv := fileView{Path: f.Path, OldPath: f.OldPath, Insertions: f.Insertions,
			Deletions: f.Deletions, Binary: f.Binary, Truncated: f.Truncated}
		lines := 0
		for _, h := range f.Hunks {
			hv := hunkView{Header: h.Header}
			for _, l := range h.Lines {
				hv.Lines = append(hv.Lines, lineView{
					Kind: l.Kind, Old: num(l.OldNum), New: num(l.NewNum),
					Glyph: glyph(l.Kind), HTML: lineHTML(l),
				})
			}
			lines += len(h.Lines)
			fv.Hunks = append(fv.Hunks, hv)
		}
		fv.Open = lines > 0 && lines <= 60 && len(d.Files) <= 12
		v.Files = append(v.Files, fv)
	}
	return v
}

func num(n int) string {
	if n == 0 {
		return ""
	}
	return strconv.Itoa(n)
}

// glyph is the redundant half of the diff encoding: what a reader sees when
// the colours do not arrive, or do not separate.
func glyph(kind string) string {
	switch kind {
	case LineAdd:
		return "+"
	case LineDel:
		return "−" // a real minus, not a hyphen
	default:
		return " "
	}
}

// lineHTML escapes a diff line and wraps the changed span, if there is one.
func lineHTML(l DiffLine) template.HTML {
	text := l.Text
	if text == "" {
		text = " " // keep the row's height
	}
	s, e := l.SpanStart, l.SpanEnd
	if s <= 0 && e <= 0 || s >= e || e > len(l.Text) {
		return template.HTML(html.EscapeString(text))
	}
	return template.HTML(html.EscapeString(l.Text[:s]) +
		"<mark>" + html.EscapeString(l.Text[s:e]) + "</mark>" +
		html.EscapeString(l.Text[e:]))
}

// buildStrips groups consecutive tool messages so they can collapse
// together, and leaves every other role expanded. A subagent's turn is the
// substance of the thread and never hides.
func buildStrips(msgs []*store.Message) []strip {
	var out []strip
	for i := 0; i < len(msgs); {
		if msgs[i].Role != "tool" {
			out = append(out, strip{Messages: []messageView{view(msgs[i])}})
			i++
			continue
		}
		start := i
		errs := 0
		var group []messageView
		for i < len(msgs) && msgs[i].Role == "tool" {
			if looksLikeError(msgs[i]) {
				errs++
			}
			group = append(group, view(msgs[i]))
			i++
		}
		out = append(out, strip{
			IsToolStrip: true,
			Label:       plural(i-start, "tool call", "tool calls"),
			Errors:      errs,
			Messages:    group,
		})
	}
	return out
}

func view(m *store.Message) messageView {
	return messageView{Role: m.Role, Who: m.Role + " · " + records.FormatTime(m.CreatedAt), Body: m.Body}
}

// looksLikeError decides whether a collapsed tool call should raise a badge
// on the closed summary. It is a heuristic over text Ark does not structure,
// and it errs towards showing the badge: a false badge costs a glance, a
// missed one costs the whole reason the strip is collapsible.
func looksLikeError(m *store.Message) bool {
	hay := strings.ToLower(m.Body + " " + m.MetadataJSON)
	for _, needle := range []string{"error", "failed", "failure", "exception",
		"traceback", "panic:", "exit code 1", "exit status 1", `"ok":false`} {
		if strings.Contains(hay, needle) {
			return true
		}
	}
	return false
}

func humanBytes(n int64) string {
	switch {
	case n <= 0:
		return ""
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f kB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func shortID(id string) string {
	if len(id) > 10 {
		return id[:10]
	}
	return id
}

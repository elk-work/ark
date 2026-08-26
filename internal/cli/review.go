package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/output"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/review"
	"github.com/elk-work/ark/internal/store"
)

func newReviewCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Review what ran here — runs, what they were asked, what they produced",
		Long: `Review gathers agent runs and everything the records already hang off
them: the task, the thread, the pull request and its reviews, the artifacts,
and the diff between the two commits each run recorded.

It only reads. Nothing here is a new record, and a run that has not finished
is always in scope whatever the window, because that is the one most likely
to need a person.

With no scope, review covers everything finished since the last review.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()

			scope, err := resolveScope(cmd, a)
			if err != nil {
				return err
			}
			rev, err := review.Collect(ctx, a.Store, review.Options{
				Scope:  scope,
				Git:    a.Git,
				Name:   filepath.Base(a.Root),
				Branch: currentBranch(ctx, a),
			})
			if err != nil {
				return err
			}

			p := g.printer(cmd)
			out, _ := cmd.Flags().GetString("out")
			openIt, _ := cmd.Flags().GetBool("open")
			attach, _ := cmd.Flags().GetBool("artifact")
			asHTML, _ := cmd.Flags().GetBool("html")
			// The sinks all take a page, so asking for one implies --html.
			asHTML = asHTML || out != "" || openIt || attach

			if scope.Kind == review.ScopeSinceLastReview {
				if noAdvance, _ := cmd.Flags().GetBool("no-advance"); !noAdvance {
					// Best effort: a review that rendered is a review that
					// happened, and a failed cursor write only widens the
					// next one.
					_ = review.WriteCursor(a.ArkDir, rev.GeneratedAt)
				}
			}

			if !asHTML {
				return p.Result(rev, func() { printReview(p, rev) })
			}

			html, err := review.RenderHTML(rev)
			if err != nil {
				return err
			}
			written, err := deliver(ctx, a, rev, html, out, openIt, attach)
			if err != nil {
				return err
			}
			if written == "" {
				// No sink asked for a file: the page is the output.
				_, err := cmd.OutOrStdout().Write(html)
				return err
			}
			if p.JSON {
				return p.JSONValue(map[string]string{"path": written})
			}
			p.Line("Wrote %s", written)
			return nil
		},
	}
	cmd.Flags().String("run", "", "review one run (id or unambiguous prefix)")
	cmd.Flags().String("since", "", "review runs finished within this duration (e.g. 24h)")
	cmd.Flags().Bool("html", false, "render a self-contained HTML page")
	cmd.Flags().String("out", "", "write the page to this path")
	cmd.Flags().Bool("open", false, "write the page under .ark/tmp and open it")
	cmd.Flags().Bool("artifact", false, "attach the page to the run as an artifact")
	cmd.Flags().Bool("no-advance", false, "leave the review cursor where it is")
	// The third sink, --elk, is deliberately absent. Pushing a page into an
	// Elk camp is Elk's client's business, not this command's: the seam is
	// RenderHTML returning bytes, and anything that wants to deliver them
	// somewhere calls that. Nothing Elk-shaped belongs in this repository.
	return cmd
}

// resolveScope turns the flags into the window under review.
func resolveScope(cmd *cobra.Command, a *app.Context) (review.Scope, error) {
	runRef, _ := cmd.Flags().GetString("run")
	sinceStr, _ := cmd.Flags().GetString("since")
	if runRef != "" && sinceStr != "" {
		return review.Scope{}, records.Validationf("--run and --since select different things; pass one")
	}
	switch {
	case runRef != "":
		return review.Scope{Kind: review.ScopeRun, RunID: runRef}, nil
	case sinceStr != "":
		d, err := time.ParseDuration(sinceStr)
		if err != nil {
			return review.Scope{}, records.Validationf("invalid --since %q: %v", sinceStr, err)
		}
		if d < 0 {
			d = -d
		}
		return review.Scope{Kind: review.ScopeSince,
			Since: time.Now().UTC().Add(-d).Format(time.RFC3339Nano)}, nil
	default:
		return review.Scope{Kind: review.ScopeSinceLastReview,
			Since: review.ReadCursor(a.ArkDir)}, nil
	}
}

func currentBranch(ctx context.Context, a *app.Context) string {
	b, err := a.Git.CurrentBranch(ctx)
	if err != nil {
		return ""
	}
	return b
}

// deliver writes the page wherever the flags asked, returning the path it
// landed at (empty when nothing asked for a file).
func deliver(ctx context.Context, a *app.Context, rev *review.Review, html []byte,
	out string, openIt, attach bool) (string, error) {

	var path string
	if out != "" {
		path = out
	} else if openIt {
		path = filepath.Join(a.ArkDir, "tmp",
			"review-"+time.Now().UTC().Format("20060102-150405")+".html")
	}
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", &records.Error{Kind: records.KindGeneral, Message: "create output directory", Err: err}
		}
		if err := os.WriteFile(path, html, 0o644); err != nil {
			return "", &records.Error{Kind: records.KindGeneral, Message: "write review", Err: err}
		}
	}
	if attach {
		if err := attachReview(ctx, a, rev, html); err != nil {
			return path, err
		}
	}
	if openIt && path != "" {
		openInBrowser(ctx, path)
	}
	return path, nil
}

// attachReview stores the page against its run through the ordinary artifact
// path — content-addressed, checksummed, and synced like any other artifact.
func attachReview(ctx context.Context, a *app.Context, rev *review.Review, html []byte) error {
	if len(rev.Runs) != 1 {
		return records.Validationf(
			"--artifact attaches the page to one run, and this review covers %d; pass --run",
			len(rev.Runs))
	}
	return attachReviewTo(ctx, a, rev.Runs[0].ID, html)
}

func attachReviewTo(ctx context.Context, a *app.Context, runID string, html []byte) error {
	tmp := filepath.Join(a.ArkDir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return &records.Error{Kind: records.KindGeneral, Message: "create tmp dir", Err: err}
	}
	f, err := os.CreateTemp(tmp, "review-*.html")
	if err != nil {
		return &records.Error{Kind: records.KindGeneral, Message: "create tmp file", Err: err}
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(html); err != nil {
		f.Close()
		return &records.Error{Kind: records.KindGeneral, Message: "write tmp file", Err: err}
	}
	if err := f.Close(); err != nil {
		return &records.Error{Kind: records.KindGeneral, Message: "close tmp file", Err: err}
	}
	_, err = a.Store.AddArtifact(ctx, a.ArkDir, f.Name(), "agent_run", runID, "review.html")
	return err
}

// skipRunReview reports whether `ark run finish` should leave the review
// alone. The environment variable exists for the callers with no command
// line to change — a wrapper script, a CI image, an agent harness that
// finishes runs on your behalf.
func skipRunReview(cmd *cobra.Command) bool {
	if skip, _ := cmd.Flags().GetBool("no-review"); skip {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ARK_NO_RUN_REVIEW"))) {
	case "", "0", "false", "no":
		return false
	default:
		return true
	}
}

// attachRunReview renders and attaches a review of one finished run. It is
// called from `ark run finish`, where it must never be able to change the
// finish's result: a run that finished, finished.
func attachRunReview(ctx context.Context, a *app.Context, r *store.Run) error {
	rev, err := review.Collect(ctx, a.Store, review.Options{
		Scope:  review.Scope{Kind: review.ScopeRun, RunID: r.ID},
		Git:    a.Git,
		Name:   filepath.Base(a.Root),
		Branch: r.BranchName,
	})
	if err != nil {
		return err
	}
	html, err := review.RenderHTML(rev)
	if err != nil {
		return err
	}
	return attachReviewTo(ctx, a, r.ID, html)
}

// openInBrowser hands the file to the desktop. Best effort by design: the
// file is already written and its path already reported, so a missing opener
// is an inconvenience rather than a failure.
func openInBrowser(ctx context.Context, path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", abs)
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", abs)
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", abs)
	}
	_ = cmd.Start()
}

// printReview is the terminal rendering: the table first, then the sentence
// for every run that is not settled, because that is the part a person acts
// on and a table column cannot hold it.
func printReview(p *output.Printer, rev *review.Review) {
	if len(rev.Runs) == 0 {
		p.Line("no runs in scope")
		return
	}
	rows := make([][]string, len(rev.Runs))
	for i, r := range rev.Runs {
		rows[i] = []string{
			shortID(r.ID),
			r.Liveness,
			r.Outcome,
			diffCell(r),
			records.Truncate(headlineOf(r), 44),
		}
	}
	p.Table([]string{"RUN", "LIVE", "OUTCOME", "DIFF", "WHAT"}, rows)

	var needs []*review.Run
	for _, r := range rev.Runs {
		if r.NeedBecause != "" {
			needs = append(needs, r)
		}
	}
	if len(needs) == 0 {
		p.Line("")
		p.Line("Nothing is waiting on anyone.")
		return
	}
	p.Line("")
	for _, r := range needs {
		p.Line("%s  %s", shortID(r.ID), r.NeedBecause)
	}
}

func diffCell(r *review.Run) string {
	if r.Diff == nil || r.Diff.Unavailable != "" {
		return "-"
	}
	return fmt.Sprintf("+%d/-%d", r.Diff.Insertions, r.Diff.Deletions)
}

func headlineOf(r *review.Run) string {
	if r.Task != nil && strings.TrimSpace(r.Task.Title) != "" {
		return fmt.Sprintf("#%d %s", r.Task.Number, r.Task.Title)
	}
	if s := strings.TrimSpace(r.InputSummary); s != "" {
		return s
	}
	return r.AgentName
}

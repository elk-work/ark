package cli

import (
	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	arksync "github.com/elk-work/ark/internal/sync"
	"github.com/elk-work/ark/pkg/api"
)

// `ark repo dangling`: the references the sync service accepted while holding
// nothing at the other end (docs/v1-spec.md §9.1, elk-work/ark#74).
//
// **Why this is a command and not a line in `ark status`.**
//
// `ark status` answers about a checkout, from `.ark/ark.db`, without a
// network round trip. Every line it prints today is local, including the two
// that report on the service — a rejection and a history reset are both
// written down locally by the sync that discovered them, and status reads
// what sync stored. That is the shape a status line about the service has to
// have here, and a dangling count has no such local copy: asking for one
// would put a round trip inside the command a person runs most, on a
// local-first tool (principle 003), and would give `ark status` a way to be
// slow, or to fail, because a service is unreachable.
//
// So it lives with `ark repo show` and `ark repo grants`, which are already
// the commands that ask the service what it holds about this repository, and
// which already fail with exit 6 when there is no remote or the service is
// down — the right answer for a question only the service can answer.
//
// **It is still one story with `ark status`.** A held record (#89) and a
// dangling reference (#77) are the same skew seen from the two sides: a
// client sets a record aside because the record it names has not arrived, and
// the service accepted that record while holding nothing at the other end. So
// status's `held` line points here when a remote is configured, and this
// command points back — the client-side count says this checkout is waiting,
// and this one says whether the service is waiting too. Where it is, waiting
// will not end on its own.

func newRepoDanglingCmd(g *globals) *cobra.Command {
	var repoFlag string
	var all bool
	var limit int
	cmd := &cobra.Command{
		Use:   "dangling",
		Short: "List references the sync service accepted and cannot resolve",
		Long: `List the references the sync service accepted while holding nothing at the
other end.

The service does not refuse a record whose referent it has not seen, and that
is deliberate: nothing orders one client's push against another's, so the
record being named may still be sitting in somebody else's queue, and refusing
the child would turn a skew that ends by itself into a rejection that never
does. What the service does instead is write the reference down. This is how
you read what it wrote.

An outstanding entry is a defect, not a statistic: no client can render the
record it belongs to. It is the service's state and not this checkout's,
though — ` + "`ark status`" + ` counts the same skew from the client's side, as held
records.

By default only what is still outstanding is listed. --all adds the entries
whose referent has since arrived, which are kept because how often a
repository sees this skew is worth knowing.

Reading this needs ` + "`read`" + ` on the repository — the level that already pulls
its records.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			repoID, err := targetRepo(a, repoFlag)
			if err != nil {
				return err
			}
			if limit < 0 || limit > api.DanglingMaxLimit {
				return records.Validationf("--limit takes a number from 1 to %d",
					api.DanglingMaxLimit)
			}
			// Offline (exit 6) when there is no remote, and offline again
			// when the service cannot be reached. Both are the honest answer
			// to a question only the service can answer, and neither is a
			// state this command should paper over with a zero.
			client, err := arksync.Client(a)
			if err != nil {
				return err
			}
			resp, err := client.Dangling(cmd.Context(), repoID, all, limit)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(resp, func() { printDangling(g, cmd, *resp, all) })
		},
	}
	cmd.Flags().StringVar(&repoFlag, "repo", "", "repository ULID (defaults to this repository)")
	cmd.Flags().BoolVar(&all, "all", false, "include entries whose referent has since arrived")
	cmd.Flags().IntVar(&limit, "limit", 0, "maximum entries to list (default 100)")
	return cmd
}

func printDangling(g *globals, cmd *cobra.Command, resp api.DanglingResponse, all bool) {
	p := g.printer(cmd)
	if resp.Outstanding == 0 && (!all || resp.Recorded == 0) {
		// Worth a sentence rather than an empty table, and worth naming what
		// was checked: this is the good answer to a question about a defect,
		// and "nothing printed" is also what a broken command looks like.
		p.Line("Nothing dangling on %s: every reference the service holds resolves.", resp.RepositoryID)
		if resp.Recorded > 0 {
			p.Line("%d reference(s) have dangled here and since resolved (--all lists them).",
				resp.Recorded)
		}
		return
	}

	header := []string{"RECORD", "FIELD", "MISSING", "FIRST SEEN"}
	if all {
		header = append(header, "")
	}
	rows := make([][]string, 0, len(resp.References))
	for _, ref := range resp.References {
		row := []string{
			ref.RecordType + " " + ref.RecordID,
			ref.Field,
			ref.ParentType + " " + ref.ParentID,
			records.FormatTime(ref.FirstSeenAt),
		}
		if all {
			state := "outstanding"
			if ref.Resolved {
				state = "resolved"
			}
			row = append(row, state)
		}
		rows = append(rows, row)
	}
	p.Line("%s: %d outstanding, %d recorded (revision %d)",
		resp.RepositoryID, resp.Outstanding, resp.Recorded, resp.ServerRevision)
	p.Line("")
	p.Table(header, rows)
	if resp.Truncated {
		p.Line("")
		p.Line("Oldest %d shown; raise --limit (up to %d) for the rest.",
			len(resp.References), api.DanglingMaxLimit)
	}
	if resp.Outstanding == 0 {
		return
	}
	p.Line("")
	// What it is, and what it means for the person reading — who very
	// probably cannot fix it, because the missing record is on a machine
	// they do not have.
	p.Line("Each outstanding row is a record the service is serving whose pointer resolves")
	p.Line("to nothing, so no client can render it. It clears itself if the record it names")
	p.Line("is still in some client's queue and reaches the service. If no client holds that")
	p.Line("record, it never will, and nothing here can repair it.")
	p.Line("`ark status` counts the local half of this, as held records.")
}

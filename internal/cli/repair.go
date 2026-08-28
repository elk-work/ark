package cli

import (
	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/output"
	"github.com/elk-work/ark/internal/records"
	arksync "github.com/elk-work/ark/internal/sync"
)

// newRepairCmd builds `ark repair`.
//
// A group rather than a flag on `ark sync`, and the noun carries a direction
// for a reason. There are two recoveries from a service that has lost a
// repository and they move data opposite ways: restoring the service from its
// own stored copy of `repos/<id>.db`, which is the first thing to reach for
// and involves no client at all, and replaying a client's mutation log into
// it, which is this. Naming the command `push` says which one is happening,
// and leaves the other nameable if it ever grows a client-side half.
//
// The stronger reason is what a flag would do. `ark sync` is the command every
// agent and every hook runs on a loop; `--repair` would sit one copy-paste
// from a wrapper script, and a repair that runs on a schedule is exactly the
// automatic reconciliation §9.2 refuses to do. A separate command with its own
// gate, its own confirmation and its own help text cannot become habitual by
// accident.
func newRepairCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repair",
		Short: "Recover a repository the sync service has lost",
	}
	cmd.AddCommand(newRepairPushCmd(g))
	return cmd
}

func newRepairPushCmd(g *globals) *cobra.Command {
	var confirm bool
	cmd := &cobra.Command{
		Use:   "push",
		Short: "Replay this checkout's mutation log into the sync service",
		Long: `Replay this checkout's mutation log into the sync service.

Run this after ` + "`ark sync`" + ` reports that the service is serving a revision
below one this checkout had already synced past — its database for this
repository was reset, lost, or restored from an earlier point, and the records
it acknowledged are gone. This checkout still holds them, along with the
mutation log that produced them, so it can put them back.

It refuses to run unless such a history reset has been recorded, and previews
by default: nothing changes until you pass --confirm. What it then does, in
order, is rewind the pull cursor, register, pull everything the service does
still hold, re-queue every mutation the lost service ruled on — in ULID order,
rebased onto the history the service is serving now — and sync.

Display numbers are not preserved. See docs/v1-spec.md §9.3.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			res, err := arksync.Repair(cmd.Context(), a, confirm)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if err := p.Result(res, func() {
				if res.DryRun {
					printRepairPlan(p, a.Config.Remote, res)
					return
				}
				printRepairResult(p, a.Config.Remote, res)
			}); err != nil {
				return err
			}

			// The one exit code this command can answer 0 with is a repair
			// that left nothing to repair. Everything else is spec §22's
			// partial success: a preview did what it was asked and left the
			// repository needing work, and so did a replay the service only
			// half accepted. A 0 from either would be the same false "in
			// sync" that let this failure sit unnoticed for six weeks.
			if res.Cleared {
				return nil
			}
			if res.DryRun {
				return records.Partialf(
					"nothing has been changed; run `ark repair push --confirm` to replay %d mutation(s)",
					res.Replayable)
			}
			if res.Sync != nil && res.Sync.Rejected > 0 {
				return records.Partialf(
					"%d mutation(s) rejected; the history reset stays recorded until a repair completes cleanly — see `ark status`",
					res.Sync.Rejected)
			}
			return records.Partialf(
				"the replay finished but the service is still not serving this checkout's history; see `ark status`")
		},
	}
	cmd.Flags().BoolVar(&confirm, "confirm", false,
		"actually replay; without it the command only reports what it would do")
	return cmd
}

// printRepairPlan is the preview, and the renumbering warning lives here
// rather than in the result because §6.2 is a thing to be told *before* the
// service reassigns anything: an `ark:<repo>#N` written into a commit message
// or a design doc cannot be corrected afterwards by the person who reads this.
func printRepairPlan(p *output.Printer, remote string, res *arksync.RepairResult) {
	hr := res.HistoryReset
	p.Line("Repair plan for %s", remote)
	p.Line("")
	p.Line("  The service is at revision %d for this repository, below revision %d,", hr.ServerRevision, hr.LocalRevision)
	p.Line("  which this checkout had already synced past. First seen %s.", records.FormatTime(hr.DetectedAt))
	p.Line("")
	p.Line("  %d mutation(s) would be replayed, oldest first — everything the", res.Replayable)
	p.Line("  lost service ruled on, whether it accepted or refused it.")
	p.Line("  The pull cursor would be rewound first, so every record the service")
	p.Line("  still holds merges into this checkout before anything is pushed at it.")
	p.Line("")
	p.Line("  Display numbers are not preserved. The service reassigns a task or")
	p.Line("  pull request number another record already holds, so an `ark:<repo>#N`")
	p.Line("  reference written down outside Ark can name a different record")
	p.Line("  afterwards. ULIDs do not change and are what to quote instead.")
	p.Line("")
	p.Line("  Nothing has been changed. Run `ark repair push --confirm` to replay.")
}

func printRepairResult(p *output.Printer, remote string, res *arksync.RepairResult) {
	sync := res.Sync
	p.Line("Repaired %s (server revision %d)", remote, sync.ServerRevision)
	p.Line("  re-queued  %d mutations for replay", res.Requeued)
	p.Line("  pushed     %d mutations (%d applied, %d rejected, %d conflicts)",
		sync.Pushed, sync.Applied, sync.Rejected, sync.Conflicts)
	if sync.ArtifactsUploaded > 0 {
		p.Line("  uploaded   %d artifact blobs", sync.ArtifactsUploaded)
	}
	p.Line("  pulled     %d records, %d tombstones", sync.PulledRecords, sync.PulledTombstones)
	for _, issue := range sync.Issues {
		p.Line("  %s: %s %s — %s", issue.Status, issue.RecordType, shortID(issue.RecordID), issue.Error)
	}
	// Named one by one, because "numbers may have changed" is not actionable
	// and "task #3 is now #7" is.
	for _, r := range res.Renumbered {
		p.Line("  renumbered %s #%d is now #%d (%s)", r.RecordType, r.From, r.To, shortID(r.RecordID))
	}
	if res.Cleared {
		p.Line("  the recorded history reset is cleared: the service is serving")
		p.Line("  this checkout's records again.")
		return
	}
	p.Line("  the recorded history reset stands. Run `ark status`.")
}

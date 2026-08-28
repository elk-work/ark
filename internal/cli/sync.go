package cli

import (
	"sort"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	arksync "github.com/elk-work/ark/internal/sync"
)

func newSyncCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Push pending mutations and pull accepted records",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			res, err := arksync.Run(cmd.Context(), a)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if err := p.Result(res, func() {
				p.Line("Synced with %s (server revision %d)", a.Config.Remote, res.ServerRevision)
				if res.Pushed > 0 {
					p.Line("  pushed    %d mutations (%d applied, %d rejected, %d conflicts)",
						res.Pushed, res.Applied, res.Rejected, res.Conflicts)
				}
				if res.ArtifactsUploaded > 0 {
					p.Line("  uploaded  %d artifact blobs", res.ArtifactsUploaded)
				}
				if res.ArtifactsDeduped > 0 {
					p.Line("  matched   %d artifact blobs already stored", res.ArtifactsDeduped)
				}
				if res.PulledRecords > 0 || res.PulledTombstones > 0 {
					p.Line("  pulled    %d records, %d tombstones", res.PulledRecords, res.PulledTombstones)
				}
				if res.Pushed == 0 && res.PulledRecords == 0 && res.PulledTombstones == 0 {
					p.Line("  nothing to do")
				}
				// Say so rather than dropping them quietly: this is history
				// the repository holds and this build cannot show.
				for _, recordType := range sortedKeys(res.SkippedRecords) {
					p.Line("  skipped   %d %s record(s) — this build does not know that type; upgrade ark to see them",
						res.SkippedRecords[recordType], recordType)
				}
				for _, issue := range res.Issues {
					p.Line("  %s: %s %s — %s", issue.Status, issue.RecordType,
						shortID(issue.RecordID), issue.Error)
				}
				if res.Conflicts > 0 {
					p.Line("Run `ark conflict list` to inspect conflicts.")
				}
				// Loudest thing this command can say, and it goes last so it
				// is the line left on the screen.
				if hr := res.HistoryReset; hr != nil {
					p.Line("")
					p.Line("WARNING: the sync service is at revision %d for this repository,", hr.ServerRevision)
					p.Line("below revision %d, which this checkout had already synced past.", hr.LocalRevision)
					p.Line("A revision counter only ever increases, so the service is not serving")
					p.Line("the history this checkout was tracking — its database was reset, lost,")
					p.Line("or restored from an earlier point. Records it acknowledged may be gone.")
					p.Line("Ark has not tried to reconcile this: which side is authoritative is not")
					p.Line("a decision it can make. First detected %s.", hr.DetectedAt)
					// Naming the way out is not the same as taking it. The
					// decision stays a person's; what changes is that they
					// now have somewhere to make it, rather than the SQLite
					// surgery this used to require (elk-work/ark#60).
					p.Line("")
					p.Line("If the service's own storage cannot be restored, this checkout can")
					p.Line("rebuild the repository from its mutation log: `ark repair push`.")
				}
			}); err != nil {
				return err
			}

			// A sync that pushed changes the server refused is spec §22's
			// partial success (exit 7), not a clean 0. The transfer itself
			// worked; the repository came out of it disagreeing with the
			// service, and the disagreement is terminal — the mutation is out
			// of the queue and will not be retried. Reporting success there
			// is how an automated caller learns nothing happened.
			//
			// Conflicts deliberately stay exit 0. They are a designed state
			// with a repair path (`ark conflict resolve`) that `ark status`
			// has always named, so a conflicting sync was never claiming to
			// be in sync. Rejections were the silent half, and they are what
			// changes here.
			//
			// A history reset is partial success for the same reason and more
			// urgently: the transfer worked and the repository needs repair by
			// a person. It is reported ahead of a rejection because a service
			// that lost the repository explains any number of rejections, and
			// the rejection is the smaller fact.
			if hr := res.HistoryReset; hr != nil {
				return records.Partialf(
					"the sync service is at revision %d, below this checkout's %d — its history for this repository was reset or lost; see `ark status`",
					hr.ServerRevision, hr.LocalRevision)
			}
			if res.Rejected > 0 {
				return records.Partialf(
					"%d mutation(s) rejected; the local records still carry changes the server refused — see `ark status`",
					res.Rejected)
			}
			return nil
		},
	}
}

// sortedKeys gives map output a stable order so repeated syncs read the same.
func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

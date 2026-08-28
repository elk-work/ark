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

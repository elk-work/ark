package cli

import (
	"encoding/json"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/elkproject/ark/internal/workrecord"
)

// newElkCmd builds `ark elk`, the work-record adapter's client-side half.
// See docs/rfc-0002-elk-work-record-adapter.md.
func newElkCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "elk",
		Short: "Publish this repository's work record to Elk",
		Long: `Normalize Ark records into Elk work-record events.

Elk's connector turns these events into workspace signals, captures, and
actions. Emitting the same event twice is safe: Elk deduplicates on the
event key, so a repeated run is a no-op and a repository that has never
synced can still be backfilled from nothing.`,
	}
	cmd.AddCommand(newElkEventsCmd(g))
	return cmd
}

type elkEventsReport struct {
	RepositoryID   string             `json:"repository_id"`
	RepositoryName string             `json:"repository_name"`
	Count          int                `json:"count"`
	Events         []workrecord.Event `json:"events"`
}

func newElkEventsCmd(g *globals) *cobra.Command {
	var since string
	var comments bool
	var ndjson bool

	cmd := &cobra.Command{
		Use:   "events",
		Short: "Emit work-record events for this repository",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()

			repo := workrecord.Repo{
				ID:   a.Config.RepositoryID,
				Name: filepath.Base(a.Root),
			}
			events, err := workrecord.Collect(cmd.Context(), a.Store, repo, workrecord.Options{
				Since:           since,
				IncludeComments: comments,
			})
			if err != nil {
				return err
			}

			// NDJSON is the wire format: one event per line, streamable,
			// and what a delivery step POSTs.
			if ndjson {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetEscapeHTML(false)
				for _, e := range events {
					if err := enc.Encode(e); err != nil {
						return err
					}
				}
				return nil
			}

			rep := elkEventsReport{
				RepositoryID:   repo.ID,
				RepositoryName: repo.Name,
				Count:          len(events),
				Events:         events,
			}
			p := g.printer(cmd)
			return p.Result(rep, func() {
				if len(events) == 0 {
					p.Line("no work-record events")
					return
				}
				rows := make([][]string, 0, len(events))
				for _, e := range events {
					rows = append(rows, []string{
						e.OccurredAt, e.Kind, e.Actor.DisplayName, e.Title,
					})
				}
				p.Table([]string{"WHEN", "KIND", "ACTOR", "TITLE"}, rows)
				p.Line("")
				p.Line("%d events", len(events))
			})
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only events after this RFC3339 timestamp")
	cmd.Flags().BoolVar(&comments, "comments", false, "include comment events (high volume, off by default)")
	cmd.Flags().BoolVar(&ndjson, "ndjson", false, "emit one JSON event per line (the wire format)")
	return cmd
}

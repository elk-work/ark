package cli

import (
	"github.com/spf13/cobra"

	"github.com/ijroth/ark/internal/records"
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
			pending, err := a.Store.PendingMutations(cmd.Context())
			if err != nil {
				return err
			}
			if a.Config.Remote == "" {
				return records.Offlinef(
					"no Ark remote configured (%d mutations queued locally); set `remote` in .ark/config.toml once the sync service exists",
					pending)
			}
			// Phase 4 (docs/v1-spec.md §9) implements the push/pull protocol.
			return records.Offlinef("cloud sync is not implemented yet (%d mutations queued locally)", pending)
		},
	}
}

package cli

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

func newSearchCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across tasks, comments, threads, PRs, and reviews",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			limit, _ := cmd.Flags().GetInt("limit")
			results, err := a.Store.Search(cmd.Context(), strings.Join(args, " "), limit)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if p.JSON {
				if results == nil {
					results = []store.SearchResult{}
				}
				return p.JSONValue(results)
			}
			if len(results) == 0 {
				p.Line("no matches")
				return nil
			}
			rows := make([][]string, len(results))
			for i, r := range results {
				rows[i] = []string{r.RecordType, shortID(r.RecordID),
					records.Truncate(firstNonEmpty(r.Title, r.Snippet), 70)}
			}
			p.Table([]string{"TYPE", "RECORD", "MATCH"}, rows)
			return nil
		},
	}
	cmd.Flags().Int("limit", 20, "maximum results")
	return cmd
}

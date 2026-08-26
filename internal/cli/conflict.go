package cli

import (
	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
)

// conflictRow mirrors the conflicts table for output.
type conflictRow struct {
	ID         string `json:"id"`
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	MutationID string `json:"mutation_id"`
	Status     string `json:"status"`
	CreatedAt  string `json:"created_at"`
	BaseJSON   string `json:"base_json,omitempty"`
	LocalJSON  string `json:"local_json,omitempty"`
	RemoteJSON string `json:"remote_json,omitempty"`
}

func newConflictCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conflict",
		Short: "Inspect and resolve sync conflicts",
	}
	cmd.AddCommand(newConflictListCmd(g), newConflictViewCmd(g), newConflictResolveCmd(g))
	return cmd
}

func newConflictListCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List unresolved conflicts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			// Oldest first, by ULID — created_at is RFC3339Nano text, which
			// SQLite orders byte by byte and not chronologically.
			rows, err := a.DB.QueryContext(cmd.Context(), `SELECT id, record_type, record_id,
				mutation_id, status, created_at FROM conflicts WHERE status = 'unresolved'
				ORDER BY id`)
			if err != nil {
				return records.DBErr("list conflicts", err)
			}
			defer rows.Close()
			out := []conflictRow{}
			for rows.Next() {
				var c conflictRow
				if err := rows.Scan(&c.ID, &c.RecordType, &c.RecordID, &c.MutationID,
					&c.Status, &c.CreatedAt); err != nil {
					return records.DBErr("scan conflict", err)
				}
				out = append(out, c)
			}
			p := g.printer(cmd)
			if p.JSON {
				return p.JSONValue(out)
			}
			if len(out) == 0 {
				p.Line("no unresolved conflicts")
				return nil
			}
			table := make([][]string, len(out))
			for i, c := range out {
				table[i] = []string{shortID(c.ID), c.RecordType, shortID(c.RecordID),
					records.FormatTime(c.CreatedAt)}
			}
			p.Table([]string{"CONFLICT", "TYPE", "RECORD", "CREATED"}, table)
			return nil
		},
	}
}

func newConflictViewCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "view <id>",
		Short: "Show one conflict with base, local, and remote values",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			var c conflictRow
			err = a.DB.QueryRowContext(cmd.Context(), `SELECT id, record_type, record_id,
				mutation_id, status, created_at, base_json, local_json, remote_json
				FROM conflicts WHERE id LIKE ?`, args[0]+"%").
				Scan(&c.ID, &c.RecordType, &c.RecordID, &c.MutationID, &c.Status,
					&c.CreatedAt, &c.BaseJSON, &c.LocalJSON, &c.RemoteJSON)
			if err != nil {
				return records.NotFoundf("conflict %q not found", args[0])
			}
			p := g.printer(cmd)
			return p.Result(c, func() {
				p.Line("conflict %s on %s %s [%s]", c.ID, c.RecordType, c.RecordID, c.Status)
				p.Line("base:   %s", c.BaseJSON)
				p.Line("local:  %s", c.LocalJSON)
				p.Line("remote: %s", c.RemoteJSON)
			})
		},
	}
}

func newConflictResolveCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve <id>",
		Short: "Mark a conflict resolved",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			keep, _ := cmd.Flags().GetString("keep")
			var status string
			switch keep {
			case "local":
				status = "resolved_local"
				// Re-submit the local change against the record's current
				// server revision; the next sync carries it up.
				if err := a.Store.RequeueConflictLocal(cmd.Context(), args[0]); err != nil {
					return err
				}
			case "remote":
				status = "resolved_remote" // server state already pulled; nothing to send
			case "manual":
				status = "resolved_manual"
			default:
				return records.Validationf("--keep must be local, remote, or manual")
			}
			res, err := a.DB.ExecContext(cmd.Context(), `UPDATE conflicts
				SET status = ?, resolved_at = ? WHERE id LIKE ? AND status = 'unresolved'`,
				status, records.Now(), args[0]+"%")
			if err != nil {
				return records.DBErr("resolve conflict", err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return records.NotFoundf("no unresolved conflict matching %q", args[0])
			}
			p := g.printer(cmd)
			return p.Result(map[string]string{"status": status}, func() {
				p.Line("Resolved conflict (%s)", status)
			})
		},
	}
	cmd.Flags().String("keep", "", "which side to keep: local, remote, or manual")
	return cmd
}

package cli

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/elkproject/ark/internal/app"
	"github.com/elkproject/ark/internal/records"
	"github.com/elkproject/ark/internal/store"
	arksync "github.com/elkproject/ark/internal/sync"
)

func newArtifactCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "artifact",
		Short: "Attach and retrieve files produced by work",
	}
	cmd.AddCommand(
		newArtifactAddCmd(g),
		newArtifactListCmd(g),
		newArtifactGetCmd(g),
	)
	return cmd
}

// resolveParent turns "task:12"-style refs into a concrete (type, record id).
func resolveParent(cmd *cobra.Command, a *app.Context, ref string) (string, string, error) {
	parentType, parentRef, err := store.ParseParentRef(ref)
	if err != nil {
		return "", "", err
	}
	ctx := cmd.Context()
	switch parentType {
	case "task":
		t, err := a.Store.ResolveTask(ctx, parentRef)
		if err != nil {
			return "", "", err
		}
		return parentType, t.ID, nil
	case "pull_request":
		pr, err := a.Store.ResolvePR(ctx, parentRef)
		if err != nil {
			return "", "", err
		}
		return parentType, pr.ID, nil
	case "agent_run":
		r, err := a.Store.ResolveRun(ctx, parentRef)
		if err != nil {
			return "", "", err
		}
		return parentType, r.ID, nil
	default: // review — reviews have no CLI resolver yet; require a full ID
		return parentType, parentRef, nil
	}
}

func newArtifactAddCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <file>",
		Short: "Store a file as an artifact of a task, run, PR, or review",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			parentRef, _ := cmd.Flags().GetString("parent")
			if parentRef == "" {
				return records.Validationf("--parent is required (e.g. --parent task:12 or --parent run:01ABC)")
			}
			parentType, parentID, err := resolveParent(cmd, a, parentRef)
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString("name")
			art, err := a.Store.AddArtifact(cmd.Context(), a.ArkDir, args[0], parentType, parentID, name)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(art, func() {
				p.Line("Stored artifact %s: %s (%d bytes, sha256 %s)", art.ID, art.Name,
					art.SizeBytes, art.SHA256[:12])
			})
		},
	}
	cmd.Flags().String("parent", "", "owning record, e.g. task:12, pr:3, run:<id>, review:<id>")
	cmd.Flags().String("name", "", "artifact name (defaults to the file name)")
	return cmd
}

func newArtifactListCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List artifacts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			var parentType, parentID string
			if parentRef, _ := cmd.Flags().GetString("parent"); parentRef != "" {
				if parentType, parentID, err = resolveParent(cmd, a, parentRef); err != nil {
					return err
				}
			}
			arts, err := a.Store.ListArtifacts(cmd.Context(), parentType, parentID)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if p.JSON {
				if arts == nil {
					arts = []*store.Artifact{}
				}
				return p.JSONValue(arts)
			}
			if len(arts) == 0 {
				p.Line("no artifacts")
				return nil
			}
			rows := make([][]string, len(arts))
			for i, art := range arts {
				rows[i] = []string{shortID(art.ID), art.Name, art.MediaType,
					fmt.Sprintf("%d", art.SizeBytes),
					fmt.Sprintf("%s:%s", art.ParentType, shortID(art.ParentID))}
			}
			p.Table([]string{"ARTIFACT", "NAME", "TYPE", "BYTES", "PARENT"}, rows)
			return nil
		},
	}
	cmd.Flags().String("parent", "", "only artifacts of this record, e.g. task:12")
	return cmd
}

func newArtifactGetCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id>",
		Short: "Copy an artifact out of the object store",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			art, err := a.Store.ResolveArtifact(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			dest, _ := cmd.Flags().GetString("output")
			if dest == "" {
				dest = art.Name
			}
			// A record synced from another client has no local object yet;
			// fetch it from the remote's object storage.
			if art.LocalPath == "" {
				if _, err := arksync.FetchArtifact(cmd.Context(), a, art); err != nil {
					return err
				}
			}
			if err := a.Store.CopyArtifact(art, a.ArkDir, dest); err != nil {
				return err
			}
			abs, _ := filepath.Abs(dest)
			p := g.printer(cmd)
			type got struct {
				*store.Artifact
				Path string `json:"path"`
			}
			return p.Result(got{art, abs}, func() {
				p.Line("Wrote %s (%d bytes)", abs, art.SizeBytes)
			})
		},
	}
	cmd.Flags().StringP("output", "o", "", "destination path (defaults to the artifact name)")
	return cmd
}

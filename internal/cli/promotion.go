package cli

import (
	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

func newPromotionCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "promotion",
		Short: "Record which version is active in each environment",
	}
	cmd.AddCommand(
		newPromotionCreateCmd(g),
		newPromotionListCmd(g),
		newPromotionEndCmd(g),
	)
	return cmd
}

func newPromotionCreateCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Record that a version became active in an environment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()

			p := &store.Promotion{}
			p.Environment, _ = cmd.Flags().GetString("env")
			p.Service, _ = cmd.Flags().GetString("service")
			p.MergeCommitSHA, _ = cmd.Flags().GetString("commit")
			p.ArtifactSHA256, _ = cmd.Flags().GetString("artifact-sha256")
			p.MetadataJSON, _ = cmd.Flags().GetString("metadata")
			if p.MergeCommitSHA == "" && p.ArtifactSHA256 == "" {
				// Default the promoted version to wherever HEAD is now.
				if head, err := a.Git.Head(ctx); err == nil {
					p.MergeCommitSHA = head
				}
			}
			if prRef, _ := cmd.Flags().GetString("pr"); prRef != "" {
				pr, err := a.Store.ResolvePR(ctx, prRef)
				if err != nil {
					return err
				}
				p.PullRequestID = pr.ID
			}
			p, err = a.Store.CreatePromotion(ctx, p)
			if err != nil {
				return err
			}
			pr := g.printer(cmd)
			return pr.Result(p, func() {
				pr.Line("Promoted %s to %s%s", firstNonEmpty(short(p.MergeCommitSHA), short(p.ArtifactSHA256)),
					p.Environment, serviceSuffix(p.Service))
			})
		},
	}
	cmd.Flags().StringP("env", "e", "", "environment the version became active in (required)")
	cmd.Flags().String("service", "", "service within the environment")
	cmd.Flags().String("commit", "", "merge commit SHA (defaults to current HEAD)")
	cmd.Flags().String("artifact-sha256", "", "sha256 of the deployed artifact")
	cmd.Flags().String("pr", "", "pull request this promotion deploys (number or id)")
	cmd.Flags().String("metadata", "", "structured metadata as JSON")
	return cmd
}

func serviceSuffix(service string) string {
	if service == "" {
		return ""
	}
	return " (" + service + ")"
}

func newPromotionListCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List promotions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			env, _ := cmd.Flags().GetString("env")
			activeOnly, _ := cmd.Flags().GetBool("active")
			proms, err := a.Store.ListPromotions(cmd.Context(), env, activeOnly)
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			if p.JSON {
				if proms == nil {
					proms = []*store.Promotion{}
				}
				return p.JSONValue(proms)
			}
			if len(proms) == 0 {
				p.Line("no promotions")
				return nil
			}
			rows := make([][]string, len(proms))
			for i, pm := range proms {
				rows[i] = []string{shortID(pm.ID), pm.Environment, pm.Service,
					firstNonEmpty(short(pm.MergeCommitSHA), short(pm.ArtifactSHA256)),
					records.FormatTime(pm.ActivatedAt),
					firstNonEmpty(records.FormatTime(pm.EndedAt), "active")}
			}
			p.Table([]string{"PROMOTION", "ENV", "SERVICE", "VERSION", "ACTIVATED", "ENDED"}, rows)
			return nil
		},
	}
	cmd.Flags().StringP("env", "e", "", "only promotions for this environment")
	cmd.Flags().Bool("active", false, "only promotions still active")
	return cmd
}

func newPromotionEndCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "end <id>",
		Short: "End an active promotion without a successor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			pm, err := a.Store.EndPromotion(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			p := g.printer(cmd)
			return p.Result(pm, func() {
				p.Line("Ended promotion %s in %s", shortID(pm.ID), pm.Environment)
			})
		},
	}
}

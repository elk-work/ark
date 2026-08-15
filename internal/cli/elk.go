package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/workrecord"
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
	cmd.AddCommand(newElkEventsCmd(g), newElkPushCmd(g))
	return cmd
}

// elkPushResult is the connector's per-batch ledger, echoed for `--json`.
type elkPushResult struct {
	Endpoint string           `json:"endpoint"`
	Sent     int              `json:"sent"`
	Accepted int              `json:"accepted"`
	Results  []elkPushOutcome `json:"results,omitempty"`
	Dropped  []string         `json:"dropped,omitempty"`
}

type elkPushOutcome struct {
	ExternalID string   `json:"external_id"`
	Status     string   `json:"status,omitempty"`
	Ops        []string `json:"ops,omitempty"`
}

func newElkPushCmd(g *globals) *cobra.Command {
	var endpoint, token, since string
	var comments, dryRun bool

	cmd := &cobra.Command{
		Use:   "push",
		Short: "Send this repository's work-record events to Elk",
		Long: `Normalize this repository's records and deliver them to Elk's
work-record connector.

Safe to repeat. Elk deduplicates on the event key, so re-sending is a no-op
and a repository can be backfilled from nothing — there is no cursor to keep
and nothing to repair after an interrupted run.

The endpoint comes from --url or ARK_ELK_ENDPOINT; the bearer token from
--token or ARK_ELK_TOKEN. Neither is stored in the repository.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			endpoint = firstNonEmpty(endpoint, os.Getenv("ARK_ELK_ENDPOINT"))
			token = firstNonEmpty(token, os.Getenv("ARK_ELK_TOKEN"))
			if endpoint == "" {
				return records.Validationf("no Elk endpoint (pass --url or set ARK_ELK_ENDPOINT)")
			}
			if token == "" && !dryRun {
				return records.Validationf("no Elk token (pass --token or set ARK_ELK_TOKEN)")
			}

			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()

			repo := workrecord.Repo{ID: a.Config.RepositoryID, Name: filepath.Base(a.Root)}
			events, err := workrecord.Collect(cmd.Context(), a.Store, repo, workrecord.Options{
				Since: since, IncludeComments: comments,
			})
			if err != nil {
				return err
			}

			p := g.printer(cmd)
			if len(events) == 0 {
				return p.Result(elkPushResult{Endpoint: endpoint}, func() {
					p.Line("no work-record events to send")
				})
			}
			if dryRun {
				return p.Result(elkPushResult{Endpoint: endpoint, Sent: len(events)}, func() {
					p.Line("would send %d events to %s", len(events), endpoint)
				})
			}

			res, err := postEvents(cmd.Context(), endpoint, token, events)
			if err != nil {
				return err
			}
			res.Sent = len(events)
			return p.Result(res, func() {
				p.Line("Sent %d events to %s", res.Sent, res.Endpoint)
				counts := map[string]int{}
				for _, r := range res.Results {
					if len(r.Ops) == 0 {
						counts[r.Status]++
						continue
					}
					for _, op := range r.Ops {
						counts[op]++
					}
				}
				for _, k := range sortedKeys(counts) {
					p.Line("  %-10s %d", k, counts[k])
				}
				for _, d := range res.Dropped {
					p.Line("  dropped    %s", d)
				}
			})
		},
	}
	cmd.Flags().StringVar(&endpoint, "url", "", "connector endpoint (or ARK_ELK_ENDPOINT)")
	cmd.Flags().StringVar(&token, "token", "", "bearer token (or ARK_ELK_TOKEN)")
	cmd.Flags().StringVar(&since, "since", "", "only events after this RFC3339 timestamp")
	cmd.Flags().BoolVar(&comments, "comments", false, "include comment events (high volume, off by default)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would be sent without sending it")
	return cmd
}

// postEvents delivers one batch and returns the connector's ledger.
func postEvents(ctx context.Context, endpoint, token string, events []workrecord.Event) (elkPushResult, error) {
	var out elkPushResult
	body, err := json.Marshal(events)
	if err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return out, records.Validationf("invalid endpoint %q: %v", endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, records.Offlinef("cannot reach %s: %v", endpoint, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized, http.StatusForbidden:
		return out, records.Permissionf("Elk rejected the token")
	case http.StatusServiceUnavailable:
		return out, records.Offlinef("Elk's ark connector is not configured yet")
	default:
		return out, records.Validationf("Elk returned %d: %s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		return out, records.Validationf("unreadable response from Elk: %v", err)
	}
	out.Endpoint = endpoint
	return out, nil
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

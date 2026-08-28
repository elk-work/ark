package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
)

type statusReport struct {
	RepositoryID     string `json:"repository_id"`
	Root             string `json:"root"`
	Branch           string `json:"branch,omitempty"`
	Head             string `json:"head,omitempty"`
	Actor            string `json:"actor"`
	ActorType        string `json:"actor_type"`
	OpenTasks        int64  `json:"open_tasks"`
	OpenPRs          int64  `json:"open_pull_requests"`
	PendingMutations int64  `json:"pending_mutations"`
	// RejectedMutations counts changes the server refused whose effect it
	// still does not hold — the repository's divergence from the service.
	//
	// pending_mutations cannot answer that on its own, and the way it fails
	// is the worst available: a rejection *removes* the mutation from the
	// queue, so the number goes to zero at the exact moment the two copies
	// stop agreeing. A client reported `0 pending mutations` about a server
	// that had just refused three writes, and nothing in this command said
	// otherwise (elk-work/ark#46). Whatever else status reports, it must not
	// be able to describe a diverged repository as a clean one.
	RejectedMutations int64 `json:"rejected_mutations"`
	Conflicts         int64 `json:"unresolved_conflicts"`
	// HeldRecords counts records pulled from the service and not applied,
	// because each names a record this checkout does not hold. #75 made that
	// survivable — the batch and the cursor go through, and a held record is
	// applied on a later pull the moment its referent arrives — and in the
	// ordinary case it resolves itself before anyone looks.
	//
	// It is reported because it can also never resolve. The service accepts a
	// child whose parent it does not hold, deliberately (elk-work/ark#56), so
	// a held record is the client-side face of a dangling reference the
	// service has already recorded (elk-work/ark#77). If the parent is never
	// coming, nothing else in this command would ever say so.
	//
	// Deliberately NOT part of the divergence story above. A rejection is
	// terminal and means the service refused something; this means the client
	// is waiting, resolves without intervention, and must not push `ark sync`
	// to exit 7 (spec §9.2).
	HeldRecords int64  `json:"held_records,omitempty"`
	Remote      string `json:"remote,omitempty"`
	// HistoryReset is the third and worst of the states this command has to
	// tell apart. Nothing pending is one answer; something rejected is a
	// second; the service disagreeing about what *exists* is a third, and it
	// is the one that cost a repository. It is not derivable from either of
	// the others — the queue was empty and every mutation had been
	// acknowledged, by a service that no longer held the result
	// (elk-work/ark#58) — so it is carried separately rather than folded into
	// a count.
	HistoryReset *store.HistoryReset `json:"history_reset,omitempty"`
	// TokenSource is which store answered for the sync token — env, keyring,
	// file, or none — and is set only when a remote is configured. `ark login`
	// says where it wrote the token; resolution deserves to be as legible,
	// because reading one out of a plaintext file is a different state from
	// reading one out of the keychain. Never the token itself.
	TokenSource string `json:"token_source,omitempty"`
	// TokenSourceError is why nothing resolved, in the one case where that is
	// a state to repair rather than a blank to fill in: ~/.ark/credentials.toml
	// exists and will not parse, so whether it holds a token for this remote is
	// unknown (#63). `ark login`, `ark logout` and `ark sync` all refuse and
	// name that file; status, whose whole job is to say what state this
	// checkout is in, was the only command that reported it as "never logged
	// in" — honest about what resolved and useless to the person most likely to
	// be reading it.
	//
	// token_source stays "none" beside it. It is a stable interface and it is
	// not wrong: nothing did resolve. The diagnosis is an addition, not a new
	// value inside a field agents already match on.
	//
	// Empty for a machine that has simply never logged in — "run `ark login`"
	// is the whole of that state, and status already says it. Empty too for a
	// keyring that is locked or absent: resolution warns about that on stderr
	// as it happens, which is where §20 puts it and where a reader will see it
	// whether or not they asked for --json.
	//
	// The text is the resolution error verbatim, which is the sentence `ark
	// sync` prints when it refuses for this reason. One condition, one wording.
	TokenSourceError string `json:"token_source_error,omitempty"`
}

func newStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show repository, actor, and sync state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.open(cmd)
			if err != nil {
				return err
			}
			defer a.Close()
			ctx := cmd.Context()

			rep := statusReport{
				RepositoryID: a.Config.RepositoryID,
				Root:         a.Root,
				Actor:        a.Store.Actor.Name,
				ActorType:    string(a.Store.Actor.Type),
				Remote:       a.Config.Remote,
			}
			rep.Branch, _ = a.Git.CurrentBranch(ctx)
			rep.Head, _ = a.Git.Head(ctx)

			// Resolving here can warn on stderr that the keyring is locked or
			// absent, which is exactly where someone would want to hear it. A
			// missing credential is not a status failure — it is a state to
			// report — so the error never stops the command. It is no longer
			// dropped, though: one of the ways it can fail is a state the user
			// has to act on, and status is where they will look for it.
			tokenAt := cloud.SourceNone
			if rep.Remote != "" {
				cred, err := cloud.ResolveCredential(rep.Remote)
				switch {
				case err == nil:
					tokenAt = cred.Source
				case errors.Is(err, cloud.ErrCredentialsUnreadable):
					rep.TokenSourceError = err.Error()
				}
				rep.TokenSource = string(tokenAt)
			}

			count := func(query string, dest *int64, args ...any) error {
				if err := a.DB.QueryRowContext(ctx, query, args...).Scan(dest); err != nil {
					return records.DBErr("count records", err)
				}
				return nil
			}
			if err := count(`SELECT COUNT(*) FROM tasks WHERE repository_id = ? AND status NOT IN ('done','closed') AND deleted_at IS NULL`, &rep.OpenTasks, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM pull_requests WHERE repository_id = ? AND status = 'open' AND deleted_at IS NULL`, &rep.OpenPRs, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM mutations WHERE repository_id = ? AND status = 'pending'`, &rep.PendingMutations, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM mutations WHERE repository_id = ? AND status = 'rejected' AND resolved_at IS NULL`, &rep.RejectedMutations, a.Config.RepositoryID); err != nil {
				return err
			}
			if err := count(`SELECT COUNT(*) FROM conflicts WHERE status = 'unresolved'`, &rep.Conflicts); err != nil {
				return err
			}
			if rep.HeldRecords, err = a.Store.DeferredRecordCount(ctx); err != nil {
				return err
			}
			if rep.HistoryReset, err = a.Store.HistoryReset(ctx); err != nil {
				return err
			}

			p := g.printer(cmd)
			return p.Result(rep, func() {
				p.Line("repository  %s", rep.Root)
				p.Line("id          %s", rep.RepositoryID)
				if rep.Branch != "" {
					p.Line("branch      %s (%s)", rep.Branch, short(rep.Head))
				}
				p.Line("actor       %s (%s)", rep.Actor, rep.ActorType)
				p.Line("open        %d tasks, %d pull requests", rep.OpenTasks, rep.OpenPRs)
				// The queue count alone is the number that lied, so it never
				// stands alone again once anything has been rejected.
				queue := fmt.Sprintf("%d pending mutations", rep.PendingMutations)
				if rep.RejectedMutations > 0 {
					queue += fmt.Sprintf(", %d rejected", rep.RejectedMutations)
				}
				if rep.Remote == "" {
					p.Line("sync        no remote configured; %s recorded", queue)
				} else {
					p.Line("sync        %s (%s)", rep.Remote, queue)
					switch {
					// Before the source, because it is the reason there is
					// none — and not with "run `ark login`" appended, which is
					// the advice for a machine holding nothing and here is the
					// command that would overwrite the file (#62). The message
					// carries its own repair.
					case rep.TokenSourceError != "":
						p.Line("token       %s", rep.TokenSourceError)
					case tokenAt == cloud.SourceNone:
						p.Line("token       none — run `ark login`")
					case tokenAt == cloud.SourceFile:
						p.Line("token       %s (plaintext fallback)", tokenAt.Description())
					default:
						p.Line("token       %s", tokenAt.Description())
					}
				}
				// Spelled out rather than left as a number on the sync line:
				// a rejection is terminal — the mutation will not be retried
				// — so the local record keeps a change the service has never
				// accepted, and a reader has to be told that in words.
				if rep.RejectedMutations > 0 {
					p.Line("diverged    %d rejected change(s) kept locally and not on the server (`ark sync` names each)",
						rep.RejectedMutations)
				}
				if rep.Conflicts > 0 {
					p.Line("conflicts   %d unresolved (see `ark conflict list`)", rep.Conflicts)
				}
				// Waiting, not diverged, and said in those words. Every other
				// line here is about something the operator must act on; this
				// one usually clears itself on the next pull, and saying
				// otherwise would spend the attention the lines above need.
				// It is reported at all because "usually" is not "always" —
				// the referent may not be coming.
				if rep.HeldRecords > 0 {
					p.Line("held        %d record(s) waiting for records that have not arrived yet;", rep.HeldRecords)
					p.Line("            they apply on their own once those do")
				}
				// Last, and in its own words. The two lines above are about
				// changes; this one is about whether the service still has
				// the repository, which is a different question and the only
				// one whose answer has ever been "no".
				if hr := rep.HistoryReset; hr != nil {
					p.Line("history     service at revision %d, below this checkout's %d — its history for",
						hr.ServerRevision, hr.LocalRevision)
					p.Line("            this repository was reset or lost (first seen %s)", hr.DetectedAt)
				}
			})
		},
	}
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

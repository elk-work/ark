// Package sync orchestrates a client synchronization: register, push
// pending mutations, upload artifact blobs, and pull accepted records.
// See docs/v1-spec.md §9.
package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/elk-work/ark/internal/app"
	"github.com/elk-work/ark/internal/cloud"
	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

// Issue is one rejected or conflicted mutation, for display.
type Issue struct {
	MutationID string `json:"mutation_id"`
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	Status     string `json:"status"` // rejected|conflict
	Error      string `json:"error"`
}

// Result summarizes one sync.
type Result struct {
	Pushed            int     `json:"pushed"`
	Applied           int     `json:"applied"`
	Rejected          int     `json:"rejected"`
	Conflicts         int     `json:"conflicts"`
	ArtifactsUploaded int     `json:"artifacts_uploaded"`
	ArtifactsDeduped  int     `json:"artifacts_deduped,omitempty"`
	PulledRecords     int     `json:"pulled_records"`
	PulledTombstones  int     `json:"pulled_tombstones"`
	ServerRevision    int64   `json:"server_revision"`
	Issues            []Issue `json:"issues,omitempty"`

	// HistoryReset is set when the service answered with a revision below one
	// this checkout had already synced past. The service is then not serving
	// the history this client was tracking, which is a different and worse
	// condition than being behind on it.
	HistoryReset *store.HistoryReset `json:"history_reset,omitempty"`

	// SkippedRecords counts pulled records this build cannot represent,
	// broken out by record type. Server and client versions skew by design,
	// so this is information, not an error — but it is data the operator
	// cannot see locally, so it is reported rather than dropped silently.
	SkippedRecords map[string]int `json:"skipped_records,omitempty"`
}

// Client builds the cloud client for a repository, or an offline error when
// no remote is configured.
func Client(a *app.Context) (*cloud.Client, error) {
	if a.Config.Remote == "" {
		return nil, records.Offlinef("no Ark remote configured (run `ark remote set <url>`)")
	}
	return cloud.New(a.Config.Remote)
}

// Run performs a full synchronization.
func Run(ctx context.Context, a *app.Context) (*Result, error) {
	client, err := Client(a)
	if err != nil {
		return nil, err
	}
	res := &Result{Issues: []Issue{}}

	// The cursor rides along so the service can tell a repository being
	// created from one being resurrected: above zero, this checkout is
	// asserting it holds history the service issued, and a service with no
	// database for it will refuse to create one rather than quietly stand an
	// empty one back up (elk-work/ark#66, spec §19).
	_, synced, err := a.Store.SyncCursor(ctx)
	if err != nil {
		return nil, err
	}

	if err := registerRepo(ctx, a, client, synced); err != nil {
		// A refusal to register a repository this checkout has already synced
		// is the same finding the pull below makes, arriving one call earlier:
		// the service is not serving the history this client was tracking. It
		// has to be recorded here, because refusing means there is nothing
		// left to push to or pull from — and a sync that ended with a bare
		// error would leave `ark status` reporting a clean repository, which
		// is the silence #59 exists to break.
		var arkErr *records.Error
		if synced > 0 && errors.As(err, &arkErr) && arkErr.Kind == records.KindNotFound {
			if err := a.Store.RecordHistoryReset(ctx, synced, 0); err != nil {
				return nil, err
			}
			reset, err := a.Store.HistoryReset(ctx)
			if err != nil {
				return nil, err
			}
			res.HistoryReset = reset
			return res, nil
		}
		return nil, err
	}

	if err := Push(ctx, a, client, res); err != nil {
		return nil, err
	}
	if err := uploadArtifacts(ctx, a, client, res, false); err != nil {
		return nil, err
	}
	if err := Pull(ctx, a, client, res); err != nil {
		return nil, err
	}
	return res, nil
}

// Renumber is one display number the service reassigned during a repair.
type Renumber struct {
	RecordType string `json:"record_type"`
	RecordID   string `json:"record_id"`
	From       int64  `json:"from"`
	To         int64  `json:"to"`
}

// RepairResult summarizes one `ark repair push`.
type RepairResult struct {
	// DryRun is set when nothing was changed, which is the default. The
	// command previews and stops unless a person passes --confirm.
	DryRun bool `json:"dry_run"`
	// HistoryReset is the recorded reset being repaired — the gate, and the
	// thing the preview describes.
	HistoryReset *store.HistoryReset `json:"history_reset"`
	// Replayable is how many mutations the replay covers; Requeued is how
	// many it actually put back in the queue, which is the same number unless
	// the run stopped at the preview.
	Replayable int `json:"replayable_mutations"`
	Requeued   int `json:"requeued_mutations"`
	// Renumbered lists the display numbers the service reassigned (§6.2).
	Renumbered []Renumber `json:"renumbered,omitempty"`
	// Cleared reports whether the recorded reset was retired.
	Cleared bool    `json:"history_reset_cleared"`
	Sync    *Result `json:"sync,omitempty"`
}

// Repair replays this checkout's mutation log into a service that no longer
// holds its result (docs/v1-spec.md §9.3, elk-work/ark#60).
//
// It is gated on a recorded history reset and, beyond that, on a person: with
// `confirm` false it reports what it would do and changes nothing. That is not
// timidity about a destructive command, it is the design. §9.2 says Ark does
// not reconcile a history reset, because which side is authoritative is a
// judgment about which records matter, and a tool that made it automatically
// would turn a detected incident back into an undetected one. This is the
// judgment being carried out, so it has to be a person carrying it.
//
// The order is deliberate and each step earns its place:
//
//  1. **Rewind the cursor.** Without this the pull below returns nothing at
//     all — see store.ResetSyncCursor — and it is also what makes step 2
//     legal, because a checkout claiming a cursor above zero is refused a
//     repository the service does not hold (§19).
//  2. **Register.** Creates the repository where the service lost it, and
//     no-ops where the service merely rolled back. One path for both shapes.
//  3. **Pull first.** After a reset the service usually holds *something* —
//     actors from its own re-registration, work another client pushed since —
//     and a repair's first duty is not to delete it. Pulling before pushing
//     merges those records into this checkout, and it is the only way to know
//     the revision each of them is at, which step 4 needs.
//  4. **Re-queue, rebased.** store.RequeueForReplay, on the map step 3
//     returned. The bases are the part most able to lose data quietly.
//  5. **Push, upload, pull.** An ordinary sync from there. The mutation IDs
//     are unchanged by a re-queue and the service keys idempotency on them,
//     so two clients replaying at once converge instead of duplicating, and
//     re-running a repair that half-finished resumes it.
//  6. **Clear, only on success.** Below.
func Repair(ctx context.Context, a *app.Context, confirm bool) (*RepairResult, error) {
	reset, err := a.Store.HistoryReset(ctx)
	if err != nil {
		return nil, err
	}
	if reset == nil {
		// The gate. A replay is only recovery where the service has been
		// found to have lost the history it acknowledged; anywhere else it is
		// an unprompted re-assertion of one checkout's records over everyone
		// else's, which is the thing §9.2 refuses to do automatically.
		return nil, records.Validationf(
			"no history reset is recorded for this repository, so there is nothing to repair — " +
				"`ark repair push` replays this checkout's mutation log into the service, " +
				"and is only correct after a sync has found the service serving a revision below one " +
				"this checkout had already synced past (`ark status`, `ark sync` exit 7)")
	}
	replayable, err := a.Store.ReplayableMutations(ctx)
	if err != nil {
		return nil, err
	}
	res := &RepairResult{DryRun: !confirm, HistoryReset: reset, Replayable: replayable}
	if !confirm {
		return res, nil
	}

	client, err := Client(a)
	if err != nil {
		return nil, err
	}
	if err := a.Store.ResetSyncCursor(ctx); err != nil {
		return nil, err
	}
	if err := registerRepo(ctx, a, client, 0); err != nil {
		return nil, err
	}

	sync := &Result{Issues: []Issue{}}
	held, err := pull(ctx, a, client, sync)
	if err != nil {
		return nil, err
	}
	res.Requeued, err = a.Store.RequeueForReplay(ctx, held)
	if err != nil {
		return nil, err
	}

	// Read on both sides of the push, because the service is free to move a
	// number and nothing else reports that it did (§6.2).
	before, err := a.Store.DisplayNumbers(ctx)
	if err != nil {
		return nil, err
	}
	if err := Push(ctx, a, client, sync); err != nil {
		return nil, err
	}
	if err := uploadArtifacts(ctx, a, client, sync, true); err != nil {
		return nil, err
	}
	if err := Pull(ctx, a, client, sync); err != nil {
		return nil, err
	}
	after, err := a.Store.DisplayNumbers(ctx)
	if err != nil {
		return nil, err
	}
	res.Renumbered = renumbered(before, after)
	res.Sync = sync

	// The one honest clearing condition is a repair that actually completed.
	// A rejection means the service still does not hold something this
	// checkout does — commonly an update to a record another client authored,
	// which is that client's replay to run — and a reset observed again means
	// the service is still behind. Either way the repository still needs a
	// person, so the mark stays and `ark status` goes on saying so.
	if sync.Rejected == 0 && sync.HistoryReset == nil {
		if err := a.Store.ClearHistoryReset(ctx); err != nil {
			return nil, err
		}
		res.Cleared = true
	}
	return res, nil
}

// renumbered reports the display numbers that moved across a repair, oldest
// number first so repeated runs read the same.
func renumbered(before, after map[string]int64) []Renumber {
	var out []Renumber
	for key, from := range before {
		to, ok := after[key]
		if !ok || to == from {
			continue
		}
		recordType, recordID, _ := strings.Cut(key, "/")
		out = append(out, Renumber{RecordType: recordType, RecordID: recordID, From: from, To: to})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].RecordType != out[j].RecordType {
			return out[i].RecordType < out[j].RecordType
		}
		return out[i].From < out[j].From
	})
	return out
}

// registerRepo makes the registration call every sync opens with, asserting
// `lastRevision` as this checkout's cursor.
//
// Actors travel with registration, not only with a push. They used to ride on
// api.PushRequest alone, and Push returns early when the mutation queue is
// empty — so a repository that had registered and synced but never pushed had
// no actor records on the service at all, not even the human `ark init`
// created. Every RFC-0004 write route resolves its writer against those
// records and admits a new agent only under a `delegated_by` naming a human
// the service already holds, so the first remote write into such a repository
// failed with a complaint about a field nobody typed (elk-work/ark#47).
//
// Registration is the right carrier: it already runs on every sync,
// unconditionally and idempotently, and it already pays for a full
// repository-database write on the service. Sending an otherwise-empty push
// instead — the other obvious fix — would add a second such write to every
// no-op poll, rewriting an identical database for a set of actors the server
// almost always already has. Who this checkout is belongs with what this
// checkout is, not with whether it happens to have queued work.
//
// It is also idempotent and cheap, and it validates the token.
func registerRepo(ctx context.Context, a *app.Context, client *cloud.Client, lastRevision int64) error {
	actors, err := a.Store.AllActors(ctx)
	if err != nil {
		return err
	}
	return client.RegisterRepo(ctx, api.RegisterRepositoryRequest{
		ID:            a.Config.RepositoryID,
		Name:          filepath.Base(a.Root),
		DefaultBranch: a.Git.DefaultBranch(ctx),
		GitRemoteURL:  a.Git.RemoteURL(ctx, "origin"),
		Actors:        actors,
		LastRevision:  lastRevision,
	})
}

// Push sends the pending mutation queue and applies the server's verdicts.
func Push(ctx context.Context, a *app.Context, client *cloud.Client, res *Result) error {
	muts, err := a.Store.PendingMutationRows(ctx)
	if err != nil {
		return err
	}
	if len(muts) == 0 {
		return nil
	}
	actors, err := a.Store.AllActors(ctx)
	if err != nil {
		return err
	}
	clientID, _, err := a.Store.SyncCursor(ctx)
	if err != nil {
		return err
	}

	resp, err := client.Push(ctx, api.PushRequest{
		RepositoryID: a.Config.RepositoryID,
		ClientID:     clientID,
		Actors:       actors,
		Mutations:    muts,
	})
	if err != nil {
		return err
	}
	res.Pushed += len(muts)
	res.ServerRevision = resp.ServerRevision

	byID := map[string]api.Mutation{}
	for _, m := range muts {
		byID[m.ID] = m
	}
	for _, out := range resp.Applied {
		if err := a.Store.MarkMutation(ctx, out.MutationID, "applied", ""); err != nil {
			return err
		}
		if m, ok := byID[out.MutationID]; ok && out.ServerRevision > 0 {
			if err := a.Store.SetRecordRevision(ctx, m.RecordType, m.RecordID, out.ServerRevision); err != nil {
				return err
			}
		}
		res.Applied++
	}
	for _, out := range resp.Rejected {
		m, ok := byID[out.MutationID]
		if !ok {
			// A verdict for a mutation this push did not send. Nothing to
			// mark diverged, but the verdict is still recorded rather than
			// dropped.
			if err := a.Store.MarkMutation(ctx, out.MutationID, "rejected", out.Error); err != nil {
				return err
			}
			res.Rejected++
			continue
		}
		if err := a.Store.RejectMutation(ctx, m, out.Error); err != nil {
			return err
		}
		res.Rejected++
		res.Issues = append(res.Issues, Issue{MutationID: out.MutationID,
			RecordType: m.RecordType, RecordID: m.RecordID, Status: "rejected", Error: out.Error})
	}
	for _, out := range resp.Conflicts {
		if err := a.Store.MarkMutation(ctx, out.MutationID, "conflict", out.Error); err != nil {
			return err
		}
		if m, ok := byID[out.MutationID]; ok {
			if err := a.Store.RecordConflict(ctx, m, out.Remote); err != nil {
				return err
			}
			res.Issues = append(res.Issues, Issue{MutationID: out.MutationID,
				RecordType: m.RecordType, RecordID: m.RecordID, Status: "conflict", Error: out.Error})
		}
		res.Conflicts++
	}
	return nil
}

// Pull fetches records after the local cursor and applies them atomically.
func Pull(ctx context.Context, a *app.Context, client *cloud.Client, res *Result) error {
	_, err := pull(ctx, a, client, res)
	return err
}

// pull is Pull, additionally reporting the revision the service holds for
// every record the response carried, keyed by store.RecordKey.
//
// Only a repair reads that map, and it needs it for one thing: after a
// history reset the revisions in the local database count a history nobody
// serves, so the pull response is the only place a truthful "what revision is
// this record at" can come from. See store.RequeueForReplay.
func pull(ctx context.Context, a *app.Context, client *cloud.Client, res *Result) (map[string]int64, error) {
	_, after, err := a.Store.SyncCursor(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := client.Pull(ctx, api.PullRequest{
		RepositoryID:  a.Config.RepositoryID,
		AfterRevision: after,
	})
	if err != nil {
		return nil, err
	}

	// One comparison, on every sync, for the failure that no other signal in
	// Ark can see. A repository's revision counter only ever increases, so a
	// service answering below a revision this checkout already synced past is
	// not behind — it is serving a different history, because the repository's
	// database was reset, lost, or restored from before that point.
	//
	// Every local indicator is correct and useless here: the queue is empty
	// because every mutation really was acknowledged, and it was acknowledged
	// by a service that no longer holds the result. That is how a repository
	// synced to revision 18 with seventeen applied mutations sat absent from
	// the service for six weeks while `ark status` reported it clean
	// (elk-work/ark#58).
	if resp.ServerRevision < after {
		if err := a.Store.RecordHistoryReset(ctx, after, resp.ServerRevision); err != nil {
			return nil, err
		}
		reset, err := a.Store.HistoryReset(ctx)
		if err != nil {
			return nil, err
		}
		res.HistoryReset = reset
	}

	skips, err := a.Store.ApplyPull(ctx, resp)
	if err != nil {
		return nil, err
	}
	for recordType, n := range skips {
		if res.SkippedRecords == nil {
			res.SkippedRecords = map[string]int{}
		}
		res.SkippedRecords[recordType] += n
	}
	res.PulledRecords += len(resp.Records)
	res.PulledTombstones += len(resp.Tombstones)
	res.ServerRevision = resp.ServerRevision

	held := make(map[string]int64, len(resp.Records)+len(resp.Tombstones))
	for _, rec := range resp.Records {
		held[store.RecordKey(rec.RecordType, rec.RecordID)] = rec.ServerRevision
	}
	// Tombstones count as held. The record is on the service, soft-deleted;
	// an update replayed against it is refused as `record not found`, which is
	// the loud answer and the correct one.
	for _, tomb := range resp.Tombstones {
		held[store.RecordKey(tomb.RecordType, tomb.RecordID)] = tomb.ServerRevision
	}
	return held, nil
}

// uploadArtifacts sends local artifact blobs the server has not stored.
//
// `rebuild` is set by a repair, and turns off the storage-key shortcut. A
// storage key on a local artifact records that *some* service confirmed the
// blob, and after a history reset that is not evidence about the service being
// talked to now: the `blobs` table lives in the repository database, so a
// service that lost the repository lost its record of every blob with it.
// Skipping on the old key would replay the records and leave their bytes
// behind. The upload itself stays cheap either way — content addressing means
// UploadURL answers `AlreadyStored` for a blob the service does have, and only
// the confirmation is re-sent.
func uploadArtifacts(ctx context.Context, a *app.Context, client *cloud.Client, res *Result, rebuild bool) error {
	arts, err := a.Store.ListArtifacts(ctx, "", "")
	if err != nil {
		return err
	}
	for _, art := range arts {
		if art.LocalPath == "" || (art.StorageKey != "" && !rebuild) {
			continue
		}
		path := filepath.Join(a.ArkDir, art.LocalPath)
		if _, err := os.Stat(path); err != nil {
			continue // object missing locally; another client may hold it
		}
		req := api.UploadURLRequest{
			RepositoryID: a.Config.RepositoryID,
			SHA256:       art.SHA256,
			SizeBytes:    art.SizeBytes,
			MediaType:    art.MediaType,
		}
		urlResp, err := client.UploadURL(ctx, req)
		if err != nil {
			return fmt.Errorf("upload url for %s: %w", art.Name, err)
		}
		if !urlResp.AlreadyStored {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			err = client.PutBlob(ctx, urlResp.URL, art.MediaType, f, art.SizeBytes)
			f.Close()
			if err != nil {
				return fmt.Errorf("upload %s: %w", art.Name, err)
			}
		}
		if err := client.ConfirmUpload(ctx, req); err != nil {
			return fmt.Errorf("confirm %s: %w", art.Name, err)
		}
		// Count only blobs whose bytes actually moved. Content addressing means
		// a second client holding the same artifact confirms without uploading,
		// and reporting that as "uploaded" made every replica look like it had
		// pushed data it never sent.
		if !urlResp.AlreadyStored {
			res.ArtifactsUploaded++
		} else {
			res.ArtifactsDeduped++
		}
	}
	return nil
}

// FetchArtifact downloads an artifact blob into the local object store and
// returns its local path.
func FetchArtifact(ctx context.Context, a *app.Context, art *store.Artifact) (string, error) {
	if art.StorageKey == "" {
		return "", records.NotFoundf("artifact %s has no cloud copy yet", art.ID)
	}
	client, err := Client(a)
	if err != nil {
		return "", err
	}
	urlResp, err := client.DownloadURL(ctx, api.DownloadURLRequest{
		RepositoryID: a.Config.RepositoryID,
		StorageKey:   art.StorageKey,
	})
	if err != nil {
		return "", err
	}

	rel := filepath.Join("objects", "sha256", art.SHA256[:2], art.SHA256)
	dst := filepath.Join(a.ArkDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Join(a.ArkDir, "tmp"), "download-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := client.GetBlob(ctx, urlResp.URL, tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", err
	}
	if err := a.Store.SetArtifactLocalPath(ctx, art.ID, rel); err != nil {
		return "", err
	}
	art.LocalPath = rel
	return rel, nil
}

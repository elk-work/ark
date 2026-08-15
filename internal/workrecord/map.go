package workrecord

import (
	"fmt"
	"strings"

	"github.com/elk-work/ark/internal/store"
	"github.com/elk-work/ark/pkg/api"
)

// Repo is the repository context every event carries.
type Repo struct {
	ID   string // Ark repository ULID — the routing key
	Name string // human label, display only
}

// keyRoot is the namespace for every Ark external ref. Keyed on the
// repository ULID rather than a name so a Git remote rename cannot orphan
// refs.
func (r Repo) keyRoot() string { return Provider + ":" + r.ID }

// objectKey is the stable identity of a thing (a task, a PR). Actions bind
// here, and closure resolves here.
func (r Repo) objectKey(kind, ulid string) string {
	return fmt.Sprintf("%s#%s/%s", r.keyRoot(), kind, ulid)
}

// eventKey is the identity of something that happened. Signals and captures
// reference this so a replayed pull deduplicates against external_refs'
// unique index on (provider, external_id).
func (r Repo) eventKey(kind, ulid, suffix string) string {
	return r.objectKey(kind, ulid) + ":" + suffix
}

// url builds a stable, greppable locator. Ark has no web UI in V1 by design,
// so this is not clickable; `ark task view <n>` resolves it.
func (r Repo) url(kind string, number int64, ulid string) string {
	if number > 0 {
		return fmt.Sprintf("ark://%s/%s/%d", r.ID, kind, number)
	}
	return fmt.Sprintf("ark://%s/%s/%s", r.ID, kind, ulid)
}

// Actors resolves an actor ULID to the identity Elk maps to a person.
type Actors struct {
	byID map[string]api.Actor
}

// NewActors indexes the repository's actor records.
func NewActors(list []api.Actor) *Actors {
	m := make(map[string]api.Actor, len(list))
	for _, a := range list {
		m[a.ID] = a
	}
	return &Actors{byID: m}
}

// login is the source_actor_map key: email when known, name otherwise.
func login(a api.Actor) string {
	if a.Email != "" {
		return a.Email
	}
	return a.Name
}

// Resolve turns an actor ULID into an event Actor.
//
// An agent resolves to its delegating human where one is recorded. This is
// not cosmetic: in the repositories under trial, every Ark record was written
// by an agent actor and the human actor authored nothing. Without following
// delegation the whole stream would land unattributed, discarding the one
// thing Ark knows that GitHub does not — who authorized this agent.
func (as *Actors) Resolve(id string) Actor {
	a, ok := as.byID[id]
	if !ok {
		// An actor we have no record for still needs a stable key; the ULID
		// is all we have. Elk falls back to the binding owner when the map
		// misses, so this degrades to "attributed to whoever bound the repo"
		// rather than dropping the event.
		return Actor{ProviderLogin: id, DisplayName: id, ActorType: "unknown"}
	}
	out := Actor{
		ProviderLogin: login(a),
		DisplayName:   a.Name,
		ActorType:     a.Type,
		AgentName:     a.AgentName,
	}
	if a.Type == "agent" && a.DelegatedBy != "" {
		if h, ok := as.byID[a.DelegatedBy]; ok {
			out.DelegatedByLogin = login(h)
			// The delegating human is the identity Elk should map: the agent
			// acted under their authority.
			out.ProviderLogin = login(h)
			if a.AgentName != "" {
				out.DisplayName = fmt.Sprintf("%s (via %s)", h.Name, a.AgentName)
			}
		}
	}
	return out
}

// terminalTaskStatus reports whether a task status means the work is over.
// Spec §6.2 allows open|in_progress|blocked|done|closed.
func terminalTaskStatus(s string) bool { return s == "done" || s == "closed" }

// Task maps a task's current state to the events it implies.
//
// Two events at most: the creation, and — when the task has moved on — the
// transition. The transition's event key carries the status, so the
// external_refs unique index does state-transition dedup with no projection
// table. Accepted cost: a task that goes done -> open -> done emits once.
// The GitHub adapter has the identical property on issue closure.
func Task(r Repo, as *Actors, t *store.Task) []Event {
	base := func(kind, suffix, title, body, at string) Event {
		return Event{
			Provider:     Provider,
			ExternalID:   r.eventKey("task", t.ID, suffix),
			ObjectKey:    r.objectKey("task", t.ID),
			Kind:         kind,
			Actor:        as.Resolve(t.CreatedBy),
			Title:        title,
			Body:         body,
			URL:          r.url("task", t.Number, t.ID),
			OccurredAt:   at,
			RepoFullName: r.ID,
			RepoName:     r.Name,
			Refs:         Refs{TaskNumber: t.Number, TaskID: t.ID, Status: t.Status},
		}
	}

	out := []Event{base(KindTaskOpened, "created",
		fmt.Sprintf("%s #%d: %s", r.Name, t.Number, t.Title),
		t.Body, t.CreatedAt)}

	switch {
	case terminalTaskStatus(t.Status):
		out = append(out, base(KindTaskClosed, "status/"+t.Status,
			fmt.Sprintf("%s #%d closed: %s", r.Name, t.Number, t.Title),
			t.Body, t.UpdatedAt))
	case t.Status == "blocked" || t.Status == "in_progress":
		out = append(out, base(KindTaskStatus, "status/"+t.Status,
			fmt.Sprintf("%s #%d is %s: %s", r.Name, t.Number, t.Status, t.Title),
			t.Body, t.UpdatedAt))
	}
	return out
}

// Run maps a finished agent run to a capture-worthy event.
//
// Only finished runs produce events: a start carries no story, and the
// finish carries the whole one. This is the record type that justifies the
// adapter — input, result, and the commit it produced, linked, which is
// precisely what a commit message and a PR title lose.
func Run(r Repo, as *Actors, run *store.Run, artifacts []*store.Artifact) []Event {
	if run.Status == "" || run.Status == "running" || run.FinishedAt == "" {
		return nil
	}
	var b strings.Builder
	if run.InputSummary != "" {
		fmt.Fprintf(&b, "**Asked:** %s\n\n", run.InputSummary)
	}
	if run.ResultSummary != "" {
		fmt.Fprintf(&b, "**Result:** %s\n\n", run.ResultSummary)
	}
	if run.BranchName != "" {
		fmt.Fprintf(&b, "Branch `%s`", run.BranchName)
		if run.ResultCommitSHA != "" {
			fmt.Fprintf(&b, " at `%s`", shortSHA(run.ResultCommitSHA))
		}
		b.WriteString("\n")
	}
	var names []string
	for _, a := range artifacts {
		names = append(names, a.Name)
	}
	if len(names) > 0 {
		fmt.Fprintf(&b, "Evidence: %s\n", strings.Join(names, ", "))
	}

	agent := run.AgentName
	if agent == "" {
		agent = "agent"
	}
	return []Event{{
		Provider:     Provider,
		ExternalID:   r.eventKey("run", run.ID, "finished"),
		ObjectKey:    r.objectKey("run", run.ID),
		Kind:         KindRunFinished,
		Actor:        as.Resolve(run.CreatedBy),
		Title:        fmt.Sprintf("%s: %s run %s", r.Name, agent, run.Status),
		Body:         strings.TrimRight(b.String(), "\n"),
		URL:          r.url("run", 0, run.ID),
		OccurredAt:   run.FinishedAt,
		RepoFullName: r.ID,
		RepoName:     r.Name,
		Refs: Refs{
			RunID: run.ID, TaskID: run.TaskID, ThreadID: run.ThreadID,
			Status: run.Status, Branch: run.BranchName,
			BaseCommitSHA: run.BaseCommitSHA, ResultCommitSHA: run.ResultCommitSHA,
			ExitCode: run.ExitCode, ArtifactNames: names,
		},
	}}
}

// PR maps a pull request's current state. A merged PR is a capture and runs
// its body through Elk's `Closes elk:<id>` resolver downstream — the parser
// is provider-agnostic and needs no Ark-specific handling.
func PR(r Repo, as *Actors, pr *store.PullRequest) []Event {
	base := func(kind, suffix, title, at string) Event {
		return Event{
			Provider:     Provider,
			ExternalID:   r.eventKey("pr", pr.ID, suffix),
			ObjectKey:    r.objectKey("pr", pr.ID),
			Kind:         kind,
			Actor:        as.Resolve(pr.CreatedBy),
			Title:        title,
			Body:         pr.Body,
			URL:          r.url("pr", pr.Number, pr.ID),
			OccurredAt:   at,
			RepoFullName: r.ID,
			RepoName:     r.Name,
			Refs: Refs{
				PRNumber: pr.Number, PRID: pr.ID, TaskID: pr.TaskID,
				Status: pr.Status, Branch: pr.HeadBranch,
				BaseCommitSHA: pr.BaseCommitSHA, MergeCommitSHA: pr.MergeCommitSHA,
			},
		}
	}
	out := []Event{base(KindPROpened, "opened",
		fmt.Sprintf("%s PR #%d: %s", r.Name, pr.Number, pr.Title), pr.CreatedAt)}

	if pr.Status == "merged" {
		at := pr.MergedAt
		if at == "" {
			at = pr.UpdatedAt
		}
		out = append(out, base(KindPRMerged, "merged",
			fmt.Sprintf("%s PR #%d merged: %s", r.Name, pr.Number, pr.Title), at))
	}
	return out
}

// Review maps a submitted review. Reviews are immutable (spec §6.8), so a
// first sighting is the event and no diffing is needed.
func Review(r Repo, as *Actors, rev *store.Review, pr *store.PullRequest) []Event {
	label := strings.ReplaceAll(rev.State, "_", " ")
	title := fmt.Sprintf("%s: review %s", r.Name, label)
	if pr != nil {
		title = fmt.Sprintf("%s PR #%d %s: %s", r.Name, pr.Number, label, pr.Title)
	}
	e := Event{
		Provider:     Provider,
		ExternalID:   r.eventKey("review", rev.ID, "submitted"),
		Kind:         KindReviewSubmitted,
		Actor:        as.Resolve(rev.CreatedBy),
		Title:        title,
		Body:         rev.Body,
		OccurredAt:   rev.CreatedAt,
		RepoFullName: r.ID,
		RepoName:     r.Name,
		Refs:         Refs{PRID: rev.PullRequestID, ReviewState: rev.State},
	}
	// A review binds to its pull request's object key so approval and merge
	// meet on the same row.
	e.ObjectKey = r.objectKey("pr", rev.PullRequestID)
	if pr != nil {
		e.URL = r.url("pr", pr.Number, pr.ID)
		e.Refs.PRNumber = pr.Number
	} else {
		e.URL = r.url("pr", 0, rev.PullRequestID)
	}
	return []Event{e}
}

// Promotion maps an activated promotion — something reached an environment.
func Promotion(r Repo, as *Actors, p *store.Promotion) []Event {
	target := p.Environment
	if p.Service != "" {
		target = p.Service + " → " + p.Environment
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Promoted to **%s**", p.Environment)
	if p.Service != "" {
		fmt.Fprintf(&b, " (%s)", p.Service)
	}
	b.WriteString("\n")
	if p.MergeCommitSHA != "" {
		fmt.Fprintf(&b, "Commit `%s`\n", shortSHA(p.MergeCommitSHA))
	}
	return []Event{{
		Provider:     Provider,
		ExternalID:   r.eventKey("promotion", p.ID, "activated"),
		ObjectKey:    r.objectKey("promotion", p.ID),
		Kind:         KindPromotionActive,
		Actor:        as.Resolve(p.CreatedBy),
		Title:        fmt.Sprintf("%s: %s", r.Name, target),
		Body:         strings.TrimRight(b.String(), "\n"),
		URL:          r.url("promotion", 0, p.ID),
		OccurredAt:   p.ActivatedAt,
		RepoFullName: r.ID,
		RepoName:     r.Name,
		Refs: Refs{
			Environment: p.Environment, PRID: p.PullRequestID,
			MergeCommitSHA: p.MergeCommitSHA,
		},
	}}
}

// Thread maps a closed thread to one capture. Individual messages are
// deliberately not ingested: a design conversation is one memory, not forty.
func Thread(r Repo, as *Actors, t *store.Thread, messageCount int) []Event {
	if t.Status != "closed" {
		return nil
	}
	at := t.ClosedAt
	if at == "" {
		at = t.CreatedAt
	}
	return []Event{{
		Provider:     Provider,
		ExternalID:   r.eventKey("thread", t.ID, "closed"),
		ObjectKey:    r.objectKey("thread", t.ID),
		Kind:         KindThreadClosed,
		Actor:        as.Resolve(t.CreatedBy),
		Title:        fmt.Sprintf("%s: thread closed — %s", r.Name, t.Title),
		Body:         fmt.Sprintf("%d messages.", messageCount),
		URL:          r.url("thread", 0, t.ID),
		OccurredAt:   at,
		RepoFullName: r.ID,
		RepoName:     r.Name,
		Refs:         Refs{ThreadID: t.ID, TaskID: t.TaskID, MessageCount: messageCount},
	}}
}

// Comment maps a comment. Comments are append-only (spec §6.3); a correction
// arrives as a new comment carrying supersedes_id, which is a distinct ULID
// and therefore a distinct event — correct, and self-deduplicating.
//
// Elk gates these behind fencing_config.comments, off by default, the same
// noise lever the GitHub adapter uses.
func Comment(r Repo, as *Actors, c *store.Comment, parentTitle string, parentNumber int64) []Event {
	title := fmt.Sprintf("%s: comment", r.Name)
	if parentTitle != "" {
		title = fmt.Sprintf("%s #%d: %s", r.Name, parentNumber, parentTitle)
	}
	return []Event{{
		Provider:     Provider,
		ExternalID:   r.eventKey("comment", c.ID, "created"),
		ObjectKey:    r.objectKey(objectKindFor(c.ParentType), c.ParentID),
		Kind:         KindComment,
		Actor:        as.Resolve(c.CreatedBy),
		Title:        title,
		Body:         c.Body,
		URL:          r.url(objectKindFor(c.ParentType), parentNumber, c.ParentID),
		OccurredAt:   c.CreatedAt,
		RepoFullName: r.ID,
		RepoName:     r.Name,
		Refs:         Refs{TaskNumber: parentNumber, TaskID: c.ParentID},
	}}
}

// objectKindFor maps an Ark parent_type onto the object-key namespace.
func objectKindFor(parentType string) string {
	switch parentType {
	case "pull_request":
		return "pr"
	case "agent_run":
		return "run"
	case "review":
		return "review"
	default:
		return "task"
	}
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

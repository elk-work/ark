package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/elk-work/ark/internal/records"
	"github.com/elk-work/ark/pkg/api"
)

// syncedRepo is a repository that has registered with the service and pushed
// at least once.
//
// The push used to be load-bearing: actors travelled only on a push, so a
// checkout that had synced without one was unknown to the service and every
// remote write into it was refused (elk-work/ark#47). Registration carries
// actors now, so the seeded task is only setup — a repository with a task in
// it — and no longer a workaround.
func syncedRepo(t *testing.T) (dir, url string) {
	t.Helper()
	url = startSyncServer(t)
	dir = gitRepo(t)
	ark(t, dir, "init")
	ark(t, dir, "remote", "set", url)
	ark(t, dir, "task", "create", "-t", "something for the first push")
	ark(t, dir, "sync")
	return dir, url
}

// TestRepoSetCorrectsRegisteredMetadata walks elk-work/ark#11 end to end: a
// repository registered under the basename of wherever it was checked out,
// renamed deliberately afterwards, with the correction surviving the next
// sync.
func TestRepoSetCorrectsRegisteredMetadata(t *testing.T) {
	dir, _ := syncedRepo(t)

	var local struct {
		RepositoryID string `json:"repository_id"`
		Root         string `json:"root"`
	}
	arkJSON(t, dir, &local, "status")

	var before api.RepositoryMetadata
	arkJSON(t, dir, &before, "repo", "show")
	// The name the service holds is the basename of wherever this checkout
	// happens to live, which is the whole complaint.
	if before.Name != filepath.Base(local.Root) {
		t.Fatalf("registered name = %q, want the checkout's basename %q",
			before.Name, filepath.Base(local.Root))
	}
	// Addressed by the bound repository id, never by that directory name.
	if before.ID != local.RepositoryID {
		t.Errorf("repo show reported %s, want this repository's id %s", before.ID, local.RepositoryID)
	}

	var set api.RepositoryResponse
	arkJSON(t, dir, &set, "repo", "set",
		"--name", "scout", "--git-remote", "https://github.com/elk-work/scout.git")
	if !set.Changed {
		t.Error("a real correction reported changed = false")
	}
	if set.Repository.Name != "scout" {
		t.Errorf("after set: %+v", set.Repository)
	}

	// It is readable afterwards, and a later sync does not undo it —
	// registration only ever backfills.
	ark(t, dir, "sync")
	var after api.RepositoryMetadata
	arkJSON(t, dir, &after, "repo", "show")
	if after.Name != "scout" || after.GitRemoteURL != "https://github.com/elk-work/scout.git" {
		t.Errorf("after a later sync: %+v", after)
	}
	if after.Revision <= before.Revision {
		t.Errorf("revision %d did not advance past %d", after.Revision, before.Revision)
	}

	// Asserting what is already true changes nothing and mints no revision.
	var repeat api.RepositoryResponse
	arkJSON(t, dir, &repeat, "repo", "set", "--name", "scout")
	if repeat.Changed {
		t.Error("a repeat reported changed = true")
	}
	if repeat.ServerRevision != after.Revision {
		t.Errorf("a repeat moved the revision %d -> %d", after.Revision, repeat.ServerRevision)
	}

	// The human rendering names the outcome rather than leaving the operator
	// to infer it from a revision number.
	out := ark(t, dir, "repo", "set", "--name", "scout")
	if !strings.Contains(out, "nothing changed") {
		t.Errorf("repeat output did not say it was a no-op: %s", out)
	}
}

// Only the flags passed are asserted; the rest of the record is left alone.
func TestRepoSetIsPartial(t *testing.T) {
	dir, _ := syncedRepo(t)
	ark(t, dir, "repo", "set", "--name", "watch", "--default-branch", "trunk")

	ark(t, dir, "repo", "set", "--git-remote", "git@github.com:elk-work/watch.git")

	var after api.RepositoryMetadata
	arkJSON(t, dir, &after, "repo", "show")
	if after.Name != "watch" || after.DefaultBranch != "trunk" {
		t.Errorf("setting the remote disturbed the other fields: %+v", after)
	}
	if after.GitRemoteURL != "git@github.com:elk-work/watch.git" {
		t.Errorf("remote = %q", after.GitRemoteURL)
	}
	// An explicit empty value is the one way to clear a field, for a
	// repository that genuinely has no remote.
	ark(t, dir, "repo", "set", "--git-remote", "")
	arkJSON(t, dir, &after, "repo", "show")
	if after.GitRemoteURL != "" {
		t.Errorf("remote after clearing = %q", after.GitRemoteURL)
	}
	if after.Name != "watch" {
		t.Errorf("clearing the remote disturbed the name: %+v", after)
	}
}

// Spec §22: invalid input is exit 2 and an unknown repository is exit 3,
// whether the check ran locally or the service made it.
func TestRepoSetExitCodes(t *testing.T) {
	dir, _ := syncedRepo(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"nothing to set", []string{"repo", "set"}, 2},
		{"blank name", []string{"repo", "set", "--name", "   "}, 2},
		{"implausible remote", []string{"repo", "set", "--git-remote", "not a remote"}, 2},
		{"invalid branch", []string{"repo", "set", "--default-branch", "no spaces allowed"}, 2},
		{"repo that is not a ULID", []string{"repo", "set", "--repo", "scout", "--name", "x"}, 2},
		{"unknown repository", []string{"repo", "set", "--repo", "01ABSENTREP000000000000000", "--name", "x"}, 3},
		{"show an unknown repository", []string{"repo", "show", "--repo", "01ABSENTREP000000000000000"}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := arkErr(t, dir, c.args...)
			if err == nil {
				t.Fatalf("ark %v should have failed, got:\n%s", c.args, out)
			}
			if got := records.ExitCode(err); got != c.want {
				t.Errorf("ark %v: exit code = %d, want %d: %v", c.args, got, c.want, err)
			}
		})
	}
}

// Without a remote there is no service holding a repository record, and that
// is an offline condition (exit 6), not a missing one.
func TestRepoWithoutARemote(t *testing.T) {
	dir := gitRepo(t)
	ark(t, dir, "init")
	for _, args := range [][]string{{"repo", "show"}, {"repo", "set", "--name", "scout"}} {
		_, err := arkErr(t, dir, args...)
		if err == nil {
			t.Fatalf("ark %v with no remote should have failed", args)
		}
		if got := records.ExitCode(err); got != 6 {
			t.Errorf("ark %v: exit code = %d, want 6 (offline): %v", args, got, err)
		}
	}
}

// `ark repo grant` end to end, against a real service.
//
// The client here holds the service token, which carries implicit admin
// everywhere until elk-work/ark#54 retires it — so this exercises the
// command, the route and the store without needing an identity provider. What
// each level *does* is pinned server-side, in internal/server/grants_test.go.
func TestRepoGrantIssuesRevokesAndLists(t *testing.T) {
	dir, _ := syncedRepo(t)

	// A grant to somebody who has never logged in is the point of keying on
	// an email, and the command has to say that is what happened.
	out := ark(t, dir, "repo", "grant", "newcomer@example.com", "--write")
	if !strings.Contains(out, "first login") || !strings.Contains(out, "write") {
		t.Errorf("granting to an unknown address did not say it is waiting: %s", out)
	}

	var list api.GrantListResponse
	arkJSON(t, dir, &list, "repo", "grants")
	if len(list.Grants) != 1 || list.Grants[0].Email != "newcomer@example.com" ||
		list.Grants[0].Level != api.GrantWrite || !list.Grants[0].Pending {
		t.Fatalf("grants after issuing one: %+v", list.Grants)
	}

	// Re-granting corrects the level rather than adding a second row.
	ark(t, dir, "repo", "grant", "newcomer@example.com", "--read")
	arkJSON(t, dir, &list, "repo", "grants")
	if len(list.Grants) != 1 || list.Grants[0].Level != api.GrantRead {
		t.Fatalf("after re-granting: %+v", list.Grants)
	}

	// Revoking is idempotent: removing what nobody holds is a success.
	for i := 0; i < 2; i++ {
		ark(t, dir, "repo", "grant", "newcomer@example.com", "--revoke")
	}
	arkJSON(t, dir, &list, "repo", "grants")
	if len(list.Grants) != 0 {
		t.Errorf("grants after revoking: %+v", list.Grants)
	}
}

// Spec §22: a level that was not asked for, or two that were, is invalid
// input — exit 2, and refused before anything reaches the service.
func TestRepoGrantExitCodes(t *testing.T) {
	dir, _ := syncedRepo(t)
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no level", []string{"repo", "grant", "someone@example.com"}, 2},
		{"two levels", []string{"repo", "grant", "someone@example.com", "--read", "--admin"}, 2},
		{"a level and a revoke", []string{"repo", "grant", "someone@example.com", "--read", "--revoke"}, 2},
		{"no email", []string{"repo", "grant", "--read"}, 2},
		{"unknown repository", []string{"repo", "grants", "--repo", "01ABSENTREP000000000000000"}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := arkErr(t, dir, c.args...)
			if err == nil {
				t.Fatalf("ark %v should have failed, got:\n%s", c.args, out)
			}
			if got := records.ExitCode(err); got != c.want {
				t.Errorf("ark %v: exit code = %d, want %d: %v", c.args, got, c.want, err)
			}
		})
	}
}

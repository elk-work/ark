package git

import (
	"context"
	"strconv"
	"strings"
)

// Reading a commit range. Ark keeps the history of the work around the
// source, so a run's record points at two SHAs and Git holds everything
// between them. Principle 001: shell out with machine-readable flags rather
// than growing a second view of the object graph.

// HasCommit reports whether sha names a commit present in this repository.
// A record can outlive the objects it points at — a shallow clone, a pruned
// branch, or a run recorded on another machine — and "the commit is not here"
// is a different answer from "nothing changed".
func (r *Repo) HasCommit(ctx context.Context, sha string) bool {
	if sha == "" {
		return false
	}
	_, err := r.Run(ctx, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// FileStat is one file's line counts in a diff. Insertions and Deletions
// are -1 for a binary file, matching git's own "-" in --numstat.
type FileStat struct {
	Path       string `json:"path"`
	OldPath    string `json:"old_path,omitempty"` // set when the file was renamed
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	Binary     bool   `json:"binary,omitempty"`
}

// DiffStat returns per-file line counts between two commits, detecting
// renames. NUL-delimited so paths with spaces or quotes survive.
func (r *Repo) DiffStat(ctx context.Context, base, head string) ([]FileStat, error) {
	out, err := r.Run(ctx, "diff", "--numstat", "-z", "--find-renames",
		"--no-ext-diff", base, head)
	if err != nil {
		return nil, err
	}
	// --numstat -z emits "ins\tdel\tpath\0", and for a rename
	// "ins\tdel\0old\0new\0" — the paths move out of the tab-joined field.
	fields := strings.Split(out.Stdout, "\x00")
	var stats []FileStat
	for i := 0; i < len(fields); i++ {
		head := fields[i]
		if strings.TrimSpace(head) == "" {
			continue
		}
		parts := strings.SplitN(head, "\t", 3)
		if len(parts) < 2 {
			continue
		}
		var fs FileStat
		fs.Insertions, fs.Deletions, fs.Binary = countPair(parts[0], parts[1])
		if len(parts) == 3 && parts[2] != "" {
			fs.Path = parts[2]
		} else if i+2 < len(fields) {
			fs.OldPath, fs.Path = fields[i+1], fields[i+2]
			i += 2
		}
		if fs.Path == "" {
			continue
		}
		stats = append(stats, fs)
	}
	return stats, nil
}

// countPair parses git's numstat counts, where "-" means binary.
func countPair(ins, del string) (int, int, bool) {
	if ins == "-" || del == "-" {
		return -1, -1, true
	}
	i, _ := strconv.Atoi(ins)
	d, _ := strconv.Atoi(del)
	return i, d, false
}

// DiffUnified returns the unified patch between two commits with the given
// context, renames detected and colour off.
func (r *Repo) DiffUnified(ctx context.Context, base, head string, contextLines int) (string, error) {
	if contextLines < 0 {
		contextLines = 3
	}
	out, err := r.Run(ctx, "diff", "--no-color", "--no-ext-diff", "--find-renames",
		"--unified="+strconv.Itoa(contextLines), base, head)
	if err != nil {
		return "", err
	}
	return out.Stdout, nil
}

// Commit is one commit's identifying metadata.
type Commit struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date"` // ISO 8601, as Git reports it
	Subject string `json:"subject"`
}

// commitSep is a delimiter that cannot occur in any of the fields.
const commitSep = "\x1f"

// CommitsBetween lists the commits reachable from head but not from base,
// oldest last (git log order).
func (r *Repo) CommitsBetween(ctx context.Context, base, head string) ([]Commit, error) {
	out, err := r.Run(ctx, "log", "--no-color",
		"--format=%H"+commitSep+"%an"+commitSep+"%aI"+commitSep+"%s", base+".."+head)
	if err != nil {
		return nil, err
	}
	var commits []Commit
	for _, line := range strings.Split(out.Stdout, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.SplitN(line, commitSep, 4)
		if len(f) != 4 {
			continue
		}
		commits = append(commits, Commit{SHA: f[0], Author: f[1], Date: f[2], Subject: f[3]})
	}
	return commits, nil
}

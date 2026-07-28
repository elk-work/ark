// Package buildinfo resolves the version string a binary reports.
package buildinfo

import "runtime/debug"

// Dev is the version a binary reports when nothing better is known.
const Dev = "dev"

// Resolve reports the version to display, most authoritative first:
//
//  1. an explicit build stamp, set with -ldflags "-X main.version=...";
//  2. the module version Go records for "go install <pkg>@v0.1.0";
//  3. "dev".
//
// When the version was not stamped explicitly, the VCS data Go embeds
// automatically is appended — a short commit plus "+dirty" for a modified
// tree — so a plain "go build" still identifies what it was built from. A
// stamped version is returned untouched, because the stamp (from
// "git describe --tags --always --dirty") already carries that detail.
func Resolve(stamped string) string {
	if stamped != "" && stamped != Dev {
		return stamped
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return Dev
	}
	base := Dev
	if v := info.Main.Version; v != "" && v != "(devel)" {
		base = v
	}
	var revision string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if revision == "" {
		return base
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if dirty {
		revision += "+dirty"
	}
	return base + " (" + revision + ")"
}

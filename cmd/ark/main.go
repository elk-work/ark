// Command ark is the Ark CLI: a local-first, agent-native work record
// system that sits beside Git.
package main

import (
	"os"

	"github.com/elk-work/ark/internal/buildinfo"
	"github.com/elk-work/ark/internal/cli"
)

// version is set at build time via -ldflags "-X main.version=...". Left
// unset, buildinfo.Resolve falls back to the module version or the VCS
// stamp Go embeds on its own.
var version = buildinfo.Dev

func main() {
	os.Exit(cli.Execute(buildinfo.Resolve(version)))
}

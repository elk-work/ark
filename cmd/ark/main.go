// Command ark is the Ark CLI: a local-first, agent-native work record
// system that sits beside Git.
package main

import (
	"os"

	"github.com/elkproject/ark/internal/cli"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(cli.Execute(version))
}

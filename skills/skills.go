// Package skills embeds the agent guidance Ark installs into a repository.
//
// The canonical copy lives here as a plain Markdown file so it is readable on
// its own; `ark init` writes it into the repository at .claude/skills/, where
// coding agents load it automatically.
package skills

import "embed"

//go:embed ark/SKILL.md
var FS embed.FS

// Ark is the path of the Ark skill inside FS.
const Ark = "ark/SKILL.md"

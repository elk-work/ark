// Package migrations embeds the numbered SQL migration files.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

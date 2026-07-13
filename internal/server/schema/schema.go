// Package schema embeds the sync service's PostgreSQL schema.
package schema

import _ "embed"

//go:embed schema.sql
var SQL string

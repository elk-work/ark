package migrations

import _ "embed"

// UISessions is applied only by the authentication store. Root numbered SQL
// migrations remain the client record schema.
//
//go:embed auth/0001_ui_sessions.sql
var UISessions string

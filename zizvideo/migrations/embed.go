// Package migrations embeds the versioned SQL applied at startup.
package migrations

import "embed"

// FS holds the .sql files in this directory.
//
//go:embed *.sql
var FS embed.FS

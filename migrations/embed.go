// Package migrations embeds the goose SQL migrations applied by the bot at
// startup. Until the first tagged release, 00001_initial.sql may
// be edited in place; after that, every schema change is a new file.
package migrations

import "embed"

// FS holds every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS

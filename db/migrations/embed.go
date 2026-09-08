// Package migrations embeds the goose SQL migrations so a compiled binary
// can bring an empty database up to date with no files on disk beside it.
package migrations

import "embed"

// FS holds every .sql migration in this directory, in goose's expected
// layout (the files sit at the root of the FS).
//
//go:embed *.sql
var FS embed.FS

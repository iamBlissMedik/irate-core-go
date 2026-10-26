// Package migrations embeds the goose SQL migrations into the binary, so the
// service and the migrate command never depend on files existing on disk.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

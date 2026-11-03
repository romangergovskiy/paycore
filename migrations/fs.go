// Package migrations embeds Goose SQL files so the migrate command and tests
// use the same files that live in the repository.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

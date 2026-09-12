// Package migrations embeds the versioned PostgreSQL schema in the relaytale binary.
package migrations

import "embed"

//go:embed *.sql
var Files embed.FS

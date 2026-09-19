// Package migrations embeds the SQL migration files so that cmd/migrate is a
// single self-contained binary -- there is no directory of .sql files to ship
// next to it, and no path to get wrong in a container.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

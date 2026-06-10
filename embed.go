// Package truengine is the module root. It exists solely to embed repo-level
// assets that //go:embed cannot reach from subdirectories (an embed path may
// never contain ".."): the SQL migrations applied by cmd/engine at startup.
package truengine

import "embed"

// MigrationsFS holds the full migrations/ directory. cmd/engine feeds it to
// golang-migrate's iofs source via iofs.New(MigrationsFS, "migrations"), so
// the binary is self-contained — no migrations volume to mount, no drift
// between the deployed schema and the deployed code.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

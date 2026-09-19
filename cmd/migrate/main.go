// Command migrate applies the database migrations and exits.
//
// It is a separate binary from cmd/api on purpose: schema changes run once, as
// the schema owner, before the API rolls. The API connects as app_user, which
// owns nothing and could not apply a migration if it tried.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	"github.com/miqbalhamdani/sewain-api/db/migrations"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

func main() {
	down := flag.Int("down", 0, "roll back this many migrations instead of applying. Local use only -- production is forward-only.")
	flag.Parse()

	if err := run(*down); err != nil {
		slog.Error("migrate failed", "error", err)
		os.Exit(1)
	}
}

func run(down int) error {
	m, err := open(config.DatabaseURL())
	if err != nil {
		return err
	}
	// Close reports the source and database errors separately; neither is
	// actionable here beyond logging, and the migration itself has already
	// committed or rolled back.
	defer func() {
		if srcErr, dbErr := m.Close(); srcErr != nil || dbErr != nil {
			slog.Warn("closing migrator", "source", srcErr, "database", dbErr)
		}
	}()

	if down > 0 {
		err = m.Steps(-down)
	} else {
		err = m.Up()
	}
	// Already at the target version. Not a failure -- migrate runs on every
	// deploy and most deploys change no schema.
	if err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}

	version, dirty, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		slog.Info("no migrations applied")
		return nil
	}
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		// A migration failed part-way and the version was not committed. Nothing
		// automatic can fix this: someone has to look at what half-applied.
		return fmt.Errorf("schema is dirty at version %d -- inspect and force a version before retrying", version)
	}
	slog.Info("migrations applied", "version", version)
	return nil
}

// open builds a migrator over the embedded SQL files.
func open(dsn string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	// golang-migrate picks its database driver from the URL scheme, and the
	// pgx/v5 driver registers itself as "pgx5". Everything else in this repo
	// speaks postgres://, so rewrite the scheme rather than keeping a second
	// DSN in the environment purely for this binary.
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	u.Scheme = "pgx5"

	m, err := migrate.NewWithSourceInstance("iofs", src, u.String())
	if err != nil {
		return nil, fmt.Errorf("open migrator: %w", err)
	}
	return m, nil
}

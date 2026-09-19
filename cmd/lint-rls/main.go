// Command lint-rls fails when an owner-owned table is not protected by row
// level security.
//
// It runs against a live database rather than by reading the migration files,
// because what matters is the state PostgreSQL is actually in -- a migration
// that was written correctly and never applied would pass a source-level check
// and still leave the table open.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/miqbalhamdani/sewain-api/internal/db"
	"github.com/miqbalhamdani/sewain-api/internal/platform/config"
)

func main() {
	if err := run(context.Background()); err != nil {
		slog.Error("lint-rls failed", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// The owner's DSN: this is a check on the schema, run in the same place
	// migrations are.
	store, err := db.New(ctx, config.DatabaseURL())
	if err != nil {
		return err
	}
	defer store.Close()

	problems, err := store.CheckOwnerRLS(ctx)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Fprintln(os.Stderr, "  "+p.String())
		}
		return fmt.Errorf("%d owner table(s) are not protected", len(problems))
	}

	slog.Info("every owner table is protected")
	return nil
}

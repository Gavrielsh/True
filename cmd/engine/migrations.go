package main

// migrations.go runs the embedded schema migrations at startup, BEFORE the
// engine is wired. FAIL FAST: a stale or dirty schema must never serve
// traffic — at this TPS a single missed constraint is unbounded financial
// exposure.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres" // registers postgres://
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	truengine "github.com/Gavrielsh/True"
)

// RunMigrations applies all pending embedded migrations against postgresURL.
// Returns nil when the schema is already current (logged at DEBUG) or after
// applying the pending set (count logged at INFO). Any failure — including a
// dirty version left by a crashed deploy — is returned for main to abort on.
func RunMigrations(ctx context.Context, logger *slog.Logger, postgresURL string) error {
	src, err := iofs.New(truengine.MigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("open embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", src, postgresURL)
	if err != nil {
		return fmt.Errorf("init migrator: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			logger.WarnContext(ctx, "migrator source close", slog.String("error", srcErr.Error()))
		}
		if dbErr != nil {
			logger.WarnContext(ctx, "migrator db close", slog.String("error", dbErr.Error()))
		}
	}()

	// golang-migrate's Up() has no ctx parameter; bridge cancellation through
	// its GracefulStop channel so a SIGTERM during boot stops cleanly after
	// the in-flight migration instead of being killed mid-DDL.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			m.GracefulStop <- true
		case <-done:
		}
	}()

	before, _, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return fmt.Errorf("read schema version: %w", err)
	}

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			logger.DebugContext(ctx, "schema already up to date",
				slog.Uint64("version", uint64(before)))
			return nil
		}
		return fmt.Errorf("apply migrations: %w", err)
	}

	after, dirty, err := m.Version()
	if err != nil {
		return fmt.Errorf("read post-migration version: %w", err)
	}
	if dirty {
		// Up() returned nil yet the version is dirty — never proceed.
		return fmt.Errorf("schema version %d is dirty after migration", after)
	}

	applied, err := countVersionsBetween(src, before, after)
	if err != nil {
		return fmt.Errorf("count applied migrations: %w", err)
	}
	logger.InfoContext(ctx, "migrations applied",
		slog.Int("applied", applied),
		slog.Uint64("from_version", uint64(before)),
		slog.Uint64("to_version", uint64(after)),
	)
	return nil
}

// countVersionsBetween walks the source driver's version chain and counts
// versions v with before < v <= after. Versions need not be contiguous, so
// "after - before" would be wrong in general.
func countVersionsBetween(src source.Driver, before, after uint) (int, error) {
	v, err := src.First()
	if err != nil {
		return 0, fmt.Errorf("first version: %w", err)
	}
	count := 0
	for {
		if v > before && v <= after {
			count++
		}
		next, err := src.Next(v)
		if err != nil {
			// The iofs driver returns fs.ErrNotExist when no later version
			// exists — the normal end of the chain.
			if errors.Is(err, fs.ErrNotExist) {
				break
			}
			return 0, fmt.Errorf("next version after %d: %w", v, err)
		}
		v = next
	}
	return count, nil
}

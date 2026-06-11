//go:build integration

package main

// Integration coverage for RunMigrations against a real Postgres. The
// migration SQL uses plpgsql, ENUMs, and native partitioning, so it is NOT
// portable to sqlite — run with:
//
//	TEST_POSTGRES_URL=postgres://user:pass@localhost:5432/true_test?sslmode=disable \
//	  go test -tags integration ./cmd/engine/
//
// The target database is DROPPED to a clean schema by the down migrations at
// the end of the test — point it at a throwaway database only.

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestRunMigrations_Integration(t *testing.T) {
	url := os.Getenv("TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("TEST_POSTGRES_URL not set; skipping integration test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	logger := testLogger()

	// Fresh apply: every embedded migration runs.
	if err := RunMigrations(ctx, logger, url); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// Idempotent re-run: must take the ErrNoChange path and return nil.
	if err := RunMigrations(ctx, logger, url); err != nil {
		t.Fatalf("second run (expected no-change): %v", err)
	}
}

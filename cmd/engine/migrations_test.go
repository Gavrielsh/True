package main

import (
	"context"
	"io"
	"io/fs"
	"log/slog"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4/source/iofs"

	truengine "github.com/Gavrielsh/True"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// The embedded migrations must be a complete, paired set: every up has a
// down, and the set is non-empty. Catches a botched //go:embed pattern at
// test time instead of at deploy time.
func TestMigrationsEmbed_PairedUpDown(t *testing.T) {
	t.Parallel()
	entries, err := fs.ReadDir(truengine.MigrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	ups := map[string]bool{}
	downs := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".up.sql"):
			ups[strings.TrimSuffix(name, ".up.sql")] = true
		case strings.HasSuffix(name, ".down.sql"):
			downs[strings.TrimSuffix(name, ".down.sql")] = true
		default:
			t.Errorf("unexpected embedded file %q (want *.up.sql / *.down.sql)", name)
		}
	}
	if len(ups) == 0 {
		t.Fatal("no up migrations embedded")
	}
	for base := range ups {
		if !downs[base] {
			t.Errorf("migration %s has no down counterpart", base)
		}
	}
	for base := range downs {
		if !ups[base] {
			t.Errorf("migration %s has no up counterpart", base)
		}
	}
}

// countVersionsBetween walks the real embedded chain, so this pins both the
// helper's logic and the iofs source wiring.
func TestCountVersionsBetween(t *testing.T) {
	t.Parallel()
	src, err := iofs.New(truengine.MigrationsFS, "migrations")
	if err != nil {
		t.Fatalf("iofs: %v", err)
	}
	t.Cleanup(func() { _ = src.Close() })

	total, err := countVersionsBetween(src, 0, ^uint(0))
	if err != nil {
		t.Fatalf("count all: %v", err)
	}
	if total < 8 {
		t.Fatalf("embedded migration count: got %d want >= 8", total)
	}

	cases := []struct {
		name          string
		before, after uint
		want          int
	}{
		{"none_when_equal", 3, 3, 0},
		{"single_step", 2, 3, 1},
		{"first_three_from_fresh", 0, 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := countVersionsBetween(src, tc.before, tc.after)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			if got != tc.want {
				t.Errorf("count(%d,%d): got %d want %d", tc.before, tc.after, got, tc.want)
			}
		})
	}
}

// FAIL FAST: an unusable database URL must surface as an error, never a hang
// or a silent skip.
func TestRunMigrations_BadURLFailsFast(t *testing.T) {
	t.Parallel()
	err := RunMigrations(context.Background(), testLogger(), "bogus://nowhere")
	if err == nil {
		t.Fatal("expected error for unsupported database URL")
	}
}

package db_test

import (
	"context"
	"testing"

	"mino/internal/db"
	"mino/internal/testutil"
)

func TestMigrationsAreOrderedAndUnique(t *testing.T) {
	migs, err := db.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 {
		t.Fatal("no migrations embedded")
	}
	for i := 1; i < len(migs); i++ {
		if migs[i].Version <= migs[i-1].Version {
			t.Fatalf("migrations out of order: %s after %s", migs[i].Name, migs[i-1].Name)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testutil.DB(t) // already migrated once
	ctx := context.Background()

	applied, err := db.Migrate(ctx, pool, testutil.Logger())
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second run applied %v, want nothing", applied)
	}

	migs, _ := db.Migrations()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migs) {
		t.Fatalf("schema_migrations has %d rows, want %d", n, len(migs))
	}

	// Seed data is present exactly once.
	var roles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM roles`).Scan(&roles); err != nil {
		t.Fatal(err)
	}
	if roles != 3 {
		t.Fatalf("roles = %d, want 3", roles)
	}
}

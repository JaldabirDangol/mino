// Package testutil provides an isolated PostgreSQL schema per test.
//
// Tests that need a database are skipped unless MINO_TEST_DATABASE_URL is set, e.g.
//
//	MINO_TEST_DATABASE_URL=postgres://mino:mino@localhost:5440/mino?sslmode=disable go test ./...
package testutil

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"mino/internal/db"
)

// Logger discards output.
func Logger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// DB returns a pool bound to a fresh, migrated schema that is dropped when the test ends.
func DB(t testing.TB) *pgxpool.Pool { t.Helper(); return DBWith(t, nil) }

// DBWith is DB with a hook to adjust the pool configuration (e.g. to install a query tracer).
func DBWith(t testing.TB, configure func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("MINO_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MINO_TEST_DATABASE_URL not set; skipping database test")
	}
	ctx := context.Background()
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	schema := "test_" + hex.EncodeToString(b)

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.MaxConns = 4
	if configure != nil {
		configure(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close(ctx)
	})
	if _, err := db.Migrate(ctx, pool, Logger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

// Site inserts a site and returns its id.
func Site(t testing.TB, pool *pgxpool.Pool, slug string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sites (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&id); err != nil {
		t.Fatalf("create site: %v", err)
	}
	return id
}

// QueryCounter is a pgx tracer that counts executed statements.
type QueryCounter struct{ n atomic.Int64 }

func (c *QueryCounter) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	c.n.Add(1)
	return ctx
}

func (c *QueryCounter) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// Reset sets the count to zero.
func (c *QueryCounter) Reset() { c.n.Store(0) }

// Count returns the number of statements since the last reset.
func (c *QueryCounter) Count() int64 { return c.n.Load() }

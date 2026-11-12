// Package postgrestest gives tests a real Postgres to work against.
//
// Tests that need one call Open. It reads the address of a test database
// from the environment and skips the test when the variable is not set, so
// that "go test ./..." passes on a machine with no database. compose.yaml at
// the root of the module starts a suitable one.
package postgrestest

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// URLVariable names the environment variable holding the address of the
// test database, for example
// "postgres://grafsol:grafsol@localhost:54329/grafsol".
const URLVariable = "GRAFSOL_TEST_POSTGRES_URL"

// schemas numbers the schemas this process has created.
var schemas atomic.Int64

// Open returns a connection pool whose connections all work inside a schema
// of their own: empty to begin with, invisible to every other test, and
// dropped when the test ends. The pgvector extension is available in it.
//
// Open does not create the grafsol tables; a test does that itself by
// calling postgres.CreateSchema, which keeps this package free of imports
// from the code it helps to test.
func Open(t testing.TB) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv(URLVariable)
	if url == "" {
		t.Skipf("%s is not set; compose.yaml shows how to start a test database", URLVariable)
	}

	// Not the test's own context: that one ends before cleanup functions
	// run, and the cleanup below still needs to talk to the database.
	ctx := context.Background()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("postgrestest: connecting to the test database: %v", err)
	}

	// The extension is installed once, in the public schema, and shared by
	// every test. Test binaries of different packages run at the same time,
	// so they take turns installing it.
	const setupLock = `hashtextextended('grafsol: test setup', 0)`
	for _, statement := range []string{
		`select pg_advisory_lock(` + setupLock + `)`,
		`create extension if not exists vector with schema public`,
		`select pg_advisory_unlock(` + setupLock + `)`,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("postgrestest: %s: %v", statement, err)
		}
	}

	schema := pgx.Identifier{fmt.Sprintf("grafsol_test_%d_%d", os.Getpid(), schemas.Add(1))}.Sanitize()
	for _, statement := range []string{
		`drop schema if exists ` + schema + ` cascade`, // left behind by a run that crashed
		`create schema ` + schema,
	} {
		if _, err := admin.Exec(ctx, statement); err != nil {
			t.Fatalf("postgrestest: %s: %v", statement, err)
		}
	}

	config, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("postgrestest: reading %s: %v", URLVariable, err)
	}
	// Unqualified names resolve to the test's schema first, then to public,
	// where the vector type lives.
	config.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("postgrestest: opening a pool: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, `drop schema if exists `+schema+` cascade`); err != nil {
			t.Errorf("postgrestest: dropping %s: %v", schema, err)
		}
		admin.Close(ctx)
	})
	return pool
}

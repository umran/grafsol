package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/postgres/postgrestest"
)

// TestStatementsUseTheirIndexes asks Postgres how it would run each read
// statement and checks that the answer involves the index the statement was
// written for. A statement that drifts away from its index would still
// return correct rows, only slowly, so nothing else would catch it.
func TestStatementsUseTheirIndexes(t *testing.T) {
	pool := postgrestest.Open(t)
	ctx := t.Context()
	if err := CreateSchema(ctx, pool, 4); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}

	cases := []struct {
		name    string
		sql     string
		args    []any
		index   string
		ordered bool // rows must come out of the index already in order
	}{
		{"outEdges", outEdges, []any{"doc:s:a", "", "", 10}, "edges_pkey", true},
		{"outEdgesOfKind", outEdgesOfKind, []any{"doc:s:a", "links_to", "", 10}, "edges_pkey", true},
		{"inEdges", inEdges, []any{"doc:s:a", "", "", 10}, "edges_in", true},
		{"inEdgesOfKind", inEdgesOfKind, []any{"doc:s:a", "links_to", "", 10}, "edges_in", true},
		{"expandOut", expandOut, []any{[]string{"doc:s:a", "doc:s:b"}}, "edges_pkey", false},
		{"expandOutOfKind", expandOutOfKind, []any{[]string{"doc:s:a", "doc:s:b"}, "links_to"}, "edges_pkey", false},
		{"expandIn", expandIn, []any{[]string{"doc:s:a", "doc:s:b"}}, "edges_in", false},
		{"expandInOfKind", expandInOfKind, []any{[]string{"doc:s:a", "doc:s:b"}, "links_to"}, "edges_in", false},
		{"nearestDocuments", nearestDocuments, []any{[]float32{1, 0, 0, 0}, 5}, "vertices_doc_embedding", true},
		{"unseenMarks", unseenMarks, []any{"github", int64(100), int64(0), "", 10}, "ledger_marks_unseen", true},
		{"selectVertices", selectVertices, []any{[]string{"doc:s:a", "doc:s:b"}}, "vertices_pkey", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := explain(t, ctx, pool, c.sql, c.args...)
			if !strings.Contains(plan, c.index) {
				t.Errorf("the plan does not use %s:\n%s", c.index, plan)
			}
			if c.ordered && strings.Contains(plan, "Sort") {
				t.Errorf("the plan sorts, although %s already holds the rows in order:\n%s", c.index, plan)
			}
		})
	}
}

// rankedNeighbors draws on three indexes at once: one for the edges that
// leave the vertex, one for those that arrive, and one to find the vertex at
// the far end of each.
func TestRankedNeighborsReadsItsIndexes(t *testing.T) {
	pool := postgrestest.Open(t)
	ctx := t.Context()
	if err := CreateSchema(ctx, pool, 4); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}

	plan := explain(t, ctx, pool, rankedNeighbors, "doc:s:a", []float32{1, 0, 0, 0}, float64(-1), "", "", int32(0), 10)
	for _, index := range []string{"edges_pkey", "edges_in", "vertices_pkey"} {
		if !strings.Contains(plan, index) {
			t.Errorf("the plan does not use %s:\n%s", index, plan)
		}
	}
}

// explain returns Postgres' plan for a statement.
//
// The tables are empty, and for an empty table reading the whole thing is
// always cheapest. Turning sequential and bitmap scans off for the duration
// shows the plan Postgres falls back on, which is the one that matters once
// the tables have grown.
func explain(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background())

	for _, setting := range []string{"enable_seqscan", "enable_bitmapscan"} {
		if _, err := tx.Exec(ctx, setLocal, setting, "off"); err != nil {
			t.Fatalf("turning %s off: %v", setting, err)
		}
	}
	rows, err := tx.Query(ctx, "explain (costs off) "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	lines, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	return strings.Join(lines, "\n")
}

// NearestDocuments relies on a batch of statements running as one
// transaction: its first two statements change settings "until the end of
// the transaction", and the third must still see them. This checks both
// halves of that: the setting reaches a later statement of the batch, and it
// is gone once the batch is over.
func TestBatchedSettingsLastExactlyOneBatch(t *testing.T) {
	pool := postgrestest.Open(t)
	ctx := t.Context()
	if err := CreateSchema(ctx, pool, 4); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}

	// One connection throughout, so "afterwards" is on the same session.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	const read = `select coalesce(current_setting('hnsw.ef_search', true), '')`

	batch := &pgx.Batch{}
	batch.Queue(setLocal, "hnsw.ef_search", "123")
	batch.Queue(read)
	results := conn.SendBatch(ctx, batch)
	if _, err := results.Exec(); err != nil {
		t.Fatalf("setting: %v", err)
	}
	var during string
	if err := results.QueryRow().Scan(&during); err != nil {
		t.Fatalf("reading the setting inside the batch: %v", err)
	}
	if err := results.Close(); err != nil {
		t.Fatalf("closing the batch: %v", err)
	}
	if during != "123" {
		t.Errorf("inside the batch the setting reads %q, want 123: the batch is not one transaction", during)
	}

	var after string
	if err := conn.QueryRow(ctx, read).Scan(&after); err != nil {
		t.Fatalf("reading the setting after the batch: %v", err)
	}
	if after == "123" {
		t.Errorf("the setting outlived the batch")
	}
}

// Record leaves a mark alone when it is already stored exactly as given,
// reading of the clock included. If it stopped doing so the mark would read
// the same, only at the cost of a rewritten row, so nothing else would catch
// it. A row's ctid is its place in the table, which changes whenever the row
// is rewritten.
func TestRecordingAnUnchangedMarkRewritesNothing(t *testing.T) {
	pool := postgrestest.Open(t)
	ctx := t.Context()
	if err := CreateSchema(ctx, pool, 4); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	ledger := NewLedger(pool)
	mark := storage.Mark{ID: "doc:s:a", Source: "s", Version: 1, Fingerprint: "abc"}

	record := func() {
		t.Helper()
		if err := ledger.Record(ctx, mark); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	place := func() string {
		t.Helper()
		var ctid string
		err := pool.QueryRow(ctx, `select ctid::text from ledger_marks where id = $1`, string(mark.ID)).Scan(&ctid)
		if err != nil {
			t.Fatalf("locating the mark's row: %v", err)
		}
		return ctid
	}

	record()
	first := place()
	record()
	if again := place(); again != first {
		t.Errorf("recording the same mark again moved its row from %s to %s: the row was rewritten", first, again)
	}

	// Once the clock has moved, the same mark is no longer stored as given:
	// its Seen is out of date, and the row must be rewritten.
	if _, err := ledger.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	record()
	if after := place(); after == first {
		t.Errorf("recording the mark after a tick left its row at %s: its Seen was not brought up to date", first)
	}
}

// The sync engine claims the graph's embedding space for every document it
// stores. All but the first of those claims must cost nothing: no row
// written, so no dead row versions for vacuum to clean up.
func TestClaimingARecordedSpaceRewritesNothing(t *testing.T) {
	pool := postgrestest.Open(t)
	ctx := t.Context()
	if err := CreateSchema(ctx, pool, 4); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	store := New(pool)
	const space = "acme/embedder-a@1f2e; 4 dimensions"

	// place returns where the record's row is and which transaction wrote
	// it. Either changes if the row is rewritten.
	place := func() string {
		t.Helper()
		var where string
		err := pool.QueryRow(ctx, `select ctid::text || ' by ' || xmin::text from embedding_space`).Scan(&where)
		if err != nil {
			t.Fatalf("locating the record's row: %v", err)
		}
		return where
	}

	if err := store.ClaimEmbeddingSpace(ctx, space); err != nil {
		t.Fatalf("ClaimEmbeddingSpace: %v", err)
	}
	first := place()
	for range 3 {
		if err := store.ClaimEmbeddingSpace(ctx, space); err != nil {
			t.Fatalf("claiming the same space again: %v", err)
		}
	}
	if err := store.ClaimEmbeddingSpace(ctx, "acme/embedder-b@9c0d; 4 dimensions"); err == nil {
		t.Fatal("another space was claimed")
	}
	if again := place(); again != first {
		t.Errorf("later claims moved the record's row from %s to %s: the row was rewritten", first, again)
	}
}

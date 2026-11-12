// Package postgres is the storage backend built on PostgreSQL with the
// pgvector extension. It provides a storage.Store and
// storage.DocumentSearcher (Store), a storage.Ledger (Ledger) and a faster
// way to expand a traversal frontier (Store.Expand).
//
// The whole design fits in two files worth reading first: schema.sql defines
// the tables and indexes and says why each is the way it is, and queries.go
// lists every SQL statement the package sends.
//
// The caller owns the connection pool:
//
//	pool, err := pgxpool.New(ctx, databaseURL)
//	err = postgres.CreateSchema(ctx, pool, 1536)
//	store := postgres.New(pool)
//	ledger := postgres.NewLedger(pool)
package postgres

import (
	"cmp"
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

//go:embed schema.sql
var schemaSQL string

// MaxEmbeddingDimensions is the longest embedding the schema can hold:
// pgvector's HNSW index handles vectors of at most 2,000 dimensions.
const MaxEmbeddingDimensions = 2000

// CreateSchema creates the tables and indexes in schema.sql if they do not
// exist yet. embeddingDimensions is the length of the embeddings that will
// be stored; it is fixed from then on, because a vector index can only
// compare vectors of one length.
//
// Calling CreateSchema again is harmless, but calling it with a different
// number of dimensions than the existing schema has is an error.
func CreateSchema(ctx context.Context, pool *pgxpool.Pool, embeddingDimensions int) error {
	if embeddingDimensions < 1 || embeddingDimensions > MaxEmbeddingDimensions {
		return fmt.Errorf("postgres: embeddings must have between 1 and %d dimensions, not %d", MaxEmbeddingDimensions, embeddingDimensions)
	}
	ddl := strings.ReplaceAll(schemaSQL, "{{dimensions}}", strconv.Itoa(embeddingDimensions))

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		// Two processes creating the schema at the same moment would trip
		// over each other inside Postgres; this lock makes them take turns.
		if _, err := tx.Exec(ctx, lockKey, "grafsol: create schema"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, ddl); err != nil {
			return err
		}

		// "create table if not exists" leaves an existing table as it is,
		// so check that the embedding column has the length asked for. For
		// a vector column, atttypmod is its number of dimensions.
		var existing int
		err := tx.QueryRow(ctx, `
select atttypmod
  from pg_attribute
 where attrelid = 'vertices'::regclass and attname = 'embedding'`).Scan(&existing)
		if err != nil {
			return err
		}
		if existing != embeddingDimensions {
			return fmt.Errorf("the schema already exists with %d-dimensional embeddings, not %d", existing, embeddingDimensions)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("postgres: creating the schema: %w", err)
	}
	return nil
}

// Store is a storage.Store and storage.DocumentSearcher kept in Postgres.
// Its Expand method also makes it a traverse.Expander.
type Store struct {
	pool *pgxpool.Pool
}

var (
	_ storage.Store            = (*Store)(nil)
	_ storage.DocumentSearcher = (*Store)(nil)
)

// New returns a Store that uses the given pool. The schema must already
// exist; see CreateSchema.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// PutVertices implements storage.Store. The whole batch travels in one round
// trip and is applied in one transaction.
func (s *Store) PutVertices(ctx context.Context, vertices []graph.Vertex) error {
	if len(vertices) == 0 {
		return nil
	}

	// If a vertex appears twice, the later one wins, as it would if the
	// batch were applied one vertex at a time.
	latest := make(map[string]vertexRow, len(vertices))
	for _, v := range vertices {
		row, err := encodeVertex(v)
		if err != nil {
			return err
		}
		latest[row.id] = row
	}

	// Rows are written in ID order so that two overlapping batches always
	// lock rows in the same order and cannot deadlock each other.
	rows := make([]vertexRow, 0, len(latest))
	for _, row := range latest {
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b vertexRow) int { return cmp.Compare(a.id, b.id) })

	batch := &pgx.Batch{}
	for _, row := range rows {
		batch.Queue(upsertVertex, row.id, row.kind, row.attrs, row.embedding)
	}
	if err := s.pool.SendBatch(ctx, batch).Close(); err != nil {
		return fmt.Errorf("postgres: storing %d vertices: %w", len(rows), err)
	}
	return nil
}

// GetVertices implements storage.Store.
func (s *Store) GetVertices(ctx context.Context, ids []graph.ID) (map[graph.ID]graph.Vertex, error) {
	found := make(map[graph.ID]graph.Vertex, len(ids))
	if len(ids) == 0 {
		return found, nil
	}

	rows, err := s.pool.Query(ctx, selectVertices, toStrings(ids))
	if err != nil {
		return nil, fmt.Errorf("postgres: fetching vertices: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var row vertexRow
		if err := rows.Scan(&row.id, &row.kind, &row.attrs, &row.embedding); err != nil {
			return nil, fmt.Errorf("postgres: fetching vertices: %w", err)
		}
		vertex, err := decodeVertex(row)
		if err != nil {
			return nil, err
		}
		found[vertex.VertexID()] = vertex
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: fetching vertices: %w", err)
	}
	return found, nil
}

// DeleteVertices implements storage.Store.
func (s *Store) DeleteVertices(ctx context.Context, ids []graph.ID) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := s.pool.Exec(ctx, deleteVertices, toStrings(ids)); err != nil {
		return fmt.Errorf("postgres: deleting vertices: %w", err)
	}
	return nil
}

// PutEdges implements storage.Store. The whole batch is one statement.
func (s *Store) PutEdges(ctx context.Context, edges []graph.Edge) error {
	if len(edges) == 0 {
		return nil
	}

	// If an edge appears twice, the later one wins. (Postgres would refuse
	// a statement that writes the same row twice.)
	latest := make(map[graph.Edge]graph.Edge, len(edges))
	for _, e := range edges {
		if err := e.Validate(); err != nil {
			return err
		}
		latest[e.Triple()] = e
	}

	// Sorted for the same reason as in PutVertices: a fixed locking order.
	unique := make([]graph.Edge, 0, len(latest))
	for _, e := range latest {
		unique = append(unique, e)
	}
	slices.SortFunc(unique, graph.Edge.Compare)

	src, kind, dst := edgeColumns(unique)
	summary := make([]string, len(unique))
	for i, e := range unique {
		summary[i] = e.Summary
	}
	if _, err := s.pool.Exec(ctx, upsertEdges, src, kind, dst, summary); err != nil {
		return fmt.Errorf("postgres: storing %d edges: %w", len(unique), err)
	}
	return nil
}

// DeleteEdges implements storage.Store.
func (s *Store) DeleteEdges(ctx context.Context, edges []graph.Edge) error {
	if len(edges) == 0 {
		return nil
	}
	src, kind, dst := edgeColumns(edges)
	if _, err := s.pool.Exec(ctx, deleteEdges, src, kind, dst); err != nil {
		return fmt.Errorf("postgres: deleting edges: %w", err)
	}
	return nil
}

// edgeColumns turns a list of edges into three parallel arrays, the form in
// which a batch of edges is passed to Postgres.
func edgeColumns(edges []graph.Edge) (src, kind, dst []string) {
	src = make([]string, len(edges))
	kind = make([]string, len(edges))
	dst = make([]string, len(edges))
	for i, e := range edges {
		src[i], kind[i], dst[i] = string(e.From), string(e.Kind), string(e.To)
	}
	return src, kind, dst
}

// Edges implements storage.Store.
func (s *Store) Edges(ctx context.Context, q storage.EdgeQuery) (storage.EdgePage, error) {
	if err := q.Validate(); err != nil {
		return storage.EdgePage{}, err
	}
	var after storage.EdgePosition
	if err := storage.DecodeCursor(q.Cursor, &after); err != nil {
		return storage.EdgePage{}, err
	}

	// Ask for one row more than the page holds. Whether that extra row
	// comes back is how we know if there is a next page.
	size := q.PageSize()
	sql, args := edgesStatement(q, after, size+1)

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return storage.EdgePage{}, fmt.Errorf("postgres: listing the %s edges of %s: %w", q.Direction, q.Vertex, err)
	}
	edges, err := pgx.CollectRows(rows, scanEdge)
	if err != nil {
		return storage.EdgePage{}, fmt.Errorf("postgres: listing the %s edges of %s: %w", q.Direction, q.Vertex, err)
	}

	page := storage.EdgePage{Edges: edges}
	if len(edges) > size {
		page.Edges = edges[:size]
		last := page.Edges[size-1]
		page.Next = storage.EncodeCursor(storage.EdgePosition{Kind: last.Kind, Neighbor: last.Far(q.Direction)})
	}
	return page, nil
}

// edgesStatement picks the statement that answers an edge query, and its
// arguments. There is one statement per combination of direction and
// "one kind or every kind", so that each reads its index in the best way.
func edgesStatement(q storage.EdgeQuery, after storage.EdgePosition, limit int) (sql string, args []any) {
	vertex, afterKind, afterNeighbor := string(q.Vertex), string(after.Kind), string(after.Neighbor)
	switch {
	case q.Direction == graph.Out && q.Kind == "":
		return outEdges, []any{vertex, afterKind, afterNeighbor, limit}
	case q.Direction == graph.Out:
		return outEdgesOfKind, []any{vertex, string(q.Kind), afterNeighbor, limit}
	case q.Kind == "":
		return inEdges, []any{vertex, afterKind, afterNeighbor, limit}
	default:
		return inEdgesOfKind, []any{vertex, string(q.Kind), afterNeighbor, limit}
	}
}

// scanEdge reads one row of (src, kind, dst, summary).
func scanEdge(row pgx.CollectableRow) (graph.Edge, error) {
	var src, kind, dst, summary string
	if err := row.Scan(&src, &kind, &dst, &summary); err != nil {
		return graph.Edge{}, err
	}
	return graph.Edge{From: graph.ID(src), Kind: graph.EdgeKind(kind), To: graph.ID(dst), Summary: summary}, nil
}

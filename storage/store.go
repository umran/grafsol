// Package storage defines what the rest of the module needs from the place
// where the graph is kept. There are four contracts:
//
//   - Store keeps vertices and edges.
//   - DocumentSearcher finds documents by embedding similarity.
//   - NeighborSearcher reads the neighbours of a vertex, most similar first.
//   - Ledger keeps the sync engine's bookkeeping, apart from the graph.
//
// The contracts are small and shaped like the graph rather than like any one
// database, so each backend is free to choose its own physical layout. The
// backends live in sub-packages (memory, postgres), and every one of them
// must pass the suites in storagetest.
package storage

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/umran/grafsol/graph"
)

// Store keeps the vertices and edges of the graph, and remembers which
// embedding space the vertices' embeddings are in.
//
// Every method that reads or writes vertices or edges takes or returns whole
// batches, so a backend can serve each call in one round trip. Writes are
// idempotent: repeating a call leaves the store as the first call left it.
// Implementations must be safe for concurrent use.
type Store interface {
	// PutVertices creates the given vertices, replacing any stored vertex
	// that has the same ID.
	PutVertices(ctx context.Context, vertices []graph.Vertex) error

	// GetVertices returns the stored vertices among ids, keyed by ID. An ID
	// with no stored vertex is simply absent from the map.
	GetVertices(ctx context.Context, ids []graph.ID) (map[graph.ID]graph.Vertex, error)

	// DeleteVertices removes the vertices with the given IDs. IDs with no
	// stored vertex are ignored. Edges are left alone: an edge may point at
	// a vertex that is not stored.
	DeleteVertices(ctx context.Context, ids []graph.ID) error

	// PutEdges adds the given edges. An edge that is already stored keeps
	// its place and takes the new Summary. Neither end of an edge needs to
	// be a stored vertex.
	PutEdges(ctx context.Context, edges []graph.Edge) error

	// DeleteEdges removes the edges with the given (From, Kind, To); their
	// Summary is ignored. Edges that are not stored are ignored.
	DeleteEdges(ctx context.Context, edges []graph.Edge) error

	// Edges lists the edges that touch one vertex in one direction,
	// optionally of one kind, a page at a time. Edges come back ordered by
	// kind and then by the ID of the vertex at the other end, comparing
	// bytes. Passing each page's Next as the following query's Cursor,
	// until Next is empty, yields every matching edge exactly once.
	Edges(ctx context.Context, q EdgeQuery) (EdgePage, error)

	// EmbeddingSpace returns the name of the embedding space that the
	// graph's embeddings are in, or "" if none has been claimed yet.
	//
	// An embedding space is one model, set up one way. Embeddings can be
	// compared only if they are in the same space, so a graph has to hold
	// to one. A space is known by a name that its embedder makes up (see
	// model.Embedder); here the name is only ever compared.
	EmbeddingSpace(ctx context.Context) (string, error)

	// ClaimEmbeddingSpace declares that embeddings in the named space are
	// about to be stored. The first claim made on a graph is recorded for
	// good. A later claim of the same space does nothing. A claim of any
	// other space fails with an error that wraps ErrOtherSpace and names
	// both.
	//
	// A Store does not check the embeddings it is given against the
	// record: it cannot tell what made a vector. It keeps the record, and
	// the sync and search engines consult it.
	ClaimEmbeddingSpace(ctx context.Context, space string) error
}

// ErrOtherSpace reports that embeddings from two embedding spaces have met:
// a graph whose embeddings are in one space, and embeddings from another
// that were about to be stored in it or searched for in it.
var ErrOtherSpace = errors.New("storage: embeddings of another embedding space")

// CheckSpaceName reports what is wrong with the name of an embedding space,
// or nil if it can be claimed.
func CheckSpaceName(space string) error {
	if strings.TrimSpace(space) == "" {
		return errors.New("storage: an embedding space must have a name")
	}
	return nil
}

// DefaultPageSize is how many edges a page holds when EdgeQuery.Limit is zero.
const DefaultPageSize = 1000

// EdgeQuery selects the edges that touch one vertex in one direction.
type EdgeQuery struct {
	Vertex    graph.ID
	Direction graph.Direction // graph.Out or graph.In
	Kind      graph.EdgeKind  // empty matches every kind

	// Limit is the most edges to return in this page; zero means
	// DefaultPageSize. It sizes pages and never cuts a listing short: the
	// rest is reached through Cursor.
	Limit int

	// Cursor is the Next of the previous page; empty asks for the first page.
	Cursor string
}

// EdgePage is one page of an edge listing.
type EdgePage struct {
	Edges []graph.Edge

	// Next continues the listing. It is empty exactly when no matching
	// edges remain after this page.
	Next string
}

// Validate reports what is wrong with the query, or nil. Backends call it
// before running a query so that they all reject the same inputs.
func (q EdgeQuery) Validate() error {
	if err := q.Vertex.Validate(); err != nil {
		return fmt.Errorf("storage: edge query: %w", err)
	}
	if err := q.Direction.Validate(); err != nil {
		return fmt.Errorf("storage: edge query: %w", err)
	}
	if q.Kind != "" {
		if err := q.Kind.Validate(); err != nil {
			return fmt.Errorf("storage: edge query: %w", err)
		}
	}
	if q.Limit < 0 {
		return errors.New("storage: edge query: negative Limit")
	}
	return nil
}

// PageSize returns Limit, or DefaultPageSize when Limit is zero.
func (q EdgeQuery) PageSize() int {
	if q.Limit == 0 {
		return DefaultPageSize
	}
	return q.Limit
}

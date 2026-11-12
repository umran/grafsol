package storage

import (
	"context"

	"github.com/umran/grafsol/graph"
)

// DocumentSearcher finds documents by how close their embeddings are to a
// query vector. It is separate from Store because not every place that can
// keep a graph can also index vectors; a backend implements it if it can.
type DocumentSearcher interface {
	// NearestDocuments returns the k documents whose embeddings are closest
	// to query by cosine distance, closest first. Documents without an
	// embedding are never returned, so fewer than k matches come back when
	// fewer than k documents have one. The query must have the same number
	// of dimensions as the stored embeddings.
	NearestDocuments(ctx context.Context, query []float32, k int) ([]Match, error)
}

// Match is one result of a similarity search.
type Match struct {
	ID graph.ID

	// Distance is the cosine distance between the query and the document's
	// embedding: 0 for the same direction, 1 for unrelated, 2 for opposite.
	Distance float32
}

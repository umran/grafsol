package storage

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/umran/grafsol/graph"
)

// NeighborSearcher reads the surroundings of one vertex: every edge that
// touches it, in either direction, with the vertex at the far end of each,
// ordered by how close that vertex is to a query vector.
//
// It is what lets a search look at the most promising neighbours of a
// vertex first, and read the rest only if it needs them. Like
// DocumentSearcher it is separate from Store: a backend that can rank inside
// the database implements it, and NeighborsByScan serves the others.
type NeighborSearcher interface {
	// Neighbors lists the edges touching q.Vertex whose far end is a stored
	// vertex, a page at a time, ordered by (distance, edge kind, far-end ID,
	// direction). Following Next until it is empty yields every such edge
	// exactly once. Two kinds of edge are left out because they lead
	// nowhere: an edge whose far end is not stored, and an edge from the
	// vertex to itself.
	Neighbors(ctx context.Context, q NeighborQuery) (NeighborPage, error)
}

// NeighborQuery asks for one page of the neighbours of a vertex.
type NeighborQuery struct {
	Vertex graph.ID

	// Near is the vector to rank by: neighbours whose embeddings are closer
	// to it come first. Without it, neighbours come in (edge kind, far-end
	// ID, direction) order.
	Near []float32

	// Limit is the most neighbours to return; zero means DefaultPageSize.
	Limit int

	// Cursor is NeighborPage.Next of the previous page, or empty for the
	// first page. The rest of the query must be the same as it was then.
	Cursor string
}

// Validate reports what is wrong with the query, or nil if it can be run.
func (q NeighborQuery) Validate() error {
	if err := q.Vertex.Validate(); err != nil {
		return fmt.Errorf("storage: neighbor query: %w", err)
	}
	if q.Limit < 0 {
		return errors.New("storage: neighbor query: negative Limit")
	}
	if len(q.Near) > 0 && !slices.ContainsFunc(q.Near, func(x float32) bool { return x != 0 }) {
		return errors.New("storage: neighbor query: Near is all zeroes, which is no direction to rank by")
	}
	return nil
}

// PageSize returns Limit, or DefaultPageSize when Limit is zero.
func (q NeighborQuery) PageSize() int {
	if q.Limit == 0 {
		return DefaultPageSize
	}
	return q.Limit
}

// NeighborPage is one page of neighbours.
type NeighborPage struct {
	Neighbors []Neighbor

	// Next is the cursor for the following page; empty when nothing is left.
	Next string
}

// Neighbor is one edge of the vertex that was asked about, with the vertex
// at its far end.
type Neighbor struct {
	Edge graph.Edge

	// Vertex is the far end of Edge: the end that is not the vertex asked
	// about. It comes without its embedding, which a caller that only wants
	// to read the neighbour has no use for.
	Vertex graph.Vertex

	// Distance is the cosine distance between the query's Near and the
	// vertex's embedding, as in Match. It is Farthest when there is nothing
	// to measure: no Near, or a vertex without an embedding.
	Distance float32
}

// Direction says which way Edge runs as seen from the vertex that was asked
// about: Out when the neighbour is the edge's To, In when it is its From.
func (n Neighbor) Direction() graph.Direction {
	if n.Edge.To == n.Vertex.VertexID() {
		return graph.Out
	}
	return graph.In
}

// NeighborPosition is where a page of neighbours ended: the sort key of the
// last neighbour returned. Since neighbours are listed in that order, the
// next page is everything after this position. Backends are free to use it
// as the content of their cursors.
type NeighborPosition struct {
	Distance  float64         `json:"distance"`
	Kind      graph.EdgeKind  `json:"kind"`
	Neighbor  graph.ID        `json:"neighbor"`
	Direction graph.Direction `json:"direction"`
}

// BeforeAllNeighbors is the position of a first page: it sorts before every
// neighbour there can be, because no distance is negative.
var BeforeAllNeighbors = NeighborPosition{Distance: -1}

// Position returns the neighbour's sort key.
func (n Neighbor) Position() NeighborPosition {
	return NeighborPosition{
		Distance:  float64(n.Distance),
		Kind:      n.Edge.Kind,
		Neighbor:  n.Vertex.VertexID(),
		Direction: n.Direction(),
	}
}

// Compare orders positions the way Neighbors lists them.
func (p NeighborPosition) Compare(other NeighborPosition) int {
	return cmp.Or(
		cmp.Compare(p.Distance, other.Distance),
		cmp.Compare(p.Kind, other.Kind),
		cmp.Compare(p.Neighbor, other.Neighbor),
		cmp.Compare(p.Direction, other.Direction),
	)
}

// NeighborsByScan returns a NeighborSearcher for any Store, built on Edges
// and GetVertices alone.
//
// Each call reads the whole neighbourhood of the vertex, measures every
// neighbour and sorts them, however small the page asked for. That is
// correct for every backend and perfectly good for vertices with a modest
// number of edges, but it makes each page of a hub cost as much as the whole
// hub. A backend that can rank inside the database should implement
// NeighborSearcher itself, as the Postgres backend does.
func NeighborsByScan(store Store) NeighborSearcher {
	return neighborScan{store: store}
}

type neighborScan struct {
	store Store
}

func (s neighborScan) Neighbors(ctx context.Context, q NeighborQuery) (NeighborPage, error) {
	if err := q.Validate(); err != nil {
		return NeighborPage{}, err
	}
	after := BeforeAllNeighbors
	if err := DecodeCursor(q.Cursor, &after); err != nil {
		return NeighborPage{}, err
	}

	edges, err := s.edgesOf(ctx, q.Vertex)
	if err != nil {
		return NeighborPage{}, err
	}
	neighbors, err := s.measure(ctx, q, edges)
	if err != nil {
		return NeighborPage{}, err
	}
	slices.SortFunc(neighbors, func(a, b Neighbor) int { return a.Position().Compare(b.Position()) })

	// Skip everything up to and including where the previous page ended.
	start := sort.Search(len(neighbors), func(i int) bool { return neighbors[i].Position().Compare(after) > 0 })
	neighbors = neighbors[start:]

	page := NeighborPage{Neighbors: neighbors}
	if size := q.PageSize(); len(neighbors) > size {
		page.Neighbors = neighbors[:size]
		page.Next = EncodeCursor(page.Neighbors[size-1].Position())
	}
	return page, nil
}

// edgesOf returns every edge that touches a vertex, from either end, however
// many pages that takes. An edge from the vertex to itself is left out.
func (s neighborScan) edgesOf(ctx context.Context, vertex graph.ID) ([]graph.Edge, error) {
	var edges []graph.Edge
	for _, direction := range []graph.Direction{graph.Out, graph.In} {
		query := EdgeQuery{Vertex: vertex, Direction: direction}
		for {
			page, err := s.store.Edges(ctx, query)
			if err != nil {
				return nil, err
			}
			for _, edge := range page.Edges {
				if edge.From != edge.To {
					edges = append(edges, edge)
				}
			}
			if page.Next == "" {
				break
			}
			query.Cursor = page.Next
		}
	}
	return edges, nil
}

// measure fetches the vertex at the far end of each edge and measures its
// distance from the query's Near. Edges whose far end is not stored are
// dropped.
func (s neighborScan) measure(ctx context.Context, q NeighborQuery, edges []graph.Edge) ([]Neighbor, error) {
	farEnd := func(edge graph.Edge) graph.ID {
		if edge.From == q.Vertex {
			return edge.To
		}
		return edge.From
	}

	ids := make([]graph.ID, 0, len(edges))
	for _, edge := range edges {
		ids = append(ids, farEnd(edge))
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)

	vertices := make(map[graph.ID]graph.Vertex, len(ids))
	for batch := range slices.Chunk(ids, DefaultPageSize) {
		found, err := s.store.GetVertices(ctx, batch)
		if err != nil {
			return nil, err
		}
		for id, vertex := range found {
			vertices[id] = vertex
		}
	}

	neighbors := make([]Neighbor, 0, len(edges))
	for _, edge := range edges {
		vertex, stored := vertices[farEnd(edge)]
		if !stored {
			continue
		}
		bare, embedding := withoutEmbedding(vertex)
		distance := Farthest
		if len(q.Near) > 0 && len(embedding) > 0 {
			if len(embedding) != len(q.Near) {
				return nil, fmt.Errorf("storage: neighbor query: Near has %d dimensions but %s has %d",
					len(q.Near), vertex.VertexID(), len(embedding))
			}
			if measured, ok := CosineDistance(q.Near, embedding); ok {
				distance = measured
			}
		}
		neighbors = append(neighbors, Neighbor{Edge: edge, Vertex: bare, Distance: distance})
	}
	return neighbors, nil
}

// withoutEmbedding returns a copy of the vertex that carries no embedding,
// and the embedding it had.
func withoutEmbedding(vertex graph.Vertex) (graph.Vertex, []float32) {
	switch v := vertex.(type) {
	case *graph.Document:
		bare := *v
		bare.Embedding = nil
		return &bare, v.Embedding
	case *graph.Topic:
		bare := *v
		bare.Embedding = nil
		return &bare, v.Embedding
	}
	return vertex, nil
}

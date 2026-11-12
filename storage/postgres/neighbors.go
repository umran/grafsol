package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

var _ storage.NeighborSearcher = (*Store)(nil)

// Neighbors implements storage.NeighborSearcher with one statement per page:
// the edges, the vertices at their far ends and the distances all come back
// together, and the embeddings never leave the database.
//
// Postgres measures every neighbour of the vertex to find the nearest ones,
// on each page. For a vertex with a few hundred edges that is nothing. For a
// hub it is the cost of reading the hub's embeddings once per page asked
// for; BenchmarkNeighborsOfAHub puts a number on it.
func (s *Store) Neighbors(ctx context.Context, q storage.NeighborQuery) (storage.NeighborPage, error) {
	if err := q.Validate(); err != nil {
		return storage.NeighborPage{}, err
	}
	after := storage.BeforeAllNeighbors
	if err := storage.DecodeCursor(q.Cursor, &after); err != nil {
		return storage.NeighborPage{}, err
	}

	var near []float32 // stays nil, and so is sent as null, when there is nothing to rank by
	if len(q.Near) > 0 {
		near = q.Near
	}

	// As in Edges, one row more than the page holds tells us whether there
	// is a next page.
	size := q.PageSize()
	rows, err := s.pool.Query(ctx, rankedNeighbors,
		string(q.Vertex), near,
		after.Distance, string(after.Kind), string(after.Neighbor), int32(after.Direction),
		size+1)
	if err != nil {
		return storage.NeighborPage{}, fmt.Errorf("postgres: listing the neighbours of %s: %w", q.Vertex, err)
	}

	// The position is kept beside each neighbour because it holds the
	// distance exactly as Postgres computed it, which is what the next page
	// must be compared with. Neighbor.Distance is that number rounded.
	type ranked struct {
		neighbor storage.Neighbor
		position storage.NeighborPosition
	}
	found, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ranked, error) {
		var (
			direction                           int32
			kind, neighbor, summary, vertexKind string
			attrs                               []byte
			distance                            float64
		)
		if err := row.Scan(&direction, &kind, &neighbor, &summary, &vertexKind, &attrs, &distance); err != nil {
			return ranked{}, err
		}
		vertex, err := decodeVertex(vertexRow{id: neighbor, kind: vertexKind, attrs: attrs})
		if err != nil {
			return ranked{}, err
		}

		// The statement numbers the directions as graph does: 1 is Out, an
		// edge that leaves the vertex asked about, and 2 is In.
		edge := graph.Edge{From: q.Vertex, Kind: graph.EdgeKind(kind), To: graph.ID(neighbor), Summary: summary}
		if graph.Direction(direction) == graph.In {
			edge.From, edge.To = edge.To, edge.From
		}
		return ranked{
			neighbor: storage.Neighbor{Edge: edge, Vertex: vertex, Distance: float32(distance)},
			position: storage.NeighborPosition{
				Distance:  distance,
				Kind:      edge.Kind,
				Neighbor:  graph.ID(neighbor),
				Direction: graph.Direction(direction),
			},
		}, nil
	})
	if err != nil {
		return storage.NeighborPage{}, fmt.Errorf("postgres: listing the neighbours of %s: %w", q.Vertex, err)
	}

	var page storage.NeighborPage
	if len(found) > size {
		found = found[:size]
		page.Next = storage.EncodeCursor(found[size-1].position)
	}
	page.Neighbors = make([]storage.Neighbor, len(found))
	for i, each := range found {
		page.Neighbors[i] = each.neighbor
	}
	return page, nil
}

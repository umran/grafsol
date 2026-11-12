package storagetest

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// RunNeighborSearcher checks a NeighborSearcher against its contract. open
// must return a fresh, empty store together with the searcher that reads
// from it; for most backends the two are the same value. dimensions is the
// length of the embeddings the store accepts, and must be at least 3.
func RunNeighborSearcher(t *testing.T, dimensions int, open func(t *testing.T) (storage.Store, storage.NeighborSearcher)) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int)
	}{
		{"NearestFirstFromBothEnds", testNeighborsNearestFirst},
		{"Paged", testNeighborsArePaged},
		{"TiesInOrder", testNeighborTies},
		{"UnmeasuredLast", testUnmeasuredNeighborsLast},
		{"WithoutAVector", testNeighborsWithoutAVector},
		{"OnlyWhatLeadsSomewhere", testNeighborsLeadSomewhere},
		{"RejectsBadArguments", testNeighborsRejectBadArguments},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, searcher := open(t)
			test.run(t, store, searcher, dimensions)
		})
	}
}

const hub = graph.ID("doc:s:hub")

func testNeighborsNearestFirst(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int) {
	// Four neighbours at known angles from the first axis, two reached by
	// edges that leave the hub and two by edges that arrive at it. One of
	// them is a topic: neighbours are vertices of any kind.
	same := sampleDocument("doc:s:same", dimensions)
	same.Embedding = vector(dimensions, 2, 0, 0)
	diagonal := sampleDocument("doc:s:diagonal", dimensions)
	diagonal.Title, diagonal.Embedding = "Diagonal", vector(dimensions, 1, 1, 0)
	orthogonal := &graph.Topic{ID: "topic:orthogonal", Name: "Orthogonal", Summary: "At right angles.", Embedding: vector(dimensions, 0, 3, 0)}
	opposite := sampleDocument("doc:s:opposite", dimensions)
	opposite.Embedding = vector(dimensions, -1, 0, 0)
	putVertices(t, store, &graph.Document{ID: hub, Title: "Hub"}, same, diagonal, orthogonal, opposite)

	toSame := graph.Edge{From: hub, Kind: graph.LinksTo, To: same.ID, Summary: "see the same"}
	fromDiagonal := graph.Edge{From: diagonal.ID, Kind: graph.LinksTo, To: hub, Summary: "the diagonal mentions the hub"}
	toOrthogonal := graph.Edge{From: hub, Kind: graph.TaggedWith, To: orthogonal.ID}
	fromOpposite := graph.Edge{From: opposite.ID, Kind: graph.ChildOf, To: hub, Summary: "sub-issue of"}
	putEdges(t, store, fromOpposite, toOrthogonal, fromDiagonal, toSame)

	got := listNeighbors(t, searcher, storage.NeighborQuery{Vertex: hub, Near: vector(dimensions, 1, 0, 0)})
	want := []struct {
		edge      graph.Edge
		vertex    graph.Vertex
		distance  float64
		direction graph.Direction
	}{
		{toSame, same, 0, graph.Out},
		{fromDiagonal, diagonal, 1 - 1/math.Sqrt2, graph.In},
		{toOrthogonal, orthogonal, 1, graph.Out},
		{fromOpposite, opposite, 2, graph.In},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d neighbours, want %d: %+v", len(got), len(want), got)
	}
	for i, neighbor := range got {
		if neighbor.Edge != want[i].edge {
			t.Errorf("neighbour %d came with edge %+v, want %+v", i, neighbor.Edge, want[i].edge)
		}
		// The vertex comes whole, except for its embedding.
		bare := graph.CloneVertex(want[i].vertex)
		switch v := bare.(type) {
		case *graph.Document:
			v.Embedding = nil
		case *graph.Topic:
			v.Embedding = nil
		}
		if !sameVertex(neighbor.Vertex, bare) {
			t.Errorf("neighbour %d is %+v, want %+v", i, neighbor.Vertex, bare)
		}
		if math.Abs(float64(neighbor.Distance)-want[i].distance) > 1e-4 {
			t.Errorf("neighbour %d (%s) is at distance %v, want %v", i, neighbor.Vertex.VertexID(), neighbor.Distance, want[i].distance)
		}
		if neighbor.Direction() != want[i].direction {
			t.Errorf("neighbour %d (%s) is reported as %s, want %s", i, neighbor.Vertex.VertexID(), neighbor.Direction(), want[i].direction)
		}
	}

	// A vertex with no edges has no neighbours, and that is not an error.
	if alone := listNeighbors(t, searcher, storage.NeighborQuery{Vertex: "doc:s:alone", Near: vector(dimensions, 1, 0, 0)}); len(alone) != 0 {
		t.Errorf("a vertex without edges has %d neighbours", len(alone))
	}
}

func testNeighborsArePaged(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int) {
	const limit = 7
	const count = 3*limit + 2

	// Neighbour i sits at an angle that grows with i, so the order by
	// distance is the order of i. The IDs run the other way, so that a
	// listing ordered by ID instead would be caught.
	want := make([]graph.ID, count)
	for i := range count {
		id := graph.DocumentID("s", fmt.Sprintf("spoke-%03d", count-i))
		angle := float64(i) * math.Pi / (2 * count)
		putVertices(t, store, &graph.Document{
			ID:        id,
			Embedding: vector(dimensions, float32(math.Cos(angle)), float32(math.Sin(angle)), 0),
		})
		edge := graph.Edge{From: hub, Kind: graph.LinksTo, To: id}
		if i%2 == 1 {
			edge = graph.Edge{From: id, Kind: graph.ChildOf, To: hub}
		}
		putEdges(t, store, edge)
		want[i] = id
	}

	query := storage.NeighborQuery{Vertex: hub, Near: vector(dimensions, 1, 0, 0), Limit: limit}
	var got []graph.ID
	for pages := 1; ; pages++ {
		page, err := searcher.Neighbors(t.Context(), query)
		if err != nil {
			t.Fatalf("Neighbors, page %d: %v", pages, err)
		}
		if len(page.Neighbors) > limit {
			t.Fatalf("page %d holds %d neighbours, more than the limit of %d", pages, len(page.Neighbors), limit)
		}
		for _, neighbor := range page.Neighbors {
			got = append(got, neighbor.Vertex.VertexID())
		}
		if page.Next == "" {
			break
		}
		if len(page.Neighbors) == 0 || pages > count {
			t.Fatalf("Neighbors is not making progress (page %d)", pages)
		}
		query.Cursor = page.Next
	}
	if !slices.Equal(got, want) {
		t.Fatalf("the pages, put together, are\n  %v\nwant every neighbour once, nearest first:\n  %v", got, want)
	}

	query.Cursor = "!!! not a cursor !!!"
	if _, err := searcher.Neighbors(t.Context(), query); err == nil {
		t.Errorf("Neighbors accepted a malformed cursor")
	}
}

// testNeighborTies checks the order among neighbours that are equally far:
// by edge kind, then by the far vertex, then by direction. The same vertex
// reached from both ends by edges of one kind is the case only the direction
// can settle.
func testNeighborTies(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int) {
	a, b, c := graph.ID("doc:s:a"), graph.ID("doc:s:b"), graph.ID("doc:s:c")
	putVertices(t, store, &graph.Document{ID: a}, &graph.Document{ID: b}, &graph.Document{ID: c})

	want := []graph.Edge{
		{From: hub, Kind: graph.ChildOf, To: b},
		{From: hub, Kind: graph.LinksTo, To: a},
		{From: a, Kind: graph.LinksTo, To: hub},
		{From: c, Kind: graph.LinksTo, To: hub},
	}
	putEdges(t, store, want[3], want[2], want[1], want[0])

	// One neighbour per page, so that every boundary between two of them is
	// crossed by a cursor.
	got := listNeighbors(t, searcher, storage.NeighborQuery{Vertex: hub, Near: vector(dimensions, 1, 0, 0), Limit: 1})
	edges := make([]graph.Edge, len(got))
	for i, neighbor := range got {
		edges[i] = neighbor.Edge
	}
	requireEdges(t, edges, want...)
}

func testUnmeasuredNeighborsLast(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int) {
	// Two neighbours can be measured. Two cannot: one was never embedded,
	// and one has an embedding of zeroes, which points nowhere.
	putVertices(t, store,
		&graph.Document{ID: "doc:s:near", Embedding: vector(dimensions, 1, 0, 0)},
		&graph.Document{ID: "doc:s:away", Embedding: vector(dimensions, 0, 1, 0)},
		&graph.Document{ID: "doc:s:bare"},
		&graph.Document{ID: "doc:s:zero", Embedding: vector(dimensions)},
	)
	for _, id := range []graph.ID{"doc:s:zero", "doc:s:bare", "doc:s:away", "doc:s:near"} {
		putEdges(t, store, graph.Edge{From: hub, Kind: graph.LinksTo, To: id})
	}

	got := listNeighbors(t, searcher, storage.NeighborQuery{Vertex: hub, Near: vector(dimensions, 1, 0, 0)})
	requireNeighbors(t, got, "doc:s:near", "doc:s:away", "doc:s:bare", "doc:s:zero")
	for _, neighbor := range got[2:] {
		if neighbor.Distance != storage.Farthest {
			t.Errorf("%s cannot be measured but is reported at distance %v, want %v", neighbor.Vertex.VertexID(), neighbor.Distance, storage.Farthest)
		}
	}
}

func testNeighborsWithoutAVector(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int) {
	// With nothing to rank by, the embeddings play no part: the order is by
	// kind and then by far vertex, as for Store.Edges.
	putVertices(t, store,
		&graph.Document{ID: "doc:s:z", Embedding: vector(dimensions, 1, 0, 0)},
		&graph.Document{ID: "doc:s:m", Embedding: vector(dimensions, 0, 1, 0)},
		&graph.Document{ID: "doc:s:a"},
	)
	putEdges(t, store,
		graph.Edge{From: hub, Kind: graph.LinksTo, To: "doc:s:z"},
		graph.Edge{From: "doc:s:m", Kind: graph.LinksTo, To: hub},
		graph.Edge{From: hub, Kind: graph.LinksTo, To: "doc:s:a"},
	)

	got := listNeighbors(t, searcher, storage.NeighborQuery{Vertex: hub, Limit: 2})
	requireNeighbors(t, got, "doc:s:a", "doc:s:m", "doc:s:z")
	for _, neighbor := range got {
		if neighbor.Distance != storage.Farthest {
			t.Errorf("%s is reported at distance %v although nothing was measured", neighbor.Vertex.VertexID(), neighbor.Distance)
		}
	}
}

func testNeighborsLeadSomewhere(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int) {
	putVertices(t, store,
		&graph.Document{ID: hub, Embedding: vector(dimensions, 1, 0, 0)},
		&graph.Document{ID: "doc:s:stored", Embedding: vector(dimensions, 1, 0, 0)},
		&graph.Document{ID: "doc:s:removed", Embedding: vector(dimensions, 1, 0, 0)},
	)
	putEdges(t, store,
		graph.Edge{From: hub, Kind: graph.LinksTo, To: "doc:s:stored"},
		graph.Edge{From: hub, Kind: graph.LinksTo, To: "doc:s:never-stored"}, // points at nothing
		graph.Edge{From: "doc:s:never-stored", Kind: graph.ChildOf, To: hub}, // comes from nothing
		graph.Edge{From: hub, Kind: graph.LinksTo, To: "doc:s:removed"},      // points at something that will go
		graph.Edge{From: hub, Kind: graph.LinksTo, To: hub},                  // leads back where it started
	)
	if err := store.DeleteVertices(t.Context(), []graph.ID{"doc:s:removed"}); err != nil {
		t.Fatalf("DeleteVertices: %v", err)
	}

	got := listNeighbors(t, searcher, storage.NeighborQuery{Vertex: hub, Near: vector(dimensions, 1, 0, 0), Limit: 1})
	requireNeighbors(t, got, "doc:s:stored")
}

func testNeighborsRejectBadArguments(t *testing.T, store storage.Store, searcher storage.NeighborSearcher, dimensions int) {
	putVertices(t, store, &graph.Document{ID: "doc:s:a", Embedding: vector(dimensions, 1, 0, 0)})
	putEdges(t, store, graph.Edge{From: hub, Kind: graph.LinksTo, To: "doc:s:a"})

	bad := map[string]storage.NeighborQuery{
		"no vertex":                  {Near: vector(dimensions, 1, 0, 0)},
		"a malformed vertex ID":      {Vertex: "nonsense", Near: vector(dimensions, 1, 0, 0)},
		"a negative limit":           {Vertex: hub, Near: vector(dimensions, 1, 0, 0), Limit: -1},
		"a vector of zeroes":         {Vertex: hub, Near: vector(dimensions)},
		"a vector of another length": {Vertex: hub, Near: vector(dimensions+1, 1)},
	}
	for name, query := range bad {
		if _, err := searcher.Neighbors(t.Context(), query); err == nil {
			t.Errorf("a query with %s was accepted", name)
		}
	}
}

// listNeighbors follows a listing to its end and returns every neighbour in
// it.
func listNeighbors(t *testing.T, searcher storage.NeighborSearcher, query storage.NeighborQuery) []storage.Neighbor {
	t.Helper()
	var all []storage.Neighbor
	for pages := 0; ; pages++ {
		if pages > 100_000 {
			t.Fatalf("Neighbors(%s) never reached a last page", query.Vertex)
		}
		page, err := searcher.Neighbors(t.Context(), query)
		if err != nil {
			t.Fatalf("Neighbors(%s): %v", query.Vertex, err)
		}
		all = append(all, page.Neighbors...)
		if page.Next == "" {
			return all
		}
		query.Cursor = page.Next
	}
}

// requireNeighbors fails the test unless the neighbours are exactly the
// given vertices, in order.
func requireNeighbors(t *testing.T, got []storage.Neighbor, want ...graph.ID) {
	t.Helper()
	ids := make([]graph.ID, len(got))
	for i, neighbor := range got {
		ids[i] = neighbor.Vertex.VertexID()
	}
	if !slices.Equal(ids, want) {
		t.Fatalf("got neighbours %v, want %v", ids, want)
	}
}

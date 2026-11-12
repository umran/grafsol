// Package memory is the in-process storage backend: a Store, DocumentSearcher
// and NeighborSearcher (store.go, search.go, neighbors.go) and a Ledger
// (ledger.go), all kept in ordinary Go maps.
//
// It is meant for tests, examples and graphs small enough to rebuild at
// start-up. It is also the reference implementation: short enough to read in
// one sitting, it shows what the contracts in package storage ask for.
package memory

import (
	"cmp"
	"context"
	"slices"
	"sort"
	"sync"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// Store is an in-memory storage.Store and storage.DocumentSearcher. The zero
// value is not usable; create one with New.
type Store struct {
	mu       sync.RWMutex
	vertices map[graph.ID]graph.Vertex

	// Every edge is filed twice: under its From vertex in out and under its
	// To vertex in in, so that it can be found from either end. Each list is
	// kept sorted by (kind, neighbor), which is the order Edges returns.
	out map[graph.ID][]arc
	in  map[graph.ID][]arc

	// space is the embedding space claimed for the graph: "" until one is,
	// and never changed after that.
	space string
}

// arc is an edge as seen from one of its ends.
type arc struct {
	kind     graph.EdgeKind
	neighbor graph.ID // the vertex at the other end
	summary  string
}

// compareArcs orders arcs by kind and then by neighbor. The summary plays no
// part: it is not what identifies an edge.
func compareArcs(a, b arc) int {
	return cmp.Or(cmp.Compare(a.kind, b.kind), cmp.Compare(a.neighbor, b.neighbor))
}

var (
	_ storage.Store            = (*Store)(nil)
	_ storage.DocumentSearcher = (*Store)(nil)
)

// New returns an empty store.
func New() *Store {
	return &Store{
		vertices: make(map[graph.ID]graph.Vertex),
		out:      make(map[graph.ID][]arc),
		in:       make(map[graph.ID][]arc),
	}
}

// PutVertices implements storage.Store.
func (s *Store) PutVertices(ctx context.Context, vertices []graph.Vertex) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Check everything first so that a bad vertex leaves the store untouched.
	for _, v := range vertices {
		if err := graph.ValidateVertex(v); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, v := range vertices {
		// Keep a private copy: the caller remains free to change its own.
		s.vertices[v.VertexID()] = graph.CloneVertex(v)
	}
	return nil
}

// GetVertices implements storage.Store.
func (s *Store) GetVertices(ctx context.Context, ids []graph.ID) (map[graph.ID]graph.Vertex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	found := make(map[graph.ID]graph.Vertex, len(ids))
	for _, id := range ids {
		if v, ok := s.vertices[id]; ok {
			found[id] = graph.CloneVertex(v)
		}
	}
	return found, nil
}

// DeleteVertices implements storage.Store.
func (s *Store) DeleteVertices(ctx context.Context, ids []graph.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.vertices, id)
	}
	return nil
}

// PutEdges implements storage.Store.
func (s *Store) PutEdges(ctx context.Context, edges []graph.Edge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, e := range edges {
		if err := e.Validate(); err != nil {
			return err
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range edges {
		s.out[e.From] = file(s.out[e.From], arc{kind: e.Kind, neighbor: e.To, summary: e.Summary})
		s.in[e.To] = file(s.in[e.To], arc{kind: e.Kind, neighbor: e.From, summary: e.Summary})
	}
	return nil
}

// DeleteEdges implements storage.Store.
func (s *Store) DeleteEdges(ctx context.Context, edges []graph.Edge) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range edges {
		unfile(s.out, e.From, arc{kind: e.Kind, neighbor: e.To})
		unfile(s.in, e.To, arc{kind: e.Kind, neighbor: e.From})
	}
	return nil
}

// file puts an arc into a sorted list, replacing the arc for the same kind
// and neighbor if there is one, and returns the list.
func file(list []arc, a arc) []arc {
	i, found := slices.BinarySearchFunc(list, a, compareArcs)
	if found {
		list[i] = a
		return list
	}
	return slices.Insert(list, i, a)
}

// unfile takes an arc out of the sorted list kept for a vertex, if it is
// there, and drops the list once it is empty.
func unfile(lists map[graph.ID][]arc, vertex graph.ID, a arc) {
	list := lists[vertex]
	i, found := slices.BinarySearchFunc(list, a, compareArcs)
	if !found {
		return
	}
	list = slices.Delete(list, i, i+1)
	if len(list) == 0 {
		delete(lists, vertex)
		return
	}
	lists[vertex] = list
}

// Edges implements storage.Store.
func (s *Store) Edges(ctx context.Context, q storage.EdgeQuery) (storage.EdgePage, error) {
	if err := ctx.Err(); err != nil {
		return storage.EdgePage{}, err
	}
	if err := q.Validate(); err != nil {
		return storage.EdgePage{}, err
	}
	var after storage.EdgePosition
	if err := storage.DecodeCursor(q.Cursor, &after); err != nil {
		return storage.EdgePage{}, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	arcs := s.out[q.Vertex]
	if q.Direction == graph.In {
		arcs = s.in[q.Vertex]
	}

	// The list is sorted by kind first, so the arcs of one kind sit together
	// in a single run.
	if q.Kind != "" {
		first := sort.Search(len(arcs), func(i int) bool { return arcs[i].kind >= q.Kind })
		end := sort.Search(len(arcs), func(i int) bool { return arcs[i].kind > q.Kind })
		arcs = arcs[first:end]
	}

	// Skip everything up to and including where the previous page ended.
	if q.Cursor != "" {
		last := arc{kind: after.Kind, neighbor: after.Neighbor}
		next := sort.Search(len(arcs), func(i int) bool { return compareArcs(arcs[i], last) > 0 })
		arcs = arcs[next:]
	}

	size := min(q.PageSize(), len(arcs))
	page := storage.EdgePage{Edges: make([]graph.Edge, 0, size)}
	for _, a := range arcs[:size] {
		page.Edges = append(page.Edges, edgeOf(q.Vertex, q.Direction, a))
	}
	if len(arcs) > size {
		last := arcs[size-1]
		page.Next = storage.EncodeCursor(storage.EdgePosition{Kind: last.kind, Neighbor: last.neighbor})
	}
	return page, nil
}

// edgeOf rebuilds the edge that an arc stands for, given the vertex it was
// filed under and the direction it was filed in.
func edgeOf(vertex graph.ID, direction graph.Direction, a arc) graph.Edge {
	if direction == graph.In {
		return graph.Edge{From: a.neighbor, Kind: a.kind, To: vertex, Summary: a.summary}
	}
	return graph.Edge{From: vertex, Kind: a.kind, To: a.neighbor, Summary: a.summary}
}

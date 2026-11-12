// Package traverse walks the graph.
//
// It is built in two layers. An Expander answers the one question a
// traversal asks of the graph, "which edges touch these vertices?", and Walk
// is a breadth-first search written against that question alone. Keeping the
// two apart means the search is written once, while each storage backend can
// answer the question in whatever way suits it best.
//
// FanOut is the Expander that works with every backend: it issues ordinary
// Store.Edges calls, several at a time. A backend that can do better, for
// example by fetching the edges of many vertices in one query, provides an
// Expander of its own; the Postgres backend does.
package traverse

import (
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// Expander lists the edges that touch a set of vertices.
type Expander interface {
	// Expand yields every edge that touches any vertex in frontier in the
	// given direction: edges leaving the frontier for graph.Out, edges
	// arriving at it for graph.In. An empty kind matches every kind.
	//
	// The listing is exhaustive: every matching edge is yielded once,
	// however many there are, in no particular order. The frontier is
	// treated as a set, so listing a vertex twice changes nothing. The
	// sequence ends after the first error it yields.
	Expand(ctx context.Context, frontier []graph.ID, direction graph.Direction, kind graph.EdgeKind) iter.Seq2[graph.Edge, error]
}

// EdgeLister is the one method of storage.Store that FanOut needs. Every
// Store is an EdgeLister.
type EdgeLister interface {
	Edges(ctx context.Context, q storage.EdgeQuery) (storage.EdgePage, error)
}

// FanOut is the Expander that works with any storage.Store. For each vertex
// in the frontier it pages through Store.Edges until the listing is
// exhausted, working on several vertices at a time and handing pages to the
// caller as they arrive.
type FanOut struct {
	Store EdgeLister

	// Workers is how many vertices are listed at the same time. Zero means
	// DefaultWorkers.
	Workers int

	// PageSize is how many edges each Store.Edges call asks for. Zero leaves
	// the choice to the store.
	PageSize int
}

// DefaultWorkers is how many vertices a FanOut lists at once when its
// Workers field is zero.
const DefaultWorkers = 16

var _ Expander = FanOut{}

// Expand implements Expander.
func (f FanOut) Expand(ctx context.Context, frontier []graph.ID, direction graph.Direction, kind graph.EdgeKind) iter.Seq2[graph.Edge, error] {
	return func(yield func(graph.Edge, error) bool) {
		// Cancelling this context is how the workers are told to stop,
		// whether because one of them failed or because the caller has
		// seen enough. The caller's own context is kept to tell those
		// cases apart from the caller giving up.
		callerCtx := ctx
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// The frontier is a set: a vertex listed twice must not have its
		// edges yielded twice.
		frontier := slices.Compact(slices.Sorted(slices.Values(frontier)))

		var (
			vertices = make(chan graph.ID)     // the frontier, handed out one vertex at a time
			pages    = make(chan []graph.Edge) // pages of edges, from the workers to the caller
			failed   = make(chan error, 1)     // the first error any worker met
			running  sync.WaitGroup            // the feeder and the workers
		)

		// The feeder hands the frontier to the workers.
		running.Go(func() {
			defer close(vertices)
			for _, vertex := range frontier {
				select {
				case vertices <- vertex:
				case <-ctx.Done():
					return
				}
			}
		})

		// Each worker takes a vertex and lists all of its edges, page by
		// page, before taking the next.
		for range min(f.workers(), len(frontier)) {
			running.Go(func() {
				for vertex := range vertices {
					if err := f.listAll(ctx, vertex, direction, kind, pages); err != nil {
						select {
						case failed <- err:
						default: // another worker already reported a failure
						}
						cancel()
						return
					}
				}
			})
		}

		// Closing pages once everyone is done is what ends the loop below.
		go func() {
			running.Wait()
			close(pages)
		}()

		// Hand the edges to the caller. Whatever happens, the loop keeps
		// reading until pages is closed, so that no worker is left blocked
		// on a send: when Expand returns, every goroutine it started has
		// finished.
		stopped := false
		for page := range pages {
			if stopped {
				continue
			}
			for _, edge := range page {
				if !yield(edge, nil) {
					stopped = true
					cancel()
					break
				}
			}
		}
		if stopped {
			return
		}
		select {
		case err := <-failed:
			yield(graph.Edge{}, err)
		default:
			// If the caller's context ended, the feeder may have stopped
			// before handing out the whole frontier without any worker
			// noticing. Report it: a listing that was cut short must never
			// pass for a complete one.
			if err := callerCtx.Err(); err != nil {
				yield(graph.Edge{}, err)
			}
		}
	}
}

// listAll pages through every edge of one vertex and sends each page on.
func (f FanOut) listAll(ctx context.Context, vertex graph.ID, direction graph.Direction, kind graph.EdgeKind, pages chan<- []graph.Edge) error {
	query := storage.EdgeQuery{Vertex: vertex, Direction: direction, Kind: kind, Limit: f.PageSize}
	for {
		page, err := f.Store.Edges(ctx, query)
		if err != nil {
			return fmt.Errorf("traverse: listing the %s edges of %s: %w", direction, vertex, err)
		}
		if len(page.Edges) > 0 {
			select {
			case pages <- page.Edges:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if page.Next == "" {
			return nil
		}
		query.Cursor = page.Next
	}
}

func (f FanOut) workers() int {
	if f.Workers > 0 {
		return f.Workers
	}
	return DefaultWorkers
}

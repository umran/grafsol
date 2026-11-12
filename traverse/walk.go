package traverse

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/umran/grafsol/graph"
)

// Step says which edges a walk follows: those of one kind, in one direction.
type Step struct {
	Direction graph.Direction
	Kind      graph.EdgeKind // empty follows edges of every kind
}

// Out returns the step that follows edges of the given kind forwards, from
// an edge's From vertex to its To vertex. An empty kind follows every kind.
func Out(kind graph.EdgeKind) Step {
	return Step{Direction: graph.Out, Kind: kind}
}

// In returns the step that follows edges of the given kind backwards, from
// an edge's To vertex to its From vertex. An empty kind follows every kind.
func In(kind graph.EdgeKind) Step {
	return Step{Direction: graph.In, Kind: kind}
}

// Options shape a walk.
type Options struct {
	// Follow lists the edges to follow from every vertex the walk reaches.
	// A walk needs at least one step.
	Follow []Step

	// MaxDepth is how many hops from the seeds the walk may go. Zero means
	// no limit: the walk reaches everything connected to the seeds by the
	// followed edges.
	MaxDepth int
}

func (o Options) validate() error {
	if len(o.Follow) == 0 {
		return errors.New("traverse: Options.Follow is empty: there are no edges to follow")
	}
	for _, step := range o.Follow {
		if err := step.Direction.Validate(); err != nil {
			return fmt.Errorf("traverse: %w", err)
		}
		if step.Kind != "" {
			if err := step.Kind.Validate(); err != nil {
				return fmt.Errorf("traverse: %w", err)
			}
		}
	}
	if o.MaxDepth < 0 {
		return errors.New("traverse: Options.MaxDepth is negative")
	}
	return nil
}

// Visit reports one vertex reached by a walk.
type Visit struct {
	ID graph.ID

	// Depth is the number of hops from the nearest seed. Seeds are at 0.
	Depth int

	// Parent is the vertex this one was reached from, and Via is the stored
	// edge that joins the two. Both are zero for a seed. Following Parent
	// from visit to visit leads back to a seed; PathTo does exactly that.
	Parent graph.ID
	Via    graph.Edge
}

// Walk performs a breadth-first search from the seeds and yields every
// vertex it reaches exactly once, at the smallest depth at which the vertex
// can be reached.
//
// The search is exhaustive within MaxDepth: nothing is capped or sampled.
// Visits arrive as they are found, so a caller that has seen enough can
// simply break out of the loop. The sequence ends after the first error it
// yields.
//
// The order is fully determined by the graph: seeds first, in the order
// given, then one depth at a time with the vertices of a depth sorted by
// ID. When a vertex can be reached at the same depth by several edges, Via
// is the smallest of them in graph.Edge.Compare order. The output is
// therefore the same whichever Expander is used.
//
// Walk holds in memory the set of vertices visited so far, and nothing else
// that grows with the graph.
func Walk(ctx context.Context, x Expander, seeds []graph.ID, opts Options) iter.Seq2[Visit, error] {
	return func(yield func(Visit, error) bool) {
		if err := opts.validate(); err != nil {
			yield(Visit{}, err)
			return
		}
		for _, seed := range seeds {
			if err := seed.Validate(); err != nil {
				yield(Visit{}, fmt.Errorf("traverse: seed: %w", err))
				return
			}
		}

		visited := make(map[graph.ID]struct{}, len(seeds)) // every vertex yielded so far
		var frontier []graph.ID                            // the vertices found at the previous depth

		for _, seed := range seeds {
			if _, repeated := visited[seed]; repeated {
				continue
			}
			visited[seed] = struct{}{}
			frontier = append(frontier, seed)
			if !yield(Visit{ID: seed}, nil) {
				return
			}
		}

		for depth := 1; len(frontier) > 0 && (opts.MaxDepth == 0 || depth <= opts.MaxDepth); depth++ {
			// Checked here as well as inside the Expander, so that a walk
			// that was cut short always ends with an error rather than
			// looking complete.
			if err := ctx.Err(); err != nil {
				yield(Visit{}, err)
				return
			}
			found, err := oneHopFurther(ctx, x, frontier, opts.Follow, visited, depth)
			if err != nil {
				yield(Visit{}, err)
				return
			}
			frontier = frontier[:0]
			for _, visit := range found {
				visited[visit.ID] = struct{}{}
				frontier = append(frontier, visit.ID)
				if !yield(visit, nil) {
					return
				}
			}
		}
	}
}

// oneHopFurther expands the frontier once and returns the vertices it leads
// to that have not been visited before, sorted by ID.
//
// Edges arrive from the Expander in no particular order, and a vertex is
// often reachable by several of them. Keeping the smallest edge per vertex
// makes the result independent of the order of arrival.
func oneHopFurther(ctx context.Context, x Expander, frontier []graph.ID, follow []Step, visited map[graph.ID]struct{}, depth int) ([]Visit, error) {
	found := make(map[graph.ID]Visit)
	for _, step := range follow {
		for edge, err := range x.Expand(ctx, frontier, step.Direction, step.Kind) {
			if err != nil {
				return nil, err
			}
			far := edge.Far(step.Direction)
			if _, seen := visited[far]; seen {
				continue
			}
			if earlier, already := found[far]; already && earlier.Via.Compare(edge) <= 0 {
				continue
			}
			found[far] = Visit{ID: far, Depth: depth, Parent: edge.Near(step.Direction), Via: edge}
		}
	}
	return slices.SortedFunc(maps.Values(found), func(a, b Visit) int {
		return cmp.Compare(a.ID, b.ID)
	}), nil
}

// Collect gathers a walk into a slice. If the walk fails, Collect returns
// the visits made before the failure together with the error.
func Collect(walk iter.Seq2[Visit, error]) ([]Visit, error) {
	var visits []Visit
	for visit, err := range walk {
		if err != nil {
			return visits, err
		}
		visits = append(visits, visit)
	}
	return visits, nil
}

// PathTo returns the edges that lead from a seed to target, in the order
// they were walked, according to the visits of one walk. It returns nil when
// target is a seed or was not reached.
func PathTo(visits []Visit, target graph.ID) []graph.Edge {
	byID := make(map[graph.ID]Visit, len(visits))
	for _, visit := range visits {
		byID[visit.ID] = visit
	}

	var path []graph.Edge
	for at, reached := byID[target]; reached && at.Depth > 0; at, reached = byID[at.Parent] {
		path = append(path, at.Via)
	}
	slices.Reverse(path)
	return path
}

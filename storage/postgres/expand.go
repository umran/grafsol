package postgres

import (
	"context"
	"fmt"
	"iter"
	"slices"

	"github.com/umran/grafsol/graph"
)

// expandBatch is how many frontier vertices go into one query.
const expandBatch = 1000

// Expand yields every edge that touches any vertex in frontier, in the given
// direction and, unless kind is empty, of the given kind. It has the method
// set of traverse.Expander, so a *Store can be handed straight to
// traverse.Walk.
//
// It gives the same answer as traverse.FanOut over this store, by a shorter
// route. FanOut asks about one vertex at a time, which costs at least one
// round trip per vertex. Expand asks about a thousand vertices at a time and
// lets Postgres stream back all of their edges, so the round trips depend on
// the size of the frontier divided by a thousand, not on the frontier itself.
// Nothing is capped: every matching row is yielded.
func (s *Store) Expand(ctx context.Context, frontier []graph.ID, direction graph.Direction, kind graph.EdgeKind) iter.Seq2[graph.Edge, error] {
	return func(yield func(graph.Edge, error) bool) {
		sql, err := expandStatement(direction, kind)
		if err != nil {
			yield(graph.Edge{}, err)
			return
		}

		// Cancelling this context is how a query still in flight is
		// abandoned when the caller stops early.
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()

		// The frontier is a set: a vertex listed twice must not have its
		// edges yielded twice, which could happen if its two mentions fell
		// into different batches.
		frontier := slices.Compact(slices.Sorted(slices.Values(frontier)))

		for batch := range slices.Chunk(frontier, expandBatch) {
			args := []any{toStrings(batch)}
			if kind != "" {
				args = append(args, string(kind))
			}
			rows, err := s.pool.Query(ctx, sql, args...)
			if err != nil {
				yield(graph.Edge{}, fmt.Errorf("postgres: expanding %d vertices: %w", len(batch), err))
				return
			}
			for rows.Next() {
				edge, err := scanEdge(rows)
				if err != nil {
					rows.Close()
					yield(graph.Edge{}, fmt.Errorf("postgres: expanding %d vertices: %w", len(batch), err))
					return
				}
				if !yield(edge, nil) {
					// The caller has seen enough. Cancel first, so that
					// closing does not wait to read the rows still to come.
					cancel()
					rows.Close()
					return
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				yield(graph.Edge{}, fmt.Errorf("postgres: expanding %d vertices: %w", len(batch), err))
				return
			}
		}
	}
}

// expandStatement picks the statement for one combination of direction and
// "one kind or every kind".
func expandStatement(direction graph.Direction, kind graph.EdgeKind) (string, error) {
	if err := direction.Validate(); err != nil {
		return "", fmt.Errorf("postgres: %w", err)
	}
	if kind != "" {
		if err := kind.Validate(); err != nil {
			return "", fmt.Errorf("postgres: %w", err)
		}
	}
	switch {
	case direction == graph.Out && kind == "":
		return expandOut, nil
	case direction == graph.Out:
		return expandOutOfKind, nil
	case kind == "":
		return expandIn, nil
	default:
		return expandInOfKind, nil
	}
}

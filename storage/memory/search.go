package memory

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// NearestDocuments implements storage.DocumentSearcher by comparing the
// query with every stored document. That is exact and perfectly adequate for
// the small graphs this backend is meant for; it is not an index.
func (s *Store) NearestDocuments(ctx context.Context, query []float32, k int) ([]storage.Match, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if k <= 0 {
		return nil, errors.New("memory: NearestDocuments needs k > 0")
	}
	if _, measurable := storage.CosineDistance(query, query); !measurable {
		return nil, errors.New("memory: NearestDocuments needs a non-zero query vector")
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var matches []storage.Match
	for id, v := range s.vertices {
		document, ok := v.(*graph.Document)
		if !ok || len(document.Embedding) == 0 {
			continue
		}
		if len(document.Embedding) != len(query) {
			return nil, fmt.Errorf("memory: query has %d dimensions but %s has %d", len(query), id, len(document.Embedding))
		}
		distance, measured := storage.CosineDistance(query, document.Embedding)
		if !measured {
			continue // a zero vector points nowhere, so it is near nothing
		}
		matches = append(matches, storage.Match{ID: id, Distance: distance})
	}

	// Closest first; ties broken by ID so that results are repeatable.
	slices.SortFunc(matches, func(a, b storage.Match) int {
		return cmp.Or(cmp.Compare(a.Distance, b.Distance), cmp.Compare(a.ID, b.ID))
	})
	if len(matches) > k {
		matches = matches[:k]
	}
	return matches, nil
}

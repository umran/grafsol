package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// pgvector's HNSW index looks at "ef_search" candidates per search: 40
// unless told otherwise, and never more than 1,000.
const (
	defaultEfSearch = 40
	maxEfSearch     = 1000
)

// NearestDocuments implements storage.DocumentSearcher using the HNSW index
// on document embeddings.
//
// The index is approximate: it finds the nearest documents with high
// probability rather than certainty, in exchange for not comparing the query
// with every row. Two settings, applied to this one search only, keep that
// from costing results:
//
//   - ef_search is raised to k when k is larger than the default, so the
//     index considers at least as many candidates as were asked for;
//   - iterative_scan lets the index keep going until it has k rows, instead
//     of stopping at its candidate list.
func (s *Store) NearestDocuments(ctx context.Context, query []float32, k int) ([]storage.Match, error) {
	if k <= 0 {
		return nil, errors.New("postgres: NearestDocuments needs k > 0")
	}
	if len(query) == 0 {
		return nil, errors.New("postgres: NearestDocuments needs a query vector")
	}
	efSearch := min(max(k, defaultEfSearch), maxEfSearch)

	// The three statements travel together and run in one transaction, so
	// the two settings last exactly as long as the search.
	batch := &pgx.Batch{}
	batch.Queue(setLocal, "hnsw.ef_search", strconv.Itoa(efSearch))
	batch.Queue(setLocal, "hnsw.iterative_scan", "strict_order")
	batch.Queue(nearestDocuments, query, k)

	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()

	for range 2 { // the two settings
		if _, err := results.Exec(); err != nil {
			return nil, fmt.Errorf("postgres: preparing the search: %w", err)
		}
	}
	rows, err := results.Query()
	if err != nil {
		return nil, fmt.Errorf("postgres: searching documents: %w", err)
	}
	matches, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (storage.Match, error) {
		var id string
		var distance float64
		if err := row.Scan(&id, &distance); err != nil {
			return storage.Match{}, err
		}
		return storage.Match{ID: graph.ID(id), Distance: float32(distance)}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: searching documents: %w", err)
	}
	return matches, nil
}

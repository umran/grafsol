package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/umran/grafsol/storage"
)

// EmbeddingSpace implements storage.Store.
func (s *Store) EmbeddingSpace(ctx context.Context) (string, error) {
	var space string
	err := s.pool.QueryRow(ctx, selectSpace).Scan(&space)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("postgres: reading the graph's embedding space: %w", err)
	}
	return space, nil
}

// ClaimEmbeddingSpace implements storage.Store. The claim and the look at
// whose claim stands travel together, in one round trip.
func (s *Store) ClaimEmbeddingSpace(ctx context.Context, space string) error {
	if err := storage.CheckSpaceName(space); err != nil {
		return err
	}

	batch := &pgx.Batch{}
	batch.Queue(claimSpace, space)
	batch.Queue(selectSpace)
	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()

	if _, err := results.Exec(); err != nil {
		return fmt.Errorf("postgres: claiming the graph's embedding space: %w", err)
	}
	var recorded string
	if err := results.QueryRow().Scan(&recorded); err != nil {
		return fmt.Errorf("postgres: claiming the graph's embedding space: %w", err)
	}
	if recorded != space {
		return fmt.Errorf("%w: the graph's embeddings are in %q, and %q was claimed", storage.ErrOtherSpace, recorded, space)
	}
	return nil
}

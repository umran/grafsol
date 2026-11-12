package memory

import (
	"context"
	"fmt"

	"github.com/umran/grafsol/storage"
)

// EmbeddingSpace implements storage.Store.
func (s *Store) EmbeddingSpace(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.space, nil
}

// ClaimEmbeddingSpace implements storage.Store.
func (s *Store) ClaimEmbeddingSpace(ctx context.Context, space string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := storage.CheckSpaceName(space); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.space == "" {
		s.space = space
	}
	if s.space != space {
		return fmt.Errorf("%w: the graph's embeddings are in %q, and %q was claimed", storage.ErrOtherSpace, s.space, space)
	}
	return nil
}

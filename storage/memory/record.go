package memory

import (
	"context"
	"slices"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

var _ storage.Record = (*Store)(nil)

// PutSaid implements storage.Record.
func (s *Store) PutSaid(ctx context.Context, said storage.Said) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := said.Validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Keep a private copy: the caller remains free to change its own.
	s.said[said.Document.ID] = said.Clone()
	return nil
}

// DeleteSaid implements storage.Record.
func (s *Store) DeleteSaid(ctx context.Context, document graph.ID) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.said, document)
	return nil
}

// Said implements storage.Record.
func (s *Store) Said(ctx context.Context, documents []graph.ID) (map[graph.ID]storage.Said, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	found := make(map[graph.ID]storage.Said, len(documents))
	for _, id := range documents {
		if said, ok := s.said[id]; ok {
			found[id] = said.Clone()
		}
	}
	return found, nil
}

// recordedPosition is the content of a Recorded cursor: the last ID returned.
type recordedPosition struct {
	After graph.ID `json:"after"`
}

// Recorded implements storage.Record.
func (s *Store) Recorded(ctx context.Context, cursor string) ([]graph.ID, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	var position recordedPosition
	if err := storage.DecodeCursor(cursor, &position); err != nil {
		return nil, "", err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	var ids []graph.ID
	for id := range s.said {
		if id > position.After {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)

	size := s.PageSize
	if size <= 0 {
		size = storage.DefaultPageSize
	}
	if len(ids) <= size {
		return ids, "", nil
	}
	ids = ids[:size]
	return ids, storage.EncodeCursor(recordedPosition{After: ids[size-1]}), nil
}

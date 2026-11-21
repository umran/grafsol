package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

var _ storage.Record = (*Store)(nil)

// PutSaid implements storage.Record.
func (s *Store) PutSaid(ctx context.Context, said storage.Said) error {
	body, err := encodeSaid(said)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, upsertSaid, string(said.Document.ID), body); err != nil {
		return fmt.Errorf("postgres: storing what was said of %s: %w", said.Document.ID, err)
	}
	return nil
}

// DeleteSaid implements storage.Record.
func (s *Store) DeleteSaid(ctx context.Context, document graph.ID) error {
	if _, err := s.pool.Exec(ctx, deleteSaid, string(document)); err != nil {
		return fmt.Errorf("postgres: deleting what was said of %s: %w", document, err)
	}
	return nil
}

// Said implements storage.Record.
func (s *Store) Said(ctx context.Context, documents []graph.ID) (map[graph.ID]storage.Said, error) {
	found := make(map[graph.ID]storage.Said, len(documents))
	if len(documents) == 0 {
		return found, nil
	}

	rows, err := s.pool.Query(ctx, selectSaid, toStrings(documents))
	if err != nil {
		return nil, fmt.Errorf("postgres: fetching what sources said: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return nil, fmt.Errorf("postgres: fetching what sources said: %w", err)
		}
		said, err := decodeSaid(graph.ID(id), body)
		if err != nil {
			return nil, err
		}
		found[said.Document.ID] = said
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: fetching what sources said: %w", err)
	}
	return found, nil
}

// recordedPosition is the content of a Recorded cursor: the last ID returned.
type recordedPosition struct {
	After graph.ID `json:"after"`
}

// Recorded implements storage.Record.
func (s *Store) Recorded(ctx context.Context, cursor string) ([]graph.ID, string, error) {
	var position recordedPosition
	if err := storage.DecodeCursor(cursor, &position); err != nil {
		return nil, "", err
	}
	size := s.PageSize
	if size <= 0 {
		size = storage.DefaultPageSize
	}

	// As in Edges, one extra row tells us whether there is more.
	rows, err := s.pool.Query(ctx, recordedDocuments, string(position.After), size+1)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: listing the documents on record: %w", err)
	}
	ids, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (graph.ID, error) {
		var id string
		err := row.Scan(&id)
		return graph.ID(id), err
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: listing the documents on record: %w", err)
	}

	next := ""
	if len(ids) > size {
		ids = ids[:size]
		next = storage.EncodeCursor(recordedPosition{After: ids[size-1]})
	}
	return ids, next, nil
}

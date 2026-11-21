package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/jackc/pgx/v5"

	"github.com/umran/grafsol/storage"
)

var _ storage.Settings = (*Store)(nil)

// Settings implements storage.Settings.
func (s *Store) Settings(ctx context.Context) ([]byte, uint64, error) {
	var version int64
	var document []byte
	if err := s.pool.QueryRow(ctx, selectSettings).Scan(&version, &document); err != nil {
		return nil, 0, fmt.Errorf("postgres: reading the settings: %w", err)
	}
	return document, uint64(version), nil
}

// SaveSettings implements storage.Settings.
func (s *Store) SaveSettings(ctx context.Context, document []byte, replaces uint64) (uint64, error) {
	moved := fmt.Errorf("%w: the save was made from version %d, which is not the version stored", storage.ErrSettingsMoved, replaces)
	if replaces >= math.MaxInt64 {
		return 0, moved // the column is a bigint, and no version it holds is this large
	}
	if document == nil {
		document = []byte{} // a nil slice would be sent as null, which the column forbids
	}

	var version int64
	err := s.pool.QueryRow(ctx, saveSettings, document, int64(replaces)).Scan(&version)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, moved
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: saving the settings: %w", err)
	}
	return uint64(version), nil
}

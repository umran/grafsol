package memory

import (
	"bytes"
	"context"
	"fmt"

	"github.com/umran/grafsol/storage"
)

var _ storage.Settings = (*Store)(nil)

// Settings implements storage.Settings.
func (s *Store) Settings(ctx context.Context) ([]byte, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return bytes.Clone(s.settings), s.settingsVersion, nil
}

// SaveSettings implements storage.Settings.
func (s *Store) SaveSettings(ctx context.Context, document []byte, replaces uint64) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settingsVersion != replaces {
		return 0, fmt.Errorf("%w: the save was made from version %d, and version %d is stored", storage.ErrSettingsMoved, replaces, s.settingsVersion)
	}
	s.settings = bytes.Clone(document)
	s.settingsVersion++
	return s.settingsVersion, nil
}

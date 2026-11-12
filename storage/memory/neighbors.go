package memory

import (
	"context"

	"github.com/umran/grafsol/storage"
)

var _ storage.NeighborSearcher = (*Store)(nil)

// Neighbors implements storage.NeighborSearcher. With everything in memory
// there is no cheaper way to rank a neighbourhood than to read it, so this
// is the generic scan and nothing more.
func (s *Store) Neighbors(ctx context.Context, q storage.NeighborQuery) (storage.NeighborPage, error) {
	if err := ctx.Err(); err != nil {
		return storage.NeighborPage{}, err
	}
	return storage.NeighborsByScan(s).Neighbors(ctx, q)
}

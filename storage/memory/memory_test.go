package memory_test

import (
	"testing"

	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
	"github.com/umran/grafsol/storage/storagetest"
)

// The embeddings used by the suites are this long. The in-memory store takes
// any length; the number only has to be at least 3.
const dimensions = 4

func TestStore(t *testing.T) {
	storagetest.RunStore(t, dimensions, func(*testing.T) storage.Store {
		return memory.New()
	})
}

func TestSearcher(t *testing.T) {
	storagetest.RunSearcher(t, dimensions, func(*testing.T) (storage.Store, storage.DocumentSearcher) {
		store := memory.New()
		return store, store
	})
}

func TestNeighbors(t *testing.T) {
	storagetest.RunNeighborSearcher(t, dimensions, func(*testing.T) (storage.Store, storage.NeighborSearcher) {
		store := memory.New()
		return store, store
	})
}

func TestLedger(t *testing.T) {
	const pageSize = 3
	storagetest.RunLedger(t, pageSize, func(*testing.T) storage.Ledger {
		ledger := memory.NewLedger()
		ledger.PageSize = pageSize
		return ledger
	})
}

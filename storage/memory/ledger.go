package memory

import (
	"bytes"
	"context"
	"slices"
	"sync"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// Ledger is an in-memory storage.Ledger. Being in-process, its locks
// coordinate goroutines but not separate processes. Create one with
// NewLedger.
type Ledger struct {
	// PageSize is how many documents Unseen returns per page; zero means
	// storage.DefaultPageSize.
	PageSize int

	mu          sync.Mutex
	tick        uint64 // the clock: Tick advances it, Record reads it
	marks       map[graph.ID]storage.Mark
	checkpoints map[string][]byte

	locks keyedLocks
}

var _ storage.Ledger = (*Ledger)(nil)

// NewLedger returns an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{
		tick:        1,
		marks:       make(map[graph.ID]storage.Mark),
		checkpoints: make(map[string][]byte),
		locks:       keyedLocks{held: make(map[string]*keyLock)},
	}
}

// Lock implements storage.Ledger.
func (l *Ledger) Lock(ctx context.Context, key string) (func(), error) {
	return l.locks.lock(ctx, key)
}

// Mark implements storage.Ledger.
func (l *Ledger) Mark(ctx context.Context, id graph.ID) (storage.Mark, bool, error) {
	if err := ctx.Err(); err != nil {
		return storage.Mark{}, false, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	mark, found := l.marks[id]
	return mark, found, nil
}

// Record implements storage.Ledger.
func (l *Ledger) Record(ctx context.Context, mark storage.Mark) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := mark.ID.Validate(); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	mark.Seen = l.tick
	l.marks[mark.ID] = mark
	return nil
}

// Tick implements storage.Ledger.
func (l *Ledger) Tick(ctx context.Context) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	l.tick++
	return l.tick, nil
}

// unseenPosition is the content of an Unseen cursor: the last ID returned.
type unseenPosition struct {
	After graph.ID `json:"after"`
}

// Unseen implements storage.Ledger. Pages are ordered by document ID.
func (l *Ledger) Unseen(ctx context.Context, source string, before uint64, cursor string) ([]graph.ID, string, error) {
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	var position unseenPosition
	if err := storage.DecodeCursor(cursor, &position); err != nil {
		return nil, "", err
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	var ids []graph.ID
	for id, mark := range l.marks {
		if mark.Source == source && !mark.Deleted && mark.Seen < before && id > position.After {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)

	size := l.PageSize
	if size <= 0 {
		size = storage.DefaultPageSize
	}
	if len(ids) <= size {
		return ids, "", nil
	}
	ids = ids[:size]
	return ids, storage.EncodeCursor(unseenPosition{After: ids[size-1]}), nil
}

// Checkpoint implements storage.Ledger.
func (l *Ledger) Checkpoint(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.checkpoints[name]), nil
}

// SaveCheckpoint implements storage.Ledger.
func (l *Ledger) SaveCheckpoint(ctx context.Context, name string, checkpoint []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	// Store an empty slice rather than nil so that "saved as empty" and
	// "never saved" stay distinguishable.
	l.checkpoints[name] = append([]byte{}, checkpoint...)
	return nil
}

// keyedLocks is a set of mutexes, one per key, created on demand and
// discarded when nobody holds or awaits them.
type keyedLocks struct {
	mu   sync.Mutex
	held map[string]*keyLock
}

// keyLock is the lock for one key.
type keyLock struct {
	// token has room for exactly one value. Whoever manages to put a value
	// in holds the lock; taking the value out releases it. Using a channel
	// rather than a sync.Mutex lets a waiter give up when its context ends.
	token chan struct{}

	// users counts the goroutines holding or waiting for the lock, so the
	// entry can be removed from the map once the count returns to zero.
	users int
}

func (k *keyedLocks) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	lock := k.held[key]
	if lock == nil {
		lock = &keyLock{token: make(chan struct{}, 1)}
		k.held[key] = lock
	}
	lock.users++
	k.mu.Unlock()

	select {
	case lock.token <- struct{}{}:
	case <-ctx.Done():
		k.leave(key, lock)
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-lock.token
			k.leave(key, lock)
		})
	}, nil
}

// leave notes that one goroutine is done with the lock for key.
func (k *keyedLocks) leave(key string, lock *keyLock) {
	k.mu.Lock()
	defer k.mu.Unlock()
	lock.users--
	if lock.users == 0 {
		delete(k.held, key)
	}
}

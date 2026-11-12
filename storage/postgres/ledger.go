package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// Ledger is a storage.Ledger kept in Postgres. Every process that uses the
// same database shares it, locks included.
type Ledger struct {
	// PageSize is how many documents Unseen returns per page; zero means
	// storage.DefaultPageSize.
	PageSize int

	pool *pgxpool.Pool

	// lockSlots limits how many locks this process holds or waits for at
	// once. A lock occupies a pooled connection for as long as it is held;
	// without a limit, locks could take every connection and leave none for
	// the very work they protect. Half the pool is the limit.
	lockSlots chan struct{}
}

var _ storage.Ledger = (*Ledger)(nil)

// NewLedger returns a Ledger that uses the given pool. The schema must
// already exist; see CreateSchema.
func NewLedger(pool *pgxpool.Pool) *Ledger {
	slots := max(1, int(pool.Config().MaxConns)/2)
	return &Ledger{pool: pool, lockSlots: make(chan struct{}, slots)}
}

// Lock implements storage.Ledger with a Postgres advisory lock.
//
// The lock belongs to a transaction opened for the purpose and is released
// when that transaction ends, which Postgres guarantees even if this process
// dies while holding it.
func (l *Ledger) Lock(ctx context.Context, key string) (func(), error) {
	select {
	case l.lockSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		<-l.lockSlots
		return nil, fmt.Errorf("postgres: locking %q: %w", key, err)
	}
	if _, err := tx.Exec(ctx, lockKey, key); err != nil {
		endLockTransaction(tx)
		<-l.lockSlots
		return nil, fmt.Errorf("postgres: locking %q: %w", key, err)
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			endLockTransaction(tx)
			<-l.lockSlots
		})
	}, nil
}

// endLockTransaction ends the transaction that holds a lock, which releases
// the lock and returns the connection to the pool. It uses a context of its
// own: the lock must be released even when the caller's context has ended.
func endLockTransaction(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// A failure here means the connection is gone, and with it the lock.
	_ = tx.Rollback(ctx)
}

// Mark implements storage.Ledger.
func (l *Ledger) Mark(ctx context.Context, id graph.ID) (storage.Mark, bool, error) {
	mark := storage.Mark{ID: id}
	var version, seen int64
	err := l.pool.QueryRow(ctx, selectMark, string(id)).Scan(&mark.Source, &version, &mark.Fingerprint, &mark.Deleted, &seen)
	if errors.Is(err, pgx.ErrNoRows) {
		return storage.Mark{}, false, nil
	}
	if err != nil {
		return storage.Mark{}, false, fmt.Errorf("postgres: reading the mark of %s: %w", id, err)
	}
	mark.Version = uint64(version) // stored as the same 64 bits; see schema.sql
	mark.Seen = uint64(seen)
	return mark, true, nil
}

// Record implements storage.Ledger.
func (l *Ledger) Record(ctx context.Context, mark storage.Mark) error {
	if err := mark.ID.Validate(); err != nil {
		return err
	}
	_, err := l.pool.Exec(ctx, upsertMark, string(mark.ID), mark.Source, int64(mark.Version), mark.Fingerprint, mark.Deleted)
	if err != nil {
		return fmt.Errorf("postgres: recording the mark of %s: %w", mark.ID, err)
	}
	return nil
}

// Tick implements storage.Ledger.
func (l *Ledger) Tick(ctx context.Context) (uint64, error) {
	var tick int64
	if err := l.pool.QueryRow(ctx, nextTick).Scan(&tick); err != nil {
		return 0, fmt.Errorf("postgres: advancing the ledger's clock: %w", err)
	}
	return uint64(tick), nil
}

// unseenPosition is the content of an Unseen cursor: where the last page
// ended in (seen, id) order.
type unseenPosition struct {
	Seen int64    `json:"seen"`
	ID   graph.ID `json:"id"`
}

// Unseen implements storage.Ledger.
func (l *Ledger) Unseen(ctx context.Context, source string, before uint64, cursor string) ([]graph.ID, string, error) {
	var after unseenPosition
	if err := storage.DecodeCursor(cursor, &after); err != nil {
		return nil, "", err
	}
	size := l.PageSize
	if size <= 0 {
		size = storage.DefaultPageSize
	}
	// The clock is a bigint, so no reading lies beyond its range.
	beforeTick := int64(min(before, math.MaxInt64))

	// As in Store.Edges, one extra row tells us whether there is more.
	rows, err := l.pool.Query(ctx, unseenMarks, source, beforeTick, after.Seen, string(after.ID), size+1)
	if err != nil {
		return nil, "", fmt.Errorf("postgres: listing unseen documents of %s: %w", source, err)
	}
	positions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (unseenPosition, error) {
		var id string
		var position unseenPosition
		if err := row.Scan(&id, &position.Seen); err != nil {
			return unseenPosition{}, err
		}
		position.ID = graph.ID(id)
		return position, nil
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: listing unseen documents of %s: %w", source, err)
	}

	next := ""
	if len(positions) > size {
		positions = positions[:size]
		next = storage.EncodeCursor(positions[size-1])
	}
	ids := make([]graph.ID, len(positions))
	for i, position := range positions {
		ids[i] = position.ID
	}
	return ids, next, nil
}

// Checkpoint implements storage.Ledger.
func (l *Ledger) Checkpoint(ctx context.Context, name string) ([]byte, error) {
	var checkpoint []byte
	err := l.pool.QueryRow(ctx, selectCheckpoint, name).Scan(&checkpoint)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: reading checkpoint %q: %w", name, err)
	}
	if checkpoint == nil {
		checkpoint = []byte{} // saved as empty, which is not the same as never saved
	}
	return checkpoint, nil
}

// SaveCheckpoint implements storage.Ledger.
func (l *Ledger) SaveCheckpoint(ctx context.Context, name string, checkpoint []byte) error {
	if checkpoint == nil {
		checkpoint = []byte{} // a nil slice would be sent as null, which the column forbids
	}
	if _, err := l.pool.Exec(ctx, upsertCheckpoint, name, checkpoint); err != nil {
		return fmt.Errorf("postgres: saving checkpoint %q: %w", name, err)
	}
	return nil
}

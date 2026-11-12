package storage

import (
	"context"

	"github.com/umran/grafsol/graph"
)

// Ledger is the sync engine's memory. The graph says what is known; the
// ledger says what has been synced: which version of each document was last
// applied, and how far each feed has been read. Keeping the two apart leaves
// the graph free of bookkeeping.
//
// Implementations must be safe for concurrent use, including by several
// processes that share one ledger.
type Ledger interface {
	// Lock blocks until the caller holds the lock for key, then returns the
	// function that releases it. Only one holder per key exists at a time,
	// across every process that shares the ledger. The sync engine uses it
	// so that two workers never rewrite the same document at once. Calling
	// the returned function more than once is harmless.
	Lock(ctx context.Context, key string) (unlock func(), err error)

	// Mark returns what was last recorded for a document, and whether
	// anything was.
	Mark(ctx context.Context, id graph.ID) (Mark, bool, error)

	// Record stores a mark, replacing any earlier mark for the same
	// document. The ledger sets the mark's Seen to the current reading of
	// its clock; the Seen passed in is ignored.
	Record(ctx context.Context, mark Mark) error

	// Tick advances the ledger's clock and returns the new reading. Nothing
	// else advances it: the sync engine calls Tick once, when a scan of a
	// source begins. A mark recorded before that call has a Seen below the
	// reading it returned, and a mark recorded after it does not, which is
	// how a finished scan tells the documents it came across from those it
	// missed. The clock never reads zero.
	Tick(ctx context.Context) (uint64, error)

	// Unseen lists, a page at a time, the documents of one source that are
	// not tombstones and whose marks were last recorded before the clock
	// reached the given reading. Passing each call's next as the following
	// call's cursor, until next is empty, visits every such document.
	Unseen(ctx context.Context, source string, before uint64, cursor string) (ids []graph.ID, next string, err error)

	// Checkpoint returns the bytes last saved under name, or nil if nothing
	// was. The ledger does not interpret them.
	Checkpoint(ctx context.Context, name string) ([]byte, error)

	// SaveCheckpoint stores bytes under name, replacing what was there.
	SaveCheckpoint(ctx context.Context, name string, checkpoint []byte) error
}

// Mark is what the ledger remembers about one document.
type Mark struct {
	ID     graph.ID
	Source string // the source system the document belongs to

	// Version is the newest version of the document that was applied. A
	// change with an older version arrived late and is ignored.
	Version uint64

	// Fingerprint is a hash of the state that was applied. A change with
	// the same fingerprint would rewrite the graph with what it already
	// holds, so it is skipped.
	Fingerprint string

	// Deleted marks a tombstone: the document was removed, and the mark is
	// kept so that a late change cannot bring it back.
	Deleted bool

	// Seen is what the ledger's clock read when the mark was last recorded.
	// The clock advances each time a scan of a source begins, so a mark
	// whose Seen is below the reading a scan began with has not been
	// recorded since: that scan did not come across the document.
	Seen uint64
}

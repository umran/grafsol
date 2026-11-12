// Package source defines how documents reach the graph from the systems
// that own them: what a connector to GitHub, Jira, Notion or anything else
// has to provide.
//
// Everything a connector reports is a Change: the complete state of one
// document at one moment, or word that the document is gone. A connector
// never reports "the title changed" or "a label was added"; it reports the
// document as it now is. That one decision is what lets the graph converge
// on the truth even when changes are lost, repeated or arrive out of order:
// applying the newest change makes the document right, whatever came before.
//
// Systems differ in how they let changes be observed, so a connector
// implements whichever of three capabilities its system supports:
//
//   - Subscriber: the system keeps a change log that can be read from a
//     saved position (an event API, a queue, a replication stream).
//   - Receiver: the system pushes changes as they happen (webhooks).
//   - Scanner: the system can list everything it currently has.
//
// A change feed, by subscription or by push, is the preferred way to stay
// current: it is prompt and cheap. A scan is the safety net underneath it.
// It fills the graph the first time, and run periodically it repairs
// whatever the feed missed. Package ingest consumes all three.
package source

import (
	"context"
	"iter"

	"github.com/umran/grafsol/graph"
)

// Source is the part every connector has, whatever else it can do.
type Source interface {
	// Name names the source system, such as "github". It is the middle part
	// of the IDs of the source's documents ("doc:github:...") and must not
	// change once documents have been ingested.
	Name() string

	// Resolve maps a URL to the document it addresses, if the URL belongs
	// to this source. This is how a link in a Notion page to a GitHub pull
	// request becomes an edge between the right two vertices: the sync
	// engine asks every source whether it recognizes the URL.
	//
	// Resolve must not perform I/O; it only reads the URL. It answers for
	// any well-formed URL of the source's system, whether or not the
	// document has been ingested.
	Resolve(url string) (graph.ID, bool)
}

// Subscriber is a source that keeps a change log.
type Subscriber interface {
	Source

	// Subscribe streams the changes recorded after the given checkpoint,
	// oldest first, and then keeps waiting for new ones until ctx ends. A
	// nil checkpoint means "from the start of the log".
	//
	// Each change carries the Checkpoint to resume from once it has been
	// applied. A change may be delivered more than once (after a restart,
	// say), but none may be skipped. The sequence ends after the first
	// error it yields.
	Subscribe(ctx context.Context, after Checkpoint) iter.Seq2[Change, error]
}

// Receiver is a source that pushes its changes.
type Receiver interface {
	Source

	// Receive checks that a delivery is authentic and decodes the changes
	// in it. It returns an error wrapping ErrUnauthorized when the delivery
	// fails authentication and one wrapping ErrMalformed when it cannot be
	// understood. A delivery that is authentic but about something the
	// connector does not track yields no changes and no error.
	Receive(ctx context.Context, delivery Delivery) ([]Change, error)
}

// Scanner is a source that can list everything it has.
type Scanner interface {
	Source

	// Scan streams every document that currently exists, once each, in any
	// order. Passing the Checkpoint of the last change received resumes an
	// interrupted scan; a nil checkpoint starts a new one.
	//
	// A scan that could not cover everything must end with an error. A
	// sequence that ends without one is taken as proof that the documents
	// it did not mention no longer exist.
	Scan(ctx context.Context, resume Checkpoint) iter.Seq2[Change, error]
}

package ingest

import (
	"context"

	"github.com/umran/grafsol/source"
)

// Enricher adds to a change what the source system cannot know: a summary
// of the document, an embedding of it, the topics it is about.
//
// Enrichers are where models come in, which makes them the slow and costly
// part of ingestion. The Syncer therefore runs them only when a document's
// state has actually changed: never for a removal, and never for a change
// the ledger shows to be already applied.
type Enricher interface {
	// Enrich may fill in c.Document.Summary and c.Document.Embedding, and
	// may add to c.Topics and c.Edges. c.Content holds the document's raw
	// text. By the time Enrich runs, the links the source reported have
	// already been turned into edges.
	//
	// An error stops the change from being applied, so that it is tried
	// again later. The exception is an error that wraps model.ErrDeclined:
	// a model that refuses a document would refuse it every time, so the
	// Syncer logs the refusal and applies the change without what this
	// enricher would have added.
	Enrich(ctx context.Context, c *source.Change) error
}

// EnricherFunc lets an ordinary function serve as an Enricher.
type EnricherFunc func(ctx context.Context, c *source.Change) error

// Enrich calls f.
func (f EnricherFunc) Enrich(ctx context.Context, c *source.Change) error {
	return f(ctx, c)
}

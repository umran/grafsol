// Package ingest keeps the graph in step with the systems its documents
// come from.
//
// A Syncer takes changes from sources and makes the graph reflect them.
// Changes arrive in three ways, one for each capability a source can have:
//
//   - Follow reads a source's change log (a source.Subscriber);
//   - Deliver, or Handler for HTTP, accepts changes that a source pushes,
//     such as webhooks (a source.Receiver);
//   - Reconcile scans everything a source has (a source.Scanner).
//
// All three end in Apply, which turns one change into graph writes. apply.go
// is the file to read to understand the package.
//
// # What a Syncer guarantees
//
// Changes may be lost, repeated, reordered or handled by several workers at
// once. However that goes, once a source stops changing and two Reconcile
// passes have run to completion, the graph holds exactly that source's
// documents and their edges.
//
// Each way a change can go astray has its own remedy:
//
//   - Delivered twice: applying a change is idempotent, and the ledger
//     remembers a fingerprint of what was applied, so a repeat does nothing.
//   - Delivered out of order: every change carries a version, and a change
//     older than what the ledger has seen is ignored.
//   - An edit delivered after the removal: removals leave a tombstone in the
//     ledger. When an edit and a removal carry the same version, the removal
//     wins.
//   - A removed document restored without notice: a scan lists what exists
//     now, so a Reconcile pass that began after the removal was recorded
//     brings back any document it lists, even at a version older than the
//     removal's.
//   - Two workers on one document: the ledger's lock lets one go at a time.
//   - Lost: a Reconcile pass re-reads the source and applies what differs.
//   - A removal lost: Reconcile removes documents the source no longer
//     lists, but only after two passes in a row have missed them, because a
//     listing that changes while it is being read can skip a document.
//   - The process dies mid-change: the ledger is written last, so the change
//     simply counts as not yet applied.
//
// # One embedding space to a graph
//
// Embeddings can be compared only if one model, set up one way, made all of
// them. So a change that carries embeddings says which embedding space they
// are in (the Embed enricher writes it), and before writing such a change
// the Syncer claims the graph for that space. The first claim settles what
// the graph holds. A change from any other space fails from then on, and
// nothing of it is written: a second program syncing into the graph through
// another model stops at its first document, instead of mixing its vectors
// in among ones they cannot be compared with.
package ingest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
)

// Config says what a Syncer works with.
type Config struct {
	// Store is the graph to keep in step. Required.
	Store storage.Store

	// Ledger is where the Syncer keeps its own bookkeeping. Required.
	// Several Syncers, in one process or many, may share a Store and a
	// Ledger; the Ledger is what coordinates them.
	Ledger storage.Ledger

	// Sources are asked to recognize the URLs found in documents, so that
	// a link from one system into another becomes an edge. List every
	// source whose documents may be linked to.
	Sources []source.Source

	// Enrichers run, in order, on every change that alters a document.
	// Summarize and Embed are the two that come with the package.
	Enrichers []Enricher

	// Logger hears about two things that would otherwise pass unseen: a
	// pushed delivery that could not be applied (the sender is only told
	// that something went wrong), and a document that a model declined to
	// enrich. Nil means slog.Default().
	Logger *slog.Logger
}

// Syncer keeps a graph in step with its sources. It is safe for concurrent
// use.
type Syncer struct {
	store     storage.Store
	ledger    storage.Ledger
	sources   []source.Source
	enrichers []Enricher
	logger    *slog.Logger
}

// NewSyncer returns a Syncer. It panics if cfg has no Store or no Ledger: a
// Syncer can do nothing without them, and leaving one out is a mistake in
// the program rather than a condition to handle while it runs.
func NewSyncer(cfg Config) *Syncer {
	if cfg.Store == nil {
		panic("ingest: NewSyncer needs a Store")
	}
	if cfg.Ledger == nil {
		panic("ingest: NewSyncer needs a Ledger")
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{
		store:     cfg.Store,
		ledger:    cfg.Ledger,
		sources:   cfg.Sources,
		enrichers: cfg.Enrichers,
		logger:    logger,
	}
}

// Outcome says what became of a change.
type Outcome int

const (
	// Applied: the graph was rewritten to match the change.
	Applied Outcome = iota + 1

	// Unchanged: the graph already matched the change.
	Unchanged

	// Stale: the change was older than what the graph holds, and the graph
	// was left as it is.
	Stale

	// Removed: the document was removed from the graph.
	Removed
)

// String returns the outcome in lower case: "applied", "unchanged", ...
func (o Outcome) String() string {
	switch o {
	case Applied:
		return "applied"
	case Unchanged:
		return "unchanged"
	case Stale:
		return "stale"
	case Removed:
		return "removed"
	}
	return fmt.Sprintf("Outcome(%d)", int(o))
}

// Report counts the outcomes of the changes handled in one pass.
type Report struct {
	Applied, Unchanged, Stale, Removed int

	// Declined counts the documents, among Applied, that were stored
	// without something a model declined to add: a summary, an embedding,
	// or both. Each is also logged, with the reason.
	Declined int
}

func (r *Report) count(o Outcome) {
	switch o {
	case Applied:
		r.Applied++
	case Unchanged:
		r.Unchanged++
	case Stale:
		r.Stale++
	case Removed:
		r.Removed++
	}
}

// Follow reads a source's change log and applies every change in it, one
// after another, until ctx ends or something fails. It returns the reason it
// stopped, which is never nil.
//
// After each change is applied its checkpoint is saved in the ledger, and
// the next call to Follow resumes from there. A change whose checkpoint was
// not saved in time is simply delivered again, which Apply shrugs off.
func (s *Syncer) Follow(ctx context.Context, src source.Subscriber) error {
	name := "feed/" + src.Name()
	checkpoint, err := s.ledger.Checkpoint(ctx, name)
	if err != nil {
		return fmt.Errorf("ingest: following %s: %w", src.Name(), err)
	}

	for change, err := range src.Subscribe(ctx, checkpoint) {
		if err != nil {
			return fmt.Errorf("ingest: following %s: %w", src.Name(), err)
		}
		if _, err := s.Apply(ctx, change); err != nil {
			return err
		}
		if len(change.Checkpoint) > 0 {
			if err := s.ledger.SaveCheckpoint(ctx, name, change.Checkpoint); err != nil {
				return fmt.Errorf("ingest: following %s: %w", src.Name(), err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("ingest: the change log of %s ended", src.Name())
}

// Deliver applies the changes in one pushed delivery. It returns the
// Receiver's error if the delivery is rejected; errors.Is tells
// source.ErrUnauthorized and source.ErrMalformed apart from a failure to
// apply.
//
// A delivery that fails part-way may be delivered again in full: applying
// the changes that had already gone through does nothing the second time.
func (s *Syncer) Deliver(ctx context.Context, src source.Receiver, delivery source.Delivery) error {
	changes, err := src.Receive(ctx, delivery)
	if err != nil {
		return fmt.Errorf("ingest: delivery from %s: %w", src.Name(), err)
	}
	for _, change := range changes {
		if _, err := s.Apply(ctx, change); err != nil {
			return err
		}
	}
	return nil
}

// maxDeliveryBytes bounds the body of a pushed delivery. GitHub, the most
// generous of the usual senders, caps its webhook payloads at 25 MB.
const maxDeliveryBytes = 25 << 20

// Handler returns the HTTP handler to mount at the URL a source pushes to:
//
//	http.Handle("POST /webhooks/github", syncer.Handler(github))
//
// It answers 204 when a delivery has been applied, 401 when it is not
// authentic, 400 when it cannot be understood, and 500 when applying it
// failed. Senders redeliver after a 500, which is exactly what is wanted.
func (s *Syncer) Handler(src source.Receiver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "deliveries must be POSTed", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxDeliveryBytes))
		if err != nil {
			status := http.StatusBadRequest
			if tooLarge := new(http.MaxBytesError); errors.As(err, &tooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			http.Error(w, "the delivery could not be read", status)
			return
		}

		err = s.Deliver(r.Context(), src, source.Delivery{Header: r.Header, Body: body})
		switch {
		case err == nil:
			w.WriteHeader(http.StatusNoContent)
		case errors.Is(err, source.ErrUnauthorized):
			http.Error(w, "the delivery is not authentic", http.StatusUnauthorized)
		case errors.Is(err, source.ErrMalformed):
			http.Error(w, "the delivery is malformed", http.StatusBadRequest)
		default:
			// The failure is ours, not the sender's. The details go to the
			// log and stay out of the response: they describe our insides.
			s.logger.ErrorContext(r.Context(), "a delivery could not be applied", "source", src.Name(), "error", err)
			http.Error(w, "the delivery could not be applied", http.StatusInternalServerError)
		}
	})
}

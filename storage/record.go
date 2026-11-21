package storage

import (
	"context"
	"fmt"
	"slices"

	"github.com/umran/grafsol/graph"
)

// Record keeps what the source of each document last said of it.
//
// The graph holds what was made of that: which kind of edge each reference
// became, which topic each label filed the document under. Keeping what was
// said beside it is what lets the making be done again, when a team changes
// its rules, without reading any source a second time.
//
// Implementations must be safe for concurrent use.
type Record interface {
	// PutSaid stores what was said of a document, in place of whatever was
	// stored for the same document before.
	PutSaid(ctx context.Context, said Said) error

	// DeleteSaid removes what is stored for a document. A document with
	// nothing stored is ignored.
	DeleteSaid(ctx context.Context, document graph.ID) error

	// Said returns what is stored for the given documents, keyed by ID. A
	// document with nothing stored is simply absent from the map.
	Said(ctx context.Context, documents []graph.ID) (map[graph.ID]Said, error)

	// Recorded lists the documents that have something stored, a page at a
	// time, in the order of their IDs, comparing bytes. Passing each call's
	// next as the following call's cursor, until next is empty, visits every
	// such document exactly once. next is empty exactly when no documents
	// remain after the page.
	Recorded(ctx context.Context, cursor string) (documents []graph.ID, next string, err error)
}

// Said is what a document's source last said of it: everything that the
// document's part of the graph is made from.
type Said struct {
	// Document is the document as its source and the summariser left it.
	Document graph.Document

	// References and Labels are what the source reported, before anything
	// had decided what they mean: what the document points at, and the
	// labels on it. Rules (package mapping) turn them into edges.
	References []graph.Reference
	Labels     []graph.Label

	// Edges are the edges that the source stated outright, kind and all;
	// they go into the graph as they are. Topics describes the topics those
	// edges point at.
	Edges  []graph.Edge
	Topics []graph.Topic
}

// Validate reports what is wrong with what was said, or nil. Backends call
// it before storing, so that they all refuse the same things.
func (s Said) Validate() error {
	id := s.Document.ID
	if err := graph.ValidateVertex(&s.Document); err != nil {
		return fmt.Errorf("storage: what was said: %w", err)
	}
	for _, reference := range s.References {
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("storage: what was said of %s: %w", id, err)
		}
	}
	for _, label := range s.Labels {
		if err := label.Validate(); err != nil {
			return fmt.Errorf("storage: what was said of %s: %w", id, err)
		}
	}
	for _, edge := range s.Edges {
		if err := edge.Validate(); err != nil {
			return fmt.Errorf("storage: what was said of %s: %w", id, err)
		}
		if edge.From != id {
			return fmt.Errorf("storage: what was said of %s holds an edge that starts at %s", id, edge.From)
		}
	}
	for i := range s.Topics {
		if err := graph.ValidateVertex(&s.Topics[i]); err != nil {
			return fmt.Errorf("storage: what was said of %s: %w", id, err)
		}
	}
	return nil
}

// Clone returns a copy that shares no memory with the original.
func (s Said) Clone() Said {
	clone := Said{
		Document:   *s.Document.Clone(),
		References: slices.Clone(s.References),
		Labels:     slices.Clone(s.Labels),
		Edges:      slices.Clone(s.Edges),
		Topics:     slices.Clone(s.Topics),
	}
	for i := range clone.Topics {
		clone.Topics[i] = *clone.Topics[i].Clone()
	}
	return clone
}

package graph

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// Vertex is a node of the graph. Exactly two types implement it, *Document
// and *Topic; the unexported method keeps it that way, so a type switch over
// a Vertex can cover every case:
//
//	switch v := vertex.(type) {
//	case *graph.Document:
//	case *graph.Topic:
//	}
type Vertex interface {
	// VertexID returns the ID of the vertex.
	VertexID() ID

	isVertex()
}

// DocumentType says what sort of document a Document is. The set is open: a
// new source may introduce its own types alongside the well-known ones below.
type DocumentType string

const (
	TypeIssue        DocumentType = "issue"
	TypePullRequest  DocumentType = "pull_request"
	TypeMergeRequest DocumentType = "merge_request"
	TypeTicket       DocumentType = "ticket"
	TypePage         DocumentType = "page"
)

// Document is a vertex standing for one unit of written work in an external
// system. The graph keeps what is needed to find the document and to judge
// its relevance, not its full text: URL points back to the original.
type Document struct {
	ID     ID
	Type   DocumentType
	Source string // the system that owns the document: "github", "jira", ...
	URL    string // where a person can read the original
	Title  string

	// Summary is a short account of what the document says, and Embedding
	// places the document in a vector space for similarity search. Sources
	// usually leave both empty; enrichers fill them in during ingestion.
	Summary   string
	Embedding []float32

	CreatedAt time.Time
	UpdatedAt time.Time
}

// VertexID returns the document's ID.
func (d *Document) VertexID() ID { return d.ID }

func (*Document) isVertex() {}

// Clone returns a copy of the document that shares no memory with it.
func (d *Document) Clone() *Document {
	clone := *d
	clone.Embedding = slices.Clone(d.Embedding)
	return &clone
}

// Topic is a vertex standing for a subject. Documents point at topics with
// InTopic edges (the document belongs to the topic) and TaggedWith edges
// (somebody labelled the document with it), which makes a topic a hub that
// connects otherwise unrelated documents.
type Topic struct {
	ID        ID
	Name      string // as people write it: "Good First Issue"
	Summary   string // what the topic covers
	Embedding []float32
}

// VertexID returns the topic's ID.
func (t *Topic) VertexID() ID { return t.ID }

func (*Topic) isVertex() {}

// Clone returns a copy of the topic that shares no memory with it.
func (t *Topic) Clone() *Topic {
	clone := *t
	clone.Embedding = slices.Clone(t.Embedding)
	return &clone
}

// CloneVertex returns a copy of v that shares no memory with it.
func CloneVertex(v Vertex) Vertex {
	switch v := v.(type) {
	case *Document:
		return v.Clone()
	case *Topic:
		return v.Clone()
	}
	return nil
}

// ValidateVertex reports what is wrong with a vertex, or nil if it can be
// stored: it must be a non-nil *Document or *Topic whose ID is well formed
// and of the matching kind.
func ValidateVertex(v Vertex) error {
	switch v := v.(type) {
	case *Document:
		if v == nil {
			return errors.New("graph: nil *Document")
		}
		return validateIDOfKind(v.ID, KindDocument)
	case *Topic:
		if v == nil {
			return errors.New("graph: nil *Topic")
		}
		return validateIDOfKind(v.ID, KindTopic)
	}
	return errors.New("graph: nil vertex")
}

func validateIDOfKind(id ID, want VertexKind) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if got := id.Kind(); got != want {
		return fmt.Errorf("graph: ID %q names a %s, not a %s", string(id), got, want)
	}
	return nil
}

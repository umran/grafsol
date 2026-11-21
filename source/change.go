package source

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/umran/grafsol/graph"
)

// Change is the complete state of one document at one moment, or word that
// the document is gone.
//
// A change stands on its own: it lists the document's fields and everything
// that the edges starting at the document are made from, so applying it
// never depends on what was applied before.
type Change struct {
	// Document is the document as it now is. For a removal only ID and
	// Source need to be set. Summary and Embedding are normally left empty
	// here and filled in by enrichers.
	Document graph.Document

	// Version orders the changes of this one document: of two changes, the
	// one with the larger Version is the newer. It is whatever the source
	// system offers for the purpose, such as a revision number or the
	// document's last-modified time. It says nothing about other documents
	// and is never used as a position in a feed; that is what Checkpoint is
	// for.
	Version uint64

	// Deleted reports that the document no longer exists at the source.
	Deleted bool

	// Content is the document's raw text. Enrichers read it to write the
	// summary and compute the embedding; it is not stored in the graph.
	Content string

	// References is everything the document points at, told the way the
	// source told it: a mention found in its text, a parent the source
	// records, a relation the source gives a name. Labels are the labels,
	// tags or categories the source put on the document.
	//
	// A connector reports these and stops there. Which kind of edge a
	// reference becomes, and which topic a label files the document under,
	// is for the graph's rules to say (package mapping), and a team may
	// change its mind about that without the source being read again.
	References []graph.Reference
	Labels     []graph.Label

	// Edges is for a source that states an edge outright, kind and all,
	// because it knows what the relationship is and no rule has anything to
	// add. Such edges go into the graph as they are. Each edge's From must
	// be Document.ID. Topics describes the topics those edges point at, so
	// that a topic can be created the first time a document refers to it.
	//
	// References, Labels and Edges together are everything that starts at
	// the document. An edge the document used to have, and that nothing in
	// this change gives rise to, is removed from the graph.
	Edges  []graph.Edge
	Topics []graph.Topic

	// EmbeddingSpace names the embedding space that the embeddings in this
	// change are in: the document's, and those of Topics. Whoever fills in
	// an embedding sets it, which is normally the Embed enricher, with the
	// name its embedder gives (see model.Embedder). A change that carries
	// an embedding and no EmbeddingSpace is not valid: the graph holds to
	// one space, and could not tell whether these belong in it.
	EmbeddingSpace string

	// Checkpoint is the position in the feed or scan that produced this
	// change, to resume from once the change has been applied. It is empty
	// when the change did not come from something resumable.
	Checkpoint Checkpoint
}

// Checkpoint is a position in a source's change log or scan. It is opaque:
// only the source that issued it can read it. The sync engine stores it and
// hands it back, nothing more.
type Checkpoint []byte

// Delivery is one message pushed by a source: a webhook request, or a
// message taken off a queue.
type Delivery struct {
	Header http.Header
	Body   []byte
}

var (
	// ErrUnauthorized is returned, wrapped, by Receiver.Receive for a
	// delivery that fails authentication, such as a missing or wrong
	// signature.
	ErrUnauthorized = errors.New("source: delivery is not authentic")

	// ErrMalformed is returned, wrapped, by Receiver.Receive for a
	// delivery that is authentic but cannot be understood.
	ErrMalformed = errors.New("source: delivery is malformed")
)

// Mention records that the document's text points at another document.
// written is the reference as it stood in the text ("#12", "acme/web#3", an
// address), and context is the line it was on: rules look at both to decide
// what the mention means.
func (c *Change) Mention(target graph.ID, written, context string) {
	c.References = append(c.References, graph.Reference{Target: target, Form: graph.FormMention, Written: written, Context: context})
}

// MentionURL records an address found in the document's text that the
// connector cannot put a document to, typically because it points into
// another system. The sync engine asks every source whose it is. context is
// the line the address was on.
func (c *Change) MentionURL(url, context string) {
	c.References = append(c.References, graph.Reference{URL: url, Form: graph.FormMention, Written: url, Context: context})
}

// Parent records that the source places the document under another: a
// sub-issue under its issue, a page under its parent page. context says how,
// in the source's own words, for example "sub-issue of".
func (c *Change) Parent(parent graph.ID, context string) {
	c.References = append(c.References, graph.Reference{Target: parent, Form: graph.FormParent, Context: context})
}

// Label records a label, tag or category that the source put on the
// document. description says what the label is for, if the source knows.
func (c *Change) Label(name, description string) {
	c.Labels = append(c.Labels, graph.Label{Name: name, Description: description})
}

// Validate reports what is wrong with the change, or nil. The sync engine
// refuses a change that does not validate.
func (c *Change) Validate() error {
	id := c.Document.ID
	if err := graph.ValidateVertex(&c.Document); err != nil {
		return fmt.Errorf("source: change: %w", err)
	}
	if c.Document.Source == "" {
		return fmt.Errorf("source: change for %s names no source", id)
	}
	if c.Document.Source != id.Source() {
		return fmt.Errorf("source: change for %s claims to come from %q", id, c.Document.Source)
	}
	if c.Deleted {
		return nil // nothing else matters about a document that is gone
	}

	for _, edge := range c.Edges {
		if err := edge.Validate(); err != nil {
			return fmt.Errorf("source: change for %s: %w", id, err)
		}
		if edge.From != id {
			return fmt.Errorf("source: change for %s carries an edge that starts at %s", id, edge.From)
		}
	}
	for i := range c.Topics {
		if err := graph.ValidateVertex(&c.Topics[i]); err != nil {
			return fmt.Errorf("source: change for %s: %w", id, err)
		}
	}
	for _, reference := range c.References {
		if err := reference.Validate(); err != nil {
			return fmt.Errorf("source: change for %s: %w", id, err)
		}
	}
	for _, label := range c.Labels {
		if err := label.Validate(); err != nil {
			return fmt.Errorf("source: change for %s: %w", id, err)
		}
	}
	if c.HasEmbeddings() && c.EmbeddingSpace == "" {
		return fmt.Errorf("source: change for %s carries an embedding and does not say what embedding space it is in", id)
	}
	return nil
}

// HasEmbeddings reports whether the change carries an embedding: the
// document's, or that of one of its Topics.
func (c *Change) HasEmbeddings() bool {
	if len(c.Document.Embedding) > 0 {
		return true
	}
	for i := range c.Topics {
		if len(c.Topics[i].Embedding) > 0 {
			return true
		}
	}
	return false
}

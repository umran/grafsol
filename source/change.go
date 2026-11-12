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
// A change stands on its own: it lists the document's fields and every edge
// that starts at the document, so applying it never depends on what was
// applied before.
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

	// Edges is every edge that starts at the document. Edges the document
	// used to have and that are missing from this list are removed from the
	// graph. Each edge's From must be Document.ID.
	Edges []graph.Edge

	// Topics describes the topics that Edges point at, so that a topic can
	// be created the first time a document refers to it.
	Topics []graph.Topic

	// EmbeddingSpace names the embedding space that the embeddings in this
	// change are in: the document's, and those of Topics. Whoever fills in
	// an embedding sets it, which is normally the Embed enricher, with the
	// name its embedder gives (see model.Embedder). A change that carries
	// an embedding and no EmbeddingSpace is not valid: the graph holds to
	// one space, and could not tell whether these belong in it.
	EmbeddingSpace string

	// Links are URLs found in the document that the connector could not
	// turn into edges itself, typically because they point into another
	// system. The sync engine resolves them across all sources.
	Links []Link

	// Checkpoint is the position in the feed or scan that produced this
	// change, to resume from once the change has been applied. It is empty
	// when the change did not come from something resumable.
	Checkpoint Checkpoint
}

// Link is a URL found in a document.
type Link struct {
	URL string

	// Context is the text around the link. It becomes the Summary of the
	// edge the link turns into.
	Context string
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

// LinkTo records that the document references another document. context is
// the text around the reference; it becomes the edge's summary.
func (c *Change) LinkTo(target graph.ID, context string) {
	c.Edges = append(c.Edges, graph.Edge{From: c.Document.ID, Kind: graph.LinksTo, To: target, Summary: context})
}

// LinkURL records a URL found in the document, for the sync engine to
// resolve. context is the text around the URL.
func (c *Change) LinkURL(url, context string) {
	c.Links = append(c.Links, Link{URL: url, Context: context})
}

// ChildOf records that the document sits under parent in a hierarchy.
// summary says how, for example "sub-issue of".
func (c *Change) ChildOf(parent graph.ID, summary string) {
	c.Edges = append(c.Edges, graph.Edge{From: c.Document.ID, Kind: graph.ChildOf, To: parent, Summary: summary})
}

// Tag records that the document carries the named tag. description says
// what the tag means, if the source knows; it becomes the topic's summary
// when the topic is first created.
func (c *Change) Tag(name, description string) {
	c.pointAtTopic(graph.TaggedWith, name, description)
}

// InTopic records that the document belongs to the named topic. description
// says what the topic covers, if the source knows.
func (c *Change) InTopic(name, description string) {
	c.pointAtTopic(graph.InTopic, name, description)
}

func (c *Change) pointAtTopic(kind graph.EdgeKind, name, description string) {
	topic := graph.Topic{ID: graph.TopicID(name), Name: name, Summary: description}
	c.Topics = append(c.Topics, topic)
	c.Edges = append(c.Edges, graph.Edge{From: c.Document.ID, Kind: kind, To: topic.ID})
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
	for _, link := range c.Links {
		if link.URL == "" {
			return fmt.Errorf("source: change for %s carries a link with no URL", id)
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

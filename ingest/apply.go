package ingest

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
)

// Apply makes the graph reflect one change, and reports what it did.
//
// It is safe to call Apply for the same document from several goroutines or
// processes at once, to call it again with a change already applied, and to
// call it with changes in the wrong order. See the package documentation
// for how each of those is handled.
func (s *Syncer) Apply(ctx context.Context, c source.Change) (Outcome, error) {
	outcome, _, err := s.apply(ctx, c, notAScan)
	return outcome, err
}

// notAScan is the scanStarted of a change that comes from a feed. The
// ledger's clock never reads zero, so no scan begins with this reading.
const notAScan uint64 = 0

// apply is Apply with one more thing known: whether the change comes from a
// scan of the source, as opposed to a feed, and if so what the ledger's
// clock read when that scan began. A scan shows what exists right now,
// which matters in two places; see decide and sighted.
//
// declined reports that the change was applied without something a model
// declined to add to the document.
func (s *Syncer) apply(ctx context.Context, c source.Change, scanStarted uint64) (outcome Outcome, declined bool, err error) {
	if err := c.Validate(); err != nil {
		return 0, false, fmt.Errorf("ingest: %w", err)
	}
	id := c.Document.ID
	fromScan := scanStarted != notAScan

	// The change was passed by value, but its slices still share memory
	// with the caller's. Copy them before anything is added.
	c.Edges = slices.Clone(c.Edges)
	c.Topics = slices.Clone(c.Topics)

	var fingerprint string
	if !c.Deleted {
		s.resolveLinks(&c)
		normalize(&c)
		var err error
		if fingerprint, err = fingerprintOf(&c); err != nil {
			return 0, false, fmt.Errorf("ingest: %s: %w", id, err)
		}
	}

	// A first look at the ledger, before any real work is done. Repeated
	// deliveries and late ones mostly end here.
	mark, known, err := s.ledger.Mark(ctx, id)
	if err != nil {
		return 0, false, fmt.Errorf("ingest: applying %s: %w", id, err)
	}
	outcome = decide(mark, known, c, fingerprint, scanStarted)
	if outcome == Stale && !sighted(mark, c, fromScan) {
		return Stale, false, nil
	}

	// Enrichers may call models and take seconds. They run now, before the
	// document's lock is taken, so that the lock is held only briefly.
	enriched := false
	if outcome == Applied {
		if declined, err = s.enrich(ctx, &c); err != nil {
			return 0, false, err
		}
		enriched = true
	}

	unlock, err := s.ledger.Lock(ctx, string(id))
	if err != nil {
		return 0, false, fmt.Errorf("ingest: applying %s: %w", id, err)
	}
	defer unlock()

	// A second look, now that no other worker can touch this document.
	// Another worker may have got here first, so the first look counts for
	// nothing; this one is final.
	mark, known, err = s.ledger.Mark(ctx, id)
	if err != nil {
		return 0, false, fmt.Errorf("ingest: applying %s: %w", id, err)
	}
	outcome = decide(mark, known, c, fingerprint, scanStarted)

	switch outcome {
	case Stale:
		// Nothing is written to the graph: the ledger holds something newer.
		// But if a scan listed the document, the document exists, and its
		// mark is recorded again, just as it is, to note that it was seen.
		if sighted(mark, c, fromScan) {
			err = s.ledger.Record(ctx, mark)
		}

	case Unchanged:
		// Nothing to write to the graph. The mark is recorded again all the
		// same: that notes the document was seen, which is what stops a
		// scan from concluding it is gone.
		mark.Version = c.Version
		err = s.ledger.Record(ctx, mark)

	case Removed:
		err = s.remove(ctx, id, c.Document.Source, c.Version)

	case Applied:
		if !enriched {
			// Rare: the first look said there was nothing to do and the
			// second disagrees. Enrich now, lock or no lock.
			if declined, err = s.enrich(ctx, &c); err != nil {
				return 0, false, err
			}
		}
		err = s.write(ctx, c, fingerprint)
	}
	if err != nil {
		return 0, false, fmt.Errorf("ingest: applying %s: %w", id, err)
	}
	return outcome, declined && outcome == Applied, nil
}

// decide compares a change with what the ledger last recorded for the same
// document and says what should become of the change. It is the whole of
// the Syncer's policy on ordering, and it touches nothing.
//
// scanStarted is what the ledger's clock read when the scan that listed the
// change began, or notAScan for a change from a feed.
func decide(mark storage.Mark, known bool, c source.Change, fingerprint string, scanStarted uint64) Outcome {
	switch {
	case !known && c.Deleted:
		// A removal for a document we never had. Record it all the same, so
		// that an edit arriving late cannot create the document.
		return Removed

	case !known:
		return Applied

	case mark.Deleted && !c.Deleted:
		// The ledger holds a removal and the change says the document
		// exists. With a newer version there is no doubt: it is back.
		if c.Version > mark.Version {
			return Applied
		}
		// A scan lists what exists now. One that began after the removal
		// was recorded has the later word, whatever the versions say: the
		// document was restored and no news of that arrived.
		if scanStarted > mark.Seen {
			return Applied
		}
		// What is left is a change no newer than the removal, from a feed
		// or from a scan that was under way when the removal was recorded
		// and may have read its listing first. The removal stands: wrongly
		// keeping a document that is gone is worse than briefly lacking one
		// that is not, and if the document does exist, the next scan will
		// say so.
		return Stale

	case c.Version < mark.Version:
		return Stale

	case mark.Deleted:
		// A removal of a document already removed.
		return Unchanged

	case c.Deleted:
		return Removed

	case fingerprint == mark.Fingerprint:
		return Unchanged

	default:
		return Applied
	}
}

// sighted reports whether a change that decide found stale has shown, all
// the same, that its document exists. A scan that lists a document shows as
// much, whatever version it lists; a feed may only be repeating old news.
// The exception is a ledger that holds a removal: decide has let the removal
// stand, so there is no document to have seen.
func sighted(mark storage.Mark, c source.Change, fromScan bool) bool {
	return fromScan && !c.Deleted && !mark.Deleted
}

// write makes the stored document, its edges and its topics match the
// change, then records the change in the ledger. The caller holds the
// document's lock.
func (s *Syncer) write(ctx context.Context, c source.Change, fingerprint string) error {
	id := c.Document.ID

	// Embeddings can be compared only if they are all in one embedding
	// space, so a change that carries any claims the graph for theirs
	// before a thing is written. The first such claim settles what the
	// graph holds. If the graph already holds another space the claim
	// fails, and with it the change.
	if c.HasEmbeddings() {
		if err := s.store.ClaimEmbeddingSpace(ctx, c.EmbeddingSpace); err != nil {
			return err
		}
	}

	// Edges: the document's stored out-edges become exactly the change's.
	// New edges are stored before old ones are deleted, so that a reader
	// catching the document mid-change sees too much rather than too little.
	stored, err := s.outEdges(ctx, id)
	if err != nil {
		return err
	}
	toStore, toDelete := diffEdges(c.Edges, stored)
	if err := s.store.PutEdges(ctx, toStore); err != nil {
		return err
	}
	if err := s.store.DeleteEdges(ctx, toDelete); err != nil {
		return err
	}

	if err := s.createMissingTopics(ctx, c.Topics); err != nil {
		return err
	}

	document := c.Document
	if err := s.store.PutVertices(ctx, []graph.Vertex{&document}); err != nil {
		return err
	}

	// The mark goes last. If anything above failed, or the process died on
	// the way, the ledger still shows the old state and the change will be
	// applied again from the top.
	return s.ledger.Record(ctx, storage.Mark{
		ID:          id,
		Source:      c.Document.Source,
		Version:     c.Version,
		Fingerprint: fingerprint,
	})
}

// remove takes a document and its out-edges out of the graph and leaves a
// tombstone in the ledger. The caller holds the document's lock.
//
// Edges that point at the document from other documents stay: those
// documents still say what they say. They simply lead nowhere, as they would
// have before the document was first ingested.
func (s *Syncer) remove(ctx context.Context, id graph.ID, sourceName string, version uint64) error {
	stored, err := s.outEdges(ctx, id)
	if err != nil {
		return err
	}
	if err := s.store.DeleteEdges(ctx, stored); err != nil {
		return err
	}
	if err := s.store.DeleteVertices(ctx, []graph.ID{id}); err != nil {
		return err
	}
	return s.ledger.Record(ctx, storage.Mark{ID: id, Source: sourceName, Version: version, Deleted: true})
}

// outEdges returns every edge that starts at a vertex, however many pages
// that takes.
func (s *Syncer) outEdges(ctx context.Context, id graph.ID) ([]graph.Edge, error) {
	var edges []graph.Edge
	query := storage.EdgeQuery{Vertex: id, Direction: graph.Out}
	for {
		page, err := s.store.Edges(ctx, query)
		if err != nil {
			return nil, err
		}
		edges = append(edges, page.Edges...)
		if page.Next == "" {
			return edges, nil
		}
		query.Cursor = page.Next
	}
}

// diffEdges compares the edges a document should have with the edges it has
// and returns the ones to store (missing, or stored with another summary)
// and the ones to delete.
func diffEdges(wanted, stored []graph.Edge) (toStore, toDelete []graph.Edge) {
	wantedTriples := make(map[graph.Edge]bool, len(wanted))
	for _, edge := range wanted {
		wantedTriples[edge.Triple()] = true
	}
	storedSummaries := make(map[graph.Edge]string, len(stored))
	for _, edge := range stored {
		storedSummaries[edge.Triple()] = edge.Summary
		if !wantedTriples[edge.Triple()] {
			toDelete = append(toDelete, edge)
		}
	}
	for _, edge := range wanted {
		if summary, exists := storedSummaries[edge.Triple()]; !exists || summary != edge.Summary {
			toStore = append(toStore, edge)
		}
	}
	return toStore, toDelete
}

// createMissingTopics stores the topics that are not in the graph yet.
//
// A topic that already exists is left exactly as it is. Sources mention
// topics in passing, usually knowing no more than a name, and must not
// overwrite a summary or an embedding that an enricher has since given the
// topic.
func (s *Syncer) createMissingTopics(ctx context.Context, topics []graph.Topic) error {
	if len(topics) == 0 {
		return nil
	}
	ids := make([]graph.ID, len(topics))
	for i, topic := range topics {
		ids[i] = topic.ID
	}
	existing, err := s.store.GetVertices(ctx, ids)
	if err != nil {
		return err
	}
	var missing []graph.Vertex
	for i := range topics {
		if _, exists := existing[topics[i].ID]; !exists {
			missing = append(missing, &topics[i])
		}
	}
	return s.store.PutVertices(ctx, missing)
}

// resolveLinks turns the URLs a source found in a document into LinksTo
// edges, by asking every known source whether it recognizes each URL. URLs
// that no source recognizes lead outside the graph and are dropped.
func (s *Syncer) resolveLinks(c *source.Change) {
	for _, link := range c.Links {
		for _, src := range s.sources {
			if target, recognized := src.Resolve(link.URL); recognized {
				c.LinkTo(target, link.Context)
				break
			}
		}
	}
}

// enrich runs the enrichers over a change. It reports whether a model
// declined to add something, which the document will then be stored without.
func (s *Syncer) enrich(ctx context.Context, c *source.Change) (declined bool, err error) {
	if len(s.enrichers) == 0 {
		return false, nil
	}
	for _, enricher := range s.enrichers {
		err := enricher.Enrich(ctx, c)
		if errors.Is(err, model.ErrDeclined) {
			// A model that refuses a document will refuse it again, so
			// failing the change here would make it fail for ever. The
			// document is stored without what this enricher would have
			// added, and the refusal is logged and counted.
			s.logger.WarnContext(ctx, "a model declined to enrich a document, which is stored without",
				"document", c.Document.ID, "error", err)
			declined = true
			continue
		}
		if err != nil {
			return false, fmt.Errorf("ingest: enriching %s: %w", c.Document.ID, err)
		}
	}
	// Enrichers may have added edges, topics and embeddings. Hold those to
	// the same rules as the source's own.
	if err := c.Validate(); err != nil {
		return false, fmt.Errorf("ingest: after enriching: %w", err)
	}
	normalize(c)
	return declined, nil
}

// normalize puts a change into canonical form, so that two changes that
// describe the same state look the same: edges without repeats or
// self-references and in a fixed order, and an entry in Topics for every
// topic an edge points at.
func normalize(c *source.Change) {
	id := c.Document.ID

	edges := make(map[graph.Edge]graph.Edge, len(c.Edges))
	for _, edge := range c.Edges {
		if edge.To == id {
			continue // a document that points at itself says nothing
		}
		// When a document refers to the same thing twice, keep the first
		// reference that comes with a summary.
		if kept, repeated := edges[edge.Triple()]; repeated && kept.Summary != "" {
			continue
		}
		edges[edge.Triple()] = edge
	}
	c.Edges = slices.SortedFunc(maps.Values(edges), graph.Edge.Compare)

	topics := make(map[graph.ID]graph.Topic, len(c.Topics))
	for _, topic := range c.Topics {
		if _, repeated := topics[topic.ID]; !repeated {
			topics[topic.ID] = topic
		}
	}
	for _, edge := range c.Edges {
		if edge.To.Kind() != graph.KindTopic {
			continue
		}
		if _, described := topics[edge.To]; !described {
			// The source pointed at a topic without describing it. The
			// topic's key is the best name on offer.
			topics[edge.To] = graph.Topic{ID: edge.To, Name: edge.To.Key()}
		}
	}
	c.Topics = slices.SortedFunc(maps.Values(topics), func(a, b graph.Topic) int {
		return cmp.Compare(a.ID, b.ID)
	})
}

// fingerprintOf hashes everything about a change that ends up in the graph
// or is read by an enricher. Two changes with the same fingerprint would
// leave the graph in the same state, so the second need not be applied.
//
// The version is left out on purpose: a document whose version moved while
// nothing else did has not changed in any way the graph can show.
func fingerprintOf(c *source.Change) (string, error) {
	type edge struct {
		Kind    graph.EdgeKind `json:"kind"`
		To      graph.ID       `json:"to"`
		Summary string         `json:"summary"`
	}
	type topic struct {
		ID        graph.ID  `json:"id"`
		Name      string    `json:"name"`
		Summary   string    `json:"summary"`
		Embedding []float32 `json:"embedding"`
	}
	// The fields are spelled out here rather than taken from the graph and
	// source types, so that renaming a field over there cannot quietly
	// change every fingerprint and cause everything to be applied again.
	state := struct {
		Format    int                `json:"format"` // raise when a field below changes meaning
		Type      graph.DocumentType `json:"type"`
		URL       string             `json:"url"`
		Title     string             `json:"title"`
		Summary   string             `json:"summary"`
		Embedding []float32          `json:"embedding"`
		CreatedAt string             `json:"created_at"`
		UpdatedAt string             `json:"updated_at"`
		Content   string             `json:"content"`
		Edges     []edge             `json:"edges"`
		Topics    []topic            `json:"topics"`
		// Left out when empty, as it is for every change whose source
		// supplied no embeddings of its own: fingerprints taken before
		// this field existed stay what they were.
		EmbeddingSpace string `json:"embedding_space,omitempty"`
	}{
		Format:    1,
		Type:      c.Document.Type,
		URL:       c.Document.URL,
		Title:     c.Document.Title,
		Summary:   c.Document.Summary,
		Embedding: c.Document.Embedding,
		// Times are written in UTC so that one instant always hashes alike.
		CreatedAt: c.Document.CreatedAt.UTC().Format(time.RFC3339Nano),
		UpdatedAt: c.Document.UpdatedAt.UTC().Format(time.RFC3339Nano),
		Content:   c.Content,

		EmbeddingSpace: c.EmbeddingSpace,
	}
	for _, e := range c.Edges {
		state.Edges = append(state.Edges, edge{Kind: e.Kind, To: e.To, Summary: e.Summary})
	}
	for _, t := range c.Topics {
		state.Topics = append(state.Topics, topic{ID: t.ID, Name: t.Name, Summary: t.Summary, Embedding: t.Embedding})
	}

	encoded, err := json.Marshal(state)
	if err != nil {
		// Only a number that is not a number can cause this.
		return "", fmt.Errorf("the change cannot be fingerprinted: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

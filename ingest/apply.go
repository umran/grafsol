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
	"strings"
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
	// with the caller's. Copy them before anything is added or filled in.
	c.References = slices.Clone(c.References)
	c.Labels = slices.Clone(c.Labels)
	c.Edges = slices.Clone(c.Edges)
	c.Topics = slices.Clone(c.Topics)

	var fingerprint string
	if !c.Deleted {
		s.resolve(&c)
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
		//
		// One thing may be owed first. A document last applied before the
		// graph kept what sources say has no record of its own, or only one
		// worked out from its edges. The change in hand describes the very
		// state that was applied, so the record is written from it.
		if !hasRecord(mark) {
			var adopted bool
			if adopted, err = s.adopt(ctx, c); err != nil {
				break
			}
			if adopted {
				mark.Fingerprint = fingerprint + withRecord
			}
		}
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

	case fingerprint == hashIn(mark):
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

// A mark's fingerprint is the hash of the state that was applied, followed
// by withRecord if the Syncer that applied it also wrote down what the
// source said (storage.Record). Syncers from before such records were kept
// left the bare hash, and that is how a document they applied is told from
// one whose record is whole.
const withRecord = "+said"

// hashIn returns the hash in a mark's fingerprint.
func hashIn(mark storage.Mark) string {
	return strings.TrimSuffix(mark.Fingerprint, withRecord)
}

// hasRecord reports whether the document a mark is for was applied by a
// Syncer that kept a record of what its source said.
func hasRecord(mark storage.Mark) bool {
	return strings.HasSuffix(mark.Fingerprint, withRecord)
}

// write puts on record what the change's source said, makes the stored
// document, its edges and its topics match, and then records the change in
// the ledger. The caller holds the document's lock.
func (s *Syncer) write(ctx context.Context, c source.Change, fingerprint string) error {
	id := c.Document.ID

	// What the document's part of the graph should be is worked out before
	// anything is written, so that a change which cannot be composed leaves
	// nothing behind.
	said := saidOf(&c)
	work, under, err := s.composer.prepare(ctx, said, &c)
	if err != nil {
		return err
	}

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

	// What was said goes on record, and then the graph is written. The
	// graph can always be made again from what is on record, which is what
	// a change of rules relies on later.
	if err := s.record.PutSaid(ctx, said); err != nil {
		return err
	}

	// From here on the document is on record, and whoever saves new
	// settings will compose it again afterwards. Settings saved before this
	// moment and after the work was prepared are the one case that leaves
	// out: the save found no record of the document, and the work was not
	// prepared from what it saved. So look again, and prepare again if the
	// settings have moved.
	for {
		current, err := s.composer.settingsVersion(ctx)
		if err != nil {
			return err
		}
		if current == under {
			break
		}
		if work, under, err = s.composer.prepare(ctx, said, &c); err != nil {
			return err
		}
	}
	if _, err := s.composer.carryOut(ctx, work); err != nil {
		return err
	}

	// The mark goes last. If anything above failed, or the process died on
	// the way, the ledger still shows the old state and the change will be
	// applied again from the top.
	return s.ledger.Record(ctx, storage.Mark{
		ID:          id,
		Source:      c.Document.Source,
		Version:     c.Version,
		Fingerprint: fingerprint + withRecord,
	})
}

// saidOf returns what a change's source said of its document, as it is kept
// on record: the change without its text, its version and its embeddings.
// The graph's own tables are where embeddings live.
func saidOf(c *source.Change) storage.Said {
	said := storage.Said{
		Document:   c.Document,
		References: c.References,
		Labels:     c.Labels,
		Edges:      c.Edges,
		Topics:     slices.Clone(c.Topics),
	}
	said.Document.Embedding = nil
	for i := range said.Topics {
		said.Topics[i].Embedding = nil
	}
	return said
}

// adopt puts on record what the source said of a document that was applied
// before such records were kept, and makes the document's edges again from
// it. The change is one that decide found unchanged: it describes the state
// that was applied, so nothing needs enriching and the stored document is
// left as it is. The caller holds the document's lock.
//
// The one thing an unchanged change does not carry is the summary, which an
// enricher wrote when the document was applied. It is taken from what is
// already kept: the record that an upgrade worked out from the document's
// edges, or failing that the stored document. With neither there is nothing
// to adopt, and adopt reports false.
func (s *Syncer) adopt(ctx context.Context, c source.Change) (bool, error) {
	id := c.Document.ID
	said := saidOf(&c)

	before, err := s.record.Said(ctx, []graph.ID{id})
	if err != nil {
		return false, err
	}
	if carried, onRecord := before[id]; onRecord {
		said.Document.Summary = cmp.Or(said.Document.Summary, carried.Document.Summary)
	} else {
		stored, err := s.store.GetVertices(ctx, []graph.ID{id})
		if err != nil {
			return false, err
		}
		document, isDocument := stored[id].(*graph.Document)
		if !isDocument {
			return false, nil
		}
		said.Document.Summary = cmp.Or(said.Document.Summary, document.Summary)
	}

	if err := s.record.PutSaid(ctx, said); err != nil {
		return false, err
	}
	_, err = s.composer.compose(ctx, said)
	return err == nil, err
}

// remove takes a document, its out-edges and what is on record for it out of
// the graph and leaves a tombstone in the ledger. The caller holds the
// document's lock.
//
// Edges that point at the document from other documents stay: those
// documents still say what they say. They simply lead nowhere, as they would
// have before the document was first ingested.
func (s *Syncer) remove(ctx context.Context, id graph.ID, sourceName string, version uint64) error {
	stored, err := outEdges(ctx, s.store, id)
	if err != nil {
		return err
	}
	if err := s.store.DeleteEdges(ctx, stored); err != nil {
		return err
	}
	if err := s.store.DeleteVertices(ctx, []graph.ID{id}); err != nil {
		return err
	}
	if err := s.record.DeleteSaid(ctx, id); err != nil {
		return err
	}
	return s.ledger.Record(ctx, storage.Mark{ID: id, Source: sourceName, Version: version, Deleted: true})
}

// outEdges returns every edge that starts at a vertex, however many pages
// that takes.
func outEdges(ctx context.Context, store storage.Store, id graph.ID) ([]graph.Edge, error) {
	var edges []graph.Edge
	query := storage.EdgeQuery{Vertex: id, Direction: graph.Out}
	for {
		page, err := store.Edges(ctx, query)
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

// resolve finds the documents behind the addresses that a source could not
// place itself, by asking every known source whether it recognizes each. An
// address that no source recognizes keeps an empty target: it leads outside
// the graph, and nothing is made of it.
func (s *Syncer) resolve(c *source.Change) {
	for i := range c.References {
		reference := &c.References[i]
		if reference.Target != "" || reference.URL == "" {
			continue
		}
		for _, src := range s.sources {
			if target, recognized := src.Resolve(reference.URL); recognized {
				reference.Target = target
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
	// Enrichers may have added references, labels, edges, topics and
	// embeddings. Hold those to the same rules as the source's own.
	if err := c.Validate(); err != nil {
		return false, fmt.Errorf("ingest: after enriching: %w", err)
	}
	normalize(c)
	return declined, nil
}

// normalize puts the edges a change states outright, and its topics, into
// canonical form, so that two changes that describe the same state look the
// same: edges without repeats or self-references and in a fixed order, and
// an entry in Topics for every topic an edge points at. References and
// labels are left in the order the source gave them, which is the order
// they stood in the document.
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

// fingerprintOf hashes everything a change's source said that ends up in
// the graph or is read by an enricher. Two changes with the same
// fingerprint say the same thing, so the second need not be applied.
//
// The version is left out on purpose: a document whose version moved while
// nothing else did has not changed in any way the graph can show. So are
// the graph's rules: what a source said has not changed because a rule has,
// and a change of rules is acted on by composing again (Composer.CatchUp),
// not by applying every document afresh.
//
// The hash is taken of the change as inTheFirstForm renders it, which is
// what keeps the fingerprints in ledgers filled before references and
// labels good.
func fingerprintOf(c *source.Change) (string, error) {
	edges, topics := inTheFirstForm(c)

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
	for _, e := range edges {
		state.Edges = append(state.Edges, edge{Kind: e.Kind, To: e.To, Summary: e.Summary})
	}
	for _, t := range topics {
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

// inTheFirstForm returns a change's edges and topics as changes first
// carried them, when a connector decided the kind of every edge itself: a
// mention as a links_to edge with its line for a summary, a parent as a
// child_of edge, a label as a tagged_with edge to a topic of the label's
// name.
//
// Nothing is made of this form any more except the fingerprint. It is kept
// so that a document applied before sources reported references and labels
// has the fingerprint it had then. Were it otherwise, the first scan after
// an upgrade would find every document changed and have every one
// summarised again.
func inTheFirstForm(c *source.Change) ([]graph.Edge, []graph.Topic) {
	id := c.Document.ID
	first := source.Change{
		Document: graph.Document{ID: id},
		Edges:    slices.Clone(c.Edges),
		Topics:   slices.Clone(c.Topics),
	}
	for _, label := range c.Labels {
		topic := graph.Topic{ID: graph.TopicID(label.Name), Name: label.Name, Summary: label.Description}
		first.Topics = append(first.Topics, topic)
		first.Edges = append(first.Edges, graph.Edge{From: id, Kind: "tagged_with", To: topic.ID})
	}
	// A reference whose target the connector named became an edge on the
	// spot. An address left for the sync engine became one only afterwards,
	// behind all of those. The order decides whose line is kept when a
	// document is pointed at twice.
	for _, leftForTheEngine := range []bool{false, true} {
		for _, reference := range c.References {
			if reference.Target == "" || (reference.URL != "") != leftForTheEngine {
				continue
			}
			kind := graph.EdgeKind(reference.Form) // a form with no first form of its own
			switch reference.Form {
			case graph.FormMention:
				kind = graph.LinksTo
			case graph.FormParent:
				kind = graph.ChildOf
			}
			first.Edges = append(first.Edges, graph.Edge{From: id, Kind: kind, To: reference.Target, Summary: reference.Context})
		}
	}
	normalize(&first)
	return first.Edges, first.Topics
}

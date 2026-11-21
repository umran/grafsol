package ingest

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/mapping"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
)

// Composer makes a document's part of the graph, its edges and the topics
// they point at, out of two things that are stored: what the document's
// source said of it (storage.Record), and the settings a team has chosen for
// its graph (storage.Settings).
//
// It is the one place where edges are decided, and it reads nothing but what
// is stored. That is what lets the settings change without any source being
// read again: a Syncer composes a document whenever its source reports a
// change to it, and CatchUp composes every document again after the settings
// have changed. A Composer is safe for concurrent use.
type Composer struct {
	store    storage.Store
	ledger   storage.Ledger
	record   storage.Record
	settings storage.Settings
	embedder model.Embedder // nil if topics are to be created without embeddings
	logger   *slog.Logger

	// The settings as last decoded, and the version they were decoded from.
	// Every composition asks the store which version is current, and decodes
	// only when it has moved.
	mu      sync.Mutex
	decoded *mapping.Settings
	version uint64
}

// NewComposer returns a Composer that works with the Store, Ledger, Record,
// Settings and Embedder of cfg. Like NewSyncer it panics if cfg lacks what it
// cannot do without. A Syncer has a Composer of its own; this is for a
// program that changes settings, and must compose without syncing.
func NewComposer(cfg Config) *Composer {
	cfg = cfg.complete("NewComposer")
	return &Composer{
		store:    cfg.Store,
		ledger:   cfg.Ledger,
		record:   cfg.Record,
		settings: cfg.Settings,
		embedder: cfg.Embedder,
		logger:   cfg.Logger,
	}
}

// Compose makes one document's part of the graph again from what is on
// record for it, and reports whether anything in the graph changed. A
// document with nothing on record is left alone.
func (c *Composer) Compose(ctx context.Context, document graph.ID) (changed bool, err error) {
	unlock, err := c.ledger.Lock(ctx, string(document))
	if err != nil {
		return false, fmt.Errorf("ingest: composing %s: %w", document, err)
	}
	defer unlock()

	said, err := c.record.Said(ctx, []graph.ID{document})
	if err != nil {
		return false, fmt.Errorf("ingest: composing %s: %w", document, err)
	}
	onRecord, found := said[document]
	if !found {
		return false, nil
	}
	if changed, err = c.compose(ctx, onRecord); err != nil {
		return false, fmt.Errorf("ingest: composing %s: %w", document, err)
	}
	return changed, nil
}

// composedUnder says which settings, and which edition of grafsol's own
// rules, every document in the graph has been composed under. It is kept in
// the ledger as the checkpoint named composedCheckpoint.
type composedUnder struct {
	Settings uint64 `json:"settings"`
	Edition  int    `json:"edition"`
}

const composedCheckpoint = "composed"

// CatchUp brings the whole graph up to the settings. If the settings, or the
// rules that grafsol ships with, have changed since every document was last
// composed, it composes every document on record again, and returns how
// many came out different. Otherwise it does nothing.
//
// A program that writes to the graph should call it once as it starts: that
// is what applies a change of rules, and what puts rules to a graph that was
// filled before there were any. Whatever saves new settings should call it
// straight after. If it is interrupted, the next call starts over; composing
// a document twice does no harm.
func (c *Composer) CatchUp(ctx context.Context) (changed int, err error) {
	version, err := c.settingsVersion(ctx)
	if err != nil {
		return 0, fmt.Errorf("ingest: catching up with the settings: %w", err)
	}
	// Each document is composed under the settings current at that moment,
	// which may be later ones than these. The note left at the end names
	// these all the same, so that a save made meanwhile is caught up with by
	// the next call.
	now := composedUnder{Settings: version, Edition: mapping.Edition}

	var last composedUnder
	saved, err := c.ledger.Checkpoint(ctx, composedCheckpoint)
	if err != nil {
		return 0, fmt.Errorf("ingest: catching up with the settings: %w", err)
	}
	if len(saved) > 0 {
		if err := json.Unmarshal(saved, &last); err != nil {
			return 0, fmt.Errorf("ingest: the note of what the graph was composed under is unreadable: %w", err)
		}
		if last == now {
			return 0, nil
		}
	}

	for cursor := ""; ; {
		documents, next, err := c.record.Recorded(ctx, cursor)
		if err != nil {
			return changed, fmt.Errorf("ingest: catching up with the settings: %w", err)
		}
		for _, document := range documents {
			different, err := c.Compose(ctx, document)
			if err != nil {
				return changed, err
			}
			if different {
				changed++
			}
		}
		if next == "" {
			break
		}
		cursor = next
	}

	encoded, err := json.Marshal(now)
	if err != nil {
		return changed, err
	}
	if err := c.ledger.SaveCheckpoint(ctx, composedCheckpoint, encoded); err != nil {
		return changed, fmt.Errorf("ingest: catching up with the settings: %w", err)
	}
	return changed, nil
}

// compose makes a document's edges and topics what the record and the
// settings say they should be, and reports whether that changed anything.
// The stored document itself is left alone: nothing about it has changed.
// The caller holds the document's lock.
//
// The document must be on record already. That is what makes it safe to read
// the settings here and now: whoever saves new settings composes, after the
// save, every document then on record, and waits for this lock to do it. So
// a composition made from settings that have just been replaced is always
// followed by one made from the new.
func (c *Composer) compose(ctx context.Context, said storage.Said) (changed bool, err error) {
	work, _, err := c.prepare(ctx, said, nil)
	if err != nil {
		return false, err
	}
	return c.carryOut(ctx, work)
}

// work is everything one composition is about to write.
type work struct {
	toStore, toDelete []graph.Edge
	topics            []graph.Vertex  // topics to create
	document          *graph.Document // the document to store, or nil to leave it as it is
}

// prepare works out everything a composition will write, and writes none of
// it. The caller holds the document's lock.
//
// It is apart from carryOut because it is where a composition can fail for
// reasons of its own: it reads the settings, and it asks a model to embed
// the topics that are about to be created. A Syncer prepares before it puts
// anything on record, so that a change which cannot be composed leaves
// nothing of itself behind.
//
// applied is the change being applied, once enriched, when that is why the
// document is being composed. The document is then stored as that change
// has it, and a topic takes the embedding the change carries for it.
//
// under is the version of the settings the work was prepared from.
func (c *Composer) prepare(ctx context.Context, said storage.Said, applied *source.Change) (prepared work, under uint64, err error) {
	settings, under, err := c.currentSettings(ctx)
	if err != nil {
		return work{}, 0, err
	}
	wanted := composition(said, settings)

	stored, err := outEdges(ctx, c.store, said.Document.ID)
	if err != nil {
		return work{}, 0, err
	}
	prepared.toStore, prepared.toDelete = diffEdges(wanted.edges, stored)

	if prepared.topics, err = c.missingTopics(ctx, wanted.topics, applied); err != nil {
		return work{}, 0, err
	}
	if applied != nil {
		document := applied.Document
		prepared.document = &document
	}
	return prepared, under, nil
}

// settingsVersion returns the version of the settings as they are stored
// now.
func (c *Composer) settingsVersion(ctx context.Context) (uint64, error) {
	_, version, err := c.settings.Settings(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading the settings: %w", err)
	}
	return version, nil
}

// carryOut writes what prepare worked out, and reports whether the graph's
// edges or topics changed.
func (c *Composer) carryOut(ctx context.Context, prepared work) (changed bool, err error) {
	// The document's stored out-edges become exactly the wanted ones. New
	// edges are stored before old ones are deleted, so that a reader
	// catching the document mid-change sees too much rather than too little.
	if err := c.store.PutEdges(ctx, prepared.toStore); err != nil {
		return false, err
	}
	if err := c.store.DeleteEdges(ctx, prepared.toDelete); err != nil {
		return false, err
	}
	if err := c.store.PutVertices(ctx, prepared.topics); err != nil {
		return false, err
	}
	if prepared.document != nil {
		if err := c.store.PutVertices(ctx, []graph.Vertex{prepared.document}); err != nil {
			return false, err
		}
	}
	return len(prepared.toStore)+len(prepared.toDelete)+len(prepared.topics) > 0, nil
}

// currentSettings returns the settings as they are stored now, and their
// version.
func (c *Composer) currentSettings(ctx context.Context) (mapping.Settings, uint64, error) {
	document, version, err := c.settings.Settings(ctx)
	if err != nil {
		return mapping.Settings{}, 0, fmt.Errorf("reading the settings: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decoded == nil || c.version != version {
		settings, err := mapping.DecodeSettings(document)
		if err != nil {
			return mapping.Settings{}, 0, err
		}
		c.decoded, c.version = &settings, version
	}
	return *c.decoded, version, nil
}

// composed is one document's part of the graph, as it should be.
type composed struct {
	// edges is every edge that starts at the document, in a fixed order.
	edges []graph.Edge

	// topics is every topic that has to exist: those the edges point at and
	// those the source described, each described as well as anything
	// describes it.
	topics []graph.Topic
}

// composition works out a document's part of the graph from what its source
// said and the settings. It is the whole of what composing decides, and it
// touches nothing.
//
// The edges are those the source stated outright and those the rules make
// of its references and labels. An edge to a topic that was merged into
// another goes to that other topic. Where two edges come out the same, the
// first that has a summary is the one kept.
func composition(said storage.Said, settings mapping.Settings) composed {
	id := said.Document.ID
	made := settings.Rules.Apply(id, id.Source(), said.References, said.Labels)

	edges := make(map[graph.Edge]graph.Edge, len(said.Edges)+len(made.Edges))
	for _, edge := range slices.Concat(said.Edges, made.Edges) {
		edge.To = settings.MergedInto(edge.To)
		if edge.To == id {
			continue // a document that points at itself says nothing
		}
		if kept, repeated := edges[edge.Triple()]; repeated && kept.Summary != "" {
			continue
		}
		edges[edge.Triple()] = edge
	}
	wanted := composed{edges: slices.SortedFunc(maps.Values(edges), graph.Edge.Compare)}

	described := make(map[graph.ID]graph.Topic, len(said.Topics)+len(made.Topics))
	for _, topic := range slices.Concat(said.Topics, made.Topics) {
		if _, already := described[topic.ID]; !already {
			described[topic.ID] = topic
		}
	}
	topics := make(map[graph.ID]graph.Topic)
	for _, edge := range wanted.edges {
		if edge.To.Kind() != graph.KindTopic {
			continue
		}
		topic, known := described[edge.To]
		if !known {
			// Nothing describes the topic: it was pointed at without a word
			// more, or is the one another was merged into. Its key is the
			// best name on offer.
			topic = graph.Topic{ID: edge.To, Name: edge.To.Key()}
		}
		topics[edge.To] = topic
	}
	// A topic the source described is created whether or not an edge points
	// at it, unless it has been merged away.
	for _, topic := range said.Topics {
		if settings.MergedInto(topic.ID) == topic.ID {
			topics[topic.ID] = described[topic.ID]
		}
	}
	wanted.topics = slices.SortedFunc(maps.Values(topics), func(a, b graph.Topic) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return wanted
}

// missingTopics returns, ready to be stored, the topics that are not in the
// graph yet.
//
// A topic that already exists is left exactly as it is. Sources mention
// topics in passing, usually knowing no more than a name, and must not
// overwrite a summary or an embedding that the topic has since been given.
//
// A topic is created with an embedding wherever one can be had: the one the
// change being applied carries for it, or else one made here, of the topic's
// name and summary, if the Composer was given an Embedder.
func (c *Composer) missingTopics(ctx context.Context, topics []graph.Topic, applied *source.Change) ([]graph.Vertex, error) {
	if len(topics) == 0 {
		return nil, nil
	}
	ids := make([]graph.ID, len(topics))
	for i, topic := range topics {
		ids[i] = topic.ID
	}
	existing, err := c.store.GetVertices(ctx, ids)
	if err != nil {
		return nil, err
	}
	var missing []*graph.Topic
	for _, topic := range topics {
		if _, exists := existing[topic.ID]; !exists {
			missing = append(missing, topic.Clone())
		}
	}

	carriedSpace := ""
	if applied != nil {
		carriedSpace = applied.EmbeddingSpace
		for _, topic := range missing {
			at := slices.IndexFunc(applied.Topics, func(carried graph.Topic) bool { return carried.ID == topic.ID })
			if at >= 0 {
				topic.Embedding = slices.Clone(applied.Topics[at].Embedding)
			}
		}
	}
	if err := c.embed(ctx, missing, carriedSpace); err != nil {
		return nil, err
	}

	vertices := make([]graph.Vertex, len(missing))
	for i, topic := range missing {
		vertices[i] = topic
	}
	return vertices, nil
}

// embed gives an embedding to each of the topics that has none, in one call
// to the model, and claims the graph for the embedder's space. Without an
// Embedder it does nothing.
//
// carriedSpace is the embedding space of the change being applied, if it
// carries embeddings of its own. Embeddings can be compared only within one
// space, so if the embedder's is another, or the graph already holds
// another, nothing is embedded and the composition fails.
func (c *Composer) embed(ctx context.Context, topics []*graph.Topic, carriedSpace string) error {
	if c.embedder == nil {
		return nil
	}
	var texts []string
	var places []*[]float32
	for _, topic := range topics {
		if text := embeddable(topic.Name, topic.Summary); len(topic.Embedding) == 0 && text != "" {
			texts = append(texts, text)
			places = append(places, &topic.Embedding)
		}
	}
	if len(texts) == 0 {
		return nil
	}

	space, err := c.embedder.Space(ctx)
	if err != nil {
		return fmt.Errorf("embedding %d topics: %w", len(texts), err)
	}
	if carriedSpace != "" && carriedSpace != space {
		return fmt.Errorf("%w: the change carries embeddings in %q, and the embedder's are in %q", storage.ErrOtherSpace, carriedSpace, space)
	}
	// Ask before embedding, so that a model is not put to work for vectors
	// the graph cannot take. The claim itself is made once there is
	// something to store.
	held, err := c.store.EmbeddingSpace(ctx)
	if err != nil {
		return err
	}
	if held != "" && held != space {
		return fmt.Errorf("%w: the graph's embeddings are in %q, and the embedder's are in %q", storage.ErrOtherSpace, held, space)
	}

	vectors, err := c.embedder.EmbedDocuments(ctx, texts)
	if errors.Is(err, model.ErrDeclined) {
		// As with a document, a refusal would be a refusal next time too.
		// The topics are created without embeddings, and that is logged.
		c.logger.WarnContext(ctx, "a model declined to embed topics, which are stored without", "topics", len(texts), "error", err)
		return nil
	}
	if err != nil {
		return fmt.Errorf("embedding %d topics: %w", len(texts), err)
	}
	if len(vectors) != len(texts) {
		return fmt.Errorf("ingest: the embedder returned %d vectors for %d texts", len(vectors), len(texts))
	}
	if err := c.store.ClaimEmbeddingSpace(ctx, space); err != nil {
		return err
	}
	for i, vector := range vectors {
		*places[i] = vector
	}
	return nil
}

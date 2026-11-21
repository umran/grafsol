package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/mapping"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
)

// --- the record ---------------------------------------------------------------

func TestTheRecordHoldsWhatTheSourceSaid(t *testing.T) {
	h := newHarness(t)
	h.enrich = func(c *source.Change) error {
		c.Document.Summary = "A summary an enricher wrote."
		c.Document.Embedding = []float32{0.5, 0.25}
		c.EmbeddingSpace = "acme/by-hand@1; 2 dimensions"
		return nil
	}

	change := issue("42", 1)
	mention(&change, "7", "Fixes #7.")
	change.Parent(doc("1"), "sub-issue of")
	change.Label("Bug", "Something is not working")
	change.Edges = []graph.Edge{{From: doc("42"), Kind: graph.InTopic, To: "topic:payments", Summary: "filed there by its source"}}
	change.Topics = []graph.Topic{{ID: "topic:payments", Name: "Payments", Summary: "Money in and out.", Embedding: []float32{1, 0}}}
	change.EmbeddingSpace = "acme/by-hand@1; 2 dimensions"
	h.apply(change, ingest.Applied)

	said, onRecord := h.said(doc("42"))[doc("42")]
	if !onRecord {
		t.Fatal("nothing is on record for the document")
	}
	// What the source said, to the letter: nothing on record shows what the
	// rules made of it.
	if !slices.Equal(said.References, change.References) || !slices.Equal(said.Labels, change.Labels) || !slices.Equal(said.Edges, change.Edges) {
		t.Errorf("the record holds\n  %+v\n  %+v\n  %+v\nwant what the change said", said.References, said.Labels, said.Edges)
	}
	// The document as the source and the enricher left it. Embeddings stay
	// in the graph, where a search finds them.
	if said.Document.Title != change.Document.Title || said.Document.Summary != "A summary an enricher wrote." {
		t.Errorf("the document on record is %+v", said.Document)
	}
	if len(said.Document.Embedding) != 0 || len(said.Topics) != 1 || len(said.Topics[0].Embedding) != 0 {
		t.Errorf("the record holds embeddings: the document's %v, and topics %+v", said.Document.Embedding, said.Topics)
	}
	if topic := h.vertex("topic:payments").(*graph.Topic); !slices.Equal(topic.Embedding, []float32{1, 0}) {
		t.Errorf("the topic was stored with the embedding %v, want the one its source gave it", topic.Embedding)
	}

	// A later version says less. What is on record is replaced, not added to.
	later := issue("42", 2)
	h.apply(later, ingest.Applied)
	if said := h.said(doc("42"))[doc("42")]; len(said.References)+len(said.Labels)+len(said.Edges) != 0 {
		t.Errorf("the record still holds what an earlier version said: %+v", said)
	}
}

// --- a change of settings -----------------------------------------------------

// A team writes rules of its own, and merges one topic into another. Every
// document already in the graph has its edges made again, from what is on
// record, and no source or model is asked anything.
func TestCatchUpMakesTheGraphAgainUnderNewSettings(t *testing.T) {
	h := newHarness(t)
	h.enrich = func(c *source.Change) error {
		c.Document.Summary = "summary of " + c.Document.Title
		return nil
	}
	composer := h.syncer.Composer()

	pullRequest := issue("43", 1)
	mention(&pullRequest, "9", "implements #9, #16 and #17")
	mention(&pullRequest, "16", "implements #9, #16 and #17")
	mention(&pullRequest, "17", "implements #9, #16 and #17")
	mention(&pullRequest, "30", "As described in issue #30, the worker hangs.")
	pullRequest.Label("docs", "")
	pullRequest.Label("dependencies", "")
	untouched := issue("44", 1)
	mention(&untouched, "30", "As described in issue #30, the worker hangs.")
	for _, change := range []source.Change{pullRequest, untouched} {
		h.apply(change, ingest.Applied)
	}
	implements := "implements #9, #16 and #17"
	h.requireOutEdges(doc("43"),
		graph.Edge{From: doc("43"), Kind: graph.LinksTo, To: doc("9"), Summary: implements},
		graph.Edge{From: doc("43"), Kind: graph.LinksTo, To: doc("16"), Summary: implements},
		graph.Edge{From: doc("43"), Kind: graph.LinksTo, To: doc("17"), Summary: implements},
		graph.Edge{From: doc("43"), Kind: graph.LinksTo, To: doc("30"), Summary: "As described in issue #30, the worker hangs."},
		graph.Edge{From: doc("43"), Kind: graph.InTopic, To: "topic:docs"},
		graph.Edge{From: doc("43"), Kind: graph.InTopic, To: "topic:dependencies"},
	)

	// The graph was made under the settings it begins with. Nothing to do.
	if changed := catchUp(t, composer); changed != 0 {
		t.Fatalf("with the settings as they were, CatchUp made %d documents again", changed)
	}

	saveSettings(t, h.store, func(s *mapping.Settings) {
		s.Rules = mapping.Rules{
			{ID: "implements", Source: "github", Form: graph.FormMention, After: []string{"implements", "addresses"}, Kind: mapping.Resolves},
			{ID: "bots", Form: mapping.FormLabel, Label: "dependencies", Ignore: true},
		}
		s.Aliases = map[graph.ID]graph.ID{"topic:docs": "topic:documentation"}
	})
	// Saving changes nothing by itself.
	h.requireOutEdges(doc("44"), graph.Edge{From: doc("44"), Kind: graph.LinksTo, To: doc("30"), Summary: "As described in issue #30, the worker hangs."})

	// One of the two documents comes out different.
	if changed := catchUp(t, composer); changed != 1 {
		t.Errorf("CatchUp reported %d documents changed, want 1", changed)
	}
	h.requireOutEdges(doc("43"),
		graph.Edge{From: doc("43"), Kind: mapping.Resolves, To: doc("9"), Summary: implements},
		graph.Edge{From: doc("43"), Kind: mapping.Resolves, To: doc("16"), Summary: implements},
		graph.Edge{From: doc("43"), Kind: mapping.Resolves, To: doc("17"), Summary: implements},
		graph.Edge{From: doc("43"), Kind: graph.LinksTo, To: doc("30"), Summary: "As described in issue #30, the worker hangs."},
		graph.Edge{From: doc("43"), Kind: graph.InTopic, To: "topic:documentation"},
	)
	h.requireOutEdges(doc("44"), graph.Edge{From: doc("44"), Kind: graph.LinksTo, To: doc("30"), Summary: "As described in issue #30, the worker hangs."})

	// The topic that took the other's place exists now. The one merged away
	// is not deleted: what was written about it may be wanted again.
	if topic := h.vertex("topic:documentation").(*graph.Topic); topic.Name != "documentation" {
		t.Errorf("the topic merged into was created as %+v", topic)
	}
	// Nothing was enriched again, and the documents themselves are as they
	// were.
	if h.enriched.Load() != 2 {
		t.Errorf("the enricher has run %d times, want only the 2 for the first applying", h.enriched.Load())
	}
	if document := h.vertex(doc("43")).(*graph.Document); document.Summary != "summary of issue 43 at version 1" {
		t.Errorf("composing again changed the document: %+v", document)
	}

	// Caught up, there is nothing more to do: not for this Composer, and not
	// for another that starts later over the same graph.
	if changed := catchUp(t, composer); changed != 0 {
		t.Errorf("a second CatchUp made %d documents again", changed)
	}
	later := ingest.NewComposer(ingest.Config{Store: h.store, Ledger: h.ledger})
	if changed := catchUp(t, later); changed != 0 {
		t.Errorf("a Composer started later made %d documents again", changed)
	}

	// A document applied from now on is made under the new settings.
	next := issue("45", 1)
	mention(&next, "9", "addresses #9")
	next.Label("docs", "")
	h.apply(next, ingest.Applied)
	h.requireOutEdges(doc("45"),
		graph.Edge{From: doc("45"), Kind: mapping.Resolves, To: doc("9"), Summary: "addresses #9"},
		graph.Edge{From: doc("45"), Kind: graph.InTopic, To: "topic:documentation"},
	)
}

// CatchUp lists the documents on record a page at a time. Every one of them
// has to be reached, however many pages that takes.
func TestCatchUpReachesEveryDocument(t *testing.T) {
	h := newHarness(t)
	h.store.PageSize = 3
	const documents = 20
	for n := range documents {
		change := issue(fmt.Sprint(n+1), 1)
		change.Label("bug", "")
		h.apply(change, ingest.Applied)
	}

	saveSettings(t, h.store, func(s *mapping.Settings) {
		s.Rules = mapping.Rules{{ID: "bugs", Form: mapping.FormLabel, Label: "bug", Topic: "Defects"}}
	})
	if changed := catchUp(t, h.syncer.Composer()); changed != documents {
		t.Errorf("CatchUp reported %d documents changed, want all %d", changed, documents)
	}
	for n := range documents {
		id := doc(fmt.Sprint(n + 1))
		h.requireOutEdges(id, graph.Edge{From: id, Kind: graph.InTopic, To: "topic:defects"})
	}
}

// The rules that ship with grafsol are part of what a graph is made by. When
// a later version ships others, the graph made under the earlier ones is
// made again, though no team has saved a thing.
func TestCatchUpMakesTheGraphAgainUnderALaterEdition(t *testing.T) {
	stray := graph.Edge{From: doc("1"), Kind: "made_by_an_earlier_edition", To: doc("2")}
	fill := func(t *testing.T) *harness {
		h := newHarness(t)
		h.apply(issue("1", 1), ingest.Applied)
		if changed := catchUp(t, h.syncer.Composer()); changed != 0 {
			t.Fatalf("the first CatchUp made %d documents again", changed)
		}
		// An edge that nothing on record gives rise to, as an earlier
		// edition's rules might have left.
		if err := h.store.PutEdges(t.Context(), []graph.Edge{stray}); err != nil {
			t.Fatalf("PutEdges: %v", err)
		}
		return h
	}

	t.Run("the same edition", func(t *testing.T) {
		h := fill(t)
		if changed := catchUp(t, h.syncer.Composer()); changed != 0 {
			t.Errorf("CatchUp made %d documents again though nothing it goes by has changed", changed)
		}
		h.requireOutEdges(doc("1"), stray)
	})

	t.Run("an earlier edition", func(t *testing.T) {
		h := fill(t)
		// The note of what the graph was composed under, as a version of
		// grafsol with other rules of its own would have left it.
		earlier := fmt.Appendf(nil, `{"settings":0,"edition":%d}`, mapping.Edition-1)
		if err := h.ledger.SaveCheckpoint(t.Context(), "composed", earlier); err != nil {
			t.Fatalf("SaveCheckpoint: %v", err)
		}
		if changed := catchUp(t, h.syncer.Composer()); changed != 1 {
			t.Errorf("CatchUp reported %d documents changed, want the 1 with an edge of the earlier edition", changed)
		}
		h.requireOutEdges(doc("1"))
	})

	t.Run("a note that cannot be read", func(t *testing.T) {
		h := fill(t)
		if err := h.ledger.SaveCheckpoint(t.Context(), "composed", []byte("not a note")); err != nil {
			t.Fatalf("SaveCheckpoint: %v", err)
		}
		if _, err := h.syncer.Composer().CatchUp(t.Context()); err == nil {
			t.Errorf("CatchUp read a note that is not one")
		}
	})
}

func TestComposeLeavesAloneWhatHasNothingOnRecord(t *testing.T) {
	h := newHarness(t)
	// A document put in the graph by something other than a Syncer.
	byHand := graph.Edge{From: doc("1"), Kind: graph.LinksTo, To: doc("2"), Summary: "put there by hand"}
	if err := h.store.PutVertices(t.Context(), []graph.Vertex{&graph.Document{ID: doc("1"), Title: "By hand"}}); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}
	if err := h.store.PutEdges(t.Context(), []graph.Edge{byHand}); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	for _, id := range []graph.ID{doc("1"), doc("never-heard-of")} {
		changed, err := h.syncer.Composer().Compose(t.Context(), id)
		if err != nil || changed {
			t.Errorf("Compose(%s) = %v, %v; want nothing done", id, changed, err)
		}
	}
	h.requireOutEdges(doc("1"), byHand)
}

func TestComposeMakesOneDocumentAgain(t *testing.T) {
	h := newHarness(t)
	for _, number := range []string{"1", "2"} {
		change := issue(number, 1)
		change.Label("bug", "")
		h.apply(change, ingest.Applied)
	}
	saveSettings(t, h.store, func(s *mapping.Settings) {
		s.Rules = mapping.Rules{{ID: "bugs", Form: mapping.FormLabel, Label: "bug", Ignore: true}}
	})

	changed, err := h.syncer.Composer().Compose(t.Context(), doc("1"))
	if err != nil || !changed {
		t.Fatalf("Compose = %v, %v; want the document changed", changed, err)
	}
	h.requireOutEdges(doc("1"))
	h.requireOutEdges(doc("2"), graph.Edge{From: doc("2"), Kind: graph.InTopic, To: "topic:bug"})

	// Composing again finds the document as it should be.
	if changed, err := h.syncer.Composer().Compose(t.Context(), doc("1")); err != nil || changed {
		t.Errorf("composing again = %v, %v; want nothing changed", changed, err)
	}
}

// What is on record need not have come through a Syncer. Whatever put it
// there, a document that points at itself says nothing.
func TestComposeMakesNothingOfADocumentThatPointsAtItself(t *testing.T) {
	h := newHarness(t)
	h.apply(issue("42", 1), ingest.Applied)

	said := h.said(doc("42"))[doc("42")]
	said.Edges = []graph.Edge{
		{From: doc("42"), Kind: "supersedes", To: doc("42"), Summary: "itself"},
		{From: doc("42"), Kind: "supersedes", To: doc("3"), Summary: "another"},
	}
	if err := h.store.PutSaid(t.Context(), said); err != nil {
		t.Fatalf("PutSaid: %v", err)
	}
	changed, err := h.syncer.Composer().Compose(t.Context(), doc("42"))
	if err != nil || !changed {
		t.Fatalf("Compose = %v, %v; want the document changed", changed, err)
	}
	h.requireOutEdges(doc("42"), graph.Edge{From: doc("42"), Kind: "supersedes", To: doc("3"), Summary: "another"})
}

// A topic that an edge points at has to exist. If it has gone missing,
// composing the document puts it back, and says that something changed.
func TestComposingAgainRestoresATopicThatHasGoneMissing(t *testing.T) {
	h := newHarness(t)
	change := issue("42", 1)
	change.Label("Bug", "Something is not working")
	h.apply(change, ingest.Applied)
	if err := h.store.DeleteVertices(t.Context(), []graph.ID{"topic:bug"}); err != nil {
		t.Fatalf("DeleteVertices: %v", err)
	}

	changed, err := h.syncer.Composer().Compose(t.Context(), doc("42"))
	if err != nil || !changed {
		t.Fatalf("Compose = %v, %v; want it to report the topic it put back", changed, err)
	}
	if topic := h.vertex("topic:bug").(*graph.Topic); topic.Name != "Bug" || topic.Summary != "Something is not working" {
		t.Errorf("the topic was put back as %+v", topic)
	}
}

func TestSettingsThatCannotBeReadStopAChangeAndLeaveNothingBehind(t *testing.T) {
	h := newHarness(t)
	if _, err := h.store.SaveSettings(t.Context(), []byte("{ not settings"), 0); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	change := issue("42", 1)
	mention(&change, "7", "see #7")
	if _, err := h.syncer.Apply(t.Context(), change); err == nil {
		t.Fatal("a change was applied under settings that cannot be read")
	}
	if len(h.vertices(doc("42"))) != 0 || len(h.said(doc("42"))) != 0 {
		t.Errorf("the change left something behind")
	}
	h.requireOutEdges(doc("42"))
	if _, err := h.syncer.Composer().CatchUp(t.Context()); err != nil {
		// With nothing on record there is nothing to compose, and so
		// nothing to read the settings for.
		t.Errorf("CatchUp of a graph with nothing on record: %v", err)
	}
}

// --- documents from before records were kept ----------------------------------

// leftByTheFirstRelease applies a change, and then makes its document look
// the way the first release of grafsol left one: a bare fingerprint in the
// ledger, and the edges that release made of what the source said. It
// returns the change.
func leftByTheFirstRelease(t *testing.T, h *harness) source.Change {
	t.Helper()
	h.enrich = func(c *source.Change) error {
		c.Document.Summary = "A summary written when the document was first applied."
		c.Document.Embedding = []float32{0.5, 0.25}
		c.EmbeddingSpace = "acme/by-hand@1; 2 dimensions"
		return nil
	}
	change := issue("42", 1)
	mention(&change, "7", "See #7.")
	mention(&change, "7", "Fixes #7.")
	change.Label("Bug", "Something is not working")
	h.apply(change, ingest.Applied)

	ctx := t.Context()
	mark, _, err := h.ledger.Mark(ctx, doc("42"))
	if err != nil {
		t.Fatalf("Mark: %v", err)
	}
	mark.Fingerprint = hashOf(mark)
	if err := h.ledger.Record(ctx, mark); err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Of two mentions of one document, that release kept the first line.
	page, err := h.store.Edges(ctx, storage.EdgeQuery{Vertex: doc("42"), Direction: graph.Out})
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if err := h.store.DeleteEdges(ctx, page.Edges); err != nil {
		t.Fatalf("DeleteEdges: %v", err)
	}
	if err := h.store.PutEdges(ctx, []graph.Edge{
		{From: doc("42"), Kind: graph.LinksTo, To: doc("7"), Summary: "See #7."},
		{From: doc("42"), Kind: graph.InTopic, To: "topic:bug"},
	}); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	return change
}

func TestADocumentFromBeforeRecordsIsAdoptedWhenNextSeen(t *testing.T) {
	whole := []graph.Reference{
		{Target: doc("7"), Form: graph.FormMention, Written: "#7", Context: "See #7."},
		{Target: doc("7"), Form: graph.FormMention, Written: "#7", Context: "Fixes #7."},
	}
	adopted := func(t *testing.T, h *harness, change source.Change) {
		t.Helper()
		// The same state arrives again: by a scan, say.
		h.apply(change, ingest.Unchanged)

		// Nothing was summarised or embedded again, and the stored document
		// is untouched.
		if h.enriched.Load() != 1 {
			t.Errorf("the enricher ran %d times, want only the once", h.enriched.Load())
		}
		document := h.vertex(doc("42")).(*graph.Document)
		if document.Summary != "A summary written when the document was first applied." || !slices.Equal(document.Embedding, []float32{0.5, 0.25}) {
			t.Errorf("adopting the document changed it: %+v", document)
		}

		// The record is whole now, summary included, and the edges are made
		// from it: the second mention, which nothing kept before, is there.
		said := h.said(doc("42"))[doc("42")]
		if !slices.Equal(said.References, whole) || !slices.Equal(said.Labels, change.Labels) {
			t.Errorf("the record holds\n  %+v\n  %+v\nwant everything the change says", said.References, said.Labels)
		}
		if said.Document.Summary != document.Summary || said.Document.Title != change.Document.Title {
			t.Errorf("the document on record is %+v", said.Document)
		}
		h.requireOutEdges(doc("42"),
			graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: doc("7"), Summary: "See #7."},
			graph.Edge{From: doc("42"), Kind: mapping.Resolves, To: doc("7"), Summary: "Fixes #7."},
			graph.Edge{From: doc("42"), Kind: graph.InTopic, To: "topic:bug"},
		)

		// Once is enough. The next sighting finds a document like any other
		// and does not go to the record at all.
		if err := h.store.DeleteSaid(t.Context(), doc("42")); err != nil {
			t.Fatalf("DeleteSaid: %v", err)
		}
		h.apply(change, ingest.Unchanged)
		if said := h.said(doc("42")); len(said) != 0 {
			t.Errorf("the document was adopted a second time")
		}
	}

	t.Run("with what an upgrade worked out from its edges", func(t *testing.T) {
		h := newHarness(t)
		change := leftByTheFirstRelease(t, h)
		// What the upgrade of a database writes: one mention for the one
		// edge there was, in no words, and the document as it was stored.
		carried := storage.Said{
			Document:   *h.vertex(doc("42")).(*graph.Document),
			References: []graph.Reference{{Target: doc("7"), Form: graph.FormMention, Context: "See #7."}},
			Labels:     []graph.Label{{Name: "Bug", Description: "Something is not working"}},
		}
		carried.Document.Embedding = nil
		if err := h.store.PutSaid(t.Context(), carried); err != nil {
			t.Fatalf("PutSaid: %v", err)
		}
		adopted(t, h, change)
	})

	t.Run("with nothing on record", func(t *testing.T) {
		h := newHarness(t)
		change := leftByTheFirstRelease(t, h)
		if err := h.store.DeleteSaid(t.Context(), doc("42")); err != nil {
			t.Fatalf("DeleteSaid: %v", err)
		}
		adopted(t, h, change)
	})

	t.Run("with nothing in the graph either", func(t *testing.T) {
		h := newHarness(t)
		change := leftByTheFirstRelease(t, h)
		ctx := t.Context()
		if err := h.store.DeleteSaid(ctx, doc("42")); err != nil {
			t.Fatalf("DeleteSaid: %v", err)
		}
		if err := h.store.DeleteVertices(ctx, []graph.ID{doc("42")}); err != nil {
			t.Fatalf("DeleteVertices: %v", err)
		}

		// There is no summary to be had, so nothing is put on record: a
		// record without one would be taken for the whole of it later.
		h.apply(change, ingest.Unchanged)
		if said := h.said(doc("42")); len(said) != 0 {
			t.Errorf("a record was written without the document's summary: %+v", said)
		}
		mark, _, _ := h.ledger.Mark(ctx, doc("42"))
		if mark.Fingerprint != hashOf(mark) {
			t.Errorf("the mark says a record was kept, and none was")
		}
	})

	t.Run("a change that is not the state applied adopts nothing", func(t *testing.T) {
		h := newHarness(t)
		leftByTheFirstRelease(t, h)
		if err := h.store.DeleteSaid(t.Context(), doc("42")); err != nil {
			t.Fatalf("DeleteSaid: %v", err)
		}
		// Older news of the document says nothing of what is in the graph.
		older := issue("42", 0)
		mention(&older, "8", "See #8.")
		h.apply(older, ingest.Stale)
		if said := h.said(doc("42")); len(said) != 0 {
			t.Errorf("a stale change was put on record: %+v", said)
		}
	})
}

// A document applied by this version has its record from the start, and its
// mark says so. It is not taken, the next time it is seen, for one that
// still needs a record written.
func TestADocumentThatHasItsRecordIsNotAdopted(t *testing.T) {
	h := newHarness(t)
	change := issue("42", 1)
	mention(&change, "7", "see #7")
	h.apply(change, ingest.Applied)

	// Were the document adopted on its next sighting, its record would be
	// written again.
	if err := h.store.DeleteSaid(t.Context(), doc("42")); err != nil {
		t.Fatalf("DeleteSaid: %v", err)
	}
	h.apply(change, ingest.Unchanged)
	if said := h.said(doc("42")); len(said) != 0 {
		t.Errorf("a document with a record of its own was adopted when next seen")
	}
}

// --- the topics that rules make -----------------------------------------------

// A source goes on describing a topic that a team has merged into another.
// The document lands in the other, and the one merged away is not made
// again.
func TestATopicThatWasMergedAwayIsNotCreatedAgain(t *testing.T) {
	h := newHarness(t)
	saveSettings(t, h.store, func(s *mapping.Settings) {
		s.Aliases = map[graph.ID]graph.ID{"topic:docs": "topic:documentation"}
	})

	change := issue("42", 1)
	stateTopic(&change, "docs", "Where the documentation lives.")
	change.Label("Docs", "The same topic, as a label")
	h.apply(change, ingest.Applied)

	h.requireOutEdges(doc("42"), graph.Edge{From: doc("42"), Kind: graph.InTopic, To: "topic:documentation"})
	if merged := h.vertices("topic:docs"); len(merged) != 0 {
		t.Errorf("the topic that was merged away was created again: %+v", merged["topic:docs"])
	}
	h.vertex("topic:documentation")
}

func TestATopicMadeOfALabelIsEmbeddedWhenItIsCreated(t *testing.T) {
	labelled := func(number string) source.Change {
		change := issue(number, 1)
		change.Label("Billing", "Invoices, refunds and payment providers.")
		return change
	}
	const text = "Billing\n\nInvoices, refunds and payment providers."
	syncerWith := func(embedder model.Embedder) (*memory.Store, *memory.Ledger, *ingest.Syncer) {
		store, ledger := memory.New(), memory.NewLedger()
		return store, ledger, ingest.NewSyncer(ingest.Config{Store: store, Ledger: ledger, Embedder: embedder})
	}
	topicOf := func(t *testing.T, store *memory.Store) *graph.Topic {
		t.Helper()
		stored, err := store.GetVertices(t.Context(), []graph.ID{"topic:billing"})
		if err != nil || stored["topic:billing"] == nil {
			t.Fatalf("the topic is not in the graph (error %v)", err)
		}
		return stored["topic:billing"].(*graph.Topic)
	}

	t.Run("with the Syncer's embedder", func(t *testing.T) {
		embedder := &fakeEmbedder{}
		store, _, syncer := syncerWith(embedder)
		for _, number := range []string{"1", "2"} {
			if _, err := syncer.Apply(t.Context(), labelled(number)); err != nil {
				t.Fatalf("Apply: %v", err)
			}
		}
		// Asked once, when the first document made the topic.
		if len(embedder.calls) != 1 || !slices.Equal(embedder.calls[0], []string{text}) {
			t.Errorf("the embedder was called with %q, want once with the topic", embedder.calls)
		}
		if topic := topicOf(t, store); !slices.Equal(topic.Embedding, vectorOf(text)) {
			t.Errorf("the topic was stored with the embedding %v", topic.Embedding)
		}
		if space, _ := store.EmbeddingSpace(t.Context()); space != testSpace {
			t.Errorf("the graph is in the space %q, want the embedder's", space)
		}
	})

	t.Run("without an embedder", func(t *testing.T) {
		store, _, syncer := syncerWith(nil)
		if _, err := syncer.Apply(t.Context(), labelled("1")); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if topic := topicOf(t, store); len(topic.Embedding) != 0 {
			t.Errorf("the topic has the embedding %v, and nothing could have made one", topic.Embedding)
		}
		if space, _ := store.EmbeddingSpace(t.Context()); space != "" {
			t.Errorf("a graph without a single embedding is in the space %q", space)
		}
	})

	t.Run("when the settings change", func(t *testing.T) {
		embedder := &fakeEmbedder{}
		store, _, syncer := syncerWith(embedder)
		if _, err := syncer.Apply(t.Context(), labelled("1")); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		saveSettings(t, store, func(s *mapping.Settings) {
			s.Rules = mapping.Rules{{ID: "money", Form: mapping.FormLabel, Label: "billing", Topic: "Money"}}
		})
		if changed := catchUp(t, syncer.Composer()); changed != 1 {
			t.Fatalf("CatchUp reported %d documents changed, want 1", changed)
		}
		// The topic the new rule names is created, and embedded, there and
		// then.
		stored, _ := store.GetVertices(t.Context(), []graph.ID{"topic:money"})
		if topic, _ := stored["topic:money"].(*graph.Topic); topic == nil || !slices.Equal(topic.Embedding, vectorOf("Money")) {
			t.Errorf("the topic a new rule files documents under is stored as %+v", stored["topic:money"])
		}
	})

	t.Run("a model that declines", func(t *testing.T) {
		store, _, syncer := syncerWith(failingEmbedder{fmt.Errorf("embedding: %w", model.ErrDeclined)})
		if _, err := syncer.Apply(t.Context(), labelled("1")); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		// As with a document, the topic is stored without.
		if topic := topicOf(t, store); len(topic.Embedding) != 0 {
			t.Errorf("the topic has the embedding %v", topic.Embedding)
		}
	})

	for name, embedder := range map[string]model.Embedder{
		"a model that fails":             failingEmbedder{errors.New("the embeddings service is down")},
		"a model that miscounts":         &fakeEmbedder{drop: 1},
		"a model with no word of itself": &fakeEmbedder{lost: errors.New("the embeddings service is not there")},
	} {
		t.Run(name, func(t *testing.T) {
			store, ledger, syncer := syncerWith(embedder)
			change := labelled("1")
			mention(&change, "7", "see #7")
			if _, err := syncer.Apply(t.Context(), change); err == nil {
				t.Fatal("the change was applied")
			}
			// Nothing of the change is left behind, so it is tried again in
			// full once the model is back.
			stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("1"), "topic:billing"})
			said, _ := store.Said(t.Context(), []graph.ID{doc("1")})
			edges, _ := store.Edges(t.Context(), storage.EdgeQuery{Vertex: doc("1"), Direction: graph.Out})
			_, marked, _ := ledger.Mark(t.Context(), doc("1"))
			if len(stored)+len(said)+len(edges.Edges) != 0 || marked {
				t.Errorf("the change left behind %d vertices, %d records, %d edges and a mark: %v", len(stored), len(said), len(edges.Edges), marked)
			}
		})
	}

	t.Run("an embedder of another space than the graph's", func(t *testing.T) {
		store, ledger := memory.New(), memory.NewLedger()
		if err := store.ClaimEmbeddingSpace(t.Context(), otherSpace); err != nil {
			t.Fatalf("ClaimEmbeddingSpace: %v", err)
		}
		embedder := &fakeEmbedder{}
		syncer := ingest.NewSyncer(ingest.Config{Store: store, Ledger: ledger, Embedder: embedder})
		_, err := syncer.Apply(t.Context(), labelled("1"))
		if !errors.Is(err, storage.ErrOtherSpace) || !strings.Contains(err.Error(), testSpace) || !strings.Contains(err.Error(), otherSpace) {
			t.Errorf("Apply returned %v, want an error that wraps ErrOtherSpace and names both spaces", err)
		}
		// The model was not put to work for vectors the graph cannot take.
		if len(embedder.calls) != 0 {
			t.Errorf("the embedder was asked to embed %q all the same", embedder.calls)
		}
	})
}

// failingEmbedder knows its space and can embed nothing.
type failingEmbedder struct{ failure error }

func (failingEmbedder) Space(context.Context) (string, error) { return testSpace, nil }

func (f failingEmbedder) EmbedDocuments(context.Context, []string) ([][]float32, error) {
	return nil, f.failure
}

func (f failingEmbedder) EmbedQueries(context.Context, []string) ([][]float32, error) {
	return nil, f.failure
}

// --- what a Syncer has to be given ---------------------------------------------

// bareStore is a Store that keeps the graph and nothing that goes with it.
type bareStore struct{ storage.Store }

func TestASyncerNeedsSomewhereToKeepTheRecordAndTheSettings(t *testing.T) {
	whole := memory.New()
	needs := func(what string, cfg ingest.Config) {
		t.Helper()
		defer func() {
			if failure := fmt.Sprint(recover()); !strings.Contains(failure, what) {
				t.Errorf("NewSyncer said %q, want it to say that it needs %s", failure, what)
			}
		}()
		ingest.NewSyncer(cfg)
	}
	needs("a Store", ingest.Config{Ledger: memory.NewLedger()})
	needs("a Ledger", ingest.Config{Store: whole})
	needs("a Record", ingest.Config{Store: bareStore{whole}, Ledger: memory.NewLedger(), Settings: whole})
	needs("Settings", ingest.Config{Store: bareStore{whole}, Ledger: memory.NewLedger(), Record: whole})

	// Told where both are kept, a Syncer works over a Store that keeps
	// neither.
	syncer := ingest.NewSyncer(ingest.Config{Store: bareStore{whole}, Ledger: memory.NewLedger(), Record: whole, Settings: whole})
	change := issue("42", 1)
	change.Label("bug", "")
	if _, err := syncer.Apply(t.Context(), change); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if said, _ := whole.Said(t.Context(), []graph.ID{doc("42")}); len(said) != 1 {
		t.Errorf("the change was not put on the record the Syncer was given")
	}
}

// --- helpers ------------------------------------------------------------------

// saveSettings changes a graph's settings and saves them in place of what
// was stored.
func saveSettings(t *testing.T, store storage.Settings, change func(*mapping.Settings)) {
	t.Helper()
	document, version, err := store.Settings(t.Context())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	settings, err := mapping.DecodeSettings(document)
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	change(&settings)
	encoded, err := settings.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := store.SaveSettings(t.Context(), encoded, version); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
}

func catchUp(t *testing.T, composer *ingest.Composer) int {
	t.Helper()
	changed, err := composer.CatchUp(t.Context())
	if err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	return changed
}

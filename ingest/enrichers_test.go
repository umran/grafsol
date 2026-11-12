package ingest_test

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
)

func TestSummarizeAndEmbedFillInADocumentAndItsTopics(t *testing.T) {
	summarizer, embedder := &fakeSummarizer{}, &fakeEmbedder{}
	store, syncer := enrichingSyncer(t, nil, ingest.Summarize(summarizer), ingest.Embed(embedder))

	change := issue("42", 1)
	change.Tag("Billing", "Invoices, refunds and payment providers.")
	change.Tag("Urgent", "")
	if _, err := syncer.Apply(t.Context(), change); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The summarizer was given the title and the text, as they came.
	if got := summarizer.asked; len(got) != 1 || got[0] != "issue 42 at version 1|body of issue 42 at version 1" {
		t.Errorf("the summarizer was asked for %q", got)
	}
	// The embedder was called once, for the document and both topics, and
	// given what the graph keeps of each: title and summary, name and
	// summary. The summary it embedded is the one just written.
	want := []string{
		"issue 42 at version 1\n\nsummary of issue 42 at version 1",
		"Billing\n\nInvoices, refunds and payment providers.",
		"Urgent",
	}
	if len(embedder.calls) != 1 || !slices.Equal(embedder.calls[0], want) {
		t.Errorf("the embedder was called with\n  %q\nwant one call with\n  %q", embedder.calls, want)
	}

	stored, err := store.GetVertices(t.Context(), []graph.ID{doc("42"), graph.TopicID("Billing"), graph.TopicID("Urgent")})
	if err != nil {
		t.Fatalf("GetVertices: %v", err)
	}
	document := stored[doc("42")].(*graph.Document)
	if document.Summary != "summary of issue 42 at version 1" || !slices.Equal(document.Embedding, vectorOf(want[0])) {
		t.Errorf("the document was stored with summary %q and embedding %v", document.Summary, document.Embedding)
	}
	for i, id := range []graph.ID{graph.TopicID("Billing"), graph.TopicID("Urgent")} {
		if topic := stored[id].(*graph.Topic); !slices.Equal(topic.Embedding, vectorOf(want[i+1])) {
			t.Errorf("topic %s was stored with embedding %v", id, topic.Embedding)
		}
	}
}

func TestSummarizeLeavesAloneWhatNeedsNoSummary(t *testing.T) {
	summarizer := &fakeSummarizer{}
	store, syncer := enrichingSyncer(t, nil, ingest.Summarize(summarizer))

	// The source wrote a summary of its own.
	supplied := issue("1", 1)
	supplied.Document.Summary = "Written by the source."
	// There is no text to summarise.
	empty := issue("2", 1)
	empty.Content = "  \n"
	// The text says nothing that the title does not: a connector puts the
	// title at the head of the text, so this is what a pull request with no
	// description looks like.
	titleOnly := issue("3", 1)
	titleOnly.Content = "  " + titleOnly.Document.Title + " \n\n"
	for _, change := range []source.Change{supplied, empty, titleOnly} {
		if _, err := syncer.Apply(t.Context(), change); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	if len(summarizer.asked) != 0 {
		t.Errorf("the summarizer was asked for %q", summarizer.asked)
	}
	stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("1"), doc("2")})
	if got := stored[doc("1")].(*graph.Document).Summary; got != "Written by the source." {
		t.Errorf("the source's summary was replaced by %q", got)
	}
	if got := stored[doc("2")].(*graph.Document).Summary; got != "" {
		t.Errorf("a document with no text was given the summary %q", got)
	}

	// A document that says anything more than its title is summarised,
	// from the whole of its text.
	more := issue("4", 1)
	more.Content = more.Document.Title + "\n\nFixes #38"
	if _, err := syncer.Apply(t.Context(), more); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(summarizer.asked) != 1 || summarizer.asked[0] != more.Document.Title+"|"+more.Content {
		t.Errorf("the summarizer was asked for %q, want the one document that says more than its title", summarizer.asked)
	}
}

func TestEmbedLeavesAloneWhatIsAlreadyEmbedded(t *testing.T) {
	embedder := &fakeEmbedder{}
	store, syncer := enrichingSyncer(t, nil, ingest.Embed(embedder))

	change := issue("1", 1)
	change.Document.Embedding = []float32{9, 9, 9}
	change.Topics = []graph.Topic{{ID: graph.TopicID("billing"), Name: "Billing", Embedding: []float32{8, 8, 8}}}
	change.EmbeddingSpace = testSpace
	change.Edges = []graph.Edge{{From: doc("1"), Kind: graph.TaggedWith, To: graph.TopicID("billing")}}
	if _, err := syncer.Apply(t.Context(), change); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(embedder.calls) != 0 {
		t.Errorf("the embedder was called with %q although nothing lacked an embedding", embedder.calls)
	}
	stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("1")})
	if got := stored[doc("1")].(*graph.Document).Embedding; !slices.Equal(got, []float32{9, 9, 9}) {
		t.Errorf("the source's embedding was replaced by %v", got)
	}
}

func TestADocumentAModelDeclinesIsStoredWithoutAndTheRefusalLogged(t *testing.T) {
	var log bytes.Buffer
	summarizer := &fakeSummarizer{fail: fmt.Errorf("summarising: %w: the service refused the text (unfinished)", model.ErrDeclined)}
	embedder := &fakeEmbedder{}
	store, syncer := enrichingSyncer(t, &log, ingest.Summarize(summarizer), ingest.Embed(embedder))

	outcome, err := syncer.Apply(t.Context(), issue("42", 1))
	if err != nil || outcome != ingest.Applied {
		t.Fatalf("Apply was %v, %v; want the change applied in spite of the refusal", outcome, err)
	}

	// Stored, with no summary, and embedded from its title alone: the
	// enrichers after the refusal still ran.
	stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("42")})
	document, ok := stored[doc("42")].(*graph.Document)
	if !ok {
		t.Fatalf("the document was not stored")
	}
	if document.Summary != "" || !slices.Equal(document.Embedding, vectorOf("issue 42 at version 1")) {
		t.Errorf("the document was stored with summary %q and embedding %v", document.Summary, document.Embedding)
	}
	if logged := log.String(); !strings.Contains(logged, "declined") || !strings.Contains(logged, string(doc("42"))) {
		t.Errorf("the refusal was not logged with the document it concerns:\n%s", logged)
	}

	// The same change again is not a new attempt: the model would only
	// refuse again.
	if outcome, err := syncer.Apply(t.Context(), issue("42", 1)); err != nil || outcome != ingest.Unchanged {
		t.Errorf("applying the change again was %v, %v", outcome, err)
	}
	if len(summarizer.asked) != 1 {
		t.Errorf("the summarizer was asked %d times", len(summarizer.asked))
	}
}

func TestAModelThatFailsStopsTheChange(t *testing.T) {
	outage := errors.New("529 overloaded")
	summarizer := &fakeSummarizer{fail: outage}
	store, syncer := enrichingSyncer(t, nil, ingest.Summarize(summarizer), ingest.Embed(&fakeEmbedder{}))

	// A failure that may pass is not a refusal: the change must fail, so
	// that it is tried again, and nothing of it may be stored meanwhile.
	if _, err := syncer.Apply(t.Context(), issue("42", 1)); !errors.Is(err, outage) {
		t.Fatalf("Apply returned %v, want the summarizer's error", err)
	}
	if stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("42")}); len(stored) != 0 {
		t.Errorf("the document was stored although it could not be summarised")
	}

	summarizer.fail = nil
	if outcome, err := syncer.Apply(t.Context(), issue("42", 1)); err != nil || outcome != ingest.Applied {
		t.Errorf("once the summarizer recovered, Apply was %v, %v", outcome, err)
	}
}

func TestEmbedRefusesAnEmbedderThatMiscounts(t *testing.T) {
	_, syncer := enrichingSyncer(t, nil, ingest.Embed(&fakeEmbedder{drop: 1}))

	change := issue("42", 1)
	change.Tag("Billing", "")
	if _, err := syncer.Apply(t.Context(), change); err == nil {
		t.Errorf("one vector for two texts was accepted")
	}
}

// --- helpers ----------------------------------------------------------------

// enrichingSyncer returns a Syncer over in-memory storage that runs the
// given enrichers, with the store it writes to. Its log goes to log, if
// there is one.
func enrichingSyncer(t *testing.T, log *bytes.Buffer, enrichers ...ingest.Enricher) (*memory.Store, *ingest.Syncer) {
	t.Helper()
	logger := slog.New(slog.DiscardHandler)
	if log != nil {
		logger = slog.New(slog.NewTextHandler(log, nil))
	}
	store := memory.New()
	return store, ingest.NewSyncer(ingest.Config{
		Store:     store,
		Ledger:    memory.NewLedger(),
		Enrichers: enrichers,
		Logger:    logger,
	})
}

// fakeSummarizer summarises a document as "summary of <title>", or fails.
type fakeSummarizer struct {
	asked []string // "title|text" of every document it was asked about
	fail  error
}

func (f *fakeSummarizer) Summarize(_ context.Context, title, text string) (string, error) {
	f.asked = append(f.asked, title+"|"+text)
	if f.fail != nil {
		return "", f.fail
	}
	return "summary of " + title, nil
}

// fakeEmbedder embeds a text as vectorOf says.
type fakeEmbedder struct {
	calls [][]string // the texts of every call
	drop  int        // how many vectors to leave off each reply
	space string     // the embedding space it says it is in; "" means testSpace
	lost  error      // if set, what asking for its space fails with
}

// testSpace is the embedding space of a fakeEmbedder that names no other.
const testSpace = "acme/test-embedder@1; 3 dimensions"

func (f *fakeEmbedder) Space(context.Context) (string, error) {
	if f.lost != nil {
		return "", f.lost
	}
	return cmp.Or(f.space, testSpace), nil
}

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	f.calls = append(f.calls, slices.Clone(texts))
	vectors := make([][]float32, 0, len(texts))
	for _, text := range texts[:len(texts)-f.drop] {
		vectors = append(vectors, vectorOf(text))
	}
	return vectors, nil
}

func (f *fakeEmbedder) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	return f.EmbedDocuments(ctx, texts)
}

// vectorOf returns a made-up embedding of a text: its length and its first
// and last bytes, which is enough to tell the texts in these tests apart.
func vectorOf(text string) []float32 {
	return []float32{float32(len(text)), float32(text[0]), float32(text[len(text)-1])}
}

// --- the graph's embedding space ------------------------------------------------

const otherSpace = "acme/another-embedder@2; 3 dimensions"

func TestTheFirstEmbeddedDocumentClaimsTheGraphForItsEmbeddersSpace(t *testing.T) {
	store, syncer := enrichingSyncer(t, nil, ingest.Embed(&fakeEmbedder{}))
	if space, err := store.EmbeddingSpace(t.Context()); err != nil || space != "" {
		t.Fatalf("before anything is stored the graph is in the space %q, %v", space, err)
	}

	for _, number := range []string{"1", "2"} {
		if _, err := syncer.Apply(t.Context(), issue(number, 1)); err != nil {
			t.Fatalf("Apply: %v", err)
		}
	}

	if space, err := store.EmbeddingSpace(t.Context()); err != nil || space != testSpace {
		t.Errorf("the graph is in the space %q, %v; want the embedder's, %q", space, err, testSpace)
	}
}

func TestADocumentStoredWithoutAnEmbeddingClaimsNoSpace(t *testing.T) {
	store, syncer := enrichingSyncer(t, nil, ingest.Summarize(&fakeSummarizer{}))

	if _, err := syncer.Apply(t.Context(), issue("1", 1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if space, err := store.EmbeddingSpace(t.Context()); err != nil || space != "" {
		t.Errorf("a graph without a single embedding is in the space %q, %v", space, err)
	}
}

// Two programs sync into one graph, each through an embeddings service of
// its own, and the second service runs another model.
func TestADocumentEmbeddedInAnotherSpaceIsNotStored(t *testing.T) {
	store, ledger := memory.New(), memory.NewLedger()
	syncerWith := func(embedder *fakeEmbedder) *ingest.Syncer {
		return ingest.NewSyncer(ingest.Config{Store: store, Ledger: ledger, Enrichers: []ingest.Enricher{ingest.Embed(embedder)}})
	}
	first, second := syncerWith(&fakeEmbedder{}), syncerWith(&fakeEmbedder{space: otherSpace})
	if _, err := first.Apply(t.Context(), issue("1", 1)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	change := issue("2", 1)
	change.LinkTo(doc("1"), "see #1")
	change.Tag("bug", "Defects in shipped behaviour.")
	_, err := second.Apply(t.Context(), change)

	if !errors.Is(err, storage.ErrOtherSpace) {
		t.Fatalf("Apply returned %v, want an error that wraps storage.ErrOtherSpace", err)
	}
	if !strings.Contains(err.Error(), testSpace) || !strings.Contains(err.Error(), otherSpace) {
		t.Errorf("the error does not name both spaces: %v", err)
	}
	if errors.Is(err, model.ErrDeclined) {
		t.Error("the refusal passes for a model declining, which would let the document through without its embedding")
	}

	// Nothing of the refused document is left behind.
	stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("2"), graph.TopicID("bug")})
	if len(stored) != 0 {
		t.Errorf("the refused change left %d vertices in the graph", len(stored))
	}
	edges, _ := store.Edges(t.Context(), storage.EdgeQuery{Vertex: doc("2"), Direction: graph.Out})
	if len(edges.Edges) != 0 {
		t.Errorf("the refused change left edges in the graph: %v", edges.Edges)
	}
	if _, known, _ := ledger.Mark(t.Context(), doc("2")); known {
		t.Error("the refused change was recorded in the ledger, so it would never be tried again")
	}

	// The graph is where it was, and the first program is not disturbed.
	if space, _ := store.EmbeddingSpace(t.Context()); space != testSpace {
		t.Errorf("the graph is now in the space %q", space)
	}
	if _, err := first.Apply(t.Context(), issue("3", 1)); err != nil {
		t.Errorf("the first program can no longer store documents: %v", err)
	}
}

func TestAChangeMustSayWhatSpaceItsEmbeddingsAreIn(t *testing.T) {
	summarizer := &fakeSummarizer{}
	store, syncer := enrichingSyncer(t, nil, ingest.Summarize(summarizer))
	withEmbeddings := map[string]func(c *source.Change){
		"the document's": func(c *source.Change) { c.Document.Embedding = []float32{1, 2, 3} },
		"a topic's": func(c *source.Change) {
			c.Topics = []graph.Topic{{ID: graph.TopicID("billing"), Name: "Billing", Embedding: []float32{1, 2, 3}}}
			c.Edges = []graph.Edge{{From: c.Document.ID, Kind: graph.TaggedWith, To: graph.TopicID("billing")}}
		},
	}

	// A source that embeds for itself and does not say in what space.
	for whose, supply := range withEmbeddings {
		change := issue("1", 1)
		supply(&change)
		if _, err := syncer.Apply(t.Context(), change); err == nil || !strings.Contains(err.Error(), "does not say what embedding space it is in") {
			t.Errorf("a change carrying %s embedding and naming no space gave %v", whose, err)
		}
	}
	// Such a change is turned away at the door, before a model is asked
	// anything about it.
	if len(summarizer.asked) != 0 {
		t.Errorf("a change that was not valid was summarised all the same: %q", summarizer.asked)
	}
	if stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("1"), graph.TopicID("billing")}); len(stored) != 0 {
		t.Errorf("a change that was not valid left %d vertices behind", len(stored))
	}

	// With the space named, the source's own embeddings are stored, and
	// claim the graph like any others.
	for whose, supply := range withEmbeddings {
		store, syncer := enrichingSyncer(t, nil)
		change := issue("1", 1)
		supply(&change)
		change.EmbeddingSpace = otherSpace
		if _, err := syncer.Apply(t.Context(), change); err != nil {
			t.Fatalf("a change carrying %s embedding and naming its space: %v", whose, err)
		}
		if space, _ := store.EmbeddingSpace(t.Context()); space != otherSpace {
			t.Errorf("a change carrying %s embedding left the graph in the space %q, want %q", whose, space, otherSpace)
		}
	}
}

// The source embedded the document itself. Its topic still needs an
// embedding, and the enricher's embedder is in another space: the one
// change cannot hold embeddings from both.
func TestEmbedDoesNotAddToEmbeddingsFromAnotherSpace(t *testing.T) {
	embedder := &fakeEmbedder{}
	store, syncer := enrichingSyncer(t, nil, ingest.Embed(embedder))
	change := issue("1", 1)
	change.Document.Embedding = []float32{9, 9, 9}
	change.EmbeddingSpace = otherSpace
	change.Tag("billing", "Invoices and refunds.")

	_, err := syncer.Apply(t.Context(), change)

	if !errors.Is(err, storage.ErrOtherSpace) {
		t.Fatalf("Apply returned %v, want an error that wraps storage.ErrOtherSpace", err)
	}
	if len(embedder.calls) != 0 {
		t.Errorf("the embedder was asked to embed %q all the same", embedder.calls)
	}
	if stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("1"), graph.TopicID("billing")}); len(stored) != 0 {
		t.Errorf("the change left %d vertices behind", len(stored))
	}

	// In its own space the source's embedding is kept and the topic's added.
	change.EmbeddingSpace = testSpace
	if _, err := syncer.Apply(t.Context(), change); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(embedder.calls) != 1 || len(embedder.calls[0]) != 1 {
		t.Errorf("the embedder was asked for %q, want the topic alone", embedder.calls)
	}
}

func TestAnEmbedderThatCannotNameItsSpaceFailsTheChange(t *testing.T) {
	lost := errors.New("the embeddings service is not there")
	embedder := &fakeEmbedder{lost: lost}
	store, syncer := enrichingSyncer(t, nil, ingest.Embed(embedder))

	_, err := syncer.Apply(t.Context(), issue("1", 1))

	if !errors.Is(err, lost) {
		t.Fatalf("Apply returned %v, want the embedder's error", err)
	}
	if len(embedder.calls) != 0 {
		t.Error("texts were embedded although the embedder could not say what space it is in")
	}
	if stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("1")}); len(stored) != 0 {
		t.Error("the document was stored all the same")
	}
}

// --- refusals -------------------------------------------------------------------

func TestReconcileCountsTheDocumentsStoredWithoutSomethingAModelRefused(t *testing.T) {
	refuse := func(numbers ...string) ingest.Enricher {
		return ingest.EnricherFunc(func(_ context.Context, c *source.Change) error {
			for _, number := range numbers {
				if c.Document.ID == doc(number) {
					return fmt.Errorf("enriching: %w", model.ErrDeclined)
				}
			}
			return nil
		})
	}
	// Two models, in turn. The first refuses documents 2 and 4, the second
	// documents 4 and 5: three documents are refused something, one of them
	// twice.
	store, syncer := enrichingSyncer(t, nil, refuse("2", "4"), refuse("4", "5"))
	scanner := &fakeSource{name: "github"}
	for _, number := range []string{"1", "2", "3", "4", "5"} {
		scanner.docs = append(scanner.docs, issue(number, 1))
	}

	report, err := syncer.Reconcile(t.Context(), scanner)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if report.Applied != 5 || report.Declined != 3 {
		t.Errorf("the report counts %d applied and %d declined; want 5 and 3", report.Applied, report.Declined)
	}
	if stored, _ := store.GetVertices(t.Context(), []graph.ID{doc("2"), doc("4"), doc("5")}); len(stored) != 3 {
		t.Errorf("%d of the three refused documents were stored, want all of them", len(stored))
	}

	// A second pass finds nothing to do, and nothing to refuse.
	report, err = syncer.Reconcile(t.Context(), scanner)
	if err != nil {
		t.Fatalf("Reconcile again: %v", err)
	}
	if report.Unchanged != 5 || report.Declined != 0 {
		t.Errorf("the second report counts %d unchanged and %d declined; want 5 and 0", report.Unchanged, report.Declined)
	}
}

// A document stored before changes said which embedding space their
// embeddings are in must not look changed now. Its fingerprint is what it
// was, so nothing is synced again on account of the new field.
func TestTheFingerprintOfADocumentAlreadyStoredIsWhatItWas(t *testing.T) {
	ledger := memory.NewLedger()
	syncer := ingest.NewSyncer(ingest.Config{Store: memory.New(), Ledger: ledger})
	change := issue("42", 1)
	change.LinkTo(doc("7"), "see #7")
	change.Tag("bug", "Defects in shipped behaviour.")
	if _, err := syncer.Apply(t.Context(), change); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	mark, known, err := ledger.Mark(t.Context(), doc("42"))
	if err != nil || !known {
		t.Fatalf("Mark: %v, %v", known, err)
	}
	// Computed by the code as it stood before the field was added.
	const before = "b79c2a57d5453b106025101813cd53b0acbf8115577ce391eedd299e27d5310c"
	if mark.Fingerprint != before {
		t.Errorf("the fingerprint is now %s, and it was %s", mark.Fingerprint, before)
	}
}

package ingest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/ingest/ingesttest"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
)

// --- Apply ------------------------------------------------------------------

func TestApplyWritesTheDocumentItsEdgesAndItsTopics(t *testing.T) {
	h := newHarness(t)

	change := issue("42", 1)
	change.LinkTo(doc("7"), "fixes the bug in #7")
	change.ChildOf(doc("1"), "sub-issue of")
	change.Tag("Bug", "Something is not working")
	h.apply(change, ingest.Applied)

	h.requireDocument(doc("42"), "issue 42 at version 1")
	h.requireOutEdges(doc("42"),
		graph.Edge{From: doc("42"), Kind: graph.ChildOf, To: doc("1"), Summary: "sub-issue of"},
		graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: doc("7"), Summary: "fixes the bug in #7"},
		graph.Edge{From: doc("42"), Kind: graph.TaggedWith, To: "topic:bug"},
	)
	topic := h.vertex("topic:bug").(*graph.Topic)
	if topic.Name != "Bug" || topic.Summary != "Something is not working" {
		t.Errorf("the topic was created as %+v", topic)
	}
	// Neither end of an edge is created just for being pointed at.
	if _, exists := h.vertices(doc("7"))[doc("7")]; exists {
		t.Errorf("a document was created merely because another one links to it")
	}
}

func TestApplyingTheSameChangeAgainDoesNothing(t *testing.T) {
	h := newHarness(t)
	change := issue("42", 1)
	change.LinkTo(doc("7"), "see #7")

	h.apply(change, ingest.Applied)
	h.apply(change, ingest.Unchanged)

	// A newer version with nothing else different is not a change either.
	sameStateLater := change
	sameStateLater.Version = 2
	h.apply(sameStateLater, ingest.Unchanged)

	if h.enriched.Load() != 1 {
		t.Errorf("the enricher ran %d times for one real change", h.enriched.Load())
	}
}

// Whether a change needs applying is judged by fingerprint. Everything that
// reaches the graph has to count towards it, the edges and topics as much as
// the document's own fields.
func TestAnyDifferenceMakesAChange(t *testing.T) {
	base := func() source.Change {
		change := issue("42", 1)
		change.LinkTo(doc("7"), "see #7")
		return change
	}
	differences := map[string]func(c *source.Change){
		"the title":            func(c *source.Change) { c.Document.Title = "renamed" },
		"the content":          func(c *source.Change) { c.Content = "rewritten" },
		"the URL":              func(c *source.Change) { c.Document.URL += "?moved" },
		"an edge more":         func(c *source.Change) { c.LinkTo(doc("8"), "see #8") },
		"an edge fewer":        func(c *source.Change) { c.Edges = nil },
		"an edge's summary":    func(c *source.Change) { c.Edges[0].Summary = "fixes #7" },
		"an edge's kind":       func(c *source.Change) { c.Edges[0].Kind = graph.ChildOf },
		"a tag":                func(c *source.Change) { c.Tag("bug", "") },
		"a link that resolves": func(c *source.Change) { c.LinkURL("https://github.test/acme/api/issues/7", "") },
	}
	for name, alter := range differences {
		h := newHarness(t)
		h.apply(base(), ingest.Applied)

		altered := base()
		altered.Edges = slices.Clone(altered.Edges)
		alter(&altered)
		if name == "a link that resolves" {
			// Resolves to the edge the document already has, so this one
			// is the exception that proves the rule: nothing changed.
			h.apply(altered, ingest.Unchanged)
			continue
		}
		outcome, err := h.syncer.Apply(t.Context(), altered)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if outcome != ingest.Applied {
			t.Errorf("a change differing only in %s was %s, want applied", name, outcome)
		}
	}
}

func TestApplyReplacesTheDocumentsEdges(t *testing.T) {
	h := newHarness(t)

	// Another document links to #42; that edge is its own and must survive.
	other := issue("9", 1)
	other.LinkTo(doc("42"), "depends on #42")
	h.apply(other, ingest.Applied)

	first := issue("42", 1)
	first.LinkTo(doc("7"), "see #7")
	first.LinkTo(doc("8"), "see #8")
	first.Tag("bug", "")
	h.apply(first, ingest.Applied)

	// The next version drops the link to #8 and the tag, rewords the link
	// to #7 and gains a link to #10.
	second := issue("42", 2)
	second.LinkTo(doc("7"), "really fixes #7")
	second.LinkTo(doc("10"), "see #10")
	h.apply(second, ingest.Applied)

	h.requireOutEdges(doc("42"),
		graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: doc("10"), Summary: "see #10"},
		graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: doc("7"), Summary: "really fixes #7"},
	)
	h.requireOutEdges(doc("9"),
		graph.Edge{From: doc("9"), Kind: graph.LinksTo, To: doc("42"), Summary: "depends on #42"},
	)
}

func TestApplyTidiesWhatSourcesReport(t *testing.T) {
	h := newHarness(t)

	change := issue("42", 1)
	change.LinkTo(doc("42"), "mentions itself")         // says nothing
	change.LinkTo(doc("7"), "")                         // mentioned twice:
	change.LinkTo(doc("7"), "the mention with context") // the one with a summary is kept
	change.Edges = append(change.Edges, graph.Edge{From: doc("42"), Kind: graph.InTopic, To: "topic:undescribed"})
	h.apply(change, ingest.Applied)

	h.requireOutEdges(doc("42"),
		graph.Edge{From: doc("42"), Kind: graph.InTopic, To: "topic:undescribed"},
		graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: doc("7"), Summary: "the mention with context"},
	)
	// A topic that was pointed at without being described still gets created.
	if topic := h.vertex("topic:undescribed").(*graph.Topic); topic.Name != "undescribed" {
		t.Errorf("the undescribed topic was created as %+v", topic)
	}
}

func TestApplyIgnoresChangesThatArriveLate(t *testing.T) {
	h := newHarness(t)

	newer := issue("42", 5)
	newer.LinkTo(doc("7"), "see #7")
	h.apply(newer, ingest.Applied)

	older := issue("42", 3)
	older.LinkTo(doc("8"), "see #8")
	h.apply(older, ingest.Stale)

	h.requireDocument(doc("42"), "issue 42 at version 5")
	h.requireOutEdges(doc("42"), graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: doc("7"), Summary: "see #7"})
}

func TestRemovalLeavesATombstone(t *testing.T) {
	h := newHarness(t)

	other := issue("9", 1)
	other.LinkTo(doc("42"), "depends on #42")
	h.apply(other, ingest.Applied)

	change := issue("42", 5)
	change.LinkTo(doc("7"), "see #7")
	h.apply(change, ingest.Applied)

	h.apply(removal("42", 6), ingest.Removed)
	if _, exists := h.vertices(doc("42"))[doc("42")]; exists {
		t.Errorf("the document is still in the graph after its removal")
	}
	h.requireOutEdges(doc("42"))
	// What other documents say about it is theirs to say.
	h.requireOutEdges(doc("9"), graph.Edge{From: doc("9"), Kind: graph.LinksTo, To: doc("42"), Summary: "depends on #42"})

	h.apply(removal("42", 6), ingest.Unchanged)

	// Late edits cannot bring it back: neither an older one, nor one with
	// the very version of the removal.
	h.apply(issue("42", 5), ingest.Stale)
	h.apply(issue("42", 6), ingest.Stale)
	if _, exists := h.vertices(doc("42"))[doc("42")]; exists {
		t.Errorf("a late edit brought a removed document back")
	}

	// A genuinely newer version does.
	h.apply(issue("42", 7), ingest.Applied)
	h.requireDocument(doc("42"), "issue 42 at version 7")
}

func TestRemovalOfAnUnknownDocumentStillBlocksLateEdits(t *testing.T) {
	h := newHarness(t)

	h.apply(removal("42", 5), ingest.Removed)
	h.apply(issue("42", 3), ingest.Stale)

	if _, exists := h.vertices(doc("42"))[doc("42")]; exists {
		t.Errorf("an edit older than the removal created the document")
	}
}

func TestLinksResolveAcrossSources(t *testing.T) {
	jira := &fakeSource{name: "jira", urls: map[string]graph.ID{
		"https://acme.atlassian.net/browse/PAY-7": "doc:jira:PAY-7",
	}}
	h := newHarness(t, jira)

	change := issue("42", 1)
	change.LinkURL("https://acme.atlassian.net/browse/PAY-7", "implements PAY-7")
	change.LinkURL("https://example.com/nothing-we-track", "an outside link")
	change.LinkURL("https://github.test/acme/api/issues/7", "relates to #7") // the harness's own source knows this one
	h.apply(change, ingest.Applied)

	h.requireOutEdges(doc("42"),
		graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: doc("7"), Summary: "relates to #7"},
		graph.Edge{From: doc("42"), Kind: graph.LinksTo, To: "doc:jira:PAY-7", Summary: "implements PAY-7"},
	)
}

func TestSourcesNeverOverwriteAnExistingTopic(t *testing.T) {
	h := newHarness(t)

	// Somebody (an enricher, say) has given the topic a proper description.
	curated := &graph.Topic{ID: "topic:bug", Name: "Bug", Summary: "Defects in shipped behaviour.", Embedding: []float32{1, 2, 3}}
	if err := h.store.PutVertices(t.Context(), []graph.Vertex{curated}); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}

	change := issue("42", 1)
	change.Tag("bug", "") // the source knows only the name
	h.apply(change, ingest.Applied)

	topic := h.vertex("topic:bug").(*graph.Topic)
	if topic.Summary != curated.Summary || !slices.Equal(topic.Embedding, curated.Embedding) {
		t.Errorf("the source overwrote the topic: %+v", topic)
	}
}

func TestEnrichersFillInAndExtendAChange(t *testing.T) {
	h := newHarness(t)
	h.enrich = func(c *source.Change) error {
		c.Document.Summary = "summary of: " + c.Content
		c.Document.Embedding = []float32{0.5, 0.25}
		c.EmbeddingSpace = "acme/by-hand@1; 2 dimensions"
		c.InTopic("Payments", "Money in, money out.")
		return nil
	}

	h.apply(issue("42", 1), ingest.Applied)

	document := h.vertex(doc("42")).(*graph.Document)
	if document.Summary != "summary of: body of issue 42 at version 1" || !slices.Equal(document.Embedding, []float32{0.5, 0.25}) {
		t.Errorf("the enricher's work was not stored: %+v", document)
	}
	h.requireOutEdges(doc("42"), graph.Edge{From: doc("42"), Kind: graph.InTopic, To: "topic:payments"})
}

func TestAFailedEnricherLeavesNothingBehind(t *testing.T) {
	h := newHarness(t)
	broken := errors.New("the model is down")
	h.enrich = func(*source.Change) error { return broken }

	change := issue("42", 1)
	change.LinkTo(doc("7"), "see #7")
	if _, err := h.syncer.Apply(t.Context(), change); !errors.Is(err, broken) {
		t.Fatalf("Apply returned %v, want the enricher's error", err)
	}
	if len(h.vertices(doc("42"))) != 0 {
		t.Errorf("a document was stored although enriching it failed")
	}
	h.requireOutEdges(doc("42"))

	// Once the enricher works again, the same change goes through.
	h.enrich = nil
	h.apply(change, ingest.Applied)
}

func TestAnEnricherCannotSmuggleInInvalidEdges(t *testing.T) {
	h := newHarness(t)
	h.enrich = func(c *source.Change) error {
		c.Edges = append(c.Edges, graph.Edge{From: doc("someone-else"), Kind: graph.LinksTo, To: doc("7")})
		return nil
	}
	if _, err := h.syncer.Apply(t.Context(), issue("42", 1)); err == nil {
		t.Errorf("an edge starting at another document was accepted from an enricher")
	}
}

func TestApplyRejectsInvalidChanges(t *testing.T) {
	h := newHarness(t)
	valid := issue("42", 1)

	bad := map[string]func(c *source.Change){
		"malformed ID":          func(c *source.Change) { c.Document.ID = "42" },
		"a topic ID":            func(c *source.Change) { c.Document.ID = "topic:bug" },
		"no source":             func(c *source.Change) { c.Document.Source = "" },
		"someone else's source": func(c *source.Change) { c.Document.Source = "jira" },
		"an edge from elsewhere": func(c *source.Change) {
			c.Edges = append(c.Edges, graph.Edge{From: doc("7"), Kind: graph.LinksTo, To: doc("8")})
		},
		"a malformed edge": func(c *source.Change) {
			c.Edges = append(c.Edges, graph.Edge{From: c.Document.ID, Kind: "Not A Kind", To: doc("8")})
		},
		"a malformed topic":  func(c *source.Change) { c.Topics = append(c.Topics, graph.Topic{ID: "nope"}) },
		"a link with no URL": func(c *source.Change) { c.LinkURL("", "nowhere") },
	}
	for name, spoil := range bad {
		change := valid
		spoil(&change)
		if _, err := h.syncer.Apply(t.Context(), change); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(h.vertices(doc("42"))) != 0 {
		t.Errorf("an invalid change left something in the graph")
	}
}

// Many workers apply different versions of one document at the same time.
// Whatever the interleaving, the document must end up exactly as its newest
// version describes it, with no edge left over from any other.
func TestConcurrentChangesToOneDocument(t *testing.T) {
	h := newHarness(t)
	const versions = 40

	changes := make([]source.Change, versions)
	for v := range changes {
		change := issue("42", uint64(v+1))
		for target := range 5 {
			// Each version links to its own five documents.
			change.LinkTo(doc(fmt.Sprintf("v%d-t%d", v+1, target)), fmt.Sprintf("version %d", v+1))
		}
		changes[v] = change
	}

	var group sync.WaitGroup
	for _, change := range changes {
		group.Go(func() {
			if _, err := h.syncer.Apply(t.Context(), change); err != nil {
				t.Errorf("Apply: %v", err)
			}
		})
	}
	group.Wait()

	newest := changes[versions-1]
	h.requireDocument(doc("42"), newest.Document.Title)
	h.requireOutEdges(doc("42"), newest.Edges...)
}

// --- Follow, Deliver, Handler -------------------------------------------------

func TestFollowSavesCheckpointsAndResumesFromThem(t *testing.T) {
	h := newHarness(t)
	feed := &fakeSource{name: "github"}
	for v := range 3 {
		change := issue(fmt.Sprint(v+1), 1)
		change.Checkpoint = source.Checkpoint(fmt.Sprintf("position-%d", v+1))
		feed.feed = append(feed.feed, change)
	}

	// The feed ends after three changes; Follow reports that it stopped.
	if err := h.syncer.Follow(t.Context(), feed); err == nil {
		t.Fatalf("Follow returned nil, want the reason it stopped")
	}
	if len(h.vertices(doc("1"), doc("2"), doc("3"))) != 3 {
		t.Errorf("the changes in the feed were not all applied")
	}
	if got := feed.subscribedFrom; len(got) != 1 || got[0] != "" {
		t.Errorf("the first Follow subscribed from %q, want the beginning", got)
	}

	// The second time, it picks up where the first left off.
	_ = h.syncer.Follow(t.Context(), feed)
	if got := feed.subscribedFrom; len(got) != 2 || got[1] != "position-3" {
		t.Errorf("the second Follow subscribed from %q, want position-3", got)
	}
}

func TestFollowStopsWhenTheContextEnds(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.syncer.Follow(ctx, &fakeSource{name: "github"}); !errors.Is(err, context.Canceled) {
		t.Errorf("Follow returned %v, want context.Canceled", err)
	}
}

func TestHandlerAnswersSendersCorrectly(t *testing.T) {
	h := newHarness(t)
	receiver := &fakeSource{name: "github"}
	server := httptest.NewServer(h.syncer.Handler(receiver))
	defer server.Close()

	post := func(body string) int {
		t.Helper()
		response, err := http.Post(server.URL, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer response.Body.Close()
		reply, _ := io.ReadAll(response.Body)
		if strings.Contains(string(reply), "secret detail") {
			t.Errorf("the response leaks an internal error: %q", reply)
		}
		return response.StatusCode
	}

	receiver.receive = func(source.Delivery) ([]source.Change, error) {
		return []source.Change{issue("42", 1)}, nil
	}
	if status := post("a delivery"); status != http.StatusNoContent {
		t.Errorf("a good delivery was answered with %d, want 204", status)
	}
	h.requireDocument(doc("42"), "issue 42 at version 1")

	receiver.receive = func(source.Delivery) ([]source.Change, error) { return nil, nil }
	if status := post("about something we do not track"); status != http.StatusNoContent {
		t.Errorf("a delivery with no changes was answered with %d, want 204", status)
	}

	receiver.receive = func(source.Delivery) ([]source.Change, error) {
		return nil, fmt.Errorf("bad signature: %w", source.ErrUnauthorized)
	}
	if status := post("forged"); status != http.StatusUnauthorized {
		t.Errorf("an unauthentic delivery was answered with %d, want 401", status)
	}

	receiver.receive = func(source.Delivery) ([]source.Change, error) {
		return nil, fmt.Errorf("not JSON: %w", source.ErrMalformed)
	}
	if status := post("garbled"); status != http.StatusBadRequest {
		t.Errorf("a malformed delivery was answered with %d, want 400", status)
	}

	// A failure on our side asks the sender to try again, without details.
	h.enrich = func(*source.Change) error { return errors.New("secret detail: the model is down") }
	receiver.receive = func(source.Delivery) ([]source.Change, error) {
		return []source.Change{issue("43", 1)}, nil
	}
	if status := post("a delivery we fail to apply"); status != http.StatusInternalServerError {
		t.Errorf("a delivery we failed to apply was answered with %d, want 500", status)
	}

	response, err := http.Get(server.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("a GET was answered with %d, want 405", response.StatusCode)
	}
}

// --- Reconcile ----------------------------------------------------------------

func TestReconcileFillsTheGraphAndThenHasNothingToDo(t *testing.T) {
	h := newHarness(t)
	scanner := &fakeSource{name: "github"}
	for n := range 5 {
		change := issue(fmt.Sprint(n+1), 1)
		change.LinkTo(doc(fmt.Sprint((n+1)%5+1)), "next")
		scanner.docs = append(scanner.docs, change)
	}

	report := h.reconcile(scanner)
	if report != (ingest.Report{Applied: 5}) {
		t.Errorf("the first pass reported %+v, want 5 applied", report)
	}

	report = h.reconcile(scanner)
	if report != (ingest.Report{Unchanged: 5}) {
		t.Errorf("the second pass reported %+v, want 5 unchanged", report)
	}
	if h.enriched.Load() != 5 {
		t.Errorf("enrichers ran %d times; documents that had not changed were enriched again", h.enriched.Load())
	}
}

func TestReconcileRemovesADocumentOnlyAfterTwoPassesMissIt(t *testing.T) {
	h := newHarness(t)
	scanner := &fakeSource{name: "github", docs: []source.Change{issue("1", 1), issue("2", 1), issue("3", 1)}}
	h.reconcile(scanner)

	// #2 disappears from the listing.
	scanner.skip = map[graph.ID]bool{doc("2"): true}

	if report := h.reconcile(scanner); report.Removed != 0 {
		t.Fatalf("one pass that missed a document removed it: %+v", report)
	}
	h.requireDocument(doc("2"), "issue 2 at version 1")

	if report := h.reconcile(scanner); report.Removed != 1 {
		t.Fatalf("a second pass that missed the document reported %+v, want it removed", report)
	}
	if len(h.vertices(doc("2"))) != 0 {
		t.Errorf("the document is still in the graph")
	}
	if len(h.vertices(doc("1"), doc("3"))) != 2 {
		t.Errorf("documents that were listed all along were removed")
	}

	// It had only been hiding: the next pass lists it again, unchanged, and
	// it comes back even though its version is no newer than its tombstone.
	scanner.skip = nil
	if report := h.reconcile(scanner); report.Applied != 1 {
		t.Fatalf("the pass that found the document again reported %+v, want it applied", report)
	}
	h.requireDocument(doc("2"), "issue 2 at version 1")
}

func TestAChangeFromTheFeedSavesADocumentFromTheSweep(t *testing.T) {
	h := newHarness(t)
	scanner := &fakeSource{name: "github", docs: []source.Change{issue("1", 1), issue("2", 1)}}
	h.reconcile(scanner)

	scanner.skip = map[graph.ID]bool{doc("2"): true}
	h.reconcile(scanner)

	// Between the two passes that miss it, the feed reports on the document:
	// proof that it is alive, whatever the listings say.
	h.apply(issue("2", 2), ingest.Applied)

	if report := h.reconcile(scanner); report.Removed != 0 {
		t.Fatalf("a document the feed had just reported on was removed: %+v", report)
	}
	h.requireDocument(doc("2"), "issue 2 at version 2")
}

func TestALateChangeFromTheFeedDoesNotSaveADocumentFromTheSweep(t *testing.T) {
	h := newHarness(t)
	scanner := &fakeSource{name: "github", docs: []source.Change{issue("1", 1), issue("2", 5)}}
	h.reconcile(scanner)

	scanner.skip = map[graph.ID]bool{doc("2"): true}
	h.reconcile(scanner)

	// Between the two passes that miss it, the feed delivers old news of the
	// document. That shows it existed once, which was never in doubt.
	h.apply(issue("2", 3), ingest.Stale)

	if report := h.reconcile(scanner); report.Removed != 1 {
		t.Fatalf("the second pass to miss the document reported %+v, want it removed", report)
	}
}

func TestAScanThatListsAnOlderVersionHasStillSeenTheDocument(t *testing.T) {
	h := newHarness(t)

	// The feed is ahead of the listing: it has delivered version 2, and the
	// passes that follow all list version 1.
	h.apply(issue("1", 2), ingest.Applied)
	scanner := &fakeSource{name: "github", docs: []source.Change{issue("1", 1)}}

	for pass := 1; pass <= 3; pass++ {
		if report := h.reconcile(scanner); report != (ingest.Report{Stale: 1}) {
			t.Fatalf("pass %d reported %+v, want the listed version found stale and nothing more", pass, report)
		}
		// Old as the listing is, it shows that the document exists.
		h.requireDocument(doc("1"), "issue 1 at version 2")
	}
}

func TestAScanBringsBackADocumentRemovedBeforeItBegan(t *testing.T) {
	h := newHarness(t)

	// The feed reported the document and then its removal, giving the
	// removal the later version. Since then the document has been restored
	// at the source, just as it was, and no word of that has arrived.
	h.apply(issue("1", 3), ingest.Applied)
	h.apply(removal("1", 5), ingest.Removed)
	scanner := &fakeSource{name: "github", docs: []source.Change{issue("1", 3)}}

	// A scan lists what exists now, so the removal's later version does not
	// outrank the listing.
	if report := h.reconcile(scanner); report != (ingest.Report{Applied: 1}) {
		t.Fatalf("the pass that listed the document reported %+v, want it applied", report)
	}
	h.requireDocument(doc("1"), "issue 1 at version 3")

	if report := h.reconcile(scanner); report != (ingest.Report{Unchanged: 1}) {
		t.Fatalf("the pass after that reported %+v, want nothing left to do", report)
	}
}

func TestARemovalThatArrivesDuringAScanOutranksItsListing(t *testing.T) {
	h := newHarness(t)
	scanner := &fakeSource{name: "github", docs: []source.Change{issue("1", 1), issue("2", 1)}}
	h.reconcile(scanner)

	// While a pass is under way, the feed reports that #2 is gone. The pass
	// goes on to list #2, but the listing may have been read before the
	// removal happened, so the removal stands.
	scanner.beforeListing = func(id graph.ID) {
		if id == doc("2") {
			h.apply(removal("2", 1), ingest.Removed)
		}
	}
	if report := h.reconcile(scanner); report != (ingest.Report{Unchanged: 1, Stale: 1}) {
		t.Fatalf("the pass the removal arrived during reported %+v, want #1 unchanged and the listing of #2 found stale", report)
	}
	if len(h.vertices(doc("2"))) != 0 {
		t.Errorf("a listing brought back a document that was removed while the scan was under way")
	}

	// The next pass began after the removal was recorded. If it lists the
	// document all the same, the document exists.
	scanner.beforeListing = nil
	if report := h.reconcile(scanner); report != (ingest.Report{Applied: 1, Unchanged: 1}) {
		t.Fatalf("the next pass reported %+v, want #2 applied again", report)
	}
	h.requireDocument(doc("2"), "issue 2 at version 1")
}

func TestReconcileResumesAScanThatFailed(t *testing.T) {
	h := newHarness(t)
	scanner := &fakeSource{name: "github"}
	for n := range 6 {
		change := issue(fmt.Sprint(n+1), 1)
		change.Checkpoint = source.Checkpoint(fmt.Sprintf("after-%d", n+1))
		scanner.docs = append(scanner.docs, change)
	}

	// The first attempt dies after four documents.
	outage := errors.New("rate limited")
	scanner.failAfter, scanner.failure = 4, outage
	if _, err := h.syncer.Reconcile(t.Context(), scanner); !errors.Is(err, outage) {
		t.Fatalf("Reconcile returned %v, want the scanner's error", err)
	}

	// The second picks up from the last checkpoint instead of starting over.
	scanner.failAfter = 0
	report := h.reconcile(scanner)
	if got := scanner.scannedFrom; len(got) != 2 || got[0] != "" || got[1] != "after-4" {
		t.Errorf("the scans started from %q, want the beginning and then after-4", got)
	}
	if report.Applied != 2 {
		t.Errorf("the resumed pass reported %+v, want the 2 remaining documents applied", report)
	}
	if len(h.vertices(doc("1"), doc("2"), doc("3"), doc("4"), doc("5"), doc("6"))) != 6 {
		t.Errorf("not every document made it into the graph")
	}

	// A scan that failed is not a completed scan: nothing may be swept on
	// the strength of it. Two clean passes later, everything is still here.
	h.reconcile(scanner)
	if report := h.reconcile(scanner); report.Removed != 0 {
		t.Errorf("documents were removed after a failed scan: %+v", report)
	}
}

func TestReconcileStopsWhenTheContextEnds(t *testing.T) {
	h := newHarness(t)
	scanner := &fakeSource{name: "github", docs: []source.Change{issue("1", 1)}}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.syncer.Reconcile(ctx, scanner); err == nil {
		t.Errorf("Reconcile with an ended context reported success")
	}
}

// --- convergence --------------------------------------------------------------

func TestTheGraphConvergesOnTheSource(t *testing.T) {
	for seed := range uint64(25) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			ingesttest.Converge(t, memory.New(), memory.NewLedger(), seed, ingesttest.Options{})
		})
	}
}

func TestTheFeedAloneSufficesWhenNothingIsLost(t *testing.T) {
	for seed := range uint64(25) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			ingesttest.ConvergeWithoutLoss(t, memory.New(), memory.NewLedger(), seed, ingesttest.Options{})
		})
	}
}

// --- helpers ------------------------------------------------------------------

// harness is a Syncer over in-memory storage with one counting enricher.
type harness struct {
	t        *testing.T
	store    *memory.Store
	ledger   *memory.Ledger
	syncer   *ingest.Syncer
	enriched atomic.Int64               // how many times the enricher has run
	enrich   func(*source.Change) error // what the enricher does, if anything
}

func newHarness(t *testing.T, others ...source.Source) *harness {
	t.Helper()
	h := &harness{t: t, store: memory.New(), ledger: memory.NewLedger()}
	github := &fakeSource{name: "github", urls: map[string]graph.ID{
		"https://github.test/acme/api/issues/7": doc("7"),
	}}
	h.syncer = ingest.NewSyncer(ingest.Config{
		Store:   h.store,
		Ledger:  h.ledger,
		Sources: append([]source.Source{github}, others...),
		Enrichers: []ingest.Enricher{ingest.EnricherFunc(func(_ context.Context, c *source.Change) error {
			h.enriched.Add(1)
			if h.enrich != nil {
				return h.enrich(c)
			}
			return nil
		})},
		Logger: slog.New(slog.DiscardHandler),
	})
	return h
}

func (h *harness) apply(change source.Change, want ingest.Outcome) {
	h.t.Helper()
	got, err := h.syncer.Apply(h.t.Context(), change)
	if err != nil {
		h.t.Fatalf("Apply(%s at version %d): %v", change.Document.ID, change.Version, err)
	}
	if got != want {
		h.t.Fatalf("Apply(%s at version %d) was %s, want %s", change.Document.ID, change.Version, got, want)
	}
}

func (h *harness) reconcile(scanner source.Scanner) ingest.Report {
	h.t.Helper()
	report, err := h.syncer.Reconcile(h.t.Context(), scanner)
	if err != nil {
		h.t.Fatalf("Reconcile: %v", err)
	}
	return report
}

func (h *harness) vertices(ids ...graph.ID) map[graph.ID]graph.Vertex {
	h.t.Helper()
	found, err := h.store.GetVertices(h.t.Context(), ids)
	if err != nil {
		h.t.Fatalf("GetVertices: %v", err)
	}
	return found
}

func (h *harness) vertex(id graph.ID) graph.Vertex {
	h.t.Helper()
	vertex, exists := h.vertices(id)[id]
	if !exists {
		h.t.Fatalf("%s is not in the graph", id)
	}
	return vertex
}

func (h *harness) requireDocument(id graph.ID, title string) {
	h.t.Helper()
	document, ok := h.vertex(id).(*graph.Document)
	if !ok || document.Title != title {
		h.t.Fatalf("%s is stored as %+v, want a document titled %q", id, h.vertex(id), title)
	}
}

// requireOutEdges fails the test unless the vertex has exactly these
// outgoing edges, in any order.
func (h *harness) requireOutEdges(id graph.ID, want ...graph.Edge) {
	h.t.Helper()
	page, err := h.store.Edges(h.t.Context(), storage.EdgeQuery{Vertex: id, Direction: graph.Out, Limit: 10_000})
	if err != nil {
		h.t.Fatalf("Edges(%s): %v", id, err)
	}
	got := slices.SortedFunc(slices.Values(page.Edges), graph.Edge.Compare)
	wanted := slices.SortedFunc(slices.Values(want), graph.Edge.Compare)
	if !slices.Equal(got, wanted) {
		h.t.Fatalf("the edges of %s are\n  %v\nwant\n  %v", id, got, wanted)
	}
}

// doc returns the ID of a pretend GitHub issue.
func doc(number string) graph.ID {
	return graph.DocumentID("github", "acme/api#"+number)
}

// issue returns a change describing a pretend GitHub issue at some version.
func issue(number string, version uint64) source.Change {
	return source.Change{
		Document: graph.Document{
			ID:     doc(number),
			Type:   graph.TypeIssue,
			Source: "github",
			URL:    "https://github.test/acme/api/issues/" + number,
			Title:  fmt.Sprintf("issue %s at version %d", number, version),
		},
		Version: version,
		Content: fmt.Sprintf("body of issue %s at version %d", number, version),
	}
}

// removal returns a change saying that a pretend GitHub issue is gone.
func removal(number string, version uint64) source.Change {
	return source.Change{
		Document: graph.Document{ID: doc(number), Source: "github"},
		Version:  version,
		Deleted:  true,
	}
}

// fakeSource is a source with every capability, driven by the test.
type fakeSource struct {
	name string
	urls map[string]graph.ID // what Resolve recognizes

	feed           []source.Change // what Subscribe yields before ending
	subscribedFrom []string        // the checkpoint of every Subscribe call

	receive func(source.Delivery) ([]source.Change, error) // what Receive does

	docs          []source.Change   // what Scan lists
	skip          map[graph.ID]bool // documents Scan pretends not to have
	beforeListing func(graph.ID)    // called just before Scan lists each document, if set
	failAfter     int               // Scan fails after this many documents, if not zero
	failure       error             // the error Scan fails with
	scannedFrom   []string          // the checkpoint of every Scan call
}

func (f *fakeSource) Name() string { return f.name }

func (f *fakeSource) Resolve(url string) (graph.ID, bool) {
	id, recognized := f.urls[url]
	return id, recognized
}

func (f *fakeSource) Subscribe(ctx context.Context, after source.Checkpoint) iter.Seq2[source.Change, error] {
	f.subscribedFrom = append(f.subscribedFrom, string(after))
	return func(yield func(source.Change, error) bool) {
		for _, change := range f.feed {
			if err := ctx.Err(); err != nil {
				yield(source.Change{}, err)
				return
			}
			if !yield(change, nil) {
				return
			}
		}
	}
}

func (f *fakeSource) Receive(_ context.Context, delivery source.Delivery) ([]source.Change, error) {
	return f.receive(delivery)
}

func (f *fakeSource) Scan(ctx context.Context, resume source.Checkpoint) iter.Seq2[source.Change, error] {
	f.scannedFrom = append(f.scannedFrom, string(resume))
	return func(yield func(source.Change, error) bool) {
		// Skip what was listed before the checkpoint.
		start := 0
		for i, change := range f.docs {
			if len(resume) > 0 && string(change.Checkpoint) == string(resume) {
				start = i + 1
			}
		}
		for listed, change := range f.docs[start:] {
			if err := ctx.Err(); err != nil {
				yield(source.Change{}, err)
				return
			}
			if f.failAfter > 0 && listed == f.failAfter {
				yield(source.Change{}, f.failure)
				return
			}
			if f.skip[change.Document.ID] {
				continue
			}
			if f.beforeListing != nil {
				f.beforeListing(change.Document.ID)
			}
			if !yield(change, nil) {
				return
			}
		}
	}
}

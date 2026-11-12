package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/retrieval/jev"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
	"github.com/umran/grafsol/typesafe"
)

const performance = graph.ID("topic:performance")

// scriptedJudge rates leads and weighs evidence by rule, and writes down
// everything it was asked.
type scriptedJudge struct {
	// rate rates a lead; nil means 0 for everything.
	rate func(q retrieval.Question, lead retrieval.Lead) float64

	// weigh reads a set of evidence; nil means 0 for everything.
	weigh func(q retrieval.Question, evidence []graph.ID) float64

	// failFor makes Assess fail for the question with this text.
	failFor string

	mu    sync.Mutex
	calls []judgeCall
}

type judgeCall struct {
	question retrieval.Question
	evidence []graph.ID
	leads    []retrieval.Lead
}

var errJudge = errors.New("the judge is out")

func (j *scriptedJudge) Assess(_ context.Context, q retrieval.Question, evidence []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	shown := idsOf(evidence)
	j.mu.Lock()
	j.calls = append(j.calls, judgeCall{question: q, evidence: shown, leads: slices.Clone(leads)})
	j.mu.Unlock()
	if j.failFor != "" && q.Text == j.failFor {
		return retrieval.Assessment{}, errJudge
	}

	assessment := retrieval.Assessment{Potential: make([]float64, len(leads)), Tokens: 10}
	for i, lead := range leads {
		if j.rate != nil {
			assessment.Potential[i] = j.rate(q, lead)
		}
	}
	if len(evidence) > 0 && j.weigh != nil {
		assessment.Sufficient = j.weigh(q, shown)
	}
	return assessment, nil
}

// asked returns the calls made about the question with that text, in order.
func (j *scriptedJudge) asked(text string) []judgeCall {
	j.mu.Lock()
	defer j.mu.Unlock()
	var calls []judgeCall
	for _, call := range j.calls {
		if call.question.Text == text {
			calls = append(calls, call)
		}
	}
	return calls
}

func idsOf(evidence []retrieval.Evidence) []graph.ID {
	var ids []graph.ID
	for _, piece := range evidence {
		ids = append(ids, piece.Document.ID)
	}
	return ids
}

// byTarget returns a rule that rates a lead by the vertex it leads to.
func byTarget(ratings map[graph.ID]float64) func(retrieval.Question, retrieval.Lead) float64 {
	return func(_ retrieval.Question, lead retrieval.Lead) float64 { return ratings[lead.Target.VertexID()] }
}

func TestLeadsAreEveryWayASearchCouldComeAcrossAVertex(t *testing.T) {
	set := example(t)
	leads := Leads(set.Graph)

	type shape struct {
		target, from graph.ID
		edge         graph.Edge
	}
	var got []shape
	for _, lead := range leads {
		s := shape{target: lead.Target.VertexID(), edge: lead.Edge}
		if lead.From != nil {
			s.from = lead.From.VertexID()
		}
		got = append(got, s)
	}
	causes := set.Graph.Edges[0] // cache links to slow
	tagged := set.Graph.Edges[1] // slow is tagged with the topic
	want := []shape{
		// Each document as a place to start.
		{target: slow}, {target: cache}, {target: dashboard}, {target: rota},
		// Each edge, from either end. A topic is reached only this way.
		{target: slow, from: cache, edge: causes},
		{target: cache, from: slow, edge: causes},
		{target: performance, from: slow, edge: tagged},
		{target: slow, from: performance, edge: tagged},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the leads are\n%+v\nwant\n%+v", got, want)
	}
	// A lead carries the vertices themselves, as the judge is shown them,
	// and the edge with its note.
	if title := leads[4].From.(*graph.Document).Title; title != "Remove the price cache" || leads[4].Edge.Summary == "" {
		t.Errorf("a lead reached by an edge carries the vertex %q and the note %q", title, leads[4].Edge.Summary)
	}
	if !leads[0].Seed() || leads[4].Seed() {
		t.Error("the leads do not say rightly which of them are places to start")
	}

	// An edge that ends at a vertex the graph does not hold leads nowhere,
	// and neither does one from a vertex to itself.
	set.Graph.Edges = append(set.Graph.Edges,
		graph.Edge{From: slow, Kind: graph.LinksTo, To: "doc:s:elsewhere"},
		graph.Edge{From: slow, Kind: graph.LinksTo, To: slow})
	if more := Leads(set.Graph); len(more) != len(want) {
		t.Errorf("with two edges that lead nowhere there are %d leads, want %d", len(more), len(want))
	}
}

func TestRatingsAskAboutEveryLeadForEveryQuestion(t *testing.T) {
	set := example(t)
	judge := &scriptedJudge{rate: func(_ retrieval.Question, lead retrieval.Lead) float64 {
		switch {
		case !lead.Seed():
			return 0.5
		case lead.Target.VertexID() == cache:
			return 1
		}
		return 0
	}}

	ratings, err := Ratings(t.Context(), judge, set)
	if err != nil {
		t.Fatalf("Ratings: %v", err)
	}
	if len(ratings) != 3*8 {
		t.Fatalf("%d ratings came back, want one for each of 8 leads for each of 3 questions", len(ratings))
	}

	// The judge was asked once per question: the question as it stands in
	// the set, no evidence, and every lead.
	for _, q := range set.Questions {
		calls := judge.asked(q.Text)
		if len(calls) != 1 || calls[0].question.Context != q.Context || len(calls[0].evidence) != 0 || len(calls[0].leads) != 8 {
			t.Errorf("for %s the judge was asked %+v", q.ID, calls)
		}
	}

	// The ratings come back in the order of the questions, then of the
	// leads, each saying what it is a rating of.
	first, viaEdge := ratings[1], ratings[8+4]
	if first != (Rating{Question: "slow-checkout", Target: cache, Potential: 1}) {
		t.Errorf("the second rating is %+v", first)
	}
	if viaEdge.Question != "which-change" || viaEdge.Target != slow || viaEdge.From != cache ||
		viaEdge.Edge.Triple() != set.Graph.Edges[0].Triple() || viaEdge.Potential != 0.5 || viaEdge.Seed() || !first.Seed() {
		t.Errorf("a rating of a lead reached by an edge is %+v", viaEdge)
	}

	// A judge that fails stops the measurement, and the error says where.
	judge.failFor = set.Questions[1].Text
	if _, err := Ratings(t.Context(), judge, set); !errors.Is(err, errJudge) || !strings.Contains(err.Error(), "which-change") {
		t.Errorf("with a failing judge Ratings returned %v", err)
	}
}

func TestSufficiencyPutsTheLabelledEvidenceToTheJudge(t *testing.T) {
	set := example(t)
	// One more question, answered by two documents that each state a fact,
	// and one whose only fact either of two documents states.
	set.Questions = append(set.Questions,
		Question{ID: "two-documents", Text: "What happened, and when?", Kind: Several, Part: Tune,
			Facts: []Fact{
				{Text: "Latency doubled.", Stated: []Stating{{Document: slow, Quote: "doubled"}}},
				{Text: "The cache was removed.", Stated: []Stating{{Document: cache, Quote: "removed"}}},
			},
			Near: []graph.ID{dashboard}},
		Question{ID: "either-document", Text: "What was it about?", Kind: OneDocument, Part: Tune,
			Facts: []Fact{{Text: "The checkout.", Stated: []Stating{{Document: slow, Quote: "Checkout"}, {Document: dashboard, Quote: "checkout"}}}}},
		// The first fact is stated by two documents, and the second of them
		// states the other fact as well: the answer is one document for
		// each fact, yet the first can be taken away and nothing is lost.
		Question{ID: "overlapping", Text: "What doubled, and why?", Kind: Several, Part: Tune,
			Facts: []Fact{
				{Text: "Latency doubled.", Stated: []Stating{{Document: slow, Quote: "doubled"}, {Document: cache, Quote: "3 March"}}},
				{Text: "The cache was removed.", Stated: []Stating{{Document: cache, Quote: "removed"}}},
			}},
		// One document states a needed fact and an extra one too. It is part
		// of the answer, not an extra.
		Question{ID: "needed-and-extra", Text: "What was removed, and what else?", Kind: OneDocument, Part: Tune,
			Facts: []Fact{
				{Text: "The cache was removed.", Stated: []Stating{{Document: cache, Quote: "removed"}}},
				{Text: "Prices come from the database.", Extra: true, Stated: []Stating{{Document: cache, Quote: "database"}}},
				{Text: "Latency doubled.", Extra: true, Stated: []Stating{{Document: slow, Quote: "doubled"}}},
			}},
	)
	judge := &scriptedJudge{weigh: func(_ retrieval.Question, evidence []graph.ID) float64 { return float64(len(evidence)) / 10 }}

	met := map[string][][]graph.ID{
		"slow-checkout": {{dashboard, cache}, {cache}}, // the second is a labelled set already
		"rota-approver": {{rota, dashboard}},
	}
	readings, err := Sufficiency(t.Context(), judge, set, met)
	if err != nil {
		t.Fatalf("Sufficiency: %v", err)
	}

	type row struct {
		question string
		why      Case
		evidence []graph.ID
		enough   bool
	}
	var got []row
	for _, reading := range readings {
		got = append(got, row{reading.Question, reading.Case, reading.Evidence, reading.Enough})
		if want := float64(len(reading.Evidence)) / 10; reading.Sufficient != want {
			t.Errorf("the reading of %v for %s is %v, want what the judge said, %v", reading.Evidence, reading.Question, reading.Sufficient, want)
		}
	}
	want := []row{
		{"slow-checkout", WholeAnswer, []graph.ID{cache}, true},
		{"slow-checkout", WithExtras, []graph.ID{cache, slow}, true},
		{"slow-checkout", WithNearMisses, []graph.ID{cache, dashboard}, true},
		{"slow-checkout", ExtrasOnly, []graph.ID{slow}, false},
		{"slow-checkout", NearMissesOnly, []graph.ID{dashboard}, false},
		{"slow-checkout", MetBySearch, []graph.ID{dashboard, cache}, true},

		// The fact is stated only on the link between the two documents:
		// the documents themselves are all a judge of evidence is shown,
		// and by the facts they are not enough.
		{"which-change", AllButTheNote, []graph.ID{cache, slow}, false},

		{"rota-approver", NearMissesOnly, []graph.ID{rota}, false},
		{"rota-approver", MetBySearch, []graph.ID{rota, dashboard}, false},

		{"two-documents", WholeAnswer, []graph.ID{slow, cache}, true},
		{"two-documents", WithNearMisses, []graph.ID{slow, cache, dashboard}, true},
		{"two-documents", LessOneDocument, []graph.ID{cache}, false},
		{"two-documents", LessOneDocument, []graph.ID{slow}, false},
		{"two-documents", NearMissesOnly, []graph.ID{dashboard}, false},

		// Either document states the one fact; the first named stands for
		// the answer, and there is nothing to take away from it.
		{"either-document", WholeAnswer, []graph.ID{slow}, true},

		// Without the cache document a fact is missing. Without the slow
		// one nothing is, so that set is not a test of anything and is not
		// read.
		{"overlapping", WholeAnswer, []graph.ID{slow, cache}, true},
		{"overlapping", LessOneDocument, []graph.ID{slow}, false},

		{"needed-and-extra", WholeAnswer, []graph.ID{cache}, true},
		{"needed-and-extra", WithExtras, []graph.ID{cache, slow}, true},
		{"needed-and-extra", ExtrasOnly, []graph.ID{slow}, false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the readings are of\n%+v\nwant\n%+v", got, want)
	}

	// Each set went to the judge on its own, with its documents in order
	// and no leads. Several are asked at once, so the askings come in no
	// particular order.
	calls := judge.asked("Why did checkout latency double in March?")
	if len(calls) != 6 {
		t.Fatalf("for the first question the judge was asked %d times, want 6", len(calls))
	}
	for _, call := range calls {
		if len(call.leads) != 0 {
			t.Errorf("a set of evidence went to the judge with %d leads", len(call.leads))
		}
	}
	if !slices.ContainsFunc(calls, func(call judgeCall) bool { return slices.Equal(call.evidence, []graph.ID{cache, slow}) }) ||
		slices.ContainsFunc(calls, func(call judgeCall) bool { return slices.Equal(call.evidence, []graph.ID{slow, cache}) }) {
		t.Errorf("the judge was not shown the documents in the order of the set: %+v", calls)
	}

	// A set met by a search can only hold documents of the graph.
	_, err = Sufficiency(t.Context(), judge, set, map[string][][]graph.ID{"slow-checkout": {{"doc:s:gone"}}})
	if err == nil || !strings.Contains(err.Error(), "doc:s:gone") {
		t.Errorf("evidence that is not in the graph was read: %v", err)
	}
	// Nor can there be sets for a question the test set does not have.
	before := len(judge.asked("Who approves the holiday rota?"))
	_, err = Sufficiency(t.Context(), judge, set, map[string][][]graph.ID{"no-such-question": {{cache}}})
	if err == nil || !strings.Contains(err.Error(), "no-such-question") {
		t.Errorf("sets of evidence for a question that is not in the set were passed over: %v", err)
	}
	if after := len(judge.asked("Who approves the holiday rota?")); after != before {
		t.Errorf("the judge was asked %d times before the mistake was found", after-before)
	}
	judge.failFor = "Who approves the holiday rota?"
	if _, err := Sufficiency(t.Context(), judge, set, nil); !errors.Is(err, errJudge) || !strings.Contains(err.Error(), "rota-approver") {
		t.Errorf("with a failing judge Sufficiency returned %v", err)
	}
}

// bench is a test set loaded into a store, with what a search needs.
func bench(t *testing.T, set Set, judge retrieval.Judge) retrieval.Config {
	t.Helper()
	return benchWith(t, set, judge, &countingEmbedder{space: "test model; 3 dimensions"})
}

func benchWith(t *testing.T, set Set, judge retrieval.Judge, embedder model.Embedder) retrieval.Config {
	t.Helper()
	store := memory.New()
	if err := Fill(t.Context(), set, store, memory.NewLedger(), embedder); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	return retrieval.Config{Store: store, Searcher: store, Neighbors: store, Embedder: embedder, Judge: judge}
}

// nearOne is an embedder for which every question is nearest the document
// whose text begins with a given word, and equally far from all the others.
type nearOne struct{ start string }

func (e nearOne) Space(context.Context) (string, error) { return "near one; 2 dimensions", nil }

func (e nearOne) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = []float32{0, 1}
		if strings.HasPrefix(text, e.start) {
			vectors[i] = []float32{1, 0}
		}
	}
	return vectors, nil
}

func (e nearOne) EmbedQueries(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range texts {
		vectors[i] = []float32{1, 0}
	}
	return vectors, nil
}

func TestFillLoadsTheFrozenGraphThroughTheSyncEngine(t *testing.T) {
	ctx := t.Context()
	set := example(t)
	store, ledger := memory.New(), memory.NewLedger()
	embedder := &countingEmbedder{space: "test model; 3 dimensions"}
	if err := Fill(ctx, set, store, ledger, embedder); err != nil {
		t.Fatalf("Fill: %v", err)
	}

	stored, err := store.GetVertices(ctx, []graph.ID{slow, cache, dashboard, rota, performance})
	if err != nil || len(stored) != 5 {
		t.Fatalf("the store holds %d of the 5 vertices, %v", len(stored), err)
	}
	document := stored[cache].(*graph.Document)
	if document.Title != "Remove the price cache" || document.Type != graph.TypePullRequest ||
		document.URL != "https://shop.test/pull/9" || !strings.HasPrefix(document.Summary, "The price cache") {
		t.Errorf("a document was stored as %+v", document)
	}
	// Embedded by the rule ingestion uses, title then summary, and the
	// graph knows which model its vectors are from.
	if len(document.Embedding) != 3 {
		t.Errorf("the document's embedding is %v", document.Embedding)
	}
	if !slices.Contains(embedder.documents, "Remove the price cache\n\nThe price cache was removed on 3 March to fix stale prices. Every price is now read from the database.") {
		t.Errorf("the texts embedded were %q", embedder.documents)
	}
	if space, _ := store.EmbeddingSpace(ctx); space != embedder.space {
		t.Errorf("the store's embedding space is %q", space)
	}

	out, err := store.Edges(ctx, storage.EdgeQuery{Vertex: cache, Direction: graph.Out})
	if err != nil || len(out.Edges) != 1 || out.Edges[0] != set.Graph.Edges[0] {
		t.Errorf("the edges out of the cache document are %+v, %v", out.Edges, err)
	}
	tags, _ := store.Edges(ctx, storage.EdgeQuery{Vertex: performance, Direction: graph.In})
	if len(tags.Edges) != 1 {
		t.Errorf("the topic has %d edges in", len(tags.Edges))
	}

	// Filling again changes nothing and embeds nothing.
	asked := len(embedder.documents)
	if err := Fill(ctx, set, store, ledger, embedder); err != nil {
		t.Fatalf("Fill, a second time: %v", err)
	}
	if len(embedder.documents) != asked {
		t.Errorf("filling a second time embedded %d more texts", len(embedder.documents)-asked)
	}
}

// decliningEmbedder refuses, for good, any text that begins with a given
// word, as a model does with a text it cannot take.
type decliningEmbedder struct {
	countingEmbedder
	refuses string
}

func (e *decliningEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	for _, text := range texts {
		if strings.HasPrefix(text, e.refuses) {
			return nil, fmt.Errorf("the text is too long: %w", model.ErrDeclined)
		}
	}
	return e.countingEmbedder.EmbedDocuments(ctx, texts)
}

// A document stored without its embedding would never be found by a search,
// and the measurements would be of a graph with a hole in it.
func TestFillFailsIfADocumentCannotBeEmbedded(t *testing.T) {
	set := example(t)
	embedder := &decliningEmbedder{countingEmbedder: countingEmbedder{space: "test model"}, refuses: "Holiday rota"}
	err := Fill(t.Context(), set, memory.New(), memory.NewLedger(), embedder)
	if err == nil || !strings.Contains(err.Error(), "could not be embedded") {
		t.Errorf("Fill with a document the model declined returned %v", err)
	}
}

func TestSearchesRunTheRealSearchAndSayWhatItCameTo(t *testing.T) {
	set := example(t)
	judge := &scriptedJudge{
		rate: func(q retrieval.Question, lead retrieval.Lead) float64 {
			if strings.HasPrefix(q.Text, "Who approves") {
				return 0.3 // everything is on the subject, nothing answers
			}
			return byTarget(map[graph.ID]float64{cache: 0.9, slow: 0.5, dashboard: 0.4, rota: 0.1, performance: 0.3})(q, lead)
		},
		weigh: func(_ retrieval.Question, evidence []graph.ID) float64 {
			if slices.Contains(evidence, cache) {
				return 0.9
			}
			return 0.2
		},
	}

	searches, err := Searches(t.Context(), bench(t, set, judge), set)
	if err != nil {
		t.Fatalf("Searches: %v", err)
	}
	if len(searches) != 3 || searches[0].Question != "slow-checkout" || searches[2].Question != "rota-approver" {
		t.Fatalf("the searches are %+v", searches)
	}

	// The first question. Step one: the four documents are rated, and the
	// three worth following are followed, of which the cache document is
	// evidence. Step two: the topic, a new lead, is rated, the evidence is
	// weighed, and it is enough.
	answered := searches[0]
	if answered.Verdict != "sufficient" || answered.Sufficient != 0.9 || answered.Steps != 2 || answered.Err != "" {
		t.Errorf("the first search ended %+v", answered)
	}
	if want := []Found{{Document: cache, Potential: 0.9}}; !reflect.DeepEqual(answered.Evidence, want) {
		t.Errorf("the first search returned %+v, want %+v", answered.Evidence, want)
	}
	if want := (Asked{Times: 2, Leads: 5, Readings: 1}); answered.Asked != want {
		t.Errorf("the first search asked %+v of the judge, want %+v", answered.Asked, want)
	}
	if !slices.Equal(answered.Followed, []graph.ID{cache, slow, dashboard}) {
		t.Errorf("the first search followed %v", answered.Followed)
	}
	// Every lead it put to the judge, with what the judge said.
	if len(answered.Met) != 5 || answered.Met[4].Target != performance || answered.Met[4].From != slow || answered.Met[4].Potential != 0.3 {
		t.Errorf("the first search met %+v", answered.Met)
	}

	// The last question: everything is followed and nothing is evidence, so
	// the search runs out of leads.
	unanswered := searches[2]
	if unanswered.Verdict != "exhausted" || len(unanswered.Evidence) != 0 || unanswered.Asked.Readings != 0 {
		t.Errorf("the search with no answer ended %+v", unanswered)
	}
}

func TestSearchesSayWhichSearchFailed(t *testing.T) {
	set := example(t)
	judge := &scriptedJudge{failFor: "Which change made the checkout slow?"}
	searches, err := Searches(t.Context(), bench(t, set, judge), set)
	if !errors.Is(err, errJudge) || !strings.Contains(err.Error(), "which-change") {
		t.Fatalf("Searches returned %v, want the judge's error naming the question", err)
	}
	// The others were still searched for, and the one that failed says so.
	if len(searches) != 3 || searches[1].Err == "" || searches[0].Err != "" || searches[0].Verdict != "exhausted" {
		t.Errorf("the searches are %+v", searches)
	}
}

func TestEvidenceReachedByALinkIsMarkedSo(t *testing.T) {
	set, err := example(t).Only("slow-checkout")
	if err != nil {
		t.Fatal(err)
	}
	// The search starts from the slow document alone. It is on the subject
	// and no more; the cache document, which is evidence, can only be
	// reached through the link between the two.
	judge := &scriptedJudge{rate: byTarget(map[graph.ID]float64{slow: 0.6, cache: 0.9})}
	cfg := benchWith(t, set, judge, nearOne{start: "Checkout is slow"})
	cfg.Seeds = 1

	searches, err := Searches(t.Context(), cfg, set)
	if err != nil {
		t.Fatal(err)
	}
	if want := []Found{{Document: cache, Potential: 0.9, ByLink: true}}; !reflect.DeepEqual(searches[0].Evidence, want) {
		t.Errorf("the search returned %+v, want %+v", searches[0].Evidence, want)
	}
	// What it met: the one place to start, then that document's neighbours.
	met := searches[0].Met
	if len(met) != 3 || !met[0].Seed() || met[0].Target != slow || met[1].Seed() || met[1].From != slow {
		t.Errorf("the search met %+v", met)
	}
}

func TestArrivalsAreTheEvidenceASearchComesToWeigh(t *testing.T) {
	set, err := example(t).Only("slow-checkout")
	if err != nil {
		t.Fatal(err)
	}
	judge := &scriptedJudge{
		rate: byTarget(map[graph.ID]float64{cache: 0.9, slow: 0.8, dashboard: 0.4, rota: 0.1, performance: 0.3}),
		// A judge that would stop the search at once, if it were asked.
		weigh: func(retrieval.Question, []graph.ID) float64 { return 1 },
	}
	cfg := bench(t, set, judge)

	// Following one lead at a time, the evidence grows a document at a
	// time, and each state it passes through is one a search could have
	// stopped at.
	cfg.Follow = 1
	arrivals, err := Arrivals(t.Context(), cfg, set)
	if err != nil {
		t.Fatalf("Arrivals: %v", err)
	}
	want := map[string][][]graph.ID{"slow-checkout": {{cache}, {cache, slow}}}
	if !reflect.DeepEqual(arrivals, want) {
		t.Errorf("the arrivals are %v, want %v", arrivals, want)
	}

	// The judge was never asked to weigh anything: finding out what there
	// is to weigh must not cost a reading, nor let one end the search.
	for _, call := range judge.asked(set.Questions[0].Text) {
		if len(call.evidence) != 0 {
			t.Errorf("the judge was asked to weigh %v", call.evidence)
		}
	}

	// Not even a judge that calls the evidence enough without being shown
	// any ends the search early.
	cfg.Judge = sureJudge{judge}
	if arrivals, err = Arrivals(t.Context(), cfg, set); err != nil || !reflect.DeepEqual(arrivals, want) {
		t.Errorf("with a judge that is always sure, the arrivals are %v, %v; want %v", arrivals, err, want)
	}

	// Here the evidence stops growing at one document, and the search goes
	// on to rate a new lead with that same evidence in hand: one state of
	// the evidence, weighed twice, and listed once.
	judge.rate = byTarget(map[graph.ID]float64{cache: 0.9, slow: 0.5, performance: 0.3})
	cfg.Judge = judge
	arrivals, err = Arrivals(t.Context(), cfg, set)
	if want := map[string][][]graph.ID{"slow-checkout": {{cache}}}; err != nil || !reflect.DeepEqual(arrivals, want) {
		t.Errorf("the arrivals are %v, %v; want %v", arrivals, err, want)
	}
}

// Finding what a search arrives at must go all the way, whatever the judge
// says of the evidence. Here the search starts from the cache document alone
// and finds the slow one through the link: when that new lead is rated, the
// evidence is one document, and a second is still to come.
func TestArrivalsGoOnPastWhereASearchWouldStop(t *testing.T) {
	set, err := example(t).Only("slow-checkout")
	if err != nil {
		t.Fatal(err)
	}
	rating := &scriptedJudge{rate: byTarget(map[graph.ID]float64{cache: 0.9, slow: 0.8, performance: 0.3})}
	cfg := benchWith(t, set, sureJudge{rating}, nearOne{start: "Remove the price cache"})
	cfg.Seeds, cfg.Follow = 1, 1

	arrivals, err := Arrivals(t.Context(), cfg, set)
	want := map[string][][]graph.ID{"slow-checkout": {{cache}, {cache, slow}}}
	if err != nil || !reflect.DeepEqual(arrivals, want) {
		t.Errorf("the arrivals are %v, %v; want %v", arrivals, err, want)
	}
}

// sureJudge has another judge do the rating, and then says that the evidence
// is enough, whatever it is and even if there is none.
type sureJudge struct{ inner retrieval.Judge }

func (j sureJudge) Assess(ctx context.Context, q retrieval.Question, evidence []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	assessment, err := j.inner.Assess(ctx, q, evidence, leads)
	assessment.Sufficient = 1
	return assessment, err
}

func TestSettingsAroundTheDefaults(t *testing.T) {
	around := Around(DefaultSettings, 0.1)
	if len(around) != 27 || !slices.Contains(around, DefaultSettings) {
		t.Fatalf("there are %d settings around the defaults, and the defaults among them: %v", len(around), slices.Contains(around, DefaultSettings))
	}
	if !slices.ContainsFunc(around, func(s Settings) bool {
		return near(s.Sufficient, 0.6) && near(s.MinPotential, 0.35) && near(s.MinEvidence, 0.85)
	}) {
		t.Errorf("the settings do not reach one step to each side: %v", around)
	}
	cfg := Settings{Sufficient: 0.6, MinPotential: 0.15, MinEvidence: 0.65}.Apply(retrieval.Config{Seeds: 5})
	if cfg.Sufficient != 0.6 || cfg.MinPotential != 0.15 || cfg.MinEvidence != 0.65 || cfg.Seeds != 5 {
		t.Errorf("the settings were applied as %+v", cfg)
	}
	// The paths a search can take depend on two of the three thresholds.
	if paths := Paths(around); len(paths) != 9 {
		t.Errorf("27 settings give %d different paths, want 9", len(paths))
	}
}

func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// The heart of the matter: what the measurements record is exactly what a
// search asks. A search replayed from the tape, under the real judge, never
// comes across a question that the ratings and the readings did not put
// there.
func TestASearchReplaysFromWhatTheMeasurementsRecorded(t *testing.T) {
	ctx := t.Context()
	set := example(t)
	api := newFakeJev(t)
	// A Jev that judges by what it is shown, the same way every time.
	api.answer = func(raw json.RawMessage) any {
		var question struct {
			Type         string `json:"type"`
			Instructions struct {
				Candidate struct {
					Title string `json:"title"`
					Name  string `json:"name"`
				} `json:"candidate"`
				Evidence []struct {
					Title string `json:"title"`
				} `json:"evidence"`
			} `json:"instructions"`
		}
		if err := json.Unmarshal(raw, &question); err != nil {
			t.Errorf("a question is not JSON: %v", err)
		}
		if question.Type == "noul" {
			for _, document := range question.Instructions.Evidence {
				if document.Title == "Remove the price cache" {
					return map[string]any{"type": "noul", "noul": 0.9}
				}
			}
			return map[string]any{"type": "noul", "noul": 0.2}
		}
		score := map[string]float64{"Remove the price cache": 1.9, "Checkout is slow": 1.6, "Latency dashboard": 0.9}[question.Instructions.Candidate.Title]
		return map[string]any{"type": "score", "score": score}
	}

	tape, _ := openTape(t)
	recording := jev.New(jev.Config{Client: api.client(tape.Record(1, http.DefaultTransport)), Model: "jev-test"})
	replay := tape.Replay(1)
	replaying := jev.New(jev.Config{Client: offline(replay), Model: "jev-test"})

	// Record: every lead for every question.
	if _, err := Ratings(ctx, recording, set); err != nil {
		t.Fatalf("recording the ratings: %v", err)
	}
	sent := len(api.asked())

	// From the ratings alone, replayed, every set of evidence a search
	// could come to weigh, under each setting.
	cfg := bench(t, set, replaying)
	settings := Around(DefaultSettings, 0.1)
	met := make(map[string][][]graph.ID)
	for _, path := range Paths(settings) {
		arrivals, err := Arrivals(ctx, path.Apply(cfg), set)
		if err != nil {
			t.Fatalf("finding what searches arrive at, from the tape: %v", err)
		}
		for id, sets := range arrivals {
			met[id] = append(met[id], sets...)
		}
	}
	if len(met["slow-checkout"]) == 0 {
		t.Fatalf("no search arrived at any evidence: %v", met)
	}
	if got := len(api.asked()); got != sent {
		t.Fatalf("finding what searches arrive at sent %d requests to the API", got-sent)
	}

	// Record: the labelled sets, and the ones the searches arrive at.
	if _, err := Sufficiency(ctx, recording, set, met); err != nil {
		t.Fatalf("recording the readings: %v", err)
	}
	sent = len(api.asked())

	// Now any search, under any of the settings, replays in full.
	for _, setting := range settings {
		searches, err := Searches(ctx, setting.Apply(cfg), set)
		if err != nil {
			t.Fatalf("a search under %+v did not replay: %v", setting, err)
		}
		if setting == DefaultSettings && (searches[0].Verdict != "sufficient" || len(searches[0].Evidence) == 0) {
			t.Errorf("under the defaults the first search came to %+v", searches[0])
		}
	}
	if replay.Missed() != 0 || len(api.asked()) != sent {
		t.Errorf("replaying missed %d questions and sent %d requests", replay.Missed(), len(api.asked())-sent)
	}

	// And a second pass over the measurements themselves costs nothing.
	if _, err := Ratings(ctx, recording, set); err != nil {
		t.Fatal(err)
	}
	if _, err := Sufficiency(ctx, recording, set, met); err != nil {
		t.Fatal(err)
	}
	if got := len(api.asked()); got != sent {
		t.Errorf("measuring again sent %d more requests", got-sent)
	}
}

var _ = typesafe.DefaultModel // the fake API speaks the client's protocol

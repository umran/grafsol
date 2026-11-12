package retrieval_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
	"github.com/umran/grafsol/traverse"
)

// --- the search, end to end ---------------------------------------------------

func TestAnAnswerTwoHopsAwayIsFoundWithThePathToIt(t *testing.T) {
	w := newWorld(t)
	slow := w.document("slow", "Checkout is slow", near(0))
	notes := w.document("notes", "Latency investigation notes", near(40))
	cause := w.document("cause", "The cache was removed in March", near(50))
	other := w.document("other", "Holiday rota", near(80))
	billing := w.topic("billing", near(70))
	w.edge(slow, graph.LinksTo, notes, "investigated in the notes")
	w.edge(notes, graph.LinksTo, cause, "root cause")
	w.edge(slow, graph.LinksTo, other, "")
	w.edge(slow, graph.TaggedWith, billing, "")

	// The first two documents are about the question without answering it;
	// only the third states the answer.
	judge := &scriptedJudge{
		ratings: map[graph.ID]float64{slow: 0.5, notes: 0.5, cause: 1},
		enough:  func(evidence []retrieval.Evidence) float64 { return float64(len(evidence)) },
	}
	engine := w.engine(judge, retrieval.Config{Seeds: 1})

	findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "Why is checkout slow?", Context: "A performance review."})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if findings.Verdict != retrieval.Sufficient || findings.Sufficient != 1 {
		t.Fatalf("the search ended %s with a sufficiency of %v, want sufficient at 1", findings.Verdict, findings.Sufficient)
	}

	// Only the document that states the answer is evidence. The two on the
	// way to it are in its path, not beside it.
	if len(findings.Evidence) != 1 {
		t.Fatalf("the evidence is %s, want only %s", evidenceIDs(findings), cause)
	}
	evidence := findings.Evidence[0]
	if evidence.Document.ID != cause || evidence.Potential != 1 || evidence.Document.Embedding != nil {
		t.Errorf("the evidence is %+v at %v", evidence.Document, evidence.Potential)
	}
	wantPath := []graph.Edge{
		{From: slow, Kind: graph.LinksTo, To: notes, Summary: "investigated in the notes"},
		{From: notes, Kind: graph.LinksTo, To: cause, Summary: "root cause"},
	}
	if !slices.Equal(evidence.Path, wantPath) {
		t.Errorf("the path to the evidence is\n  %+v\nwant\n  %+v", evidence.Path, wantPath)
	}

	// The trace, step by step: what was judged, and what was visited.
	requireTrace(t, findings,
		step{judged: ids(slow), followed: ids(slow)},
		step{judged: ids(notes, billing, other), followed: ids(notes)},
		step{judged: ids(cause), followed: ids(cause)},
		step{sufficient: 1}, // the evidence weighed, with nothing new to judge
	)
	// The judge was shown the question as asked, and each lead with the
	// edge and the vertex it was reached from.
	if got := judge.calls[0].question; got.Text != "Why is checkout slow?" || got.Context != "A performance review." {
		t.Errorf("the judge was given the question %+v", got)
	}
	reached := findings.Trace[1].Judged[0].Lead
	if reached.Seed() || reached.Edge != wantPath[0] || reached.From.VertexID() != slow {
		t.Errorf("the lead to the notes was put to the judge as %+v", reached)
	}
	if seed := findings.Trace[0].Judged[0].Lead; !seed.Seed() || seed.From != nil {
		t.Errorf("the seed was put to the judge as %+v", seed)
	}
	if findings.Left != (retrieval.Left{}) {
		t.Errorf("the search left %+v behind", findings.Left)
	}
}

func TestASearchWithNothingLeftToFollowIsExhausted(t *testing.T) {
	w := newWorld(t)
	a := w.document("a", "On the subject", near(0))
	b := w.document("b", "Also on the subject", near(10))
	c := w.document("c", "States part of the answer", near(20))
	d := w.document("d", "Unrelated", near(30))
	w.edge(a, graph.LinksTo, b, "")
	w.edge(b, graph.LinksTo, c, "")
	w.edge(c, graph.LinksTo, d, "")

	judge := &scriptedJudge{ratings: map[graph.ID]float64{a: 0.5, b: 0.6, c: 0.9, d: 0.1}}
	findings, err := w.engine(judge, retrieval.Config{Seeds: 1}).Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	if findings.Verdict != retrieval.Exhausted || findings.Reason != "" {
		t.Errorf("the search ended %s (%q), want exhausted", findings.Verdict, findings.Reason)
	}
	// c is evidence; a and b were only passed through; d was declined.
	if got := evidenceIDs(findings); !slices.Equal(got, ids(c)) {
		t.Errorf("the evidence is %s, want %s", got, ids(c))
	}
	if findings.Left != (retrieval.Left{}) {
		t.Errorf("an exhausted search left %+v behind", findings.Left)
	}
	requireTrace(t, findings,
		step{judged: ids(a), followed: ids(a)},
		step{judged: ids(b), followed: ids(b)},
		step{judged: ids(c), followed: ids(c)},
		step{judged: ids(d)}, // judged with the new evidence, and declined
	)
	if last := findings.Trace[3].Judged[0]; last.Potential != 0.1 {
		t.Errorf("the declined lead is in the trace with a rating of %v, want 0.1", last.Potential)
	}
}

func TestTheJudgesLastWordIsInTheTraceWhenNothingComesOfIt(t *testing.T) {
	w := newWorld(t)
	a := w.document("a", "States part of the answer", near(0))

	judge := &scriptedJudge{
		ratings: map[graph.ID]float64{a: 1},
		enough:  func([]retrieval.Evidence) float64 { return 0.4 },
	}
	findings, err := w.engine(judge, retrieval.Config{}).Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	// One document, which is evidence but not enough. The judge is asked
	// about it once more after it is admitted, and that reading is kept.
	if findings.Verdict != retrieval.Exhausted || findings.Sufficient != 0.4 {
		t.Errorf("the search ended %s with a sufficiency of %v", findings.Verdict, findings.Sufficient)
	}
	requireTrace(t, findings,
		step{judged: ids(a), followed: ids(a)},
		step{sufficient: 0.4},
	)
}

func TestSufficiencyNeedsEvidence(t *testing.T) {
	w := newWorld(t)
	a := w.document("a", "On the subject only", near(0))

	// A judge that calls the evidence sufficient when there is none.
	judge := &scriptedJudge{
		ratings: map[graph.ID]float64{a: 0.5},
		enough:  func([]retrieval.Evidence) float64 { return 1 },
	}
	findings, err := w.engine(judge, retrieval.Config{}).Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if findings.Verdict != retrieval.Exhausted || len(findings.Evidence) != 0 {
		t.Errorf("with no evidence the search ended %s", findings.Verdict)
	}
}

// --- limits -------------------------------------------------------------------

func TestTheStepLimitStopsASearchAndSaysWhatWasLeft(t *testing.T) {
	w := newWorld(t)
	chain := make([]graph.ID, 6)
	for i := range chain {
		chain[i] = w.document(fmt.Sprintf("d%d", i), "On the subject", near(float64(i)))
		if i > 0 {
			w.edge(chain[i-1], graph.LinksTo, chain[i], "")
		}
	}
	// Two more hang off the first document; they will be judged worth
	// following and never reached.
	extra1 := w.document("x1", "Also promising", near(60))
	extra2 := w.document("x2", "Also promising", near(61))
	w.edge(chain[0], graph.LinksTo, extra1, "")
	w.edge(chain[0], graph.LinksTo, extra2, "")

	judge := &scriptedJudge{rate: func(lead retrieval.Lead) float64 {
		if strings.HasPrefix(lead.Target.VertexID().Key(), "s:x") {
			return 0.3
		}
		return 0.6
	}}
	engine := w.engine(judge, retrieval.Config{Seeds: 1, Follow: 1, MaxSteps: 3})

	findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if findings.Verdict != retrieval.Stopped || !strings.Contains(findings.Reason, "3 steps") {
		t.Errorf("the search ended %s (%q), want stopped at the step limit", findings.Verdict, findings.Reason)
	}
	if len(findings.Trace) != 3 || len(judge.calls) != 3 {
		t.Errorf("the search took %d steps and asked the judge %d times, want 3 of each", len(findings.Trace), len(judge.calls))
	}
	// Followed: d0, d1, d2. Judged and waiting: x1, x2. Found by the last
	// visit and not yet judged: d3.
	if want := (retrieval.Left{Unjudged: 1, Unfollowed: 2}); findings.Left != want {
		t.Errorf("the search left %+v, want %+v", findings.Left, want)
	}
}

func TestTheTimeLimitStopsASearchWithoutAnError(t *testing.T) {
	w := newWorld(t)
	previous := w.document("d0", "On the subject", near(0))
	for i := 1; i < 50; i++ {
		next := w.document(fmt.Sprintf("d%d", i), "On the subject", near(float64(i)))
		w.edge(previous, graph.LinksTo, next, "")
		previous = next
	}
	judge := &scriptedJudge{rate: func(retrieval.Lead) float64 { return 0.6 }, delay: 20 * time.Millisecond}
	engine := w.engine(judge, retrieval.Config{Seeds: 1, MaxSteps: -1, MaxDuration: 150 * time.Millisecond})

	started := time.Now()
	findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather returned %v; running out of time is a verdict, not an error", err)
	}
	if findings.Verdict != retrieval.Stopped || !strings.Contains(findings.Reason, "time limit") {
		t.Errorf("the search ended %s (%q), want stopped at the time limit", findings.Verdict, findings.Reason)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Errorf("a search limited to 150ms took %v", elapsed)
	}
	if len(findings.Trace) == 0 || len(findings.Trace) >= 49 {
		t.Errorf("the search took %d steps in its 150ms", len(findings.Trace))
	}
}

func TestCancellingReturnsWhatWasGatheredAlongWithTheError(t *testing.T) {
	w := newWorld(t)
	a := w.document("a", "States part of the answer", near(0))
	b := w.document("b", "On the subject", near(10))
	w.edge(a, graph.LinksTo, b, "")

	ctx, cancel := context.WithCancel(t.Context())
	judge := &scriptedJudge{ratings: map[graph.ID]float64{a: 1, b: 0.5}}
	judge.before = func(call int) error {
		if call == 2 {
			cancel() // the caller gives up during the second assessment
			return ctx.Err()
		}
		return nil
	}

	findings, err := w.engine(judge, retrieval.Config{Seeds: 1}).Gather(ctx, retrieval.Question{Text: "?"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Gather returned %v, want the context's error", err)
	}
	if findings == nil || findings.Verdict != retrieval.Stopped || !slices.Equal(evidenceIDs(findings), ids(a)) {
		t.Fatalf("the findings so far are %+v, want the evidence gathered before the caller gave up", findings)
	}
	// The lead that was about to be judged is still counted.
	if findings.Left.Unjudged != 1 {
		t.Errorf("the search left %+v, want the one lead it had not judged", findings.Left)
	}
}

// --- anchors and seeds --------------------------------------------------------

func TestAnchorsAreVisitedWhateverTheJudgeMakesOfThem(t *testing.T) {
	w := newWorld(t)
	start := w.document("start", "Somewhere to start", nil)
	hub := w.topic("performance", nil)
	answer := w.document("answer", "States the answer", nil)
	w.edge(start, graph.LinksTo, answer, "explained here")
	w.edge(answer, graph.InTopic, hub, "")

	// The judge thinks nothing of either anchor.
	judge := &scriptedJudge{ratings: map[graph.ID]float64{start: 0, hub: 0, answer: 1}}
	// No searcher and no embedder: the anchors are all the search has.
	engine := retrieval.NewEngine(retrieval.Config{Store: w.store, Judge: judge})

	findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "?", Anchors: ids(start, hub)})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	// Both anchors were visited, in spite of their ratings, and neither is
	// evidence; the document found through them is.
	if got := findings.Trace[0].Followed; !slices.Equal(got, ids(start, hub)) {
		t.Errorf("the first step visited %s, want both anchors", got)
	}
	if got := evidenceIDs(findings); !slices.Equal(got, ids(answer)) {
		t.Errorf("the evidence is %s, want %s", got, ids(answer))
	}
	if path := findings.Evidence[0].Path; len(path) != 1 || path[0].Summary != "explained here" {
		t.Errorf("the path to the answer is %+v", path)
	}

	// An anchor that the judge does rate highly is evidence like any other.
	judge = &scriptedJudge{ratings: map[graph.ID]float64{start: 0.9}}
	findings, err = retrieval.NewEngine(retrieval.Config{Store: w.store, Judge: judge}).
		Gather(t.Context(), retrieval.Question{Text: "?", Anchors: ids(start)})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if got := evidenceIDs(findings); !slices.Equal(got, ids(start)) || len(findings.Evidence[0].Path) != 0 {
		t.Errorf("a highly rated anchor gave the evidence %s", got)
	}
}

func TestASearchNeedsSomewhereToStart(t *testing.T) {
	w := newWorld(t)
	w.document("a", "A document", near(0))
	judge := &scriptedJudge{}

	// No anchors, and no way to look for a starting point.
	engine := retrieval.NewEngine(retrieval.Config{Store: w.store, Judge: judge})
	if _, err := engine.Gather(t.Context(), retrieval.Question{Text: "?"}); !errors.Is(err, retrieval.ErrNowhereToStart) {
		t.Errorf("Gather returned %v, want ErrNowhereToStart", err)
	}
	// An anchor that is not in the graph.
	if _, err := engine.Gather(t.Context(), retrieval.Question{Text: "?", Anchors: ids("doc:s:missing")}); err == nil || !strings.Contains(err.Error(), "doc:s:missing") {
		t.Errorf("Gather with a missing anchor returned %v", err)
	}
	// A question with nothing in it, or with a malformed anchor.
	for _, q := range []retrieval.Question{{Text: "  "}, {Text: "?", Anchors: ids("nonsense")}} {
		if _, err := w.engine(judge, retrieval.Config{}).Gather(t.Context(), q); err == nil {
			t.Errorf("the question %+v was accepted", q)
		}
	}
	if len(judge.calls) != 0 {
		t.Errorf("the judge was asked %d times about questions that could not be searched for", len(judge.calls))
	}

	// A graph with nothing near the question is not an error: there is
	// simply nothing to find.
	empty := newWorld(t)
	findings, err := empty.engine(judge, retrieval.Config{}).Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil || findings.Verdict != retrieval.Exhausted || len(findings.Trace) != 0 {
		t.Errorf("searching an empty graph gave %+v, %v", findings, err)
	}
}

func TestSeedsAreTheNearestDocumentsNearestFirst(t *testing.T) {
	w := newWorld(t)
	for i := range 10 {
		w.document(fmt.Sprintf("d%d", i), "A document", near(float64(9-i)*10))
	}
	w.document("unembedded", "Never embedded", nil)
	w.topic("close", near(0)) // a topic is not a place to start, however close

	judge := &scriptedJudge{}
	findings, err := w.engine(judge, retrieval.Config{Seeds: 4}).Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	judged := findings.Trace[0].Judged
	if got := targets(judged); !slices.Equal(got, ids("doc:s:d9", "doc:s:d8", "doc:s:d7", "doc:s:d6")) {
		t.Errorf("the seeds were %s, want the four nearest documents, nearest first", got)
	}
	// 0 degrees from the question is an estimate of 1; 30 degrees is less.
	if first, last := judged[0].Lead.Estimate, judged[3].Lead.Estimate; math.Abs(first-1) > 1e-6 || last >= first || last < 0.9 {
		t.Errorf("the seeds' estimates run from %v to %v", first, last)
	}
}

// --- what the frontier guarantees ----------------------------------------------

func TestNoVertexIsJudgedOrVisitedTwice(t *testing.T) {
	w := newWorld(t)
	// A tangle: every document links to the next two and back to the first,
	// and all of them share a topic.
	const count = 12
	docs := make([]graph.ID, count)
	for i := range docs {
		docs[i] = w.document(fmt.Sprintf("d%02d", i), "On the subject", near(float64(i)))
	}
	shared := w.topic("shared", near(5))
	for i, id := range docs {
		w.edge(id, graph.LinksTo, docs[(i+1)%count], "next")
		w.edge(id, graph.LinksTo, docs[(i+2)%count], "next but one")
		w.edge(id, graph.ChildOf, docs[0], "back to the start")
		w.edge(id, graph.TaggedWith, shared, "")
	}

	judge := &scriptedJudge{rate: func(retrieval.Lead) float64 { return 0.9 }}
	findings, err := w.engine(judge, retrieval.Config{Seeds: 1, MaxSteps: -1}).Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	var judged, followed []graph.ID
	for _, step := range findings.Trace {
		judged = append(judged, targets(step.Judged)...)
		followed = append(followed, step.Followed...)
	}
	all := append(slices.Clone(docs), shared)
	for name, got := range map[string][]graph.ID{"judged": judged, "visited": followed} {
		slices.Sort(got)
		if want := slices.Sorted(slices.Values(all)); !slices.Equal(got, want) {
			t.Errorf("the vertices %s were\n  %s\nwant each of the %d exactly once", name, got, len(want))
		}
	}
	// Every document is evidence once; the topic, though visited, is not.
	if got := slices.Sorted(slices.Values(evidenceIDs(findings))); !slices.Equal(got, docs) {
		t.Errorf("the evidence is %s", got)
	}
}

func TestLeadsAreJudgedBestFirstAcrossHalfReadNeighbourhoods(t *testing.T) {
	w := newWorld(t)
	// Two anchors with a dozen neighbours each. Their neighbours' estimates
	// interleave: the first anchor has the even angles, the second the odd.
	first := w.document("first", "An anchor", nil)
	second := w.document("second", "An anchor", nil)
	for i := range 24 {
		id := w.document(fmt.Sprintf("n%02d", i), "A neighbour", near(float64(i)*3))
		w.edge([]graph.ID{first, second}[i%2], graph.LinksTo, id, "")
	}

	// Nothing but the anchors is followed, so the only leads are their
	// neighbours, which arrive four to a page.
	judge := &scriptedJudge{}
	searcher := &countingNeighbors{inner: w.store}
	engine := w.anchored(judge, retrieval.Config{Neighbors: searcher, Batch: 4, MaxSteps: -1})
	findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "?", Anchors: ids(first, second)})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	// Leaving out the first step, which judges the anchors, every lead the
	// judge saw must have been at least as promising as every lead it saw
	// later, although each neighbourhood was only ever read a page at a
	// time.
	var estimates []float64
	for _, step := range findings.Trace[1:] {
		for _, judgment := range step.Judged {
			estimates = append(estimates, judgment.Lead.Estimate)
		}
	}
	if len(estimates) != 24 {
		t.Fatalf("%d neighbours were judged, want 24", len(estimates))
	}
	if !slices.IsSortedFunc(estimates, func(a, b float64) int { return -cmpFloat(a, b) }) {
		t.Errorf("the leads were not judged best first: %v", estimates)
	}
	if searcher.largest > 4 {
		t.Errorf("a page of %d neighbours was asked for; the batch size is 4", searcher.largest)
	}
}

func TestAHubIsReadOnlyAsFarAsTheSearchGoes(t *testing.T) {
	w := newWorld(t)
	start := w.document("start", "Where the search starts", near(0))
	hub := w.topic("everything", near(1))
	w.edge(start, graph.InTopic, hub, "")

	// 5,000 documents in one topic. One of them states the answer; it is
	// the nearest to the question but for a handful.
	const size = 5000
	var answer graph.ID
	for i := range size {
		angle := 2 + float64(i)*80/size
		id := w.document(fmt.Sprintf("m%04d", i), "A member of the topic", near(angle))
		w.edge(id, graph.InTopic, hub, "")
		if i == 30 {
			answer = id
		}
	}

	judge := &scriptedJudge{
		ratings: map[graph.ID]float64{start: 0.5, hub: 0.5, answer: 1},
		enough:  func(evidence []retrieval.Evidence) float64 { return float64(len(evidence)) },
	}
	searcher := &countingNeighbors{inner: w.store}
	engine := w.engine(judge, retrieval.Config{Neighbors: searcher, Seeds: 1, Batch: 50, MaxSteps: -1})

	findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if findings.Verdict != retrieval.Sufficient || !slices.Equal(evidenceIDs(findings), ids(answer)) {
		t.Fatalf("the search ended %s with evidence %s", findings.Verdict, evidenceIDs(findings))
	}
	// The answer was on the hub's first page. The search read a few pages
	// to keep its batches full, not the hub, and says that the hub has more.
	if searcher.read > 4*50 {
		t.Errorf("%d neighbours were read to find an answer on the hub's first page", searcher.read)
	}
	if findings.Left.Unread != 1 {
		t.Errorf("the search left %+v, want one vertex with neighbours unread", findings.Left)
	}

	// Asked to go on to the end, the search reads the whole hub, a page at
	// a time, and judges every member exactly once.
	judge = &scriptedJudge{ratings: map[graph.ID]float64{start: 0.5, hub: 0.5}}
	searcher = &countingNeighbors{inner: w.store}
	engine = w.engine(judge, retrieval.Config{Neighbors: searcher, Seeds: 1, Batch: 50, MaxSteps: -1})
	findings, err = engine.Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	judged := 0
	seen := make(map[graph.ID]bool)
	for _, step := range findings.Trace {
		for _, id := range targets(step.Judged) {
			if seen[id] {
				t.Fatalf("%s was judged twice", id)
			}
			seen[id] = true
			judged++
		}
	}
	if findings.Verdict != retrieval.Exhausted || judged != size+2 || findings.Left != (retrieval.Left{}) {
		t.Errorf("the search ended %s having judged %d of %d vertices, leaving %+v", findings.Verdict, judged, size+2, findings.Left)
	}
	if searcher.largest > 50 {
		t.Errorf("a page of %d neighbours was asked for; the batch size is 50", searcher.largest)
	}
}

// With a judge that follows every lead and is never satisfied, a search must
// reach exactly what a plain traversal reaches. The traversal knows nothing
// of leads, batches or pages, so it makes an independent check that the
// search loses nothing on the way.
func TestAnUnsatisfiedSearchReachesEverythingATraversalDoes(t *testing.T) {
	for seed := range uint64(20) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			random := rand.New(rand.NewPCG(seed, 11))
			w := newWorld(t)

			// A random graph in a few disconnected pieces.
			const documents, topics = 120, 6
			var docs, all []graph.ID
			for i := range documents {
				docs = append(docs, w.document(fmt.Sprintf("d%03d", i), "A document", near(random.Float64()*90)))
			}
			all = append(all, docs...)
			for i := range topics {
				all = append(all, w.topic(fmt.Sprintf("topic-%d", i), near(random.Float64()*90)))
			}
			piece := func(id graph.ID) int { return slices.Index(all, id) % 3 }
			for range 260 {
				from, to := docs[random.IntN(documents)], all[random.IntN(len(all))]
				if from == to || piece(from) != piece(to) {
					continue
				}
				kind := graph.LinksTo
				if to.Kind() == graph.KindTopic {
					kind = graph.TaggedWith
				}
				w.edge(from, kind, to, "")
			}

			anchors := []graph.ID{docs[0], docs[1]}
			judge := &scriptedJudge{rate: func(retrieval.Lead) float64 { return 1 }}
			engine := w.anchored(judge, retrieval.Config{
				Batch: 1 + random.IntN(9), Follow: 1 + random.IntN(4), MaxSteps: -1,
			})
			findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "?", Anchors: anchors})
			if err != nil {
				t.Fatalf("Gather: %v", err)
			}

			// What a traversal reaches from the anchors, following every
			// edge both ways.
			visits, err := traverse.Collect(traverse.Walk(t.Context(), traverse.FanOut{Store: w.store}, anchors,
				traverse.Options{Follow: []traverse.Step{traverse.Out(""), traverse.In("")}}))
			if err != nil {
				t.Fatalf("Walk: %v", err)
			}
			var reachable []graph.ID
			for _, visit := range visits {
				if visit.ID.Kind() == graph.KindDocument {
					reachable = append(reachable, visit.ID)
				}
			}
			slices.Sort(reachable)

			got := slices.Sorted(slices.Values(evidenceIDs(findings)))
			if !slices.Equal(got, reachable) {
				t.Errorf("the search gathered %d documents and the traversal reached %d", len(got), len(reachable))
			}
			if findings.Verdict != retrieval.Exhausted || findings.Left != (retrieval.Left{}) {
				t.Errorf("the search ended %s leaving %+v", findings.Verdict, findings.Left)
			}
			if len(reachable) < 10 || len(reachable) > documents-10 {
				t.Fatalf("the graph was too tame to prove anything: %d of %d documents reachable", len(reachable), documents)
			}
		})
	}
}

func TestTheSameSearchAlwaysGoesTheSameWay(t *testing.T) {
	build := func() *retrieval.Findings {
		w := newWorld(t)
		random := rand.New(rand.NewPCG(7, 7))
		var docs []graph.ID
		for i := range 60 {
			// Many documents at exactly the same distance, so that only
			// the tie-breaking keeps the order fixed.
			docs = append(docs, w.document(fmt.Sprintf("d%02d", i), "A document", near(float64(i%4)*10)))
		}
		for range 150 {
			from, to := docs[random.IntN(len(docs))], docs[random.IntN(len(docs))]
			if from != to {
				w.edge(from, graph.LinksTo, to, "")
			}
		}
		judge := &scriptedJudge{rate: func(lead retrieval.Lead) float64 {
			return float64(len(lead.Target.VertexID())%3) / 2 // 0, 0.5 or 1, the same every time
		}}
		findings, err := w.engine(judge, retrieval.Config{Batch: 7, MaxSteps: -1}).Gather(t.Context(), retrieval.Question{Text: "?"})
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		return findings
	}
	first, second := build(), build()
	if !reflect.DeepEqual(first, second) {
		t.Errorf("two identical searches differ: %d and %d steps, evidence %s and %s",
			len(first.Trace), len(second.Trace), evidenceIDs(first), evidenceIDs(second))
	}
}

// --- the judge ------------------------------------------------------------------

func TestAJudgeThatFailsEndsTheSearchWithWhatItHad(t *testing.T) {
	w := newWorld(t)
	a := w.document("a", "States part of the answer", near(0))
	b := w.document("b", "On the subject", near(10))
	w.edge(a, graph.LinksTo, b, "")
	outage := errors.New("the judge is unavailable")

	cases := []struct {
		name    string
		break_  func(*scriptedJudge)
		err     error // the error Gather must return; nil for none
		reason  string
		anyFail bool // Gather must fail, with some error
	}{
		{"an outage", func(j *scriptedJudge) {
			j.before = func(call int) error {
				if call == 2 {
					return outage
				}
				return nil
			}
		}, outage, "failed", false},
		{"evidence too large to read", func(j *scriptedJudge) {
			j.before = func(call int) error {
				if call == 2 {
					return fmt.Errorf("jev: %w", retrieval.ErrTooLarge)
				}
				return nil
			}
		}, nil, "outgrew", false},
		{"too few ratings", func(j *scriptedJudge) { j.shortBy = 1 }, nil, "", true},
		{"a rating out of range", func(j *scriptedJudge) { j.ratings[a] = 1.5 }, nil, "", true},
		{"a rating that is not a number", func(j *scriptedJudge) { j.ratings[a] = math.NaN() }, nil, "", true},
		{"a sufficiency out of range", func(j *scriptedJudge) { j.enough = func([]retrieval.Evidence) float64 { return -0.1 } }, nil, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			judge := &scriptedJudge{ratings: map[graph.ID]float64{a: 1, b: 0.5}}
			c.break_(judge)
			findings, err := w.engine(judge, retrieval.Config{Seeds: 1}).Gather(t.Context(), retrieval.Question{Text: "?"})

			switch {
			case c.anyFail:
				if err == nil {
					t.Fatalf("Gather accepted a judge that returned %s", c.name)
				}
				return
			case !errors.Is(err, c.err) || (c.err == nil && err != nil):
				t.Fatalf("Gather returned %v, want %v", err, c.err)
			}
			if findings.Verdict != retrieval.Stopped || !strings.Contains(findings.Reason, c.reason) {
				t.Errorf("the search ended %s (%q)", findings.Verdict, findings.Reason)
			}
			// The first step's evidence survives, and the lead that could
			// not be judged is still counted.
			if !slices.Equal(evidenceIDs(findings), ids(a)) || findings.Left.Unjudged != 1 {
				t.Errorf("the findings are evidence %s with %+v left", evidenceIDs(findings), findings.Left)
			}
		})
	}
}

func TestTheJudgesCostIsAddedUp(t *testing.T) {
	w := newWorld(t)
	a := w.document("a", "States part of the answer", near(0))
	b := w.document("b", "States part of the answer", near(10))
	w.edge(a, graph.LinksTo, b, "")

	judge := &scriptedJudge{ratings: map[graph.ID]float64{a: 1, b: 1}, tokens: 100}
	findings, err := w.engine(judge, retrieval.Config{Seeds: 1}).Gather(t.Context(), retrieval.Question{Text: "?"})
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	if want := 100 * len(judge.calls); findings.Tokens != want || want == 0 {
		t.Errorf("the findings report %d tokens over %d assessments of 100 each", findings.Tokens, len(judge.calls))
	}
}

// --- several questions ----------------------------------------------------------

func TestGatherAllAnswersEachQuestionOnItsOwn(t *testing.T) {
	w := newWorld(t)
	refunds := w.document("refunds", "How refunds work", near(0))
	latency := w.document("latency", "Why checkout is slow", near(90))

	// Each question is embedded differently, so each starts somewhere else.
	embedder := &fakeEmbedder{vectors: map[string][]float32{
		"How do refunds work?":  near(0),
		"Why is checkout slow?": near(90),
		"Will this one fail?":   near(45),
	}}
	judge := &scriptedJudge{
		rate: func(retrieval.Lead) float64 { return 1 },
		enough: func(evidence []retrieval.Evidence) float64 {
			return 1
		},
	}
	judge.before = func(int) error { return nil }
	judge.failFor = "Will this one fail?"
	engine := retrieval.NewEngine(retrieval.Config{
		Store: w.store, Searcher: w.store, Neighbors: w.store, Embedder: embedder, Judge: judge, Seeds: 1,
	})

	results := engine.GatherAll(t.Context(), []retrieval.Question{
		{Text: "How do refunds work?"},
		{Text: ""},
		{Text: "Why is checkout slow?"},
		{Text: "Will this one fail?"},
	})
	if len(results) != 4 {
		t.Fatalf("GatherAll returned %d results for 4 questions", len(results))
	}
	// In order, each with its own outcome.
	if r := results[0]; r.Err != nil || !slices.Equal(evidenceIDs(r.Findings), ids(refunds)) {
		t.Errorf("the first question gave %v, %v", r.Findings, r.Err)
	}
	if r := results[1]; r.Err == nil || r.Findings != nil {
		t.Errorf("the empty question gave %v, %v", r.Findings, r.Err)
	}
	if r := results[2]; r.Err != nil || !slices.Equal(evidenceIDs(r.Findings), ids(latency)) {
		t.Errorf("the third question gave %v, %v", r.Findings, r.Err)
	}
	if r := results[3]; r.Err == nil {
		t.Errorf("the question whose judge fails gave no error")
	}
	// The three questions that could be searched for were embedded in one
	// call.
	if len(embedder.calls) != 1 || len(embedder.calls[0]) != 3 {
		t.Errorf("the embedder was called with %q, want one call with the three valid questions", embedder.calls)
	}
}

// --- the graph's embedding space ------------------------------------------------

const graphSpace = "acme/the-graphs-embedder@7; 3 dimensions"

// satisfied is a judge that rates every lead highly and finds any evidence
// enough.
func satisfied() *scriptedJudge {
	return &scriptedJudge{
		rate:   func(retrieval.Lead) float64 { return 1 },
		enough: func([]retrieval.Evidence) float64 { return 1 },
	}
}

// The graph was embedded with one model and the program is pointed at an
// embeddings service running another. The vectors are of the same length,
// so nothing else would notice.
func TestASearchIsRefusedWhenItsEmbedderIsNotInTheGraphsSpace(t *testing.T) {
	w := newWorld(t)
	answer := w.document("answer", "The answer", near(5))
	if err := w.store.ClaimEmbeddingSpace(t.Context(), graphSpace); err != nil {
		t.Fatalf("ClaimEmbeddingSpace: %v", err)
	}
	embedder, judge := &fakeEmbedder{}, satisfied() // the embedder is in testSpace
	engine := retrieval.NewEngine(retrieval.Config{
		Store: w.store, Searcher: w.store, Neighbors: w.store, Embedder: embedder, Judge: judge,
	})

	results := engine.GatherAll(t.Context(), []retrieval.Question{
		{Text: "What is the answer?"},
		{Text: "And starting from it?", Anchors: ids(answer)},
	})

	for i, result := range results {
		if !errors.Is(result.Err, storage.ErrOtherSpace) {
			t.Errorf("question %d gave %v; want an error that wraps storage.ErrOtherSpace", i+1, result.Err)
			continue
		}
		if !strings.Contains(result.Err.Error(), graphSpace) || !strings.Contains(result.Err.Error(), testSpace) {
			t.Errorf("question %d: the error does not name both spaces: %v", i+1, result.Err)
		}
		if result.Findings != nil {
			t.Errorf("question %d came back with findings: %+v", i+1, result.Findings)
		}
	}
	if len(embedder.calls) != 0 {
		t.Errorf("the questions were embedded all the same: %q", embedder.calls)
	}
	if len(judge.calls) != 0 {
		t.Errorf("the judge was consulted %d times about a search that could not be trusted", len(judge.calls))
	}
}

func TestASearchGoesAheadWhenTheSpacesAgreeOrThereIsNothingToCompare(t *testing.T) {
	gather := func(t *testing.T, claimed string, embedder *fakeEmbedder) retrieval.Result {
		t.Helper()
		w := newWorld(t)
		answer := w.document("answer", "The answer", near(5))
		if claimed != "" {
			if err := w.store.ClaimEmbeddingSpace(t.Context(), claimed); err != nil {
				t.Fatalf("ClaimEmbeddingSpace: %v", err)
			}
		}
		cfg := retrieval.Config{Store: w.store, Searcher: w.store, Neighbors: w.store, Judge: satisfied()}
		if embedder != nil {
			cfg.Embedder = embedder
		}
		results := retrieval.NewEngine(cfg).GatherAll(t.Context(), []retrieval.Question{{Text: "What is the answer?", Anchors: ids(answer)}})
		return results[0]
	}

	// The embedder is in the space the graph holds.
	if r := gather(t, testSpace, &fakeEmbedder{}); r.Err != nil || len(r.Findings.Evidence) != 1 {
		t.Errorf("with the spaces agreeing the search gave %v, %v", r.Findings, r.Err)
	}
	// No space has been claimed for the graph: there is nothing to hold the embedder to.
	if r := gather(t, "", &fakeEmbedder{}); r.Err != nil || len(r.Findings.Evidence) != 1 {
		t.Errorf("with no space claimed the search gave %v, %v", r.Findings, r.Err)
	}
	// No embedder: the search brings no vectors to the graph at all.
	if r := gather(t, graphSpace, nil); r.Err != nil || len(r.Findings.Evidence) != 1 {
		t.Errorf("with no embedder the search gave %v, %v", r.Findings, r.Err)
	}
}

func TestAnEmbedderThatCannotNameItsSpaceFailsTheSearch(t *testing.T) {
	w := newWorld(t)
	answer := w.document("answer", "The answer", near(5))
	lost := errors.New("the embeddings service is not there")
	embedder := &fakeEmbedder{lost: lost}
	engine := retrieval.NewEngine(retrieval.Config{
		Store: w.store, Searcher: w.store, Neighbors: w.store, Embedder: embedder, Judge: satisfied(),
	})

	findings, err := engine.Gather(t.Context(), retrieval.Question{Text: "What is the answer?", Anchors: ids(answer)})

	if !errors.Is(err, lost) || findings != nil {
		t.Errorf("Gather gave %v, %v; want the embedder's error", findings, err)
	}
	if len(embedder.calls) != 0 {
		t.Error("the question was embedded by an embedder that could not say what space it is in")
	}
}

func TestGatherAllWorksOnSeveralQuestionsAtOnceButNoMoreThanAllowed(t *testing.T) {
	w := newWorld(t)
	w.document("a", "A document", near(0))

	var mu sync.Mutex
	running, most := 0, 0
	judge := &scriptedJudge{}
	judge.before = func(int) error {
		mu.Lock()
		running++
		most = max(most, running)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}

	questions := make([]retrieval.Question, 12)
	for i := range questions {
		questions[i] = retrieval.Question{Text: fmt.Sprintf("Question %d?", i)}
	}
	for _, result := range w.engine(judge, retrieval.Config{Parallel: 3}).GatherAll(t.Context(), questions) {
		if result.Err != nil {
			t.Fatalf("GatherAll: %v", result.Err)
		}
	}
	if most != 3 {
		t.Errorf("at most %d questions were worked on at once, want 3", most)
	}
}

// --- helpers --------------------------------------------------------------------

// world is a graph in memory, built by a test.
type world struct {
	t     *testing.T
	store *memory.Store
}

func newWorld(t *testing.T) *world {
	return &world{t: t, store: memory.New()}
}

// engine returns an Engine over the world with the given judge. Questions
// are all embedded as the same vector, the one near(0) returns, so that a
// document's angle says how near the question it is. cfg supplies anything
// else.
func (w *world) engine(judge retrieval.Judge, cfg retrieval.Config) *retrieval.Engine {
	cfg.Store, cfg.Searcher, cfg.Judge = w.store, w.store, judge
	if cfg.Neighbors == nil {
		cfg.Neighbors = w.store
	}
	cfg.Embedder = &fakeEmbedder{}
	return retrieval.NewEngine(cfg)
}

// anchored returns an Engine like engine's, except that it cannot look for
// a place to start: its searches begin at the question's anchors and nowhere
// else.
func (w *world) anchored(judge retrieval.Judge, cfg retrieval.Config) *retrieval.Engine {
	cfg.Store, cfg.Judge = w.store, judge
	if cfg.Neighbors == nil {
		cfg.Neighbors = w.store
	}
	cfg.Embedder = &fakeEmbedder{}
	return retrieval.NewEngine(cfg)
}

func (w *world) document(key, title string, embedding []float32) graph.ID {
	w.t.Helper()
	id := graph.DocumentID("s", key)
	w.put(&graph.Document{ID: id, Type: graph.TypeIssue, Source: "s", Title: title, Summary: "Summary of " + title, Embedding: embedding})
	return id
}

func (w *world) topic(name string, embedding []float32) graph.ID {
	w.t.Helper()
	id := graph.TopicID(name)
	w.put(&graph.Topic{ID: id, Name: name, Summary: "About " + name, Embedding: embedding})
	return id
}

func (w *world) put(vertex graph.Vertex) {
	w.t.Helper()
	if err := w.store.PutVertices(context.Background(), []graph.Vertex{vertex}); err != nil {
		w.t.Fatalf("PutVertices: %v", err)
	}
}

func (w *world) edge(from graph.ID, kind graph.EdgeKind, to graph.ID, summary string) {
	w.t.Helper()
	if err := w.store.PutEdges(context.Background(), []graph.Edge{{From: from, Kind: kind, To: to, Summary: summary}}); err != nil {
		w.t.Fatalf("PutEdges: %v", err)
	}
}

// near returns an embedding at the given angle, in degrees, from the one
// every question gets in these tests. A smaller angle is a nearer document.
func near(degrees float64) []float32 {
	radians := degrees * math.Pi / 180
	return []float32{float32(math.Cos(radians)), float32(math.Sin(radians)), 0}
}

// scriptedJudge is a Judge that does what a test tells it to.
type scriptedJudge struct {
	// ratings gives the rating of each vertex; one not listed rates 0.
	// rate, if set, is used instead.
	ratings map[graph.ID]float64
	rate    func(retrieval.Lead) float64

	// enough gives the sufficiency of the evidence; unset means never.
	enough func([]retrieval.Evidence) float64

	before  func(call int) error // runs first in every call; an error ends the call
	failFor string               // the text of a question every call for which fails
	delay   time.Duration        // how long each call takes
	shortBy int                  // how many ratings to leave off each answer
	tokens  int                  // what each call claims to cost

	mu    sync.Mutex
	calls []judgeCall
}

type judgeCall struct {
	question retrieval.Question
	evidence int
	leads    []graph.ID
}

func (j *scriptedJudge) Assess(ctx context.Context, q retrieval.Question, evidence []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	j.mu.Lock()
	call := judgeCall{question: q, evidence: len(evidence)}
	for _, lead := range leads {
		call.leads = append(call.leads, lead.Target.VertexID())
	}
	j.calls = append(j.calls, call)
	number := len(j.calls)
	j.mu.Unlock()

	if j.before != nil {
		if err := j.before(number); err != nil {
			return retrieval.Assessment{}, err
		}
	}
	if j.failFor != "" && q.Text == j.failFor {
		return retrieval.Assessment{}, errors.New("the judge fails for this question")
	}
	if j.delay > 0 {
		select {
		case <-time.After(j.delay):
		case <-ctx.Done():
			return retrieval.Assessment{}, ctx.Err()
		}
	}

	assessment := retrieval.Assessment{Tokens: j.tokens}
	if j.enough != nil {
		assessment.Sufficient = j.enough(evidence)
	}
	for _, lead := range leads[:len(leads)-min(j.shortBy, len(leads))] {
		rating := j.ratings[lead.Target.VertexID()]
		if j.rate != nil {
			rating = j.rate(lead)
		}
		assessment.Potential = append(assessment.Potential, rating)
	}
	return assessment, nil
}

// fakeEmbedder embeds a question as the vector listed for it, or as near(0).
type fakeEmbedder struct {
	vectors map[string][]float32
	calls   [][]string
	space   string // the embedding space it says it is in; "" means testSpace
	lost    error  // if set, what asking for its space fails with
}

// testSpace is the embedding space of a fakeEmbedder that names no other.
const testSpace = "acme/test-embedder@1; 4 dimensions"

func (f *fakeEmbedder) Space(context.Context) (string, error) {
	if f.lost != nil {
		return "", f.lost
	}
	return cmp.Or(f.space, testSpace), nil
}

func (f *fakeEmbedder) EmbedQueries(_ context.Context, texts []string) ([][]float32, error) {
	f.calls = append(f.calls, slices.Clone(texts))
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = near(0)
		if listed, ok := f.vectors[text]; ok {
			vectors[i] = listed
		}
	}
	return vectors, nil
}

func (f *fakeEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return f.EmbedQueries(ctx, texts)
}

// countingNeighbors passes neighbour queries through, noting how many
// neighbours were read in all and the largest page that was asked for.
type countingNeighbors struct {
	inner storage.NeighborSearcher

	mu      sync.Mutex
	read    int
	largest int
}

func (c *countingNeighbors) Neighbors(ctx context.Context, q storage.NeighborQuery) (storage.NeighborPage, error) {
	page, err := c.inner.Neighbors(ctx, q)
	c.mu.Lock()
	c.read += len(page.Neighbors)
	c.largest = max(c.largest, q.PageSize())
	c.mu.Unlock()
	return page, err
}

// step is what one step of a search is expected to have done.
type step struct {
	judged     []graph.ID
	followed   []graph.ID
	sufficient float64
}

// requireTrace fails the test unless the search took exactly the given
// steps.
func requireTrace(t *testing.T, findings *retrieval.Findings, want ...step) {
	t.Helper()
	if len(findings.Trace) != len(want) {
		t.Fatalf("the search took %d steps, want %d:\n%s", len(findings.Trace), len(want), describe(findings))
	}
	for i, got := range findings.Trace {
		if !slices.Equal(targets(got.Judged), want[i].judged) || !slices.Equal(got.Followed, want[i].followed) || got.Sufficient != want[i].sufficient {
			t.Fatalf("step %d judged %s, followed %s and read the evidence as %v;\nwant judged %s, followed %s, evidence %v\n%s",
				i+1, targets(got.Judged), got.Followed, got.Sufficient, want[i].judged, want[i].followed, want[i].sufficient, describe(findings))
		}
	}
}

// describe prints a trace for a failure message.
func describe(findings *retrieval.Findings) string {
	var out strings.Builder
	for i, step := range findings.Trace {
		fmt.Fprintf(&out, "  step %d: sufficient %v, judged %s, followed %s\n", i+1, step.Sufficient, targets(step.Judged), step.Followed)
	}
	return out.String()
}

func ids(list ...graph.ID) []graph.ID { return list }

func targets(judged []retrieval.Judgment) []graph.ID {
	var out []graph.ID
	for _, judgment := range judged {
		out = append(out, judgment.Lead.Target.VertexID())
	}
	return out
}

func evidenceIDs(findings *retrieval.Findings) []graph.ID {
	if findings == nil {
		return nil
	}
	var out []graph.ID
	for _, evidence := range findings.Evidence {
		out = append(out, evidence.Document.ID)
	}
	return out
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

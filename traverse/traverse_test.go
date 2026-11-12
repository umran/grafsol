package traverse_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
	"github.com/umran/grafsol/traverse"
)

func TestWalkVisitsEachVertexAtItsSmallestDepth(t *testing.T) {
	//   a -> b -> d -> e
	//   a -> c -> d
	store := storeWith(t,
		link("a", "b"), link("a", "c"),
		link("b", "d"), link("c", "d"),
		link("d", "e"),
	)

	visits := walk(t, store, []graph.ID{doc("a")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.LinksTo)},
	})

	want := []traverse.Visit{
		{ID: doc("a")},
		{ID: doc("b"), Depth: 1, Parent: doc("a"), Via: link("a", "b")},
		{ID: doc("c"), Depth: 1, Parent: doc("a"), Via: link("a", "c")},
		// d is two hops away by two routes; the smaller edge, from b, is reported.
		{ID: doc("d"), Depth: 2, Parent: doc("b"), Via: link("b", "d")},
		{ID: doc("e"), Depth: 3, Parent: doc("d"), Via: link("d", "e")},
	}
	requireVisits(t, visits, want)
}

func TestWalkTerminatesOnCycles(t *testing.T) {
	store := storeWith(t, link("a", "b"), link("b", "c"), link("c", "a"), link("b", "b"))

	visits := walk(t, store, []graph.ID{doc("a")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.LinksTo)},
	})
	requireIDs(t, visits, doc("a"), doc("b"), doc("c"))
}

func TestWalkMaxDepth(t *testing.T) {
	store := storeWith(t, link("a", "b"), link("b", "c"), link("c", "d"), link("d", "e"))
	follow := []traverse.Step{traverse.Out(graph.LinksTo)}

	for depth, want := range map[int][]graph.ID{
		1: {doc("a"), doc("b")},
		2: {doc("a"), doc("b"), doc("c")},
		0: {doc("a"), doc("b"), doc("c"), doc("d"), doc("e")}, // no limit
	} {
		visits := walk(t, store, []graph.ID{doc("a")}, traverse.Options{Follow: follow, MaxDepth: depth})
		requireIDs(t, visits, want...)
	}
}

func TestWalkFollowsOnlyTheRequestedEdges(t *testing.T) {
	billing := graph.TopicID("billing")
	store := storeWith(t,
		edge("child1", graph.ChildOf, doc("parent")),
		edge("child2", graph.ChildOf, doc("parent")),
		edge("child1", graph.LinksTo, doc("elsewhere")),
		edge("child1", graph.TaggedWith, billing),
		edge("other", graph.TaggedWith, billing),
	)

	// Backwards along child_of: the children of a document.
	children := walk(t, store, []graph.ID{doc("parent")}, traverse.Options{
		Follow: []traverse.Step{traverse.In(graph.ChildOf)},
	})
	requireIDs(t, children, doc("parent"), doc("child1"), doc("child2"))

	// Forwards along child_of: its parent, and nothing reached by other kinds.
	parents := walk(t, store, []graph.ID{doc("child1")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.ChildOf)},
	})
	requireIDs(t, parents, doc("child1"), doc("parent"))

	// Out to a topic and back in from it: the documents that share a tag.
	sameTag := walk(t, store, []graph.ID{doc("child1")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.TaggedWith), traverse.In(graph.TaggedWith)},
	})
	requireIDs(t, sameTag, doc("child1"), billing, doc("other"))

	// An empty kind follows every kind.
	everything := walk(t, store, []graph.ID{doc("child1")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(""), traverse.In("")},
	})
	requireIDs(t, everything, doc("child1"), doc("parent"), doc("elsewhere"), billing, doc("child2"), doc("other"))
}

func TestWalkSeeds(t *testing.T) {
	store := storeWith(t, link("a", "b"))
	opts := traverse.Options{Follow: []traverse.Step{traverse.Out(graph.LinksTo)}}

	// Seeds come first, in the order given, without repeats.
	visits := walk(t, store, []graph.ID{doc("z"), doc("a"), doc("z")}, opts)
	requireIDs(t, visits, doc("z"), doc("a"), doc("b"))
	if visits[0].ID != doc("z") || visits[1].ID != doc("a") {
		t.Errorf("seeds were reordered: %v", ids(visits))
	}

	if visits := walk(t, store, nil, opts); len(visits) != 0 {
		t.Errorf("a walk with no seeds visited %v", ids(visits))
	}

	if _, err := traverse.Collect(traverse.Walk(t.Context(), traverse.FanOut{Store: store}, []graph.ID{"nope"}, opts)); err == nil {
		t.Errorf("a malformed seed was accepted")
	}
}

func TestWalkRejectsBadOptions(t *testing.T) {
	store := storeWith(t, link("a", "b"))
	bad := map[string]traverse.Options{
		"nothing to follow": {},
		"no direction":      {Follow: []traverse.Step{{Kind: graph.LinksTo}}},
		"malformed kind":    {Follow: []traverse.Step{traverse.Out("Links To")}},
		"negative depth":    {Follow: []traverse.Step{traverse.Out("")}, MaxDepth: -1},
	}
	for name, opts := range bad {
		visits, err := traverse.Collect(traverse.Walk(t.Context(), traverse.FanOut{Store: store}, []graph.ID{doc("a")}, opts))
		if err == nil {
			t.Errorf("%s: accepted", name)
		}
		if len(visits) != 0 {
			t.Errorf("%s: visited %v before failing", name, ids(visits))
		}
	}
}

func TestWalkIsTheSameHoweverEdgesAreFetched(t *testing.T) {
	// A graph with plenty of alternative routes, so that the order in which
	// edges happen to arrive would show if it leaked into the result.
	var edges []graph.Edge
	const size = 60
	for i := range size {
		for _, step := range []int{1, 7, 13} {
			edges = append(edges, link(fmt.Sprintf("n%02d", i), fmt.Sprintf("n%02d", (i*step+step)%size)))
		}
	}
	store := storeWith(t, edges...)
	opts := traverse.Options{Follow: []traverse.Step{traverse.Out(graph.LinksTo), traverse.In(graph.LinksTo)}}
	seeds := []graph.ID{doc("n00")}

	reference := walkWith(t, traverse.FanOut{Store: store, Workers: 1}, seeds, opts)
	if len(reference) != size {
		t.Fatalf("the reference walk visited %d of %d vertices", len(reference), size)
	}
	for _, fanOut := range []traverse.FanOut{
		{Store: store},
		{Store: store, Workers: 3, PageSize: 1},
		{Store: store, Workers: 64, PageSize: 2},
	} {
		for range 5 {
			requireVisits(t, walkWith(t, fanOut, seeds, opts), reference)
		}
	}
}

func TestFanOutListsEveryEdge(t *testing.T) {
	// One vertex with far more edges than a page holds, among ordinary ones.
	var edges []graph.Edge
	for i := range 2500 {
		edges = append(edges, link("hub", fmt.Sprintf("spoke%04d", i)))
	}
	edges = append(edges, link("a", "b"), link("a", "hub"), edge("a", graph.ChildOf, doc("b")))
	store := storeWith(t, edges...)
	frontier := []graph.ID{doc("hub"), doc("a"), doc("isolated")}

	for _, fanOut := range []traverse.FanOut{
		{Store: store},
		{Store: store, Workers: 2, PageSize: 100},
		{Store: store, Workers: 1, PageSize: 7},
	} {
		var got []graph.Edge
		for e, err := range fanOut.Expand(t.Context(), frontier, graph.Out, graph.LinksTo) {
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			got = append(got, e)
		}
		var want []graph.Edge
		for _, e := range edges {
			if e.Kind == graph.LinksTo {
				want = append(want, e)
			}
		}
		requireSameEdges(t, got, want)
	}

	// Backwards: the edges arriving at the frontier.
	var incoming []graph.Edge
	for e, err := range (traverse.FanOut{Store: store}).Expand(t.Context(), []graph.ID{doc("b"), doc("hub")}, graph.In, "") {
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		incoming = append(incoming, e)
	}
	requireSameEdges(t, incoming, []graph.Edge{link("a", "b"), link("a", "hub"), edge("a", graph.ChildOf, doc("b"))})
}

func TestStoppingEarlyLeavesNothingRunning(t *testing.T) {
	var edges []graph.Edge
	for i := range 200 {
		for j := range 20 {
			edges = append(edges, link(fmt.Sprintf("n%03d", i), fmt.Sprintf("n%03d", (i+j+1)%200)))
		}
	}
	store := storeWith(t, edges...)
	before := runtime.NumGoroutine()

	// Break out of a walk part-way through...
	seen := 0
	for _, err := range traverse.Walk(t.Context(), traverse.FanOut{Store: store, PageSize: 3}, []graph.ID{doc("n000")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.LinksTo)},
	}) {
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if seen++; seen == 25 {
			break
		}
	}

	// ...and out of an expansion after its very first edge.
	frontier := make([]graph.ID, 200)
	for i := range frontier {
		frontier[i] = doc(fmt.Sprintf("n%03d", i))
	}
	for _, err := range (traverse.FanOut{Store: store, PageSize: 2}).Expand(t.Context(), frontier, graph.Out, "") {
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		break
	}

	requireGoroutinesSettle(t, before)
}

func TestWalkReportsStoreErrors(t *testing.T) {
	store := storeWith(t, link("a", "b"), link("a", "c"), link("b", "d"), link("c", "e"))
	broken := errors.New("disk on fire")
	failing := failingLister{EdgeLister: store, failOn: doc("c"), err: broken}
	before := runtime.NumGoroutine()

	visits, err := traverse.Collect(traverse.Walk(t.Context(), traverse.FanOut{Store: failing}, []graph.ID{doc("a")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.LinksTo)},
	}))
	if !errors.Is(err, broken) {
		t.Fatalf("got error %v, want it to wrap %v", err, broken)
	}
	// Everything found before the failing level is still reported.
	requireIDs(t, visits, doc("a"), doc("b"), doc("c"))
	requireGoroutinesSettle(t, before)
}

func TestWalkStopsWhenTheContextEnds(t *testing.T) {
	var edges []graph.Edge
	for i := range 50 {
		edges = append(edges, link(fmt.Sprintf("n%02d", i), fmt.Sprintf("n%02d", i+1)))
	}
	store := storeWith(t, edges...)
	ctx, cancel := context.WithCancel(t.Context())
	before := runtime.NumGoroutine()

	var visited int
	var failure error
	for _, err := range traverse.Walk(ctx, traverse.FanOut{Store: store}, []graph.ID{doc("n00")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.LinksTo)},
	}) {
		if err != nil {
			failure = err
			break
		}
		if visited++; visited == 10 {
			cancel()
		}
	}
	cancel()
	if !errors.Is(failure, context.Canceled) {
		t.Fatalf("after cancelling, the walk ended with %v (visited %d), want context.Canceled", failure, visited)
	}
	requireGoroutinesSettle(t, before)
}

// A listing that was cut short must never pass for a complete one: with a
// context that has already ended, Expand has to say so rather than quietly
// yield nothing.
func TestExpandReportsAnEndedContext(t *testing.T) {
	store := storeWith(t, link("a", "b"), link("c", "d"))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	for range 50 { // the outcome must not depend on scheduling
		var failure error
		edges := 0
		for _, err := range (traverse.FanOut{Store: store}).Expand(ctx, []graph.ID{doc("a"), doc("c")}, graph.Out, "") {
			if err != nil {
				failure = err
				break
			}
			edges++
		}
		if !errors.Is(failure, context.Canceled) {
			t.Fatalf("Expand with an ended context yielded %d edges and error %v, want context.Canceled", edges, failure)
		}
	}
}

func TestPathTo(t *testing.T) {
	store := storeWith(t, link("a", "b"), link("b", "c"), edge("d", graph.ChildOf, doc("c")))
	visits := walk(t, store, []graph.ID{doc("a")}, traverse.Options{
		Follow: []traverse.Step{traverse.Out(graph.LinksTo), traverse.In(graph.ChildOf)},
	})

	// The last hop runs against its edge: d was reached from c through d -> c.
	want := []graph.Edge{link("a", "b"), link("b", "c"), edge("d", graph.ChildOf, doc("c"))}
	if got := traverse.PathTo(visits, doc("d")); !slices.Equal(got, want) {
		t.Errorf("PathTo(d) = %v, want %v", got, want)
	}
	if got := traverse.PathTo(visits, doc("a")); got != nil {
		t.Errorf("PathTo(a seed) = %v, want nil", got)
	}
	if got := traverse.PathTo(visits, doc("unreached")); got != nil {
		t.Errorf("PathTo(an unreached vertex) = %v, want nil", got)
	}
}

// --- helpers ----------------------------------------------------------------

// doc returns the ID of a test document.
func doc(key string) graph.ID {
	return graph.DocumentID("t", key)
}

// link returns a links_to edge between two test documents.
func link(from, to string) graph.Edge {
	return edge(from, graph.LinksTo, doc(to))
}

// edge returns an edge of any kind from a test document to any vertex.
func edge(from string, kind graph.EdgeKind, to graph.ID) graph.Edge {
	return graph.Edge{From: doc(from), Kind: kind, To: to, Summary: fmt.Sprintf("%s %s %s", from, kind, to.Key())}
}

func storeWith(t *testing.T, edges ...graph.Edge) *memory.Store {
	t.Helper()
	store := memory.New()
	if err := store.PutEdges(t.Context(), edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	return store
}

func walk(t *testing.T, store *memory.Store, seeds []graph.ID, opts traverse.Options) []traverse.Visit {
	t.Helper()
	return walkWith(t, traverse.FanOut{Store: store}, seeds, opts)
}

func walkWith(t *testing.T, x traverse.Expander, seeds []graph.ID, opts traverse.Options) []traverse.Visit {
	t.Helper()
	visits, err := traverse.Collect(traverse.Walk(t.Context(), x, seeds, opts))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return visits
}

func ids(visits []traverse.Visit) []graph.ID {
	out := make([]graph.ID, len(visits))
	for i, visit := range visits {
		out[i] = visit.ID
	}
	return out
}

// requireIDs fails the test unless the walk visited exactly the given
// vertices, in any order.
func requireIDs(t *testing.T, visits []traverse.Visit, want ...graph.ID) {
	t.Helper()
	got := slices.Sorted(slices.Values(ids(visits)))
	wanted := slices.Sorted(slices.Values(want))
	if !slices.Equal(got, wanted) {
		t.Fatalf("visited %v, want %v", got, wanted)
	}
}

// requireVisits fails the test unless the two walks are identical, visit for
// visit and in the same order.
func requireVisits(t *testing.T, got, want []traverse.Visit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d visits, want %d\n got: %v\nwant: %v", len(got), len(want), ids(got), ids(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("visit %d differs\n got: %+v\nwant: %+v", i, got[i], want[i])
		}
	}
}

// requireSameEdges fails the test unless got and want hold the same edges,
// in any order and without repeats.
func requireSameEdges(t *testing.T, got, want []graph.Edge) {
	t.Helper()
	sortEdges := func(edges []graph.Edge) []graph.Edge {
		return slices.SortedFunc(slices.Values(edges), graph.Edge.Compare)
	}
	sortedGot, sortedWant := sortEdges(got), sortEdges(want)
	if !slices.Equal(sortedGot, sortedWant) {
		t.Fatalf("got %d edges, want %d (or the same number but different ones)", len(got), len(want))
	}
}

// requireGoroutinesSettle fails the test if more goroutines are still
// running than before the code under test started.
func requireGoroutinesSettle(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("%d goroutines running, up from %d before: something was left behind", runtime.NumGoroutine(), before)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// failingLister is a store whose Edges call fails for one particular vertex.
type failingLister struct {
	traverse.EdgeLister
	failOn graph.ID
	err    error
}

func (f failingLister) Edges(ctx context.Context, q storage.EdgeQuery) (storage.EdgePage, error) {
	if q.Vertex == f.failOn {
		return storage.EdgePage{}, f.err
	}
	return f.EdgeLister.Edges(ctx, q)
}

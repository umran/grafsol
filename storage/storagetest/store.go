// Package storagetest holds the conformance suites that every storage
// backend must pass. A backend's own test file calls RunStore, RunSearcher
// and RunLedger, handing each a function that opens a fresh, empty instance:
//
//	func TestStore(t *testing.T) {
//		storagetest.RunStore(t, 4, func(*testing.T) storage.Store {
//			return memory.New()
//		})
//	}
//
// The suites are the executable form of the doc comments in package storage:
// where the two disagree, one of them has a bug.
package storagetest

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// RunStore checks a Store against its contract. open must return a fresh,
// empty store every time it is called. dimensions is the length of the
// embeddings the store accepts, and must be at least 3.
func RunStore(t *testing.T, dimensions int, open func(t *testing.T) storage.Store) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, store storage.Store, dimensions int)
	}{
		{"VerticesRoundTrip", testVerticesRoundTrip},
		{"PutVerticesReplaces", testPutVerticesReplaces},
		{"GetVerticesSkipsMissing", testGetVerticesSkipsMissing},
		{"DeleteVertices", testDeleteVertices},
		{"PutVerticesRejectsInvalid", testPutVerticesRejectsInvalid},
		{"StoredVerticesAreCopies", testStoredVerticesAreCopies},
		{"EdgesFromBothEnds", testEdgesFromBothEnds},
		{"EdgesOfOneKind", testEdgesOfOneKind},
		{"EdgesAreOrdered", testEdgesAreOrdered},
		{"EdgesArePaged", testEdgesArePaged},
		{"EdgesDefaultPageSize", testEdgesDefaultPageSize},
		{"PutEdgesIsIdempotent", testPutEdgesIsIdempotent},
		{"DeleteEdges", testDeleteEdges},
		{"EdgesToMissingVertices", testEdgesToMissingVertices},
		{"RejectsInvalidEdgesAndQueries", testRejectsInvalidEdgesAndQueries},
		{"AwkwardText", testAwkwardText},
		{"EmbeddingSpaceStartsUnclaimed", testEmbeddingSpaceStartsUnclaimed},
		{"FirstClaimOfASpaceIsRecorded", testFirstClaimOfASpaceIsRecorded},
		{"ClaimOfAnotherSpaceIsRefused", testClaimOfAnotherSpaceIsRefused},
		{"SpaceWithoutANameCannotBeClaimed", testSpaceWithoutANameCannotBeClaimed},
		{"RivalClaimsLeaveOneWinner", testRivalClaimsLeaveOneWinner},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, open(t), dimensions)
		})
	}
}

// --- vertices ---------------------------------------------------------------

func testVerticesRoundTrip(t *testing.T, store storage.Store, dimensions int) {
	document := sampleDocument("doc:github:acme/api#42", dimensions)
	topic := &graph.Topic{
		ID:        graph.TopicID("Billing"),
		Name:      "Billing",
		Summary:   "Invoices, refunds and payment providers.",
		Embedding: vector(dimensions, 1, 0, 0.5),
	}
	bare := &graph.Document{ID: "doc:jira:PAY-7"} // every optional field left empty

	putVertices(t, store, document, topic, bare)

	got := getVertices(t, store, document.ID, topic.ID, bare.ID)
	if len(got) != 3 {
		t.Fatalf("got %d vertices, want 3", len(got))
	}
	requireVertex(t, got, document)
	requireVertex(t, got, topic)
	requireVertex(t, got, bare)
}

func testPutVerticesReplaces(t *testing.T, store storage.Store, dimensions int) {
	original := sampleDocument("doc:github:acme/api#42", dimensions)
	putVertices(t, store, original)

	// The replacement drops the summary and the embedding: nothing of the
	// original may survive.
	replacement := &graph.Document{ID: original.ID, Type: graph.TypeIssue, Title: "Renamed"}
	putVertices(t, store, replacement)

	requireVertex(t, getVertices(t, store, original.ID), replacement)
}

func testGetVerticesSkipsMissing(t *testing.T, store storage.Store, dimensions int) {
	document := sampleDocument("doc:github:acme/api#42", dimensions)
	putVertices(t, store, document)

	got := getVertices(t, store, document.ID, "doc:github:acme/api#404", document.ID)
	if len(got) != 1 {
		t.Fatalf("got %d vertices, want only the stored one: %v", len(got), got)
	}
	requireVertex(t, got, document)

	if got := getVertices(t, store); len(got) != 0 {
		t.Errorf("asking for no IDs returned %d vertices", len(got))
	}
}

func testDeleteVertices(t *testing.T, store storage.Store, dimensions int) {
	a := sampleDocument("doc:s:a", dimensions)
	b := sampleDocument("doc:s:b", dimensions)
	edge := graph.Edge{From: a.ID, Kind: graph.LinksTo, To: b.ID}
	putVertices(t, store, a, b)
	putEdges(t, store, edge)

	if err := store.DeleteVertices(t.Context(), []graph.ID{b.ID, "doc:s:never-stored"}); err != nil {
		t.Fatalf("DeleteVertices: %v", err)
	}

	got := getVertices(t, store, a.ID, b.ID)
	if len(got) != 1 {
		t.Fatalf("got %d vertices after the delete, want 1", len(got))
	}
	requireVertex(t, got, a)

	// Deleting a vertex leaves the edges that touch it in place.
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a.ID, Direction: graph.Out}), edge)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: b.ID, Direction: graph.In}), edge)
}

func testPutVerticesRejectsInvalid(t *testing.T, store storage.Store, dimensions int) {
	valid := sampleDocument("doc:s:valid", dimensions)

	batches := map[string][]graph.Vertex{
		"malformed ID":   {valid, &graph.Document{ID: "nope"}},
		"nil vertex":     {valid, nil},
		"wrong ID kind":  {valid, &graph.Topic{ID: "doc:s:not-a-topic"}},
		"control in ID":  {valid, &graph.Topic{ID: "topic:a\x00b"}},
		"nil *Document":  {valid, (*graph.Document)(nil)},
		"empty topic ID": {valid, &graph.Topic{}},
	}
	for name, batch := range batches {
		if err := store.PutVertices(t.Context(), batch); err == nil {
			t.Errorf("%s: PutVertices accepted an invalid vertex", name)
		}
	}

	// A rejected batch must leave nothing behind, not even its valid part.
	if got := getVertices(t, store, valid.ID); len(got) != 0 {
		t.Errorf("a rejected batch still stored %v", got)
	}
}

func testStoredVerticesAreCopies(t *testing.T, store storage.Store, dimensions int) {
	document := sampleDocument("doc:s:a", dimensions)
	want := document.Clone()
	putVertices(t, store, document)

	// Changing the value that was handed in must not reach the store...
	document.Title = "changed after the put"
	document.Embedding[0] = 99
	requireVertex(t, getVertices(t, store, want.ID), want)

	// ...and neither must changing a value the store handed out.
	fetched := getVertices(t, store, want.ID)[want.ID].(*graph.Document)
	fetched.Title = "changed after the get"
	fetched.Embedding[0] = 99
	requireVertex(t, getVertices(t, store, want.ID), want)
}

// --- edges ------------------------------------------------------------------

func testEdgesFromBothEnds(t *testing.T, store storage.Store, _ int) {
	a, b, c := graph.ID("doc:s:a"), graph.ID("doc:s:b"), graph.ID("doc:s:c")
	ab := graph.Edge{From: a, Kind: graph.LinksTo, To: b, Summary: "a mentions b"}
	cb := graph.Edge{From: c, Kind: graph.LinksTo, To: b, Summary: "c mentions b"}
	ac := graph.Edge{From: a, Kind: graph.ChildOf, To: c}
	putEdges(t, store, ab, cb, ac)

	// child_of sorts before links_to.
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.Out}), ac, ab)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: b, Direction: graph.In}), ab, cb)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: c, Direction: graph.In}), ac)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: c, Direction: graph.Out}), cb)

	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: b, Direction: graph.Out}))
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.In}))
}

func testEdgesOfOneKind(t *testing.T, store storage.Store, _ int) {
	a, b, c := graph.ID("doc:s:a"), graph.ID("doc:s:b"), graph.ID("doc:s:c")
	topic := graph.TopicID("billing")
	ab := graph.Edge{From: a, Kind: graph.LinksTo, To: b}
	ac := graph.Edge{From: a, Kind: graph.LinksTo, To: c}
	child := graph.Edge{From: a, Kind: graph.ChildOf, To: c}
	tagged := graph.Edge{From: a, Kind: graph.TaggedWith, To: topic}
	putEdges(t, store, ab, ac, child, tagged)

	out := func(kind graph.EdgeKind) []graph.Edge {
		return listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.Out, Kind: kind})
	}
	requireEdges(t, out(graph.LinksTo), ab, ac)
	requireEdges(t, out(graph.ChildOf), child)
	requireEdges(t, out(graph.TaggedWith), tagged)
	requireEdges(t, out(graph.InTopic))
	requireEdges(t, out(""), child, ab, ac, tagged)

	in := listEdges(t, store, storage.EdgeQuery{Vertex: c, Direction: graph.In, Kind: graph.ChildOf})
	requireEdges(t, in, child)
}

func testEdgesAreOrdered(t *testing.T, store storage.Store, _ int) {
	// Keys chosen so that comparing bytes and comparing the way a human
	// language sorts give different answers: upper case before lower case,
	// "10" before "9", and the accented letter last.
	keys := []string{"B", "a", "C", "b", "_", "10", "9", "Z", "é"}
	hub := graph.ID("doc:s:hub")

	var outgoing, incoming []graph.Edge
	for i, key := range keys {
		kind := graph.LinksTo
		if i%2 == 1 {
			kind = graph.ChildOf
		}
		other := graph.DocumentID("s", key)
		outgoing = append(outgoing, graph.Edge{From: hub, Kind: kind, To: other})
		incoming = append(incoming, graph.Edge{From: other, Kind: kind, To: hub})
	}
	putEdges(t, store, outgoing...)
	putEdges(t, store, incoming...)

	requireEdges(t,
		listEdges(t, store, storage.EdgeQuery{Vertex: hub, Direction: graph.Out}),
		inListingOrder(outgoing, graph.Out)...)
	requireEdges(t,
		listEdges(t, store, storage.EdgeQuery{Vertex: hub, Direction: graph.In}),
		inListingOrder(incoming, graph.In)...)
}

func testEdgesArePaged(t *testing.T, store storage.Store, _ int) {
	const pageSize = 5
	for _, direction := range []graph.Direction{graph.Out, graph.In} {
		for _, count := range []int{0, 1, 5, 20, 23} {
			hub := graph.DocumentID("s", fmt.Sprintf("hub-%s-%d", direction, count))
			edges := spokes(hub, direction, count)
			putEdges(t, store, edges...)
			want := inListingOrder(edges, direction)

			// Walk the listing page by page, checking each page as it comes.
			var got []graph.Edge
			query := storage.EdgeQuery{Vertex: hub, Direction: direction, Limit: pageSize}
			for pages := 1; ; pages++ {
				page, err := store.Edges(t.Context(), query)
				if err != nil {
					t.Fatalf("Edges(%+v): %v", query, err)
				}
				if len(page.Edges) > pageSize {
					t.Fatalf("%s, %d edges: page %d holds %d edges, more than the limit of %d", direction, count, pages, len(page.Edges), pageSize)
				}
				got = append(got, page.Edges...)
				if page.Next == "" {
					break
				}
				if len(page.Edges) == 0 {
					t.Fatalf("%s, %d edges: page %d is empty yet says there is more", direction, count, pages)
				}
				if len(got) >= len(want) {
					t.Fatalf("%s, %d edges: page %d returned the last edge yet says there is more", direction, count, pages)
				}
				query.Cursor = page.Next
			}
			requireEdges(t, got, want...)

			// The same, restricted to one kind.
			ofKind := listEdges(t, store, storage.EdgeQuery{Vertex: hub, Direction: direction, Kind: graph.LinksTo, Limit: 3})
			var wantOfKind []graph.Edge
			for _, e := range want {
				if e.Kind == graph.LinksTo {
					wantOfKind = append(wantOfKind, e)
				}
			}
			requireEdges(t, ofKind, wantOfKind...)
		}
	}
}

func testEdgesDefaultPageSize(t *testing.T, store storage.Store, _ int) {
	hub := graph.ID("doc:s:hub")
	edges := spokes(hub, graph.Out, 2*storage.DefaultPageSize+300)
	putEdges(t, store, edges...)

	first, err := store.Edges(t.Context(), storage.EdgeQuery{Vertex: hub, Direction: graph.Out})
	if err != nil {
		t.Fatalf("Edges: %v", err)
	}
	if len(first.Edges) == 0 || len(first.Edges) > storage.DefaultPageSize {
		t.Errorf("with no Limit the first page holds %d edges, want between 1 and %d", len(first.Edges), storage.DefaultPageSize)
	}
	if first.Next == "" {
		t.Errorf("the first page of %d edges claims to be the last", len(edges))
	}

	all := listEdges(t, store, storage.EdgeQuery{Vertex: hub, Direction: graph.Out})
	requireEdges(t, all, inListingOrder(edges, graph.Out)...)
}

func testPutEdgesIsIdempotent(t *testing.T, store storage.Store, _ int) {
	a, b := graph.ID("doc:s:a"), graph.ID("doc:s:b")
	edge := graph.Edge{From: a, Kind: graph.LinksTo, To: b, Summary: "first wording"}

	putEdges(t, store, edge)
	putEdges(t, store, edge)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.Out}), edge)

	// Putting the edge again with a new summary rewords it, from both ends.
	reworded := edge
	reworded.Summary = "second wording"
	putEdges(t, store, reworded)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.Out}), reworded)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: b, Direction: graph.In}), reworded)

	// The same edge twice in one call: the later one wins.
	final := edge
	final.Summary = "third wording"
	putEdges(t, store, edge, final)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.Out}), final)
}

func testDeleteEdges(t *testing.T, store storage.Store, _ int) {
	a, b, c := graph.ID("doc:s:a"), graph.ID("doc:s:b"), graph.ID("doc:s:c")
	ab := graph.Edge{From: a, Kind: graph.LinksTo, To: b, Summary: "stored wording"}
	ac := graph.Edge{From: a, Kind: graph.LinksTo, To: c}
	childAB := graph.Edge{From: a, Kind: graph.ChildOf, To: b}
	putEdges(t, store, ab, ac, childAB)

	// The summary is not part of an edge's identity, so a different one
	// still names the stored edge. Edges that are not stored are ignored.
	err := store.DeleteEdges(t.Context(), []graph.Edge{
		{From: a, Kind: graph.LinksTo, To: b, Summary: "some other wording"},
		{From: c, Kind: graph.LinksTo, To: a},
	})
	if err != nil {
		t.Fatalf("DeleteEdges: %v", err)
	}

	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.Out}), childAB, ac)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: b, Direction: graph.In}), childAB)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: c, Direction: graph.In}), ac)

	if err := store.DeleteEdges(t.Context(), nil); err != nil {
		t.Errorf("DeleteEdges(nil): %v", err)
	}
}

func testEdgesToMissingVertices(t *testing.T, store storage.Store, _ int) {
	// Neither end is a stored vertex: a ticket may link to a pull request
	// that has not been ingested yet.
	edge := graph.Edge{From: "doc:jira:PAY-7", Kind: graph.LinksTo, To: "doc:github:acme/api#42"}
	putEdges(t, store, edge)

	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: edge.From, Direction: graph.Out}), edge)
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: edge.To, Direction: graph.In}), edge)
	if got := getVertices(t, store, edge.From, edge.To); len(got) != 0 {
		t.Errorf("putting an edge created vertices: %v", got)
	}
}

func testRejectsInvalidEdgesAndQueries(t *testing.T, store storage.Store, _ int) {
	a, b := graph.ID("doc:s:a"), graph.ID("doc:s:b")
	valid := graph.Edge{From: a, Kind: graph.LinksTo, To: b}

	badEdges := map[string]graph.Edge{
		"empty From":     {Kind: graph.LinksTo, To: b},
		"malformed To":   {From: a, Kind: graph.LinksTo, To: "b"},
		"empty kind":     {From: a, To: b},
		"malformed kind": {From: a, Kind: "Links To", To: b},
	}
	for name, bad := range badEdges {
		if err := store.PutEdges(t.Context(), []graph.Edge{valid, bad}); err == nil {
			t.Errorf("%s: PutEdges accepted an invalid edge", name)
		}
	}
	// A rejected batch must leave nothing behind, not even its valid part.
	requireEdges(t, listEdges(t, store, storage.EdgeQuery{Vertex: a, Direction: graph.Out}))

	badQueries := map[string]storage.EdgeQuery{
		"no direction":      {Vertex: a},
		"unknown direction": {Vertex: a, Direction: graph.Direction(7)},
		"malformed vertex":  {Vertex: "a", Direction: graph.Out},
		"malformed kind":    {Vertex: a, Direction: graph.Out, Kind: "Links To"},
		"negative limit":    {Vertex: a, Direction: graph.Out, Limit: -1},
	}
	for name, query := range badQueries {
		if _, err := store.Edges(t.Context(), query); err == nil {
			t.Errorf("%s: Edges accepted an invalid query", name)
		}
	}

	for _, cursor := range []string{"!!! not a cursor !!!", "bm90IGpzb24"} { // the second is base64 for "not json"
		_, err := store.Edges(t.Context(), storage.EdgeQuery{Vertex: a, Direction: graph.Out, Cursor: cursor})
		if !errors.Is(err, storage.ErrBadCursor) {
			t.Errorf("cursor %q: got error %v, want storage.ErrBadCursor", cursor, err)
		}
	}
}

func testAwkwardText(t *testing.T, store storage.Store, dimensions int) {
	// Text that trips naive quoting, escaping or pattern matching.
	const awkward = "it's \"quoted\" \\ back\\slash, 100% _done_;\n-- drop table edges; 🙂 日本語"

	ids := []graph.ID{
		"doc:github:acme/api#42",
		"doc:web:https://example.com/a?b=c&d=%20e#frag",
		"doc:odd:it's \"quoted\" \\ back_slash % percent",
		"doc:unicode:日本語/ドキュメント",
		"doc:spaced:a key with spaces",
	}
	topic := &graph.Topic{ID: "topic:c++/100%_done", Name: awkward, Summary: awkward}

	vertices := []graph.Vertex{topic}
	for _, id := range ids {
		document := sampleDocument(id, dimensions)
		document.Title = awkward
		document.Summary = awkward
		document.URL = awkward
		vertices = append(vertices, document)
	}
	putVertices(t, store, vertices...)

	got := getVertices(t, store, append(ids, topic.ID)...)
	for _, v := range vertices {
		requireVertex(t, got, v)
	}

	// Chain the awkward IDs together and hang them all off the topic.
	var edges []graph.Edge
	for i, id := range ids {
		edges = append(edges, graph.Edge{From: id, Kind: graph.TaggedWith, To: topic.ID, Summary: awkward})
		if i > 0 {
			edges = append(edges, graph.Edge{From: ids[i-1], Kind: graph.LinksTo, To: id, Summary: awkward})
		}
	}
	putEdges(t, store, edges...)

	// One edge per page, so that every awkward ID passes through a cursor.
	tagged := listEdges(t, store, storage.EdgeQuery{Vertex: topic.ID, Direction: graph.In, Limit: 1})
	var wantTagged []graph.Edge
	for _, e := range edges {
		if e.Kind == graph.TaggedWith {
			wantTagged = append(wantTagged, e)
		}
	}
	requireEdges(t, tagged, inListingOrder(wantTagged, graph.In)...)

	for _, id := range ids {
		var want []graph.Edge
		for _, e := range edges {
			if e.From == id {
				want = append(want, e)
			}
		}
		out := listEdges(t, store, storage.EdgeQuery{Vertex: id, Direction: graph.Out, Limit: 1})
		requireEdges(t, out, inListingOrder(want, graph.Out)...)
	}
}

// --- the embedding space ------------------------------------------------------

// Two embedding spaces, as an embedder would name them.
const (
	spaceA = "acme/embedder-a@1f2e; query prompt search; document prompt none; 3 dimensions"
	spaceB = "acme/embedder-b@9c0d; query prompt none; document prompt none; 3 dimensions"
)

func testEmbeddingSpaceStartsUnclaimed(t *testing.T, store storage.Store, _ int) {
	if got := embeddingSpace(t, store); got != "" {
		t.Errorf("a new graph is in the embedding space %q, want none", got)
	}
}

func testFirstClaimOfASpaceIsRecorded(t *testing.T, store storage.Store, _ int) {
	if err := store.ClaimEmbeddingSpace(t.Context(), spaceA); err != nil {
		t.Fatalf("ClaimEmbeddingSpace: %v", err)
	}
	if got := embeddingSpace(t, store); got != spaceA {
		t.Errorf("after the first claim the graph is in %q, want %q", got, spaceA)
	}

	// The same claim again is welcome, however often, and changes nothing.
	for range 3 {
		if err := store.ClaimEmbeddingSpace(t.Context(), spaceA); err != nil {
			t.Fatalf("claiming the same space again: %v", err)
		}
	}
	if got := embeddingSpace(t, store); got != spaceA {
		t.Errorf("after the same claim again the graph is in %q, want %q", got, spaceA)
	}
}

func testClaimOfAnotherSpaceIsRefused(t *testing.T, store storage.Store, _ int) {
	if err := store.ClaimEmbeddingSpace(t.Context(), spaceA); err != nil {
		t.Fatalf("ClaimEmbeddingSpace: %v", err)
	}

	err := store.ClaimEmbeddingSpace(t.Context(), spaceB)

	if !errors.Is(err, storage.ErrOtherSpace) {
		t.Fatalf("claiming another space returned %v, want an error that wraps ErrOtherSpace", err)
	}
	if !strings.Contains(err.Error(), spaceA) || !strings.Contains(err.Error(), spaceB) {
		t.Errorf("the error does not name both spaces: %v", err)
	}
	// The record is written once: the refused claim has not touched it.
	if got := embeddingSpace(t, store); got != spaceA {
		t.Errorf("after a refused claim the graph is in %q, want %q", got, spaceA)
	}
	if err := store.ClaimEmbeddingSpace(t.Context(), spaceA); err != nil {
		t.Errorf("after a refused claim, the recorded space can no longer be claimed: %v", err)
	}
}

func testSpaceWithoutANameCannotBeClaimed(t *testing.T, store storage.Store, _ int) {
	for _, nameless := range []string{"", "  ", "\n\t"} {
		if err := store.ClaimEmbeddingSpace(t.Context(), nameless); err == nil {
			t.Errorf("the space %q was claimed", nameless)
		}
	}
	if got := embeddingSpace(t, store); got != "" {
		t.Errorf("a space without a name was recorded as %q", got)
	}
}

// Several programs may start on a new graph at the same moment, each with
// its own embedder. Exactly one of them may win the graph.
func testRivalClaimsLeaveOneWinner(t *testing.T, store storage.Store, _ int) {
	const rivals = 10
	name := func(rival int) string { return fmt.Sprintf("acme/rival-%d@0; 3 dimensions", rival) }

	failures := make([]error, rivals)
	var group sync.WaitGroup
	for rival := range rivals {
		group.Go(func() { failures[rival] = store.ClaimEmbeddingSpace(t.Context(), name(rival)) })
	}
	group.Wait()

	winner := -1
	for rival, err := range failures {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("rivals %d and %d both claimed the graph", winner, rival)
		case err == nil:
			winner = rival
		case !errors.Is(err, storage.ErrOtherSpace):
			t.Errorf("rival %d failed with %v, want an error that wraps ErrOtherSpace", rival, err)
		}
	}
	if winner < 0 {
		t.Fatal("no rival claimed the graph")
	}
	if got := embeddingSpace(t, store); got != name(winner) {
		t.Errorf("the graph is in %q, and the claim that succeeded was of %q", got, name(winner))
	}
}

// --- helpers ----------------------------------------------------------------

func embeddingSpace(t *testing.T, store storage.Store) string {
	t.Helper()
	space, err := store.EmbeddingSpace(t.Context())
	if err != nil {
		t.Fatalf("EmbeddingSpace: %v", err)
	}
	return space
}

// Two fixed moments. The second is in a zone other than UTC: a store must
// return the same instant, whatever zone it hands it back in.
var (
	createdAt = time.Date(2026, time.March, 14, 9, 26, 53, 589_793_000, time.UTC)
	updatedAt = time.Date(2026, time.June, 28, 18, 3, 7, 0, time.FixedZone("", 2*60*60))
)

// vector returns an embedding of the given length that starts with values
// and is zero elsewhere.
func vector(dimensions int, values ...float32) []float32 {
	v := make([]float32, dimensions)
	copy(v, values)
	return v
}

// sampleDocument returns a document with every field filled in.
func sampleDocument(id graph.ID, dimensions int) *graph.Document {
	return &graph.Document{
		ID:        id,
		Type:      graph.TypePullRequest,
		Source:    "github",
		URL:       "https://github.com/acme/api/pull/42",
		Title:     "Retry refunds that time out",
		Summary:   "Adds a retry with backoff around the refund call.",
		Embedding: vector(dimensions, 0.25, -0.5, 1),
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}
}

// spokes returns count edges between hub and count other documents, running
// in the given direction as seen from the hub and alternating between two
// kinds so that a listing crosses from one kind to the next.
func spokes(hub graph.ID, direction graph.Direction, count int) []graph.Edge {
	edges := make([]graph.Edge, 0, count)
	for i := range count {
		kind := graph.LinksTo
		if i%3 == 0 {
			kind = graph.ChildOf
		}
		other := graph.DocumentID("s", fmt.Sprintf("%s/spoke-%05d", hub.Key(), i))
		edge := graph.Edge{From: hub, Kind: kind, To: other, Summary: fmt.Sprintf("spoke %d", i)}
		if direction == graph.In {
			edge.From, edge.To = other, hub
		}
		edges = append(edges, edge)
	}
	return edges
}

// inListingOrder returns the edges sorted the way Edges must return them
// for a query in the given direction: by kind, then by the far vertex.
func inListingOrder(edges []graph.Edge, direction graph.Direction) []graph.Edge {
	sorted := slices.Clone(edges)
	slices.SortFunc(sorted, func(a, b graph.Edge) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Far(direction), b.Far(direction)))
	})
	return sorted
}

func putVertices(t *testing.T, store storage.Store, vertices ...graph.Vertex) {
	t.Helper()
	if err := store.PutVertices(t.Context(), vertices); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}
}

func getVertices(t *testing.T, store storage.Store, ids ...graph.ID) map[graph.ID]graph.Vertex {
	t.Helper()
	got, err := store.GetVertices(t.Context(), ids)
	if err != nil {
		t.Fatalf("GetVertices: %v", err)
	}
	return got
}

func putEdges(t *testing.T, store storage.Store, edges ...graph.Edge) {
	t.Helper()
	if err := store.PutEdges(t.Context(), edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
}

// listEdges follows a listing to its end and returns every edge in it.
func listEdges(t *testing.T, store storage.Store, query storage.EdgeQuery) []graph.Edge {
	t.Helper()
	var all []graph.Edge
	for pages := 0; ; pages++ {
		if pages > 100_000 {
			t.Fatalf("Edges(%+v) never reached a last page", query)
		}
		page, err := store.Edges(t.Context(), query)
		if err != nil {
			t.Fatalf("Edges(%+v): %v", query, err)
		}
		all = append(all, page.Edges...)
		if page.Next == "" {
			return all
		}
		query.Cursor = page.Next
	}
}

// requireEdges fails the test unless got is exactly want, in order.
func requireEdges(t *testing.T, got []graph.Edge, want ...graph.Edge) {
	t.Helper()
	if slices.Equal(got, want) {
		return
	}
	if len(got) != len(want) {
		t.Fatalf("got %d edges, want %d\n got: %v\nwant: %v", len(got), len(want), abbreviate(got), abbreviate(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("edge %d of %d differs\n got: %+v\nwant: %+v", i, len(got), got[i], want[i])
		}
	}
}

// abbreviate keeps failure messages readable when a listing is long.
func abbreviate(edges []graph.Edge) []graph.Edge {
	if len(edges) > 12 {
		return edges[:12]
	}
	return edges
}

// requireVertex fails the test unless got holds a vertex equal to want.
func requireVertex(t *testing.T, got map[graph.ID]graph.Vertex, want graph.Vertex) {
	t.Helper()
	have, ok := got[want.VertexID()]
	if !ok {
		t.Fatalf("vertex %s is missing", want.VertexID())
	}
	if !sameVertex(have, want) {
		t.Fatalf("vertex %s differs\n got: %+v\nwant: %+v", want.VertexID(), have, want)
	}
}

// sameVertex compares two vertices field by field. Times are compared as
// instants, and a nil embedding equals an empty one.
func sameVertex(a, b graph.Vertex) bool {
	switch a := a.(type) {
	case *graph.Document:
		b, ok := b.(*graph.Document)
		return ok && a != nil && b != nil &&
			a.ID == b.ID && a.Type == b.Type && a.Source == b.Source &&
			a.URL == b.URL && a.Title == b.Title && a.Summary == b.Summary &&
			slices.Equal(a.Embedding, b.Embedding) &&
			a.CreatedAt.Equal(b.CreatedAt) && a.UpdatedAt.Equal(b.UpdatedAt)
	case *graph.Topic:
		b, ok := b.(*graph.Topic)
		return ok && a != nil && b != nil &&
			a.ID == b.ID && a.Name == b.Name && a.Summary == b.Summary &&
			slices.Equal(a.Embedding, b.Embedding)
	}
	return false
}

package storagetest

import (
	"math"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// RunSearcher checks a DocumentSearcher against its contract. open must
// return a fresh, empty store together with the searcher that reads from it;
// for most backends the two are the same value. dimensions is the length of
// the embeddings the store accepts, and must be at least 3.
func RunSearcher(t *testing.T, dimensions int, open func(t *testing.T) (storage.Store, storage.DocumentSearcher)) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, store storage.Store, searcher storage.DocumentSearcher, dimensions int)
	}{
		{"NearestFirst", testNearestFirst},
		{"OnlyDocumentsWithEmbeddings", testOnlyDocumentsWithEmbeddings},
		{"FewerThanK", testFewerThanK},
		{"RejectsBadArguments", testSearchRejectsBadArguments},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, searcher := open(t)
			test.run(t, store, searcher, dimensions)
		})
	}
}

// searchCorpus stores four documents at known angles from the first axis and
// returns their IDs from nearest to farthest for a query along that axis,
// with the cosine distance each should be reported at.
func searchCorpus(t *testing.T, store storage.Store, dimensions int) (ids []graph.ID, distances []float32) {
	t.Helper()
	documents := []*graph.Document{
		{ID: "doc:s:same", Embedding: vector(dimensions, 2, 0, 0)},       // same direction, longer
		{ID: "doc:s:diagonal", Embedding: vector(dimensions, 1, 1, 0)},   // 45 degrees away
		{ID: "doc:s:orthogonal", Embedding: vector(dimensions, 0, 3, 0)}, // 90 degrees away
		{ID: "doc:s:opposite", Embedding: vector(dimensions, -1, 0, 0)},  // pointing the other way
	}
	for _, document := range documents {
		putVertices(t, store, document)
		ids = append(ids, document.ID)
	}
	return ids, []float32{0, float32(1 - 1/math.Sqrt2), 1, 2}
}

func testNearestFirst(t *testing.T, store storage.Store, searcher storage.DocumentSearcher, dimensions int) {
	ids, distances := searchCorpus(t, store, dimensions)
	query := vector(dimensions, 1, 0, 0)

	matches, err := searcher.NearestDocuments(t.Context(), query, len(ids))
	if err != nil {
		t.Fatalf("NearestDocuments: %v", err)
	}
	requireMatches(t, matches, ids, distances)

	// Asking for fewer returns the nearest ones.
	top2, err := searcher.NearestDocuments(t.Context(), query, 2)
	if err != nil {
		t.Fatalf("NearestDocuments: %v", err)
	}
	requireMatches(t, top2, ids[:2], distances[:2])
}

func testOnlyDocumentsWithEmbeddings(t *testing.T, store storage.Store, searcher storage.DocumentSearcher, dimensions int) {
	ids, distances := searchCorpus(t, store, dimensions)

	// Neither of these may show up: a document that was never embedded, and
	// a topic whose embedding happens to match the query exactly.
	putVertices(t, store,
		&graph.Document{ID: "doc:s:not-embedded", Title: "No embedding"},
		&graph.Topic{ID: "topic:matching", Name: "Matching", Embedding: vector(dimensions, 1, 0, 0)},
	)

	matches, err := searcher.NearestDocuments(t.Context(), vector(dimensions, 1, 0, 0), 10)
	if err != nil {
		t.Fatalf("NearestDocuments: %v", err)
	}
	requireMatches(t, matches, ids, distances)
}

func testFewerThanK(t *testing.T, store storage.Store, searcher storage.DocumentSearcher, dimensions int) {
	query := vector(dimensions, 1, 0, 0)

	matches, err := searcher.NearestDocuments(t.Context(), query, 5)
	if err != nil {
		t.Fatalf("NearestDocuments on an empty store: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("an empty store returned %d matches", len(matches))
	}

	ids, distances := searchCorpus(t, store, dimensions)
	matches, err = searcher.NearestDocuments(t.Context(), query, 100)
	if err != nil {
		t.Fatalf("NearestDocuments: %v", err)
	}
	requireMatches(t, matches, ids, distances)
}

func testSearchRejectsBadArguments(t *testing.T, store storage.Store, searcher storage.DocumentSearcher, dimensions int) {
	searchCorpus(t, store, dimensions)
	query := vector(dimensions, 1, 0, 0)

	if _, err := searcher.NearestDocuments(t.Context(), query, 0); err == nil {
		t.Errorf("k = 0 was accepted")
	}
	if _, err := searcher.NearestDocuments(t.Context(), nil, 3); err == nil {
		t.Errorf("an empty query was accepted")
	}
	if _, err := searcher.NearestDocuments(t.Context(), vector(dimensions+1, 1), 3); err == nil {
		t.Errorf("a query with the wrong number of dimensions was accepted")
	}
}

// requireMatches fails the test unless the matches name exactly the given
// documents, in order, at the given distances.
func requireMatches(t *testing.T, matches []storage.Match, ids []graph.ID, distances []float32) {
	t.Helper()
	if len(matches) != len(ids) {
		t.Fatalf("got %d matches, want %d: %+v", len(matches), len(ids), matches)
	}
	for i, match := range matches {
		if match.ID != ids[i] {
			t.Fatalf("match %d is %s, want %s (all: %+v)", i, match.ID, ids[i], matches)
		}
		if math.Abs(float64(match.Distance-distances[i])) > 1e-4 {
			t.Errorf("match %d (%s) is at distance %v, want %v", i, match.ID, match.Distance, distances[i])
		}
	}
}

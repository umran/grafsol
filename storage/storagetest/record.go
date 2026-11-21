package storagetest

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// RunRecord checks a Record against its contract. open must return a fresh,
// empty record every time it is called, set up so that Recorded returns
// pageSize documents per page (a small number keeps the paging tests quick).
func RunRecord(t *testing.T, pageSize int, open func(t *testing.T) storage.Record) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, record storage.Record, pageSize int)
	}{
		{"SaidRoundTrip", testSaidRoundTrip},
		{"PutSaidReplaces", testPutSaidReplaces},
		{"SaidSkipsMissing", testSaidSkipsMissing},
		{"DeleteSaid", testDeleteSaid},
		{"PutSaidRejectsInvalid", testPutSaidRejectsInvalid},
		{"StoredSaidIsACopy", testStoredSaidIsACopy},
		{"SaidAwkwardText", testSaidAwkwardText},
		{"RecordedIsPagedAndOrdered", testRecordedIsPagedAndOrdered},
		{"RecordedWhileDeleting", testRecordedWhileDeleting},
		{"RecordedRejectsABadCursor", testRecordedRejectsABadCursor},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, open(t), pageSize)
		})
	}
}

func testSaidRoundTrip(t *testing.T, record storage.Record, _ int) {
	full := sampleSaid("doc:github:acme/api#42")
	bare := storage.Said{Document: graph.Document{ID: "doc:jira:PAY-7"}} // every optional field left empty
	putSaid(t, record, full, bare)

	got := said(t, record, full.Document.ID, bare.Document.ID)
	if len(got) != 2 {
		t.Fatalf("Said returned %d records, want 2", len(got))
	}
	requireSaid(t, got, full)
	requireSaid(t, got, bare)
}

func testPutSaidReplaces(t *testing.T, record storage.Record, _ int) {
	id := graph.ID("doc:github:acme/api#42")
	putSaid(t, record, sampleSaid(id))

	// The document was edited down to almost nothing. None of what was said
	// before may linger.
	later := storage.Said{
		Document: graph.Document{ID: id, Source: "github", Title: "Retitled"},
		Labels:   []graph.Label{{Name: "wontfix"}},
	}
	putSaid(t, record, later)
	requireSaid(t, said(t, record, id), later)

	// Storing the same thing again changes nothing.
	putSaid(t, record, later)
	requireSaid(t, said(t, record, id), later)
	requireIDs(t, recorded(t, record), id)
}

func testSaidSkipsMissing(t *testing.T, record storage.Record, _ int) {
	stored := sampleSaid("doc:github:acme/api#42")
	putSaid(t, record, stored)

	got := said(t, record, "doc:github:acme/api#1", stored.Document.ID, "doc:github:acme/api#2", "topic:bug")
	if len(got) != 1 {
		t.Fatalf("Said returned %d records, want only the one that is stored", len(got))
	}
	requireSaid(t, got, stored)

	if got := said(t, record); len(got) != 0 {
		t.Errorf("Said of no documents returned %d records", len(got))
	}
}

func testDeleteSaid(t *testing.T, record storage.Record, _ int) {
	kept, gone := sampleSaid("doc:github:acme/api#1"), sampleSaid("doc:github:acme/api#2")
	putSaid(t, record, kept, gone)

	if err := record.DeleteSaid(t.Context(), gone.Document.ID); err != nil {
		t.Fatalf("DeleteSaid: %v", err)
	}
	// Deleting what is not there, once more or for the first time, is fine.
	for _, id := range []graph.ID{gone.Document.ID, "doc:github:acme/api#never"} {
		if err := record.DeleteSaid(t.Context(), id); err != nil {
			t.Fatalf("DeleteSaid(%s), which is not stored: %v", id, err)
		}
	}

	got := said(t, record, kept.Document.ID, gone.Document.ID)
	if len(got) != 1 {
		t.Fatalf("after the delete Said returned %d records, want 1", len(got))
	}
	requireSaid(t, got, kept)
	requireIDs(t, recorded(t, record), kept.Document.ID)
}

func testPutSaidRejectsInvalid(t *testing.T, record storage.Record, _ int) {
	const id = graph.ID("doc:github:acme/api#42")
	tests := []struct {
		name   string
		change func(*storage.Said)
	}{
		{"no document", func(s *storage.Said) { s.Document.ID = "" }},
		{"a topic where the document should be", func(s *storage.Said) { s.Document.ID = "topic:bug" }},
		{"a reference to nothing", func(s *storage.Said) {
			s.References = append(s.References, graph.Reference{Form: graph.FormMention})
		}},
		{"a reference of no form", func(s *storage.Said) {
			s.References = append(s.References, graph.Reference{Target: "doc:github:acme/api#1"})
		}},
		{"a reference to a malformed ID", func(s *storage.Said) {
			s.References = append(s.References, graph.Reference{Target: "nowhere", Form: graph.FormMention})
		}},
		{"a label with no name", func(s *storage.Said) { s.Labels = append(s.Labels, graph.Label{Description: "nameless"}) }},
		{"an edge from another document", func(s *storage.Said) {
			s.Edges = append(s.Edges, graph.Edge{From: "doc:github:acme/api#1", Kind: graph.LinksTo, To: id})
		}},
		{"an edge of a malformed kind", func(s *storage.Said) {
			s.Edges = append(s.Edges, graph.Edge{From: id, Kind: "Links To", To: "doc:github:acme/api#1"})
		}},
		{"a topic that is a document", func(s *storage.Said) { s.Topics = append(s.Topics, graph.Topic{ID: "doc:github:acme/api#1"}) }},
	}
	for _, test := range tests {
		invalid := sampleSaid(id)
		test.change(&invalid)
		if err := record.PutSaid(t.Context(), invalid); err == nil {
			t.Errorf("PutSaid accepted %s", test.name)
		}
	}
	if got := recorded(t, record); len(got) != 0 {
		t.Errorf("after only refused puts the record lists %v", got)
	}
}

func testStoredSaidIsACopy(t *testing.T, record storage.Record, _ int) {
	id := graph.ID("doc:github:acme/api#42")
	original := sampleSaid(id)
	putSaid(t, record, original)

	// Scribble over everything the caller still holds.
	scribble := func(s storage.Said) {
		s.Document.Embedding[0] = 99
		s.References[0].Context = "changed"
		s.Labels[0].Name = "changed"
		s.Edges[0].Summary = "changed"
		s.Topics[0].Name = "changed"
		s.Topics[0].Embedding[0] = 99
	}
	scribble(original)
	first := said(t, record, id)
	requireSaid(t, first, sampleSaid(id))

	// And over what a read handed back.
	scribble(first[id])
	requireSaid(t, said(t, record, id), sampleSaid(id))
}

func testSaidAwkwardText(t *testing.T, record storage.Record, pageSize int) {
	// Text that trips naive quoting, escaping or pattern matching.
	const awkward = "it's \"quoted\" \\ back\\slash, 100% _done_;\n-- drop table said; 🙂 日本語 <b>&amp;</b> {\"json\": [1]}"

	ids := []graph.ID{
		"doc:github:acme/api#42",
		"doc:web:https://example.com/a?b=c&d=%20e#frag",
		"doc:odd:it's \"quoted\" \\ back_slash % percent",
		"doc:unicode:日本語/ドキュメント",
		"doc:spaced:a key with spaces",
	}
	var all []storage.Said
	for _, id := range ids {
		all = append(all, storage.Said{
			Document:   graph.Document{ID: id, Source: id.Source(), Title: awkward, Summary: awkward, URL: awkward},
			References: []graph.Reference{{Target: ids[0], Form: awkward, Written: awkward, Context: awkward}, {URL: awkward, Form: graph.FormMention}},
			Labels:     []graph.Label{{Name: awkward, Description: awkward}},
			Edges:      []graph.Edge{{From: id, Kind: graph.InTopic, To: "topic:c++/100%_done", Summary: awkward}},
			Topics:     []graph.Topic{{ID: "topic:c++/100%_done", Name: awkward, Summary: awkward}},
		})
	}
	putSaid(t, record, all...)

	got := said(t, record, ids...)
	for _, want := range all {
		requireSaid(t, got, want)
	}
	// Every awkward ID passes through a cursor when the pages are small.
	sorted := slices.Sorted(slices.Values(ids))
	requireIDs(t, recorded(t, record), sorted...)
}

func testRecordedIsPagedAndOrdered(t *testing.T, record storage.Record, pageSize int) {
	if got := recorded(t, record); len(got) != 0 {
		t.Fatalf("an empty record lists %v", got)
	}

	// Exactly three pages' worth, stored in no particular order. The IDs
	// are chosen so that byte order and "natural" order disagree: "#10"
	// sorts before "#9", and an upper-case letter before a lower-case one.
	count := 3 * pageSize
	var want []graph.ID
	for i := range count {
		repository := []string{"acme/api", "Acme/Zed", "acme/web"}[i%3]
		want = append(want, graph.DocumentID("github", fmt.Sprintf("%s#%d", repository, i+1)))
	}
	shuffled := slices.Clone(want)
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	for _, id := range shuffled {
		putSaid(t, record, storage.Said{Document: graph.Document{ID: id}})
	}
	slices.Sort(want)

	var got []graph.ID
	pages, cursor := 0, ""
	for {
		ids, next, err := record.Recorded(t.Context(), cursor)
		if err != nil {
			t.Fatalf("Recorded: %v", err)
		}
		pages++
		if len(ids) != pageSize {
			t.Fatalf("page %d holds %d documents, want %d: every page of this listing is full", pages, len(ids), pageSize)
		}
		got = append(got, ids...)
		if next == "" {
			break
		}
		cursor = next
	}
	// With nothing left after the third page, its cursor is empty: there is
	// no fourth, empty page to fetch.
	if pages != 3 {
		t.Errorf("the listing took %d pages, want 3", pages)
	}
	requireIDs(t, got, want...)
}

func testRecordedWhileDeleting(t *testing.T, record storage.Record, pageSize int) {
	var all []graph.ID
	for i := range 3 * pageSize {
		id := graph.DocumentID("github", fmt.Sprintf("acme/api#%03d", i))
		all = append(all, id)
		putSaid(t, record, storage.Said{Document: graph.Document{ID: id}})
	}

	// A caller that deletes each document as the listing hands it over, and
	// adds a new one behind the listing's back, still sees every document
	// that was there throughout exactly once.
	seen := make(map[graph.ID]int)
	cursor := ""
	for page := 0; ; page++ {
		ids, next, err := record.Recorded(t.Context(), cursor)
		if err != nil {
			t.Fatalf("Recorded: %v", err)
		}
		for _, id := range ids {
			seen[id]++
			if err := record.DeleteSaid(t.Context(), id); err != nil {
				t.Fatalf("DeleteSaid: %v", err)
			}
		}
		if page == 0 {
			// Sorts before everything, so the listing has already passed it.
			putSaid(t, record, storage.Said{Document: graph.Document{ID: "doc:github:acme/api#"}})
		}
		if next == "" {
			break
		}
		cursor = next
	}
	for _, id := range all {
		if seen[id] != 1 {
			t.Errorf("%s was listed %d times, want once", id, seen[id])
		}
	}
	if len(seen) != len(all) {
		t.Errorf("the listing returned %d documents, want %d", len(seen), len(all))
	}
}

func testRecordedRejectsABadCursor(t *testing.T, record storage.Record, _ int) {
	putSaid(t, record, sampleSaid("doc:github:acme/api#42"))
	for _, cursor := range []string{"not a cursor", "!!!", "e30x"} {
		if _, _, err := record.Recorded(t.Context(), cursor); !errors.Is(err, storage.ErrBadCursor) {
			t.Errorf("Recorded with the cursor %q failed with %v, want an error that wraps ErrBadCursor", cursor, err)
		}
	}
}

// --- helpers ------------------------------------------------------------------

// sampleSaid returns what a source might say of a document, with one of
// everything in it.
func sampleSaid(id graph.ID) storage.Said {
	const line = "Fixes #12 and acme/web#3, see https://acme.atlassian.net/browse/PAY-7."
	return storage.Said{
		Document: *sampleDocument(id, 3),
		References: []graph.Reference{
			{Target: "doc:github:acme/api#12", Form: graph.FormMention, Written: "#12", Context: line},
			{Target: "doc:github:acme/web#3", Form: graph.FormMention, Written: "acme/web#3", Context: line},
			// An address that another source recognized: both are kept.
			{Target: "doc:jira:PAY-7", URL: "https://acme.atlassian.net/browse/PAY-7", Form: graph.FormMention, Written: "https://acme.atlassian.net/browse/PAY-7", Context: line},
			// An address that no source recognized.
			{URL: "https://example.com/elsewhere", Form: graph.FormMention, Written: "https://example.com/elsewhere", Context: "More at https://example.com/elsewhere"},
			{Target: "doc:github:acme/api#7", Form: graph.FormParent, Context: "sub-issue of"},
			{Target: "doc:jira:PAY-9", Form: "jira:blocks"},
		},
		Labels: []graph.Label{
			{Name: "Bug", Description: "Something is not working"},
			{Name: "area/billing"},
		},
		Edges: []graph.Edge{
			{From: id, Kind: graph.InTopic, To: "topic:payments", Summary: "filed there by its source"},
			{From: id, Kind: "supersedes", To: "doc:github:acme/api#3"},
		},
		Topics: []graph.Topic{
			{ID: "topic:payments", Name: "Payments", Summary: "Money in and out.", Embedding: vector(3, 1, 0, 0.5)},
		},
	}
}

func putSaid(t *testing.T, record storage.Record, all ...storage.Said) {
	t.Helper()
	for _, one := range all {
		if err := record.PutSaid(t.Context(), one); err != nil {
			t.Fatalf("PutSaid(%s): %v", one.Document.ID, err)
		}
	}
}

func said(t *testing.T, record storage.Record, ids ...graph.ID) map[graph.ID]storage.Said {
	t.Helper()
	found, err := record.Said(t.Context(), ids)
	if err != nil {
		t.Fatalf("Said: %v", err)
	}
	return found
}

// recorded returns every document the record lists, however many pages that
// takes.
func recorded(t *testing.T, record storage.Record) []graph.ID {
	t.Helper()
	var all []graph.ID
	cursor := ""
	for {
		ids, next, err := record.Recorded(t.Context(), cursor)
		if err != nil {
			t.Fatalf("Recorded: %v", err)
		}
		all = append(all, ids...)
		if next == "" {
			return all
		}
		cursor = next
	}
}

func requireSaid(t *testing.T, got map[graph.ID]storage.Said, want storage.Said) {
	t.Helper()
	have, ok := got[want.Document.ID]
	if !ok {
		t.Fatalf("nothing is stored for %s", want.Document.ID)
	}
	if !sameSaid(have, want) {
		t.Fatalf("what was said of %s differs\n got: %+v\nwant: %+v", want.Document.ID, have, want)
	}
}

func sameSaid(a, b storage.Said) bool {
	return sameVertex(&a.Document, &b.Document) &&
		slices.Equal(a.References, b.References) &&
		slices.Equal(a.Labels, b.Labels) &&
		slices.Equal(a.Edges, b.Edges) &&
		slices.EqualFunc(a.Topics, b.Topics, func(x, y graph.Topic) bool { return sameVertex(&x, &y) })
}

package evaluation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/umran/grafsol/graph"
)

// The test set most tests here start from: a shop's slow checkout. Four
// documents, a topic and two edges; three questions, one of each sort that
// matters: answered by documents, answered only with the help of a link's
// note, and not answered at all.
const (
	slow      = graph.ID("doc:s:slow")      // an issue: checkout is slow
	cache     = graph.ID("doc:s:cache")     // the pull request that caused it
	dashboard = graph.ID("doc:s:dashboard") // on the subject, answers nothing
	rota      = graph.ID("doc:s:rota")      // unrelated
)

type object = map[string]any

func exampleGraph() object {
	return object{
		"documents": []any{
			object{"id": slow, "type": "issue", "source": "s", "url": "https://shop.test/issues/7",
				"title": "Checkout is slow", "summary": "Checkout p95 latency doubled on 3 March.",
				"created_at": "2026-03-04T09:00:00Z", "updated_at": "2026-03-05T09:00:00Z"},
			object{"id": cache, "type": "pull_request", "source": "s", "url": "https://shop.test/pull/9",
				"title": "Remove the price cache", "summary": "The price cache was removed on 3 March to fix stale prices. Every price is now read from the database.",
				"created_at": "2026-03-03T09:00:00Z", "updated_at": "2026-03-03T09:00:00Z"},
			object{"id": dashboard, "type": "page", "source": "s", "url": "https://shop.test/wiki/latency",
				"title": "Latency dashboard", "summary": "Describes the dashboard that graphs checkout latency.",
				"created_at": "2025-01-01T00:00:00Z", "updated_at": "2025-01-01T00:00:00Z"},
			object{"id": rota, "type": "page", "source": "s", "url": "https://shop.test/wiki/rota",
				"title": "Holiday rota", "summary": "Lists who is away in August.",
				"created_at": "2025-01-01T00:00:00Z", "updated_at": "2025-01-01T00:00:00Z"},
		},
		"topics": []any{
			object{"id": "topic:performance", "name": "performance", "summary": "Speed and latency."},
		},
		"edges": []any{
			object{"from": cache, "kind": "links_to", "to": slow, "summary": "This change caused the slowdown reported in the checkout issue."},
			object{"from": slow, "kind": "tagged_with", "to": "topic:performance", "summary": ""},
		},
	}
}

func exampleQuestions() object {
	return object{
		"context": "The web shop's engineering documents.",
		"questions": []any{
			object{
				"id": "slow-checkout", "question": "Why did checkout latency double in March?",
				"kind": "one", "part": "tune",
				"facts": []any{
					object{"fact": "The price cache was removed on 3 March.",
						"stated": []any{object{"in": cache, "quote": "The price cache was removed on 3 March"}}},
					object{"fact": "Every price is now read from the database.",
						"stated": []any{object{"in": cache, "quote": "Every price is now read from the database"}}},
					object{"fact": "Latency doubled on 3 March.", "extra": true,
						"stated": []any{object{"in": slow, "quote": "doubled on 3 March"}}},
				},
				"near": []any{dashboard},
			},
			object{
				"id": "which-change", "question": "Which change made the checkout slow?",
				"context": "A review of one incident.",
				"kind":    "link", "part": "tune",
				"facts": []any{
					object{"fact": "The removal of the price cache caused it.",
						"stated": []any{object{"on": object{"from": cache, "kind": "links_to", "to": slow}, "quote": "caused the slowdown"}}},
				},
			},
			object{
				"id": "rota-approver", "question": "Who approves the holiday rota?",
				"kind": "absent", "part": "check",
				"near": []any{rota},
				"note": "The rota lists who is away; nothing says who approves it.",
			},
		},
	}
}

// write puts a test set in a new folder and returns the folder.
func write(t *testing.T, graphFile, questionsFile any) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]any{"graph.json": graphFile, "questions.json": questionsFile} {
		encoded, err := json.MarshalIndent(content, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), encoded, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// example loads the example set, which must be sound.
func example(t *testing.T) Set {
	t.Helper()
	set, err := Load(write(t, exampleGraph(), exampleQuestions()))
	if err != nil {
		t.Fatalf("the example set does not load: %v", err)
	}
	return set
}

// question returns the question of the example set with that ID.
func question(t *testing.T, set Set, id string) Question {
	t.Helper()
	for _, q := range set.Questions {
		if q.ID == id {
			return q
		}
	}
	t.Fatalf("the set has no question %q", id)
	return Question{}
}

func TestLoadReadsTheGraphAndTheQuestions(t *testing.T) {
	set := example(t)

	if got := len(set.Graph.Documents); got != 4 {
		t.Errorf("%d documents were loaded, want 4", got)
	}
	if got := len(set.Graph.Topics); got != 1 || set.Graph.Topics[0].Name != "performance" {
		t.Errorf("the topics loaded are %+v", set.Graph.Topics)
	}
	if got := len(set.Graph.Edges); got != 2 || set.Graph.Edges[0].Summary == "" {
		t.Errorf("the edges loaded are %+v", set.Graph.Edges)
	}
	document := set.Graph.Documents[1]
	if document.ID != cache || document.Type != graph.TypePullRequest || document.Source != "s" ||
		document.URL != "https://shop.test/pull/9" || document.Title != "Remove the price cache" ||
		!strings.HasPrefix(document.Summary, "The price cache") || document.UpdatedAt.Year() != 2026 {
		t.Errorf("the second document was loaded as %+v", document)
	}

	// The questions come in the order of the file. One that gives no context
	// of its own takes the file's.
	var ids []string
	for _, q := range set.Questions {
		ids = append(ids, q.ID)
	}
	if want := []string{"slow-checkout", "which-change", "rota-approver"}; !slices.Equal(ids, want) {
		t.Fatalf("the questions loaded are %v, want %v", ids, want)
	}
	first := set.Questions[0]
	if first.Text != "Why did checkout latency double in March?" || first.Context != "The web shop's engineering documents." ||
		first.Kind != OneDocument || first.Part != Tune {
		t.Errorf("the first question was loaded as %+v", first)
	}
	if got := set.Questions[1].Context; got != "A review of one incident." {
		t.Errorf("a question's own context was replaced by %q", got)
	}
	if last := set.Questions[2]; last.Kind != Absent || last.Part != Check || last.Note == "" {
		t.Errorf("the last question was loaded as %+v", last)
	}

	// Facts, with where each is stated.
	if len(first.Facts) != 3 || first.Facts[0].Extra || !first.Facts[2].Extra {
		t.Fatalf("the first question's facts are %+v", first.Facts)
	}
	stating := first.Facts[0].Stated[0]
	if stating.Document != cache || stating.Edge != (graph.Edge{}) || stating.Quote != "The price cache was removed on 3 March" {
		t.Errorf("a fact stated in a document was loaded as %+v", stating)
	}
	onEdge := set.Questions[1].Facts[0].Stated[0]
	if onEdge.Document != "" || onEdge.Edge.Triple() != (graph.Edge{From: cache, Kind: graph.LinksTo, To: slow}) {
		t.Errorf("a fact stated on an edge was loaded as %+v", onEdge)
	}
	if !slices.Equal(first.Near, []graph.ID{dashboard}) {
		t.Errorf("the near misses are %v", first.Near)
	}
}

func TestWhatTheFactsSayOfDocumentsAndEvidence(t *testing.T) {
	set := example(t)
	answered := question(t, set, "slow-checkout")
	byNote := question(t, set, "which-change")
	unanswered := question(t, set, "rota-approver")

	if !answered.Answerable() || !byNote.Answerable() || unanswered.Answerable() {
		t.Errorf("answerable: %v, %v, %v; want true, true, false",
			answered.Answerable(), byNote.Answerable(), unanswered.Answerable())
	}

	// A document states a fact whether the fact is needed or extra. A near
	// miss states none, and neither does an edge's note make its ends do so.
	for id, want := range map[graph.ID]bool{cache: true, slow: true, dashboard: false, rota: false} {
		if got := answered.States(id); got != want {
			t.Errorf("States(%s) is %v, want %v", id, got, want)
		}
	}
	if byNote.States(cache) || byNote.States(slow) {
		t.Error("the ends of an edge whose note states a fact are said to state it themselves")
	}

	// Evidence is enough when every needed fact is stated by one of the
	// documents. The extra fact is not needed, and does not make up for one
	// that is.
	enough := map[string]struct {
		evidence []graph.ID
		want     bool
		covered  int
	}{
		"the document that states both":      {[]graph.ID{cache}, true, 2},
		"with a near miss beside it":         {[]graph.ID{dashboard, cache}, true, 2},
		"only the document of the extra one": {[]graph.ID{slow}, false, 0},
		"a near miss alone":                  {[]graph.ID{dashboard}, false, 0},
		"nothing":                            {nil, false, 0},
	}
	for name, c := range enough {
		if got := answered.Enough(c.evidence); got != c.want {
			t.Errorf("%s: Enough is %v, want %v", name, got, c.want)
		}
		if covered, needed := answered.Covered(c.evidence); covered != c.covered || needed != 2 {
			t.Errorf("%s: Covered is %d of %d, want %d of 2", name, covered, needed, c.covered)
		}
	}

	// A fact stated only on an edge is one no set of documents covers: the
	// question needs the note. And nothing is enough for a question that the
	// graph cannot answer.
	if !byNote.NeedsNote() || answered.NeedsNote() || unanswered.NeedsNote() {
		t.Errorf("NeedsNote: %v, %v, %v; want true, false, false", byNote.NeedsNote(), answered.NeedsNote(), unanswered.NeedsNote())
	}
	if byNote.Enough([]graph.ID{cache, slow}) {
		t.Error("documents were found enough for a fact that only an edge's note states")
	}
	// An extra fact that only a link states takes nothing from the answer:
	// the documents still state all that is needed.
	withExtraOnLink := answered
	withExtraOnLink.Facts = append(slices.Clone(answered.Facts),
		Fact{Text: "The change caused it.", Extra: true, Stated: []Stating{{Edge: set.Graph.Edges[0], Quote: "caused the slowdown"}}})
	if withExtraOnLink.NeedsNote() || !withExtraOnLink.Enough([]graph.ID{cache}) {
		t.Error("an extra fact stated on a link made a question need the link's note")
	}
	if unanswered.Enough([]graph.ID{rota}) || unanswered.Enough(nil) {
		t.Error("evidence was found enough for a question the graph cannot answer")
	}
}

func TestOnlyKeepsTheNamedQuestions(t *testing.T) {
	set := example(t)

	some, err := set.Only("rota-approver", "slow-checkout")
	if err != nil {
		t.Fatalf("Only: %v", err)
	}
	// In the set's order, not the order asked for; the graph is the same.
	if len(some.Questions) != 2 || some.Questions[0].ID != "slow-checkout" || some.Questions[1].ID != "rota-approver" {
		t.Errorf("Only kept %+v", some.Questions)
	}
	if len(some.Graph.Documents) != 4 {
		t.Errorf("Only left %d documents in the graph", len(some.Graph.Documents))
	}

	if _, err := set.Only("slow-checkout", "no-such-question"); err == nil || !strings.Contains(err.Error(), "no-such-question") {
		t.Errorf("Only with an unknown ID returned %v, want an error naming it", err)
	}
	if _, err := set.Only(); err == nil {
		t.Error("Only with no IDs returned a set with no questions")
	}

	tune := set.OfPart(Tune)
	if len(tune.Questions) != 2 || tune.Questions[1].ID != "which-change" {
		t.Errorf("the part to tune on is %+v", tune.Questions)
	}
}

// at returns element i of a list in a decoded JSON object.
func at(parent object, key string, i int) object {
	return parent[key].([]any)[i].(object)
}

func TestLoadRefusesASetThatIsNotSound(t *testing.T) {
	cases := map[string]struct {
		spoil func(g, q object)
		want  string // what the error has to mention
	}{
		// The graph.
		"two documents with one ID": {func(g, q object) { at(g, "documents", 1)["id"] = slow }, string(slow)},
		"a document with a topic's ID": {func(g, q object) {
			at(g, "documents", 3)["id"] = "topic:rota"
		}, "topic:rota"},
		"an edge from a document that is not in the file": {func(g, q object) {
			at(g, "edges", 0)["from"] = "doc:s:gone"
		}, "doc:s:gone"},
		"an edge with no kind": {func(g, q object) { at(g, "edges", 0)["kind"] = "" }, "edge"},
		"the same edge twice": {func(g, q object) {
			g["edges"] = append(g["edges"].([]any), object{"from": cache, "kind": "links_to", "to": slow, "summary": "again"})
		}, "twice"},
		"a field the graph file does not have": {func(g, q object) { at(g, "documents", 0)["sumary"] = "x" }, "sumary"},

		// The questions as a whole.
		"no questions":               {func(g, q object) { q["questions"] = []any{} }, "no questions"},
		"two questions with one ID":  {func(g, q object) { at(q, "questions", 1)["id"] = "slow-checkout" }, "slow-checkout"},
		"an ID that is not a slug":   {func(g, q object) { at(q, "questions", 0)["id"] = "Slow Checkout" }, "Slow Checkout"},
		"a question with no text":    {func(g, q object) { at(q, "questions", 0)["question"] = "  " }, "slow-checkout"},
		"a kind there is not":        {func(g, q object) { at(q, "questions", 0)["kind"] = "easy" }, "easy"},
		"a part there is not":        {func(g, q object) { at(q, "questions", 0)["part"] = "train" }, "train"},
		"a field a question has not": {func(g, q object) { at(q, "questions", 0)["fact"] = []any{} }, "fact"},
		"a question in neither part": {func(g, q object) { delete(at(q, "questions", 0), "part") }, "slow-checkout"},
		"two questions that ask the same thing": {func(g, q object) {
			at(q, "questions", 2)["question"] = at(q, "questions", 0)["question"]
		}, "asks the same thing"},

		// Facts.
		"an answerable question with no facts": {func(g, q object) { delete(at(q, "questions", 0), "facts") }, "slow-checkout"},
		"an answerable question with only extra facts": {func(g, q object) {
			at(q, "questions", 1)["facts"].([]any)[0].(object)["extra"] = true
		}, "which-change"},
		"an unanswerable question with a fact": {func(g, q object) {
			at(q, "questions", 2)["facts"] = at(q, "questions", 0)["facts"]
		}, "rota-approver"},
		"an unanswerable question that does not say why": {func(g, q object) { delete(at(q, "questions", 2), "note") }, "rota-approver"},
		"a fact with no text": {func(g, q object) {
			at(at(q, "questions", 0), "facts", 0)["fact"] = ""
		}, "slow-checkout"},
		"a fact stated nowhere": {func(g, q object) {
			at(at(q, "questions", 0), "facts", 0)["stated"] = []any{}
		}, "stated"},
		"a stating that names a document and an edge": {func(g, q object) {
			at(at(at(q, "questions", 0), "facts", 0), "stated", 0)["on"] = object{"from": cache, "kind": "links_to", "to": slow}
		}, "names both or neither"},
		"a stating that names neither": {func(g, q object) {
			delete(at(at(at(q, "questions", 0), "facts", 0), "stated", 0), "in")
		}, "names both or neither"},
		"a fact stated in a document that is not in the graph": {func(g, q object) {
			at(at(at(q, "questions", 0), "facts", 0), "stated", 0)["in"] = "doc:s:gone"
		}, "doc:s:gone"},
		"a fact stated in a topic": {func(g, q object) {
			at(at(at(q, "questions", 0), "facts", 0), "stated", 0)["in"] = "topic:performance"
		}, "topic:performance"},
		"a fact stated on an edge that is not in the graph": {func(g, q object) {
			at(at(at(q, "questions", 1), "facts", 0), "stated", 0)["on"] = object{"from": slow, "kind": "links_to", "to": cache}
		}, "which-change"},
		"a stating with no quote": {func(g, q object) {
			at(at(at(q, "questions", 0), "facts", 0), "stated", 0)["quote"] = ""
		}, "quote"},
		"a quote the document does not hold": {func(g, q object) {
			at(at(at(q, "questions", 0), "facts", 0), "stated", 0)["quote"] = "The price cache was removed on 4 March"
		}, "4 March"},
		"a quote the edge's note does not hold": {func(g, q object) {
			at(at(at(q, "questions", 1), "facts", 0), "stated", 0)["quote"] = "caused the outage"
		}, "caused the outage"},

		// Near misses.
		"a near miss that is not in the graph": {func(g, q object) {
			at(q, "questions", 0)["near"] = []any{"doc:s:gone"}
		}, "doc:s:gone"},
		"a near miss named twice": {func(g, q object) {
			at(q, "questions", 0)["near"] = []any{dashboard, dashboard}
		}, string(dashboard)},
		"a near miss that states a fact": {func(g, q object) {
			at(q, "questions", 0)["near"] = []any{slow}
		}, string(slow)},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g, q := exampleGraph(), exampleQuestions()
			c.spoil(g, q)
			_, err := Load(write(t, g, q))
			if err == nil {
				t.Fatalf("a set with %s was loaded", name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error does not mention %q:\n%v", c.want, err)
			}
		})
	}
}

func TestAQuoteMayComeFromTheTitle(t *testing.T) {
	g, q := exampleGraph(), exampleQuestions()
	at(at(at(q, "questions", 0), "facts", 0), "stated", 0)["quote"] = "Remove the price cache"
	if _, err := Load(write(t, g, q)); err != nil {
		t.Errorf("a quote from a document's title was refused: %v", err)
	}
}

// A question may be asked with no context, as an agent may ask one.
func TestAQuestionMayGoWithoutAContext(t *testing.T) {
	g, q := exampleGraph(), exampleQuestions()
	delete(q, "context")
	set, err := Load(write(t, g, q))
	if err != nil {
		t.Fatalf("a set whose file gives no context was refused: %v", err)
	}
	if got := set.Questions[0].Context; got != "" {
		t.Errorf("a question with no context was given %q", got)
	}
	if got := set.Questions[1].Context; got != "A review of one incident." {
		t.Errorf("a question's own context came out as %q", got)
	}
}

// The test set in /eval is the one this package exists for. It has to load,
// which means every label in it names something the frozen graph holds and
// every quote is word for word where it is said to be.
func TestTheTestSetInTheRepositoryIsSound(t *testing.T) {
	set, err := Load(filepath.Join("..", "..", "eval"))
	if err != nil {
		t.Fatalf("the test set in /eval does not load:\n%v", err)
	}
	if len(set.Graph.Documents) == 0 || len(set.Graph.Edges) == 0 {
		t.Errorf("the frozen graph holds %d documents and %d edges", len(set.Graph.Documents), len(set.Graph.Edges))
	}

	// Both parts are there, and so is every kind of question: a report
	// breaks its numbers down by kind, and a kind with no questions would
	// be a row that says nothing.
	kinds := make(map[Kind]int)
	parts := make(map[Part]int)
	notes := 0
	for _, q := range set.Questions {
		kinds[q.Kind]++
		parts[q.Part]++
		if q.NeedsNote() {
			notes++
		}
	}
	for _, kind := range []Kind{OneDocument, Linked, Several, BothRepositories, Absent, Nowhere, Lost} {
		if kinds[kind] == 0 {
			t.Errorf("the test set has no question of the kind %q", kind)
		}
	}
	if parts[Tune] == 0 || parts[Check] == 0 {
		t.Errorf("the parts hold %d questions to tune on and %d to check on", parts[Tune], parts[Check])
	}
	t.Logf("%d documents, %d topics, %d edges; %d questions (%d to tune on, %d held back); by kind %v; %d need a link's note",
		len(set.Graph.Documents), len(set.Graph.Topics), len(set.Graph.Edges),
		len(set.Questions), parts[Tune], parts[Check], kinds, notes)
}

func TestLoadSaysWhichFileIsMissingOrBroken(t *testing.T) {
	empty := t.TempDir()
	if _, err := Load(empty); err == nil || !strings.Contains(err.Error(), "graph.json") {
		t.Errorf("Load of an empty folder returned %v, want an error naming graph.json", err)
	}

	dir := write(t, exampleGraph(), exampleQuestions())
	if err := os.WriteFile(filepath.Join(dir, "questions.json"), []byte(`{"questions": [`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "questions.json") {
		t.Errorf("Load with a broken questions file returned %v, want an error naming questions.json", err)
	}
}

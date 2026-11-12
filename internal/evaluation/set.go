package evaluation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/umran/grafsol/graph"
)

// The two files of a test set.
const (
	graphFile     = "graph.json"
	questionsFile = "questions.json"
)

// Load reads the test set in a folder, which holds graph.json (written by
// eval/export.sql) and questions.json (written by hand), and checks it.
//
// The checks are what make a hand-written file safe to measure against. A
// field that is misspelt, a label that names a document the graph does not
// have, a quote that is not word for word in the place it is said to be: each
// is an error that names the question, never something passed over. A set
// that loads is one whose every label can be traced to the words of the
// graph.
func Load(dir string) (Set, error) {
	frozen, err := loadGraph(filepath.Join(dir, graphFile))
	if err != nil {
		return Set{}, err
	}
	questions, err := loadQuestions(filepath.Join(dir, questionsFile), frozen)
	if err != nil {
		return Set{}, err
	}
	return Set{Graph: frozen, Questions: questions}, nil
}

// readJSON decodes a file, refusing any field the receiving type does not
// have: in a file written by hand, an unknown field is a misspelt one.
func readJSON(path string, into any) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("evaluation: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("evaluation: %s: %w", filepath.Base(path), err)
	}
	return nil
}

// --- graph.json -------------------------------------------------------------

// The shape of graph.json.
type graphRecord struct {
	Documents []documentRecord `json:"documents"`
	Topics    []topicRecord    `json:"topics"`
	Edges     []edgeRecord     `json:"edges"`
}

type documentRecord struct {
	ID        graph.ID           `json:"id"`
	Type      graph.DocumentType `json:"type"`
	Source    string             `json:"source"`
	URL       string             `json:"url"`
	Title     string             `json:"title"`
	Summary   string             `json:"summary"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
}

type topicRecord struct {
	ID      graph.ID `json:"id"`
	Name    string   `json:"name"`
	Summary string   `json:"summary"`
}

type edgeRecord struct {
	From    graph.ID       `json:"from"`
	Kind    graph.EdgeKind `json:"kind"`
	To      graph.ID       `json:"to"`
	Summary string         `json:"summary"`
}

func loadGraph(path string) (Graph, error) {
	var record graphRecord
	if err := readJSON(path, &record); err != nil {
		return Graph{}, err
	}
	fail := func(format string, args ...any) (Graph, error) {
		return Graph{}, fmt.Errorf("evaluation: %s: %s", graphFile, fmt.Sprintf(format, args...))
	}

	var frozen Graph
	seen := make(map[graph.ID]bool)
	add := func(vertex graph.Vertex) error {
		if err := graph.ValidateVertex(vertex); err != nil {
			return err
		}
		if seen[vertex.VertexID()] {
			return fmt.Errorf("%s is in the file twice", vertex.VertexID())
		}
		seen[vertex.VertexID()] = true
		return nil
	}
	for _, r := range record.Documents {
		document := &graph.Document{ID: r.ID, Type: r.Type, Source: r.Source, URL: r.URL,
			Title: r.Title, Summary: r.Summary, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
		if err := add(document); err != nil {
			return fail("%v", err)
		}
		if document.Source != document.ID.Source() {
			return fail("%s says its source is %q", document.ID, document.Source)
		}
		frozen.Documents = append(frozen.Documents, document)
	}
	for _, r := range record.Topics {
		topic := &graph.Topic{ID: r.ID, Name: r.Name, Summary: r.Summary}
		if err := add(topic); err != nil {
			return fail("%v", err)
		}
		frozen.Topics = append(frozen.Topics, topic)
	}

	edges := make(map[graph.Edge]bool)
	for _, r := range record.Edges {
		edge := graph.Edge{From: r.From, Kind: r.Kind, To: r.To, Summary: r.Summary}
		if err := edge.Validate(); err != nil {
			return fail("the edge from %q to %q: %v", r.From, r.To, err)
		}
		// An edge may end at a vertex the graph does not hold, as it may in
		// any store. It cannot start at one: every edge starts at a document
		// and is written with it.
		if frozen.document(edge.From) == nil {
			return fail("an edge starts at %s, which is not a document in the file", edge.From)
		}
		if edges[edge.Triple()] {
			return fail("the edge %s %s %s is in the file twice", edge.From, edge.Kind, edge.To)
		}
		edges[edge.Triple()] = true
		frozen.Edges = append(frozen.Edges, edge)
	}
	return frozen, nil
}

// --- questions.json ---------------------------------------------------------

// The shape of questions.json.
type questionsRecord struct {
	// Context is the context of every question that gives none of its own.
	Context   string           `json:"context"`
	Questions []questionRecord `json:"questions"`
}

type questionRecord struct {
	ID       string       `json:"id"`
	Question string       `json:"question"`
	Context  string       `json:"context"`
	Kind     Kind         `json:"kind"`
	Part     Part         `json:"part"`
	Facts    []factRecord `json:"facts"`
	Near     []graph.ID   `json:"near"`
	Note     string       `json:"note"`
}

type factRecord struct {
	Fact   string          `json:"fact"`
	Extra  bool            `json:"extra"`
	Stated []statingRecord `json:"stated"`
}

// statingRecord says where a fact is stated: "in" a document, or "on" an
// edge, with the words quoted.
type statingRecord struct {
	In    graph.ID      `json:"in"`
	On    *tripleRecord `json:"on"`
	Quote string        `json:"quote"`
}

type tripleRecord struct {
	From graph.ID       `json:"from"`
	Kind graph.EdgeKind `json:"kind"`
	To   graph.ID       `json:"to"`
}

// A question's ID is a slug, so that it can be typed on a command line and
// read in a table: lower-case words and numbers joined by hyphens.
var slug = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

func loadQuestions(path string, frozen Graph) ([]Question, error) {
	var record questionsRecord
	if err := readJSON(path, &record); err != nil {
		return nil, err
	}
	if len(record.Questions) == 0 {
		return nil, fmt.Errorf("evaluation: %s holds no questions", questionsFile)
	}

	questions := make([]Question, 0, len(record.Questions))
	seen := make(map[string]bool)
	wordings := make(map[[2]string]string) // by text and context: the ID of the question that asks it
	for i, r := range record.Questions {
		if !slug.MatchString(r.ID) {
			return nil, fmt.Errorf("evaluation: %s: question %d has the ID %q, which is not lower-case words and numbers joined by hyphens",
				questionsFile, i+1, r.ID)
		}
		if seen[r.ID] {
			return nil, fmt.Errorf("evaluation: %s: two questions have the ID %q", questionsFile, r.ID)
		}
		seen[r.ID] = true

		q, err := r.question(record.Context, frozen)
		if err != nil {
			return nil, fmt.Errorf("evaluation: %s: question %q: %w", questionsFile, r.ID, err)
		}
		// A judge is told a question's text and context and nothing else,
		// so two questions with the same are one question to it, and their
		// results could not be told apart.
		wording := [2]string{q.Text, q.Context}
		if other, asked := wordings[wording]; asked {
			return nil, fmt.Errorf("evaluation: %s: question %q asks the same thing, in the same context, as %q", questionsFile, r.ID, other)
		}
		wordings[wording] = r.ID
		questions = append(questions, q)
	}
	return questions, nil
}

// question turns a record into a Question, checking it against the graph.
func (r questionRecord) question(context string, frozen Graph) (Question, error) {
	q := Question{
		ID:      r.ID,
		Text:    strings.TrimSpace(r.Question),
		Context: strings.TrimSpace(cmpOr(r.Context, context)),
		Kind:    r.Kind,
		Part:    r.Part,
		Near:    r.Near,
		Note:    strings.TrimSpace(r.Note),
	}
	if q.Text == "" {
		return Question{}, errors.New("it has no text")
	}
	answerable, known := q.Kind.answerable()
	if !known {
		return Question{}, fmt.Errorf("its kind is %q, and there is no such kind", r.Kind)
	}
	if q.Part != Tune && q.Part != Check {
		return Question{}, fmt.Errorf("its part is %q, and a question is either %q or %q", r.Part, Tune, Check)
	}

	for i, f := range r.Facts {
		fact, err := f.fact(frozen)
		if err != nil {
			return Question{}, fmt.Errorf("fact %d: %w", i+1, err)
		}
		q.Facts = append(q.Facts, fact)
	}
	switch {
	case answerable && !q.Answerable():
		return Question{}, fmt.Errorf("it is of the kind %q, which the graph can answer, and it lists no fact that the answer needs", q.Kind)
	case !answerable && len(q.Facts) > 0:
		return Question{}, fmt.Errorf("it is of the kind %q, which the graph cannot answer, and it lists facts", q.Kind)
	case !answerable && q.Note == "":
		return Question{}, errors.New("it lists no facts, and has no note to say why the graph cannot answer it")
	}

	for i, id := range q.Near {
		switch {
		case frozen.document(id) == nil:
			return Question{}, fmt.Errorf("its near miss %s is not a document in the graph", id)
		case slices.Contains(q.Near[:i], id):
			return Question{}, fmt.Errorf("its near miss %s is named twice", id)
		case q.States(id):
			return Question{}, fmt.Errorf("%s is named as a near miss, and also as stating one of the facts", id)
		}
	}
	return q, nil
}

// fact turns a record into a Fact, checking that every quote is where the
// record says it is.
func (r factRecord) fact(frozen Graph) (Fact, error) {
	fact := Fact{Text: strings.TrimSpace(r.Fact), Extra: r.Extra}
	if fact.Text == "" {
		return Fact{}, errors.New("it does not say what the fact is")
	}
	if len(r.Stated) == 0 {
		return Fact{}, errors.New("it is stated nowhere: \"stated\" names no document and no edge")
	}

	for _, s := range r.Stated {
		if (s.In != "") == (s.On != nil) {
			return Fact{}, errors.New(`each place it is stated names either a document ("in") or an edge ("on"), and one of them names both or neither`)
		}
		if s.Quote == "" {
			return Fact{}, errors.New("a place it is stated gives no quote")
		}

		stating := Stating{Quote: s.Quote}
		var shown string // what the judge is shown of that place
		if s.On == nil {
			document := frozen.document(s.In)
			if document == nil {
				return Fact{}, fmt.Errorf("it is said to be stated in %s, which is not a document in the graph", s.In)
			}
			stating.Document, shown = document.ID, document.Title+"\n"+document.Summary
		} else {
			triple := graph.Edge{From: s.On.From, Kind: s.On.Kind, To: s.On.To}
			at := slices.IndexFunc(frozen.Edges, func(edge graph.Edge) bool { return edge.Triple() == triple })
			if at < 0 {
				return Fact{}, fmt.Errorf("it is said to be stated on the edge %s %s %s, which is not in the graph", triple.From, triple.Kind, triple.To)
			}
			stating.Edge, shown = frozen.Edges[at], frozen.Edges[at].Summary
		}
		if !strings.Contains(shown, s.Quote) {
			return Fact{}, fmt.Errorf("the quote %q is not, word for word, where it is said to be", s.Quote)
		}
		fact.Stated = append(fact.Stated, stating)
	}
	return fact, nil
}

// cmpOr returns the first of two strings that is not blank.
func cmpOr(first, second string) string {
	if strings.TrimSpace(first) != "" {
		return first
	}
	return second
}

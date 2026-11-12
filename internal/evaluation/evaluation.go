// Package evaluation measures how well a search does on questions whose
// answers are known, so that the judge's wording and the thresholds in
// retrieval.Config can be tuned against something firmer than a hunch.
//
// It works on a test set, which is two files in a folder (see Load):
//
//   - graph.json is a frozen copy of a graph: its documents, topics and
//     edges as they were on the day the file was written.
//   - questions.json is questions about that graph. Each lists the facts that
//     answer it and, for each fact, where the graph states it, with the words
//     quoted. A question the graph cannot answer lists none.
//
// Three things are measured, each through the same retrieval.Judge and
// retrieval.Engine that serve real questions:
//
//   - Ratings: what the judge makes of every document, for every question,
//     in every way a search could come across it.
//   - Sufficiency: whether the judge tells evidence that states the answer
//     from evidence that does not.
//   - Searches: what a whole search returns, and whether it stops when it
//     should.
//
// Asking the judge costs money, and its answers differ a little from one
// asking to the next. So every answer is kept on a Tape, filed under exactly
// what was asked, and each question is asked several times over; one round of
// asking is a take. Anything asked again in the same words is answered from
// the tape. Only new words cost anything: a new wording, a new question, a
// new model. Trying another threshold, or a change to the search itself,
// replays the tape. Vectors does the same for embeddings, so that a search
// over the test set starts from the same documents every time.
//
// The package measures and reports. cmd/grafsol-eval is the program that
// puts it to work.
package evaluation

import (
	"fmt"
	"slices"

	"github.com/umran/grafsol/graph"
)

// Set is a test set: a frozen graph, and questions whose answers are known.
type Set struct {
	Graph     Graph
	Questions []Question
}

// Graph is a frozen copy of a graph, without embeddings.
type Graph struct {
	Documents []*graph.Document
	Topics    []*graph.Topic
	Edges     []graph.Edge
}

// Question is one question and what is known of its answer.
type Question struct {
	// ID names the question in reports and on the command line: a short
	// slug such as "puddle-atomic-operations".
	ID string

	// Text is the question as an agent would ask it, and Context the wider
	// prompt it comes from. Both go to the judge as they are.
	Text    string
	Context string

	Kind Kind
	Part Part

	// Facts are what an answer has to say, and where the graph says it. A
	// question the graph cannot answer has none.
	Facts []Fact

	// Near are documents on the question's subject that state none of its
	// facts: the near misses a judge has to tell from the real thing.
	Near []graph.ID

	// Note is a remark for the people who read the test set. A question
	// with no facts says here why the graph cannot answer it.
	Note string
}

// Fact is one thing an answer has to say, and where the graph says it.
type Fact struct {
	Text string

	// Extra marks a fact that is welcome in the evidence and not needed for
	// the answer: a search is not wrong to return the document that states
	// it, and not wrong to stop without it.
	Extra bool

	// Stated is every place the graph states the fact. Any one of them is
	// enough to cover it.
	Stated []Stating
}

// Stating is one place a fact is stated: a document's title or summary, or
// the summary of an edge. Exactly one of Document and Edge is set.
type Stating struct {
	Document graph.ID
	Edge     graph.Edge

	// Quote is the words that state the fact, exactly as the graph has
	// them. Load refuses a quote it does not find there, which keeps the
	// labels from drifting away from what the judge is actually shown.
	Quote string
}

// Kind says what sort of question a question is: where its answer lies in
// the graph, or why it has none. Reports break their numbers down by kind.
type Kind string

const (
	// OneDocument: one document states the whole answer.
	OneDocument Kind = "one"

	// Linked: the documents that state the answer are joined by a link, and
	// the question is put in terms of only one of them.
	Linked Kind = "link"

	// Several: it takes more than one document, and no link joins them.
	Several Kind = "several"

	// BothRepositories: the subject comes up in more than one project, and
	// the question is about one of them.
	BothRepositories Kind = "both"

	// Absent: the subject is in the graph and the answer is not.
	Absent Kind = "absent"

	// Nowhere: nothing in the graph is even near the subject.
	Nowhere Kind = "nowhere"

	// Lost: the original document states the answer, and the summary the
	// graph keeps of it does not. The graph cannot answer, and the fault is
	// the summary's.
	Lost Kind = "lost"
)

// answerable reports whether a question of this kind has an answer in the
// graph. ok is false for a kind there is not.
func (k Kind) answerable() (answerable, ok bool) {
	switch k {
	case OneDocument, Linked, Several, BothRepositories:
		return true, true
	case Absent, Nowhere, Lost:
		return false, true
	}
	return false, false
}

// Part says which of the two parts of a test set a question belongs to.
type Part string

const (
	// Tune is the part that wording and thresholds are tuned against.
	Tune Part = "tune"

	// Check is the part held back. It is looked at only to confirm a
	// change that was made on the strength of the other part, so that what
	// is tuned does not come to fit the questions instead of the task.
	Check Part = "check"
)

// Answerable reports whether the graph states an answer to the question.
func (q Question) Answerable() bool {
	return slices.ContainsFunc(q.Facts, func(fact Fact) bool { return !fact.Extra })
}

// States reports whether the document states one of the question's facts,
// needed or extra. A document that does is a right thing for a search to
// return as evidence.
func (q Question) States(document graph.ID) bool {
	return slices.ContainsFunc(q.Facts, func(fact Fact) bool { return fact.statedIn(document) })
}

// Enough reports whether the documents, together, state every fact the
// answer needs. It is what the judge's reading of a set of evidence should
// agree with. Nothing is enough for a question the graph cannot answer.
func (q Question) Enough(evidence []graph.ID) bool {
	covered, needed := q.Covered(evidence)
	return needed > 0 && covered == needed
}

// Covered counts the facts the answer needs, and how many of them the
// documents state.
func (q Question) Covered(evidence []graph.ID) (covered, needed int) {
	for _, fact := range q.Facts {
		if fact.Extra {
			continue
		}
		needed++
		if slices.ContainsFunc(evidence, fact.statedIn) {
			covered++
		}
	}
	return covered, needed
}

// NeedsNote reports whether a fact the answer needs is stated nowhere but in
// the summary of an edge. No set of documents is enough for such a question:
// what joins them is said only on the link between them.
func (q Question) NeedsNote() bool {
	return slices.ContainsFunc(q.Facts, func(fact Fact) bool {
		return !fact.Extra && !slices.ContainsFunc(fact.Stated, func(s Stating) bool { return s.Document != "" })
	})
}

// statedIn reports whether the document is one of the places the fact is
// stated.
func (f Fact) statedIn(document graph.ID) bool {
	return slices.ContainsFunc(f.Stated, func(s Stating) bool { return s.Document == document })
}

// Only returns the set with just the named questions, in the set's own
// order. It is an error to name a question the set does not have, or none.
func (s Set) Only(ids ...string) (Set, error) {
	if len(ids) == 0 {
		return Set{}, fmt.Errorf("evaluation: no questions were named")
	}
	for _, id := range ids {
		if !slices.ContainsFunc(s.Questions, func(q Question) bool { return q.ID == id }) {
			return Set{}, fmt.Errorf("evaluation: the test set has no question %q", id)
		}
	}
	return s.keeping(func(q Question) bool { return slices.Contains(ids, q.ID) }), nil
}

// OfPart returns the set with just the questions of one part.
func (s Set) OfPart(part Part) Set {
	return s.keeping(func(q Question) bool { return q.Part == part })
}

func (s Set) keeping(keep func(Question) bool) Set {
	kept := Set{Graph: s.Graph}
	for _, q := range s.Questions {
		if keep(q) {
			kept.Questions = append(kept.Questions, q)
		}
	}
	return kept
}

// document returns the document with that ID, or nil if the graph has none.
func (g Graph) document(id graph.ID) *graph.Document {
	for _, document := range g.Documents {
		if document.ID == id {
			return document
		}
	}
	return nil
}

// vertices returns every vertex of the graph by its ID.
func (g Graph) vertices() map[graph.ID]graph.Vertex {
	byID := make(map[graph.ID]graph.Vertex, len(g.Documents)+len(g.Topics))
	for _, document := range g.Documents {
		byID[document.ID] = document
	}
	for _, topic := range g.Topics {
		byID[topic.ID] = topic
	}
	return byID
}

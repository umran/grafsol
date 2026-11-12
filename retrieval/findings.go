package retrieval

import "github.com/umran/grafsol/graph"

// Question is what a search sets out to answer.
type Question struct {
	// Text is the question, in natural language. Required. A search does
	// best with a question that asks one thing; a caller with a compound
	// question should split it and ask the parts side by side.
	Text string

	// Context is the wider prompt the question came from, if there is one:
	// what the caller is working on, which system or period it means. The
	// judge is shown it next to the question.
	Context string

	// Anchors are vertices to start from, in addition to the documents the
	// search finds for itself. An anchor is always visited, whatever the
	// judge makes of it: the caller said to look there.
	Anchors []graph.ID
}

// Lead is a vertex the search could visit next, and how it would get there.
type Lead struct {
	// Target is the vertex that following the lead would visit. Its
	// embedding is not carried.
	Target graph.Vertex

	// Edge is the edge that reaches Target, summary included, and From is
	// the visited vertex at its other end. Both are zero for a seed: a
	// vertex the search starts from rather than arrives at.
	Edge graph.Edge
	From graph.Vertex

	// Estimate is how close Target is to the question by their embeddings:
	// 1 for the same direction, 0 for opposite or unknown. It costs nothing
	// to compute, and decides the order in which leads are put to the judge.
	Estimate float64
}

// Seed reports whether the lead is a starting point rather than an edge to
// follow.
func (l Lead) Seed() bool {
	return l.Edge == graph.Edge{}
}

// Evidence is a document that the judge found to state part of the answer,
// and how the search got to it.
type Evidence struct {
	// Document is the document as stored, without its embedding.
	Document *graph.Document

	// Potential is the judge's rating of the document, from 0 to 1.
	Potential float64

	// Path is the edges the search followed from a seed to the document,
	// in order. It is empty for a seed. The vertices along it need not be
	// evidence themselves: a search may pass through a document or a topic
	// that is on the subject without answering the question.
	Path []graph.Edge
}

// Verdict says how a search ended.
type Verdict int

const (
	// Sufficient: the judge found that the evidence states every fact the
	// answer needs.
	Sufficient Verdict = iota + 1

	// Exhausted: the search ran out of leads worth following before the
	// evidence became sufficient. Every lead it found was judged, every
	// lead rated worth following was followed, and every vertex it visited
	// had all its neighbours read.
	Exhausted

	// Stopped: a limit ended the search first. Findings.Reason says which,
	// and Findings.Left how much was known and not reached.
	Stopped
)

// String returns the verdict in lower case: "sufficient", "exhausted" or
// "stopped".
func (v Verdict) String() string {
	switch v {
	case Sufficient:
		return "sufficient"
	case Exhausted:
		return "exhausted"
	case Stopped:
		return "stopped"
	}
	return "unknown"
}

// Findings is the result of a search: the evidence, how the search ended,
// and a record of everything it did.
type Findings struct {
	Question Question
	Verdict  Verdict

	// Reason says, for a search that was Stopped, what stopped it.
	Reason string

	// Sufficient is the judge's last reading of the evidence: how likely it
	// is to state every fact the answer needs, from 0 to 1.
	Sufficient float64

	// Evidence is the documents found to state part of the answer, in the
	// order the search reached them.
	Evidence []Evidence

	// Trace is the search step by step: every lead that was judged, with
	// its rating, and every vertex that was visited.
	Trace []Step

	// Left counts what the search knew of and did not get to. It is all
	// zeroes for a search that was Exhausted.
	Left Left

	// Tokens is what the judge's assessments cost in all, in the judge's
	// own units; zero if the judge does not say.
	Tokens int
}

// Step is one round of a search: at most one assessment by the judge, and
// the visits that followed from it.
type Step struct {
	// Sufficient is the judge's reading of the evidence as it stood when
	// the step began.
	Sufficient float64

	// Judged is every lead put to the judge in this step, with its rating.
	// A lead rated below Config.MinPotential was declined, and appears
	// nowhere else.
	Judged []Judgment

	// Followed is the vertices visited at the end of the step, best first.
	Followed []graph.ID
}

// Judgment is a lead and what the judge made of it.
type Judgment struct {
	Lead      Lead
	Potential float64
}

// Left counts what a search knew of and did not get to.
type Left struct {
	// Unjudged is the leads that were found and never put to the judge.
	Unjudged int

	// Unfollowed is the leads the judge rated worth following that were
	// not followed.
	Unfollowed int

	// Unread is the visited vertices that have neighbours the search never
	// read: there is more beyond them than the counts above show.
	Unread int
}

package evaluation

import (
	"context"
	"fmt"
	"slices"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
)

// Reading is what the judge made, in one take, of whether a set of evidence
// is enough to answer a question.
type Reading struct {
	// Question is the ID of the question.
	Question string

	// Case says why this set was put to the judge.
	Case Case

	// Evidence is the documents the judge was shown, in order.
	Evidence []graph.ID

	// Enough is whether, by the question's facts, the documents state the
	// whole answer. It is what the judge's reading ought to agree with.
	Enough bool

	// Sufficient is the judge's reading: how likely the evidence is to
	// state every fact the answer needs, from 0 to 1.
	Sufficient float64
}

// Case says why a set of evidence was put to the judge. The first seven are
// made from a question's labels and each tests one thing; the last is
// whatever a search happened to gather.
type Case string

const (
	// WholeAnswer: for each fact the answer needs, a document that states
	// it. This is enough.
	WholeAnswer Case = "the whole answer"

	// WithExtras: the whole answer, and the documents that state the extra
	// facts. Still enough.
	WithExtras Case = "the answer and extras"

	// WithNearMisses: the whole answer, and the near misses beside it.
	// Still enough: a document that adds nothing takes nothing away.
	WithNearMisses Case = "the answer and near misses"

	// LessOneDocument: the whole answer with one of its documents taken
	// away, leaving a needed fact unstated. Not enough.
	LessOneDocument Case = "the answer less one document"

	// ExtrasOnly: the documents that state only extra facts. Not enough.
	ExtrasOnly Case = "extras only"

	// NearMissesOnly: documents on the subject that state nothing of the
	// answer. Not enough.
	NearMissesOnly Case = "near misses only"

	// AllButTheNote: for a question one of whose facts is stated only on a
	// link, the documents that state the others and the documents the link
	// joins. By the facts this is not enough, because the documents do not
	// say what joins them; a judge shown the link's note might well find
	// that it is.
	AllButTheNote Case = "the documents without the link's note"

	// MetBySearch: a set of evidence that a search arrived at and weighed.
	MetBySearch Case = "met by a search"
)

// Sufficiency puts sets of evidence to the judge, for every question of the
// set: one take of the readings. The sets are those made from each
// question's labels (see Case), and then those in met, by question ID, which
// are the sets that searches arrive at (see Arrivals). A set in met that is
// one of the labelled ones is not read twice.
//
// Each set goes to the judge on its own, through Judge.Assess with no leads,
// which is how a search asks whether to stop.
func Sufficiency(ctx context.Context, judge retrieval.Judge, set Set, met map[string][][]graph.ID) ([]Reading, error) {
	for id := range met {
		if !slices.ContainsFunc(set.Questions, func(q Question) bool { return q.ID == id }) {
			return nil, fmt.Errorf("evaluation: there are sets of evidence for %q, which is not a question of the test set", id)
		}
	}

	var readings []Reading
	for _, q := range set.Questions {
		sets := q.labelled()
		for _, evidence := range met[q.ID] {
			if !slices.ContainsFunc(sets, func(known Reading) bool { return slices.Equal(known.Evidence, evidence) }) {
				sets = append(sets, Reading{Question: q.ID, Case: MetBySearch, Evidence: evidence, Enough: q.Enough(evidence)})
			}
		}
		readings = append(readings, sets...)
	}

	// Everything is looked up before anything is asked: a set that names a
	// document the graph does not hold should cost nothing to find out.
	questions := make(map[string]Question, len(set.Questions))
	for _, q := range set.Questions {
		questions[q.ID] = q
	}
	shown := make([][]retrieval.Evidence, len(readings))
	for i, reading := range readings {
		for _, id := range reading.Evidence {
			document := set.Graph.document(id)
			if document == nil {
				return nil, fmt.Errorf("evaluation: evidence for %q names %s, which is not a document in the graph", reading.Question, id)
			}
			shown[i] = append(shown[i], retrieval.Evidence{Document: document})
		}
	}

	err := each(ctx, len(readings), func(ctx context.Context, i int) error {
		assessment, err := judge.Assess(ctx, questions[readings[i].Question].asked(), shown[i], nil)
		if err != nil {
			return fmt.Errorf("evaluation: reading the evidence for %q: %w", readings[i].Question, err)
		}
		readings[i].Sufficient = assessment.Sufficient
		return nil
	})
	if err != nil {
		return nil, err
	}
	return readings, nil
}

// labelled returns the sets of evidence that a question's labels call for,
// each with whether it is enough, and no reading yet.
func (q Question) labelled() []Reading {
	var sets []Reading
	add := func(why Case, parts ...[]graph.ID) {
		evidence := slices.Concat(parts...)
		if len(evidence) == 0 || slices.ContainsFunc(sets, func(known Reading) bool { return slices.Equal(known.Evidence, evidence) }) {
			return
		}
		sets = append(sets, Reading{Question: q.ID, Case: why, Evidence: evidence, Enough: q.Enough(evidence)})
	}

	answer := q.answer()
	extras := q.extras(answer)
	switch {
	case q.NeedsNote():
		add(AllButTheNote, q.joined())
	case len(answer) > 0:
		add(WholeAnswer, answer)
		if len(extras) > 0 {
			add(WithExtras, answer, extras)
		}
		if len(q.Near) > 0 {
			add(WithNearMisses, answer, q.Near)
		}
		for i := range answer {
			// Taking a document away only tests anything if a needed fact
			// goes with it: another document may state the same one.
			if rest := slices.Delete(slices.Clone(answer), i, i+1); !q.Enough(rest) {
				add(LessOneDocument, rest)
			}
		}
	}
	if len(extras) > 0 {
		add(ExtrasOnly, extras)
	}
	if len(q.Near) > 0 {
		add(NearMissesOnly, q.Near)
	}
	return sets
}

// answer returns, for each fact the answer needs, the first document named
// as stating it, in the order of the facts and without repeats. A fact that
// only an edge states contributes nothing.
func (q Question) answer() []graph.ID {
	var documents []graph.ID
	for _, fact := range q.Facts {
		if fact.Extra {
			continue
		}
		for _, stating := range fact.Stated {
			if stating.Document != "" {
				documents = appendNew(documents, stating.Document)
				break
			}
		}
	}
	return documents
}

// extras returns the documents that state an extra fact and are not part of
// the answer.
func (q Question) extras(answer []graph.ID) []graph.ID {
	var documents []graph.ID
	for _, fact := range q.Facts {
		for _, stating := range fact.Stated {
			if fact.Extra && stating.Document != "" && !slices.Contains(answer, stating.Document) {
				documents = appendNew(documents, stating.Document)
			}
		}
	}
	return documents
}

// joined returns the documents a search would hold as evidence for a
// question that needs a link's note: for each needed fact, the document that
// states it, or, where only an edge does, the documents that edge joins.
func (q Question) joined() []graph.ID {
	var documents []graph.ID
	for _, fact := range q.Facts {
		if fact.Extra || len(fact.Stated) == 0 {
			continue
		}
		first := fact.Stated[0]
		for _, stating := range fact.Stated {
			if stating.Document != "" {
				first = stating
				break
			}
		}
		if first.Document != "" {
			documents = appendNew(documents, first.Document)
			continue
		}
		for _, end := range []graph.ID{first.Edge.From, first.Edge.To} {
			if end.Kind() == graph.KindDocument {
				documents = appendNew(documents, end)
			}
		}
	}
	return documents
}

// appendNew appends id unless it is there already.
func appendNew(ids []graph.ID, id graph.ID) []graph.ID {
	if slices.Contains(ids, id) {
		return ids
	}
	return append(ids, id)
}

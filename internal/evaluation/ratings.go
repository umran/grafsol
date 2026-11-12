package evaluation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
)

// Rating is what the judge made, in one take, of one way of coming across a
// vertex while searching for the answer to one question.
type Rating struct {
	// Question is the ID of the question.
	Question string

	// Target is the vertex that was rated.
	Target graph.ID

	// Edge is the edge the vertex was reached by, and From the vertex at
	// its other end. Both are zero for a vertex met as a place to start.
	// The judge is shown them, so one vertex can be rated differently by
	// each way of reaching it.
	Edge graph.Edge
	From graph.ID

	// Potential is the judge's rating, from 0 to 1.
	Potential float64
}

// Seed reports whether the rating is of a vertex met as a place to start,
// not at the end of an edge.
func (r Rating) Seed() bool {
	return r.Edge == graph.Edge{}
}

// Leads returns every lead a search over the graph could put to a judge: each
// document as a place to start, and each edge from either end. A topic is
// only ever reached by an edge.
//
// An edge that ends at a vertex the graph does not hold leads nowhere, and so
// does an edge from a vertex to itself; a search never sees either, and
// neither is here.
func Leads(g Graph) []retrieval.Lead {
	vertices := g.vertices()
	leads := make([]retrieval.Lead, 0, len(g.Documents)+2*len(g.Edges))
	for _, document := range g.Documents {
		leads = append(leads, retrieval.Lead{Target: document})
	}
	for _, edge := range g.Edges {
		from, to := vertices[edge.From], vertices[edge.To]
		if from == nil || to == nil || edge.From == edge.To {
			continue
		}
		leads = append(leads,
			retrieval.Lead{Target: to, Edge: edge, From: from},
			retrieval.Lead{Target: from, Edge: edge, From: to})
	}
	return leads
}

// Ratings asks the judge, for every question of the set, what it makes of
// every lead there is: one take of the ratings. They come back in the order
// of the questions and then of Leads.
//
// It rates the leads through Judge.Assess, shaped exactly as a search shapes
// them, so that what is measured is the judge as a search uses it. That also
// means the whole table is on the tape afterwards, for any search to replay
// from.
func Ratings(ctx context.Context, judge retrieval.Judge, set Set) ([]Rating, error) {
	leads := Leads(set.Graph)
	perQuestion := make([][]Rating, len(set.Questions))

	err := each(ctx, len(set.Questions), func(ctx context.Context, i int) error {
		q := set.Questions[i]
		assessment, err := judge.Assess(ctx, q.asked(), nil, leads)
		if err == nil && len(assessment.Potential) != len(leads) {
			err = fmt.Errorf("the judge rated %d leads when given %d", len(assessment.Potential), len(leads))
		}
		if err != nil {
			return fmt.Errorf("evaluation: rating the leads for %q: %w", q.ID, err)
		}
		ratings := make([]Rating, len(leads))
		for n, lead := range leads {
			ratings[n] = ratingOf(q.ID, lead, assessment.Potential[n])
		}
		perQuestion[i] = ratings
		return nil
	})
	if err != nil {
		return nil, err
	}
	return slices.Concat(perQuestion...), nil
}

// ratingOf records what the judge made of a lead.
func ratingOf(question string, lead retrieval.Lead, potential float64) Rating {
	rating := Rating{Question: question, Target: lead.Target.VertexID(), Edge: lead.Edge, Potential: potential}
	if lead.From != nil {
		rating.From = lead.From.VertexID()
	}
	return rating
}

// asked returns the question as it is put to a search or a judge.
func (q Question) asked() retrieval.Question {
	return retrieval.Question{Text: q.Text, Context: q.Context}
}

// atOnce is how many questions a measurement works on at the same time. Each
// may have a request to Jev in flight, so this is also how hard a recording
// leans on the API.
const atOnce = 4

// each calls do for every number from 0 up to n, a few at a time, and waits
// for all of them. If any fails, the ones not yet started are not, and the
// error returned is that of the lowest number that failed for a reason of
// its own, not merely because another had.
func each(ctx context.Context, n int, do func(ctx context.Context, i int) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	failures := make([]error, n)
	slots := make(chan struct{}, atOnce)
	var group sync.WaitGroup
	for i := range n {
		slots <- struct{}{}
		if ctx.Err() != nil {
			<-slots
			failures[i] = ctx.Err()
			continue
		}
		group.Go(func() {
			defer func() { <-slots }()
			if failures[i] = do(ctx, i); failures[i] != nil {
				cancel()
			}
		})
	}
	group.Wait()

	var first error
	for _, failure := range failures {
		if failure == nil {
			continue
		}
		if !errors.Is(failure, context.Canceled) {
			return failure
		}
		if first == nil {
			first = failure
		}
	}
	return first
}

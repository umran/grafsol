package retrieval

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// search is one question being answered.
type search struct {
	cfg      *Config
	question Question
	near     []float32 // the question's embedding; nil when there is no Embedder

	frontier *frontier            // what is known of and not yet visited
	arrivals map[graph.ID]arrival // how each visited vertex was reached
	findings *Findings

	// weighed is how many pieces of evidence the judge has seen. When the
	// evidence has grown past it, the judge is owed another look.
	weighed int

	deadline time.Time // when Config.MaxDuration runs out; zero for never
}

// arrival is how the search got to a vertex: the edge it followed and the
// vertex it followed it from. Both are zero for a seed.
type arrival struct {
	edge graph.Edge
	from graph.ID
}

// search answers one question. It is the whole of the algorithm:
//
//	seed the frontier with anchors and the documents nearest the question
//	repeat:
//	    put the best unjudged leads, and the evidence, to the judge
//	    stop if the evidence is sufficient
//	    follow the best leads the judge has rated worth following
//	    stop if there was nothing to judge and nothing to follow
//
// Every step makes at most one call to the judge, and each step's visits are
// made at the same time.
func (e *Engine) search(ctx context.Context, q Question, near []float32) (*Findings, error) {
	s := &search{
		cfg:      &e.cfg,
		question: q,
		near:     near,
		frontier: newFrontier(),
		arrivals: make(map[graph.ID]arrival),
		findings: &Findings{Question: q},
	}

	// The caller's context says whether the caller gave up; the search's
	// own time limit is laid over it.
	caller := ctx
	if s.cfg.MaxDuration > 0 {
		s.deadline = time.Now().Add(s.cfg.MaxDuration)
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, s.deadline)
		defer cancel()
	}

	if err := s.seed(ctx); err != nil {
		return nil, err
	}

	for {
		if reason := s.limitReached(); reason != "" {
			return s.end(Stopped, reason), nil
		}

		// The leads to judge in this step: the best that are known,
		// including any still unread that could outrank them.
		batch, err := s.nextBatch(ctx)
		if err != nil {
			return s.interrupted(caller, err)
		}

		// One assessment, if there is anything for the judge to look at:
		// new leads, or evidence it has not weighed.
		step := Step{Sufficient: s.findings.Sufficient}
		assessed := len(batch) > 0 || len(s.findings.Evidence) > s.weighed
		if assessed {
			sufficient, err := s.assess(ctx, batch, &step)
			if err != nil {
				return s.interrupted(caller, err)
			}
			if sufficient {
				s.findings.Trace = append(s.findings.Trace, step)
				return s.end(Sufficient, ""), nil
			}
		}

		// Follow the best of everything judged so far. With nothing to
		// follow and nothing that was left to judge, the search is over:
		// nextBatch returns nothing only once every lead has been judged
		// and every neighbourhood read.
		followed := s.frontier.follow(s.cfg.Follow)
		if len(followed) == 0 && len(batch) == 0 {
			if assessed {
				// The judge's last word on the evidence belongs in the
				// trace even though nothing came of it.
				s.findings.Trace = append(s.findings.Trace, step)
			}
			return s.end(Exhausted, ""), nil
		}
		if err := s.visit(ctx, followed, &step); err != nil {
			return s.interrupted(caller, err)
		}
		s.findings.Trace = append(s.findings.Trace, step)
	}
}

// seed puts the starting points into the frontier: the question's anchors,
// which will be visited come what may, and the documents nearest the
// question, which will be judged like any other lead.
func (s *search) seed(ctx context.Context) error {
	searchable := s.cfg.Searcher != nil && s.near != nil
	if len(s.question.Anchors) == 0 && !searchable {
		return ErrNowhereToStart
	}

	if len(s.question.Anchors) > 0 {
		found, err := s.cfg.Store.GetVertices(ctx, s.question.Anchors)
		if err != nil {
			return fmt.Errorf("retrieval: fetching the anchors: %w", err)
		}
		anchors := make([]Lead, 0, len(s.question.Anchors))
		for _, id := range s.question.Anchors {
			vertex, stored := found[id]
			if !stored {
				return fmt.Errorf("retrieval: anchor %s is not in the graph", id)
			}
			anchors = append(anchors, Lead{Target: withoutEmbedding(vertex), Estimate: 1})
		}
		s.frontier.add(anchors, true)
	}

	if searchable {
		matches, err := s.cfg.Searcher.NearestDocuments(ctx, s.near, s.cfg.Seeds)
		if err != nil {
			return fmt.Errorf("retrieval: finding the documents nearest the question: %w", err)
		}
		ids := make([]graph.ID, len(matches))
		for i, match := range matches {
			ids[i] = match.ID
		}
		found, err := s.cfg.Store.GetVertices(ctx, ids)
		if err != nil {
			return fmt.Errorf("retrieval: fetching the documents nearest the question: %w", err)
		}
		seeds := make([]Lead, 0, len(matches))
		for _, match := range matches {
			// A document removed since the index was read is simply not
			// a place to start.
			if vertex, stored := found[match.ID]; stored {
				seeds = append(seeds, Lead{Target: withoutEmbedding(vertex), Estimate: estimate(match.Distance)})
			}
		}
		s.frontier.add(seeds, false)
	}
	return nil
}

// nextBatch returns the leads to judge next: the Batch best unjudged leads
// the search knows of.
//
// "Knows of" has to reckon with neighbourhoods that are only partly read. A
// vertex's neighbours arrive nearest first, a page at a time, so the last
// one read bounds all the unread ones. Before the batch is taken, any
// neighbourhood whose unread part could still hold a better lead than the
// batch's worst is read a page further. A hub is thus read only as far as
// the search has use for, and never skipped past.
func (s *search) nextBatch(ctx context.Context) ([]candidate, error) {
	for {
		hood := s.frontier.unreadAbove(s.cfg.Batch)
		if hood == nil {
			return s.frontier.take(s.cfg.Batch), nil
		}
		leads, err := s.read(ctx, hood)
		if err != nil {
			return nil, err
		}
		s.frontier.add(leads, false)
		if hood.cursor == "" {
			s.frontier.close(hood)
		}
	}
}

// read fetches the next page of a vertex's neighbours and returns them as
// leads. It moves the neighbourhood's cursor and bound along.
func (s *search) read(ctx context.Context, hood *neighbourhood) ([]Lead, error) {
	page, err := s.cfg.Neighbors.Neighbors(ctx, storage.NeighborQuery{
		Vertex: hood.vertex.VertexID(),
		Near:   s.near,
		Limit:  s.cfg.Batch,
		Cursor: hood.cursor,
	})
	if err != nil {
		return nil, fmt.Errorf("retrieval: reading the neighbours of %s: %w", hood.vertex.VertexID(), err)
	}

	leads := make([]Lead, len(page.Neighbors))
	for i, neighbor := range page.Neighbors {
		leads[i] = Lead{
			Target:   neighbor.Vertex,
			Edge:     neighbor.Edge,
			From:     hood.vertex,
			Estimate: estimate(neighbor.Distance),
		}
	}
	hood.cursor = page.Next
	if len(leads) > 0 {
		hood.bound = leads[len(leads)-1].Estimate
	}
	return leads, nil
}

// assess puts a batch of leads, and the evidence so far, to the judge. It
// records the judge's ratings in the step, files the leads accordingly, and
// reports whether the evidence is now sufficient.
func (s *search) assess(ctx context.Context, batch []candidate, step *Step) (sufficient bool, err error) {
	leads := make([]Lead, len(batch))
	for i, c := range batch {
		leads[i] = c.lead
	}

	assessment, err := s.cfg.Judge.Assess(ctx, s.question, s.findings.Evidence, leads)
	if err == nil {
		err = checkAssessment(assessment, len(leads))
	}
	if err != nil {
		// The leads were not judged after all.
		s.frontier.unjudged = append(batch, s.frontier.unjudged...)
		return false, fmt.Errorf("retrieval: assessing the search: %w", err)
	}

	// Sufficiency is a statement about evidence. Without any there is
	// nothing for it to be true of, whatever number the judge returned.
	if len(s.findings.Evidence) == 0 {
		assessment.Sufficient = 0
	}
	s.findings.Sufficient = assessment.Sufficient
	s.findings.Tokens += assessment.Tokens
	s.weighed = len(s.findings.Evidence)

	step.Sufficient = assessment.Sufficient
	for i, lead := range leads {
		step.Judged = append(step.Judged, Judgment{Lead: lead, Potential: assessment.Potential[i]})
	}
	s.frontier.file(batch, assessment.Potential, s.cfg.MinPotential)

	return assessment.Sufficient >= s.cfg.Sufficient, nil
}

// checkAssessment reports what is wrong with an assessment of n leads.
func checkAssessment(a Assessment, n int) error {
	if len(a.Potential) != n {
		return fmt.Errorf("the judge rated %d leads when given %d", len(a.Potential), n)
	}
	inRange := func(x float64) bool { return x >= 0 && x <= 1 } // false for NaN too
	if !inRange(a.Sufficient) {
		return fmt.Errorf("the judge's sufficiency reading is %v, which is not between 0 and 1", a.Sufficient)
	}
	for i, potential := range a.Potential {
		if !inRange(potential) {
			return fmt.Errorf("the judge rated lead %d at %v, which is not between 0 and 1", i+1, potential)
		}
	}
	return nil
}

// visit follows leads: it reads the first page of each target's neighbours,
// all at the same time, then records each visit in order. A visited
// document that the judge rated highly enough becomes evidence; every
// visited vertex gives the frontier new leads.
func (s *search) visit(ctx context.Context, followed []candidate, step *Step) error {
	hoods := make([]*neighbourhood, len(followed))
	pages := make([][]Lead, len(followed))
	failures := make([]error, len(followed))
	var group sync.WaitGroup
	for i, c := range followed {
		// Nothing has been read yet, so nothing bounds what may be there.
		hoods[i] = &neighbourhood{vertex: c.lead.Target, bound: 1}
		group.Go(func() {
			pages[i], failures[i] = s.read(ctx, hoods[i])
		})
	}
	group.Wait()
	if err := errors.Join(failures...); err != nil {
		// None of the visits counts: the leads go back where they were.
		s.frontier.worth = append(followed, s.frontier.worth...)
		return err
	}

	for i, c := range followed {
		id := c.target()
		arrived := arrival{edge: c.lead.Edge}
		if c.lead.From != nil {
			arrived.from = c.lead.From.VertexID()
		}
		s.arrivals[id] = arrived
		step.Followed = append(step.Followed, id)

		if document, isDocument := c.lead.Target.(*graph.Document); isDocument && c.potential >= s.cfg.MinEvidence {
			s.findings.Evidence = append(s.findings.Evidence, Evidence{
				Document:  document,
				Potential: c.potential,
				Path:      s.pathTo(id),
			})
		}

		s.frontier.add(pages[i], false)
		if hoods[i].cursor != "" {
			s.frontier.open = append(s.frontier.open, hoods[i])
		}
	}
	return nil
}

// pathTo returns the edges the search followed from a seed to a visited
// vertex, in the order it followed them.
func (s *search) pathTo(id graph.ID) []graph.Edge {
	var path []graph.Edge
	for {
		arrived := s.arrivals[id]
		if arrived.edge == (graph.Edge{}) {
			slices.Reverse(path)
			return path
		}
		path = append(path, arrived.edge)
		id = arrived.from
	}
}

// limitReached says which of the search's limits has been reached, or
// nothing if none has.
func (s *search) limitReached() string {
	if s.cfg.MaxSteps > 0 && len(s.findings.Trace) >= s.cfg.MaxSteps {
		return fmt.Sprintf("the limit of %d steps was reached", s.cfg.MaxSteps)
	}
	if s.outOfTime() {
		return s.timeLimit()
	}
	return ""
}

func (s *search) outOfTime() bool {
	return !s.deadline.IsZero() && !time.Now().Before(s.deadline)
}

func (s *search) timeLimit() string {
	return fmt.Sprintf("the time limit of %s was reached", s.cfg.MaxDuration)
}

// end closes the search with a verdict and returns its findings.
func (s *search) end(verdict Verdict, reason string) *Findings {
	s.findings.Verdict = verdict
	s.findings.Reason = reason
	s.findings.Left = s.frontier.left()
	return s.findings
}

// interrupted closes a search that could not go on, and decides whether
// that is an error. Running into one of the search's own limits is not; the
// caller giving up, or something failing, is. Either way the findings so far
// are returned.
func (s *search) interrupted(caller context.Context, err error) (*Findings, error) {
	switch {
	case caller.Err() != nil:
		return s.end(Stopped, "the search was cancelled"), caller.Err()
	case s.outOfTime() && errors.Is(err, context.DeadlineExceeded):
		return s.end(Stopped, s.timeLimit()), nil
	case errors.Is(err, ErrTooLarge):
		return s.end(Stopped, "the evidence outgrew what the judge can read"), nil
	default:
		return s.end(Stopped, "the search failed"), err
	}
}

// estimate turns a cosine distance, which runs from 0 to 2, into a closeness
// that runs from 1 down to 0.
func estimate(distance float32) float64 {
	return max(0, min(1, 1-float64(distance)/2))
}

// withoutEmbedding returns a copy of the vertex that carries no embedding.
// A search reads titles and summaries; the vectors would only be weight.
func withoutEmbedding(vertex graph.Vertex) graph.Vertex {
	switch v := vertex.(type) {
	case *graph.Document:
		bare := *v
		bare.Embedding = nil
		return &bare
	case *graph.Topic:
		bare := *v
		bare.Embedding = nil
		return &bare
	}
	return vertex
}

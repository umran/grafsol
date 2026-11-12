package evaluation

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"math"
	"slices"
	"sync"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
)

// Fill loads the frozen graph of a test set into a store, ready to be
// searched. It goes through the sync engine, with the frozen graph standing
// in for a source: each document is embedded by the rule ingestion uses and
// stored with the edges that start at it, and the store learns which
// embedding space its vectors are in, exactly as for a graph filled from a
// real source.
//
// Filling a store a second time changes nothing.
func Fill(ctx context.Context, set Set, store storage.Store, ledger storage.Ledger, embedder model.Embedder) error {
	var sources []source.Source
	for _, document := range set.Graph.Documents {
		if !slices.ContainsFunc(sources, func(s source.Source) bool { return s.Name() == document.Source }) {
			sources = append(sources, frozenSource{name: document.Source, graph: set.Graph})
		}
	}
	syncer := ingest.NewSyncer(ingest.Config{
		Store:     store,
		Ledger:    ledger,
		Sources:   sources,
		Enrichers: []ingest.Enricher{ingest.Embed(embedder)},
		Logger:    slog.New(slog.DiscardHandler),
	})

	for _, src := range sources {
		report, err := syncer.Reconcile(ctx, src.(frozenSource))
		if err != nil {
			return fmt.Errorf("evaluation: loading the frozen graph: %w", err)
		}
		// A document stored without its embedding would never be found by
		// a search, and nothing would say so.
		if report.Declined > 0 {
			return fmt.Errorf("evaluation: loading the frozen graph: %d documents of %s could not be embedded", report.Declined, src.Name())
		}
	}
	return nil
}

// frozenSource is the documents of one source system in a frozen graph, as a
// source the sync engine can scan.
type frozenSource struct {
	name  string
	graph Graph
}

var _ source.Scanner = frozenSource{}

func (f frozenSource) Name() string { return f.name }

// Resolve recognizes no URL: a frozen graph has its edges already.
func (f frozenSource) Resolve(string) (graph.ID, bool) { return "", false }

func (f frozenSource) Scan(context.Context, source.Checkpoint) iter.Seq2[source.Change, error] {
	vertices := f.graph.vertices()
	return func(yield func(source.Change, error) bool) {
		for _, document := range f.graph.Documents {
			if document.Source != f.name {
				continue
			}
			change := source.Change{Document: *document, Version: 1}
			for _, edge := range f.graph.Edges {
				if edge.From != document.ID {
					continue
				}
				change.Edges = append(change.Edges, edge)
				if topic, isTopic := vertices[edge.To].(*graph.Topic); isTopic {
					change.Topics = append(change.Topics, *topic)
				}
			}
			if !yield(change, nil) {
				return
			}
		}
	}
}

// Settings is the three thresholds of retrieval.Config that a search runs
// under, and that the test set exists to tune.
type Settings struct {
	Sufficient   float64 // the reading at which a search stops
	MinPotential float64 // the rating a lead needs to be followed
	MinEvidence  float64 // the rating a followed document needs to be evidence
}

// DefaultSettings is the thresholds a retrieval.Engine uses when given none.
var DefaultSettings = Settings{Sufficient: 0.7, MinPotential: 0.25, MinEvidence: 0.75}

// Apply returns cfg with the three thresholds set.
func (s Settings) Apply(cfg retrieval.Config) retrieval.Config {
	cfg.Sufficient, cfg.MinPotential, cfg.MinEvidence = s.Sufficient, s.MinPotential, s.MinEvidence
	return cfg
}

// Around returns the settings that lie within one step of s in each
// threshold: 27 of them, s among them.
func Around(s Settings, step float64) []Settings {
	// Hundredths, so that 0.7 less 0.1 is 0.6 and not a hair under it.
	near := func(x float64) []float64 {
		return []float64{math.Round((x-step)*100) / 100, x, math.Round((x+step)*100) / 100}
	}
	var settings []Settings
	for _, sufficient := range near(s.Sufficient) {
		for _, potential := range near(s.MinPotential) {
			for _, evidence := range near(s.MinEvidence) {
				settings = append(settings, Settings{Sufficient: sufficient, MinPotential: potential, MinEvidence: evidence})
			}
		}
	}
	return settings
}

// Paths returns one of the settings for each different pair of MinPotential
// and MinEvidence among them. Those two decide which leads a search follows
// and which documents it gathers; Sufficient only decides where along that
// way it stops. So these are the settings worth running to find out
// everything a search could arrive at.
func Paths(settings []Settings) []Settings {
	var paths []Settings
	for _, s := range settings {
		if !slices.ContainsFunc(paths, func(p Settings) bool {
			return p.MinPotential == s.MinPotential && p.MinEvidence == s.MinEvidence
		}) {
			paths = append(paths, s)
		}
	}
	return paths
}

// Search is what one search for the answer to one question came to, in one
// take.
type Search struct {
	// Question is the ID of the question.
	Question string

	// Verdict is how the search ended: "sufficient", "exhausted" or
	// "stopped", with the Reason for the last. Sufficient is the judge's
	// last reading of the evidence.
	Verdict    string
	Reason     string
	Sufficient float64

	// Evidence is the documents the search returned, in the order it
	// reached them.
	Evidence []Found

	// Met is every lead the search put to the judge, with its rating, and
	// Followed every vertex it visited, both in the order it happened.
	Met      []Rating
	Followed []graph.ID

	// Steps is how many rounds the search took, and Asked what it asked of
	// the judge on the way.
	Steps int
	Asked Asked

	// Err is set when the search failed, and says why. The rest is then
	// whatever the search had come to when it did.
	Err string
}

// Found is a document that a search returned as evidence.
type Found struct {
	Document  graph.ID
	Potential float64

	// ByLink reports whether the search reached the document by following
	// an edge, as against having started from it.
	ByLink bool
}

// Asked counts what a search asked of the judge: how many times it asked,
// how many leads it had rated in all, and how many times it had the evidence
// weighed. Each asking is one request to Jev, or a few when there are many
// leads.
type Asked struct {
	Times    int
	Leads    int
	Readings int
}

// Searches runs the real search for every question of the set, with the
// engine that cfg describes, and returns what each came to, in the order of
// the questions: one take of the searches. cfg needs a Store that Fill has
// loaded, the Embedder it was loaded with, and a Judge.
//
// If a search fails, the others are still run. The error then names the
// first question whose search failed and wraps what went wrong, and the
// result of each failed search says so itself.
func Searches(ctx context.Context, cfg retrieval.Config, set Set) ([]Search, error) {
	counter := &countingJudge{inner: cfg.Judge, asked: make(map[wording]*Asked)}
	cfg.Judge = counter
	results := retrieval.NewEngine(cfg).GatherAll(ctx, questionsOf(set))

	searches := make([]Search, len(results))
	var failed error
	for i, result := range results {
		q := set.Questions[i]
		searches[i] = searchOf(q.ID, result.Findings)
		searches[i].Asked = counter.of(q.asked())
		if result.Err != nil {
			searches[i].Err = result.Err.Error()
			if failed == nil {
				failed = fmt.Errorf("evaluation: the search for %q: %w", q.ID, result.Err)
			}
		}
	}
	return searches, failed
}

// Arrivals finds every set of evidence that a search for each question comes
// to weigh, by running the searches with the stopping switched off: the
// judge still rates the leads, but is never asked whether the evidence is
// enough, and the search is told each time that it is not. What comes back,
// by question ID, is each state the evidence passed through, in order.
//
// A real search goes the same way for as long as it goes, so the sets it
// could stop at, whatever reading stops it, are among these. That makes
// Arrivals the list of what to put to the judge so that any such search can
// be replayed (see Sufficiency). Which way a search goes does depend on
// MinPotential and MinEvidence: run Arrivals for each of Paths.
func Arrivals(ctx context.Context, cfg retrieval.Config, set Set) (map[string][][]graph.ID, error) {
	collector := &arrivingJudge{inner: cfg.Judge, met: make(map[wording][][]graph.ID)}
	cfg.Judge = collector
	results := retrieval.NewEngine(cfg).GatherAll(ctx, questionsOf(set))

	arrivals := make(map[string][][]graph.ID)
	for i, result := range results {
		q := set.Questions[i]
		if result.Err != nil {
			return nil, fmt.Errorf("evaluation: the search for %q: %w", q.ID, result.Err)
		}
		if met := collector.of(q.asked()); len(met) > 0 {
			arrivals[q.ID] = met
		}
	}
	return arrivals, nil
}

func questionsOf(set Set) []retrieval.Question {
	questions := make([]retrieval.Question, len(set.Questions))
	for i, q := range set.Questions {
		questions[i] = q.asked()
	}
	return questions
}

// searchOf records what a search came to. findings is nil for a search that
// never began.
func searchOf(question string, findings *retrieval.Findings) Search {
	search := Search{Question: question}
	if findings == nil {
		return search
	}
	search.Verdict, search.Reason = findings.Verdict.String(), findings.Reason
	search.Sufficient = findings.Sufficient
	search.Steps = len(findings.Trace)
	for _, evidence := range findings.Evidence {
		search.Evidence = append(search.Evidence, Found{
			Document:  evidence.Document.ID,
			Potential: evidence.Potential,
			ByLink:    len(evidence.Path) > 0,
		})
	}
	for _, step := range findings.Trace {
		for _, judged := range step.Judged {
			search.Met = append(search.Met, ratingOf(question, judged.Lead, judged.Potential))
		}
		search.Followed = append(search.Followed, step.Followed...)
	}
	return search
}

// countingJudge passes every assessment on to another judge, and keeps
// count, for each question, of what was asked.
type countingJudge struct {
	inner retrieval.Judge

	mu    sync.Mutex
	asked map[wording]*Asked
}

func (j *countingJudge) Assess(ctx context.Context, q retrieval.Question, evidence []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	j.mu.Lock()
	count := j.asked[wordingOf(q)]
	if count == nil {
		count = new(Asked)
		j.asked[wordingOf(q)] = count
	}
	count.Times++
	count.Leads += len(leads)
	if len(evidence) > 0 {
		count.Readings++
	}
	j.mu.Unlock()
	return j.inner.Assess(ctx, q, evidence, leads)
}

func (j *countingJudge) of(q retrieval.Question) Asked {
	j.mu.Lock()
	defer j.mu.Unlock()
	if count := j.asked[wordingOf(q)]; count != nil {
		return *count
	}
	return Asked{}
}

// arrivingJudge has another judge rate the leads, never lets it weigh the
// evidence, and writes down each set of evidence it would have been asked
// to weigh.
type arrivingJudge struct {
	inner retrieval.Judge

	mu  sync.Mutex
	met map[wording][][]graph.ID
}

func (j *arrivingJudge) Assess(ctx context.Context, q retrieval.Question, evidence []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	if len(evidence) > 0 {
		ids := make([]graph.ID, len(evidence))
		for i, piece := range evidence {
			ids[i] = piece.Document.ID
		}
		j.mu.Lock()
		if !slices.ContainsFunc(j.met[wordingOf(q)], func(known []graph.ID) bool { return slices.Equal(known, ids) }) {
			j.met[wordingOf(q)] = append(j.met[wordingOf(q)], ids)
		}
		j.mu.Unlock()
	}
	if len(leads) == 0 {
		return retrieval.Assessment{}, nil
	}
	assessment, err := j.inner.Assess(ctx, q, nil, leads)
	assessment.Sufficient = 0
	return assessment, err
}

func (j *arrivingJudge) of(q retrieval.Question) [][]graph.ID {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.met[wordingOf(q)]
}

// wording is what tells one question from another once it has been handed to
// a search: its text and its context, which is all a judge is told of it.
// Load sees to it that no two questions of a set have the same.
type wording struct {
	text, context string
}

func wordingOf(q retrieval.Question) wording {
	return wording{text: q.Text, context: q.Context}
}

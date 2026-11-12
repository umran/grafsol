// Package retrieval gathers, from the graph, what is needed to answer a
// question.
//
// A search starts at the documents nearest the question and works outwards
// along the edges that look most promising, asking a Judge at each step what
// the leads in front of it are worth and whether the evidence gathered so
// far already answers the question. It ends when the judge says the evidence
// is enough, when nothing worth following is left, or when a limit is
// reached, and says which. What it returns is not an answer but the material
// for one: the documents that state it, the path of edges that led to each,
// and a trace of every judgment made on the way.
//
// search.go is the file to read to understand the package: it holds the
// loop. frontier.go keeps the leads a search knows of and has not followed.
package retrieval

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/storage"
)

// Config says what an Engine works with, and how far its searches go.
type Config struct {
	// Store is the graph to search. Required.
	Store storage.Store

	// Searcher finds the documents nearest a question, which is where a
	// search starts. Without it, or without an Embedder, a search starts
	// from the question's Anchors alone.
	Searcher storage.DocumentSearcher

	// Neighbors reads the neighbours of a vertex, most promising first.
	// Nil means storage.NeighborsByScan(Store), which works for any Store
	// and is slow at vertices with very many edges.
	Neighbors storage.NeighborSearcher

	// Embedder embeds questions. Without it there are no seeds, and the
	// neighbours of a vertex are read in no particular order.
	//
	// It has to work in the embedding space that the graph's documents were
	// embedded in, or a question's vector could not be compared with
	// theirs. The engine does not take that on trust: before every search
	// it compares the Embedder's Space with the one Store has recorded for
	// the graph, and refuses to search with any other.
	Embedder model.Embedder

	// Judge rates the leads and the evidence. Required.
	Judge Judge

	// Seeds is how many of the documents nearest the question a search
	// starts from. Zero means 8.
	Seeds int

	// Batch is the most leads put to the judge in one step, and the number
	// of neighbours read from a vertex at a time. Zero means 64.
	Batch int

	// Follow is the most leads followed in one step. Zero means 3.
	Follow int

	// Sufficient is the judge's sufficiency reading at which a search stops
	// with what it has. Zero means 0.7.
	Sufficient float64

	// MinPotential is the rating a lead needs to be followed; leads rated
	// lower are declined. Zero means 0.25, which on the judge's scale is
	// the halfway mark between "unrelated" and "on the subject".
	MinPotential float64

	// MinEvidence is the rating a followed document needs to count as
	// evidence. Zero means 0.75: the halfway mark between "on the subject"
	// and "states part of the answer". A document followed below it is a
	// waypoint: the search goes through it, and it appears in paths and in
	// the trace, but it is not evidence.
	MinEvidence float64

	// MaxSteps is how many steps a search may take. Zero means 12; a
	// negative number means no limit.
	MaxSteps int

	// MaxDuration is how long a search may take. Zero means no limit.
	MaxDuration time.Duration

	// Parallel is how many questions GatherAll works on at once. Zero
	// means 8.
	Parallel int
}

// Engine answers questions from one graph. It is safe for concurrent use.
type Engine struct {
	cfg Config
}

// NewEngine returns an Engine. It panics if cfg has no Store or no Judge: an
// Engine can do nothing without them, and leaving one out is a mistake in
// the program rather than a condition to handle while it runs.
func NewEngine(cfg Config) *Engine {
	if cfg.Store == nil {
		panic("retrieval: NewEngine needs a Store")
	}
	if cfg.Judge == nil {
		panic("retrieval: NewEngine needs a Judge")
	}
	if cfg.Neighbors == nil {
		cfg.Neighbors = storage.NeighborsByScan(cfg.Store)
	}
	cfg.Seeds = cmp.Or(cfg.Seeds, 8)
	cfg.Batch = cmp.Or(cfg.Batch, 64)
	cfg.Follow = cmp.Or(cfg.Follow, 3)
	cfg.Sufficient = cmp.Or(cfg.Sufficient, 0.7)
	cfg.MinPotential = cmp.Or(cfg.MinPotential, 0.25)
	cfg.MinEvidence = cmp.Or(cfg.MinEvidence, 0.75)
	cfg.MaxSteps = cmp.Or(cfg.MaxSteps, 12)
	cfg.Parallel = cmp.Or(cfg.Parallel, 8)
	return &Engine{cfg: cfg}
}

// ErrNowhereToStart is returned for a question that names no anchors when
// the Engine has no way to find a starting point for itself: no Searcher, or
// no Embedder.
var ErrNowhereToStart = errors.New("retrieval: the question names no anchors and the engine cannot search for a place to start")

// Gather searches the graph for what answers the question.
//
// The Findings say how the search ended. Running out of leads and reaching
// a limit are not errors: they are verdicts, and the Findings hold whatever
// was gathered. An error means the search could not be carried through: the
// context ended, or the store, the embedder or the judge failed, or the
// embedder does not work in the graph's embedding space (the error then
// wraps storage.ErrOtherSpace). Even then, if the search had begun, Gather
// returns what it had gathered so far along with the error.
func (e *Engine) Gather(ctx context.Context, q Question) (*Findings, error) {
	results := e.GatherAll(ctx, []Question{q})
	return results[0].Findings, results[0].Err
}

// Result is the outcome of one of the questions given to GatherAll.
type Result struct {
	Findings *Findings
	Err      error
}

// GatherAll searches for the answers to several questions at once and
// returns one Result per question, in order. The searches are independent:
// they share nothing but the graph, and one that fails does not disturb the
// others. The questions are embedded together, in a single call.
func (e *Engine) GatherAll(ctx context.Context, questions []Question) []Result {
	results := make([]Result, len(questions))

	// Check and embed the questions that can be searched for at all.
	var valid []int
	for i, q := range questions {
		if err := check(q); err != nil {
			results[i].Err = err
			continue
		}
		valid = append(valid, i)
	}
	vectors, err := e.embed(ctx, questions, valid)
	if err != nil {
		for _, i := range valid {
			results[i].Err = err
		}
		return results
	}

	slots := make(chan struct{}, e.cfg.Parallel)
	var group sync.WaitGroup
	for _, i := range valid {
		group.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i].Findings, results[i].Err = e.search(ctx, questions[i], vectors[i])
		})
	}
	group.Wait()
	return results
}

// check reports what makes a question impossible to search for.
func check(q Question) error {
	if strings.TrimSpace(q.Text) == "" {
		return errors.New("retrieval: the question has no text")
	}
	for _, anchor := range q.Anchors {
		if err := anchor.Validate(); err != nil {
			return fmt.Errorf("retrieval: anchor: %w", err)
		}
	}
	return nil
}

// sameSpace checks that the Embedder works in the embedding space the
// graph's embeddings are in. If it does not, a question's vector would be
// compared with vectors it has nothing to do with, and the search would come
// back with plausible nonsense: so there is no search.
//
// A graph that no space has been claimed for holds no embeddings yet, and
// there is nothing to compare.
func (e *Engine) sameSpace(ctx context.Context) error {
	embedders, err := e.cfg.Embedder.Space(ctx)
	if err != nil {
		return fmt.Errorf("retrieval: asking the embedder which embedding space it works in: %w", err)
	}
	graphs, err := e.cfg.Store.EmbeddingSpace(ctx)
	if err != nil {
		return fmt.Errorf("retrieval: reading the graph's embedding space: %w", err)
	}
	if graphs != "" && graphs != embedders {
		return fmt.Errorf("retrieval: %w: the graph's embeddings are in %q and the embedder's in %q, so a question cannot be compared with the documents",
			storage.ErrOtherSpace, graphs, embedders)
	}
	return nil
}

// embed returns the embedding of each of the questions at the given
// indexes, keyed by index. With no Embedder it returns nothing, and the
// searches go without.
func (e *Engine) embed(ctx context.Context, questions []Question, indexes []int) (map[int][]float32, error) {
	if e.cfg.Embedder == nil || len(indexes) == 0 {
		return nil, nil
	}
	if err := e.sameSpace(ctx); err != nil {
		return nil, err
	}
	texts := make([]string, len(indexes))
	for i, index := range indexes {
		texts[i] = questions[index].Text
	}
	embedded, err := e.cfg.Embedder.EmbedQueries(ctx, texts)
	if err != nil {
		return nil, fmt.Errorf("retrieval: embedding the questions: %w", err)
	}
	if len(embedded) != len(texts) {
		return nil, fmt.Errorf("retrieval: the embedder returned %d vectors for %d questions", len(embedded), len(texts))
	}
	vectors := make(map[int][]float32, len(indexes))
	for i, index := range indexes {
		vectors[index] = embedded[i]
	}
	return vectors, nil
}

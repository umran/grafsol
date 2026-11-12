package retrieval_test

import (
	"context"
	"fmt"
	"strings"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/storage/memory"
)

// keywordJudge stands in for a model. It rates a lead by the words in the
// document it leads to: one that gives a reason states part of the answer,
// one that mentions checkout is on the subject, and anything else is
// unrelated. Any evidence at all satisfies it.
type keywordJudge struct{}

func (keywordJudge) Assess(_ context.Context, _ retrieval.Question, evidence []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	assessment := retrieval.Assessment{Potential: make([]float64, len(leads))}
	if len(evidence) > 0 {
		assessment.Sufficient = 1
	}
	for i, lead := range leads {
		document, isDocument := lead.Target.(*graph.Document)
		switch {
		case !isDocument:
		case strings.Contains(document.Summary, "because"):
			assessment.Potential[i] = 1
		case strings.Contains(strings.ToLower(document.Title), "checkout"):
			assessment.Potential[i] = 0.5
		}
	}
	return assessment, nil
}

// A search follows the leads its judge rates highest until the judge finds
// the evidence sufficient, and returns that evidence with the path to it.
func Example() {
	ctx := context.Background()
	store := memory.New()
	store.PutVertices(ctx, []graph.Vertex{
		&graph.Document{ID: "doc:wiki:slow", Title: "Slow checkout", Summary: "Customers report that paying takes ten seconds."},
		&graph.Document{ID: "doc:wiki:notes", Title: "Notes on checkout latency", Summary: "Timings taken in March."},
		&graph.Document{ID: "doc:wiki:cause", Title: "Price cache removed", Summary: "Paying became slow because the price cache was removed."},
		&graph.Document{ID: "doc:wiki:rota", Title: "Holiday rota", Summary: "Who is away in August."},
	})
	store.PutEdges(ctx, []graph.Edge{
		{From: "doc:wiki:slow", Kind: graph.LinksTo, To: "doc:wiki:notes", Summary: "the timings are in the notes"},
		{From: "doc:wiki:slow", Kind: graph.LinksTo, To: "doc:wiki:rota", Summary: "ask whoever is on call"},
		{From: "doc:wiki:notes", Kind: graph.LinksTo, To: "doc:wiki:cause", Summary: "traced to the cache removal"},
	})

	// With no embedder, a search starts from the anchors it is given. With
	// one, it would start from the documents nearest the question.
	engine := retrieval.NewEngine(retrieval.Config{Store: store, Judge: keywordJudge{}})
	findings, err := engine.Gather(ctx, retrieval.Question{
		Text:    "Why is checkout slow?",
		Anchors: []graph.ID{"doc:wiki:slow"},
	})
	if err != nil {
		fmt.Println("gather:", err)
		return
	}

	fmt.Printf("%s after %d steps\n", findings.Verdict, len(findings.Trace))
	for _, evidence := range findings.Evidence {
		fmt.Printf("evidence: %s\n", evidence.Document.Title)
		for _, edge := range evidence.Path {
			fmt.Printf("  %s -> %s: %s\n", edge.From.Key(), edge.To.Key(), edge.Summary)
		}
	}
	for number, step := range findings.Trace {
		for _, judged := range step.Judged {
			fmt.Printf("step %d rated %s at %.1f\n", number+1, judged.Lead.Target.VertexID().Key(), judged.Potential)
		}
	}

	// Output:
	// sufficient after 4 steps
	// evidence: Price cache removed
	//   wiki:slow -> wiki:notes: the timings are in the notes
	//   wiki:notes -> wiki:cause: traced to the cache removal
	// step 1 rated wiki:slow at 0.5
	// step 2 rated wiki:notes at 0.5
	// step 2 rated wiki:rota at 0.0
	// step 3 rated wiki:cause at 1.0
}

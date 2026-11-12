package jev

import (
	"os"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/typesafe"
)

// TestLiveJudge puts the judge's wording to the real Jev: three documents
// that plainly are, in turn, a statement of the answer, on the subject
// without answering, and unrelated; and two sets of evidence, one that
// answers the question and one that does not.
//
// It is the check that the questions in questions.go mean to Jev what they
// are meant to mean, and that retrieval's default thresholds fall where
// they should. It is skipped unless GRAFSOL_LIVE_TYPESAFE is set, and needs
// TYPESAFE_API_KEY. It costs a fraction of a cent.
//
//	GRAFSOL_LIVE_TYPESAFE=1 TYPESAFE_API_KEY=... go test ./retrieval/jev/ -run TestLiveJudge -v
func TestLiveJudge(t *testing.T) {
	if os.Getenv("GRAFSOL_LIVE_TYPESAFE") == "" {
		t.Skip("GRAFSOL_LIVE_TYPESAFE is not set")
	}
	key := os.Getenv("TYPESAFE_API_KEY")
	if key == "" {
		t.Fatal("TYPESAFE_API_KEY is not set")
	}
	judge := New(Config{Client: typesafe.New(typesafe.Config{APIKey: key, UserAgent: "go-test-client"})})

	question := retrieval.Question{
		Text:    "Why did checkout latency double in March?",
		Context: "A review of the web shop's performance.",
	}
	answer := &graph.Document{ID: "doc:s:answer", Title: "Remove the price cache",
		Summary: "The price cache was removed on 3 March to fix stale prices. Checkout p95 latency doubled the same day, because every price is now read from the database."}
	subject := &graph.Document{ID: "doc:s:subject", Title: "Checkout latency dashboard",
		Summary: "Describes the dashboard that graphs checkout p95 latency week by week, and who maintains it."}
	unrelated := &graph.Document{ID: "doc:s:unrelated", Title: "Holiday rota for August",
		Summary: "Lists who on the support team is away in each week of August."}
	found := graph.Edge{From: subject.ID, Kind: graph.LinksTo, To: answer.ID, Summary: "the jump on 3 March is explained in the cache removal"}

	// The leads: rated in the right order, and on the right side of the
	// two thresholds.
	assessment, err := judge.Assess(t.Context(), question, nil, []retrieval.Lead{
		{Target: answer, From: subject, Edge: found},
		{Target: subject},
		{Target: unrelated},
	})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	t.Logf("ratings: the answer %.2f, on the subject %.2f, unrelated %.2f (for %d tokens)",
		assessment.Potential[0], assessment.Potential[1], assessment.Potential[2], assessment.Tokens)
	if got := assessment.Potential[0]; got < 0.75 {
		t.Errorf("the document that states the answer is rated %.2f, below the 0.75 that makes a document evidence", got)
	}
	if got := assessment.Potential[1]; got < 0.25 || got >= 0.75 {
		t.Errorf("the document on the subject is rated %.2f, want it followed (0.25 or more) but not counted as evidence (under 0.75)", got)
	}
	if got := assessment.Potential[2]; got >= 0.25 {
		t.Errorf("the unrelated document is rated %.2f, want it declined (under 0.25)", got)
	}

	// The evidence: sufficient when it states the answer, not when it is
	// only on the subject.
	enough, err := judge.Assess(t.Context(), question, []retrieval.Evidence{{Document: answer}}, nil)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	lacking, err := judge.Assess(t.Context(), question, []retrieval.Evidence{{Document: subject}}, nil)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	t.Logf("sufficiency: with the answer %.2f, with the dashboard only %.2f", enough.Sufficient, lacking.Sufficient)
	if enough.Sufficient < 0.7 {
		t.Errorf("evidence that states the answer is sufficient with probability %.2f, below the 0.7 that ends a search", enough.Sufficient)
	}
	if lacking.Sufficient >= 0.7 {
		t.Errorf("evidence that only describes a dashboard is sufficient with probability %.2f", lacking.Sufficient)
	}
}

package inference

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/storage"
)

// The tests in this file talk to the real services, which must be running:
//
//	cd inference && make embeddings     (and, in another terminal)
//	cd inference && make summaries
//
//	GRAFSOL_LIVE_INFERENCE=1 go test ./model/inference/ -run TestLive -v
//
// They are skipped unless GRAFSOL_LIVE_INFERENCE is set. The services are
// looked for at their usual local addresses, or where GRAFSOL_EMBEDDINGS_URL
// and GRAFSOL_SUMMARIES_URL say. Nothing leaves the machine, and nothing
// costs anything but time: a few seconds for the embeddings, a minute or
// two for the summaries.

func liveEmbedder(t *testing.T, cfg EmbedderConfig) *Embedder {
	t.Helper()
	if os.Getenv("GRAFSOL_LIVE_INFERENCE") == "" {
		t.Skip("GRAFSOL_LIVE_INFERENCE is not set")
	}
	cfg.BaseURL = cmp.Or(os.Getenv("GRAFSOL_EMBEDDINGS_URL"), "http://127.0.0.1:3010")
	return NewEmbedder(cfg)
}

func liveSummarizer(t *testing.T) *Summarizer {
	t.Helper()
	if os.Getenv("GRAFSOL_LIVE_INFERENCE") == "" {
		t.Skip("GRAFSOL_LIVE_INFERENCE is not set")
	}
	return NewSummarizer(SummarizerConfig{BaseURL: cmp.Or(os.Getenv("GRAFSOL_SUMMARIES_URL"), "http://127.0.0.1:3011")})
}

const liveIssue = `After release 2024.03.12 the checkout endpoint's p95 latency went from 180 ms to 410 ms. Bisecting points at #812, which removed the product cache because it served stale prices after the pricing migration. Every checkout now makes three extra calls to the catalogue service.

@dana proposes restoring a read-through cache keyed on (sku, price_version). We agreed in the 14 March sync to do the cache first (owner: dana, due 21 March) and to keep batching as a follow-up in #851.`

// TestLiveEmbed embeds a question and two documents with the real service
// and checks that the vectors behave as embeddings should: one per text,
// all of the length the service describes, scaled to length one, and nearer
// for texts that mean similar things.
func TestLiveEmbed(t *testing.T) {
	embedder := liveEmbedder(t, EmbedderConfig{})

	description, err := embedder.Describe(t.Context())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	documents, err := embedder.EmbedDocuments(t.Context(), []string{
		"Checkout latency doubled\n\nCheckout latency doubled after the price cache was removed.",
		"Summer rota\n\nWho on the support team is away in each week of August.",
	})
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}
	queries, err := embedder.EmbedQueries(t.Context(), []string{"Why is checkout slow?"})
	if err != nil {
		t.Fatalf("EmbedQueries: %v", err)
	}

	if len(documents) != 2 || len(queries) != 1 {
		t.Fatalf("%d and %d vectors for 2 documents and 1 query", len(documents), len(queries))
	}
	for i, vector := range append(documents, queries...) {
		if len(vector) != description.Dimensions {
			t.Errorf("vector %d has %d numbers; the service describes %d", i+1, len(vector), description.Dimensions)
		}
		var squares float64
		for _, x := range vector {
			squares += float64(x) * float64(x)
		}
		if length := math.Sqrt(squares); math.Abs(length-1) > 1e-3 {
			t.Errorf("vector %d has length %.4f, want 1", i+1, length)
		}
	}

	related, _ := storage.CosineDistance(queries[0], documents[0])
	unrelated, _ := storage.CosineDistance(queries[0], documents[1])
	t.Logf("%s at %.12s: %d dimensions, texts of up to %d tokens; the question is %.3f from the related document and %.3f from the unrelated one",
		description.Model, description.Revision, description.Dimensions, description.MaxTokens, related, unrelated)
	if related >= unrelated {
		t.Errorf("the question is no nearer the document about checkout (%.3f) than the one about holidays (%.3f)", related, unrelated)
	}
}

// TestLiveEmbedRefusals checks that the real service declines a text too
// long to embed, and says how long it was.
func TestLiveEmbedRefusals(t *testing.T) {
	embedder := liveEmbedder(t, EmbedderConfig{})

	_, err := embedder.EmbedDocuments(t.Context(), []string{"fine", strings.Repeat("latency ", 40_000)})
	var refusal *Error
	if !errors.Is(err, model.ErrDeclined) || !errors.As(err, &refusal) || refusal.Reason != "too_long" {
		t.Errorf("an over-long text gave %v; want a decline for being too long", err)
	} else {
		t.Logf("declined: %s", refusal.Message)
	}
}

// TestLiveSpace checks that the real service names its embedding space, the
// same in its description and in its replies, and that the name says which
// model it is.
func TestLiveSpace(t *testing.T) {
	embedder := liveEmbedder(t, EmbedderConfig{})

	description, err := embedder.Describe(t.Context())
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	// Embedding holds the reply to the space just described: a service that
	// named another in its replies would fail here.
	if _, err := embedder.EmbedQueries(t.Context(), []string{"a question"}); err != nil {
		t.Fatalf("EmbedQueries: %v", err)
	}
	space, err := embedder.Space(t.Context())
	if err != nil {
		t.Fatalf("Space: %v", err)
	}

	t.Logf("the service's embedding space: %s", space)
	if space == "" || space != description.Space {
		t.Errorf("Space is %q, and the description says %q", space, description.Space)
	}
	if !strings.Contains(space, description.Model) || !strings.Contains(space, description.Revision) {
		t.Errorf("the space %q does not say which model at which revision", space)
	}
}

// TestLiveSummarize has the real service summarise a short issue and checks
// that the facts a question would be about are still in the summary.
func TestLiveSummarize(t *testing.T) {
	summarizer := liveSummarizer(t)

	summary, err := summarizer.Summarize(t.Context(), "Checkout latency doubled", liveIssue)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	t.Logf("%d words: %s", len(strings.Fields(summary)), summary)
	for _, fact := range []string{"#812", "410", "dana"} {
		if !strings.Contains(summary, fact) {
			t.Errorf("the summary has lost %q", fact)
		}
	}
	if words := len(strings.Fields(summary)); words > 160 {
		t.Errorf("the summary runs to %d words", words)
	}
}

// TestLiveSummarizeALongDocument sends a document several times longer than
// the model reads at once, with a fact stated only at its very end, and
// checks that the fact reaches the summary. It takes a minute or two.
func TestLiveSummarizeALongDocument(t *testing.T) {
	summarizer := liveSummarizer(t)
	if testing.Short() {
		t.Skip("takes a minute or two")
	}

	filler := []string{
		"The retry budget for the catalogue client stays at two attempts with a 50 ms backoff.",
		"Load tests on staging replayed the traffic of 4 March at 1,200 requests per second.",
		"The dashboards moved to the new metrics cluster, so older figures are not comparable.",
		"The on-call rota for the payments team changes on the first Monday of each month.",
		"A separate thread tracks the flaky integration test in the refunds suite.",
	}
	paragraphs := []string{liveIssue}
	for i := range 900 {
		paragraphs = append(paragraphs, fmt.Sprintf("Comment %d (%d March). %s", i+1, i%27+1, filler[i%len(filler)]))
	}
	paragraphs = append(paragraphs, "Final decision, recorded 19 March: the fix shipped in #845 and p95 fell back to 185 ms. The code name of the rollout was BLUE HERON.")
	text := strings.Join(paragraphs, "\n\n")

	summary, err := summarizer.Summarize(t.Context(), "Checkout latency doubled", text)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	t.Logf("%d characters in, %d words out: %s", len(text), len(strings.Fields(summary)), summary)
	for _, fact := range []string{"#845", "BLUE HERON"} {
		if !strings.Contains(summary, fact) {
			t.Errorf("the summary has lost %q, which the document states only at its end", fact)
		}
	}
}

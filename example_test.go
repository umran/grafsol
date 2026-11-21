package grafsol_test

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage/memory"
	"github.com/umran/grafsol/traverse"
)

// wiki is about the smallest source there can be: three pages, listed on
// request. A real connector has the same shape, with an API behind it.
type wiki struct{}

func (wiki) Name() string { return "wiki" }

// Resolve recognizes the wiki's own addresses, which is what lets an address
// in one page become an edge to another.
func (wiki) Resolve(url string) (graph.ID, bool) {
	page, ours := strings.CutPrefix(url, "https://wiki.test/")
	return graph.DocumentID("wiki", page), ours
}

// Scan lists every page as a change: the page as it now is, what it points
// at, and the labels on it. What those become in the graph is for the
// graph's rules to say; here the rules every source starts with make a
// links_to edge of a mention and an in_topic edge of a label.
func (wiki) Scan(context.Context, source.Checkpoint) iter.Seq2[source.Change, error] {
	page := func(name, title string) source.Change {
		return source.Change{
			Document: graph.Document{
				ID:     graph.DocumentID("wiki", name),
				Type:   graph.TypePage,
				Source: "wiki",
				URL:    "https://wiki.test/" + name,
				Title:  title,
			},
			Version: 1,
		}
	}

	refunds := page("refunds", "How refunds work")
	refunds.MentionURL("https://wiki.test/payments", "Refunds reverse a payment; see the payments page.")
	refunds.Label("Billing", "Invoices, refunds and payment providers.")

	payments := page("payments", "Payment providers")
	payments.Label("Billing", "")

	onboarding := page("onboarding", "Your first week")
	onboarding.Label("Billing", "")

	return func(yield func(source.Change, error) bool) {
		for _, change := range []source.Change{refunds, payments, onboarding} {
			if !yield(change, nil) {
				return
			}
		}
	}
}

// Build a graph from a source, then ask what lies within two hops of a page.
func Example() {
	ctx := context.Background()
	store, ledger := memory.New(), memory.NewLedger()

	// Reconcile scans the source and makes the graph match it.
	syncer := ingest.NewSyncer(ingest.Config{
		Store:   store,
		Ledger:  ledger,
		Sources: []source.Source{wiki{}},
	})
	report, err := syncer.Reconcile(ctx, wiki{})
	if err != nil {
		fmt.Println("reconcile:", err)
		return
	}
	fmt.Printf("%d pages applied\n", report.Applied)

	// From the refunds page, follow its links, and follow its topic out and
	// back in to the other pages that belong to it.
	walk := traverse.Walk(ctx, traverse.FanOut{Store: store}, []graph.ID{"doc:wiki:refunds"}, traverse.Options{
		MaxDepth: 2,
		Follow: []traverse.Step{
			traverse.Out(graph.LinksTo),
			traverse.Out(graph.InTopic),
			traverse.In(graph.InTopic),
		},
	})
	for visit, err := range walk {
		if err != nil {
			fmt.Println("walk:", err)
			return
		}
		line := fmt.Sprintf("%d hops: %-20s %s", visit.Depth, visit.ID, visit.Via.Summary)
		fmt.Println(strings.TrimSpace(line))
	}

	// Output:
	// 3 pages applied
	// 0 hops: doc:wiki:refunds
	// 1 hops: doc:wiki:payments    Refunds reverse a payment; see the payments page.
	// 1 hops: topic:billing
	// 2 hops: doc:wiki:onboarding
}

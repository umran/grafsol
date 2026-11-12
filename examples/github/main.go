// Command github is a worked example of the whole module: it copies the
// issues and pull requests of one GitHub repository into a graph in
// Postgres, summarising and embedding each on the way, and then asks the
// graph a question.
//
// It needs a database, and the module's two model services running on this
// machine (see /inference; the first of these commands downloads the models,
// about 9 GB, once):
//
//	docker compose up -d --wait
//	export GRAFSOL_POSTGRES_URL=postgres://grafsol:grafsol@localhost:54329/grafsol
//	(cd inference && make models)
//	(cd inference && make embeddings)   # in a terminal of its own
//	(cd inference && make summaries)    # in another
//
//	export TYPESAFE_API_KEY=...         # Jev judges the search; only needed with -question
//	export GITHUB_TOKEN=...             # optional for a public repository
//
//	go run ./examples/github -repository owner/name -question "Why was X changed?"
//
// The first run summarises and embeds every issue and pull request in the
// repository, one after another. A summary takes a local model several
// seconds, so try it on a small repository first. Later runs only touch what
// has changed since. Leave out -question to fill the graph without asking
// anything.
//
// The graph it fills is the one cmd/grafsol-mcp serves; run that with the
// same database and the same embeddings service to put the graph in front
// of an agent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/model/inference"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/retrieval/jev"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/source/github"
	"github.com/umran/grafsol/storage/postgres"
	"github.com/umran/grafsol/typesafe"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "github example:", err)
		os.Exit(1)
	}
}

func run() error {
	repository := flag.String("repository", "", "the repository to copy, as `owner/name`")
	question := flag.String("question", "", "a question to ask the graph once it is filled")
	embeddings := flag.String("embeddings", "http://127.0.0.1:3010", "the `address` of the embeddings service")
	summaries := flag.String("summaries", "http://127.0.0.1:3011", "the `address` of the summaries service")
	flag.Parse()
	if *repository == "" {
		return errors.New("name a repository with -repository owner/name")
	}
	databaseURL := os.Getenv("GRAFSOL_POSTGRES_URL")
	if databaseURL == "" {
		return errors.New("GRAFSOL_POSTGRES_URL is not set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Models: the module's own two services. The embedder is used twice, for
	// documents as they come in and for questions when they are asked, and
	// it must be the same model both times.
	embedder := inference.NewEmbedder(inference.EmbedderConfig{BaseURL: *embeddings})
	summarizer := inference.NewSummarizer(inference.SummarizerConfig{BaseURL: *summaries})
	embedding, err := embedder.Describe(ctx)
	if err != nil {
		return fmt.Errorf("%w (is the embeddings service running? see the top of this file)", err)
	}
	fmt.Printf("embedding with %s: vectors of %d numbers\n", embedding.Model, embedding.Dimensions)

	// Storage: the graph and the sync engine's ledger, in one database. The
	// database is created for vectors of the length this model makes. It
	// will also record which model made its vectors, the first time one is
	// stored, and from then on neither the syncing nor the searching below
	// will work through any other.
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := postgres.CreateSchema(ctx, pool, embedding.Dimensions); err != nil {
		return err
	}
	store, ledger := postgres.New(pool), postgres.NewLedger(pool)

	// Ingestion: scan the repository and make the graph match it. Each
	// document that is new or has changed is summarised, then embedded.
	repo, err := github.New(github.Config{Repositories: []string{*repository}, Token: os.Getenv("GITHUB_TOKEN")})
	if err != nil {
		return err
	}
	syncer := ingest.NewSyncer(ingest.Config{
		Store:     store,
		Ledger:    ledger,
		Sources:   []source.Source{repo},
		Enrichers: []ingest.Enricher{ingest.Summarize(summarizer), ingest.Embed(embedder)},
	})
	report, err := syncer.Reconcile(ctx, repo)
	if err != nil {
		return fmt.Errorf("after %d documents applied: %w", report.Applied, err)
	}
	fmt.Printf("%s: %d applied, %d unchanged, %d removed\n", *repository, report.Applied, report.Unchanged, report.Removed)
	if report.Declined > 0 {
		fmt.Printf("  %d of those applied were stored without something a model refused to add; the log above says which, and why\n", report.Declined)
	}

	if *question == "" {
		return nil
	}

	// Retrieval: search the graph for what answers the question.
	apiKey := os.Getenv("TYPESAFE_API_KEY")
	if apiKey == "" {
		return errors.New("TYPESAFE_API_KEY is not set, and a question needs Jev to judge the search")
	}
	engine := retrieval.NewEngine(retrieval.Config{
		Store:     store,
		Searcher:  store,
		Neighbors: store,
		Embedder:  embedder,
		Judge:     jev.New(jev.Config{Client: typesafe.New(typesafe.Config{APIKey: apiKey})}),
	})
	findings, err := engine.Gather(ctx, retrieval.Question{Text: *question, Context: "The repository " + *repository + " on GitHub."})
	if err != nil {
		return err
	}
	show(findings)
	return nil
}

// show prints what a search found, with where each piece came from.
func show(findings *retrieval.Findings) {
	fmt.Printf("\n%s\nThe search ended %s", findings.Question.Text, findings.Verdict)
	if findings.Reason != "" {
		fmt.Printf(" (%s)", findings.Reason)
	}
	fmt.Printf(" after %d steps; the evidence is %.0f%% likely to be enough.\n",
		len(findings.Trace), 100*findings.Sufficient)

	for _, evidence := range findings.Evidence {
		document := evidence.Document
		fmt.Printf("\n  %s  (rated %.2f)\n  %s\n  %s\n", document.Title, evidence.Potential, document.URL, document.Summary)
		for _, edge := range evidence.Path {
			fmt.Printf("    reached by: %s %s %s", edge.From.Key(), edge.Kind, edge.To.Key())
			if edge.Summary != "" {
				fmt.Printf("  (%s)", edge.Summary)
			}
			fmt.Println()
		}
	}
	if len(findings.Evidence) == 0 {
		fmt.Println("\n  Nothing in the graph was found to state any part of the answer.")
	}
	if left := findings.Left; left != (retrieval.Left{}) {
		fmt.Printf("\n  Not reached: %d leads unjudged, %d unfollowed, %d vertices with neighbours unread.\n",
			left.Unjudged, left.Unfollowed, left.Unread)
	}
}

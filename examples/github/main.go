// Command github is a worked example of the whole module: it copies the
// issues and pull requests of one or more GitHub repositories into a graph
// in Postgres, summarising and embedding each on the way, and then asks the
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
// To put several repositories in one graph, repeat -repository or separate
// the names with commas:
//
//	go run ./examples/github -repository owner/api,owner/web
//
// Name every one of them on every run. They are all filed under one source,
// and the sync engine takes a document that two runs in a row did not list
// to be gone: leave a repository out twice, and its documents are removed
// from the graph.
//
// The first run summarises and embeds every issue and pull request, one
// after another. A summary takes a local model several seconds, so try it on
// a small repository first. Later runs only touch what has changed since.
// Leave out -question to fill the graph without asking anything.
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
	"io"
	"iter"
	"os"
	"os/signal"
	"strings"

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
	var named repositories
	flag.Var(&named, "repository", "a repository to copy, as `owner/name`; repeat the flag, or separate names with commas, to put several in one graph")
	question := flag.String("question", "", "a question to ask the graph once it is filled")
	embeddings := flag.String("embeddings", "http://127.0.0.1:3010", "the `address` of the embeddings service")
	summaries := flag.String("summaries", "http://127.0.0.1:3011", "the `address` of the summaries service")
	flag.Parse()
	if len(named) == 0 {
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

	// Ingestion: scan the repositories and make the graph match them. Each
	// document that is new or has changed is summarised, then embedded.
	// Every repository is covered by the one source, and so by the one
	// scan: what a scan does not list, the graph will in time let go of.
	repos, err := github.New(github.Config{Repositories: named, Token: os.Getenv("GITHUB_TOKEN")})
	if err != nil {
		return err
	}
	syncer := ingest.NewSyncer(ingest.Config{
		Store:     store,
		Ledger:    ledger,
		Sources:   []source.Source{repos},
		Enrichers: []ingest.Enricher{ingest.Summarize(summarizer), ingest.Embed(embedder)},
	})
	fmt.Printf("copying %s\n", named.sentence())
	report, err := syncer.Reconcile(ctx, progress{Scanner: repos, out: os.Stdout, every: 10})
	if err != nil {
		return fmt.Errorf("after %d documents applied: %w", report.Applied, err)
	}
	fmt.Printf("%d applied, %d unchanged, %d removed\n", report.Applied, report.Unchanged, report.Removed)
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
	// The judge is told which repositories the question is about.
	setting := named.sentence() + " on GitHub."
	findings, err := engine.Gather(ctx, retrieval.Question{Text: *question, Context: strings.ToUpper(setting[:1]) + setting[1:]})
	if err != nil {
		return err
	}
	show(findings)
	return nil
}

// repositories is the -repository flag: every repository named, in the
// order given. The flag may be repeated, and each use of it may name several
// repositories, separated by commas.
type repositories []string

func (r *repositories) String() string { return strings.Join(*r, ",") }

// Set takes in one use of the flag. An empty name is refused, not passed
// over: it is what an unset shell variable leaves behind, and passing over
// it would quietly copy fewer repositories than were asked for.
func (r *repositories) Set(value string) error {
	for _, name := range strings.Split(value, ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			return errors.New("a repository's name is empty")
		}
		*r = append(*r, name)
	}
	return nil
}

// sentence names the repositories in words: "the repository a/b", or "the
// repositories a/b, c/d and e/f".
func (r repositories) sentence() string {
	switch len(r) {
	case 0:
		return "no repository"
	case 1:
		return "the repository " + r[0]
	}
	last := len(r) - 1
	return "the repositories " + strings.Join(r[:last], ", ") + " and " + r[last]
}

// progress is a scanner that says, as it is read, how far the scan has got.
// A first run summarises every document, which takes minutes, and would
// otherwise pass in silence.
type progress struct {
	source.Scanner
	out   io.Writer
	every int // say so after each this many documents
}

// Scan passes on everything the scanner beneath it lists, untouched.
func (p progress) Scan(ctx context.Context, resume source.Checkpoint) iter.Seq2[source.Change, error] {
	return func(yield func(source.Change, error) bool) {
		read := 0
		for change, err := range p.Scanner.Scan(ctx, resume) {
			if err == nil {
				if read++; read%p.every == 0 {
					fmt.Fprintf(p.out, "  %d documents read\n", read)
				}
			}
			if !yield(change, err) {
				return
			}
		}
	}
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

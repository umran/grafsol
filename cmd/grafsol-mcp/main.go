// Command grafsol-mcp serves a grafsol graph to AI agents over the Model
// Context Protocol.
//
// It offers one tool, "gather": given one or more questions, it searches the
// graph for each and returns the documents that state the answer, with their
// URLs, the path that led to each, and a verdict on whether the evidence is
// enough. It only reads the graph; keeping the graph in step with its
// sources is the job of package ingest, run elsewhere.
//
// How a document was reached is part of what the judge is shown, in the
// words the graph's settings give each kind of edge ("it resolves X"). The
// server reads those settings as it starts and again every minute.
//
// By default the server talks over standard input and output, which is how
// an agent on the same machine starts it. With -http it serves the same tool
// over HTTP instead.
//
// It is configured through the environment:
//
//	GRAFSOL_POSTGRES_URL       the database that holds the graph (required)
//	TYPESAFE_API_KEY           for Jev, which judges the search (required)
//	GRAFSOL_EMBEDDINGS_URL     the embeddings service, which embeds the questions (required)
//	GRAFSOL_JEV_MODEL          the Jev model to ask; the newest if unset
//
// The embeddings service is one of the module's own, in /inference: start
// it with "make embeddings" there. Questions have to be embedded by the
// model that embedded the graph. The graph's database records which that
// was, and a search through a service running any other is refused.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umran/grafsol/model/inference"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/retrieval/jev"
	"github.com/umran/grafsol/storage/postgres"
	"github.com/umran/grafsol/typesafe"
)

func main() {
	// Standard output belongs to the protocol, so everything said to a
	// person goes to standard error.
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("grafsol-mcp stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	address := flag.String("http", "", "serve over HTTP at this `address` (for example 127.0.0.1:8080) instead of over standard input and output. The server does no authentication of its own: bind it to an address only trusted callers can reach.")
	timeout := flag.Duration("timeout", 30*time.Second, "the longest one question may be searched for; a search that runs out of time returns what it has")
	flag.Parse()

	cfg, err := configFrom(os.Getenv)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, cfg.postgresURL)
	if err != nil {
		return fmt.Errorf("connecting to the graph's database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting to the graph's database: %w", err)
	}

	store := postgres.New(pool)

	// The words the judge is shown for each kind of edge are the graph's
	// own, read now and again every minute: a team may define a kind, or
	// reword one, while the server runs.
	words := newWording()
	if err := words.refresh(ctx, store); err != nil {
		logger.Warn("the graph's settings could not be read, so the judge is shown the words every graph begins with; "+
			"a database last written by an earlier version of grafsol has none until a program that writes to it has run",
			"error", err)
	}
	go words.keep(ctx, store, time.Minute, logger)

	engine := retrieval.NewEngine(retrieval.Config{
		Store:     store,
		Searcher:  store,
		Neighbors: store,
		Embedder:  inference.NewEmbedder(inference.EmbedderConfig{BaseURL: cfg.embeddingsURL}),
		Judge: jev.New(jev.Config{
			Client: typesafe.New(typesafe.Config{APIKey: cfg.typesafeKey}),
			Model:  cfg.jevModel,
			Phrase: words.Phrase,
		}),
		MaxDuration: *timeout,
	})
	server := newServer(engine)

	if *address == "" {
		logger.Info("serving the gather tool over standard input and output")
		return server.Run(ctx, &mcp.StdioTransport{})
	}
	return serveHTTP(ctx, logger, *address, server)
}

// serveHTTP serves the MCP server at an address until the context ends.
func serveHTTP(ctx context.Context, logger *slog.Logger, address string, server *mcp.Server) error {
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	httpServer := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second}

	failed := make(chan error, 1)
	go func() { failed <- httpServer.ListenAndServe() }()
	logger.Info("serving the gather tool over HTTP", "address", address)

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}

// config is what the server reads from its environment.
type config struct {
	postgresURL   string
	typesafeKey   string
	jevModel      string
	embeddingsURL string
}

// configFrom reads the configuration through getenv, which is os.Getenv
// outside of tests. Everything that is missing or malformed is reported at
// once, by name; no value is ever put in an error.
func configFrom(getenv func(string) string) (config, error) {
	cfg := config{
		postgresURL:   getenv("GRAFSOL_POSTGRES_URL"),
		typesafeKey:   getenv("TYPESAFE_API_KEY"),
		jevModel:      getenv("GRAFSOL_JEV_MODEL"),
		embeddingsURL: getenv("GRAFSOL_EMBEDDINGS_URL"),
	}

	var problems []string
	for name, value := range map[string]string{
		"GRAFSOL_POSTGRES_URL":   cfg.postgresURL,
		"TYPESAFE_API_KEY":       cfg.typesafeKey,
		"GRAFSOL_EMBEDDINGS_URL": cfg.embeddingsURL,
	} {
		if value == "" {
			problems = append(problems, name+" is not set")
		}
	}
	if address, err := url.Parse(cfg.embeddingsURL); cfg.embeddingsURL != "" && (err != nil || address.Host == "" || (address.Scheme != "http" && address.Scheme != "https")) {
		problems = append(problems, "GRAFSOL_EMBEDDINGS_URL is not an http or https address")
	}
	if len(problems) > 0 {
		// Sorted, because a map lists its entries in no fixed order.
		slices.Sort(problems)
		return config{}, errors.New(strings.Join(problems, "; "))
	}
	return cfg, nil
}

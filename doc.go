// Package grafsol is a knowledge graph of documents: issues, pull requests,
// tickets and wiki pages from the systems a team works in, joined by simple
// typed edges, kept in step with those systems, and searched to answer
// questions.
//
// This package holds no code. It is the map of the module; the code is in
// the packages below. Imports only ever point upward in this list, so each
// package can be read knowing only the ones above it.
//
//   - graph is the vocabulary: Document and Topic vertices, the Edge between
//     them, their IDs, and the Reference and Label that a source reports of
//     a document before anything has decided what they mean. Pure data, no
//     I/O.
//
//   - mapping decides what those become: it holds the kinds of edge a team
//     has defined, the rules that say which kind a reference is and which
//     topic a label files a document under, and the Settings that keep both
//     together. Pure as well: it reads and writes nothing.
//
//   - storage says what it takes to keep a graph: a Store for vertices and
//     edges, a DocumentSearcher and a NeighborSearcher for finding things by
//     similarity, a Record of what each source said, the graph's Settings,
//     and a Ledger for the sync engine's bookkeeping. Backends live beneath
//     it: storage/memory for tests and small graphs, storage/postgres for
//     real ones. Every backend passes the same suites in storage/storagetest.
//
//   - traverse walks the graph: breadth-first, exhaustively, over any Store.
//
//   - source says what a connector to an external system provides: a feed
//     of changes to subscribe to, pushed deliveries to receive, or a full
//     listing to scan. source/github is the first connector.
//
//   - model says what the module needs from language models: an Embedder
//     and a Summarizer. model/inference provides both, by asking the
//     module's own two model services.
//
//   - ingest keeps the graph in step with its sources, and converges on
//     the truth even when changes are lost, repeated or arrive out of order.
//     Its enrichers are where summaries and embeddings get written, and its
//     Composer is where a document's edges are made: from what the source
//     said and the graph's rules, again whenever either changes.
//
//   - typesafe is a client for Jev, the System One model that judges
//     searches.
//
//   - retrieval answers questions: a best-first search that follows the
//     most promising edges until a Judge finds the evidence gathered to be
//     enough, and returns that evidence with the path that led to each
//     piece. retrieval/jev is the Judge built on Jev.
//
//   - internal/evaluation measures the search and its judge against
//     questions whose answers are known. It keeps what Jev answered on a
//     tape, so that a change to a threshold, or to the search itself, can
//     be tried again and again without asking Jev anything.
//
// Three programs put the packages to work. cmd/grafsol-mcp serves a graph to
// AI agents over the Model Context Protocol. cmd/grafsol-eval records and
// reports on the test set. examples/github fills a graph from a GitHub
// repository and asks it a question.
//
// Two folders are not Go. /inference is a Python project that holds the two
// model services: one embeds text and the other writes summaries, each with
// an open model that runs on your own machine. Nothing in this module calls
// a paid model except the judge, Jev. /eval is the test set: a frozen copy
// of a graph, questions about it with the facts that answer them, and what
// the models made of both.
//
// # The shape of the graph
//
// A vertex is a Document or a Topic. An edge runs from one vertex to
// another and has a kind. The kinds are a team's to define; every graph
// begins with these:
//
//	links_to    document -> document   the first mentions the second
//	child_of    document -> document   the first sits under the second
//	resolves    document -> document   the first settles what the second asks for
//	duplicates  document -> document   the first does what the second already does
//	blocks      document -> document   the second waits on the first
//	supersedes  document -> document   the first takes the place of the second
//	in_topic    document -> topic      the document belongs to the topic
//
// A connector does not choose among them. It reports what a document points
// at and the labels on it, and the graph's rules say what kind of edge each
// becomes: "Fixes #38" a resolves edge, a label an in_topic edge. What was
// reported is kept beside the graph, so a team can change its rules and have
// every edge made again without a source being read a second time.
//
// Documents carry a summary and an embedding; edges and topics carry
// summaries too. These are what a search uses to judge where to go next.
//
// Embeddings can be compared only if one model, set up one way, made all of
// them. A graph therefore holds to one such embedding space and records
// which: the sync engine will not store an embedding from another, and the
// search engine will not search with one.
//
// # Putting it together
//
// The example in this package builds a graph in memory from a small source
// and walks it; the one in package retrieval searches a graph for the answer
// to a question. With Postgres the only lines that change are the ones that
// create the store and the ledger:
//
//	pool, err := pgxpool.New(ctx, databaseURL)
//	err = postgres.CreateSchema(ctx, pool, 1024)
//	store, ledger := postgres.New(pool), postgres.NewLedger(pool)
package grafsol

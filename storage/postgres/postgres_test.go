package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest/ingesttest"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
	"github.com/umran/grafsol/storage/postgres"
	"github.com/umran/grafsol/storage/postgres/postgrestest"
	"github.com/umran/grafsol/storage/storagetest"
	"github.com/umran/grafsol/traverse"
)

// The test schemas are created for embeddings of this length.
const dimensions = 4

// A *postgres.Store can be handed to traverse.Walk as it is.
var _ traverse.Expander = (*postgres.Store)(nil)

// open returns a store on a fresh, empty schema, and the pool behind it.
func open(t testing.TB) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	pool := postgrestest.Open(t)
	if err := postgres.CreateSchema(context.Background(), pool, dimensions); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	return postgres.New(pool), pool
}

func TestStore(t *testing.T) {
	storagetest.RunStore(t, dimensions, func(t *testing.T) storage.Store {
		store, _ := open(t)
		return store
	})
}

func TestSearcher(t *testing.T) {
	storagetest.RunSearcher(t, dimensions, func(t *testing.T) (storage.Store, storage.DocumentSearcher) {
		store, _ := open(t)
		return store, store
	})
}

func TestNeighbors(t *testing.T) {
	storagetest.RunNeighborSearcher(t, dimensions, func(t *testing.T) (storage.Store, storage.NeighborSearcher) {
		store, _ := open(t)
		return store, store
	})
}

// The generic scan must hold to the same contract when the Store beneath it
// is a real one that hands out its edges in pages.
func TestNeighborsByScan(t *testing.T) {
	storagetest.RunNeighborSearcher(t, dimensions, func(t *testing.T) (storage.Store, storage.NeighborSearcher) {
		store, _ := open(t)
		return store, storage.NeighborsByScan(store)
	})
}

// The single statement and the generic scan are two ways of doing one thing,
// so on the same graph they must agree neighbour for neighbour.
func TestNeighborsMatchTheScan(t *testing.T) {
	store, _ := open(t)
	ctx := t.Context()
	hub := graph.DocumentID("t", "hub")

	// Neighbour i sits at an angle that grows with i, which keeps every two
	// distances far enough apart that rounding cannot swap them. The IDs are
	// shuffled so that they say nothing about the order.
	const count = 300
	random := rand.New(rand.NewPCG(3, 4))
	names := random.Perm(count)
	var vertices []graph.Vertex
	var edges []graph.Edge
	for i := range count {
		angle := float64(i) * math.Pi / count
		embedding := []float32{float32(math.Cos(angle)), float32(math.Sin(angle)), 0, 0}
		switch {
		case i%10 == 3:
			embedding = nil // never embedded
		case i == 77:
			embedding = []float32{0, 0, 0, 0} // embedded, but pointing nowhere
		}

		var vertex graph.Vertex
		if i%7 == 0 {
			name := fmt.Sprintf("topic-%04d", names[i])
			vertex = &graph.Topic{ID: graph.TopicID(name), Name: name, Summary: "A topic.", Embedding: embedding}
		} else {
			vertex = &graph.Document{ID: graph.DocumentID("t", fmt.Sprintf("n%04d", names[i])), Title: fmt.Sprintf("Neighbour %d", i), Embedding: embedding}
		}
		vertices = append(vertices, vertex)
		id := vertex.VertexID()

		switch {
		case id.Kind() == graph.KindTopic:
			edges = append(edges, graph.Edge{From: hub, Kind: graph.TaggedWith, To: id})
		case i%3 == 0:
			edges = append(edges, graph.Edge{From: id, Kind: graph.ChildOf, To: hub, Summary: "sub-issue of"})
		case i%3 == 1:
			edges = append(edges, graph.Edge{From: hub, Kind: graph.LinksTo, To: id, Summary: fmt.Sprintf("see %d", i)})
		default:
			// Joined to the hub twice, once from each end.
			edges = append(edges,
				graph.Edge{From: hub, Kind: graph.LinksTo, To: id},
				graph.Edge{From: id, Kind: graph.LinksTo, To: hub})
		}
	}
	for i := range 5 {
		edges = append(edges, graph.Edge{From: hub, Kind: graph.LinksTo, To: graph.DocumentID("t", fmt.Sprintf("missing-%d", i))})
	}
	if err := store.PutVertices(ctx, vertices); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}
	if err := store.PutEdges(ctx, edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	list := func(searcher storage.NeighborSearcher) []storage.Neighbor {
		t.Helper()
		var all []storage.Neighbor
		query := storage.NeighborQuery{Vertex: hub, Near: []float32{1, 0, 0, 0}, Limit: 37}
		for {
			page, err := searcher.Neighbors(ctx, query)
			if err != nil {
				t.Fatalf("Neighbors: %v", err)
			}
			all = append(all, page.Neighbors...)
			if page.Next == "" {
				return all
			}
			query.Cursor = page.Next
		}
	}
	statement, scan := list(store), list(storage.NeighborsByScan(store))

	if len(statement) != len(scan) || len(statement) != len(edges)-5 {
		t.Fatalf("the statement lists %d neighbours and the scan %d; the hub has %d edges that lead somewhere", len(statement), len(scan), len(edges)-5)
	}
	for i := range statement {
		a, b := statement[i], scan[i]
		if a.Edge != b.Edge || a.Vertex.VertexID() != b.Vertex.VertexID() {
			t.Fatalf("neighbour %d differs\n statement: %+v\n      scan: %+v", i, a, b)
		}
		if math.Abs(float64(a.Distance-b.Distance)) > 1e-4 {
			t.Errorf("neighbour %d (%s) is at distance %v by the statement and %v by the scan", i, a.Vertex.VertexID(), a.Distance, b.Distance)
		}
	}
}

func TestLedger(t *testing.T) {
	const pageSize = 3
	storagetest.RunLedger(t, pageSize, func(t *testing.T) storage.Ledger {
		_, pool := open(t)
		ledger := postgres.NewLedger(pool)
		ledger.PageSize = pageSize
		return ledger
	})
}

func TestCreateSchema(t *testing.T) {
	pool := postgrestest.Open(t)
	ctx := t.Context()

	for _, bad := range []int{0, -1, postgres.MaxEmbeddingDimensions + 1} {
		if err := postgres.CreateSchema(ctx, pool, bad); err == nil {
			t.Errorf("CreateSchema accepted %d dimensions", bad)
		}
	}

	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	ledger := postgres.NewLedger(pool)
	before, err := ledger.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		t.Fatalf("CreateSchema a second time: %v", err)
	}
	// The ledger's clock must be where it was left. Set back, it would make
	// every mark recorded from then on look older than the scans before it.
	after, err := ledger.Tick(ctx)
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if after <= before {
		t.Errorf("CreateSchema a second time set the ledger's clock back: it read %d, and the next tick gave %d", before, after)
	}

	// The existing table keeps its embedding length; asking for another is
	// a mistake that must not pass silently.
	if err := postgres.CreateSchema(ctx, pool, dimensions+1); err == nil {
		t.Errorf("CreateSchema accepted a different number of dimensions for an existing schema")
	}
}

// The graph's embedding space is kept in the database, not in the handle
// that claimed it: whoever opens the graph next is held to it too.
func TestTheEmbeddingSpaceIsKeptInTheDatabase(t *testing.T) {
	pool := postgrestest.Open(t)
	ctx := t.Context()
	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}
	const space, other = "acme/embedder-a@1f2e; 4 dimensions", "acme/embedder-b@9c0d; 4 dimensions"

	if err := postgres.New(pool).ClaimEmbeddingSpace(ctx, space); err != nil {
		t.Fatalf("ClaimEmbeddingSpace: %v", err)
	}

	// Creating the schema again, as every program does when it starts,
	// must leave the record alone.
	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		t.Fatalf("CreateSchema a second time: %v", err)
	}
	later := postgres.New(pool)
	if got, err := later.EmbeddingSpace(ctx); err != nil || got != space {
		t.Errorf("a new handle on the graph finds the space %q, %v; want %q", got, err, space)
	}
	if err := later.ClaimEmbeddingSpace(ctx, other); !errors.Is(err, storage.ErrOtherSpace) {
		t.Errorf("a new handle claimed another space: %v", err)
	}
	if err := later.ClaimEmbeddingSpace(ctx, space); err != nil {
		t.Errorf("a new handle could not claim the recorded space: %v", err)
	}
}

// The index looks at 40 candidates per search unless told otherwise. Asking
// for more than that must still return as many documents as were asked for.
func TestNearestDocumentsBeyondTheDefaultCandidateCount(t *testing.T) {
	store, _ := open(t)
	const documents, k = 300, 120

	vertices := make([]graph.Vertex, documents)
	for i := range vertices {
		// Spread around a quarter circle: document i is at angle i from the
		// query, so document 0 is nearest and the order is the numbering.
		angle := float64(i) / documents * (math.Pi / 2)
		vertices[i] = &graph.Document{
			ID:        graph.DocumentID("s", fmt.Sprintf("%04d", i)),
			Embedding: []float32{float32(math.Cos(angle)), float32(math.Sin(angle)), 0, 0},
		}
	}
	if err := store.PutVertices(t.Context(), vertices); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}

	matches, err := store.NearestDocuments(t.Context(), []float32{1, 0, 0, 0}, k)
	if err != nil {
		t.Fatalf("NearestDocuments: %v", err)
	}
	if len(matches) != k {
		t.Fatalf("asked for the %d nearest of %d documents, got %d", k, documents, len(matches))
	}
	for i, match := range matches {
		if i > 0 && match.Distance < matches[i-1].Distance {
			t.Fatalf("match %d is nearer than match %d: results are not in order", i, i-1)
		}
	}
	if matches[0].ID != vertices[0].VertexID() {
		t.Errorf("the nearest document is %s, want %s", matches[0].ID, vertices[0].VertexID())
	}
}

func TestExpandMatchesFanOut(t *testing.T) {
	store, _ := open(t)
	edges, vertices := randomGraph(400, 6)
	if err := store.PutEdges(t.Context(), edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	// More vertices than fit in one query, some repeated, some unknown.
	frontier := append(slices.Clone(vertices), vertices[:50]...)
	frontier = append(frontier, "doc:t:nobody-links-here")
	for len(frontier) < 2500 {
		frontier = append(frontier, vertices...)
	}

	fanOut := traverse.FanOut{Store: store}
	for _, direction := range []graph.Direction{graph.Out, graph.In} {
		for _, kind := range []graph.EdgeKind{"", graph.LinksTo, graph.ChildOf, graph.InTopic} {
			// The reference is given each vertex once; both expanders are
			// then given the frontier with its repeats and must not let
			// them show.
			want := collect(t, fanOut.Expand(t.Context(), dedupe(frontier), direction, kind))
			if viaFanOut := collect(t, fanOut.Expand(t.Context(), frontier, direction, kind)); !sameEdges(viaFanOut, want) {
				t.Errorf("%s %q: FanOut returned %d edges for a frontier with repeats, want %d", direction, kind, len(viaFanOut), len(want))
			}
			got := collect(t, store.Expand(t.Context(), frontier, direction, kind))
			if !sameEdges(got, want) {
				t.Errorf("%s %q: Expand returned %d edges, FanOut %d, or different ones", direction, kind, len(got), len(want))
			}
			if len(want) == 0 && kind != graph.InTopic {
				t.Errorf("%s %q: the comparison is vacuous, no edges matched", direction, kind)
			}
		}
	}

	// A walk comes out the same, visit for visit, with either expander.
	opts := traverse.Options{Follow: []traverse.Step{traverse.Out(graph.LinksTo), traverse.In(graph.ChildOf)}}
	seeds := vertices[:3]
	viaFanOut, err := traverse.Collect(traverse.Walk(t.Context(), fanOut, seeds, opts))
	if err != nil {
		t.Fatalf("Walk with FanOut: %v", err)
	}
	viaExpand, err := traverse.Collect(traverse.Walk(t.Context(), store, seeds, opts))
	if err != nil {
		t.Fatalf("Walk with Expand: %v", err)
	}
	if !slices.Equal(viaFanOut, viaExpand) {
		t.Errorf("the two walks differ: %d visits with FanOut, %d with Expand", len(viaFanOut), len(viaExpand))
	}
	if len(viaFanOut) < 100 {
		t.Errorf("the walk only reached %d vertices; the comparison is too weak", len(viaFanOut))
	}
}

func TestExpandRejectsBadArguments(t *testing.T) {
	store, _ := open(t)
	for _, err := range store.Expand(t.Context(), []graph.ID{"doc:t:a"}, graph.Direction(0), "") {
		if err == nil {
			t.Errorf("Expand accepted a missing direction")
		}
	}
	for _, err := range store.Expand(t.Context(), []graph.ID{"doc:t:a"}, graph.Out, "Not A Kind") {
		if err == nil {
			t.Errorf("Expand accepted a malformed kind")
		}
	}
}

// Stopping an expansion early abandons a query in mid-flight. The pool must
// come through that in working order.
func TestExpandStoppedEarlyLeavesThePoolUsable(t *testing.T) {
	store, pool := open(t)
	edges, vertices := randomGraph(300, 20)
	if err := store.PutEdges(t.Context(), edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	for range 3 * int(pool.Config().MaxConns) {
		for _, err := range store.Expand(t.Context(), vertices, graph.Out, "") {
			if err != nil {
				t.Fatalf("Expand: %v", err)
			}
			break
		}
	}

	all := collect(t, store.Expand(t.Context(), vertices, graph.Out, ""))
	if len(all) != len(edges) {
		t.Errorf("after stopping early many times, a full expansion returned %d of %d edges", len(all), len(edges))
	}
}

func TestExpandReportsAnEndedContext(t *testing.T) {
	store, _ := open(t)
	if err := store.PutEdges(t.Context(), []graph.Edge{{From: "doc:t:a", Kind: graph.LinksTo, To: "doc:t:b"}}); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var failure error
	for _, err := range store.Expand(ctx, []graph.ID{"doc:t:a"}, graph.Out, "") {
		if err != nil {
			failure = err
		}
	}
	if failure == nil {
		t.Errorf("Expand with an ended context finished without an error")
	}
}

// The Syncer's promise, checked against Postgres: locks, ledger and all.
func TestSyncerConverges(t *testing.T) {
	sizes := ingesttest.Options{Rounds: 3, Changes: 80, Workers: 6}
	for seed := range uint64(3) {
		t.Run(fmt.Sprintf("with repair/seed=%d", seed), func(t *testing.T) {
			store, pool := open(t)
			ingesttest.Converge(t, store, postgres.NewLedger(pool), seed, sizes)
		})
		t.Run(fmt.Sprintf("feed alone/seed=%d", seed), func(t *testing.T) {
			store, pool := open(t)
			ingesttest.ConvergeWithoutLoss(t, store, postgres.NewLedger(pool), seed, sizes)
		})
	}
}

// A search asks the store for seeds and for ranked neighbours, a page at a
// time. Whichever backend answers, the same search over the same graph must
// take the same steps and gather the same evidence.
func TestASearchGoesTheSameWayOnPostgresAsInMemory(t *testing.T) {
	onPostgres, _ := open(t)
	inMemory := memory.New()
	ctx := t.Context()

	// Vertex i sits at its own angle from the question, a degree apart from
	// the next, so that no two are near enough for rounding to reorder them.
	random := rand.New(rand.NewPCG(8, 9))
	var vertices []graph.Vertex
	for i := range 90 {
		angle := float64(i) * math.Pi / 180
		embedding := []float32{float32(math.Cos(angle)), float32(math.Sin(angle)), 0, 0}
		if i%9 == 4 {
			name := fmt.Sprintf("topic-%02d", i)
			vertices = append(vertices, &graph.Topic{ID: graph.TopicID(name), Name: name, Embedding: embedding})
		} else {
			vertices = append(vertices, &graph.Document{ID: graph.DocumentID("t", fmt.Sprintf("d%02d", i)), Title: fmt.Sprintf("Document %d", i), Embedding: embedding})
		}
	}
	var edges []graph.Edge
	for range 400 {
		from, to := vertices[random.IntN(len(vertices))].VertexID(), vertices[random.IntN(len(vertices))].VertexID()
		switch {
		case from == to || from.Kind() == graph.KindTopic:
		case to.Kind() == graph.KindTopic:
			edges = append(edges, graph.Edge{From: from, Kind: graph.TaggedWith, To: to})
		default:
			edges = append(edges, graph.Edge{From: from, Kind: graph.LinksTo, To: to, Summary: "see also"})
		}
	}
	for _, store := range []storage.Store{onPostgres, inMemory} {
		if err := store.PutVertices(ctx, vertices); err != nil {
			t.Fatalf("PutVertices: %v", err)
		}
		if err := store.PutEdges(ctx, edges); err != nil {
			t.Fatalf("PutEdges: %v", err)
		}
	}

	search := func(store storage.Store, searcher storage.DocumentSearcher, neighbors storage.NeighborSearcher) *retrieval.Findings {
		t.Helper()
		engine := retrieval.NewEngine(retrieval.Config{
			Store: store, Searcher: searcher, Neighbors: neighbors,
			Embedder: fixedEmbedder{1, 0, 0, 0}, Judge: ratingByID{},
			Seeds: 5, Batch: 7, MaxSteps: -1,
		})
		findings, err := engine.Gather(ctx, retrieval.Question{Text: "?"})
		if err != nil {
			t.Fatalf("Gather: %v", err)
		}
		return findings
	}
	want := search(inMemory, inMemory, inMemory)
	got := search(onPostgres, onPostgres, onPostgres)

	if got.Verdict != want.Verdict || got.Left != want.Left || len(got.Trace) != len(want.Trace) || len(got.Evidence) != len(want.Evidence) {
		t.Fatalf("on Postgres the search ended %s after %d steps with %d pieces of evidence, leaving %+v;\nin memory, %s after %d steps with %d, leaving %+v",
			got.Verdict, len(got.Trace), len(got.Evidence), got.Left, want.Verdict, len(want.Trace), len(want.Evidence), want.Left)
	}
	for i := range want.Trace {
		a, b := got.Trace[i], want.Trace[i]
		if len(a.Judged) != len(b.Judged) || !slices.Equal(a.Followed, b.Followed) {
			t.Fatalf("step %d differs: Postgres judged %d leads and followed %v; memory judged %d and followed %v",
				i+1, len(a.Judged), a.Followed, len(b.Judged), b.Followed)
		}
		for j := range b.Judged {
			x, y := a.Judged[j], b.Judged[j]
			if x.Lead.Target.VertexID() != y.Lead.Target.VertexID() || x.Lead.Edge != y.Lead.Edge || x.Potential != y.Potential ||
				math.Abs(x.Lead.Estimate-y.Lead.Estimate) > 1e-4 {
				t.Fatalf("step %d, lead %d differs\n Postgres: %+v\n   memory: %+v", i+1, j+1, x, y)
			}
		}
	}
	for i := range want.Evidence {
		if got.Evidence[i].Document.ID != want.Evidence[i].Document.ID || !slices.Equal(got.Evidence[i].Path, want.Evidence[i].Path) {
			t.Fatalf("evidence %d differs\n Postgres: %s by %v\n   memory: %s by %v", i+1,
				got.Evidence[i].Document.ID, got.Evidence[i].Path, want.Evidence[i].Document.ID, want.Evidence[i].Path)
		}
	}
	// The search has to have amounted to something for the comparison to.
	if len(want.Trace) < 5 || len(want.Evidence) < 3 {
		t.Fatalf("the search was too slight to prove anything: %d steps, %d pieces of evidence", len(want.Trace), len(want.Evidence))
	}
}

// fixedEmbedder embeds every text as the same vector.
type fixedEmbedder []float32

func (f fixedEmbedder) EmbedQueries(_ context.Context, texts []string) ([][]float32, error) {
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = f
	}
	return vectors, nil
}

func (f fixedEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return f.EmbedQueries(ctx, texts)
}

func (f fixedEmbedder) Space(context.Context) (string, error) {
	return "acme/fixed-embedder@1; 4 dimensions", nil
}

// ratingByID is a judge with fixed opinions: it rates a vertex by the last
// character of its ID, and is never satisfied.
type ratingByID struct{}

func (ratingByID) Assess(_ context.Context, _ retrieval.Question, _ []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	assessment := retrieval.Assessment{Potential: make([]float64, len(leads))}
	for i, lead := range leads {
		id := lead.Target.VertexID()
		assessment.Potential[i] = float64(id[len(id)-1]%4) / 3 // 0, a third, two thirds or 1
	}
	return assessment, nil
}

// BenchmarkFrontierExpansion compares the two ways of expanding a frontier
// over the same store: one Store.Edges call per vertex (FanOut) against one
// query per thousand vertices (Store.Expand).
func BenchmarkFrontierExpansion(b *testing.B) {
	store, _ := open(b)
	edges, vertices := randomGraph(5000, 10)
	if err := store.PutEdges(context.Background(), edges); err != nil {
		b.Fatalf("PutEdges: %v", err)
	}

	for _, size := range []int{10, 100, 1000} {
		frontier := vertices[:size]
		expanders := []struct {
			name string
			x    traverse.Expander
		}{
			{"FanOut", traverse.FanOut{Store: store}},
			{"Expand", store},
		}
		for _, expander := range expanders {
			b.Run(fmt.Sprintf("frontier=%d/%s", size, expander.name), func(b *testing.B) {
				for b.Loop() {
					count := 0
					for _, err := range expander.x.Expand(context.Background(), frontier, graph.Out, "") {
						if err != nil {
							b.Fatalf("Expand: %v", err)
						}
						count++
					}
					if count == 0 {
						b.Fatalf("no edges")
					}
				}
			})
		}
	}
}

// BenchmarkNeighborsOfAHub measures one page of the neighbours of a vertex,
// for vertices with more and more edges, at the embedding length that common
// models produce. Every page costs about the same, because Postgres measures
// the whole neighbourhood each time; the benchmark shows how that grows.
func BenchmarkNeighborsOfAHub(b *testing.B) {
	const dimensions = 1536
	ctx := context.Background()
	pool := postgrestest.Open(b)
	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		b.Fatalf("CreateSchema: %v", err)
	}
	store := postgres.New(pool)
	random := rand.New(rand.NewPCG(5, 6))
	embedding := func() []float32 {
		v := make([]float32, dimensions)
		for i := range v {
			v[i] = float32(random.NormFloat64())
		}
		return v
	}

	degrees := []int{100, 1000, 10000}
	for _, degree := range degrees {
		hub := graph.DocumentID("t", fmt.Sprintf("hub-%d", degree))
		for start := 0; start < degree; start += 500 {
			var vertices []graph.Vertex
			var edges []graph.Edge
			for i := start; i < min(start+500, degree); i++ {
				id := graph.DocumentID("t", fmt.Sprintf("hub-%d/n%05d", degree, i))
				vertices = append(vertices, &graph.Document{ID: id, Title: "A neighbour", Summary: "Something it says.", Embedding: embedding()})
				edges = append(edges, graph.Edge{From: id, Kind: graph.InTopic, To: hub, Summary: "belongs to"})
			}
			if err := store.PutVertices(ctx, vertices); err != nil {
				b.Fatalf("PutVertices: %v", err)
			}
			if err := store.PutEdges(ctx, edges); err != nil {
				b.Fatalf("PutEdges: %v", err)
			}
		}
	}
	if _, err := pool.Exec(ctx, "analyze"); err != nil {
		b.Fatalf("analyze: %v", err)
	}

	near := embedding()
	for _, degree := range degrees {
		hub := graph.DocumentID("t", fmt.Sprintf("hub-%d", degree))
		b.Run(fmt.Sprintf("degree=%d", degree), func(b *testing.B) {
			for b.Loop() {
				page, err := store.Neighbors(ctx, storage.NeighborQuery{Vertex: hub, Near: near, Limit: 64})
				if err != nil {
					b.Fatalf("Neighbors: %v", err)
				}
				if len(page.Neighbors) != 64 {
					b.Fatalf("the page holds %d neighbours, want 64", len(page.Neighbors))
				}
			}
		})
	}
}

// --- helpers ----------------------------------------------------------------

// randomGraph returns a repeatable random graph: the given number of
// documents, each with about the given number of outgoing edges of assorted
// kinds, plus a few topics.
func randomGraph(documents, edgesEach int) (edges []graph.Edge, vertices []graph.ID) {
	random := rand.New(rand.NewPCG(1, 2))
	for i := range documents {
		vertices = append(vertices, graph.DocumentID("t", fmt.Sprintf("d%05d", i)))
	}
	topics := []graph.ID{graph.TopicID("billing"), graph.TopicID("auth"), graph.TopicID("search")}

	seen := make(map[graph.Edge]bool)
	for _, from := range vertices {
		for range edgesEach {
			edge := graph.Edge{From: from, Kind: graph.LinksTo, To: vertices[random.IntN(documents)]}
			switch random.IntN(6) {
			case 0:
				edge.Kind = graph.ChildOf
			case 1:
				edge.Kind, edge.To = graph.TaggedWith, topics[random.IntN(len(topics))]
			}
			if !seen[edge] {
				seen[edge] = true
				edge.Summary = fmt.Sprintf("%s %s %s", edge.From.Key(), edge.Kind, edge.To.Key())
				edges = append(edges, edge)
			}
		}
	}
	return edges, vertices
}

func collect(t *testing.T, edges func(func(graph.Edge, error) bool)) []graph.Edge {
	t.Helper()
	var all []graph.Edge
	for edge, err := range edges {
		if err != nil {
			t.Fatalf("Expand: %v", err)
		}
		all = append(all, edge)
	}
	return all
}

// sameEdges reports whether a and b hold the same edges, summaries included,
// in any order and without repeats.
func sameEdges(a, b []graph.Edge) bool {
	sorted := func(edges []graph.Edge) []graph.Edge {
		return slices.SortedFunc(slices.Values(edges), graph.Edge.Compare)
	}
	return slices.Equal(sorted(a), sorted(b))
}

func dedupe(ids []graph.ID) []graph.ID {
	return slices.Compact(slices.Sorted(slices.Values(ids)))
}

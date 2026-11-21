package postgres_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/postgres"
	"github.com/umran/grafsol/storage/postgres/postgrestest"
)

// taggedWith is the kind that labels made before upgrade 1.
const taggedWith graph.EdgeKind = "tagged_with"

// openBeforeUpgrades returns a store on a database whose tables are exactly
// those the first release of grafsol created: no record of what sources
// said, no settings, no list of upgrades.
func openBeforeUpgrades(t *testing.T) (*postgres.Store, *pgxpool.Pool) {
	t.Helper()
	pool := postgrestest.Open(t)
	schema, err := os.ReadFile("testdata/schema-before-upgrades.sql")
	if err != nil {
		t.Fatalf("reading the old schema: %v", err)
	}
	if _, err := pool.Exec(t.Context(), strings.ReplaceAll(string(schema), "{{dimensions}}", fmt.Sprint(dimensions))); err != nil {
		t.Fatalf("creating the old schema: %v", err)
	}
	return postgres.New(pool), pool
}

func TestUpgradeWritesDownWhatSourcesMustHaveSaid(t *testing.T) {
	store, pool := openBeforeUpgrades(t)
	ctx := t.Context()

	const (
		pullRequest = graph.ID("doc:github:acme/api#43")
		issue       = graph.ID("doc:github:acme/api#42")
		never       = graph.ID("doc:github:acme/api#99") // has edges, and was never stored itself
	)
	created := time.Date(2026, time.February, 3, 4, 5, 6, 0, time.UTC)
	stored := &graph.Document{
		ID: pullRequest, Type: graph.TypePullRequest, Source: "github", URL: "https://github.com/acme/api/pull/43",
		Title: "Stop refunds timing out", Summary: "Retries the refund call with a backoff.",
		Embedding: []float32{0.25, -0.5, 1, 0}, CreatedAt: created, UpdatedAt: created.Add(time.Hour),
	}
	plain := &graph.Document{ID: issue, Type: graph.TypeIssue, Source: "github", Title: "Refunds time out"}

	// The graph as the sync engine of the first release left it.
	if err := store.PutVertices(ctx, []graph.Vertex{
		stored, plain,
		&graph.Topic{ID: "topic:bug", Name: "Bug", Summary: "Something is not working", Embedding: []float32{1, 0, 0, 0}},
		&graph.Topic{ID: "topic:area/billing", Name: "area/billing"},
		// Two topics somebody put in by hand, under IDs no label would make.
		&graph.Topic{ID: "topic:Odd One", Name: "Odd One", Summary: "Not from a label."},
		&graph.Topic{ID: "topic:Odd Two", Name: "Odd Two"},
	}); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}
	if err := store.PutEdges(ctx, []graph.Edge{
		{From: pullRequest, Kind: graph.LinksTo, To: issue, Summary: "Fixes #42."},
		{From: pullRequest, Kind: graph.LinksTo, To: "doc:github:acme/web#9", Summary: "Closes acme/web#9, #10 and #11."},
		{From: pullRequest, Kind: graph.ChildOf, To: "doc:github:acme/api#7", Summary: "sub-issue of"},
		{From: pullRequest, Kind: taggedWith, To: "topic:bug"},
		{From: pullRequest, Kind: taggedWith, To: "topic:docs"}, // the topic itself is not stored
		{From: pullRequest, Kind: taggedWith, To: "topic:Odd One"},
		{From: pullRequest, Kind: taggedWith, To: "topic:Odd Two"}, // the document is in this one twice over
		{From: pullRequest, Kind: graph.InTopic, To: "topic:Odd Two", Summary: "also filed there by hand"},
		{From: pullRequest, Kind: taggedWith, To: "topic:area/billing"},
		{From: pullRequest, Kind: graph.InTopic, To: "topic:area/billing", Summary: "filed there by its source"},
		{From: pullRequest, Kind: "reviewed_by", To: "doc:github:acme/api#1", Summary: "requested"},
		{From: never, Kind: taggedWith, To: "topic:bug"},
	}); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		t.Fatalf("CreateSchema on the old database: %v", err)
	}

	// What the two documents' sources must have said.
	said, err := store.Said(ctx, []graph.ID{pullRequest, issue, never})
	if err != nil {
		t.Fatalf("Said: %v", err)
	}
	if len(said) != 2 {
		t.Fatalf("%d documents are on record, want the 2 that are stored", len(said))
	}
	want := storage.Said{
		Document: graph.Document{
			ID: pullRequest, Type: graph.TypePullRequest, Source: "github", URL: "https://github.com/acme/api/pull/43",
			Title: "Stop refunds timing out", Summary: "Retries the refund call with a backoff.",
			CreatedAt: created, UpdatedAt: created.Add(time.Hour),
		},
		References: []graph.Reference{
			{Target: "doc:github:acme/api#7", Form: graph.FormParent, Context: "sub-issue of"},
			{Target: issue, Form: graph.FormMention, Context: "Fixes #42."},
			{Target: "doc:github:acme/web#9", Form: graph.FormMention, Context: "Closes acme/web#9, #10 and #11."},
		},
		Labels: []graph.Label{
			{Name: "area/billing"},
			{Name: "Bug", Description: "Something is not working"},
			{Name: "docs"},
		},
		// A tagged_with edge that no label would lead back to is kept as an
		// edge stated outright, unless the document already had an in_topic
		// edge to the same topic: then that one says it all.
		Edges: []graph.Edge{
			{From: pullRequest, Kind: graph.InTopic, To: "topic:Odd Two", Summary: "also filed there by hand"},
			{From: pullRequest, Kind: graph.InTopic, To: "topic:area/billing", Summary: "filed there by its source"},
			{From: pullRequest, Kind: "reviewed_by", To: "doc:github:acme/api#1", Summary: "requested"},
			{From: pullRequest, Kind: graph.InTopic, To: "topic:Odd One"},
		},
		Topics: []graph.Topic{
			{ID: "topic:Odd Two", Name: "Odd Two"},
			{ID: "topic:area/billing", Name: "area/billing"},
			{ID: "topic:Odd One", Name: "Odd One", Summary: "Not from a label."},
		},
	}
	requireSameSaid(t, said[pullRequest], want)
	requireSameSaid(t, said[issue], storage.Said{Document: *plain})

	// Labels are in_topic edges now, and nothing else has moved.
	requireOutEdges(t, store, pullRequest,
		graph.Edge{From: pullRequest, Kind: graph.ChildOf, To: "doc:github:acme/api#7", Summary: "sub-issue of"},
		graph.Edge{From: pullRequest, Kind: graph.InTopic, To: "topic:Odd One"},
		graph.Edge{From: pullRequest, Kind: graph.InTopic, To: "topic:Odd Two", Summary: "also filed there by hand"},
		graph.Edge{From: pullRequest, Kind: graph.InTopic, To: "topic:area/billing", Summary: "filed there by its source"},
		graph.Edge{From: pullRequest, Kind: graph.InTopic, To: "topic:bug"},
		graph.Edge{From: pullRequest, Kind: graph.InTopic, To: "topic:docs"},
		graph.Edge{From: pullRequest, Kind: graph.LinksTo, To: issue, Summary: "Fixes #42."},
		graph.Edge{From: pullRequest, Kind: graph.LinksTo, To: "doc:github:acme/web#9", Summary: "Closes acme/web#9, #10 and #11."},
		graph.Edge{From: pullRequest, Kind: "reviewed_by", To: "doc:github:acme/api#1", Summary: "requested"},
	)
	requireOutEdges(t, store, never, graph.Edge{From: never, Kind: graph.InTopic, To: "topic:bug"})

	// The vertices are as they were, embeddings included.
	vertices, err := store.GetVertices(ctx, []graph.ID{pullRequest})
	if err != nil {
		t.Fatalf("GetVertices: %v", err)
	}
	if after := vertices[pullRequest].(*graph.Document); after.Summary != stored.Summary || !slices.Equal(after.Embedding, stored.Embedding) {
		t.Errorf("the upgrade changed a stored document: %+v", after)
	}
}

// An upgrade is run once. A second CreateSchema must leave alone what has
// been written since the first, however much it looks like what the upgrade
// was written to change.
func TestUpgradeRunsOnce(t *testing.T) {
	store, pool := openBeforeUpgrades(t)
	ctx := t.Context()
	const id = graph.ID("doc:github:acme/api#1")

	if err := store.PutVertices(ctx, []graph.Vertex{&graph.Document{ID: id, Source: "github", Title: "An issue"}}); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}
	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}

	// Since the upgrade: the sync engine has written the whole of what the
	// source said, and something has put a tagged_with edge in.
	later := storage.Said{
		Document: graph.Document{ID: id, Source: "github", Title: "An issue"},
		Labels:   []graph.Label{{Name: "written since the upgrade"}},
	}
	if err := store.PutSaid(ctx, later); err != nil {
		t.Fatalf("PutSaid: %v", err)
	}
	stray := graph.Edge{From: id, Kind: taggedWith, To: "topic:bug"}
	if err := store.PutEdges(ctx, []graph.Edge{stray}); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	for range 2 {
		if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
			t.Fatalf("CreateSchema again: %v", err)
		}
	}
	said, err := store.Said(ctx, []graph.ID{id})
	if err != nil {
		t.Fatalf("Said: %v", err)
	}
	requireSameSaid(t, said[id], later)
	requireOutEdges(t, store, id, stray)

	var upgrades []int
	rows, err := pool.Query(ctx, `select upgrade from schema_upgrades order by upgrade`)
	if err != nil {
		t.Fatalf("reading schema_upgrades: %v", err)
	}
	for rows.Next() {
		var upgrade int
		if err := rows.Scan(&upgrade); err != nil {
			t.Fatalf("reading schema_upgrades: %v", err)
		}
		upgrades = append(upgrades, upgrade)
	}
	if !slices.Equal(upgrades, []int{1}) {
		t.Errorf("the upgrades on record are %v, want [1]", upgrades)
	}
}

// The upgrade reads the documents a page at a time. A graph of more than
// one page must come through whole.
func TestUpgradeOfMoreDocumentsThanAPage(t *testing.T) {
	store, pool := openBeforeUpgrades(t)
	ctx := t.Context()

	const count = 1234
	vertices := make([]graph.Vertex, count)
	edges := make([]graph.Edge, count)
	for i := range count {
		id := graph.DocumentID("github", fmt.Sprintf("acme/api#%d", i+1))
		vertices[i] = &graph.Document{ID: id, Source: "github", Title: fmt.Sprintf("Issue %d", i+1)}
		edges[i] = graph.Edge{From: id, Kind: taggedWith, To: graph.TopicID(fmt.Sprintf("label %d", i%7))}
	}
	if err := store.PutVertices(ctx, vertices); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}
	if err := store.PutEdges(ctx, edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}
	if err := postgres.CreateSchema(ctx, pool, dimensions); err != nil {
		t.Fatalf("CreateSchema: %v", err)
	}

	var recorded []graph.ID
	for cursor := ""; ; {
		ids, next, err := store.Recorded(ctx, cursor)
		if err != nil {
			t.Fatalf("Recorded: %v", err)
		}
		recorded = append(recorded, ids...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(recorded) != count {
		t.Fatalf("%d documents are on record, want %d", len(recorded), count)
	}
	said, err := store.Said(ctx, recorded)
	if err != nil {
		t.Fatalf("Said: %v", err)
	}
	for i, vertex := range vertices {
		got := said[vertex.VertexID()]
		want := []graph.Label{{Name: fmt.Sprintf("label-%d", i%7)}}
		if got.Document.Title != fmt.Sprintf("Issue %d", i+1) || !slices.Equal(got.Labels, want) {
			t.Fatalf("what was said of %s is %+v, want its title and the label %v", vertex.VertexID(), got, want)
		}
	}
}

func TestCreateSchemaOnANewDatabase(t *testing.T) {
	store, pool := open(t) // CreateSchema on an empty database
	ctx := t.Context()

	document, version, err := store.Settings(ctx)
	if err != nil || len(document) != 0 || version != 0 {
		t.Errorf("a new database has the settings %q at version %d (error %v), want none at version 0", document, version, err)
	}
	var had int
	if err := pool.QueryRow(ctx, `select count(*) from schema_upgrades`).Scan(&had); err != nil || had == 0 {
		t.Errorf("a new database has %d upgrades on record (error %v), want every one: it needs none of them", had, err)
	}
}

func TestCreateSchemaRefusesADatabaseFromALaterVersion(t *testing.T) {
	_, pool := open(t)
	ctx := t.Context()
	if _, err := pool.Exec(ctx, `insert into schema_upgrades (upgrade, name) values (9999, 'from the future')`); err != nil {
		t.Fatalf("recording an upgrade: %v", err)
	}
	err := postgres.CreateSchema(ctx, pool, dimensions)
	if err == nil || !strings.Contains(err.Error(), "9999") {
		t.Errorf("CreateSchema on a database a later version has used: %v, want an error that names upgrade 9999", err)
	}
}

func requireSameSaid(t *testing.T, got, want storage.Said) {
	t.Helper()
	sameDocument := got.Document.ID == want.Document.ID && got.Document.Type == want.Document.Type &&
		got.Document.Source == want.Document.Source && got.Document.URL == want.Document.URL &&
		got.Document.Title == want.Document.Title && got.Document.Summary == want.Document.Summary &&
		slices.Equal(got.Document.Embedding, want.Document.Embedding) &&
		got.Document.CreatedAt.Equal(want.Document.CreatedAt) && got.Document.UpdatedAt.Equal(want.Document.UpdatedAt)
	sameTopics := slices.EqualFunc(got.Topics, want.Topics, func(a, b graph.Topic) bool {
		return a.ID == b.ID && a.Name == b.Name && a.Summary == b.Summary && slices.Equal(a.Embedding, b.Embedding)
	})
	if !sameDocument || !sameTopics || !slices.Equal(got.References, want.References) ||
		!slices.Equal(got.Labels, want.Labels) || !slices.Equal(got.Edges, want.Edges) {
		t.Fatalf("what was said of %s is\n  %+v\nwant\n  %+v", want.Document.ID, got, want)
	}
}

func requireOutEdges(t *testing.T, store *postgres.Store, id graph.ID, want ...graph.Edge) {
	t.Helper()
	page, err := store.Edges(t.Context(), storage.EdgeQuery{Vertex: id, Direction: graph.Out})
	if err != nil {
		t.Fatalf("Edges(%s): %v", id, err)
	}
	slices.SortFunc(want, graph.Edge.Compare)
	if !slices.Equal(page.Edges, want) {
		t.Fatalf("the edges of %s are\n  %+v\nwant\n  %+v", id, page.Edges, want)
	}
}

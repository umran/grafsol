package github_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/mapping"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
	"github.com/umran/grafsol/storage/memory"
)

// A graph filled before sources reported references and labels has, in its
// ledger, a fingerprint for every document that was taken from the edges the
// connector then made itself. If the fingerprint of an unchanged issue came
// out any different today, the first scan after an upgrade would find every
// document changed, and summarise and embed every one of them again.
//
// marksFromBeforeRules are the marks that the sync engine recorded for
// issuesFromBeforeRules as it stood at commit 6c5c282, the last before
// references and labels. A scan of the same issues must find each unchanged.
func TestIssuesAppliedBeforeRulesStillLookUnchanged(t *testing.T) {
	api := newFakeAPI(t)
	for repository, issues := range issuesFromBeforeRules() {
		api.add(repository, issues...)
	}
	gh := newSource(t, api.URL)

	store, ledger := memory.New(), memory.NewLedger()
	for _, mark := range marksFromBeforeRules {
		if err := ledger.Record(t.Context(), mark); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	var enriched atomic.Int64
	syncer := ingest.NewSyncer(ingest.Config{
		Store:   store,
		Ledger:  ledger,
		Sources: []source.Source{gh, jira{}, mirror{}},
		Enrichers: []ingest.Enricher{ingest.EnricherFunc(func(context.Context, *source.Change) error {
			enriched.Add(1)
			return nil
		})},
		Logger: slog.New(slog.DiscardHandler),
	})

	report, err := syncer.Reconcile(t.Context(), gh)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if want := (ingest.Report{Unchanged: len(marksFromBeforeRules)}); report != want {
		t.Errorf("the scan reported %+v, want %+v: a fingerprint has moved", report, want)
	}
	if enriched.Load() != 0 {
		t.Errorf("%d documents were enriched again, though none had changed", enriched.Load())
	}
}

// Two of those issues are in the graph as the first release left them: the
// document with the summary and embedding it was given, and the edges that
// release made itself. A scan by this version finds them unchanged, as it
// must, and all the same leaves them as this version would have made them:
// with a record of what GitHub said, and with edges of the kinds that the
// rules for GitHub give. No model is asked for anything.
func TestAGraphFromBeforeRulesIsBroughtForwardByAScan(t *testing.T) {
	api := newFakeAPI(t)
	for repository, issues := range issuesFromBeforeRules() {
		api.add(repository, issues...)
	}
	gh := newSource(t, api.URL)
	ctx := t.Context()

	const (
		pullRequest = graph.ID("doc:github:acme/api#43")
		blocked     = graph.ID("doc:github:acme/web#3")
		closes      = "Closes acme/web#9, #10 and #11."
		title       = "Stop refunds timing out (fixes #42)"
	)
	store, ledger := memory.New(), memory.NewLedger()
	for _, mark := range marksFromBeforeRules {
		if err := ledger.Record(ctx, mark); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	embedding := []float32{0.25, 0.5, 1}
	if err := store.PutVertices(ctx, []graph.Vertex{
		&graph.Document{ID: pullRequest, Type: graph.TypePullRequest, Source: "github", Title: title, Summary: "Retries the refund call.", Embedding: embedding},
		&graph.Document{ID: blocked, Type: graph.TypeIssue, Source: "github", Title: "Checkout page", Summary: "The checkout page waits on refunds.", Embedding: embedding},
		&graph.Topic{ID: "topic:bug", Name: "Bug"},
		&graph.Topic{ID: "topic:area/billing", Name: "area/billing", Summary: "Money in and out"},
	}); err != nil {
		t.Fatalf("PutVertices: %v", err)
	}
	// Every mention a links_to edge, whatever its line said, and one edge
	// for the two lines that mention #42. The labels' edges are as the
	// upgrade of a database leaves them.
	if err := store.PutEdges(ctx, []graph.Edge{
		{From: pullRequest, Kind: graph.LinksTo, To: "doc:github:acme/api#42", Summary: title},
		{From: pullRequest, Kind: graph.LinksTo, To: "doc:github:acme/web#9", Summary: closes},
		{From: pullRequest, Kind: graph.LinksTo, To: "doc:github:acme/api#10", Summary: closes},
		{From: pullRequest, Kind: graph.LinksTo, To: "doc:github:acme/api#11", Summary: closes},
		{From: pullRequest, Kind: graph.InTopic, To: "topic:bug"},
		{From: blocked, Kind: graph.LinksTo, To: "doc:github:acme/api#42", Summary: "Blocked by acme/api#42"},
		{From: blocked, Kind: graph.LinksTo, To: "doc:github:acme/web#2", Summary: "duplicate of #2"},
		{From: blocked, Kind: graph.InTopic, To: "topic:area/billing"},
	}); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	var enriched atomic.Int64
	syncer := ingest.NewSyncer(ingest.Config{
		Store:   store,
		Ledger:  ledger,
		Sources: []source.Source{gh, jira{}, mirror{}},
		Enrichers: []ingest.Enricher{ingest.EnricherFunc(func(context.Context, *source.Change) error {
			enriched.Add(1)
			return nil
		})},
		Logger: slog.New(slog.DiscardHandler),
	})
	for pass := range 2 {
		report, err := syncer.Reconcile(ctx, gh)
		if err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		if want := (ingest.Report{Unchanged: len(marksFromBeforeRules)}); report != want {
			t.Fatalf("pass %d reported %+v, want %+v", pass+1, report, want)
		}
	}
	if enriched.Load() != 0 {
		t.Errorf("%d documents were enriched again, though none had changed", enriched.Load())
	}

	edgesOf := func(id graph.ID) []graph.Edge {
		t.Helper()
		page, err := store.Edges(ctx, storage.EdgeQuery{Vertex: id, Direction: graph.Out})
		if err != nil {
			t.Fatalf("Edges: %v", err)
		}
		return page.Edges
	}
	requireEdges := func(id graph.ID, want ...graph.Edge) {
		t.Helper()
		slices.SortFunc(want, graph.Edge.Compare)
		if got := edgesOf(id); !slices.Equal(got, want) {
			t.Errorf("the edges of %s are\n  %+v\nwant\n  %+v", id, got, want)
		}
	}
	// "fixes" and "closes" are GitHub's own closing words, and a list of
	// references after one is closed by it to the last. The line that only
	// mentions #42 is no longer lost behind the one that fixes it.
	requireEdges(pullRequest,
		graph.Edge{From: pullRequest, Kind: mapping.Resolves, To: "doc:github:acme/api#42", Summary: title},
		graph.Edge{From: pullRequest, Kind: graph.LinksTo, To: "doc:github:acme/api#42", Summary: "See also #42 for the history."},
		graph.Edge{From: pullRequest, Kind: mapping.Resolves, To: "doc:github:acme/web#9", Summary: closes},
		graph.Edge{From: pullRequest, Kind: mapping.Resolves, To: "doc:github:acme/api#10", Summary: closes},
		graph.Edge{From: pullRequest, Kind: mapping.Resolves, To: "doc:github:acme/api#11", Summary: closes},
		graph.Edge{From: pullRequest, Kind: graph.InTopic, To: "topic:bug"},
	)
	requireEdges(blocked,
		graph.Edge{From: blocked, Kind: graph.LinksTo, To: "doc:github:acme/api#42", Summary: "Blocked by acme/api#42"},
		graph.Edge{From: blocked, Kind: mapping.Duplicates, To: "doc:github:acme/web#2", Summary: "duplicate of #2"},
		graph.Edge{From: blocked, Kind: graph.InTopic, To: "topic:area/billing"},
	)

	// The two documents are as they were, and each now has its record: what
	// GitHub said, in the words it said it in, under the summary it was
	// given when it was first applied.
	stored, err := store.GetVertices(ctx, []graph.ID{pullRequest, blocked})
	if err != nil {
		t.Fatalf("GetVertices: %v", err)
	}
	said, err := store.Said(ctx, []graph.ID{pullRequest, blocked})
	if err != nil {
		t.Fatalf("Said: %v", err)
	}
	for id, summary := range map[graph.ID]string{pullRequest: "Retries the refund call.", blocked: "The checkout page waits on refunds."} {
		if document := stored[id].(*graph.Document); document.Summary != summary || !slices.Equal(document.Embedding, embedding) {
			t.Errorf("the scan changed %s: %+v", id, document)
		}
		if got := said[id].Document.Summary; got != summary {
			t.Errorf("the summary on record for %s is %q, want the one it was first given", id, got)
		}
	}
	wantReferences := []graph.Reference{
		{Target: "doc:github:acme/api#42", Form: graph.FormMention, Written: "#42", Context: title},
		{Target: "doc:github:acme/api#42", Form: graph.FormMention, Written: "#42", Context: "Fixes #42."},
		{Target: "doc:github:acme/web#9", Form: graph.FormMention, Written: "acme/web#9", Context: closes},
		{Target: "doc:github:acme/api#10", Form: graph.FormMention, Written: "#10", Context: closes},
		{Target: "doc:github:acme/api#11", Form: graph.FormMention, Written: "#11", Context: closes},
		{Target: "doc:github:acme/api#42", Form: graph.FormMention, Written: "#42", Context: "See also #42 for the history."},
	}
	if got := said[pullRequest]; !slices.Equal(got.References, wantReferences) || !slices.Equal(got.Labels, []graph.Label{{Name: "Bug"}}) {
		t.Errorf("the record of %s holds\n  %+v\n  %+v\nwant\n  %+v\nand the label Bug", pullRequest, got.References, got.Labels, wantReferences)
	}

	// The issues that were never in the graph are still not in it: a mark in
	// the ledger is no reason to make a document up.
	if stored, _ := store.GetVertices(ctx, []graph.ID{"doc:github:acme/api#44"}); len(stored) != 0 {
		t.Errorf("a document the graph did not hold was created")
	}
}

// jira stands in for a second source: it recognizes the addresses of the
// tickets that the issues link to, as only another source can.
type jira struct{}

func (jira) Name() string { return "jira" }

func (jira) Resolve(address string) (graph.ID, bool) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Host != "acme.atlassian.net" {
		return "", false
	}
	key, found := strings.CutPrefix(parsed.Path, "/browse/")
	if !found || key == "" {
		return "", false
	}
	return graph.DocumentID("jira", key), true
}

// mirror stands in for a source that knows GitHub's own documents by other
// addresses, as a second GitHub connector, set up for another host, would.
// It is how one document comes to be pointed at twice by different routes:
// by a mention that the connector places itself, and by an address that
// only the sync engine can place.
type mirror struct{}

func (mirror) Name() string { return "mirror" }

func (mirror) Resolve(address string) (graph.ID, bool) {
	path, found := strings.CutPrefix(address, "https://mirror.test/")
	parts := strings.Split(path, "/")
	if !found || len(parts) != 4 || parts[2] != "issues" {
		return "", false
	}
	return graph.DocumentID("github", parts[0]+"/"+parts[1]+"#"+parts[3]), true
}

// issuesFromBeforeRules returns issues and pull requests, by repository, as
// GitHub's API describes them. Between them they take every path by which the
// text of an issue used to become edges: labels in both of the forms GitHub
// uses, a parent, mentions and addresses, a document mentioned twice, a
// document that mentions itself, code fences, a line too long to keep,
// addresses that only another source recognizes, and a document reached
// both by a mention and by such an address.
func issuesFromBeforeRules() map[string][]map[string]any {
	issue := func(repository string, number int, title string, body any, more map[string]any) map[string]any {
		item := map[string]any{
			"number":     number,
			"html_url":   "https://github.com/" + repository + "/issues/" + strconv.Itoa(number),
			"title":      title,
			"body":       body,
			"labels":     []any{},
			"created_at": "2026-02-03T04:05:06Z",
			"updated_at": fmt.Sprintf("2026-02-03T04:05:%02dZ", number%60),
		}
		for field, value := range more {
			item[field] = value
		}
		return item
	}
	pullRequest := map[string]any{"url": "https://api.github.com/repos/acme/api/pulls/1"}
	long := strings.Repeat("The gateway answers slowly under load. ", 8)

	return map[string][]map[string]any{
		"acme/api": {
			sampleIssue(),

			issue("acme/api", 43, "Stop refunds timing out (fixes #42)",
				"Fixes #42.\n\nCloses acme/web#9, #10 and #11.\nSee also #42 for the history.",
				map[string]any{"labels": []any{"Bug"}, "pull_request": pullRequest}),

			issue("acme/api", 44, "An issue nobody described", nil, nil),

			issue("acme/api", 45, "Reproducing the timeout",
				"Reproduce with:\n```\ncurl -H 'x: #99' https://github.com/acme/api/issues/98\n```\n"+
					"~~~\n#97\n~~~\nAfter the fences, #96 counts.", nil),

			issue("acme/api", 46, "An issue that mentions itself",
				"This is #46 itself, also https://github.com/acme/api/issues/46 and acme/api#46.\nAnd #12.", nil),

			issue("acme/api", 47, "A long line", long+"All of it is in #12.\n\n#12\n", nil),

			issue("acme/api", 48, "Addresses of every sort",
				"Docs: https://example.com/a(b)c. And https://github.com/acme/web/pull/5#issuecomment-12, also <https://acme.atlassian.net/browse/PAY-7>;\n"+
					"PAY-7 again: https://acme.atlassian.net/browse/PAY-7?focus=1!\n"+
					"And PAY-8 (https://acme.atlassian.net/browse/PAY-8).", nil),

			issue("acme/api", 49, "Labels of every sort", "Nothing to add.", map[string]any{
				"labels": []any{
					map[string]any{"name": "Good First Issue", "description": "Good for newcomers"},
					map[string]any{"name": "good-first-issue", "description": "The same label, spelt another way"},
					map[string]any{"name": "", "description": "A label with no name"},
					"DOCS",
				},
				"parent_issue_url": nil,
			}),

			issue("acme/api", 50, "Résumé des paiements, voir #12",
				"Résumé des paiements — voir #12\r\nそれは acme/web#3 です\r\n- [ ] #51\r\n- [x] #52\r\n"+
					"abc#12 is no mention; path/#13 neither; &#14; no.", nil),

			issue("acme/api", 51, "A sub-issue of an issue elsewhere", "Part of the epic.", map[string]any{
				"parent_issue_url": "https://api.github.com/repos/acme/web/issues/2",
			}),

			// The address comes first in the text, and the mention after it.
			// The mention's line is the one that was kept, because the
			// connector placed the mention at once and the address waited
			// for the sync engine.
			issue("acme/api", 52, "Pointed at by two routes",
				"Mirrored at https://mirror.test/acme/api/issues/12, with the discussion.\n\nSee #12.", nil),
		},
		"acme/web": {
			issue("acme/web", 3, "Checkout page", "Blocked by acme/api#42\n\nduplicate of #2", map[string]any{
				"labels": []any{map[string]any{"name": "area/billing", "description": "Money in and out"}},
			}),

			issue("acme/web", 9, "Build the checkout page", "Implements #3 #2\n\n#3", map[string]any{"pull_request": pullRequest}),
		},
	}
}

// marksFromBeforeRules is what the ledger held for issuesFromBeforeRules
// after one scan by the sync engine as it stood at commit 6c5c282.
var marksFromBeforeRules = []storage.Mark{
	{ID: "doc:github:acme/api#42", Source: "github", Version: 1782662587000000000, Fingerprint: "c89473d9012746ddc9764daaef1c70906bb237be74fae3b2e8b25e29e12329e0"},
	{ID: "doc:github:acme/api#43", Source: "github", Version: 1770091543000000000, Fingerprint: "071c070b7d5fb1b0c65d95828902e55f5cf5a75662d3d26815bc3b50bce55bc9"},
	{ID: "doc:github:acme/api#44", Source: "github", Version: 1770091544000000000, Fingerprint: "1a483b455dd74d088c93260a1de54e49b10c3f8819fd5f1e565ef9f074beba93"},
	{ID: "doc:github:acme/api#45", Source: "github", Version: 1770091545000000000, Fingerprint: "4d50e028431248520ad8f4d34131d4e283f071f9a14e8b35a4cb70ae703d79a1"},
	{ID: "doc:github:acme/api#46", Source: "github", Version: 1770091546000000000, Fingerprint: "9875cf073e1a6165b002758238ed3533abd05aefbe77fee50f57e9b3a34f8a85"},
	{ID: "doc:github:acme/api#47", Source: "github", Version: 1770091547000000000, Fingerprint: "0639db26e6c1a8be52ea2a4a97422b90216d32cd8bc78c1ca764bb4fbba86406"},
	{ID: "doc:github:acme/api#48", Source: "github", Version: 1770091548000000000, Fingerprint: "c6f84c2fcb119e2b09f86d149859fab6b5252a727c3eae1959da26b5dccd850f"},
	{ID: "doc:github:acme/api#49", Source: "github", Version: 1770091549000000000, Fingerprint: "441809b35d8789b91ad7361cd876c3954b7b08548ff4950a1fe2748b511fe381"},
	{ID: "doc:github:acme/api#50", Source: "github", Version: 1770091550000000000, Fingerprint: "f806af5a58c160bbd76d6ebec952e9232247d7bb867c2bb8190deceeeca11fac"},
	{ID: "doc:github:acme/api#51", Source: "github", Version: 1770091551000000000, Fingerprint: "ebaa9872cce9000fc341364377f151561974e56cf72a5e9bf216382670010263"},
	{ID: "doc:github:acme/api#52", Source: "github", Version: 1770091552000000000, Fingerprint: "1787380cae249112ad936c8d26a1015bbe7071125344e8bbb09a227bc5e30853"},
	{ID: "doc:github:acme/web#3", Source: "github", Version: 1770091503000000000, Fingerprint: "f24697288950dfd8f25e6282e10d6b79250846ab590c195251f4afce44aa6bae"},
	{ID: "doc:github:acme/web#9", Source: "github", Version: 1770091509000000000, Fingerprint: "7018da51b7552050eaaa6f79fa9d03d7a59e68fee13d71eec13c5959c62b4028"},
}

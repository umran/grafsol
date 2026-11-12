package graph_test

import (
	"testing"

	"github.com/umran/grafsol/graph"
)

func TestDocumentID(t *testing.T) {
	id := graph.DocumentID("github", "acme/api#42")
	if id != "doc:github:acme/api#42" {
		t.Fatalf("DocumentID = %q", id)
	}
	if id.Kind() != graph.KindDocument {
		t.Errorf("Kind = %q, want %q", id.Kind(), graph.KindDocument)
	}
	if id.Key() != "github:acme/api#42" {
		t.Errorf("Key = %q", id.Key())
	}
	if id.Source() != "github" {
		t.Errorf("Source = %q, want github", id.Source())
	}
	for _, other := range []graph.ID{"topic:billing", "doc:github", "nope", ""} {
		if source := other.Source(); source != "" {
			t.Errorf("Source(%q) = %q, want empty", other, source)
		}
	}
}

func TestTopicIDNormalizesNames(t *testing.T) {
	cases := map[string]graph.ID{
		"bug":                 "topic:bug",
		"Bug":                 "topic:bug",
		"  bug ":              "topic:bug",
		"Good First Issue":    "topic:good-first-issue",
		"good   first\tissue": "topic:good-first-issue",
		"area/billing":        "topic:area/billing",
	}
	for name, want := range cases {
		if got := graph.TopicID(name); got != want {
			t.Errorf("TopicID(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestIDValidate(t *testing.T) {
	valid := []graph.ID{
		"doc:github:acme/api#42",
		"doc:jira:PAY-7",
		"doc:notion:9f3c2d1e",
		"topic:billing",
		"topic:área/facturación",
	}
	for _, id := range valid {
		if err := id.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", id, err)
		}
	}

	invalid := []graph.ID{
		"",
		"billing",          // no kind
		"tag:billing",      // unknown kind
		"topic:",           // empty key
		"doc:github",       // document without a source-specific key
		"doc::42",          // empty source
		"doc:github:",      // empty key after the source
		"topic:a\x00b",     // control character
		"topic:line\nfeed", // control character
		"topic:\xff\xfe",   // invalid UTF-8
	}
	for _, id := range invalid {
		if err := id.Validate(); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", id)
		}
	}
}

func TestKindOfMalformedID(t *testing.T) {
	for _, id := range []graph.ID{"", "billing", "tag:billing"} {
		if kind := id.Kind(); kind != "" {
			t.Errorf("Kind(%q) = %q, want empty", id, kind)
		}
	}
}

func TestValidateVertex(t *testing.T) {
	var nilDocument *graph.Document
	var nilTopic *graph.Topic

	cases := []struct {
		name   string
		vertex graph.Vertex
		ok     bool
	}{
		{"document", &graph.Document{ID: "doc:github:acme/api#1"}, true},
		{"topic", &graph.Topic{ID: "topic:billing"}, true},
		{"nil interface", nil, false},
		{"nil document", nilDocument, false},
		{"nil topic", nilTopic, false},
		{"document with a topic ID", &graph.Document{ID: "topic:billing"}, false},
		{"topic with a document ID", &graph.Topic{ID: "doc:github:acme/api#1"}, false},
		{"document with a malformed ID", &graph.Document{ID: "nope"}, false},
	}
	for _, c := range cases {
		err := graph.ValidateVertex(c.vertex)
		if c.ok && err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: expected an error", c.name)
		}
	}
}

func TestCloneSharesNoMemory(t *testing.T) {
	document := &graph.Document{ID: "doc:github:acme/api#1", Embedding: []float32{1, 2, 3}}
	clone := graph.CloneVertex(document).(*graph.Document)
	clone.Embedding[0] = 99
	clone.Title = "changed"
	if document.Embedding[0] != 1 || document.Title != "" {
		t.Errorf("changing the clone changed the original: %+v", document)
	}

	topic := &graph.Topic{ID: "topic:billing", Embedding: []float32{1, 2, 3}}
	topicClone := graph.CloneVertex(topic).(*graph.Topic)
	topicClone.Embedding[0] = 99
	if topic.Embedding[0] != 1 {
		t.Errorf("changing the clone changed the original: %+v", topic)
	}
}

func TestEdgeKindValidate(t *testing.T) {
	for _, kind := range []graph.EdgeKind{graph.LinksTo, graph.ChildOf, graph.InTopic, graph.TaggedWith, "blocks", "relates_to_2"} {
		if err := kind.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", kind, err)
		}
	}
	for _, kind := range []graph.EdgeKind{"", "LinksTo", "links to", "links-to", "links#to"} {
		if err := kind.Validate(); err == nil {
			t.Errorf("Validate(%q) = nil, want an error", kind)
		}
	}
}

func TestEdgeValidate(t *testing.T) {
	good := graph.Edge{From: "doc:github:acme/api#1", Kind: graph.LinksTo, To: "doc:jira:PAY-7"}
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate(%+v) = %v", good, err)
	}

	bad := []graph.Edge{
		{From: "", Kind: graph.LinksTo, To: "doc:jira:PAY-7"},
		{From: "doc:github:acme/api#1", Kind: "", To: "doc:jira:PAY-7"},
		{From: "doc:github:acme/api#1", Kind: graph.LinksTo, To: "PAY-7"},
	}
	for _, edge := range bad {
		if err := edge.Validate(); err == nil {
			t.Errorf("Validate(%+v) = nil, want an error", edge)
		}
	}
}

func TestEdgeEnds(t *testing.T) {
	edge := graph.Edge{From: "doc:a:1", Kind: graph.ChildOf, To: "doc:a:2"}

	if edge.Near(graph.Out) != "doc:a:1" || edge.Far(graph.Out) != "doc:a:2" {
		t.Errorf("Out: near %q, far %q", edge.Near(graph.Out), edge.Far(graph.Out))
	}
	if edge.Near(graph.In) != "doc:a:2" || edge.Far(graph.In) != "doc:a:1" {
		t.Errorf("In: near %q, far %q", edge.Near(graph.In), edge.Far(graph.In))
	}
}

func TestEdgeTripleAndCompare(t *testing.T) {
	a := graph.Edge{From: "doc:a:1", Kind: graph.LinksTo, To: "doc:a:2", Summary: "see also"}
	b := graph.Edge{From: "doc:a:1", Kind: graph.LinksTo, To: "doc:a:2", Summary: "fixes"}

	if a.Triple() != b.Triple() {
		t.Errorf("triples differ although only the summaries do")
	}
	if a.Triple().Summary != "" {
		t.Errorf("Triple kept the summary")
	}
	if a.Compare(b) != 0 {
		t.Errorf("Compare should ignore the summary")
	}

	// Ordered by From, then Kind, then To.
	ordered := []graph.Edge{
		{From: "doc:a:1", Kind: graph.ChildOf, To: "doc:a:9"},
		{From: "doc:a:1", Kind: graph.LinksTo, To: "doc:a:2"},
		{From: "doc:a:1", Kind: graph.LinksTo, To: "doc:a:3"},
		{From: "doc:a:2", Kind: graph.ChildOf, To: "doc:a:1"},
	}
	for i := 0; i+1 < len(ordered); i++ {
		if ordered[i].Compare(ordered[i+1]) >= 0 {
			t.Errorf("%+v should sort before %+v", ordered[i], ordered[i+1])
		}
		if ordered[i+1].Compare(ordered[i]) <= 0 {
			t.Errorf("%+v should sort after %+v", ordered[i+1], ordered[i])
		}
	}
}

func TestDirection(t *testing.T) {
	if graph.Out.String() != "out" || graph.In.String() != "in" {
		t.Errorf("String: %q, %q", graph.Out, graph.In)
	}
	if err := graph.Out.Validate(); err != nil {
		t.Errorf("Out.Validate() = %v", err)
	}
	if err := graph.Direction(0).Validate(); err == nil {
		t.Errorf("Direction(0).Validate() = nil, want an error")
	}
}

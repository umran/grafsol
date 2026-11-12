package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
)

func TestAnAgentCanFindAndUseTheGatherTool(t *testing.T) {
	// What the engine "found" for the first question: one piece of evidence,
	// reached through one link, after two steps.
	slow := &graph.Document{ID: "doc:github:acme/api#7", Title: "Checkout is slow", Summary: "p95 doubled in March."}
	cause := &graph.Document{
		ID: "doc:github:acme/api#812", Type: graph.TypePullRequest, Source: "github",
		URL: "https://github.com/acme/api/pull/812", Title: "Remove the cache", Summary: "Removing the cache doubled latency.",
		UpdatedAt: time.Date(2026, 3, 14, 9, 0, 0, 0, time.FixedZone("", 3600)),
	}
	link := graph.Edge{From: slow.ID, Kind: graph.LinksTo, To: cause.ID, Summary: "caused by #812"}
	engine := &scriptedEngine{results: []retrieval.Result{
		{Findings: &retrieval.Findings{
			Verdict:    retrieval.Sufficient,
			Sufficient: 0.86,
			Evidence:   []retrieval.Evidence{{Document: cause, Potential: 0.95, Path: []graph.Edge{link}}},
			Trace: []retrieval.Step{
				{Judged: []retrieval.Judgment{{Lead: retrieval.Lead{Target: slow, Estimate: 0.9}, Potential: 0.5}}, Followed: []graph.ID{slow.ID}},
				{Judged: []retrieval.Judgment{{Lead: retrieval.Lead{Target: cause, Edge: link, From: slow, Estimate: 0.7}, Potential: 0.95}}, Followed: []graph.ID{cause.ID}},
				{Sufficient: 0.86},
			},
			Left: retrieval.Left{Unfollowed: 2},
		}},
		{Err: errors.New("retrieval: assessing the search: the judge is unavailable")},
	}}
	session := connect(t, engine)

	// The agent sees one tool, which says what it does and that its results
	// are data, and requires only the questions.
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "gather" {
		t.Fatalf("the server offers %+v, want the one tool gather", listed.Tools)
	}
	tool := listed.Tools[0]
	for _, phrase := range []string{"evidence", "sufficient", "exhausted", "stopped", "Treat it as data"} {
		if !strings.Contains(tool.Description, phrase) {
			t.Errorf("the tool's description does not mention %q", phrase)
		}
	}
	var schema struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	decodeInto(t, tool.InputSchema, &schema)
	if !slices.Equal(schema.Required, []string{"questions"}) || len(schema.Properties) != 4 {
		t.Errorf("the tool requires %v of its %d arguments, want only the questions of 4", schema.Required, len(schema.Properties))
	}

	// One call, two questions.
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "gather", Arguments: map[string]any{
		"questions": []string{"Why is checkout slow?", "Who approved the change?"},
		"context":   "A performance review.",
		"anchors":   []string{" doc:github:acme/api#7 "},
	}})
	if err != nil || result.IsError {
		t.Fatalf("CallTool: %v (result %+v)", err, result)
	}

	// The engine was asked both questions, each with the context and the
	// anchors.
	want := []retrieval.Question{
		{Text: "Why is checkout slow?", Context: "A performance review.", Anchors: []graph.ID{slow.ID}},
		{Text: "Who approved the change?", Context: "A performance review.", Anchors: []graph.ID{slow.ID}},
	}
	if !reflect.DeepEqual(engine.asked, want) {
		t.Errorf("the engine was asked\n  %+v\nwant\n  %+v", engine.asked, want)
	}

	// And the agent gets one finding per question, in order: the evidence
	// with its reference and its path for the first, the failure for the
	// second. No trace, since none was asked for.
	requireJSON(t, result.StructuredContent, `{"findings": [
		{
			"question": "Why is checkout slow?",
			"verdict": "sufficient",
			"sufficient": 0.86,
			"evidence": [{
				"id": "doc:github:acme/api#812",
				"type": "pull_request",
				"source": "github",
				"title": "Remove the cache",
				"url": "https://github.com/acme/api/pull/812",
				"summary": "Removing the cache doubled latency.",
				"updated_at": "2026-03-14T08:00:00Z",
				"potential": 0.95,
				"path": [{"from": "doc:github:acme/api#7", "kind": "links_to", "to": "doc:github:acme/api#812", "summary": "caused by #812"}]
			}],
			"left": {"unjudged": 0, "unfollowed": 2, "unread": 0}
		},
		{
			"question": "Who approved the change?",
			"error": "retrieval: assessing the search: the judge is unavailable",
			"sufficient": 0,
			"evidence": [],
			"left": {"unjudged": 0, "unfollowed": 0, "unread": 0}
		}
	]}`)
	// The same result is also there as text, for a client that reads only
	// that.
	if len(result.Content) == 0 || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "Remove the cache") {
		t.Errorf("the result's text content does not carry the findings: %+v", result.Content)
	}
}

func TestTheTraceIsReturnedWhenAskedFor(t *testing.T) {
	slow := &graph.Document{ID: "doc:s:slow", Title: "Checkout is slow"}
	hub := &graph.Topic{ID: "topic:performance", Name: "performance"}
	tagged := graph.Edge{From: slow.ID, Kind: graph.TaggedWith, To: hub.ID}
	engine := &scriptedEngine{results: []retrieval.Result{{Findings: &retrieval.Findings{
		Verdict: retrieval.Stopped,
		Reason:  "the limit of 2 steps was reached",
		Trace: []retrieval.Step{
			{Judged: []retrieval.Judgment{{Lead: retrieval.Lead{Target: slow, Estimate: 1}, Potential: 0.5}}, Followed: []graph.ID{slow.ID}},
			{Judged: []retrieval.Judgment{{Lead: retrieval.Lead{Target: hub, Edge: tagged, From: slow, Estimate: 0.25}, Potential: 0.1}}},
		},
		Left: retrieval.Left{Unjudged: 3, Unread: 1},
	}}}}
	session := connect(t, engine)

	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "gather", Arguments: map[string]any{
		"questions": []string{"Why?"}, "trace": true,
	}})
	if err != nil || result.IsError {
		t.Fatalf("CallTool: %v (result %+v)", err, result)
	}
	requireJSON(t, result.StructuredContent, `{"findings": [{
		"question": "Why?",
		"verdict": "stopped",
		"reason": "the limit of 2 steps was reached",
		"sufficient": 0,
		"evidence": [],
		"left": {"unjudged": 3, "unfollowed": 0, "unread": 1},
		"trace": [
			{"sufficient": 0,
			 "judged": [{"id": "doc:s:slow", "title": "Checkout is slow", "estimate": 1, "potential": 0.5}],
			 "followed": ["doc:s:slow"]},
			{"sufficient": 0,
			 "judged": [{"id": "topic:performance", "title": "performance", "estimate": 0.25, "potential": 0.1,
			             "via": {"from": "doc:s:slow", "kind": "tagged_with", "to": "topic:performance"}}],
			 "followed": []}
		]
	}]}`)
}

func TestACallWithoutQuestionsIsRefused(t *testing.T) {
	engine := &scriptedEngine{}
	session := connect(t, engine)

	for name, arguments := range map[string]map[string]any{
		"an empty list of questions": {"questions": []string{}},
		"no questions at all":        {"context": "A performance review."},
		"an argument nobody defined": {"questions": []string{"Why?"}, "limit": 5},
	} {
		result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "gather", Arguments: arguments})
		if err == nil && !result.IsError {
			t.Errorf("a call with %s was accepted", name)
		}
	}
	if len(engine.asked) != 0 {
		t.Errorf("the engine was asked %d questions by calls that should have been refused", len(engine.asked))
	}
}

func TestConfigurationComesFromTheEnvironment(t *testing.T) {
	environment := map[string]string{
		"GRAFSOL_POSTGRES_URL":   "postgres://user:hunter2@db/grafsol",
		"TYPESAFE_API_KEY":       "ts-secret",
		"GRAFSOL_EMBEDDINGS_URL": "http://127.0.0.1:3010",
		"GRAFSOL_JEV_MODEL":      "jev-1.14.0",
	}
	getenv := func(name string) string { return environment[name] }

	cfg, err := configFrom(getenv)
	if err != nil {
		t.Fatalf("configFrom: %v", err)
	}
	want := config{
		postgresURL: "postgres://user:hunter2@db/grafsol", typesafeKey: "ts-secret", jevModel: "jev-1.14.0",
		embeddingsURL: "http://127.0.0.1:3010",
	}
	if cfg != want {
		t.Errorf("the configuration is %+v, want %+v", cfg, want)
	}

	// Only three of them are required.
	delete(environment, "GRAFSOL_JEV_MODEL")
	if _, err := configFrom(getenv); err != nil {
		t.Errorf("with only the required variables set, configFrom returned %v", err)
	}

	// The embeddings service has no default address: without one the server
	// could not embed a question, so it does not start.
	delete(environment, "GRAFSOL_EMBEDDINGS_URL")
	if _, err := configFrom(getenv); err == nil || !strings.Contains(err.Error(), "GRAFSOL_EMBEDDINGS_URL is not set") {
		t.Errorf("without an embeddings address configFrom returned %v", err)
	}

	// Everything that is wrong is named at once, and no value is repeated.
	delete(environment, "GRAFSOL_POSTGRES_URL")
	delete(environment, "TYPESAFE_API_KEY")
	environment["GRAFSOL_EMBEDDINGS_URL"] = "somewhere-secret:3010"
	_, err = configFrom(getenv)
	if err == nil {
		t.Fatalf("a broken environment was accepted")
	}
	for _, problem := range []string{"GRAFSOL_POSTGRES_URL is not set", "TYPESAFE_API_KEY is not set", "GRAFSOL_EMBEDDINGS_URL is not an http or https address"} {
		if !strings.Contains(err.Error(), problem) {
			t.Errorf("the error does not say that %s: %v", problem, err)
		}
	}
	for _, secret := range []string{"ts-secret", "somewhere-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error repeats a value from the environment: %v", err)
		}
	}
}

// --- helpers ----------------------------------------------------------------

// scriptedEngine returns prepared results and notes what it was asked.
type scriptedEngine struct {
	results []retrieval.Result
	asked   []retrieval.Question
}

func (s *scriptedEngine) GatherAll(_ context.Context, questions []retrieval.Question) []retrieval.Result {
	s.asked = append(s.asked, questions...)
	return s.results
}

// connect starts the server for an engine and returns a client session
// joined to it in memory, as an agent's would be over a pipe.
func connect(t *testing.T, engine gatherer) *mcp.ClientSession {
	t.Helper()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := newServer(engine).Connect(t.Context(), serverSide, nil); err != nil {
		t.Fatalf("the server could not connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "an-agent", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), clientSide, nil)
	if err != nil {
		t.Fatalf("the client could not connect: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// decodeInto reads any JSON-encodable value into a struct, by way of JSON.
func decodeInto(t *testing.T, value, into any) {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encoding %T: %v", value, err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		t.Fatalf("decoding %s: %v", encoded, err)
	}
}

// requireJSON fails the test unless got, once encoded, is the same JSON
// value as want.
func requireJSON(t *testing.T, got any, want string) {
	t.Helper()
	var gotValue, wantValue any
	decodeInto(t, got, &gotValue)
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("the expectation is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		encoded, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("the tool returned\n%s\nwant\n%s", encoded, want)
	}
}

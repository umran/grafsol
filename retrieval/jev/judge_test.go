package jev

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/typesafe"
)

var (
	slow = &graph.Document{ID: "doc:s:slow", Type: graph.TypeIssue, URL: "https://github.com/acme/shop/issues/7",
		Title: "Checkout is slow", Summary: "p95 doubled in March."}
	// A document with nothing but a title: no type, no URL and no summary.
	bare  = &graph.Document{ID: "doc:s:bare", Title: "Untitled notes"}
	cause = &graph.Document{ID: "doc:s:cause", Type: graph.TypePullRequest, URL: "https://github.com/acme/shop/pull/9",
		Title: "The cache was removed", Summary: "Removing the cache doubled latency."}
	performance = &graph.Topic{ID: "topic:performance", Name: "performance", Summary: "Speed and latency."}
)

func TestAssessAsksJevExactlyThis(t *testing.T) {
	api := newFakeJev(t)
	api.answer = func(name string) string {
		return map[string]string{
			"sufficient": `{"type": "noul", "noul": 0.82}`,
			"lead_0000":  `{"type": "score", "score": 2.0, "confidence": 0.9, "legend": {}, "probabilities": {"0": 0, "1": 0, "2": 1}}`,
			"lead_0001":  `{"type": "score", "score": 0.9, "confidence": 0.5, "legend": {}, "probabilities": {"0": 0.3, "1": 0.5, "2": 0.2}}`,
			"lead_0002":  `{"type": "score", "score": 0.0, "confidence": 0.9, "legend": {}, "probabilities": {"0": 1, "1": 0, "2": 0}}`,
		}[name]
	}
	judge := api.judge("jev-1.14.0")

	assessment, err := judge.Assess(t.Context(),
		retrieval.Question{Text: "Why is checkout slow?", Context: "A performance review."},
		[]retrieval.Evidence{{Document: cause, Potential: 1}},
		[]retrieval.Lead{
			{Target: slow, Estimate: 0.9}, // a seed
			{Target: performance, From: slow, Estimate: 0.6,
				Edge: graph.Edge{From: slow.ID, Kind: graph.TaggedWith, To: performance.ID, Summary: "labelled performance"}},
			{Target: bare, From: slow, Estimate: 0.2,
				Edge: graph.Edge{From: bare.ID, Kind: graph.LinksTo, To: slow.ID}},
		})
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}

	// One request. The user's question is the state; the evidence is inside
	// the sufficiency question; each lead is inside its own. A document is
	// shown with its type and its URL, which say where it is from, when the
	// graph has them; what the graph does not have is left out, not sent
	// empty.
	requireJSON(t, api.only().raw, `{
		"model": "jev-1.14.0",
		"state": {"user_question": "Why is checkout slow?", "context": "A performance review."},
		"questions": {
			"sufficient": {
				"type": "noul",
				"instructions": {
					"question": "Does `+"`evidence`"+` state every fact needed to answer `+"`user_question`"+`?",
					"evidence": [{"kind": "document", "type": "pull_request", "title": "The cache was removed",
						"url": "https://github.com/acme/shop/pull/9", "summary": "Removing the cache doubled latency."}]
				},
				"criteria": {
					"true": "Each step from `+"`user_question`"+` to its answer is stated in `+"`evidence`"+`.",
					"false": "At least one step from `+"`user_question`"+` to its answer is not stated in `+"`evidence`"+`."
				}
			},
			"lead_0000": {
				"type": "score",
				"instructions": {
					"question": "How much does `+"`candidate`"+` help to answer `+"`user_question`"+`?",
					"candidate": {"kind": "document", "type": "issue", "title": "Checkout is slow",
						"url": "https://github.com/acme/shop/issues/7", "summary": "p95 doubled in March."}
				},
				"criteria": [
					"`+"`candidate`"+` has nothing to do with `+"`user_question`"+`.",
					"`+"`candidate`"+` is about the subject of `+"`user_question`"+`, but states nothing that answers it.",
					"`+"`candidate`"+` states a fact that answers `+"`user_question`"+`, in whole or in part."
				]
			},
			"lead_0001": {
				"type": "score",
				"instructions": {
					"question": "How much does `+"`candidate`"+` help to answer `+"`user_question`"+`?",
					"candidate": {"kind": "topic", "name": "performance", "summary": "Speed and latency."},
					"found": {"how": "the document \"Checkout is slow\" is tagged with this topic", "note": "labelled performance"}
				},
				"criteria": [
					"`+"`candidate`"+` has nothing to do with `+"`user_question`"+`.",
					"`+"`candidate`"+` is about the subject of `+"`user_question`"+`, but states nothing that answers it.",
					"`+"`candidate`"+` states a fact that answers `+"`user_question`"+`, in whole or in part."
				]
			},
			"lead_0002": {
				"type": "score",
				"instructions": {
					"question": "How much does `+"`candidate`"+` help to answer `+"`user_question`"+`?",
					"candidate": {"kind": "document", "title": "Untitled notes"},
					"found": {"how": "it links to the document \"Checkout is slow\""}
				},
				"criteria": [
					"`+"`candidate`"+` has nothing to do with `+"`user_question`"+`.",
					"`+"`candidate`"+` is about the subject of `+"`user_question`"+`, but states nothing that answers it.",
					"`+"`candidate`"+` states a fact that answers `+"`user_question`"+`, in whole or in part."
				]
			}
		}
	}`)

	// The sufficiency is the Noul as it came. A lead's rating is its score
	// as a share of the top level: 2 of 2, 0.9 of 2, 0 of 2.
	want := retrieval.Assessment{Sufficient: 0.82, Potential: []float64{1, 0.45, 0}, Tokens: 1000}
	if !reflect.DeepEqual(assessment, want) {
		t.Errorf("the assessment is %+v, want %+v", assessment, want)
	}
}

func TestAssessAsksOnlyWhatThereIsToAsk(t *testing.T) {
	api := newFakeJev(t)
	judge := api.judge("")
	question := retrieval.Question{Text: "Why?"}

	// No evidence: nothing to be sufficient, so the question is not asked.
	assessment, err := judge.Assess(t.Context(), question, nil, []retrieval.Lead{{Target: slow}})
	if err != nil {
		t.Fatalf("Assess with no evidence: %v", err)
	}
	if got := api.only().names(); !slices.Equal(got, []string{"lead_0000"}) || assessment.Sufficient != 0 {
		t.Errorf("with no evidence Jev was asked %v and the sufficiency came back %v", got, assessment.Sufficient)
	}
	// No context was given, so none is sent; no model was named, so the
	// alias for the newest one is.
	requireJSON(t, api.only().state, `{"user_question": "Why?"}`)
	if api.only().model != typesafe.DefaultModel {
		t.Errorf("the model asked was %q", api.only().model)
	}

	// No leads: only the evidence to weigh.
	api.reset()
	assessment, err = judge.Assess(t.Context(), question, []retrieval.Evidence{{Document: cause}}, nil)
	if err != nil {
		t.Fatalf("Assess with no leads: %v", err)
	}
	if got := api.only().names(); !slices.Equal(got, []string{"sufficient"}) || len(assessment.Potential) != 0 {
		t.Errorf("with no leads Jev was asked %v and %d ratings came back", got, len(assessment.Potential))
	}

	// Neither: nothing to ask at all.
	api.reset()
	if _, err := judge.Assess(t.Context(), question, nil, nil); err != nil || len(api.sent()) != 0 {
		t.Errorf("with nothing to judge, Assess made %d requests and returned %v", len(api.sent()), err)
	}
}

func TestRelationsReadAsSentences(t *testing.T) {
	cases := []struct {
		edge graph.Edge // between slow and the target
		to   graph.Vertex
		want string
	}{
		{graph.Edge{From: slow.ID, Kind: graph.LinksTo, To: cause.ID}, cause, `the document "Checkout is slow" links to it`},
		{graph.Edge{From: cause.ID, Kind: graph.LinksTo, To: slow.ID}, cause, `it links to the document "Checkout is slow"`},
		{graph.Edge{From: slow.ID, Kind: graph.ChildOf, To: cause.ID}, cause, `the document "Checkout is slow" is a part of it`},
		{graph.Edge{From: cause.ID, Kind: graph.ChildOf, To: slow.ID}, cause, `it is a part of the document "Checkout is slow"`},
		{graph.Edge{From: slow.ID, Kind: graph.InTopic, To: performance.ID}, performance, `the document "Checkout is slow" belongs to this topic`},
		{graph.Edge{From: slow.ID, Kind: graph.TaggedWith, To: performance.ID}, performance, `the document "Checkout is slow" is tagged with this topic`},
		{graph.Edge{From: slow.ID, Kind: "reviewed_by", To: cause.ID}, cause, `the document "Checkout is slow" has a "reviewed_by" edge to it`},
		{graph.Edge{From: cause.ID, Kind: "reviewed_by", To: slow.ID}, cause, `it has a "reviewed_by" edge to the document "Checkout is slow"`},
	}
	for _, c := range cases {
		if got := relation(retrieval.Lead{Target: c.to, From: slow, Edge: c.edge}); got != c.want {
			t.Errorf("the edge %+v reads as %q, want %q", c.edge, got, c.want)
		}
	}
	// And seen from a topic, towards one of its documents.
	fromTopic := retrieval.Lead{Target: cause, From: performance, Edge: graph.Edge{From: cause.ID, Kind: graph.InTopic, To: performance.ID}}
	if got, want := relation(fromTopic), `it belongs to the topic "performance"`; got != want {
		t.Errorf("a document reached from its topic reads as %q, want %q", got, want)
	}
}

func TestLeadsThatDoNotFitOneRequestAreSpreadOverSeveral(t *testing.T) {
	// Room for the state and about three leads per request.
	lower(t, &requestBudget, 700)
	api := newFakeJev(t)
	api.answer = func(name string) string {
		if name == "sufficient" {
			return `{"type": "noul", "noul": 0.5}`
		}
		// Each lead scores by its number, so that the order can be checked.
		var number int
		fmt.Sscanf(name, "lead_%d", &number)
		return fmt.Sprintf(`{"type": "score", "score": %v}`, float64(number%3))
	}

	leads := make([]retrieval.Lead, 10)
	for i := range leads {
		leads[i] = retrieval.Lead{Target: &graph.Document{
			ID: graph.DocumentID("s", fmt.Sprint(i)), Title: fmt.Sprintf("Document %d", i), Summary: strings.Repeat("word ", 40),
		}}
	}
	assessment, err := api.judge("").Assess(t.Context(), retrieval.Question{Text: "Why?"}, []retrieval.Evidence{{Document: cause}}, leads)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}

	sent := api.sent()
	if len(sent) < 3 {
		t.Fatalf("ten leads went out in %d requests; the budget should have forced more", len(sent))
	}
	asked := make(map[string]int)
	for _, request := range sent {
		// Every request carries the state and stays within the budget.
		requireJSON(t, request.state, `{"user_question": "Why?"}`)
		if size := tokens(json.RawMessage(request.raw)); size > 700 {
			t.Errorf("a request of about %d tokens was sent; the budget is 700", size)
		}
		for _, name := range request.names() {
			asked[name]++
		}
	}
	// Each question was asked exactly once, the sufficiency question
	// included.
	if len(asked) != 11 || asked["sufficient"] != 1 {
		t.Errorf("the questions asked, with how often, were %v", asked)
	}
	for name, times := range asked {
		if times != 1 {
			t.Errorf("%s was asked %d times", name, times)
		}
	}
	// The ratings come back in the order of the leads, and the cost is the
	// sum over the requests.
	for i, potential := range assessment.Potential {
		if want := float64(i%3) / 2; potential != want {
			t.Errorf("lead %d is rated %v, want %v", i, potential, want)
		}
	}
	if assessment.Tokens != 1000*len(sent) || assessment.Sufficient != 0.5 {
		t.Errorf("the assessment reports %d tokens over %d requests, and a sufficiency of %v", assessment.Tokens, len(sent), assessment.Sufficient)
	}
}

func TestARequestJevFindsTooLongIsSplitAndSentAgain(t *testing.T) {
	api := newFakeJev(t)
	api.tooLongOver = 3 // Jev refuses any request with more than three questions

	leads := make([]retrieval.Lead, 9)
	for i := range leads {
		leads[i] = retrieval.Lead{Target: &graph.Document{ID: graph.DocumentID("s", fmt.Sprint(i)), Title: fmt.Sprintf("Document %d", i)}}
	}
	assessment, err := api.judge("").Assess(t.Context(), retrieval.Question{Text: "Why?"}, []retrieval.Evidence{{Document: cause}}, leads)
	if err != nil {
		t.Fatalf("Assess: %v", err)
	}
	if len(assessment.Potential) != 9 {
		t.Fatalf("%d leads were rated, want 9", len(assessment.Potential))
	}

	// Every question was answered exactly once, by a request small enough.
	answered := make(map[string]int)
	for _, request := range api.sent() {
		if len(request.names()) <= 3 {
			for _, name := range request.names() {
				answered[name]++
			}
		}
	}
	if len(answered) != 10 {
		t.Errorf("the questions answered were %v, want all ten", answered)
	}
	for name, times := range answered {
		if times != 1 {
			t.Errorf("%s was answered %d times", name, times)
		}
	}
}

func TestEvidenceTooLargeToReadIsReportedAsSuch(t *testing.T) {
	question := retrieval.Question{Text: "Why?"}
	evidence := []retrieval.Evidence{{Document: cause}, {Document: slow}}

	// By the judge's own reckoning: no request is made.
	t.Run("by estimate", func(t *testing.T) {
		lower(t, &questionBudget, 50)
		api := newFakeJev(t)
		_, err := api.judge("").Assess(t.Context(), question, evidence, []retrieval.Lead{{Target: bare}})
		if !errors.Is(err, retrieval.ErrTooLarge) || len(api.sent()) != 0 {
			t.Errorf("Assess returned %v after %d requests, want ErrTooLarge and none", err, len(api.sent()))
		}
	})

	// By Jev's: the sufficiency question, alone in a request, is still
	// refused.
	t.Run("by Jev", func(t *testing.T) {
		api := newFakeJev(t)
		api.tooLongOver = 0
		_, err := api.judge("").Assess(t.Context(), question, evidence, nil)
		if !errors.Is(err, retrieval.ErrTooLarge) {
			t.Errorf("Assess returned %v, want ErrTooLarge", err)
		}
	})

	// A lead that is too large is a different matter: the evidence can
	// still be read, so the search should not be told otherwise.
	t.Run("a lead instead", func(t *testing.T) {
		lower(t, &questionBudget, 100)
		api := newFakeJev(t)
		huge := &graph.Document{ID: "doc:s:huge", Title: "Huge", Summary: strings.Repeat("word ", 200)}
		_, err := api.judge("").Assess(t.Context(), question, nil, []retrieval.Lead{{Target: huge}})
		if err == nil || errors.Is(err, retrieval.ErrTooLarge) || !strings.Contains(err.Error(), "doc:s:huge") {
			t.Errorf("Assess returned %v, want an error naming the lead", err)
		}
	})
}

func TestAnswersJevDidNotGiveAreErrors(t *testing.T) {
	cases := map[string]func(name string) string{
		"no answer to a lead": func(name string) string {
			if name == "lead_0000" {
				return ""
			}
			return `{"type": "noul", "noul": 0.5}`
		},
		"the wrong kind of answer to a lead": func(string) string { return `{"type": "noul", "noul": 0.5}` },
		"the wrong kind of answer on sufficiency": func(name string) string {
			return `{"type": "score", "score": 1}`
		},
	}
	for name, answer := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeJev(t)
			api.answer = answer
			_, err := api.judge("").Assess(t.Context(), retrieval.Question{Text: "Why?"},
				[]retrieval.Evidence{{Document: cause}}, []retrieval.Lead{{Target: slow}})
			if err == nil {
				t.Errorf("a reply with %s was accepted", name)
			}
		})
	}
}

func TestAssessPassesOnJevsFailuresAndKeepsRatingsInRange(t *testing.T) {
	// An error from the API reaches the caller as what it is.
	api := newFakeJev(t)
	api.status = http.StatusUnauthorized
	_, err := api.judge("").Assess(t.Context(), retrieval.Question{Text: "Why?"}, nil, []retrieval.Lead{{Target: slow}})
	var refused *typesafe.Error
	if !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
		t.Errorf("Assess returned %v, want the API's 401", err)
	}

	// A score a shade above the top level is the top rating, not more.
	api = newFakeJev(t)
	api.answer = func(string) string { return `{"type": "score", "score": 2.0000001}` }
	assessment, err := api.judge("").Assess(t.Context(), retrieval.Question{Text: "Why?"}, nil, []retrieval.Lead{{Target: slow}})
	if err != nil || assessment.Potential[0] != 1 {
		t.Errorf("a score just over the top came back as %v, %v", assessment.Potential, err)
	}
}

// --- helpers ----------------------------------------------------------------

// fakeJev stands in for the System One API. It answers every question with
// what its answer function returns for the question's name, and records the
// requests it receives.
type fakeJev struct {
	t      *testing.T
	server *httptest.Server

	// answer returns the JSON of the answer to a question, or "" to leave
	// the question unanswered. Unset means a middling answer of the right
	// kind for the name.
	answer func(name string) string

	// tooLongOver makes the API refuse, as too long, any request with more
	// than this many questions. Negative means it refuses nothing.
	tooLongOver int

	// status, if set, is returned for every request instead of an answer.
	status int

	mu       sync.Mutex
	requests []jevRequest
}

type jevRequest struct {
	raw       []byte
	model     string
	state     []byte
	questions map[string]json.RawMessage
}

func (r jevRequest) names() []string {
	names := make([]string, 0, len(r.questions))
	for name := range r.questions {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func newFakeJev(t *testing.T) *fakeJev {
	t.Helper()
	api := &fakeJev{t: t, tooLongOver: -1}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Model     string                     `json:"model"`
			State     json.RawMessage            `json:"state"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the request body is not JSON: %v", err)
		}
		api.mu.Lock()
		api.requests = append(api.requests, jevRequest{raw: raw, model: body.Model, state: body.State, questions: body.Questions})
		api.mu.Unlock()

		switch {
		case api.status != 0:
			w.WriteHeader(api.status)
			io.WriteString(w, `{"detail": "refused"}`)
			return
		case api.tooLongOver >= 0 && len(body.Questions) > api.tooLongOver:
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"detail": "max_tokens_exceeded: the request is longer than the model's context"}`)
			return
		}

		answers := make(map[string]json.RawMessage)
		for name := range body.Questions {
			answer := `{"type": "score", "score": 1}`
			if name == sufficientName {
				answer = `{"type": "noul", "noul": 0.5}`
			}
			if api.answer != nil {
				answer = api.answer(name)
			}
			if answer != "" {
				answers[name] = json.RawMessage(answer)
			}
		}
		reply, _ := json.Marshal(map[string]any{
			"model":   "jev-1.14.0",
			"answers": answers,
			"usage":   map[string]int{"input_tokens": 1000, "output_tokens": 10},
		})
		w.Write(reply)
	}))
	t.Cleanup(api.server.Close)
	return api
}

// judge returns a Judge that asks the fake API, without retries.
func (a *fakeJev) judge(model string) *Judge {
	client := typesafe.New(typesafe.Config{APIKey: "test-key", BaseURL: a.server.URL, MaxRetries: -1})
	return New(Config{Client: client, Model: model})
}

func (a *fakeJev) sent() []jevRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

func (a *fakeJev) only() jevRequest {
	a.t.Helper()
	sent := a.sent()
	if len(sent) != 1 {
		a.t.Fatalf("Jev received %d requests, want 1", len(sent))
	}
	return sent[0]
}

func (a *fakeJev) reset() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requests = nil
}

// lower sets one of the package's budgets for the length of a test.
func lower(t *testing.T, budget *int, value int) {
	t.Helper()
	previous := *budget
	*budget = value
	t.Cleanup(func() { *budget = previous })
}

// requireJSON fails the test unless got and want are the same JSON value,
// whatever their spacing and the order of their keys.
func requireJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("what was sent is not JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("the expectation is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("Jev was sent\n%s\nwant\n%s", got, want)
	}
}

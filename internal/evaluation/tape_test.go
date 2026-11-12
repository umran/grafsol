package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/umran/grafsol/typesafe"
)

// fakeJev stands in for the System One API. Every question is answered with
// a number that depends on how many requests there have been, so that two
// askings of one question get different answers, as they do from the real
// thing. It records what it was asked.
type fakeJev struct {
	t      *testing.T
	server *httptest.Server

	// answer, if set, decides the answer to a question from what it asks,
	// so that the same question is always answered the same way and a
	// search has something to follow. It returns nil to leave the question
	// to the default.
	answer func(question json.RawMessage) any

	mu       sync.Mutex
	requests []jevRequest
	failing  int    // the next this-many requests are answered 500
	refusal  string // if set, every request is answered 400 with this detail
}

// failNext makes the API answer its next n requests with a 500.
func (a *fakeJev) failNext(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failing = n
}

// refuse makes the API answer every request with a 400 and this detail.
func (a *fakeJev) refuse(detail string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.refusal = detail
}

type jevRequest struct {
	header    http.Header
	model     string
	questions []string // the names, sorted
}

func newFakeJev(t *testing.T) *fakeJev {
	t.Helper()
	api := &fakeJev{t: t}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	return api
}

func (a *fakeJev) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Model     string                     `json:"model"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		a.t.Errorf("the request body is not JSON: %v", err)
	}

	a.mu.Lock()
	names := make([]string, 0, len(body.Questions))
	for name := range body.Questions {
		names = append(names, name)
	}
	slices.Sort(names)
	a.requests = append(a.requests, jevRequest{header: r.Header.Clone(), model: body.Model, questions: names})
	call := len(a.requests)
	failing, refusal := a.failing > 0, a.refusal
	if failing {
		a.failing--
	}
	a.mu.Unlock()

	switch {
	case refusal != "":
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"detail": refusal})
		return
	case failing:
		w.WriteHeader(http.StatusInternalServerError)
		io.WriteString(w, `{"detail": "overloaded"}`)
		return
	}

	answers := make(map[string]any)
	for name, raw := range body.Questions {
		var question struct {
			Type string `json:"type"`
		}
		json.Unmarshal(raw, &question)
		if a.answer != nil {
			if decided := a.answer(raw); decided != nil {
				answers[name] = decided
				continue
			}
		}
		if question.Type == "noul" {
			answers[name] = map[string]any{"type": "noul", "noul": float64(call) / 10}
			continue
		}
		answers[name] = map[string]any{
			"type": "score", "score": float64(call%3) + 0.25, "confidence": 0.5,
			"legend":        map[string]string{"0": "nothing to do with it", "1": "on the subject", "2": "states the answer"},
			"probabilities": map[string]float64{"0": 0.25, "1": 0.25, "2": 0.5},
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"model":   body.Model + "-as-served",
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 100 * len(body.Questions), "output_tokens": 7},
	})
}

func (a *fakeJev) asked() []jevRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

// client returns a client of the fake API that sends through the transport
// and never retries.
func (a *fakeJev) client(transport http.RoundTripper) *typesafe.Client {
	return typesafe.New(typesafe.Config{
		APIKey: "a-secret-key", BaseURL: a.server.URL, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: transport},
	})
}

// offline returns a client that can reach nothing: whatever it returns came
// from the transport itself.
func offline(transport http.RoundTripper) *typesafe.Client {
	return typesafe.New(typesafe.Config{
		APIKey: "a-secret-key", BaseURL: "http://127.0.0.1:1", MaxRetries: -1,
		HTTPClient: &http.Client{Transport: transport},
	})
}

var (
	isItSo    = typesafe.Noul(map[string]any{"question": "Is it so?", "evidence": []string{"one", "two"}}, "It is.", "It is not.")
	howMuch   = typesafe.Score(map[string]any{"question": "How much?", "candidate": map[string]string{"title": "A"}}, "none", "some", "all")
	otherwise = typesafe.Score(map[string]any{"question": "How much?", "candidate": map[string]string{"title": "B"}}, "none", "some", "all")
)

func request(questions map[string]typesafe.Question) typesafe.Request {
	return typesafe.Request{Model: "jev-test", State: map[string]any{"user_question": "Why?"}, Questions: questions}
}

func openTape(t *testing.T) (*Tape, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tape", "jev-test.jsonl")
	tape, err := OpenTape(path)
	if err != nil {
		t.Fatalf("opening a tape that does not exist yet: %v", err)
	}
	return tape, path
}

func TestTheTapeAnswersAgainWhatJevAnsweredOnce(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	tape, path := openTape(t)

	recorded, err := api.client(tape.Record(1, http.DefaultTransport)).
		SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo, "b": howMuch}))
	if err != nil {
		t.Fatalf("recording: %v", err)
	}
	if recorded.Answers["a"].Noul != 0.1 || recorded.Answers["b"].Score != 1.25 || recorded.Usage.InputTokens != 200 {
		t.Fatalf("while recording, the caller was given %+v", recorded)
	}

	// Replayed, with nothing to reach: the same answers, in full.
	replayed, err := offline(tape.Replay(1)).
		SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo, "b": howMuch}))
	if err != nil {
		t.Fatalf("replaying: %v", err)
	}
	if !reflect.DeepEqual(replayed.Answers, recorded.Answers) {
		t.Errorf("replayed %+v, recorded %+v", replayed.Answers, recorded.Answers)
	}
	if got := replayed.Answers["b"].Probabilities["2"]; got != 0.5 {
		t.Errorf("the probabilities came back as %v", replayed.Answers["b"].Probabilities)
	}
	if replayed.Usage.InputTokens != 0 {
		t.Errorf("a replay reports %d tokens spent", replayed.Usage.InputTokens)
	}

	// A question is known by what it asks, not by the name it is asked
	// under or the company it is in.
	renamed, err := offline(tape.Replay(1)).SystemOne(ctx, request(map[string]typesafe.Question{"lead_0007": howMuch}))
	if err != nil || renamed.Answers["lead_0007"].Score != 1.25 {
		t.Errorf("the same question under another name was answered %+v, %v", renamed, err)
	}

	// And it survives being written down.
	if err := tape.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reopened, err := OpenTape(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	again, err := offline(reopened.Replay(1)).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo, "b": howMuch}))
	if err != nil || !reflect.DeepEqual(again.Answers, recorded.Answers) {
		t.Errorf("after saving and reopening, replayed %+v, %v", again, err)
	}
	if reopened.Len() != 2 || reopened.Answers(1) != 2 || reopened.Takes() != 1 {
		t.Errorf("the reopened tape holds %d questions, %d answers in take 1, %d takes",
			reopened.Len(), reopened.Answers(1), reopened.Takes())
	}
	if questions, tokens := reopened.Asked(); questions != 2 || tokens != 200 {
		t.Errorf("the tape says %d questions were asked for %d tokens, want 2 for 200", questions, tokens)
	}

	if got := len(api.asked()); got != 1 {
		t.Errorf("the API was asked %d times in all, want once", got)
	}
}

func TestEachTakeIsAskedOnItsOwn(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	tape, _ := openTape(t)
	ask := request(map[string]typesafe.Question{"a": isItSo})

	var recorded []float64
	for take := 1; take <= 3; take++ {
		response, err := api.client(tape.Record(take, http.DefaultTransport)).SystemOne(ctx, ask)
		if err != nil {
			t.Fatalf("recording take %d: %v", take, err)
		}
		recorded = append(recorded, response.Answers["a"].Noul)
	}
	if !slices.Equal(recorded, []float64{0.1, 0.2, 0.3}) {
		t.Fatalf("the three takes recorded %v", recorded)
	}

	// Each take replays its own answer, in any order.
	for _, take := range []int{3, 1, 2} {
		response, err := offline(tape.Replay(take)).SystemOne(ctx, ask)
		if err != nil || response.Answers["a"].Noul != recorded[take-1] {
			t.Errorf("take %d replayed %+v, %v; want %v", take, response, err, recorded[take-1])
		}
	}
	if tape.Takes() != 3 || tape.Len() != 1 {
		t.Errorf("the tape holds %d takes of %d questions", tape.Takes(), tape.Len())
	}
}

func TestARecordingPicksUpWhereItStopped(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	tape, _ := openTape(t)
	first := request(map[string]typesafe.Question{"a": isItSo, "b": howMuch})
	second := request(map[string]typesafe.Question{"c": otherwise})

	// The first session gets one request through, and the API fails the next.
	client := api.client(tape.Record(1, http.DefaultTransport))
	before, err := client.SystemOne(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	api.failNext(1)
	if _, err := client.SystemOne(ctx, second); err == nil {
		t.Fatal("a request the API failed was recorded as a success")
	}

	// The second session asks for everything again. What is on the tape is
	// not paid for twice; only what is missing goes to the API.
	client = api.client(tape.Record(1, http.DefaultTransport))
	after, err := client.SystemOne(ctx, first)
	if err != nil || !reflect.DeepEqual(after.Answers, before.Answers) {
		t.Fatalf("asked again, the recorded request returned %+v, %v", after, err)
	}
	if _, err := client.SystemOne(ctx, second); err != nil {
		t.Fatalf("the request that had failed: %v", err)
	}
	var sent [][]string
	for _, r := range api.asked() {
		sent = append(sent, r.questions)
	}
	if want := [][]string{{"a", "b"}, {"c"}, {"c"}}; !reflect.DeepEqual(sent, want) {
		t.Errorf("the API was sent %v, want %v", sent, want)
	}

	// A request that mixes a question on the tape with a new one goes out
	// whole, and the answer already taken stands: a take has one answer to
	// a question, whatever is asked again.
	mixed, err := client.SystemOne(ctx, request(map[string]typesafe.Question{
		"a": isItSo,
		"d": typesafe.Noul("Something new?", "", ""),
	}))
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Answers["a"].Noul != before.Answers["a"].Noul {
		t.Errorf("a question answered %v in this take was answered %v when asked again", before.Answers["a"].Noul, mixed.Answers["a"].Noul)
	}
	if mixed.Answers["d"].Noul == 0 {
		t.Errorf("the new question in the mixed request was not answered: %+v", mixed.Answers)
	}
	if tape.Answers(1) != 4 {
		t.Errorf("take 1 holds %d answers, want 4", tape.Answers(1))
	}
}

func TestReplayRefusesWhatTheTapeLacks(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	tape, _ := openTape(t)
	if _, err := api.client(tape.Record(1, http.DefaultTransport)).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo})); err != nil {
		t.Fatal(err)
	}

	// A question that was never asked.
	replay := tape.Replay(1)
	_, err := offline(replay).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo, "b": howMuch, "c": otherwise}))
	if !errors.Is(err, ErrNotOnTape) {
		t.Fatalf("a request with two unrecorded questions returned %v, want ErrNotOnTape", err)
	}
	if !strings.Contains(err.Error(), "2 of the 3") {
		t.Errorf("the error does not say how much is missing: %v", err)
	}
	// Asked for again, the same two are still the same two.
	offline(replay).SystemOne(ctx, request(map[string]typesafe.Question{"x": howMuch}))
	if got := replay.Missed(); got != 2 {
		t.Errorf("the replay counts %d questions missing, want 2", got)
	}

	// A take that was never recorded.
	if _, err := offline(tape.Replay(2)).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo})); !errors.Is(err, ErrNotOnTape) {
		t.Errorf("a take that was not recorded replayed: %v", err)
	}
	if got := len(api.asked()); got != 1 {
		t.Errorf("replaying reached the API: it was asked %d times", got)
	}
}

// post sends a hand-written request body through a transport.
func post(t *testing.T, transport http.RoundTripper, url, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := transport.RoundTrip(req)
	if err != nil {
		return 0, err.Error()
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(content)
}

func TestWhatAQuestionIsFiledUnder(t *testing.T) {
	api := newFakeJev(t)
	tape, _ := openTape(t)
	url := api.server.URL + "/v1/systemone"

	status, _ := post(t, tape.Record(1, http.DefaultTransport), url, `{
		"model": "jev-test",
		"state": {"user_question": "Why?", "context": "A review."},
		"questions": {"q": {"type": "noul", "instructions": {"question": "Is it so?", "evidence": ["one"]}, "criteria": {"true": "yes", "false": "no"}}}
	}`)
	if status != http.StatusOK {
		t.Fatalf("recording returned status %d", status)
	}

	same := map[string]string{
		"another name, and its fields in another order": `{"questions": {"other": {"criteria": {"false": "no", "true": "yes"}, "instructions": {"evidence": ["one"], "question": "Is it so?"}, "type": "noul"}},
			"state": {"context": "A review.", "user_question": "Why?"}, "model": "jev-test"}`,
	}
	different := map[string]string{
		"another model":    `{"model": "jev-other", "state": {"user_question": "Why?", "context": "A review."}, "questions": {"q": {"type": "noul", "instructions": {"question": "Is it so?", "evidence": ["one"]}, "criteria": {"true": "yes", "false": "no"}}}}`,
		"another state":    `{"model": "jev-test", "state": {"user_question": "Why not?", "context": "A review."}, "questions": {"q": {"type": "noul", "instructions": {"question": "Is it so?", "evidence": ["one"]}, "criteria": {"true": "yes", "false": "no"}}}}`,
		"no context":       `{"model": "jev-test", "state": {"user_question": "Why?"}, "questions": {"q": {"type": "noul", "instructions": {"question": "Is it so?", "evidence": ["one"]}, "criteria": {"true": "yes", "false": "no"}}}}`,
		"other evidence":   `{"model": "jev-test", "state": {"user_question": "Why?", "context": "A review."}, "questions": {"q": {"type": "noul", "instructions": {"question": "Is it so?", "evidence": ["two"]}, "criteria": {"true": "yes", "false": "no"}}}}`,
		"other criteria":   `{"model": "jev-test", "state": {"user_question": "Why?", "context": "A review."}, "questions": {"q": {"type": "noul", "instructions": {"question": "Is it so?", "evidence": ["one"]}, "criteria": {"true": "yes", "false": "never"}}}}`,
		"evidence reorder": `{"model": "jev-test", "state": {"user_question": "Why?", "context": "A review."}, "questions": {"q": {"type": "noul", "instructions": {"question": "Is it so?", "evidence": ["one", "one"]}, "criteria": {"true": "yes", "false": "no"}}}}`,
	}
	for name, body := range same {
		if status, content := post(t, tape.Replay(1), url, body); status != http.StatusOK || !strings.Contains(content, `"other"`) {
			t.Errorf("the same question with %s was not found on the tape: %d %s", name, status, content)
		}
	}
	for name, body := range different {
		if status, content := post(t, tape.Replay(1), url, body); status != 0 || !strings.Contains(content, "not on the tape") {
			t.Errorf("a question with %s was answered from the tape: %d %s", name, status, content)
		}
	}
}

func TestTheMeterStopsAtItsBudget(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	meter := NewMeter(250) // tokens
	client := api.client(meter.Transport(http.DefaultTransport))

	// Each request costs 100 tokens. The third takes the spending past the
	// budget, which is only known once it has been answered; nothing is
	// sent after that.
	for i := 1; i <= 3; i++ {
		if _, err := client.SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo})); err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
	}
	_, err := client.SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo}))
	if !errors.Is(err, ErrBudgetSpent) {
		t.Fatalf("a request after the budget was spent returned %v, want ErrBudgetSpent", err)
	}
	if got := len(api.asked()); got != 3 {
		t.Errorf("the API was asked %d times, want 3", got)
	}
	if requests, tokens := meter.Spent(); requests != 3 || tokens != 300 {
		t.Errorf("the meter reads %d requests and %d tokens, want 3 and 300", requests, tokens)
	}

	// What comes off the tape is not metered: only what reaches the API is.
	tape, _ := openTape(t)
	metered := NewMeter(1000)
	recording := api.client(tape.Record(1, metered.Transport(http.DefaultTransport)))
	for range 3 {
		if _, err := recording.SystemOne(ctx, request(map[string]typesafe.Question{"b": howMuch})); err != nil {
			t.Fatal(err)
		}
	}
	if requests, tokens := metered.Spent(); requests != 1 || tokens != 100 {
		t.Errorf("three askings of one question were metered as %d requests and %d tokens, want 1 and 100", requests, tokens)
	}

	// A meter with no budget lets nothing through.
	if _, err := api.client(NewMeter(0).Transport(http.DefaultTransport)).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo})); !errors.Is(err, ErrBudgetSpent) {
		t.Errorf("a meter with a budget of nothing let a request through: %v", err)
	}
}

func TestTheTapeHoldsNoSecretAndNoLegend(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	tape, path := openTape(t)
	if _, err := api.client(tape.Record(1, http.DefaultTransport)).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo, "b": howMuch})); err != nil {
		t.Fatal(err)
	}
	// The key did go to the API, in a header.
	if got := api.asked()[0].header.Get("Authorization"); got != "Bearer a-secret-key" {
		t.Fatalf("the API was sent the authorization %q", got)
	}
	if err := tape.Save(); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"a-secret-key", "Bearer", "Authorization"} {
		if bytes.Contains(content, []byte(secret)) {
			t.Errorf("the tape file contains %q", secret)
		}
	}
	// An answer is kept as Jev gave it, probabilities and all, except for
	// the legend, which only repeats the question's own criteria.
	for _, kept := range []string{`"probabilities"`, `"confidence"`, `"score"`, `"noul"`} {
		if !bytes.Contains(content, []byte(kept)) {
			t.Errorf("the tape file does not contain %s", kept)
		}
	}
	if bytes.Contains(content, []byte("legend")) || bytes.Contains(content, []byte("on the subject")) {
		t.Errorf("the tape file kept the legend:\n%s", content)
	}
	// Nor is what was asked kept: only a fingerprint of it.
	if bytes.Contains(content, []byte("Is it so?")) || bytes.Contains(content, []byte("Why?")) {
		t.Errorf("the tape file kept the questions themselves:\n%s", content)
	}
}

func TestAReplyThatIsNotASuccessIsPassedOnAndNotKept(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	api.refuse("max_tokens_exceeded: the request is longer than the model's context")
	tape, _ := openTape(t)

	_, err := api.client(tape.Record(1, http.DefaultTransport)).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo}))
	var refused *typesafe.Error
	if !errors.As(err, &refused) || !refused.TooLong() {
		t.Fatalf("the API's refusal reached the caller as %v", err)
	}
	if tape.Len() != 0 {
		t.Errorf("a refused request left %d questions on the tape", tape.Len())
	}
	if questions, tokens := tape.Asked(); questions != 0 || tokens != 0 {
		t.Errorf("a refused request was counted as %d questions and %d tokens", questions, tokens)
	}
}

func TestRehearsingCountsWhatARecordingWouldAsk(t *testing.T) {
	ctx := t.Context()
	api := newFakeJev(t)
	tape, _ := openTape(t)
	if _, err := api.client(tape.Record(1, http.DefaultTransport)).SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo})); err != nil {
		t.Fatal(err)
	}

	rehearsal := tape.Rehearse(1)
	client := offline(rehearsal)
	// On the tape already: nothing to ask.
	if _, err := client.SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo})); err != nil {
		t.Fatal(err)
	}
	// One known and two new: the request would go out whole.
	answered, err := client.SystemOne(ctx, request(map[string]typesafe.Question{"a": isItSo, "b": howMuch, "c": otherwise}))
	if err != nil {
		t.Fatalf("rehearsing a request with new questions: %v", err)
	}
	// Every question gets an answer of the right kind, so that whatever is
	// being rehearsed runs to its end.
	if answered.Answers["a"].Noul != 0.1 || answered.Answers["b"].Type != "score" || answered.Answers["c"].Type != "score" {
		t.Errorf("the rehearsal answered %+v", answered.Answers)
	}
	// The same new question again: a recording would have it on the tape by
	// now, so it would not be sent, and it is not counted again.
	if _, err := client.SystemOne(ctx, request(map[string]typesafe.Question{"b": howMuch})); err != nil {
		t.Fatal(err)
	}

	lacking := rehearsal.Lacking()
	if lacking.Requests != 1 || lacking.Questions != 2 || lacking.Bytes < 100 {
		t.Errorf("the rehearsal counts %+v, want 1 request and 2 new questions", lacking)
	}
	if tape.Len() != 1 || len(api.asked()) != 1 {
		t.Errorf("rehearsing changed the tape (%d questions) or reached the API (%d requests)", tape.Len(), len(api.asked()))
	}
}

func TestOpenTapeChecksTheFile(t *testing.T) {
	dir := t.TempDir()
	broken := map[string]string{
		"a line that is not JSON":   "{\"tape\":1}\n{\"q\":\"abc\",\n",
		"no header":                 "{\"q\":\"abc\",\"a\":[{\"type\":\"noul\",\"noul\":0.5}]}\n",
		"a question with no key":    "{\"tape\":1}\n{\"q\":\"\",\"a\":[{\"type\":\"noul\",\"noul\":0.5}]}\n",
		"the same question twice":   "{\"tape\":1}\n{\"q\":\"abc\",\"a\":[null]}\n{\"q\":\"abc\",\"a\":[null]}\n",
		"a tape of another version": "{\"tape\":2}\n",
	}
	for name, content := range broken {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".jsonl")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenTape(path); err == nil {
			t.Errorf("a tape file with %s was opened", name)
		}
	}
}

func TestTheTapeIsSafeToUseFromManyGoroutines(t *testing.T) {
	ctx := context.Background()
	api := newFakeJev(t)
	tape, _ := openTape(t)
	questions := make([]typesafe.Question, 12)
	for i := range questions {
		questions[i] = typesafe.Noul(fmt.Sprintf("Is it %d?", i), "", "")
	}

	var group sync.WaitGroup
	for i := range questions {
		group.Go(func() {
			client := api.client(tape.Record(1+i%2, http.DefaultTransport))
			if _, err := client.SystemOne(ctx, request(map[string]typesafe.Question{"q": questions[i], "shared": isItSo})); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if err := tape.Save(); err != nil {
		t.Fatal(err)
	}
	for i := range questions {
		if _, err := offline(tape.Replay(1+i%2)).SystemOne(ctx, request(map[string]typesafe.Question{"q": questions[i], "shared": isItSo})); err != nil {
			t.Errorf("question %d was not on the tape: %v", i, err)
		}
	}
	if tape.Len() != 13 {
		t.Errorf("the tape holds %d questions, want 13", tape.Len())
	}
}

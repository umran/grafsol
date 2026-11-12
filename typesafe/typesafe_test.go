package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testKey = "ts-secret-key-that-must-never-leak"

func TestSystemOneSendsWhatTheAPIExpects(t *testing.T) {
	// The three answers are the examples from the API's own reference.
	api := newFakeAPI(t, reply{status: 200, body: `{
		"model": "jev-1.14.0",
		"answers": {
			"billing": {"type": "noul", "noul": 0.98},
			"tone": {"type": "choice", "choice": "calm", "confidence": 0.7,
			         "probabilities": {"angry": 0.15, "calm": 0.8, "excited": 0.05}},
			"urgency": {"type": "score", "score": 1.7, "confidence": 0.55,
			            "legend": {"0": "Can wait", "1": "Needs attention this week", "2": "Needs attention today"},
			            "probabilities": {"0": 0.1, "1": 0.1, "2": 0.8}}
		},
		"usage": {"input_tokens": 120, "output_tokens": 12}
	}`})
	client := api.client(Config{})

	response, err := client.SystemOne(t.Context(), Request{
		Model: "jev-1.14.0",
		State: map[string]any{"message": "I was charged twice. Please refund the extra charge."},
		Questions: map[string]Question{
			"billing": Noul("Is `message` about billing?", "It concerns a charge, invoice or refund.", "It concerns something else."),
			"tone":    Choice("What is the tone of `message`?", map[string]string{"angry": "Upset or hostile", "calm": "Neutral or polite", "excited": "Enthusiastic"}),
			"urgency": Score(map[string]any{"question": "How urgent is `message`?"}, "Can wait", "Needs attention this week", "Needs attention today"),
		},
	})
	if err != nil {
		t.Fatalf("SystemOne: %v", err)
	}

	sent := api.only()
	if sent.method != http.MethodPost || sent.path != "/v1/systemone" {
		t.Errorf("the request was %s %s, want POST /v1/systemone", sent.method, sent.path)
	}
	if got := sent.header.Get("Authorization"); got != "Bearer "+testKey {
		t.Errorf("the Authorization header does not carry the key as a bearer token")
	}
	if got := sent.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type is %q, want application/json", got)
	}
	requireJSON(t, sent.body, `{
		"model": "jev-1.14.0",
		"state": {"message": "I was charged twice. Please refund the extra charge."},
		"questions": {
			"billing": {"type": "noul", "instructions": "Is `+"`message`"+` about billing?",
			            "criteria": {"true": "It concerns a charge, invoice or refund.", "false": "It concerns something else."}},
			"tone": {"type": "choice", "instructions": "What is the tone of `+"`message`"+`?",
			         "criteria": {"angry": "Upset or hostile", "calm": "Neutral or polite", "excited": "Enthusiastic"}},
			"urgency": {"type": "score", "instructions": {"question": "How urgent is `+"`message`"+`?"},
			            "criteria": ["Can wait", "Needs attention this week", "Needs attention today"]}
		}
	}`)

	if response.Model != "jev-1.14.0" || response.Usage != (Usage{InputTokens: 120, OutputTokens: 12}) {
		t.Errorf("model and usage came back as %q and %+v", response.Model, response.Usage)
	}
	if got := response.Answers["billing"]; got.Type != "noul" || got.Noul != 0.98 {
		t.Errorf("the noul answer is %+v", got)
	}
	if got := response.Answers["tone"]; got.Choice != "calm" || got.Confidence != 0.7 || got.Probabilities["calm"] != 0.8 {
		t.Errorf("the choice answer is %+v", got)
	}
	if got := response.Answers["urgency"]; got.Score != 1.7 || got.Confidence != 0.55 || got.Probabilities["2"] != 0.8 {
		t.Errorf("the score answer is %+v", got)
	}
}

func TestSystemOneFillsInWhatIsLeftOut(t *testing.T) {
	api := newFakeAPI(t, reply{status: 200, body: `{"model": "jev-1.14.0", "answers": {"q": {"type": "noul", "noul": 0.5}}, "usage": {}}`})
	client := api.client(Config{})

	if _, err := client.SystemOne(t.Context(), Request{State: "Some text.", Questions: map[string]Question{"q": Noul("Is it text?", "", "")}}); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	// No model was named, so the default alias is; a Noul without criteria
	// sends none; the state may be a plain string.
	requireJSON(t, api.only().body, `{
		"model": "jev-latest",
		"state": "Some text.",
		"questions": {"q": {"type": "noul", "instructions": "Is it text?"}}
	}`)
	if got := api.only().header.Get("User-Agent"); got != "grafsol" {
		t.Errorf("User-Agent is %q, want grafsol", got)
	}
}

func TestSystemOneRetriesWhatMayPass(t *testing.T) {
	ok := reply{status: 200, body: `{"model": "jev-1.14.0", "answers": {}, "usage": {}}`}
	cases := []struct {
		name       string
		maxRetries int
		replies    []reply
		attempts   int
		status     int // of the error returned, or 0 for success
	}{
		{"rate limited, then fine", 0, []reply{{status: 429}, ok}, 2, 0},
		{"overloaded twice, then fine", 0, []reply{{status: 529}, {status: 503}, ok}, 3, 0},
		{"a request timeout is retried", 0, []reply{{status: 408}, ok}, 2, 0},
		{"failing for good gives up after two retries", 0, []reply{{status: 500}}, 3, 500},
		{"a bad request is not retried", 0, []reply{{status: 400}, ok}, 1, 400},
		{"a bad key is not retried", 0, []reply{{status: 401}, ok}, 1, 401},
		{"an invalid request is not retried", 0, []reply{{status: 422}, ok}, 1, 422},
		{"retries can be turned off", -1, []reply{{status: 503}, ok}, 1, 503},
		{"retries can be turned up", 4, []reply{{status: 503}}, 5, 503},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newFakeAPI(t, c.replies...)
			client := api.client(Config{MaxRetries: c.maxRetries})

			_, err := client.SystemOne(t.Context(), Request{State: "s", Questions: map[string]Question{"q": Noul("?", "", "")}})
			if got := len(api.sent()); got != c.attempts {
				t.Errorf("the API was asked %d times, want %d", got, c.attempts)
			}
			var refused *Error
			switch {
			case c.status == 0 && err != nil:
				t.Errorf("SystemOne: %v", err)
			case c.status != 0 && (!errors.As(err, &refused) || refused.Status != c.status):
				t.Errorf("SystemOne returned %v, want an Error with status %d", err, c.status)
			}
			if got := len(api.waits); got != c.attempts-1 {
				t.Errorf("the client paused %d times between %d attempts", got, c.attempts)
			}
		})
	}
}

func TestSystemOneWaitsAsLongAsItIsTold(t *testing.T) {
	ok := reply{status: 200, body: `{"model": "jev-1.14.0", "answers": {}, "usage": {}}`}
	request := Request{State: "s", Questions: map[string]Question{"q": Noul("?", "", "")}}

	// When the API names a wait, that is the wait.
	api := newFakeAPI(t, reply{status: 429, header: map[string]string{"Retry-After": "7"}}, ok)
	if _, err := api.client(Config{}).SystemOne(t.Context(), request); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if len(api.waits) != 1 || api.waits[0] != 7*time.Second {
		t.Errorf("after Retry-After: 7 the client waited %v", api.waits)
	}

	// When it does not, the wait starts near half a second and doubles.
	api = newFakeAPI(t, reply{status: 529}, reply{status: 529}, ok)
	if _, err := api.client(Config{}).SystemOne(t.Context(), request); err != nil {
		t.Fatalf("SystemOne: %v", err)
	}
	if len(api.waits) != 2 {
		t.Fatalf("the client waited %v, want two waits", api.waits)
	}
	for i, around := range []time.Duration{500 * time.Millisecond, time.Second} {
		if low, high := around*3/4, around*5/4; api.waits[i] < low || api.waits[i] > high {
			t.Errorf("wait %d was %v, want between %v and %v", i+1, api.waits[i], low, high)
		}
	}
}

func TestSystemOneRetriesABrokenConnection(t *testing.T) {
	api := newFakeAPI(t, reply{status: 200, body: `{"model": "jev-1.14.0", "answers": {}, "usage": {}}`})
	flaky := &flakyTransport{failures: 2, next: http.DefaultTransport}
	client := api.client(Config{HTTPClient: &http.Client{Transport: flaky}})

	if _, err := client.SystemOne(t.Context(), Request{State: "s", Questions: map[string]Question{"q": Noul("?", "", "")}}); err != nil {
		t.Fatalf("SystemOne after two broken connections: %v", err)
	}
	if flaky.attempts != 3 {
		t.Errorf("the client tried %d times, want 3", flaky.attempts)
	}
}

func TestErrorsCarryTheAPIsExplanation(t *testing.T) {
	cases := []struct {
		name    string
		reply   reply
		detail  string // must appear in the error
		tooLong bool
	}{
		{"a plain explanation", reply{status: 400, body: `{"detail": "state: too many nested levels"}`}, "state: too many nested levels", false},
		{"a request that is too long", reply{status: 400, body: `{"detail": "max_tokens_exceeded: 70112 tokens exceed the per-request context of 64000"}`}, "70112 tokens", true},
		{"too long, explained as an object", reply{status: 400, body: `{"detail": {"code": "max_tokens_exceeded", "limit": 64000}}`}, `"limit": 64000`, true},
		{"a list of invalid fields", reply{status: 422, body: `{"detail": [{"loc": ["body", "state"], "msg": "Field required", "type": "missing"}]}`}, "Field required", false},
		{"something that is not JSON", reply{status: 401, body: "who are you?\n"}, "who are you?", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newFakeAPI(t, c.reply)
			_, err := api.client(Config{}).SystemOne(t.Context(), Request{State: "s", Questions: map[string]Question{"q": Noul("?", "", "")}})

			var refused *Error
			if !errors.As(err, &refused) {
				t.Fatalf("SystemOne returned %v, want an Error", err)
			}
			if refused.Status != c.reply.status || !strings.Contains(refused.Detail, c.detail) {
				t.Errorf("the error is %d %q, want %d with %q in it", refused.Status, refused.Detail, c.reply.status, c.detail)
			}
			if refused.TooLong() != c.tooLong {
				t.Errorf("TooLong is %v, want %v", refused.TooLong(), c.tooLong)
			}
			if !strings.Contains(err.Error(), c.detail) {
				t.Errorf("the error's text %q leaves out the explanation", err)
			}
		})
	}

	// An explanation that runs on is cut, and says that it was.
	api := newFakeAPI(t, reply{status: 400, body: `{"detail": "` + strings.Repeat("x", 5000) + `"}`})
	_, err := api.client(Config{}).SystemOne(t.Context(), Request{State: "s", Questions: map[string]Question{"q": Noul("?", "", "")}})
	if text := err.Error(); len(text) > 2200 || !strings.Contains(text, "cut short") {
		t.Errorf("a 5,000-character explanation produced an error of %d characters", len(text))
	}
}

func TestTheAPIKeyNeverLeaks(t *testing.T) {
	request := Request{State: "s", Questions: map[string]Question{"q": Noul("?", "", "")}}

	// Refused by the API.
	api := newFakeAPI(t, reply{status: 401, body: `{"detail": "Invalid API key"}`})
	_, refused := api.client(Config{}).SystemOne(t.Context(), request)

	// Never reached the API at all.
	api.server.Close()
	_, unreachable := api.client(Config{MaxRetries: -1}).SystemOne(t.Context(), request)

	for name, err := range map[string]error{"refused": refused, "unreachable": unreachable} {
		if err == nil {
			t.Fatalf("the %s request succeeded", name)
		}
		if strings.Contains(err.Error(), testKey) {
			t.Errorf("the error for the %s request contains the API key: %v", name, err)
		}
	}
}

func TestSystemOneStopsWhenTheContextEnds(t *testing.T) {
	request := Request{State: "s", Questions: map[string]Question{"q": Noul("?", "", "")}}

	// Ended before the call.
	api := newFakeAPI(t, reply{status: 200, body: `{}`})
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := api.client(Config{}).SystemOne(ended, request); !errors.Is(err, context.Canceled) {
		t.Errorf("with an ended context SystemOne returned %v", err)
	}

	// Ended while waiting to retry: the wait is abandoned, not sat out.
	api = newFakeAPI(t, reply{status: 529})
	client := api.client(Config{})
	client.poster.Pause = nil // wait for real, so that there is a wait to abandon
	waiting, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := client.SystemOne(waiting, request); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("SystemOne returned %v, want the context's error", err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Errorf("SystemOne took %v to notice that its context had ended", elapsed)
	}
}

// --- helpers ----------------------------------------------------------------

// fakeAPI stands in for the System One API. It answers with scripted
// replies, in order, repeating the last one, and records what it was sent
// and how long the client under test paused between attempts.
type fakeAPI struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	replies  []reply
	requests []sentRequest
	waits    []time.Duration
}

type reply struct {
	status int
	body   string
	header map[string]string
}

type sentRequest struct {
	method, path string
	header       http.Header
	body         []byte
}

func newFakeAPI(t *testing.T, replies ...reply) *fakeAPI {
	t.Helper()
	api := &fakeAPI{t: t, replies: replies}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		api.mu.Lock()
		api.requests = append(api.requests, sentRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: body})
		next := api.replies[min(len(api.requests), len(api.replies))-1]
		api.mu.Unlock()

		for name, value := range next.header {
			w.Header().Set(name, value)
		}
		w.WriteHeader(next.status)
		io.WriteString(w, next.body)
	}))
	t.Cleanup(api.server.Close)
	return api
}

// client returns a Client that talks to the fake API and pauses for no time
// at all, noting how long each pause would have been.
func (a *fakeAPI) client(cfg Config) *Client {
	cfg.APIKey = testKey
	cfg.BaseURL = a.server.URL
	client := New(cfg)
	client.poster.Pause = func(_ context.Context, wait time.Duration) error {
		a.waits = append(a.waits, wait)
		return nil
	}
	return client
}

func (a *fakeAPI) sent() []sentRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sentRequest(nil), a.requests...)
}

// only returns the one request the API received, failing the test if there
// was not exactly one.
func (a *fakeAPI) only() sentRequest {
	a.t.Helper()
	sent := a.sent()
	if len(sent) != 1 {
		a.t.Fatalf("the API received %d requests, want 1", len(sent))
	}
	return sent[0]
}

// flakyTransport fails a number of requests without sending them, then lets
// the rest through.
type flakyTransport struct {
	failures int
	attempts int
	next     http.RoundTripper
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.attempts++
	if f.attempts <= f.failures {
		return nil, errors.New("connection reset by peer")
	}
	return f.next.RoundTrip(r)
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
		t.Fatalf("the request body is\n%s\nwant\n%s", got, want)
	}
}

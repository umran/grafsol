package inference

import (
	"context"
	"encoding/json"
	"errors"
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
	"time"

	"github.com/umran/grafsol/model"
)

// example reads one of the example requests and replies in
// inference/contract. The Python services are tested against the same
// files, so a client that sends and reads exactly these agrees with them.
func example(t *testing.T, name string) []byte {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", "inference", "contract", name))
	if err != nil {
		t.Fatalf("reading the contract example: %v", err)
	}
	return content
}

// sameJSON reports whether two pieces of JSON say the same thing, whatever
// their spacing and the order of their fields.
func sameJSON(t *testing.T, a, b []byte) bool {
	t.Helper()
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		t.Fatalf("not JSON: %v: %s", err, a)
	}
	if err := json.Unmarshal(b, &y); err != nil {
		t.Fatalf("not JSON: %v: %s", err, b)
	}
	return reflect.DeepEqual(x, y)
}

// received is one request a fake service was sent.
type received struct {
	method, path, contentType string
	body                      []byte
}

// fakeService stands in for one of the Python services. It remembers what
// it is sent and answers as the test tells it to.
type fakeService struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []received

	// answer says what to reply to the nth request (counting from 1).
	answer func(n int, r received) (status int, body []byte)
}

func newFakeService(t *testing.T, answer func(n int, r received) (int, []byte)) *fakeService {
	t.Helper()
	service := &fakeService{t: t, answer: answer}
	service.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got := received{method: r.Method, path: r.URL.Path, contentType: r.Header.Get("Content-Type"), body: body}

		service.mu.Lock()
		service.requests = append(service.requests, got)
		n := len(service.requests)
		service.mu.Unlock()

		status, reply := service.answer(n, got)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(reply)
	}))
	t.Cleanup(service.server.Close)
	return service
}

// always answers every request the same way.
func always(status int, body []byte) func(int, received) (int, []byte) {
	return func(int, received) (int, []byte) { return status, body }
}

func (s *fakeService) sent() []received {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// only returns the one request the service was sent, and fails the test if
// there was not exactly one.
func (s *fakeService) only() received {
	s.t.Helper()
	sent := s.sent()
	if len(sent) != 1 {
		s.t.Fatalf("the service was sent %d requests, want 1", len(sent))
	}
	return sent[0]
}

// embedder returns an Embedder that talks to the fake and never really
// waits between attempts.
func (s *fakeService) embedder(cfg EmbedderConfig) *Embedder {
	cfg.BaseURL = s.server.URL
	embedder := NewEmbedder(cfg)
	embedder.service.poster.Pause = instantly
	return embedder
}

// summarizer returns a Summarizer that talks to the fake and never really
// waits between attempts.
func (s *fakeService) summarizer(cfg SummarizerConfig) *Summarizer {
	cfg.BaseURL = s.server.URL
	summarizer := NewSummarizer(cfg)
	summarizer.service.poster.Pause = instantly
	return summarizer
}

func instantly(context.Context, time.Duration) error { return nil }

// textsOf returns the texts of an example embedding request.
func textsOf(t *testing.T, request []byte) []string {
	t.Helper()
	var body struct {
		Texts []string `json:"texts"`
	}
	if err := json.Unmarshal(request, &body); err != nil {
		t.Fatal(err)
	}
	return body.Texts
}

// embeddingsOf returns the vectors of an example embedding reply.
func embeddingsOf(t *testing.T, reply []byte) [][]float32 {
	t.Helper()
	var body struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(reply, &body); err != nil {
		t.Fatal(err)
	}
	return body.Embeddings
}

func TestTheClientSendsAndReadsWhatTheContractSays(t *testing.T) {
	t.Run("documents", func(t *testing.T) {
		request, reply := example(t, "embed_documents.request.json"), example(t, "embed_documents.reply.json")
		service := newFakeService(t, always(http.StatusOK, reply))

		vectors, err := service.embedder(EmbedderConfig{}).EmbedDocuments(t.Context(), textsOf(t, request))
		if err != nil {
			t.Fatalf("EmbedDocuments: %v", err)
		}

		sent := service.only()
		if sent.method != http.MethodPost || sent.path != "/embed/documents" || sent.contentType != "application/json" {
			t.Errorf("the request was %s %s as %q", sent.method, sent.path, sent.contentType)
		}
		if !sameJSON(t, sent.body, request) {
			t.Errorf("the request body was %s, want what the contract shows: %s", sent.body, request)
		}
		if !reflect.DeepEqual(vectors, embeddingsOf(t, reply)) {
			t.Errorf("the vectors came back as %v", vectors)
		}
	})

	t.Run("queries", func(t *testing.T) {
		request, reply := example(t, "embed_queries.request.json"), example(t, "embed_queries.reply.json")
		service := newFakeService(t, always(http.StatusOK, reply))

		vectors, err := service.embedder(EmbedderConfig{}).EmbedQueries(t.Context(), textsOf(t, request))
		if err != nil {
			t.Fatalf("EmbedQueries: %v", err)
		}

		sent := service.only()
		if sent.method != http.MethodPost || sent.path != "/embed/queries" {
			t.Errorf("the request was %s %s", sent.method, sent.path)
		}
		if !sameJSON(t, sent.body, request) {
			t.Errorf("the request body was %s, want what the contract shows: %s", sent.body, request)
		}
		if !reflect.DeepEqual(vectors, embeddingsOf(t, reply)) {
			t.Errorf("the vectors came back as %v", vectors)
		}
	})

	t.Run("describe", func(t *testing.T) {
		service := newFakeService(t, always(http.StatusOK, example(t, "describe.reply.json")))

		description, err := service.embedder(EmbedderConfig{}).Describe(t.Context())
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}

		sent := service.only()
		if sent.method != http.MethodPost || sent.path != "/describe" {
			t.Errorf("the request was %s %s", sent.method, sent.path)
		}
		if !sameJSON(t, sent.body, example(t, "describe.request.json")) {
			t.Errorf("the request body was %s", sent.body)
		}
		want := EmbedderDescription{
			Model:      "microsoft/harrier-oss-v1-0.6b",
			Revision:   "f9b9dc8d367d443f2479d27aa5d8d2850c0774ee",
			Space:      "microsoft/harrier-oss-v1-0.6b@f9b9dc8d367d443f2479d27aa5d8d2850c0774ee; query prompt web_search_query; document prompt none; 8 dimensions",
			Dimensions: 8,
			MaxTokens:  8192,
		}
		if description != want {
			t.Errorf("the description is %+v, want %+v", description, want)
		}
	})

	t.Run("summarize", func(t *testing.T) {
		request, reply := example(t, "summarize.request.json"), example(t, "summarize.reply.json")
		service := newFakeService(t, always(http.StatusOK, reply))
		var asked struct{ Title, Text string }
		var answered struct{ Summary string }
		json.Unmarshal(request, &asked)
		json.Unmarshal(reply, &answered)

		summary, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), asked.Title, asked.Text)
		if err != nil {
			t.Fatalf("Summarize: %v", err)
		}

		sent := service.only()
		if sent.method != http.MethodPost || sent.path != "/summarize" || sent.contentType != "application/json" {
			t.Errorf("the request was %s %s as %q", sent.method, sent.path, sent.contentType)
		}
		if !sameJSON(t, sent.body, request) {
			t.Errorf("the request body was %s, want what the contract shows: %s", sent.body, request)
		}
		if summary != answered.Summary || summary == "" {
			t.Errorf("the summary came back as %q", summary)
		}
	})
}

func TestARefusalIsADeclineAndSaysWhy(t *testing.T) {
	service := newFakeService(t, always(http.StatusUnprocessableEntity, example(t, "refused.reply.json")))

	_, err := service.embedder(EmbedderConfig{}).EmbedDocuments(t.Context(), textsOf(t, example(t, "refused.request.json")))

	if !errors.Is(err, model.ErrDeclined) {
		t.Fatalf("the error is %v, want one that wraps model.ErrDeclined", err)
	}
	var refusal *Error
	if !errors.As(err, &refusal) {
		t.Fatalf("the error %v carries no *Error", err)
	}
	want := Error{Status: 422, Reason: "too_long", Message: "text 2 of 2 is 9 tokens long, and the most that is embedded is 8"}
	if *refusal != want {
		t.Errorf("the refusal is %+v, want %+v", *refusal, want)
	}
	if !strings.Contains(err.Error(), "too_long") || !strings.Contains(err.Error(), "9 tokens long") {
		t.Errorf("the error does not say why: %v", err)
	}
	// A refusal is final: asking again would get the same answer.
	if n := len(service.sent()); n != 1 {
		t.Errorf("a refused request was sent %d times", n)
	}
}

func TestOnlyARefusalOfTheServicesOwnIsADecline(t *testing.T) {
	// A 422 that does not carry the service's reason came from somewhere
	// else, a proxy perhaps. Treating it as a decline would quietly strip
	// documents of their summaries and embeddings.
	for _, body := range []string{`{"error": "unprocessable"}`, `{"reason": "", "message": "x"}`, `[{"error": "no"}]`, `not json`, ``} {
		service := newFakeService(t, always(http.StatusUnprocessableEntity, []byte(body)))

		_, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), "T", "text")

		if err == nil || errors.Is(err, model.ErrDeclined) {
			t.Errorf("a 422 with body %q gave %v; want an error that is not a decline", body, err)
		}
		var failure *Error
		if !errors.As(err, &failure) || failure.Status != 422 || failure.Reason != "" {
			t.Errorf("a 422 with body %q gave %+v", body, failure)
		}
	}
}

func TestAMistakenRequestIsAnErrorWithTheServicesExplanation(t *testing.T) {
	// BentoML sends the body of an error a service raises as a one-element
	// list, and its own complaints about a malformed request as an object.
	cases := []struct {
		reply   string
		message string
	}{
		{"mistaken.reply.json", "text 2 of 2 is empty"},
		{"invalid.reply.json", "1 validation error for Input (texts: Input should be a valid array)"},
	}
	for _, c := range cases {
		service := newFakeService(t, always(http.StatusBadRequest, example(t, c.reply)))

		_, err := service.embedder(EmbedderConfig{}).EmbedQueries(t.Context(), []string{"a question"})

		var failure *Error
		if !errors.As(err, &failure) {
			t.Fatalf("%s: the error %v carries no *Error", c.reply, err)
		}
		if failure.Status != 400 || failure.Message != c.message || failure.Reason != "" {
			t.Errorf("%s: the failure is %+v, want 400 with %q", c.reply, *failure, c.message)
		}
		if errors.Is(err, model.ErrDeclined) {
			t.Errorf("%s: a mistaken request was taken for a decline", c.reply)
		}
		if n := len(service.sent()); n != 1 {
			t.Errorf("%s: a mistaken request was sent %d times", c.reply, n)
		}
	}
}

func TestAFailingServiceIsTriedAgainAndThenReported(t *testing.T) {
	service := newFakeService(t, always(http.StatusInternalServerError, example(t, "failed.reply.json")))

	_, err := service.embedder(EmbedderConfig{}).EmbedQueries(t.Context(), []string{"a question"})

	var failure *Error
	if !errors.As(err, &failure) || failure.Status != 500 {
		t.Fatalf("the error is %v, want a 500", err)
	}
	if failure.Message != "An unexpected error has occurred, please check the server log." {
		t.Errorf("the message is %q", failure.Message)
	}
	if n := len(service.sent()); n != 3 {
		t.Errorf("a failing service was tried %d times, want 3", n)
	}
}

func TestAnExplanationThatIsNotJSONIsPassedOnAndALongOneIsCut(t *testing.T) {
	service := newFakeService(t, always(http.StatusBadGateway, []byte("  upstream is down\n")))
	_, err := service.embedder(EmbedderConfig{MaxRetries: -1}).EmbedQueries(t.Context(), []string{"q"})
	var failure *Error
	if !errors.As(err, &failure) || failure.Message != "upstream is down" {
		t.Errorf("the failure is %+v", failure)
	}

	service = newFakeService(t, always(http.StatusBadGateway, []byte(strings.Repeat("x", 5000))))
	_, err = service.embedder(EmbedderConfig{MaxRetries: -1}).EmbedQueries(t.Context(), []string{"q"})
	if !errors.As(err, &failure) || len(failure.Message) > 2100 || !strings.HasSuffix(failure.Message, "(cut short)") {
		t.Errorf("a 5,000 character explanation came through as %d characters", len(failure.Message))
	}
}

func TestAServiceThatIsNotThereIsAnErrorThatNamesIt(t *testing.T) {
	service := newFakeService(t, always(http.StatusOK, nil))
	embedder := service.embedder(EmbedderConfig{})
	service.server.Close()

	_, err := embedder.EmbedQueries(t.Context(), []string{"q"})

	if err == nil || !strings.Contains(err.Error(), "embeddings service") || !strings.Contains(err.Error(), service.server.URL) {
		t.Errorf("the error does not say which service could not be reached: %v", err)
	}
	if errors.Is(err, model.ErrDeclined) {
		t.Error("an unreachable service was taken for a decline")
	}
}

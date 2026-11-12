package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/retrieval"
)

// A small test set: a shop's slow checkout. Three documents and a link; two
// questions the graph answers, one of them held back, and one it does not.
const (
	graphJSON = `{
  "documents": [
    {"id": "doc:s:slow", "type": "issue", "source": "s", "url": "https://shop.test/issues/7", "title": "Checkout is slow",
     "summary": "Checkout p95 latency doubled on 3 March.", "created_at": "2026-03-04T09:00:00Z", "updated_at": "2026-03-05T09:00:00Z"},
    {"id": "doc:s:cache", "type": "pull_request", "source": "s", "url": "https://shop.test/pull/9", "title": "Remove the price cache",
     "summary": "The price cache was removed on 3 March. Every price is now read from the database.", "created_at": "2026-03-03T09:00:00Z", "updated_at": "2026-03-03T09:00:00Z"},
    {"id": "doc:s:rota", "type": "page", "source": "s", "url": "https://shop.test/wiki/rota", "title": "Holiday rota",
     "summary": "Lists who is away in August.", "created_at": "2025-01-01T00:00:00Z", "updated_at": "2025-01-01T00:00:00Z"}
  ],
  "topics": [],
  "edges": [
    {"from": "doc:s:cache", "kind": "links_to", "to": "doc:s:slow", "summary": "This change caused the slowdown."}
  ]
}`
	questionsJSON = `{
  "context": "The web shop's engineering documents.",
  "questions": [
    {"id": "why-slow", "question": "Why did checkout latency double in March?", "kind": "one", "part": "tune",
     "facts": [{"fact": "The price cache was removed.", "stated": [{"in": "doc:s:cache", "quote": "The price cache was removed on 3 March"}]}],
     "near": ["doc:s:slow"]},
    {"id": "when-slow", "question": "When did checkout latency double?", "kind": "one", "part": "check",
     "facts": [{"fact": "On 3 March.", "stated": [{"in": "doc:s:slow", "quote": "doubled on 3 March"}]}]},
    {"id": "rota-approver", "question": "Who approves the holiday rota?", "kind": "absent", "part": "tune",
     "near": ["doc:s:rota"], "note": "The rota lists who is away; nothing says who approves it."}
  ]
}`
)

const secret = "a-very-secret-key"

// world is everything a run of the program touches: a folder with the test
// set in it, a stand-in for Jev, and a stand-in for the embeddings service.
type world struct {
	t   *testing.T
	dir string
	env map[string]string

	jev      *httptest.Server
	mu       sync.Mutex
	requests int
	asked    []string // every question sent to Jev, with the state it was asked about
	keys     []string // the Authorization header of every request
	embedded int      // texts the embeddings service was asked for
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, dir: t.TempDir(), env: map[string]string{}}
	for name, content := range map[string]string{"graph.json": graphJSON, "questions.json": questionsJSON} {
		if err := os.WriteFile(filepath.Join(w.dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	w.jev = httptest.NewServer(http.HandlerFunc(w.serveJev))
	t.Cleanup(w.jev.Close)
	return w
}

// serveJev answers as System One would, judging by what it is shown: the
// document about the cache states the answer, the one about the checkout is
// on the subject and a little more, and evidence is enough if the cache
// document is in it.
func (w *world) serveJev(rw http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Model     string                     `json:"model"`
		State     json.RawMessage            `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		w.t.Errorf("a request to Jev is not JSON: %v", err)
	}

	answers := make(map[string]any)
	w.mu.Lock()
	w.requests++
	w.keys = append(w.keys, r.Header.Get("Authorization"))
	for name, question := range body.Questions {
		w.asked = append(w.asked, string(body.State)+string(question))

		var asked struct {
			Type         string `json:"type"`
			Instructions struct {
				Candidate struct {
					Title string `json:"title"`
				} `json:"candidate"`
				Evidence []struct {
					Title string `json:"title"`
				} `json:"evidence"`
			} `json:"instructions"`
		}
		json.Unmarshal(question, &asked)
		if asked.Type == "noul" {
			reading := 0.3
			for _, document := range asked.Instructions.Evidence {
				if document.Title == "Remove the price cache" {
					reading = 0.9
				}
			}
			answers[name] = map[string]any{"type": "noul", "noul": reading}
			continue
		}
		score := map[string]float64{"Remove the price cache": 1.9, "Checkout is slow": 1.6, "Holiday rota": 0.6}[asked.Instructions.Candidate.Title]
		answers[name] = map[string]any{"type": "score", "score": score, "confidence": 0.8, "probabilities": map[string]float64{"0": 0.1, "1": 0.2, "2": 0.7}}
	}
	w.mu.Unlock()

	json.NewEncoder(rw).Encode(map[string]any{
		"model":   body.Model,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 100 * len(body.Questions), "output_tokens": 5},
	})
}

func (w *world) sent() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.requests
}

// fakeEmbedder embeds a text as three numbers made from it.
type fakeEmbedder struct{ world *world }

func (e fakeEmbedder) Space(context.Context) (string, error) { return "test model; 3 dimensions", nil }

func (e fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	return e.embed(texts, 1), nil
}

func (e fakeEmbedder) EmbedQueries(_ context.Context, texts []string) ([][]float32, error) {
	return e.embed(texts, 2), nil
}

func (e fakeEmbedder) embed(texts []string, kind float32) [][]float32 {
	e.world.mu.Lock()
	defer e.world.mu.Unlock()
	e.world.embedded += len(texts)
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = []float32{float32(len(text)), float32(text[0]), kind}
	}
	return vectors
}

// run runs the program with the given arguments and returns what it printed
// and the error it ended with. Every run is pointed at this world's folder,
// Jev and embeddings service.
func (w *world) run(args ...string) (string, error) {
	w.t.Helper()
	var out bytes.Buffer
	p := &program{
		out:        &out,
		getenv:     func(name string) string { return w.env[name] },
		now:        func() time.Time { return time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC) },
		embedderAt: func(string) model.Embedder { return fakeEmbedder{world: w} },
	}
	args = append(args, "-set", w.dir)
	if args[0] == "record" {
		args = append(args, "-typesafe-url", w.jev.URL)
	}
	err := p.run(w.t.Context(), args)

	// Whatever else, the key is never said.
	said := out.String()
	if err != nil {
		said += err.Error()
	}
	if strings.Contains(said, secret) {
		w.t.Errorf("the program printed the key:\n%s", said)
	}
	return out.String(), err
}

// must runs the program and fails the test if it ends with an error.
func (w *world) must(args ...string) string {
	w.t.Helper()
	out, err := w.run(args...)
	if err != nil {
		w.t.Fatalf("grafsol-eval %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func wants(t *testing.T, out string, wanted ...string) {
	t.Helper()
	flat := strings.Join(strings.Fields(out), " ")
	for _, want := range wanted {
		if !strings.Contains(flat, want) {
			t.Errorf("the output does not say %q:\n%s", want, out)
		}
	}
}

func TestCheckSaysWhatTheSetHoldsAndWhatItLacks(t *testing.T) {
	w := newWorld(t)
	out := w.must("check")
	wants(t, out,
		"3 documents, 0 topics and 1 edges; 3 questions.",
		"2 to tune on, 1 held back to check on.",
		"By kind: 2 one, 1 absent.",
		"every quote is word for word where it is said to be",
		"Vectors: none yet.",
		"Tapes: none yet.",
	)

	// A label that names nothing is an error, and says which question.
	broken := strings.Replace(questionsJSON, "doubled on 3 March", "doubled on 4 March", 1)
	if err := os.WriteFile(filepath.Join(w.dir, "questions.json"), []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.run("check"); err == nil || !strings.Contains(err.Error(), "when-slow") {
		t.Errorf("a quote that is not in the graph was passed: %v", err)
	}
}

func TestEmbedAsksTheServiceOnce(t *testing.T) {
	w := newWorld(t)
	// Three documents and three questions.
	wants(t, w.must("embed"), "The service was asked for 6 texts. 6 vectors are on file", "Embedded with test model; 3 dimensions.")
	wants(t, w.must("embed"), "The service was asked for 0 texts. 6 vectors are on file")
	wants(t, w.must("check"), "Vectors: 6 on file, which is all the set needs.")

	// A reworded question needs one new vector, and the old one goes.
	reworded := strings.Replace(questionsJSON, "Who approves the holiday rota?", "Who signs off the holiday rota?", 1)
	if err := os.WriteFile(filepath.Join(w.dir, "questions.json"), []byte(reworded), 0o644); err != nil {
		t.Fatal(err)
	}
	wants(t, w.must("check"), "6 on file, and 1 texts of the set have none.")
	wants(t, w.must("embed"), "The service was asked for 1 texts. 6 vectors are on file", "1 that nothing uses any more were dropped.")
}

func TestRecordWithoutLiveSendsNothing(t *testing.T) {
	w := newWorld(t)
	// Before anything is embedded there is nothing to search with.
	if _, err := w.run("record", "-model", "jev-test-1"); err == nil || !strings.Contains(err.Error(), "grafsol-eval embed") {
		t.Errorf("recording with no vectors returned %v, want to be told to embed", err)
	}
	w.must("embed")

	// Fifteen leads (three documents, and the link from either end, for
	// three questions) and five labelled sets of evidence, each take.
	out := w.must("record", "-model", "jev-test-1", "-takes", "2")
	wants(t, out,
		"Take 1: the tape lacks 20 answers.",
		"Take 2: the tape lacks 20 answers.",
		"In all: 40 answers to ask Jev for",
		"The token count is a guess",
		"Not counted, for 2 takes: the readings of whatever evidence the searches arrive at.",
		"Nothing has been sent.",
		"grafsol-eval record -model jev-test-1 -takes 2 -live -budget",
	)
	if w.sent() != 0 {
		t.Errorf("recording without -live sent %d requests", w.sent())
	}
	if _, err := os.Stat(filepath.Join(w.dir, "tape")); err == nil {
		t.Error("recording without -live wrote a tape")
	}
}

func TestRecordLiveWantsAKeyABudgetAndAVersion(t *testing.T) {
	w := newWorld(t)
	w.must("embed")

	cases := map[string]struct {
		key  string
		args []string
		want string
	}{
		"no key":       {"", []string{"record", "-model", "jev-test-1", "-live", "-budget", "100000"}, "TYPESAFE_API_KEY is not set"},
		"no budget":    {secret, []string{"record", "-model", "jev-test-1", "-live"}, "give -live a budget"},
		"no model":     {secret, []string{"record", "-live", "-budget", "100000"}, "-model"},
		"an alias":     {secret, []string{"record", "-model", "jev-latest", "-live", "-budget", "100000"}, "alias"},
		"no takes":     {secret, []string{"record", "-model", "jev-test-1", "-takes", "0"}, "-takes"},
		"not a model":  {secret, []string{"record", "-model", "../somewhere", "-live", "-budget", "100000"}, "not the name of a model"},
		"no such id":   {secret, []string{"record", "-model", "jev-test-1", "-only", "no-such-question"}, "no-such-question"},
		"no such verb": {secret, []string{"rehearse"}, "there is no command"},
	}
	for name, c := range cases {
		w.env["TYPESAFE_API_KEY"] = c.key
		if _, err := w.run(c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: the program returned %v, want an error mentioning %q", name, err, c.want)
		}
	}
	if w.sent() != 0 {
		t.Errorf("%d requests were sent by runs that should all have been refused", w.sent())
	}
}

func TestRecordLiveThenReport(t *testing.T) {
	w := newWorld(t)
	w.env["TYPESAFE_API_KEY"] = secret
	w.must("embed")

	out := w.must("record", "-model", "jev-test-1", "-takes", "2", "-live", "-budget", "1000000")
	wants(t, out,
		"Take 1 of 2 is on the tape: every lead rated for 3 questions",
		"Take 2 of 2 is on the tape",
		"3 live searches were run, one per question",
		"Sent to Jev:",
		"now holds answers to",
		"over 2 takes.",
	)
	sent := w.sent()
	if sent == 0 {
		t.Fatal("recording with -live sent nothing")
	}
	// The key went to Jev, in the header, and nowhere else.
	for _, header := range w.keys {
		if header != "Bearer "+secret {
			t.Fatalf("a request carried the authorization %q", header)
		}
	}
	for _, kept := range []string{filepath.Join("tape", "jev-test-1.jsonl"), filepath.Join("results", "live-jev-test-1.json"), "vectors.json"} {
		content, err := os.ReadFile(filepath.Join(w.dir, kept))
		if err != nil {
			t.Fatalf("%s was not written: %v", kept, err)
		}
		if bytes.Contains(content, []byte(secret)) {
			t.Errorf("%s holds the key", kept)
		}
	}

	// Recording again pays for nothing: everything is on the tape, and
	// every question has had its live search.
	wants(t, w.must("record", "-model", "jev-test-1", "-takes", "2", "-live", "-budget", "1000000"),
		"Every question of this run has had its live search already.", "Sent to Jev: 0 requests, 0 input tokens")
	wants(t, w.must("record", "-model", "jev-test-1", "-takes", "2"), "There is nothing to record.")
	if w.sent() != sent {
		t.Errorf("recording what is already on the tape sent %d more requests", w.sent()-sent)
	}
	wants(t, w.must("check"), "Tape jev-test-1: answers to", "over 2 takes")

	// The report, from the tape alone.
	w.env["TYPESAFE_API_KEY"] = ""
	report := w.must("report", "-model", "jev-test-1")
	wants(t, report,
		"The part of the test set to tune on.",
		"2 questions (1 one, 1 absent) over 3 documents and 1 edges.",
		"Judge jev-test-1, 2 takes.",
		"SEARCHES",
		// The cache document is rated as evidence and the evidence reads as
		// enough, whatever the question: right for the question it answers,
		// wrong for the one nothing answers.
		"one 1 100% 2/2",
		"absent 1 0% 0/2 100% 2/2",
		"Right in all: 50% 2/4 of searches.",
		"WHAT WENT WRONG",
		"rota-approver (absent): stopped though the graph has no answer in 2 of 2 takes.",
		"RATINGS",
		"SUFFICIENCY",
		"OTHER THRESHOLDS",
		"REPLAYED AGAINST LIVE",
		"Of 2 live searches, 2 came out as most replays of the same question did",
	)
	if strings.Contains(report, "when-slow") {
		t.Errorf("the report of the part to tune on mentions a question that is held back:\n%s", report)
	}

	// The part held back, on request; and one question by name.
	wants(t, w.must("report", "-model", "jev-test-1", "-check"),
		"The part of the test set to check on.", "1 questions (1 one)", "Right in all: 100% 2/2 of searches.")
	wants(t, w.must("report", "-model", "jev-test-1", "-only", "rota-approver", "-around=false"),
		"1 questions (1 absent)", "Right in all: 0% 0/2 of searches.")

	// Kept, and set against.
	saved := w.must("report", "-model", "jev-test-1", "-save", "The first reading!")
	path := filepath.Join(w.dir, "results", "2026-11-12-the-first-reading.json")
	wants(t, saved, "is kept in "+path)
	wants(t, w.must("report", "-model", "jev-test-1", "-against", path),
		"SET AGAINST THE FIRST READING!", "Searches right: 50% 2/4 then, 50% 2/4 now.", "Ratings that moved: none.")

	if w.sent() != sent {
		t.Errorf("reporting sent %d requests to Jev", w.sent()-sent)
	}
}

func TestRecordStopsAtItsBudgetAndGoesOnFromThere(t *testing.T) {
	w := newWorld(t)
	w.env["TYPESAFE_API_KEY"] = secret
	w.must("embed")

	// Each question costs 100 tokens, and a take of this set is 20 of them
	// and more. A budget of 300 lets the first requests go and stops the
	// rest.
	out, err := w.run("record", "-model", "jev-test-1", "-takes", "1", "-live", "-budget", "300", "-live-searches=false")
	if err == nil || !strings.Contains(err.Error(), "the budget of 300 tokens is spent") {
		t.Fatalf("a recording over its budget ended with %v\n%s", err, out)
	}
	wants(t, out, "Sent to Jev:")
	first := w.sent()
	if first == 0 {
		t.Fatal("nothing was sent before the budget ran out")
	}
	// What was answered before it ran out is on the tape.
	wants(t, w.must("check"), "Tape jev-test-1: answers to 15 questions over 1 take")

	// With room to finish, it finishes, and asks for nothing twice.
	w.must("record", "-model", "jev-test-1", "-takes", "1", "-live", "-budget", "1000000", "-live-searches=false")
	wants(t, w.must("record", "-model", "jev-test-1", "-takes", "1"), "There is nothing to record.")
	w.mu.Lock()
	asked := slices.Clone(w.asked)
	w.mu.Unlock()
	slices.Sort(asked)
	if unique := slices.Compact(slices.Clone(asked)); len(unique) != len(asked) {
		t.Errorf("%d questions were sent to Jev, of which only %d were different", len(asked), len(unique))
	}
	if w.sent() <= first {
		t.Error("the second run sent nothing, though the first had not finished")
	}
}

// A recording that fails part of the way through a stage keeps what Jev had
// answered by then: it was paid for.
func TestWhatWasAnsweredBeforeAFailureIsKept(t *testing.T) {
	w := newWorld(t)
	w.env["TYPESAFE_API_KEY"] = secret
	w.must("embed")

	// The fifteen ratings cost 1,500 tokens. A budget of 1,600 lets them
	// all through, and then a reading or more before it is spent.
	if _, err := w.run("record", "-model", "jev-test-1", "-takes", "1", "-live", "-budget", "1600", "-live-searches=false"); err == nil {
		t.Fatal("a recording over its budget ended without an error")
	}
	out := w.must("check")
	wants(t, out, "Tape jev-test-1: answers to")
	if strings.Contains(strings.Join(strings.Fields(out), " "), "answers to 15 questions") {
		t.Errorf("the readings answered before the budget ran out were not kept:\n%s", out)
	}
}

// failing is a transport that answers nothing, and counts how often it was
// asked.
type failing struct {
	mu    sync.Mutex
	calls int
}

func (f *failing) RoundTrip(request *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return nil, io.ErrUnexpectedEOF
}

// A judge that answers from a tape asks once. What the tape lacks it will
// lack a moment later too, so asking again would only be waiting.
func TestAJudgeOverATapeDoesNotAskAgain(t *testing.T) {
	transport := new(failing)
	b := &bench{model: "jev-test-1"}
	_, err := b.judge(transport, "").Assess(t.Context(), retrieval.Question{Text: "Why?"},
		nil, []retrieval.Lead{{Target: &graph.Document{ID: "doc:s:slow", Title: "Checkout is slow"}}})
	if err == nil {
		t.Fatal("a judge whose transport fails returned no error")
	}
	if transport.calls != 1 {
		t.Errorf("the transport was asked %d times, want once", transport.calls)
	}
}

func TestReportOfAnEmptyTapeSaysToRecord(t *testing.T) {
	w := newWorld(t)
	w.must("embed")
	if _, err := w.run("report", "-model", "jev-test-1"); err == nil || !strings.Contains(err.Error(), "the tape of jev-test-1 is empty") {
		t.Errorf("a report of an empty tape returned %v", err)
	}
	if _, err := w.run("report"); err == nil || !strings.Contains(err.Error(), "-model") {
		t.Errorf("a report with no model returned %v", err)
	}

	// A tape that covers only some of the questions cannot be reported on
	// in full, and says so.
	w.env["TYPESAFE_API_KEY"] = secret
	w.must("record", "-model", "jev-test-1", "-takes", "1", "-only", "why-slow", "-live", "-budget", "1000000")
	if _, err := w.run("report", "-model", "jev-test-1"); err == nil || !strings.Contains(err.Error(), "does not hold everything") {
		t.Errorf("a report of a tape that lacks two questions returned %v", err)
	}
	wants(t, w.must("report", "-model", "jev-test-1", "-only", "why-slow"), "1 questions (1 one)", "Right in all: 100% 1/1 of searches.")
}

func TestNumbersAreWrittenWithTheirThousandsMarked(t *testing.T) {
	for n, want := range map[int]string{0: "0", 7: "7", 999: "999", 1000: "1,000", 21400: "21,400", 1234567: "1,234,567", -1500: "-1,500"} {
		if got := number(n); got != want {
			t.Errorf("number(%d) is %q, want %q", n, got, want)
		}
	}
}

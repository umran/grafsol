package evaluation

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Tape holds answers that Jev gave, each filed under exactly what was asked.
//
// Jev answers every question in a request on its own: an answer depends on
// the model, on the state the request carries, and on that one question, and
// on nothing else in the request. So a question can be known by a
// fingerprint of those three, and whenever the same three come up again, in
// whatever request and under whatever name, the answer on the tape is the
// answer.
//
// Jev does not answer a question quite the same way twice, so a tape holds
// several takes: each take has at most one answer to a question, asked on
// its own occasion. Measuring over every take shows what is the question's
// doing and what is chance.
//
// A tape works underneath the TypeSafe client, as the transport of its HTTP
// client (typesafe.Config.HTTPClient). Record returns a transport that asks
// Jev for what the tape lacks and keeps the answers; Replay returns one that
// answers from the tape alone. The judge and the search above them run the
// same code either way, which is the point: what is replayed is what would
// be asked for real, word for word, or it is not found.
//
// The tape keeps the answers and not the questions, and never sees the key:
// that travels in a header, and the tape reads bodies. A Tape is safe for
// concurrent use.
type Tape struct {
	path string

	mu      sync.Mutex
	answers map[string][]json.RawMessage // by fingerprint; [take-1], nil where a take has none
	asked   tally                        // what recording has cost over the life of the tape
}

var (
	// ErrNotOnTape reports that a replay was asked something the tape has
	// no answer to in that take. The remedy is to record it.
	ErrNotOnTape = errors.New("evaluation: not on the tape")

	// ErrBudgetSpent reports that a Meter let a request go no further,
	// because what has been spent has reached its budget.
	ErrBudgetSpent = errors.New("evaluation: the budget is spent")
)

// tally counts what was sent to Jev and what it cost.
type tally struct {
	Requests  int `json:"requests"`
	Questions int `json:"questions"`
	Bytes     int `json:"bytes"`        // of request bodies
	Tokens    int `json:"input_tokens"` // as Jev counted them; these are what is billed
}

// The shape of the file: a header line, then one line per question, in the
// order of the fingerprints so that the file changes only where an answer
// did.
type tapeHeader struct {
	Tape int `json:"tape"` // the version of this format
	tally
}

type tapeLine struct {
	Question string            `json:"q"`
	Answers  []json.RawMessage `json:"a"` // one per take; null where a take has none
}

const tapeVersion = 1

// OpenTape reads the tape in a file. A file that does not exist yet is an
// empty tape, which Save will create.
func OpenTape(path string) (*Tape, error) {
	t := &Tape{path: path, answers: make(map[string][]json.RawMessage)}

	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evaluation: %w", err)
	}
	defer file.Close()
	fail := func(line int, format string, args ...any) (*Tape, error) {
		return nil, fmt.Errorf("evaluation: %s, line %d: %s", filepath.Base(path), line, fmt.Sprintf(format, args...))
	}

	lines := bufio.NewScanner(file)
	lines.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for number := 1; lines.Scan(); number++ {
		if number == 1 {
			var header tapeHeader
			if err := json.Unmarshal(lines.Bytes(), &header); err != nil {
				return fail(number, "%v", err)
			}
			if header.Tape != tapeVersion {
				return fail(number, "this is not a tape of version %d", tapeVersion)
			}
			t.asked = header.tally
			continue
		}
		var line tapeLine
		if err := json.Unmarshal(lines.Bytes(), &line); err != nil {
			return fail(number, "%v", err)
		}
		if line.Question == "" {
			return fail(number, "an answer is filed under nothing")
		}
		if _, twice := t.answers[line.Question]; twice {
			return fail(number, "the question %s is on the tape twice", line.Question)
		}
		for i, answer := range line.Answers {
			if string(answer) == "null" {
				line.Answers[i] = nil
			}
		}
		t.answers[line.Question] = line.Answers
	}
	if err := lines.Err(); err != nil {
		return nil, fmt.Errorf("evaluation: reading %s: %w", filepath.Base(path), err)
	}
	return t, nil
}

// Save writes the tape to the file it was opened from.
func (t *Tape) Save() error {
	t.mu.Lock()
	keys := make([]string, 0, len(t.answers))
	for key := range t.answers {
		keys = append(keys, key)
	}
	slices.Sort(keys)

	var content bytes.Buffer
	encoder := json.NewEncoder(&content) // one value to a line
	err := encoder.Encode(tapeHeader{Tape: tapeVersion, tally: t.asked})
	for _, key := range keys {
		if err == nil {
			err = encoder.Encode(tapeLine{Question: key, Answers: t.answers[key]})
		}
	}
	t.mu.Unlock()
	if err != nil {
		return fmt.Errorf("evaluation: encoding the tape: %w", err)
	}
	return writeFile(t.path, content.Bytes())
}

// Len returns how many different questions the tape has an answer to, in any
// take.
func (t *Tape) Len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.answers)
}

// Answers returns how many questions have an answer in the given take.
func (t *Tape) Answers(take int) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, takes := range t.answers {
		if take >= 1 && take <= len(takes) && takes[take-1] != nil {
			count++
		}
	}
	return count
}

// Takes returns the number of the last take that holds any answer: zero for
// an empty tape.
func (t *Tape) Takes() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	last := 0
	for _, takes := range t.answers {
		for i, answer := range takes {
			if answer != nil {
				last = max(last, i+1)
			}
		}
	}
	return last
}

// Asked returns what recording has cost over the life of the tape: how many
// questions were sent to Jev, and how many input tokens Jev counted for them.
func (t *Tape) Asked() (questions, tokens int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.asked.Questions, t.asked.Tokens
}

// BytesPerToken returns how many bytes of request went to Jev for each token
// it counted, over everything recorded so far, and whether anything has been
// recorded to tell from. It turns the size of a request that has not been
// sent into a guess at what it will cost.
func (t *Tape) BytesPerToken() (ratio float64, known bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.asked.Tokens == 0 {
		return 0, false
	}
	return float64(t.asked.Bytes) / float64(t.asked.Tokens), true
}

// lookup finds the tape's answers, in one take, to the questions of a
// request. It returns the answers found, by the names they were asked under,
// and the fingerprints of the questions that have none.
func (t *Tape) lookup(asked *asking, take int) (found map[string]json.RawMessage, lacking []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	found = make(map[string]json.RawMessage, len(asked.names))
	for _, name := range asked.names {
		key := asked.keys[name]
		takes := t.answers[key]
		if take < 1 || take > len(takes) || takes[take-1] == nil {
			lacking = append(lacking, key)
			continue
		}
		found[name] = takes[take-1]
	}
	return found, lacking
}

// keep files the answers to a request under one take. A question that
// already has an answer in the take keeps it: a take is one asking of each
// question, and whatever comes back from asking again is set aside.
func (t *Tape) keep(asked *asking, take int, answers map[string]json.RawMessage, cost tally) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, name := range asked.names {
		answer, answered := answers[name]
		if !answered {
			continue // the caller will find it missing, and say so
		}
		key := asked.keys[name]
		takes := t.answers[key]
		for len(takes) < take {
			takes = append(takes, nil)
		}
		if takes[take-1] == nil {
			takes[take-1] = withoutLegend(answer)
		}
		t.answers[key] = takes
	}
	t.asked.Requests += cost.Requests
	t.asked.Questions += cost.Questions
	t.asked.Bytes += cost.Bytes
	t.asked.Tokens += cost.Tokens
}

// --- recording ----------------------------------------------------------------

// Record returns a transport for take number take (1 is the first) that
// answers from the tape where the tape has every answer a request wants, and
// otherwise sends the request on through next and keeps what comes back.
//
// Because what is already on the tape is not asked for again, a recording
// that was interrupted can simply be run again: it pays only for what it did
// not get to. A request that mixes questions on the tape with new ones goes
// out whole, and the answers already taken stand.
//
// A reply that is not a success is passed back as it came and nothing is
// kept, so the caller's own handling of errors (retries, splitting a request
// that was too long) works as it does without a tape.
func (t *Tape) Record(take int, next http.RoundTripper) http.RoundTripper {
	return &recorder{tape: t, take: take, next: next}
}

type recorder struct {
	tape *Tape
	take int
	next http.RoundTripper
}

func (r *recorder) RoundTrip(request *http.Request) (*http.Response, error) {
	asked, err := readAsking(request)
	if err != nil {
		return nil, err
	}
	if found, lacking := r.tape.lookup(asked, r.take); len(lacking) == 0 {
		return asked.reply(request, asked.model, found, 0), nil
	}

	response, content, err := send(r.next, request, asked.body)
	if err != nil || response.StatusCode != http.StatusOK {
		return response, err
	}
	var got struct {
		Model   string                     `json:"model"`
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(content, &got); err != nil {
		return nil, fmt.Errorf("evaluation: the reply to a recorded request is not what System One sends: %w", err)
	}

	r.tape.keep(asked, r.take, got.Answers, tally{
		Requests: 1, Questions: len(asked.names), Bytes: len(asked.body), Tokens: got.Usage.InputTokens,
	})
	// The caller is given what the tape now holds, which is what a replay
	// will give it: the two can never differ.
	found, _ := r.tape.lookup(asked, r.take)
	return asked.reply(request, got.Model, found, got.Usage.InputTokens), nil
}

// --- replaying ----------------------------------------------------------------

// Replay returns a transport that answers from one take of the tape and
// never reaches anything. A request that asks a question the take has no
// answer to fails with an error that wraps ErrNotOnTape: nothing is made up,
// and nothing is skipped.
//
// The client that uses it should not retry (typesafe.Config.MaxRetries of
// -1): what is not on the tape will not be there a moment later.
func (t *Tape) Replay(take int) *Replay {
	return &Replay{tape: t, take: take, missed: make(map[string]bool)}
}

// Replay is the transport that Tape.Replay returns.
type Replay struct {
	tape *Tape
	take int

	mu     sync.Mutex
	missed map[string]bool
}

func (r *Replay) RoundTrip(request *http.Request) (*http.Response, error) {
	asked, err := readAsking(request)
	if err != nil {
		return nil, err
	}
	found, lacking := r.tape.lookup(asked, r.take)
	if len(lacking) > 0 {
		r.mu.Lock()
		for _, key := range lacking {
			r.missed[key] = true
		}
		r.mu.Unlock()
		return nil, fmt.Errorf("%w: take %d has no answer to %d of the %d questions in a request",
			ErrNotOnTape, r.take, len(lacking), len(asked.names))
	}
	return asked.reply(request, asked.model, found, 0), nil
}

// Missed returns how many different questions the replay was asked that the
// take had no answer to.
func (r *Replay) Missed() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.missed)
}

// --- rehearsing ---------------------------------------------------------------

// Rehearse returns a transport for finding out what recording a take would
// send, without sending it. It answers from the tape where it can and makes
// up a neutral answer where it cannot, so that whatever is asking runs to
// its end, and it counts what a recording would have had to ask for.
//
// The answers it makes up are worth nothing. Use what a rehearsal counted,
// never what the code above it concluded.
func (t *Tape) Rehearse(take int) *Rehearsal {
	return &Rehearsal{tape: t, take: take, seen: make(map[string]bool)}
}

// Rehearsal is the transport that Tape.Rehearse returns.
type Rehearsal struct {
	tape *Tape
	take int

	mu      sync.Mutex
	seen    map[string]bool // questions a recording would have taped by now
	lacking Lacking
}

// Lacking is what a recording would have to send to Jev.
type Lacking struct {
	Requests  int // requests that would go out
	Questions int // questions the tape has no answer to
	Bytes     int // the size of those requests' bodies
}

func (r *Rehearsal) RoundTrip(request *http.Request) (*http.Response, error) {
	asked, err := readAsking(request)
	if err != nil {
		return nil, err
	}
	found, lacking := r.tape.lookup(asked, r.take)

	r.mu.Lock()
	fresh := 0
	for _, key := range lacking {
		if !r.seen[key] {
			r.seen[key] = true
			fresh++
		}
	}
	if fresh > 0 {
		// A recording would send this request, whole.
		r.lacking.Requests++
		r.lacking.Questions += fresh
		r.lacking.Bytes += len(asked.body)
	}
	r.mu.Unlock()

	for _, name := range asked.names {
		if _, answered := found[name]; !answered {
			found[name] = standIn(asked.kinds[name])
		}
	}
	return asked.reply(request, asked.model, found, 0), nil
}

// Lacking returns what the rehearsal has counted so far.
func (r *Rehearsal) Lacking() Lacking {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lacking
}

// standIn is an answer of the right kind that says nothing either way.
func standIn(kind string) json.RawMessage {
	switch kind {
	case "noul":
		return json.RawMessage(`{"type":"noul","noul":0.5}`)
	case "score":
		return json.RawMessage(`{"type":"score","score":0}`)
	}
	return json.RawMessage(`{"type":` + string(mustJSON(kind)) + `}`)
}

// --- metering -----------------------------------------------------------------

// Meter counts what requests to Jev cost, and stops them once a budget is
// spent. Put it beneath a recorder: what the tape answers never reaches the
// meter, so it counts only what is really sent.
//
// The cost of a request is known only when its answer arrives. The meter
// therefore lets a request through while anything of the budget is left, and
// refuses every request after the one that used it up. Spending can pass the
// budget by the requests that were on their way at that moment, and by no
// more.
type Meter struct {
	budget int

	mu       sync.Mutex
	requests int
	tokens   int
}

// NewMeter returns a meter with a budget of that many input tokens.
func NewMeter(budget int) *Meter {
	return &Meter{budget: budget}
}

// Spent returns how many requests the meter has let through and answered,
// and how many input tokens Jev counted for them.
func (m *Meter) Spent() (requests, tokens int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.requests, m.tokens
}

// Transport returns a transport that sends through next while the budget
// lasts. Once it is spent, a request fails with an error that wraps
// ErrBudgetSpent and is not sent.
func (m *Meter) Transport(next http.RoundTripper) http.RoundTripper {
	return &metered{meter: m, next: next}
}

type metered struct {
	meter *Meter
	next  http.RoundTripper
}

func (m *metered) RoundTrip(request *http.Request) (*http.Response, error) {
	body, err := drain(request)
	if err != nil {
		return nil, err
	}
	m.meter.mu.Lock()
	spent := m.meter.tokens >= m.meter.budget
	m.meter.mu.Unlock()
	if spent {
		return nil, fmt.Errorf("%w: %d tokens were allowed", ErrBudgetSpent, m.meter.budget)
	}

	response, content, err := send(m.next, request, body)
	if err != nil || response.StatusCode != http.StatusOK {
		return response, err
	}
	var got struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	// A reply that cannot be read costs nothing that can be counted; the
	// caller will find out what is wrong with it soon enough.
	_ = json.Unmarshal(content, &got)
	m.meter.mu.Lock()
	m.meter.requests++
	m.meter.tokens += got.Usage.InputTokens
	m.meter.mu.Unlock()
	return response, nil
}

// --- what is asked, and what is answered ---------------------------------------

// asking is a System One request, taken apart.
type asking struct {
	body  []byte
	model string
	names []string          // the names the questions were asked under, sorted
	keys  map[string]string // by name: the question's fingerprint
	kinds map[string]string // by name: "noul", "score" or "choice"
}

// readAsking reads a request's body and works out what each of its questions
// is filed under.
func readAsking(request *http.Request) (*asking, error) {
	body, err := drain(request)
	if err != nil {
		return nil, err
	}
	var sent struct {
		Model     string                     `json:"model"`
		State     json.RawMessage            `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(body, &sent); err != nil {
		return nil, fmt.Errorf("evaluation: a request is not what the System One API takes: %w", err)
	}

	asked := &asking{
		body:  body,
		model: sent.Model,
		keys:  make(map[string]string, len(sent.Questions)),
		kinds: make(map[string]string, len(sent.Questions)),
	}
	state, err := canonical(sent.State)
	if err != nil {
		return nil, fmt.Errorf("evaluation: the state of a request: %w", err)
	}
	for name, raw := range sent.Questions {
		question, err := canonical(raw)
		if err != nil {
			return nil, fmt.Errorf("evaluation: the question %q of a request: %w", name, err)
		}
		asked.names = append(asked.names, name)
		asked.keys[name] = fingerprint(sent.Model, state, question)

		var kind struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &kind)
		asked.kinds[name] = kind.Type
	}
	slices.Sort(asked.names)
	return asked, nil
}

// fingerprint is what an answer is filed under: everything the answer
// depends on, and nothing it does not. The name a question is asked under is
// left out, and so is whatever else is in the request with it.
func fingerprint(model string, state, question []byte) string {
	hash := sha256.New()
	for _, part := range [][]byte{[]byte(model), state, question} {
		hash.Write(part)
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)[:16])
}

// canonical returns a JSON value written one fixed way: no spare space, and
// the fields of every object in order. Two requests that say the same thing
// then have the same fingerprint, however each happened to be written out.
func canonical(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // a number stays as it was written
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// reply builds the answer to a request, as System One would send it, from
// answers that are already in hand.
func (a *asking) reply(request *http.Request, model string, answers map[string]json.RawMessage, tokens int) *http.Response {
	content := mustJSON(map[string]any{
		"model":   model,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": tokens, "output_tokens": 0},
	})
	return &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": {"application/json"}},
		Body:          io.NopCloser(bytes.NewReader(content)),
		ContentLength: int64(len(content)),
		Request:       request,
	}
}

// withoutLegend returns an answer as Jev gave it, less its legend. The
// legend repeats the criteria of the question, word for word, in every
// answer: it would be most of the tape, and says nothing the question did
// not.
func withoutLegend(answer json.RawMessage) json.RawMessage {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(answer, &fields); err != nil {
		return answer // not an object: kept as it came
	}
	delete(fields, "legend")
	return mustJSON(fields)
}

// drain reads the whole body of a request and closes it, as a transport
// must.
func drain(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	defer request.Body.Close()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, fmt.Errorf("evaluation: reading a request: %w", err)
	}
	return body, nil
}

// send passes a request, whose body has been read already, on through next,
// and reads the whole reply. The response it returns has its body in place
// again, for whoever reads it next.
func send(next http.RoundTripper, request *http.Request, body []byte) (*http.Response, []byte, error) {
	forward := request.Clone(request.Context())
	forward.Body = io.NopCloser(bytes.NewReader(body))
	forward.ContentLength = int64(len(body))

	response, err := next.RoundTrip(forward)
	if err != nil {
		return nil, nil, err
	}
	content, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("evaluation: reading a reply: %w", err)
	}
	response.Body = io.NopCloser(bytes.NewReader(content))
	return response, content, nil
}

// mustJSON encodes a value that cannot fail to encode.
func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("evaluation: encoding %T: %v", value, err))
	}
	return encoded
}

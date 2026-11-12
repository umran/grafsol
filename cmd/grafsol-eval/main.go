// Command grafsol-eval measures the judge and the search against the test set
// in /eval: questions whose answers are known, asked over a frozen copy of a
// graph. Package internal/evaluation explains the method; this is the program
// that puts it to work.
//
//	grafsol-eval check                      load the test set and check every label and quote
//	grafsol-eval embed                      ask the local embeddings service for the vectors the set lacks
//	grafsol-eval record -model jev-1.14.0   say what the tape lacks, and what asking Jev for it would cost
//	grafsol-eval record -model jev-1.14.0 -live -budget 12000000
//	                                        ask Jev for it
//	grafsol-eval report -model jev-1.14.0   replay the tape and report
//
// Only "record -live" sends anything to TypeSafe, and it needs
// TYPESAFE_API_KEY in the environment and a budget, in input tokens, that it
// will not go on past. Everything else works from files and costs nothing:
// "report" can be run as often as the thresholds, or the search itself, are
// changed.
//
// What "record -live" sends is the test set's questions and their contexts,
// and the titles, summaries, URLs and link notes of the documents in
// eval/graph.json. The key travels in a header and is never printed or
// written down.
//
// "embed" needs the module's embeddings service running on this machine (see
// /inference): once, and again whenever a question or a summary is reworded.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/internal/evaluation"
	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/model/inference"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/retrieval/jev"
	"github.com/umran/grafsol/storage/memory"
	"github.com/umran/grafsol/typesafe"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	p := &program{
		out:    os.Stdout,
		getenv: os.Getenv,
		now:    time.Now,
		embedderAt: func(address string) model.Embedder {
			return inference.NewEmbedder(inference.EmbedderConfig{BaseURL: address})
		},
	}
	if err := p.run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "grafsol-eval:", err)
		os.Exit(1)
	}
}

// program is one run of the command. Its fields are what a test replaces.
type program struct {
	out        io.Writer
	getenv     func(string) string
	now        func() time.Time
	embedderAt func(address string) model.Embedder
}

const usage = `usage: grafsol-eval <command> [flags]

  check     load the test set and check every label and quote
  embed     ask the local embeddings service for the vectors the set lacks
  record    say what the tape lacks; with -live, ask Jev for it
  report    replay the tape and report

Run a command with -h for its flags.`

func (p *program) run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch command, rest := args[0], args[1:]; command {
	case "check":
		return p.check(ctx, rest)
	case "embed":
		return p.embed(ctx, rest)
	case "record":
		return p.record(ctx, rest)
	case "report":
		return p.report(ctx, rest)
	default:
		return fmt.Errorf("there is no command %q\n\n%s", command, usage)
	}
}

func (p *program) say(format string, args ...any) {
	fmt.Fprintf(p.out, format+"\n", args...)
}

// --- the files of a test set ------------------------------------------------------

// files names the files that go with the test set in a folder.
type files struct{ dir string }

func (f files) vectors() string { return filepath.Join(f.dir, "vectors.json") }

// tape is where the answers of one model are kept.
func (f files) tape(model string) string { return filepath.Join(f.dir, "tape", model+".jsonl") }

// live is where the searches asked of one model directly are kept.
func (f files) live(model string) string {
	return filepath.Join(f.dir, "results", "live-"+model+".json")
}

// saved is where the gist of a run is kept under a label.
func (f files) saved(when time.Time, label string) string {
	words := strings.FieldsFunc(strings.ToLower(label), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	})
	return filepath.Join(f.dir, "results", when.Format("2006-01-02")+"-"+strings.Join(words, "-")+".json")
}

// A model's name becomes part of a file's name, and it has to be the name of
// one version: an alias such as "jev-latest" moves to a new model without
// notice, and answers from two models on one tape could never be told apart.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func checkModel(name string) error {
	switch {
	case name == "":
		return errors.New("name the model with -model, as one version, such as jev-1.14.0")
	case !modelName.MatchString(name):
		return fmt.Errorf("%q is not the name of a model", name)
	case name == typesafe.DefaultModel || strings.HasSuffix(name, "-latest"):
		return fmt.Errorf("%q is an alias that moves from one model to the next; name the version, such as jev-1.14.0", name)
	}
	return nil
}

// --- check ----------------------------------------------------------------------------

func (p *program) check(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	dir := flags.String("set", "eval", "the `folder` that holds the test set")
	if err := flags.Parse(args); err != nil {
		return err
	}
	where := files{*dir}

	set, err := evaluation.Load(where.dir)
	if err != nil {
		return err
	}
	p.describe(where, set)
	p.say("Every label names something in the graph, and every quote is word for word where it is said to be.")

	vectors, err := evaluation.OpenVectors(where.vectors())
	if err != nil {
		return err
	}
	lacking, err := vectors.Lacking(ctx, set)
	if err != nil {
		return err
	}
	p.say("")
	switch {
	case vectors.Len() == 0:
		p.say("Vectors: none yet. Run `grafsol-eval embed` with the embeddings service running.")
	case lacking > 0:
		p.say("Vectors: %s on file, and %s texts of the set have none. Run `grafsol-eval embed`.", number(vectors.Len()), number(lacking))
	default:
		p.say("Vectors: %s on file, which is all the set needs. Made by %s.", number(vectors.Len()), vectors.Space())
	}

	tapes, err := filepath.Glob(filepath.Join(where.dir, "tape", "*.jsonl"))
	if err != nil {
		return err
	}
	if len(tapes) == 0 {
		p.say("Tapes: none yet. `grafsol-eval record -model <version>` says what recording one would ask of Jev.")
	}
	for _, path := range tapes {
		tape, err := evaluation.OpenTape(path)
		if err != nil {
			return err
		}
		questions, tokens := tape.Asked()
		p.say("Tape %s: answers to %s questions over %s; %s questions were sent to Jev for it, for %s tokens.",
			strings.TrimSuffix(filepath.Base(path), ".jsonl"), number(tape.Len()), takes(tape.Takes()), number(questions), number(tokens))
	}
	return nil
}

// describe says what a test set holds.
func (p *program) describe(where files, set evaluation.Set) {
	kinds, parts := make(map[evaluation.Kind]int), make(map[evaluation.Part]int)
	var notes []string
	for _, q := range set.Questions {
		kinds[q.Kind]++
		parts[q.Part]++
		if q.NeedsNote() {
			notes = append(notes, q.ID)
		}
	}
	var byKind []string
	for _, kind := range []evaluation.Kind{evaluation.OneDocument, evaluation.Linked, evaluation.Several, evaluation.BothRepositories,
		evaluation.Absent, evaluation.Nowhere, evaluation.Lost} {
		if kinds[kind] > 0 {
			byKind = append(byKind, fmt.Sprintf("%d %s", kinds[kind], kind))
		}
	}
	p.say("The test set in %s: %s documents, %s topics and %s edges; %d questions.",
		where.dir, number(len(set.Graph.Documents)), number(len(set.Graph.Topics)), number(len(set.Graph.Edges)), len(set.Questions))
	p.say("  %d to tune on, %d held back to check on.", parts[evaluation.Tune], parts[evaluation.Check])
	p.say("  By kind: %s.", strings.Join(byKind, ", "))
	if len(notes) > 0 {
		p.say("  %d need a link's note to be answered: %s.", len(notes), strings.Join(notes, ", "))
	}
}

// --- embed ----------------------------------------------------------------------------

func (p *program) embed(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("embed", flag.ContinueOnError)
	dir := flags.String("set", "eval", "the `folder` that holds the test set")
	address := flags.String("embeddings", "http://127.0.0.1:3010", "the `address` of the embeddings service")
	if err := flags.Parse(args); err != nil {
		return err
	}
	where := files{*dir}

	set, err := evaluation.Load(where.dir)
	if err != nil {
		return err
	}
	vectors, err := evaluation.OpenVectors(where.vectors())
	if err != nil {
		return err
	}
	before := vectors.Space()

	asked, err := vectors.Record(ctx, set, p.embedderAt(*address))
	if err != nil {
		return fmt.Errorf("%w (is the embeddings service running at %s? see /inference)", err, *address)
	}
	dropped := vectors.Prune()
	if err := vectors.Save(); err != nil {
		return err
	}

	if before != "" && before != vectors.Space() {
		p.say("The vectors on file were made by another model (%s). All of them have been made again.", before)
	}
	p.say("Embedded with %s.", vectors.Space())
	p.say("The service was asked for %s texts. %s vectors are on file in %s; %s that nothing uses any more were dropped.",
		number(asked), number(vectors.Len()), where.vectors(), number(dropped))
	return nil
}

// --- what record and report share ---------------------------------------------------

// bench is a test set ready to be measured: its graph in a store, its vectors
// at hand, and the tape of one model.
type bench struct {
	whole   evaluation.Set // every question
	set     evaluation.Set // the questions this run is about
	search  retrieval.Config
	tape    *evaluation.Tape
	model   string
	baseURL string
}

// open loads the test set, its vectors and a model's tape, and puts the
// frozen graph in a store.
func (p *program) open(ctx context.Context, where files, model, only string) (*bench, error) {
	if err := checkModel(model); err != nil {
		return nil, err
	}
	whole, err := evaluation.Load(where.dir)
	if err != nil {
		return nil, err
	}
	set := whole
	if only != "" {
		if set, err = whole.Only(strings.Split(only, ",")...); err != nil {
			return nil, err
		}
	}

	vectors, err := evaluation.OpenVectors(where.vectors())
	if err != nil {
		return nil, err
	}
	store := memory.New()
	if err := evaluation.Fill(ctx, whole, store, memory.NewLedger(), vectors.Embedder()); err != nil {
		return nil, needsEmbedding(err)
	}
	tape, err := evaluation.OpenTape(where.tape(model))
	if err != nil {
		return nil, err
	}
	return &bench{
		whole:  whole,
		set:    set,
		search: retrieval.Config{Store: store, Searcher: store, Neighbors: store, Embedder: vectors.Embedder()},
		tape:   tape,
		model:  model,
	}, nil
}

// needsEmbedding explains an error that comes of a missing vector.
func needsEmbedding(err error) error {
	if errors.Is(err, evaluation.ErrNoVector) {
		return fmt.Errorf("%w\nThe vectors file does not cover the test set as it now stands. Run `grafsol-eval embed`.", err)
	}
	return err
}

// judge returns the Jev judge, asking through the given transport.
//
// A judge that replays or rehearses is given a key that is no key: its
// transport answers every request itself, so nothing is ever sent, and it
// does not retry, since what a tape lacks it will lack a moment later too.
func (b *bench) judge(transport http.RoundTripper, key string) retrieval.Judge {
	cfg := typesafe.Config{
		APIKey:     key,
		BaseURL:    b.baseURL,
		HTTPClient: &http.Client{Transport: transport, Timeout: 3 * time.Minute},
	}
	if key == "" {
		cfg.APIKey, cfg.MaxRetries = "none: answered from the tape", -1
	}
	return jev.New(jev.Config{Client: typesafe.New(cfg), Model: b.model})
}

// settings is the thresholds a report tries: the ones in use first, then the
// ones a step to each side.
func settings() []evaluation.Settings {
	inUse := evaluation.DefaultSettings
	all := []evaluation.Settings{inUse}
	for _, s := range evaluation.Around(inUse, 0.1) {
		if s != inUse {
			all = append(all, s)
		}
	}
	return all
}

// arrivals finds, from the ratings on the tape, every set of evidence that a
// search for each question could come to weigh under any of the settings.
func (b *bench) arrivals(ctx context.Context, replaying retrieval.Judge) (map[string][][]graph.ID, error) {
	met := make(map[string][][]graph.ID)
	cfg := b.search
	cfg.Judge = replaying
	for _, path := range evaluation.Paths(settings()) {
		found, err := evaluation.Arrivals(ctx, path.Apply(cfg), b.set)
		if err != nil {
			return nil, err
		}
		for id, sets := range found {
			for _, evidence := range sets {
				if !slices.ContainsFunc(met[id], func(known []graph.ID) bool { return slices.Equal(known, evidence) }) {
					met[id] = append(met[id], evidence)
				}
			}
		}
	}
	return met, nil
}

// --- record ---------------------------------------------------------------------------

func (p *program) record(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("record", flag.ContinueOnError)
	dir := flags.String("set", "eval", "the `folder` that holds the test set")
	model := flags.String("model", "", "the model to record, as one `version`, such as jev-1.14.0")
	count := flags.Int("takes", 5, "how many `takes` the tape should hold: each is one asking of every question")
	only := flags.String("only", "", "record only these questions: their `IDs`, separated by commas")
	live := flags.Bool("live", false, "send to TypeSafe what the tape lacks; without it nothing is sent")
	budget := flags.Int("budget", 0, "with -live: the most input `tokens` this run may spend")
	liveSearches := flags.Bool("live-searches", true, "with -live: also ask one search per question of Jev directly, to check that replaying tells the truth")
	price := flags.Float64("price", 0.042, "what a million input tokens cost, in `dollars`, for the estimate")
	baseURL := flags.String("typesafe-url", "", "the `address` of the System One API, if not the usual one")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *count < 1 {
		return errors.New("-takes must be at least 1")
	}
	where := files{*dir}
	b, err := p.open(ctx, where, *model, *only)
	if err != nil {
		return err
	}
	b.baseURL = *baseURL

	if !*live {
		return p.rehearse(ctx, b, *count, *price)
	}

	key := p.getenv("TYPESAFE_API_KEY")
	switch {
	case key == "":
		return errors.New("TYPESAFE_API_KEY is not set, and -live sends requests to TypeSafe")
	case *budget < 1:
		return errors.New("give -live a budget: -budget <the most input tokens this run may spend>")
	}
	meter := evaluation.NewMeter(*budget)
	err = p.recordLive(ctx, where, b, *count, key, meter, *liveSearches)

	// Whatever happened, what was answered is kept: it was paid for.
	if saveErr := b.tape.Save(); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	requests, tokens := meter.Spent()
	dollars := float64(tokens) / 1e6 * (*price)
	p.say("")
	p.say("Sent to Jev: %s requests, %s input tokens (about $%.2f at $%.3f a million).", number(requests), number(tokens), dollars, *price)
	p.say("The tape %s now holds answers to %s questions over %s.", where.tape(b.model), number(b.tape.Len()), takes(b.tape.Takes()))
	if errors.Is(err, evaluation.ErrBudgetSpent) {
		return fmt.Errorf("the budget of %s tokens is spent, and the recording is not finished.\n"+
			"Run it again with a larger -budget to go on: nothing on the tape is asked for twice", number(*budget))
	}
	return err
}

// rehearse says what a recording would send, and sends nothing.
func (p *program) rehearse(ctx context.Context, b *bench, count int, price float64) error {
	ratio, measured := b.tape.BytesPerToken()
	if !measured {
		ratio = 3.5 // a guess, until something has been recorded to measure it from
	}

	var total evaluation.Lacking
	uncounted := 0
	for take := 1; take <= count; take++ {
		rehearsal := b.tape.Rehearse(take)
		judge := b.judge(rehearsal, "")
		if _, err := evaluation.Ratings(ctx, judge, b.set); err != nil {
			return err
		}
		// Which evidence the searches arrive at depends on the ratings. If
		// those are on the tape it can be worked out; if not, it has to
		// wait until they are.
		met, err := b.arrivals(ctx, b.judge(b.tape.Replay(take), ""))
		if errors.Is(err, evaluation.ErrNotOnTape) {
			met, uncounted = nil, uncounted+1
		} else if err != nil {
			return err
		}
		if _, err := evaluation.Sufficiency(ctx, judge, b.set, met); err != nil {
			return err
		}

		lacking := rehearsal.Lacking()
		total.Requests, total.Questions, total.Bytes = total.Requests+lacking.Requests, total.Questions+lacking.Questions, total.Bytes+lacking.Bytes
		if lacking.Questions == 0 {
			p.say("Take %d: on the tape already.", take)
			continue
		}
		p.say("Take %d: the tape lacks %s answers. Asking for them is %s requests, about %s input tokens.",
			take, number(lacking.Questions), number(lacking.Requests), number(int(float64(lacking.Bytes)/ratio)))
	}

	tokens := float64(total.Bytes) / ratio
	p.say("")
	if total.Questions == 0 {
		p.say("The tape of %s holds everything %s of this set ask. There is nothing to record.", b.model, takes(count))
		return nil
	}
	p.say("In all: %s answers to ask Jev for, in %s requests: about %s input tokens, about $%.2f at $%.3f a million.",
		number(total.Questions), number(total.Requests), number(int(tokens)), tokens/1e6*price, price)
	if !measured {
		p.say("The token count is a guess from the size of the requests; the first recording will show what a request really costs.")
	}
	if uncounted > 0 {
		p.say("Not counted, for %s: the readings of whatever evidence the searches arrive at. Which evidence that is", takes(uncounted))
		p.say("depends on the ratings, so it can be counted only once those are on the tape. Expect a few thousand tokens a question more.")
	}
	p.say("")
	p.say("Nothing has been sent. To record, with TYPESAFE_API_KEY set:")
	p.say("  grafsol-eval record -model %s -takes %d -live -budget <the most input tokens to spend>", b.model, count)
	return nil
}

// recordLive asks Jev for what the tape lacks, take by take.
func (p *program) recordLive(ctx context.Context, where files, b *bench, count int, key string, meter *evaluation.Meter, liveSearches bool) error {
	paid := meter.Transport(http.DefaultTransport)
	for take := 1; take <= count; take++ {
		recording := b.judge(b.tape.Record(take, paid), key)

		if _, err := evaluation.Ratings(ctx, recording, b.set); err != nil {
			return fmt.Errorf("take %d, the ratings: %w", take, err)
		}
		if err := b.tape.Save(); err != nil {
			return err
		}
		met, err := b.arrivals(ctx, b.judge(b.tape.Replay(take), ""))
		if err != nil {
			return fmt.Errorf("take %d, finding what the searches arrive at: %w", take, err)
		}
		readings, err := evaluation.Sufficiency(ctx, recording, b.set, met)
		if err != nil {
			return fmt.Errorf("take %d, the readings: %w", take, err)
		}
		if err := b.tape.Save(); err != nil {
			return err
		}
		_, tokens := meter.Spent()
		p.say("Take %d of %d is on the tape: every lead rated for %d questions, and %s sets of evidence read. %s tokens spent so far.",
			take, count, len(b.set.Questions), number(len(readings)), number(tokens))
	}
	if !liveSearches {
		return nil
	}
	return p.searchLive(ctx, where, b, b.judge(paid, key))
}

// searchLive runs one real search per question, asking Jev directly, for the
// questions that have not had one, and keeps what each came to.
func (p *program) searchLive(ctx context.Context, where files, b *bench, judge retrieval.Judge) error {
	kept, err := readLive(where.live(b.model))
	if err != nil {
		return err
	}
	var wanted []string
	for _, q := range b.set.Questions {
		if !slices.ContainsFunc(kept, func(search evaluation.Search) bool { return search.Question == q.ID }) {
			wanted = append(wanted, q.ID)
		}
	}
	if len(wanted) == 0 {
		p.say("Every question of this run has had its live search already.")
		return nil
	}
	set, err := b.set.Only(wanted...)
	if err != nil {
		return err
	}
	cfg := evaluation.DefaultSettings.Apply(b.search)
	cfg.Judge = judge
	searches, failed := evaluation.Searches(ctx, cfg, set)
	for _, search := range searches {
		if search.Err == "" {
			kept = append(kept, search)
		}
	}
	if err := writeJSON(where.live(b.model), kept); err != nil {
		return errors.Join(failed, err)
	}
	if failed != nil {
		return fmt.Errorf("the live searches: %w", failed)
	}
	p.say("%d live searches were run, one per question, and kept in %s.", len(searches), where.live(b.model))
	return nil
}

func readLive(path string) ([]evaluation.Search, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var searches []evaluation.Search
	if err := json.Unmarshal(content, &searches); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return searches, nil
}

func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// --- report ---------------------------------------------------------------------------

func (p *program) report(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	dir := flags.String("set", "eval", "the `folder` that holds the test set")
	model := flags.String("model", "", "the model whose tape to replay, as one `version`, such as jev-1.14.0")
	count := flags.Int("takes", 0, "report on the first so many `takes` of the tape; zero means all of them")
	only := flags.String("only", "", "report on these questions alone: their `IDs`, separated by commas")
	held := flags.Bool("check", false, "report on the part held back to check on, and not on the part to tune on")
	around := flags.Bool("around", true, "also search under the thresholds a step to each side of the ones in use")
	save := flags.String("save", "", "keep the gist of this run under a `label`, to set a later one against")
	against := flags.String("against", "", "set this run against the one saved in a `file`")
	if err := flags.Parse(args); err != nil {
		return err
	}
	where := files{*dir}
	b, err := p.open(ctx, where, *model, *only)
	if err != nil {
		return err
	}
	if *count == 0 {
		*count = b.tape.Takes()
	}
	if *count == 0 {
		return fmt.Errorf("the tape of %s is empty. `grafsol-eval record -model %s` says what recording it would ask of Jev", b.model, b.model)
	}

	tried := settings()
	if !*around {
		tried = tried[:1]
	}
	results := evaluation.Results{Model: b.model, Settings: tried[0]}
	for take := 1; take <= *count; take++ {
		measured, err := b.measure(ctx, take, tried)
		if errors.Is(err, evaluation.ErrNotOnTape) {
			return fmt.Errorf("%w\nThe tape of %s does not hold everything a report of take %d asks. "+
				"`grafsol-eval record -model %s -takes %d` says what is lacking", err, b.model, take, b.model, *count)
		}
		if err != nil {
			return err
		}
		results.Takes = append(results.Takes, measured)
	}
	if results.Live, err = readLive(where.live(b.model)); err != nil {
		return err
	}

	var earlier *evaluation.Saved
	if *against != "" {
		if earlier, err = evaluation.OpenSaved(*against); err != nil {
			return err
		}
	}

	// A report is of one part of the set, unless questions were named.
	shown, part := b.set, ""
	if *only == "" {
		part = string(evaluation.Tune)
		if *held {
			part = string(evaluation.Check)
		}
		shown = b.set.OfPart(evaluation.Part(part))
		p.say("The part of the test set to %s on.", part)
	}
	if err := evaluation.Report(p.out, shown, results, earlier); err != nil {
		return err
	}

	if *save != "" {
		path := where.saved(p.now(), *save)
		if err := results.Summary(b.set, *save, p.now()).Write(path); err != nil {
			return err
		}
		p.say("")
		p.say("The gist of this run, for every question measured, is kept in %s.", path)
	}
	return nil
}

// measure replays one take: the ratings, the readings, and the searches under
// each of the settings.
func (b *bench) measure(ctx context.Context, take int, tried []evaluation.Settings) (evaluation.Take, error) {
	judge := b.judge(b.tape.Replay(take), "")
	var measured evaluation.Take
	var err error

	if measured.Ratings, err = evaluation.Ratings(ctx, judge, b.set); err != nil {
		return measured, err
	}
	met, err := b.arrivals(ctx, judge)
	if err != nil {
		return measured, err
	}
	if measured.Readings, err = evaluation.Sufficiency(ctx, judge, b.set, met); err != nil {
		return measured, err
	}
	cfg := b.search
	cfg.Judge = judge
	for _, setting := range tried {
		searches, err := evaluation.Searches(ctx, setting.Apply(cfg), b.set)
		if err != nil {
			return measured, err
		}
		measured.Runs = append(measured.Runs, evaluation.Run{Settings: setting, Searches: searches})
	}
	return measured, nil
}

// --- small things ---------------------------------------------------------------------

// number writes a count with its thousands marked off.
func number(n int) string {
	digits := fmt.Sprint(n)
	if n < 0 {
		return "-" + number(-n)
	}
	for at := len(digits) - 3; at > 0; at -= 3 {
		digits = digits[:at] + "," + digits[at:]
	}
	return digits
}

func takes(n int) string {
	if n == 1 {
		return "1 take"
	}
	return fmt.Sprintf("%d takes", n)
}

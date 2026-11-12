package evaluation

import (
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/umran/grafsol/graph"
)

// Results is what the measurements came to, take by take. It is what a
// report is written from.
type Results struct {
	// Model is the model that judged, and Settings the thresholds that the
	// searches of the first Run of each take ran under: the ones in use.
	Model    string
	Settings Settings

	Takes []Take

	// Live is one real search per question, asked of Jev directly and not
	// replayed, if any were run: the check that replaying tells the truth.
	Live []Search
}

// Take is everything measured in one take.
type Take struct {
	Ratings  []Rating
	Readings []Reading

	// Runs is the searches of the take under each of the settings tried.
	// The first is under Results.Settings.
	Runs []Run
}

// Run is the searches of one take under one setting of the thresholds.
type Run struct {
	Settings Settings
	Searches []Search
}

// Outcome says how a search did against what is known of the question's
// answer.
type Outcome string

const (
	// Answered: the search stopped, and had gathered the whole answer.
	Answered Outcome = "stopped with the whole answer"

	// StoppedShort: the search stopped as if it had the answer, and a fact
	// the answer needs was not in what it had gathered.
	StoppedShort Outcome = "stopped without the whole answer"

	// WentOn: the search had gathered the whole answer and did not stop:
	// the judge did not find the evidence enough.
	WentOn Outcome = "had the whole answer and did not stop"

	// NeverHadIt: the search ended without gathering the whole answer, and
	// did not claim to have it.
	NeverHadIt Outcome = "never gathered the whole answer"

	// HeldBack: the graph has no answer, and the search did not claim one.
	HeldBack Outcome = "rightly did not stop"

	// StoppedAnyway: the graph has no answer, and the search stopped as if
	// it had found one.
	StoppedAnyway Outcome = "stopped though the graph has no answer"

	// Failed: the search could not be carried through.
	Failed Outcome = "failed"
)

// Right reports whether the outcome is what a search should come to.
func (o Outcome) Right() bool {
	return o == Answered || o == HeldBack
}

// Outcome judges a search for the question against the question's labels.
//
// "The whole answer" means documents that between them state every needed
// fact. For a question one of whose facts is stated only on a link, it means
// the documents that link joins as well: a search returns the path that led
// to each piece of evidence, so with those documents in hand the note is in
// hand too.
func (q Question) Outcome(search Search) Outcome {
	if search.Err != "" {
		return Failed
	}
	stopped := search.Verdict == "sufficient"
	if !q.Answerable() {
		if stopped {
			return StoppedAnyway
		}
		return HeldBack
	}
	switch gathered := q.gathered(evidenceOf(search)); {
	case stopped && gathered:
		return Answered
	case stopped:
		return StoppedShort
	case gathered:
		return WentOn
	}
	return NeverHadIt
}

// gathered reports whether the documents hold the whole answer, in the sense
// Outcome explains.
func (q Question) gathered(evidence []graph.ID) bool {
	if !q.NeedsNote() {
		return q.Enough(evidence)
	}
	for _, document := range q.joined() {
		if !slices.Contains(evidence, document) {
			return false
		}
	}
	return true
}

// evidenceOf returns the documents a search returned.
func evidenceOf(search Search) []graph.ID {
	documents := make([]graph.ID, len(search.Evidence))
	for i, found := range search.Evidence {
		documents[i] = found.Document
	}
	return documents
}

// --- what is kept of a run, to set a later one against --------------------------

// Saved is the gist of a set of Results, small enough to keep: what each
// thing measured came to on average over the takes, and how far the takes
// lay apart. A later report can be set against it to see what a change did.
type Saved struct {
	Label string    // what the person who saved it called it
	When  time.Time // when it was saved
	Model string

	Settings Settings
	Takes    int

	Ratings  []SavedRating
	Readings []SavedReading
	Searches []SavedSearch
}

// SavedRating is a rating over the takes: its mean, and its lowest and
// highest.
type SavedRating struct {
	Question string
	Target   graph.ID
	Edge     graph.Edge
	Mean     float64
	Low      float64
	High     float64
}

// SavedReading is a reading of one set of evidence over the takes.
type SavedReading struct {
	Question string
	Evidence []graph.ID
	Mean     float64
	Low      float64
	High     float64
}

// SavedSearch is how the searches for one question came out, take by take,
// under the settings in use.
type SavedSearch struct {
	Question string
	Outcomes []Outcome
}

// Summary returns the gist of the results for the questions of the set.
func (r Results) Summary(set Set, label string, when time.Time) Saved {
	saved := Saved{Label: label, When: when.UTC().Truncate(time.Second), Model: r.Model, Settings: r.Settings, Takes: len(r.Takes)}

	for _, series := range r.ratingSeries(set) {
		saved.Ratings = append(saved.Ratings, SavedRating{
			Question: series.Question, Target: series.Target, Edge: series.Edge,
			Mean: mean(series.Values), Low: slices.Min(series.Values), High: slices.Max(series.Values),
		})
	}
	for _, series := range r.readingSeries(set) {
		saved.Readings = append(saved.Readings, SavedReading{
			Question: series.Question, Evidence: series.Evidence,
			Mean: mean(series.Values), Low: slices.Min(series.Values), High: slices.Max(series.Values),
		})
	}
	for _, q := range set.Questions {
		search := SavedSearch{Question: q.ID}
		for _, take := range r.Takes {
			if found, ok := take.search(q.ID); ok {
				search.Outcomes = append(search.Outcomes, q.Outcome(found))
			}
		}
		saved.Searches = append(saved.Searches, search)
	}
	return saved
}

// Write writes the saved results to a file.
func (s Saved) Write(path string) error {
	encoded, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return fmt.Errorf("evaluation: encoding the saved results: %w", err)
	}
	return writeFile(path, append(encoded, '\n'))
}

// OpenSaved reads saved results from a file.
func OpenSaved(path string) (*Saved, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("evaluation: %w", err)
	}
	saved := new(Saved)
	if err := json.Unmarshal(content, saved); err != nil {
		return nil, fmt.Errorf("evaluation: %s: %w", filepath.Base(path), err)
	}
	return saved, nil
}

// --- the same thing, across the takes -------------------------------------------

// ratingSeries is one way of coming across a vertex for one question, and
// what the judge rated it in each take.
type ratingSeries struct {
	Rating
	Values []float64
}

// readingSeries is one set of evidence for one question, and how the judge
// read it in each take.
type readingSeries struct {
	Reading
	Values []float64
}

// ratingSeries gathers the ratings of the takes, for the questions of the
// set, into one series per lead. They come in the order of the first take.
func (r Results) ratingSeries(set Set) []ratingSeries {
	type lead struct {
		question string
		target   graph.ID
		edge     graph.Edge
	}
	var series []ratingSeries
	at := make(map[lead]int)
	for _, take := range r.Takes {
		for _, rating := range take.Ratings {
			if !set.has(rating.Question) {
				continue
			}
			key := lead{rating.Question, rating.Target, rating.Edge.Triple()}
			i, known := at[key]
			if !known {
				i = len(series)
				at[key] = i
				series = append(series, ratingSeries{Rating: rating})
			}
			series[i].Values = append(series[i].Values, rating.Potential)
		}
	}
	return series
}

// readingSeries does the same for the readings.
func (r Results) readingSeries(set Set) []readingSeries {
	var series []readingSeries
	for _, take := range r.Takes {
		for _, reading := range take.Readings {
			if !set.has(reading.Question) {
				continue
			}
			i := slices.IndexFunc(series, func(known readingSeries) bool {
				return known.Question == reading.Question && slices.Equal(known.Evidence, reading.Evidence)
			})
			if i < 0 {
				i = len(series)
				series = append(series, readingSeries{Reading: reading})
			}
			series[i].Values = append(series[i].Values, reading.Sufficient)
		}
	}
	return series
}

// search returns the take's search for a question under the settings in use.
func (t Take) search(question string) (Search, bool) {
	if len(t.Runs) == 0 {
		return Search{}, false
	}
	return t.Runs[0].search(question)
}

func (r Run) search(question string) (Search, bool) {
	for _, search := range r.Searches {
		if search.Question == question {
			return search, true
		}
	}
	return Search{}, false
}

// has reports whether the set has a question with that ID.
func (s Set) has(id string) bool {
	return slices.ContainsFunc(s.Questions, func(q Question) bool { return q.ID == id })
}

// question returns the question with that ID.
func (s Set) question(id string) (Question, bool) {
	for _, q := range s.Questions {
		if q.ID == id {
			return q, true
		}
	}
	return Question{}, false
}

// --- arithmetic -------------------------------------------------------------------

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

// spread is how far apart the values lie: the highest less the lowest.
func spread(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return slices.Max(values) - slices.Min(values)
}

// median returns the middle value, or the mean of the two middle ones.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Sorted(slices.Values(values))
	if middle := len(sorted) / 2; len(sorted)%2 == 1 {
		return sorted[middle]
	} else {
		return (sorted[middle-1] + sorted[middle]) / 2
	}
}

// margin says how far a mean over questions can be trusted: the range that
// holds nine in ten of the means that come of drawing as many questions
// again, at random, from the ones there are. It is wide when the questions
// are few or differ a lot, which is exactly when a mean should not be leaned
// on. The drawing is seeded, so the same numbers always give the same range.
func margin(perQuestion []float64) (low, high float64) {
	n := len(perQuestion)
	if n == 0 {
		return 0, 0
	}
	const draws = 2000
	random := rand.New(rand.NewPCG(1, uint64(n)))
	means := make([]float64, draws)
	for i := range means {
		total := 0.0
		for range n {
			total += perQuestion[random.IntN(n)]
		}
		means[i] = total / float64(n)
	}
	slices.Sort(means)
	return means[draws/20], means[draws-1-draws/20]
}

// round2 rounds to hundredths, which is as finely as Jev's numbers are worth
// reading.
func round2(x float64) float64 {
	return math.Round(x*100) / 100
}

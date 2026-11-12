package evaluation

import (
	"bytes"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
)

func TestHowASearchIsJudgedAgainstTheLabels(t *testing.T) {
	set := example(t)
	answered := question(t, set, "slow-checkout") // both needed facts are in the cache document
	byNote := question(t, set, "which-change")    // its fact is on the link between cache and slow
	unanswered := question(t, set, "rota-approver")

	found := func(documents ...graph.ID) []Found {
		var evidence []Found
		for _, document := range documents {
			evidence = append(evidence, Found{Document: document, Potential: 0.9})
		}
		return evidence
	}
	cases := []struct {
		name     string
		question Question
		search   Search
		want     Outcome
		right    bool
	}{
		{"stopped with the answer", answered, Search{Verdict: "sufficient", Evidence: found(cache)}, Answered, true},
		{"stopped with a near miss beside the answer", answered, Search{Verdict: "sufficient", Evidence: found(dashboard, cache)}, Answered, true},
		{"stopped with only the extra fact", answered, Search{Verdict: "sufficient", Evidence: found(slow)}, StoppedShort, false},
		{"stopped with nothing", answered, Search{Verdict: "sufficient"}, StoppedShort, false},
		{"ran out of leads with the answer in hand", answered, Search{Verdict: "exhausted", Evidence: found(cache)}, WentOn, false},
		{"hit a limit with the answer in hand", answered, Search{Verdict: "stopped", Evidence: found(cache)}, WentOn, false},
		{"ran out of leads without it", answered, Search{Verdict: "exhausted", Evidence: found(slow)}, NeverHadIt, false},
		{"failed", answered, Search{Verdict: "stopped", Err: "the judge is out", Evidence: found(cache)}, Failed, false},

		// The fact is on the link: the two documents it joins are the whole
		// answer, because a search returns the path between them.
		{"stopped with both ends of the link", byNote, Search{Verdict: "sufficient", Evidence: found(slow, cache)}, Answered, true},
		{"stopped with one end of the link", byNote, Search{Verdict: "sufficient", Evidence: found(cache)}, StoppedShort, false},
		{"both ends, and did not stop", byNote, Search{Verdict: "exhausted", Evidence: found(cache, slow)}, WentOn, false},

		{"no answer, and did not stop", unanswered, Search{Verdict: "exhausted"}, HeldBack, true},
		{"no answer, did not stop, returned a near miss", unanswered, Search{Verdict: "exhausted", Evidence: found(rota)}, HeldBack, true},
		{"no answer, and stopped", unanswered, Search{Verdict: "sufficient", Evidence: found(rota)}, StoppedAnyway, false},
	}
	for _, c := range cases {
		got := c.question.Outcome(c.search)
		if got != c.want || got.Right() != c.right {
			t.Errorf("%s: the outcome is %q (right: %v), want %q (right: %v)", c.name, got, got.Right(), c.want, c.right)
		}
	}
}

// A lead shows a fact if the vertex at its end states one, or if the edge it
// arrives by does: the judge is shown that edge's note.
func TestWhatALeadIsToAQuestion(t *testing.T) {
	set := example(t)
	causes := set.Graph.Edges[0] // cache links to slow; its note states the fact of which-change
	tagged := set.Graph.Edges[1]
	answered := question(t, set, "slow-checkout")
	byNote := question(t, set, "which-change")

	cases := []struct {
		name     string
		question Question
		lead     Rating
		want     standing
	}{
		{"a document that states a fact", answered, Rating{Target: cache}, states},
		{"the same document at the end of a link", answered, Rating{Target: cache, From: slow, Edge: causes}, states},
		{"a near miss", answered, Rating{Target: dashboard}, nearMiss},
		{"a document nothing is said of", answered, Rating{Target: rota}, unlabelled},
		{"a topic", answered, Rating{Target: performance, From: slow, Edge: tagged}, unlabelled},

		{"one end of the link whose note states the fact, met as a place to start", byNote, Rating{Target: cache}, unlabelled},
		{"the same, reached by that link", byNote, Rating{Target: cache, From: slow, Edge: causes}, states},
		{"the other end, reached by that link", byNote, Rating{Target: slow, From: cache, Edge: causes}, states},
		{"reached by another link", byNote, Rating{Target: performance, From: slow, Edge: tagged}, unlabelled},
	}
	for _, c := range cases {
		if got := c.question.standing(c.lead); got != c.want {
			t.Errorf("%s: its standing is %d, want %d", c.name, got, c.want)
		}
	}

	// And what belongs in the evidence returned: a document that states a
	// fact, or one of the two a fact-stating link joins.
	for document, want := range map[graph.ID]bool{cache: true, slow: true, dashboard: false, rota: false} {
		if got := byNote.belongs(document); got != want {
			t.Errorf("for the question whose fact is on a link, belongs(%s) is %v, want %v", document, got, want)
		}
	}
	if answered.belongs(dashboard) || !answered.belongs(slow) {
		t.Error("for the question whose facts are in documents, a near miss belongs or the extra fact's document does not")
	}
}

// worked returns results small enough to work out by hand: the example set,
// two takes. The numbers the report should come to are worked out in the
// test that uses it.
func worked(t *testing.T) (Set, Results) {
	t.Helper()
	set := example(t)
	causes := set.Graph.Edges[0] // cache links to slow

	ratings := func(cacheSeed, slowSeed, dashboardSeed, rotaSeed, slowByLink, cacheByLink, rotaForRota, cacheForRota float64) []Rating {
		return []Rating{
			{Question: "slow-checkout", Target: cache, Potential: cacheSeed},         // states
			{Question: "slow-checkout", Target: slow, Potential: slowSeed},           // states (the extra fact)
			{Question: "slow-checkout", Target: dashboard, Potential: dashboardSeed}, // a near miss
			{Question: "slow-checkout", Target: rota, Potential: rotaSeed},           // nothing
			{Question: "slow-checkout", Target: slow, From: cache, Edge: causes, Potential: slowByLink},
			{Question: "slow-checkout", Target: cache, From: slow, Edge: causes, Potential: cacheByLink},
			{Question: "rota-approver", Target: rota, Potential: rotaForRota},   // a near miss
			{Question: "rota-approver", Target: cache, Potential: cacheForRota}, // nothing: a surprise if rated high
		}
	}
	readings := func(whole, nearOnly, withoutNote, rotaNear float64) []Reading {
		return []Reading{
			{Question: "slow-checkout", Case: WholeAnswer, Evidence: []graph.ID{cache}, Enough: true, Sufficient: whole},
			{Question: "slow-checkout", Case: NearMissesOnly, Evidence: []graph.ID{dashboard}, Sufficient: nearOnly},
			{Question: "which-change", Case: AllButTheNote, Evidence: []graph.ID{cache, slow}, Sufficient: withoutNote},
			{Question: "rota-approver", Case: NearMissesOnly, Evidence: []graph.ID{rota}, Sufficient: rotaNear},
		}
	}
	found := func(documents ...graph.ID) []Found {
		var evidence []Found
		for _, document := range documents {
			evidence = append(evidence, Found{Document: document, Potential: 0.9})
		}
		return evidence
	}

	first := ratings(0.90, 0.50, 0.80, 0.00, 0.70, 0.60, 0.50, 0.78)
	second := ratings(0.86, 0.52, 0.70, 0.02, 0.72, 0.62, 0.55, 0.76)
	other := Settings{Sufficient: 0.6, MinPotential: 0.25, MinEvidence: 0.75}

	results := Results{
		Model:    "jev-test",
		Settings: DefaultSettings,
		Takes: []Take{
			{
				Ratings:  first,
				Readings: readings(0.80, 0.75, 0.50, 0.10),
				Runs: []Run{
					{Settings: DefaultSettings, Searches: []Search{
						{Question: "slow-checkout", Verdict: "sufficient", Sufficient: 0.80, Evidence: found(cache), Met: first[:4], Steps: 2, Asked: Asked{Times: 2, Leads: 5, Readings: 1}},
						{Question: "which-change", Verdict: "exhausted", Sufficient: 0.50, Evidence: found(cache, slow)},
						{Question: "rota-approver", Verdict: "exhausted"},
					}},
					{Settings: other, Searches: []Search{
						{Question: "slow-checkout", Verdict: "sufficient", Evidence: found(cache), Asked: Asked{Leads: 4}},
						{Question: "which-change", Verdict: "exhausted", Evidence: found(cache, slow)},
						{Question: "rota-approver", Verdict: "sufficient", Evidence: found(rota)},
					}},
				},
			},
			{
				Ratings:  second,
				Readings: readings(0.60, 0.30, 0.72, 0.20),
				Runs: []Run{
					{Settings: DefaultSettings, Searches: []Search{
						{Question: "slow-checkout", Verdict: "exhausted", Sufficient: 0.60, Evidence: found(cache), Met: second[:4]},
						{Question: "which-change", Verdict: "sufficient", Sufficient: 0.72, Evidence: found(cache, slow)},
						{Question: "rota-approver", Verdict: "sufficient", Sufficient: 0.90, Evidence: found(rota)},
					}},
					{Settings: other, Searches: []Search{
						{Question: "slow-checkout", Verdict: "sufficient", Evidence: found(cache), Asked: Asked{Leads: 4}},
						{Question: "which-change", Verdict: "exhausted", Evidence: found(cache, slow)},
						{Question: "rota-approver", Verdict: "sufficient", Evidence: found(rota)},
					}},
				},
			},
		},
		Live: []Search{
			{Question: "slow-checkout", Verdict: "sufficient", Evidence: found(cache)},
			{Question: "rota-approver", Verdict: "exhausted"},
		},
	}
	return set, results
}

// lines returns the report with runs of spaces closed up, so that a test can
// look for a row without counting the padding between its columns.
func closedUp(report string) string {
	return strings.Join(strings.Fields(report), " ")
}

func TestTheReportComesToTheNumbersWorkedByHand(t *testing.T) {
	set, results := worked(t)
	var out bytes.Buffer
	if err := Report(&out, set, results, nil); err != nil {
		t.Fatalf("Report: %v", err)
	}
	report := closedUp(out.String())

	for name, want := range map[string]string{
		"the heading": "3 questions (1 one, 1 link, 1 absent) over 4 documents and 2 edges. Judge jev-test, 2 takes. " +
			"Thresholds in use: stop at 0.70, follow from 0.25, evidence from 0.75.",

		// Searches. slow-checkout: stopped with the answer once, had it and
		// went on once. which-change: went on, then stopped with both ends.
		// rota-approver: held back, then stopped anyway.
		"the one-document row": "one 1 50% 1/2 0% 0/2 50% 1/2 0% 0/2",
		"the link row":         "link 1 50% 1/2 0% 0/2 50% 1/2 0% 0/2",
		"the absent row":       "absent 1 50% 1/2 50% 1/2 50% 1/2",
		"the total":            "Right in all: 50% 3/6 of searches.",
		"the unsteady":         "3 of 3 questions did not come out the same way in every take.",
		"the notes":            "Questions that need a link's note to be answered: 1 (which-change).",

		// Of 3 needed facts a take (2 of slow-checkout, 1 of which-change),
		// the evidence states the first two: the third is on a link.
		"the facts covered": "Of the facts the answers need, the evidence returned states 67%.",
		// Returned: cache; cache and slow (the ends of the link); then the
		// same and, wrongly, the rota.
		"the documents returned": "Of the documents returned as evidence, 86% belong there (6 of 7)",

		// What went wrong, by name.
		"a wrong search by name":  "slow-checkout (one): stopped with the whole answer in 1 of 2 takes; had the whole answer and did not stop in 1 of 2 takes.",
		"how its evidence read":   "the judge read its evidence at 0.60 to 0.80",
		"how its document fared":  "cache was met as a place to start and rated 0.86 to 0.90",
		"a document never met":    "slow was never met by a search",
		"an unanswerable one":     "rota-approver (absent): rightly did not stop in 1 of 2 takes; stopped though the graph has no answer in 1 of 2 takes.",
		"a wrong document, named": "rota states none of the answer and was returned as evidence in 1 take, rated 0.90",

		// Ratings at the evidence threshold. Met by searches: cache twice
		// over, slow twice under; the dashboard over once of two; the rota
		// never. Over every lead: the same documents, each at the best of
		// its two ratings (cache 0.90 and 0.86 as a place to start, though
		// lower by the link; slow 0.70 and 0.72 by the link, still under),
		// the dashboard and the rota for its own question once in four, and
		// the cache for the rota question over both times.
		"fact-stating documents as evidence": "documents that state a fact, rated as evidence 50% 2/4 50% 2/4",
		"near misses as evidence":            "near misses rated as evidence 50% 1/2 25% 1/4",
		"other leads as evidence":            "other leads rated as evidence 0% 0/2 50% 2/4",
		"fact-stating documents followed":    "documents that state a fact, followed 100% 4/4 100% 4/4",
		"leads followed for nothing":         "leads that state nothing, followed 50% 2/4 75% 6/8",
		"another evidence threshold":         "0.70 50% 2/4 100% 2/2 0% 0/2",
		"the threshold in use":               "0.75 in use 50% 2/4 50% 1/2 0% 0/2",
		"by the way reached":                 "slow-checkout, slow: 0.51 as a place to start, 0.71 at the end of a link",
		"by the way reached, the other way":  "slow-checkout, cache: 0.88 as a place to start, 0.61 at the end of a link",
		"between takes":                      "a rating moved by 0.02 at the median and 0.10 at the most. Of 8 ratings, 1 fell on both sides of the evidence threshold from one take to another, and 0 on both sides of the follow threshold.",

		// Sufficiency at the stop threshold.
		"the whole answer":        "the whole answer 1 enough 50% 1/2 0.70",
		"near misses only":        "near misses only 2 not enough 25% 1/4 0.34",
		"without the note":        "the documents without the link's note 1 enough, if the note were shown 50% 1/2 0.61",
		"another stop threshold":  "0.60 100% 2/2 25% 1/4",
		"the stop threshold used": "0.70 in use 50% 1/2 25% 1/4",
		"the two ranges":          "Readings of evidence that is enough: 0.60 to 0.80. Of evidence that is not: 0.10 to 0.75.",

		// The other setting: slow-checkout right twice, the rota stopped
		// wrongly twice, which-change never stopped.
		"the setting in use": "0.70 0.25 0.75 50% 3/6 17% 1/6 0.8 in use",
		"the other setting":  "0.60 0.25 0.75 33% 2/6 33% 2/6 1.3",

		"the surprise":       `rota-approver: cache, as a place to start, rated 0.76 to 0.78. "Remove the price cache"`,
		"surprises sometime": "Unlabelled leads that reached that rating in at least one take: 1.",

		"the live check": "Of 2 live searches, 2 came out as most replays of the same question did, and 2 as at least one did.",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("%s: the report does not say\n    %s", name, want)
		}
	}
	if t.Failed() {
		t.Logf("the report was:\n%s", out.String())
	}
	if strings.Contains(report, "SET AGAINST") {
		t.Error("the report sets itself against an earlier run that it was not given")
	}
}

func TestAReportOfNothingSaysSo(t *testing.T) {
	var out bytes.Buffer
	if err := Report(&out, example(t), Results{Settings: DefaultSettings}, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Nothing has been measured yet.") {
		t.Errorf("a report of no takes says:\n%s", out.String())
	}
}

func TestAReportCoversOnlyTheQuestionsItIsGiven(t *testing.T) {
	set, results := worked(t)
	one, err := set.Only("rota-approver")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Report(&out, one, results, nil); err != nil {
		t.Fatal(err)
	}
	report := closedUp(out.String())
	if !strings.Contains(report, "1 questions (1 absent)") || !strings.Contains(report, "Right in all: 50% 1/2 of searches.") {
		t.Errorf("a report of one question says:\n%s", out.String())
	}
	if strings.Contains(report, "slow-checkout") || strings.Contains(report, "which-change") {
		t.Errorf("a report of one question mentions another:\n%s", out.String())
	}
	// The readings counted are that question's too: one set, read twice.
	if !strings.Contains(report, "near misses only 1 not enough 0% 0/2 0.15") {
		t.Errorf("a report of one question counts readings that are not its own:\n%s", out.String())
	}
}

// With one take there is nothing to say about how the takes differ.
func TestAReportOfOneTakeSaysNothingOfSteadiness(t *testing.T) {
	set, results := worked(t)
	results.Takes = results.Takes[:1]
	var out bytes.Buffer
	if err := Report(&out, set, results, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Between takes") {
		t.Errorf("a report of one take speaks of the takes differing:\n%s", out.String())
	}
	if !strings.Contains(closedUp(out.String()), "Judge jev-test, 1 take.") {
		t.Errorf("the report does not say it is of one take:\n%s", out.String())
	}
}

func TestTheCommonestOutcome(t *testing.T) {
	cases := []struct {
		outcomes []Outcome
		want     Outcome
	}{
		{[]Outcome{Answered, WentOn, WentOn}, WentOn},
		{[]Outcome{WentOn, Answered, WentOn, Answered, Answered}, Answered},
		{[]Outcome{HeldBack}, HeldBack},
		// Of two that came up equally often, the earlier.
		{[]Outcome{Answered, WentOn}, Answered},
		{[]Outcome{WentOn, Answered}, WentOn},
	}
	for _, c := range cases {
		if got := commonest(c.outcomes); got != c.want {
			t.Errorf("the commonest of %v is %q, want %q", c.outcomes, got, c.want)
		}
	}
}

func TestSavedResultsCanBeSetAgainstALaterRun(t *testing.T) {
	set, results := worked(t)
	saved := results.Summary(set, "before the change", time.Date(2026, 11, 12, 9, 30, 0, 0, time.UTC))

	// The gist: a mean and a range for each thing measured.
	if len(saved.Ratings) != 8 || len(saved.Readings) != 4 || len(saved.Searches) != 3 || saved.Takes != 2 || saved.Model != "jev-test" {
		t.Fatalf("the summary holds %d ratings, %d readings, %d searches", len(saved.Ratings), len(saved.Readings), len(saved.Searches))
	}
	if first := saved.Ratings[0]; first.Question != "slow-checkout" || first.Target != cache || !near(first.Mean, 0.88) || first.Low != 0.86 || first.High != 0.90 {
		t.Errorf("the first rating was summed up as %+v", first)
	}
	if got := saved.Searches[0].Outcomes; !reflect.DeepEqual(got, []Outcome{Answered, WentOn}) {
		t.Errorf("the first question's outcomes were saved as %v", got)
	}

	// It survives the file.
	path := filepath.Join(t.TempDir(), "results", "before.json")
	if err := saved.Write(path); err != nil {
		t.Fatalf("Write: %v", err)
	}
	reopened, err := OpenSaved(path)
	if err != nil || !reflect.DeepEqual(*reopened, saved) {
		t.Fatalf("read back, the saved results are %+v, %v", reopened, err)
	}

	// Set against itself, nothing has moved.
	var same bytes.Buffer
	if err := Report(&same, set, results, reopened); err != nil {
		t.Fatal(err)
	}
	unchanged := closedUp(same.String())
	for _, want := range []string{
		"SET AGAINST BEFORE THE CHANGE Saved 12 November 2026: judge jev-test, 2 takes.",
		"Searches right: 50% 3/6 then, 50% 3/6 now.",
		"Searches that mostly come out another way: none.",
		"Ratings that moved: none.",
		"Readings that moved: none.",
	} {
		if !strings.Contains(unchanged, want) {
			t.Errorf("set against itself, the report does not say %q:\n%s", want, same.String())
		}
	}

	// A later run in which three things changed: the cache document is
	// rated far lower as a place to start, the whole answer reads far
	// lower, and the rota question is never stopped on.
	later := results
	later.Takes = nil
	for _, take := range results.Takes {
		changed := Take{Ratings: append([]Rating(nil), take.Ratings...), Readings: append([]Reading(nil), take.Readings...)}
		changed.Ratings[0].Potential -= 0.5
		changed.Readings[0].Sufficient -= 0.4
		// A rating that moves by less than the takes differ is not news.
		changed.Ratings[2].Potential += 0.03
		searches := append([]Search(nil), take.Runs[0].Searches...)
		searches[2] = Search{Question: "rota-approver", Verdict: "exhausted"}
		changed.Runs = []Run{{Settings: DefaultSettings, Searches: searches}}
		later.Takes = append(later.Takes, changed)
	}
	var moved bytes.Buffer
	if err := Report(&moved, set, later, reopened); err != nil {
		t.Fatal(err)
	}
	report := closedUp(moved.String())
	for _, want := range []string{
		"Searches right: 50% 3/6 then, 67% 4/6 now.",
		"Ratings that moved (1): slow-checkout, cache as a place to start: 0.88 then, 0.38 now",
		"Readings that moved (1): slow-checkout, cache: 0.70 then, 0.30 now",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("set against the earlier run, the report does not say %q:\n%s", want, moved.String())
		}
	}
	// The rota question came out two ways before, one of them right, and
	// comes out right both times now: its commonest outcome is the same.
	if !strings.Contains(report, "Searches that mostly come out another way: none.") {
		t.Errorf("a question whose commonest outcome did not change was listed:\n%s", moved.String())
	}
}

func TestTheMarginIsNarrowWhenQuestionsAgreeAndWideWhenTheyDoNot(t *testing.T) {
	if low, high := margin([]float64{0.8, 0.8, 0.8, 0.8}); low != 0.8 || high != 0.8 {
		t.Errorf("questions that all score 0.8 give a margin of %v to %v", low, high)
	}
	split := []float64{0, 1, 0, 1, 0, 1, 0, 1, 0, 1}
	low, high := margin(split)
	if low >= 0.5 || high <= 0.5 || low < 0.1 || high > 0.9 {
		t.Errorf("ten questions split down the middle give a margin of %v to %v", low, high)
	}
	// More questions, the same split: a narrower margin.
	many := slicesRepeat(split, 20)
	if lowMany, highMany := margin(many); highMany-lowMany >= high-low {
		t.Errorf("200 questions give a margin of %v to %v, no narrower than 10 give", lowMany, highMany)
	}
	// And the same numbers always give the same margin.
	if again, _ := margin(split); again != low {
		t.Errorf("the margin of the same numbers came out as %v and then %v", low, again)
	}
	if low, high := margin(nil); low != 0 || high != 0 {
		t.Errorf("no questions give a margin of %v to %v", low, high)
	}
}

func slicesRepeat(values []float64, times int) []float64 {
	var repeated []float64
	for range times {
		repeated = append(repeated, values...)
	}
	return repeated
}

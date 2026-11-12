package evaluation

import (
	"cmp"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/umran/grafsol/graph"
)

// Report writes what the results show, for the questions of the set, as
// text. It reports on exactly the questions it is given: hand it one part of
// a test set to see that part alone.
//
// against, if not nil, is the gist of an earlier run, and the report ends by
// setting this one beside it.
//
// The sections come in the order of what matters: how whole searches did,
// which ones went wrong and how, and then the two things a search rests on,
// the ratings of leads and the readings of evidence, each with what other
// thresholds would have made of the same answers.
func Report(w io.Writer, set Set, results Results, against *Saved) error {
	r := &report{set: set, results: results, table: tabwriter.NewWriter(w, 0, 0, 2, ' ', 0), out: w}
	r.heading()
	if len(results.Takes) == 0 {
		r.line("Nothing has been measured yet.")
		return r.err
	}
	r.searches()
	r.wrong()
	r.ratings()
	r.sufficiency()
	r.settings()
	r.surprises()
	r.live()
	if against != nil {
		r.against(*against)
	}
	return r.err
}

// report is a report being written. Every section writes through it, and it
// remembers the first error so that the sections need not.
type report struct {
	set     Set
	results Results

	out   io.Writer
	table *tabwriter.Writer // columns, lined up when the section ends
	err   error
}

func (r *report) line(format string, args ...any) {
	if r.err == nil {
		_, r.err = fmt.Fprintf(r.out, format+"\n", args...)
	}
}

// row adds a row to the table under way; cells are separated by tabs.
func (r *report) row(format string, args ...any) {
	if r.err == nil {
		_, r.err = fmt.Fprintf(r.table, format+"\n", args...)
	}
}

// endTable writes out the rows gathered so far, with their columns lined up.
func (r *report) endTable() {
	if r.err == nil {
		r.err = r.table.Flush()
	}
}

func (r *report) section(title, about string) {
	r.line("")
	r.line("%s", strings.ToUpper(title))
	r.line("%s", about)
	r.line("")
}

func (r *report) heading() {
	kinds := make(map[Kind]int)
	for _, q := range r.set.Questions {
		kinds[q.Kind]++
	}
	var byKind []string
	for _, kind := range allKinds {
		if kinds[kind] > 0 {
			byKind = append(byKind, fmt.Sprintf("%d %s", kinds[kind], kind))
		}
	}
	r.line("%d questions (%s) over %d documents and %d edges.",
		len(r.set.Questions), strings.Join(byKind, ", "), len(r.set.Graph.Documents), len(r.set.Graph.Edges))
	s := r.results.Settings
	r.line("Judge %s, %s. Thresholds in use: stop at %.2f, follow from %.2f, evidence from %.2f.",
		cmp.Or(r.results.Model, "unnamed"), takes(len(r.results.Takes)), s.Sufficient, s.MinPotential, s.MinEvidence)
}

var allKinds = []Kind{OneDocument, Linked, Several, BothRepositories, Absent, Nowhere, Lost}

// --- searches -------------------------------------------------------------------

// outcomes returns, for each question in order, how its search came out in
// each take of a run. pick chooses the run of a take.
func (r *report) outcomes(pick func(Take) (Run, bool)) [][]Outcome {
	all := make([][]Outcome, len(r.set.Questions))
	for i, q := range r.set.Questions {
		for _, take := range r.results.Takes {
			run, ok := pick(take)
			if !ok {
				continue
			}
			if search, found := run.search(q.ID); found {
				all[i] = append(all[i], q.Outcome(search))
			}
		}
	}
	return all
}

func inUse(take Take) (Run, bool) {
	if len(take.Runs) == 0 {
		return Run{}, false
	}
	return take.Runs[0], true
}

// share writes a count out of a total, with its percentage.
func share(count, total int) string {
	if total == 0 {
		return "-"
	}
	return fmt.Sprintf("%3.0f%%  %d/%d", 100*float64(count)/float64(total), count, total)
}

func (r *report) searches() {
	r.section("Searches", "The real search, replayed over each take at the thresholds in use. Counts are of searches: questions times takes.")
	outcomes := r.outcomes(inUse)

	count := func(kind Kind, which ...Outcome) (n, total int) {
		for i, q := range r.set.Questions {
			if q.Kind != kind {
				continue
			}
			for _, outcome := range outcomes[i] {
				total++
				if slices.Contains(which, outcome) {
					n++
				}
			}
		}
		return n, total
	}
	questions := func(kind Kind) int {
		n := 0
		for _, q := range r.set.Questions {
			if q.Kind == kind {
				n++
			}
		}
		return n
	}

	r.line("Questions the graph can answer:")
	r.row("  kind\tquestions\tright: stopped with the answer\tstopped without it\thad it, did not stop\tnever had it\t")
	for _, kind := range allKinds {
		if answerable, _ := kind.answerable(); !answerable || questions(kind) == 0 {
			continue
		}
		right, total := count(kind, Answered)
		short, _ := count(kind, StoppedShort)
		wentOn, _ := count(kind, WentOn)
		never, _ := count(kind, NeverHadIt)
		r.row("  %s\t%d\t%s\t%s\t%s\t%s\t", kind, questions(kind), share(right, total), share(short, total), share(wentOn, total), share(never, total))
	}
	r.endTable()

	r.line("")
	r.line("Questions the graph cannot answer:")
	r.row("  kind\tquestions\tright: did not stop\tstopped anyway\treturned documents all the same\t")
	for _, kind := range allKinds {
		if answerable, _ := kind.answerable(); answerable || questions(kind) == 0 {
			continue
		}
		right, total := count(kind, HeldBack)
		wrong, _ := count(kind, StoppedAnyway)
		withEvidence := 0
		for _, take := range r.results.Takes {
			for _, q := range r.set.Questions {
				if search, ok := take.search(q.ID); ok && q.Kind == kind && len(search.Evidence) > 0 {
					withEvidence++
				}
			}
		}
		r.row("  %s\t%d\t%s\t%s\t%s\t", kind, questions(kind), share(right, total), share(wrong, total), share(withEvidence, total))
	}
	r.endTable()

	// One number for the lot, and how far it can be trusted.
	var perQuestion []float64
	right, total, failed := 0, 0, 0
	unsteady := 0
	for i := range r.set.Questions {
		if len(outcomes[i]) == 0 {
			continue
		}
		good := 0
		for _, outcome := range outcomes[i] {
			total++
			if outcome.Right() {
				good++
				right++
			}
			if outcome == Failed {
				failed++
			}
		}
		perQuestion = append(perQuestion, float64(good)/float64(len(outcomes[i])))
		if slices.ContainsFunc(outcomes[i], func(o Outcome) bool { return o != outcomes[i][0] }) {
			unsteady++
		}
	}
	low, high := margin(perQuestion)
	r.line("")
	r.line("Right in all: %s of searches. Drawing the questions again would put that between %.0f%% and %.0f%%:",
		strings.TrimSpace(share(right, total)), 100*low, 100*high)
	r.line("with %d questions, a difference inside that range is not a finding.", len(perQuestion))
	if failed > 0 {
		r.line("%d searches failed outright and are counted as wrong.", failed)
	}
	r.line("%d of %d questions did not come out the same way in every take.", unsteady, len(perQuestion))

	if notes := r.needingNotes(); len(notes) > 0 {
		r.line("Questions that need a link's note to be answered: %d (%s). The judge of evidence is not shown notes.",
			len(notes), strings.Join(notes, ", "))
	}
	r.effort()
}

// needingNotes returns the IDs of the questions that need a link's note.
func (r *report) needingNotes() []string {
	var ids []string
	for _, q := range r.set.Questions {
		if q.NeedsNote() {
			ids = append(ids, q.ID)
		}
	}
	return ids
}

// effort says what the searches cost and how much of what they returned was
// worth returning.
func (r *report) effort() {
	var steps, times, leads, readings, searches float64
	var covered, needed, returned, stating float64
	for _, take := range r.results.Takes {
		for _, q := range r.set.Questions {
			search, ok := take.search(q.ID)
			if !ok {
				continue
			}
			searches++
			steps += float64(search.Steps)
			times += float64(search.Asked.Times)
			leads += float64(search.Asked.Leads)
			readings += float64(search.Asked.Readings)

			c, n := q.Covered(evidenceOf(search))
			covered, needed = covered+float64(c), needed+float64(n)
			for _, found := range search.Evidence {
				returned++
				if q.belongs(found.Document) {
					stating++
				}
			}
		}
	}
	if searches == 0 {
		return
	}
	r.line("")
	if needed > 0 {
		r.line("Of the facts the answers need, the evidence returned states %.0f%%.", 100*covered/needed)
	}
	if returned > 0 {
		r.line("Of the documents returned as evidence, %.0f%% belong there (%.0f of %.0f): they state a fact of the answer,", 100*stating/returned, stating, returned)
		r.line("or are joined by a link whose note does.")
	}
	r.line("A search took %.1f steps and asked the judge %.1f times: %.1f leads rated and %.1f readings of the evidence.",
		steps/searches, times/searches, leads/searches, readings/searches)
}

// wrong lists, by name, every question whose search went wrong in some take,
// with what there is to see of why.
func (r *report) wrong() {
	outcomes := r.outcomes(inUse)
	var lines []string
	for i, q := range r.set.Questions {
		if !slices.ContainsFunc(outcomes[i], func(o Outcome) bool { return !o.Right() }) {
			continue
		}
		counts := make(map[Outcome]int)
		for _, outcome := range outcomes[i] {
			counts[outcome]++
		}
		var parts []string
		for _, outcome := range []Outcome{Answered, HeldBack, StoppedShort, WentOn, NeverHadIt, StoppedAnyway, Failed} {
			if counts[outcome] > 0 {
				parts = append(parts, fmt.Sprintf("%s in %d of %d takes", outcome, counts[outcome], len(outcomes[i])))
			}
		}
		lines = append(lines, fmt.Sprintf("  %s (%s): %s.", q.ID, q.Kind, strings.Join(parts, "; ")))
		lines = append(lines, r.why(q)...)
	}
	if len(lines) == 0 {
		return
	}
	r.section("What went wrong", "Every question whose search went wrong in some take, and what there is to see of why.")
	for _, line := range lines {
		r.line("%s", line)
	}
}

// why gathers what the takes show about a question that went wrong: how its
// evidence was read, and what became of the documents that state its facts.
func (r *report) why(q Question) []string {
	var lines []string

	var readings []float64
	for _, take := range r.results.Takes {
		if search, ok := take.search(q.ID); ok && len(search.Evidence) > 0 {
			readings = append(readings, search.Sufficient)
		}
	}
	if len(readings) > 0 {
		lines = append(lines, fmt.Sprintf("      the judge read its evidence at %s", between(readings)))
	}

	// Each document the answer needs: how it was rated where the searches
	// met it, or that they never did.
	for _, document := range q.gatherable() {
		var met []float64
		byLink := false
		for _, take := range r.results.Takes {
			search, ok := take.search(q.ID)
			if !ok {
				continue
			}
			for _, rating := range search.Met {
				if rating.Target == document {
					met = append(met, rating.Potential)
					byLink = byLink || !rating.Seed()
				}
			}
		}
		how := "as a place to start"
		if byLink {
			how = "by a link"
		}
		switch {
		case len(met) == 0:
			lines = append(lines, fmt.Sprintf("      %s was never met by a search", short(document)))
		default:
			lines = append(lines, fmt.Sprintf("      %s was met %s and rated %s", short(document), how, between(met)))
		}
	}

	// What was returned that states nothing.
	seen := make(map[graph.ID][]float64)
	for _, take := range r.results.Takes {
		if search, ok := take.search(q.ID); ok {
			for _, found := range search.Evidence {
				if !q.belongs(found.Document) {
					seen[found.Document] = append(seen[found.Document], found.Potential)
				}
			}
		}
	}
	for _, document := range sortedKeys(seen) {
		lines = append(lines, fmt.Sprintf("      %s states none of the answer and was returned as evidence in %s, rated %s",
			short(document), takes(len(seen[document])), between(seen[document])))
	}
	return lines
}

// gatherable returns the documents a search has to gather for the question.
func (q Question) gatherable() []graph.ID {
	if q.NeedsNote() {
		return q.joined()
	}
	return q.answer()
}

// --- ratings --------------------------------------------------------------------

// standing says what a vertex is to a question, by its labels.
type standing int

const (
	unlabelled standing = iota // nothing says anything about it
	nearMiss                   // on the subject, states nothing
	states                     // states a fact of the answer
)

// standing says what a lead is to the question. A lead shows a fact if the
// vertex it leads to states one, or if the edge it arrives by does: the judge
// is shown that edge's note beside the vertex.
func (q Question) standing(rating Rating) standing {
	onEdge := rating.Edge != (graph.Edge{}) && slices.ContainsFunc(q.Facts, func(fact Fact) bool {
		return slices.ContainsFunc(fact.Stated, func(s Stating) bool {
			return s.Document == "" && s.Edge.Triple() == rating.Edge.Triple()
		})
	})
	switch {
	case q.States(rating.Target) || onEdge:
		return states
	case slices.Contains(q.Near, rating.Target):
		return nearMiss
	}
	return unlabelled
}

// belongs reports whether a document is a right thing for a search to return
// as evidence: it states a fact of the answer, or it is one of the documents
// joined by a link whose note states one.
func (q Question) belongs(document graph.ID) bool {
	return q.States(document) || (q.NeedsNote() && slices.Contains(q.joined(), document))
}

// leadCounts counts leads by what the labels say of them and what a
// threshold makes of their rating.
type leadCounts struct {
	answers, answersOver int // documents that state a fact, each at its best rating
	near, nearOver       int // leads to near misses
	other, otherOver     int // leads to anything else
}

func (c *leadCounts) add(other leadCounts) {
	c.answers, c.answersOver = c.answers+other.answers, c.answersOver+other.answersOver
	c.near, c.nearOver = c.near+other.near, c.nearOver+other.nearOver
	c.other, c.otherOver = c.other+other.other, c.otherOver+other.otherOver
}

// countLeads counts the ratings of one take against a threshold. A document
// that states a fact is counted once, at the best rating any way of reaching
// it got: it is found if some way of coming across it shows it for what it
// is. Everything else is counted lead by lead, since each lead is a chance
// to be taken in.
func (r *report) countLeads(ratings []Rating, threshold float64) leadCounts {
	var counts leadCounts
	type pair struct {
		question string
		target   graph.ID
	}
	best := make(map[pair]float64)
	var order []pair
	for _, rating := range ratings {
		q, ok := r.set.question(rating.Question)
		if !ok {
			continue
		}
		switch q.standing(rating) {
		case states:
			key := pair{rating.Question, rating.Target}
			if _, seen := best[key]; !seen {
				order = append(order, key)
				best[key] = rating.Potential
			}
			best[key] = max(best[key], rating.Potential)
		case nearMiss:
			counts.near++
			if rating.Potential >= threshold {
				counts.nearOver++
			}
		default:
			counts.other++
			if rating.Potential >= threshold {
				counts.otherOver++
			}
		}
	}
	for _, key := range order {
		counts.answers++
		if best[key] >= threshold {
			counts.answersOver++
		}
	}
	return counts
}

// pooled counts the leads of every take against a threshold: once over the
// leads the searches met, and once over every lead there is.
func (r *report) pooled(threshold float64) (met, every leadCounts) {
	for _, take := range r.results.Takes {
		var seen []Rating
		for _, q := range r.set.Questions {
			if search, ok := take.search(q.ID); ok {
				seen = append(seen, search.Met...)
			}
		}
		met.add(r.countLeads(seen, threshold))
		every.add(r.countLeads(take.Ratings, threshold))
	}
	return met, every
}

func (r *report) ratings() {
	s := r.results.Settings
	r.section("Ratings", fmt.Sprintf(
		"What the judge made of each lead. At %.2f or over a followed document counts as evidence; from %.2f a lead is followed.\n"+
			"Counted twice: over the leads the searches met, and over every lead there is (every document for every question,\n"+
			"which is mostly easy cases and flatters any threshold).", s.MinEvidence, s.MinPotential))

	met, every := r.pooled(s.MinEvidence)
	r.row("  \tas searches met them\tover every lead\t")
	r.row("  documents that state a fact, rated as evidence\t%s\t%s\t", share(met.answersOver, met.answers), share(every.answersOver, every.answers))
	r.row("  near misses rated as evidence\t%s\t%s\t", share(met.nearOver, met.near), share(every.nearOver, every.near))
	r.row("  other leads rated as evidence\t%s\t%s\t", share(met.otherOver, met.other), share(every.otherOver, every.other))
	metP, everyP := r.pooled(s.MinPotential)
	r.row("  documents that state a fact, followed\t%s\t%s\t", share(metP.answersOver, metP.answers), share(everyP.answersOver, everyP.answers))
	r.row("  leads that state nothing, followed\t%s\t%s\t",
		share(metP.nearOver+metP.otherOver, metP.near+metP.other), share(everyP.nearOver+everyP.otherOver, everyP.near+everyP.other))
	r.endTable()

	r.line("")
	r.line("If the evidence threshold were elsewhere (over the leads the searches met at today's thresholds):")
	r.row("  threshold\tfact-stating documents missed\tnear misses admitted\tother leads admitted\t")
	for _, threshold := range []float64{0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90} {
		counts, _ := r.pooled(threshold)
		mark := ""
		if threshold == s.MinEvidence {
			mark = "  in use"
		}
		r.row("  %.2f%s\t%s\t%s\t%s\t", threshold, mark,
			share(counts.answers-counts.answersOver, counts.answers), share(counts.nearOver, counts.near), share(counts.otherOver, counts.other))
	}
	r.endTable()

	r.byRoute()
	r.steadiness()
}

// byRoute shows the documents that the judge rates differently as a place to
// start and at the end of a link, where the difference is large.
func (r *report) byRoute() {
	type pair struct {
		question string
		target   graph.ID
	}
	seed, link := make(map[pair][]float64), make(map[pair]float64)
	var order []pair
	for _, series := range r.results.ratingSeries(r.set) {
		q, _ := r.set.question(series.Question)
		if q.standing(series.Rating) != states {
			continue
		}
		key := pair{series.Question, series.Target}
		if series.Seed() {
			seed[key] = series.Values
			order = append(order, key)
			continue
		}
		link[key] = max(link[key], mean(series.Values))
	}
	var lines []string
	for _, key := range order {
		byLink, reached := link[key]
		if !reached {
			continue
		}
		if asSeed := mean(seed[key]); byLink-asSeed >= 0.15 || asSeed-byLink >= 0.15 {
			lines = append(lines, fmt.Sprintf("  %s, %s: %.2f as a place to start, %.2f at the end of a link", key.question, short(key.target), asSeed, byLink))
		}
	}
	if len(lines) == 0 {
		return
	}
	r.line("")
	r.line("Fact-stating documents rated differently by the way they are reached. A search rates a document once,")
	r.line("by the first way it meets it:")
	for _, line := range lines {
		r.line("%s", line)
	}
}

// steadiness says how far the takes lie apart, and how often that carries a
// rating across a threshold.
func (r *report) steadiness() {
	if len(r.results.Takes) < 2 {
		return
	}
	s := r.results.Settings
	var spreads []float64
	overEvidence, overFollow := 0, 0
	for _, series := range r.results.ratingSeries(r.set) {
		spreads = append(spreads, spread(series.Values))
		low, high := slices.Min(series.Values), slices.Max(series.Values)
		if low < s.MinEvidence && high >= s.MinEvidence {
			overEvidence++
		}
		if low < s.MinPotential && high >= s.MinPotential {
			overFollow++
		}
	}
	if len(spreads) == 0 {
		return
	}
	r.line("")
	r.line("Between takes, a rating moved by %.2f at the median and %.2f at the most. Of %d ratings, %d fell on both sides",
		median(spreads), slices.Max(spreads), len(spreads), overEvidence)
	r.line("of the evidence threshold from one take to another, and %d on both sides of the follow threshold.", overFollow)
}

// --- sufficiency ----------------------------------------------------------------

func (r *report) sufficiency() {
	s := r.results.Settings
	r.section("Sufficiency", fmt.Sprintf(
		"How the judge read sets of evidence. A reading of %.2f or over stops a search.", s.Sufficient))

	type group struct {
		sets     map[string]bool
		readings []float64
	}
	groups := make(map[string]*group)
	add := func(name string, reading Reading) {
		g := groups[name]
		if g == nil {
			g = &group{sets: make(map[string]bool)}
			groups[name] = g
		}
		g.sets[reading.Question+"\x00"+fmt.Sprint(reading.Evidence)] = true
		g.readings = append(g.readings, reading.Sufficient)
	}
	var all []Reading
	for _, take := range r.results.Takes {
		for _, reading := range take.Readings {
			if !r.set.has(reading.Question) {
				continue
			}
			all = append(all, reading)
			name := string(reading.Case)
			if reading.Case == MetBySearch {
				name += ", " + verdictOf(reading)
			}
			add(name, reading)
		}
	}

	over := func(readings []float64, threshold float64) int {
		n := 0
		for _, reading := range readings {
			if reading >= threshold {
				n++
			}
		}
		return n
	}
	r.row("  evidence\tsets\tshould read as\tread as enough\tmean reading\t")
	for _, c := range []struct {
		name, should string
	}{
		{string(WholeAnswer), "enough"},
		{string(WithExtras), "enough"},
		{string(WithNearMisses), "enough"},
		{string(LessOneDocument), "not enough"},
		{string(ExtrasOnly), "not enough"},
		{string(NearMissesOnly), "not enough"},
		{string(AllButTheNote), "enough, if the note were shown"},
		{string(MetBySearch) + ", enough", "enough"},
		{string(MetBySearch) + ", not enough", "not enough"},
	} {
		g := groups[c.name]
		if g == nil {
			continue
		}
		r.row("  %s\t%d\t%s\t%s\t%.2f\t", c.name, len(g.sets), c.should, share(over(g.readings, s.Sufficient), len(g.readings)), mean(g.readings))
	}
	r.endTable()

	var enough, short []float64
	for _, reading := range all {
		switch {
		case reading.Case == AllButTheNote:
			// Neither: what it should read as depends on a note the judge
			// is not shown.
		case reading.Enough:
			enough = append(enough, reading.Sufficient)
		default:
			short = append(short, reading.Sufficient)
		}
	}
	r.line("")
	r.line("If the stop threshold were elsewhere (over every set above but the ones that need a note):")
	r.row("  threshold\tenough, and read as enough\tnot enough, and read as enough\t")
	for _, threshold := range []float64{0.40, 0.50, 0.60, 0.70, 0.80, 0.90} {
		mark := ""
		if threshold == s.Sufficient {
			mark = "  in use"
		}
		r.row("  %.2f%s\t%s\t%s\t", threshold, mark, share(over(enough, threshold), len(enough)), share(over(short, threshold), len(short)))
	}
	r.endTable()
	if len(enough) > 0 && len(short) > 0 {
		r.line("")
		r.line("Readings of evidence that is enough: %s. Of evidence that is not: %s.", between(enough), between(short))
	}
}

func verdictOf(reading Reading) string {
	if reading.Enough {
		return "enough"
	}
	return "not enough"
}

// --- other settings ---------------------------------------------------------------

func (r *report) settings() {
	runs := 0
	for _, take := range r.results.Takes {
		runs = max(runs, len(take.Runs))
	}
	if runs < 2 {
		return
	}
	r.section("Other thresholds", "The same takes, searched again under the settings one step to each side of each threshold.\n"+
		"A setting is worth moving to only if it beats the one in use by more than the margin given under Searches.")

	r.row("  stop at\tfollow from\tevidence from\tright\tstopped wrongly\tleads rated per search\t")
	for i := range runs {
		pick := func(take Take) (Run, bool) {
			if i >= len(take.Runs) {
				return Run{}, false
			}
			return take.Runs[i], true
		}
		var settings Settings
		var leads, searches float64
		for _, take := range r.results.Takes {
			if run, ok := pick(take); ok {
				settings = run.Settings
				for _, q := range r.set.Questions {
					if search, found := run.search(q.ID); found {
						leads += float64(search.Asked.Leads)
						searches++
					}
				}
			}
		}
		right, wrongStops, total := 0, 0, 0
		for _, perQuestion := range r.outcomes(pick) {
			for _, outcome := range perQuestion {
				total++
				if outcome.Right() {
					right++
				}
				if outcome == StoppedShort || outcome == StoppedAnyway {
					wrongStops++
				}
			}
		}
		mark := ""
		if settings == r.results.Settings {
			mark = "  in use"
		}
		perSearch := 0.0
		if searches > 0 {
			perSearch = leads / searches
		}
		r.row("  %.2f\t%.2f\t%.2f\t%s\t%s\t%.1f%s\t", settings.Sufficient, settings.MinPotential, settings.MinEvidence,
			share(right, total), share(wrongStops, total), perSearch, mark)
	}
	r.endTable()
}

// --- surprises --------------------------------------------------------------------

// surprises lists the leads the judge rated as evidence, on average, that no
// label says anything about. Each is for a person to settle: either the
// label is missing, or the judge was wrong.
func (r *report) surprises() {
	s := r.results.Settings
	type surprise struct {
		line string
		mean float64
	}
	var found []surprise
	sometimes := 0
	for _, series := range r.results.ratingSeries(r.set) {
		q, _ := r.set.question(series.Question)
		if q.standing(series.Rating) != unlabelled {
			continue
		}
		if slices.Max(series.Values) >= s.MinEvidence {
			sometimes++
		}
		if average := mean(series.Values); average >= s.MinEvidence {
			how := "as a place to start"
			if !series.Seed() {
				how = "reached from " + short(series.From)
			}
			found = append(found, surprise{mean: average, line: fmt.Sprintf("  %s: %s, %s, rated %s. %q",
				series.Question, short(series.Target), how, between(series.Values), r.title(series.Target))})
		}
	}
	r.section("Surprises", fmt.Sprintf(
		"Leads that no label mentions and the judge rated as evidence (%.2f or over on average). Each is for a person\n"+
			"to settle: the labels missed a document that does answer, or the judge was wrong.", s.MinEvidence))
	if len(found) == 0 {
		r.line("  None.")
	}
	slices.SortStableFunc(found, func(a, b surprise) int { return cmp.Compare(b.mean, a.mean) })
	for _, each := range found {
		r.line("%s", each.line)
	}
	r.line("")
	r.line("  Unlabelled leads that reached that rating in at least one take: %d.", sometimes)
}

// --- the live check -----------------------------------------------------------------

// live sets the searches that were asked of Jev directly beside the same
// searches replayed from the tape. If replaying is sound, a live search
// comes out the way some take's replay does, as often as one take's replay
// comes out the way another's does.
func (r *report) live() {
	if len(r.results.Live) == 0 {
		return
	}
	r.section("Replayed against live", "One search per question was asked of Jev directly. Replaying is sound if those come out as the replays do.")

	asked, likeSome, likeMost := 0, 0, 0
	var lines []string
	outcomes := r.outcomes(inUse)
	for i, q := range r.set.Questions {
		at := slices.IndexFunc(r.results.Live, func(search Search) bool { return search.Question == q.ID })
		if at < 0 || len(outcomes[i]) == 0 {
			continue
		}
		asked++
		lived := q.Outcome(r.results.Live[at])
		if slices.Contains(outcomes[i], lived) {
			likeSome++
		}
		if lived == commonest(outcomes[i]) {
			likeMost++
			continue
		}
		lines = append(lines, fmt.Sprintf("  %s: live it %s; replayed it mostly %s (in %d of %d takes it came out as it did live)",
			q.ID, lived, commonest(outcomes[i]), countOf(outcomes[i], lived), len(outcomes[i])))
	}
	r.line("Of %d live searches, %d came out as most replays of the same question did, and %d as at least one did.", asked, likeMost, likeSome)
	for _, line := range lines {
		r.line("%s", line)
	}
}

// --- set against an earlier run -------------------------------------------------------

func (r *report) against(earlier Saved) {
	now := r.results.Summary(r.set, "", earlier.When)
	r.section("Set against "+cmp.Or(earlier.Label, "an earlier run"), fmt.Sprintf(
		"Saved %s: judge %s, %s. Only what moved by more than the takes themselves differ is listed.",
		earlier.When.Format("2 January 2006"), cmp.Or(earlier.Model, "unnamed"), takes(earlier.Takes)))

	rightOf := func(searches []SavedSearch) (right, total int) {
		for _, search := range searches {
			if !r.set.has(search.Question) {
				continue
			}
			for _, outcome := range search.Outcomes {
				total++
				if outcome.Right() {
					right++
				}
			}
		}
		return right, total
	}
	thenRight, thenTotal := rightOf(earlier.Searches)
	nowRight, nowTotal := rightOf(now.Searches)
	r.line("Searches right: %s then, %s now.", strings.TrimSpace(share(thenRight, thenTotal)), strings.TrimSpace(share(nowRight, nowTotal)))

	var moved []string
	for _, search := range now.Searches {
		at := slices.IndexFunc(earlier.Searches, func(then SavedSearch) bool { return then.Question == search.Question })
		if at < 0 || len(search.Outcomes) == 0 || len(earlier.Searches[at].Outcomes) == 0 {
			continue
		}
		if was, is := commonest(earlier.Searches[at].Outcomes), commonest(search.Outcomes); was != is {
			moved = append(moved, fmt.Sprintf("  %s: mostly %s then, mostly %s now", search.Question, was, is))
		}
	}
	r.listed("Searches that mostly come out another way", moved)

	moved = nil
	for _, rating := range now.Ratings {
		at := slices.IndexFunc(earlier.Ratings, func(then SavedRating) bool {
			return then.Question == rating.Question && then.Target == rating.Target && then.Edge.Triple() == rating.Edge.Triple()
		})
		if at < 0 {
			continue
		}
		then := earlier.Ratings[at]
		if beyondNoise(then.Mean, then.Low, then.High, rating.Mean, rating.Low, rating.High) {
			how := "as a place to start"
			if rating.Edge != (graph.Edge{}) {
				how = "by a link"
			}
			moved = append(moved, fmt.Sprintf("  %s, %s %s: %.2f then, %.2f now", rating.Question, short(rating.Target), how, then.Mean, rating.Mean))
		}
	}
	r.listed("Ratings that moved", moved)

	moved = nil
	for _, reading := range now.Readings {
		at := slices.IndexFunc(earlier.Readings, func(then SavedReading) bool {
			return then.Question == reading.Question && slices.Equal(then.Evidence, reading.Evidence)
		})
		if at < 0 {
			continue
		}
		then := earlier.Readings[at]
		if beyondNoise(then.Mean, then.Low, then.High, reading.Mean, reading.Low, reading.High) {
			moved = append(moved, fmt.Sprintf("  %s, %s: %.2f then, %.2f now", reading.Question, shorts(reading.Evidence), then.Mean, reading.Mean))
		}
	}
	r.listed("Readings that moved", moved)
}

// listed writes a list under a title, or says that there is nothing to list.
func (r *report) listed(title string, lines []string) {
	r.line("")
	if len(lines) == 0 {
		r.line("%s: none.", title)
		return
	}
	r.line("%s (%d):", title, len(lines))
	for _, line := range lines {
		r.line("%s", line)
	}
}

// beyondNoise reports whether a mean moved by more than the takes of either
// run lie apart, and by at least a twentieth: a move that chance alone would
// not make.
func beyondNoise(thenMean, thenLow, thenHigh, nowMean, nowLow, nowHigh float64) bool {
	moved := nowMean - thenMean
	if moved < 0 {
		moved = -moved
	}
	return moved > max(0.05, thenHigh-thenLow, nowHigh-nowLow)
}

// --- small things -----------------------------------------------------------------

// between writes the range some numbers cover, or the one number if they are
// all the same.
func between(values []float64) string {
	low, high := round2(slices.Min(values)), round2(slices.Max(values))
	if low == high {
		return fmt.Sprintf("%.2f", low)
	}
	return fmt.Sprintf("%.2f to %.2f", low, high)
}

// takes writes a number of takes.
func takes(n int) string {
	if n == 1 {
		return "1 take"
	}
	return fmt.Sprintf("%d takes", n)
}

// short writes an ID without the prefix that every document shares.
func short(id graph.ID) string {
	if id.Kind() == graph.KindDocument {
		_, key, _ := strings.Cut(id.Key(), ":")
		return key
	}
	return string(id)
}

func shorts(ids []graph.ID) string {
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = short(id)
	}
	return strings.Join(names, " + ")
}

// title returns the title of a document, or the name of a topic.
func (r *report) title(id graph.ID) string {
	switch vertex := r.set.Graph.vertices()[id].(type) {
	case *graph.Document:
		return vertex.Title
	case *graph.Topic:
		return vertex.Name
	}
	return ""
}

// commonest returns the outcome that came up most often; the earlier of two
// that came up equally often.
func commonest(outcomes []Outcome) Outcome {
	var best Outcome
	most := 0
	for _, outcome := range outcomes {
		if n := countOf(outcomes, outcome); n > most {
			best, most = outcome, n
		}
	}
	return best
}

func countOf(outcomes []Outcome, which Outcome) int {
	n := 0
	for _, outcome := range outcomes {
		if outcome == which {
			n++
		}
	}
	return n
}

func sortedKeys[V any](m map[graph.ID]V) []graph.ID {
	keys := make([]graph.ID, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

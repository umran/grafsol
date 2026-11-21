package mapping_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/mapping"
)

// The document every test's references are found in.
const document = graph.ID("doc:github:acme/api#1")

func issue(repository string, number int) graph.ID {
	return graph.DocumentID("github", fmt.Sprintf("%s#%d", repository, number))
}

var (
	bareMention = regexp.MustCompile(`^#(\d+)$`)
	fullMention = regexp.MustCompile(`^([\w.-]+/[\w.-]+)#(\d+)$`)
	issueURL    = regexp.MustCompile(`^https://github\.com/([\w.-]+/[\w.-]+)/(?:issues|pull)/(\d+)`)
)

// targetOf says which document a reference written in acme/api names.
func targetOf(t *testing.T, written string) graph.ID {
	t.Helper()
	var repository, number string
	switch {
	case bareMention.MatchString(written):
		repository, number = "acme/api", bareMention.FindStringSubmatch(written)[1]
	case fullMention.MatchString(written):
		m := fullMention.FindStringSubmatch(written)
		repository, number = strings.ToLower(m[1]), m[2]
	case issueURL.MatchString(written):
		m := issueURL.FindStringSubmatch(written)
		repository, number = strings.ToLower(m[1]), m[2]
	default:
		t.Fatalf("the test does not know what %q refers to", written)
	}
	var n int
	fmt.Sscan(number, &n)
	return issue(repository, n)
}

// mentions returns the references written on one line of acme/api's text.
func mentions(t *testing.T, line string, written ...string) []graph.Reference {
	t.Helper()
	references := make([]graph.Reference, len(written))
	for i, w := range written {
		if !strings.Contains(line, w) {
			t.Fatalf("the line %q does not hold %q", line, w)
		}
		references[i] = graph.Reference{Target: targetOf(t, w), Form: graph.FormMention, Written: w, Context: line}
	}
	return references
}

// kindsOf returns the kind of the edge made to each target.
func kindsOf(edges []graph.Edge) map[graph.ID]graph.EdgeKind {
	kinds := make(map[graph.ID]graph.EdgeKind, len(edges))
	for _, edge := range edges {
		kinds[edge.To] = edge.Kind
	}
	return kinds
}

// The team's own rules in the tests: the words one team writes before the
// documents its pull requests carry out.
var team = mapping.Rules{
	{ID: "implements", Source: "github", Form: graph.FormMention, After: []string{"implements", "addresses"}, Kind: mapping.Resolves},
	{ID: "rework", Source: "github", Form: graph.FormMention, After: []string{"rework of"}, Kind: mapping.Supersedes},
}

// Every line here was written in a real issue or pull request and is kept in
// one of the two graphs this was designed against. The kind is what the line
// should become: under GitHub's defaults alone, and with the team's rules
// laid over them.
func TestWhatARealLineBecomes(t *testing.T) {
	cases := []struct {
		line     string
		written  []string
		defaults []graph.EdgeKind // one per reference
		withTeam []graph.EdgeKind // nil when the team's rules change nothing
	}{
		{line: "implements #9 #16 #17 #18", written: []string{"#9", "#16", "#17", "#18"},
			defaults: []graph.EdgeKind{graph.LinksTo, graph.LinksTo, graph.LinksTo, graph.LinksTo},
			withTeam: []graph.EdgeKind{mapping.Resolves, mapping.Resolves, mapping.Resolves, mapping.Resolves}},
		{line: "implements #10 and #19", written: []string{"#10", "#19"},
			defaults: []graph.EdgeKind{graph.LinksTo, graph.LinksTo},
			withTeam: []graph.EdgeKind{mapping.Resolves, mapping.Resolves}},
		{line: "This PR addresses #2 and implements the following towards our documentation requirements:", written: []string{"#2"},
			defaults: []graph.EdgeKind{graph.LinksTo},
			withTeam: []graph.EdgeKind{mapping.Resolves}},
		{line: "This implements #15", written: []string{"#15"},
			defaults: []graph.EdgeKind{graph.LinksTo},
			withTeam: []graph.EdgeKind{mapping.Resolves}},
		{line: "Implements #25", written: []string{"#25"},
			defaults: []graph.EdgeKind{graph.LinksTo},
			withTeam: []graph.EdgeKind{mapping.Resolves}},
		{line: "As described in issue #25, my application hangs on the statement `defer p.destructWG.Wait()`", written: []string{"#25"},
			defaults: []graph.EdgeKind{graph.LinksTo}},
		{line: "Will close #19", written: []string{"#19"},
			defaults: []graph.EdgeKind{mapping.Resolves}},
		{line: "Fixes https://github.com/jackc/pgx/issues/1287", written: []string{"https://github.com/jackc/pgx/issues/1287"},
			defaults: []graph.EdgeKind{mapping.Resolves}},
		{line: "Fixes #46.", written: []string{"#46"},
			defaults: []graph.EdgeKind{mapping.Resolves}},
		{line: "fixes #36", written: []string{"#36"},
			defaults: []graph.EdgeKind{mapping.Resolves}},
		{line: "Backporting #21 and #22", written: []string{"#21", "#22"},
			defaults: []graph.EdgeKind{graph.LinksTo, graph.LinksTo}},
		{line: "This PR is a rework of #29.", written: []string{"#29"},
			defaults: []graph.EdgeKind{graph.LinksTo},
			withTeam: []graph.EdgeKind{mapping.Supersedes}},
		{line: "Details in #8", written: []string{"#8"},
			defaults: []graph.EdgeKind{graph.LinksTo}},
		{line: "Since #35 merged, it seems the only usage of `unsafe` in this library is in log.go", written: []string{"#35"},
			defaults: []graph.EdgeKind{graph.LinksTo}},
		{line: "#24 replaced the previous `sync.Cond` design for a scheduler reason too", written: []string{"#24"},
			defaults: []graph.EdgeKind{graph.LinksTo}},
		{line: "I think issue #5 is related both to pull request #12 and this one.", written: []string{"#5", "#12"},
			defaults: []graph.EdgeKind{graph.LinksTo, graph.LinksTo}},
		{line: "#16", written: []string{"#16"},
			defaults: []graph.EdgeKind{graph.LinksTo}},
	}

	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			references := mentions(t, c.line, c.written...)
			check := func(name string, rules mapping.Rules, want []graph.EdgeKind) {
				t.Helper()
				made := rules.Apply(document, "github", references, nil)
				kinds := kindsOf(made.Edges)
				for i, reference := range references {
					if got := kinds[reference.Target]; got != want[i] {
						t.Errorf("%s: %s became %q, want %q", name, reference.Written, got, want[i])
					}
				}
			}
			check("GitHub's defaults", nil, c.defaults)
			check("with the team's rules", team, cmp(c.withTeam, c.defaults))
		})
	}
}

func cmp(first, otherwise []graph.EdgeKind) []graph.EdgeKind {
	if first != nil {
		return first
	}
	return otherwise
}

// What "after one of these words" means, at its edges.
func TestAfterAWord(t *testing.T) {
	cases := []struct {
		line    string
		written string
		want    graph.EdgeKind
	}{
		// Joining words and other references may come between.
		{"closes #2, #3 and #4", "#4", mapping.Resolves},
		{"Fixes the issue #12", "#12", mapping.Resolves},
		{"Resolves: #12", "#12", mapping.Resolves},
		{"fixes pull request #12", "#12", mapping.Resolves},
		{"fixes acme/web#3 and #12", "#12", mapping.Resolves},
		// The word is matched whole, whatever its case.
		{"CLOSES #7", "#7", mapping.Resolves},
		{"prefixes #12", "#12", graph.LinksTo},
		{"hotfix #12", "#12", graph.LinksTo},
		{"closes-out #12", "#12", graph.LinksTo},
		// Anything else in between breaks it.
		{"fix: see #12", "#12", graph.LinksTo},
		{"fixes a typo, unrelated to #12", "#12", graph.LinksTo},
		// It has to come before the reference, on the same line.
		{"#12 fixes this", "#12", graph.LinksTo},
		// A reference is found where it stands whole, not inside a longer one.
		{"fixes #124, see #12", "#12", graph.LinksTo},
		// Of two words, the nearer one decides.
		{"fixes #2 and is a duplicate of #12", "#12", mapping.Duplicates},
		{"is a duplicate of #2 and fixes #12", "#12", mapping.Resolves},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			var others []string
			for _, other := range []string{"#2", "#3", "#4", "#124", "acme/web#3"} {
				if other != c.written && strings.Contains(c.line, other) {
					others = append(others, other)
				}
			}
			references := mentions(t, c.line, append(others, c.written)...)
			made := mapping.Rules(nil).Apply(document, "github", references, nil)
			if got := kindsOf(made.Edges)[targetOf(t, c.written)]; got != c.want {
				t.Errorf("%s became %q, want %q", c.written, got, c.want)
			}
		})
	}
}

func TestAloneOnItsLine(t *testing.T) {
	alone := mapping.Rules{{ID: "alone", Source: "github", Form: graph.FormMention, Alone: true, Kind: graph.ChildOf}}
	cases := []struct {
		line    string
		written string
		want    graph.EdgeKind
	}{
		{"#16", "#16", graph.ChildOf},
		{"  #16  ", "#16", graph.ChildOf},
		{"Acme/Documentation#34", "Acme/Documentation#34", graph.ChildOf},
		{"https://github.com/acme/api/issues/16", "https://github.com/acme/api/issues/16", graph.ChildOf},
		{"see #16", "#16", graph.LinksTo},
		{"#16.", "#16", graph.LinksTo},
		{"#16 #17", "#16", graph.LinksTo},
	}
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			references := mentions(t, c.line, c.written)
			made := alone.Apply(document, "github", references, nil)
			if got := kindsOf(made.Edges)[targetOf(t, c.written)]; got != c.want {
				t.Errorf("%s became %q, want %q", c.written, got, c.want)
			}
		})
	}
}

// The team's rules are tried in the order the team put them, before the
// source's own: the first that matches settles it.
func TestTheFirstRuleThatMatchesWins(t *testing.T) {
	references := mentions(t, "fixes #12", "#12")
	target := targetOf(t, "#12")

	blocks := mapping.Rule{ID: "a", Source: "github", Form: graph.FormMention, After: []string{"fixes"}, Kind: mapping.Blocks}
	supersedes := mapping.Rule{ID: "b", Source: "github", Form: graph.FormMention, After: []string{"fixes"}, Kind: mapping.Supersedes}

	for _, c := range []struct {
		rules mapping.Rules
		want  graph.EdgeKind
		rule  string
	}{
		{nil, mapping.Resolves, "github-closing-words"},
		{mapping.Rules{blocks, supersedes}, mapping.Blocks, "a"},
		{mapping.Rules{supersedes, blocks}, mapping.Supersedes, "b"},
	} {
		made := c.rules.Apply(document, "github", references, nil)
		if got := kindsOf(made.Edges)[target]; got != c.want {
			t.Errorf("became %q, want %q", got, c.want)
		}
		if len(made.Why) != 1 || made.Why[0].Rule != c.rule {
			t.Errorf("the reason names %+v, want rule %q", made.Why, c.rule)
		}
	}
}

// A rule for another source, or for another form of reference, is passed over.
func TestARuleOnlyMatchesItsSourceAndForm(t *testing.T) {
	references := mentions(t, "fixes #12", "#12")
	rules := mapping.Rules{
		{ID: "jira", Source: "jira", Form: graph.FormMention, Kind: mapping.Blocks},
		{ID: "parents", Source: "github", Form: graph.FormParent, Kind: mapping.Blocks},
		{ID: "labels", Source: "github", Form: mapping.FormLabel, Label: "*", Ignore: true},
	}
	made := rules.Apply(document, "github", references, nil)
	if got := kindsOf(made.Edges)[targetOf(t, "#12")]; got != mapping.Resolves {
		t.Errorf("became %q, want %q", got, mapping.Resolves)
	}

	// A rule that names no source is for every source.
	any := mapping.Rules{{ID: "any", Form: graph.FormMention, Kind: mapping.Blocks}}
	made = any.Apply(document, "github", references, nil)
	if got := kindsOf(made.Edges)[targetOf(t, "#12")]; got != mapping.Blocks {
		t.Errorf("under a rule for every source it became %q, want %q", got, mapping.Blocks)
	}
}

func TestAParentBecomesChildOf(t *testing.T) {
	parent := graph.Reference{Target: issue("acme/api", 7), Form: graph.FormParent, Context: "sub-issue of"}
	made := mapping.Rules(nil).Apply(document, "github", []graph.Reference{parent}, nil)
	want := []graph.Edge{{From: document, Kind: graph.ChildOf, To: issue("acme/api", 7), Summary: "sub-issue of"}}
	if !slices.Equal(made.Edges, want) {
		t.Errorf("made %v, want %v", made.Edges, want)
	}
}

// A relation that a source names itself, and that no rule covers, is still a
// link: nothing a source says is dropped for want of a rule.
func TestAFormWithNoRuleBecomesAPlainLink(t *testing.T) {
	blocks := graph.Reference{Target: "doc:jira:PAY-7", Form: "jira:blocks"}
	made := mapping.Rules(nil).Apply("doc:jira:PAY-1", "jira", []graph.Reference{blocks}, nil)
	if got := kindsOf(made.Edges)["doc:jira:PAY-7"]; got != graph.LinksTo {
		t.Errorf("became %q, want %q", got, graph.LinksTo)
	}

	rules := mapping.Rules{{ID: "blocks", Source: "jira", Form: "jira:blocks", Kind: mapping.Blocks}}
	made = rules.Apply("doc:jira:PAY-1", "jira", []graph.Reference{blocks}, nil)
	if got := kindsOf(made.Edges)["doc:jira:PAY-7"]; got != mapping.Blocks {
		t.Errorf("under its rule it became %q, want %q", got, mapping.Blocks)
	}
}

// What may stand between a word and its reference is any other reference of
// the document on that line, whatever a source's references look like.
func TestReferencesOfAnotherShapeMayStandBetween(t *testing.T) {
	line := "blocks PAY-7 and PAY-9"
	references := []graph.Reference{
		{Target: "doc:jira:PAY-7", Form: graph.FormMention, Written: "PAY-7", Context: line},
		{Target: "doc:jira:PAY-9", Form: graph.FormMention, Written: "PAY-9", Context: line},
	}
	rules := mapping.Rules{{ID: "blocks", Source: "jira", Form: graph.FormMention, After: []string{"blocks"}, Kind: mapping.Blocks}}
	made := rules.Apply("doc:jira:PAY-1", "jira", references, nil)
	for _, reference := range references {
		if got := kindsOf(made.Edges)[reference.Target]; got != mapping.Blocks {
			t.Errorf("%s became %q, want %q", reference.Written, got, mapping.Blocks)
		}
	}
	if len(made.Why) != 2 || made.Why[1].How != `after "blocks"` {
		t.Errorf("the reasons are %+v", made.Why)
	}
}

// The edge keeps the line the reference was on as its note.
func TestTheEdgeKeepsTheLine(t *testing.T) {
	references := mentions(t, "Fixes #46.", "#46")
	made := mapping.Rules(nil).Apply(document, "github", references, nil)
	want := []graph.Edge{{From: document, Kind: mapping.Resolves, To: issue("acme/api", 46), Summary: "Fixes #46."}}
	if !slices.Equal(made.Edges, want) {
		t.Errorf("made %v, want %v", made.Edges, want)
	}
}

// A document may point at another twice, in two ways. Each becomes an edge
// of its own kind; two that come to the same kind are one edge, with the
// first line as its note.
func TestTwoReferencesToOneDocument(t *testing.T) {
	references := append(mentions(t, "fixes #12", "#12"), mentions(t, "see also #12", "#12")...)
	references = append(references, mentions(t, "and again #12", "#12")...)

	made := mapping.Rules(nil).Apply(document, "github", references, nil)
	want := []graph.Edge{
		{From: document, Kind: mapping.Resolves, To: issue("acme/api", 12), Summary: "fixes #12"},
		{From: document, Kind: graph.LinksTo, To: issue("acme/api", 12), Summary: "see also #12"},
	}
	if !slices.Equal(made.Edges, want) {
		t.Errorf("made %v, want %v", made.Edges, want)
	}
	if len(made.Why) != 3 {
		t.Errorf("gave %d reasons, want one for each of the 3 references", len(made.Why))
	}
}

// Two things make no edge: a reference whose address no source recognized,
// which leads outside the graph, and a document pointing at itself.
func TestReferencesThatMakeNothing(t *testing.T) {
	references := []graph.Reference{
		{URL: "https://example.com/elsewhere", Form: graph.FormMention, Written: "https://example.com/elsewhere", Context: "see https://example.com/elsewhere"},
		{Target: document, Form: graph.FormMention, Written: "#1", Context: "this is #1"},
	}
	made := mapping.Rules(nil).Apply(document, "github", references, nil)
	if len(made.Edges) != 0 {
		t.Errorf("made %v, want no edges", made.Edges)
	}
	if len(made.Why) != 2 || made.Why[0].How == "" || made.Why[1].How == "" {
		t.Errorf("the reasons do not say why nothing was made: %+v", made.Why)
	}
}

// A graph made before references kept their own text has only the line and
// the target. The reference is then looked for in the line by the target's
// number, which finds a "#19" or an address ending in it.
func TestAReferenceKeptWithoutItsText(t *testing.T) {
	cases := []struct {
		line   string
		target graph.ID
		want   graph.EdgeKind
	}{
		{"implements #10 and #19", issue("acme/api", 19), mapping.Resolves},
		{"implements #10 and #19", issue("acme/api", 10), mapping.Resolves},
		{"Fixes https://github.com/jackc/pgx/issues/1287", issue("jackc/pgx", 1287), mapping.Resolves},
		{"addresses acme/web#3", issue("acme/web", 3), mapping.Resolves},
		{"#16", issue("acme/api", 16), graph.ChildOf},
		// Not in the line at all: the line was cut short before it. Only a
		// rule that asks nothing of the line can match.
		{"2. A barging fast path in `Acquire`: try the semaphore before queueing", issue("golang/go", 56910), graph.LinksTo},
	}
	rules := append(slices.Clone(team), mapping.Rule{ID: "alone", Source: "github", Form: graph.FormMention, Alone: true, Kind: graph.ChildOf})
	for _, c := range cases {
		t.Run(c.line, func(t *testing.T) {
			reference := graph.Reference{Target: c.target, Form: graph.FormMention, Context: c.line}
			made := rules.Apply(document, "github", []graph.Reference{reference}, nil)
			if got := kindsOf(made.Edges)[c.target]; got != c.want {
				t.Errorf("became %q, want %q", got, c.want)
			}
		})
	}
}

func TestLabels(t *testing.T) {
	labels := []graph.Label{
		{Name: "bug", Description: "Something is broken"},
		{Name: "Docs"},
		{Name: "documentation", Description: "Improvements to the documentation"},
		{Name: "area/Billing"},
		{Name: "good first issue"},
	}
	rules := mapping.Rules{
		{ID: "no-bug", Source: "github", Form: mapping.FormLabel, Label: "BUG", Ignore: true},
		{ID: "docs", Source: "github", Form: mapping.FormLabel, Label: "docs", Topic: "documentation"},
		{ID: "areas", Source: "github", Form: mapping.FormLabel, Label: "area/*", Topic: "areas"},
	}

	// By default every label is a topic of its own name.
	made := mapping.Rules(nil).Apply(document, "github", nil, labels)
	var topics []graph.ID
	for _, edge := range made.Edges {
		if edge.Kind != graph.InTopic || edge.From != document {
			t.Errorf("a label became %+v, want an in_topic edge from the document", edge)
		}
		topics = append(topics, edge.To)
	}
	want := []graph.ID{"topic:bug", "topic:docs", "topic:documentation", "topic:area/billing", "topic:good-first-issue"}
	if !slices.Equal(topics, want) {
		t.Errorf("topics %v, want %v", topics, want)
	}
	if first := made.Topics[0]; first.ID != "topic:bug" || first.Name != "bug" || first.Summary != "Something is broken" {
		t.Errorf("the first topic is %+v", first)
	}

	// With the team's rules: bug is no topic, docs and documentation are
	// one, and every area is filed under "areas".
	made = rules.Apply(document, "github", nil, labels)
	topics = topics[:0]
	for _, edge := range made.Edges {
		topics = append(topics, edge.To)
	}
	want = []graph.ID{"topic:documentation", "topic:areas", "topic:good-first-issue"}
	if !slices.Equal(topics, want) {
		t.Errorf("under the team's rules, topics %v, want %v", topics, want)
	}
	// The topic the two labels share takes its description from the label
	// that carries its name, whichever comes first.
	var documentation graph.Topic
	for _, topic := range made.Topics {
		if topic.ID == "topic:documentation" {
			documentation = topic
		}
	}
	if documentation.Name != "documentation" || documentation.Summary != "Improvements to the documentation" {
		t.Errorf("the shared topic is %+v", documentation)
	}
	if len(made.Topics) != 3 {
		t.Errorf("made %d topics, want 3: %+v", len(made.Topics), made.Topics)
	}
	if len(made.Why) != len(labels) {
		t.Errorf("gave %d reasons, want one for each of the %d labels", len(made.Why), len(labels))
	}
}

// A label rule may relate the document to its topic by a kind of the team's
// own, as long as that kind joins a document to a topic.
func TestALabelRuleMayNameAKind(t *testing.T) {
	rules := mapping.Rules{{ID: "owners", Source: "github", Form: mapping.FormLabel, Label: "team/*", Kind: "owned_by"}}
	made := rules.Apply(document, "github", nil, []graph.Label{{Name: "team/payments"}})
	want := []graph.Edge{{From: document, Kind: "owned_by", To: "topic:team/payments"}}
	if !slices.Equal(made.Edges, want) {
		t.Errorf("made %v, want %v", made.Edges, want)
	}
}

func TestStarterKinds(t *testing.T) {
	kinds := mapping.StarterKinds()
	seen := make(map[graph.EdgeKind]bool)
	for _, kind := range kinds {
		if err := kind.Validate(); err != nil {
			t.Errorf("%q: %v", kind.Key, err)
		}
		if seen[kind.Key] {
			t.Errorf("%q is in the starter set twice", kind.Key)
		}
		seen[kind.Key] = true
		if kind.ToTopic != (kind.Key == graph.InTopic) {
			t.Errorf("%q: ToTopic is %v", kind.Key, kind.ToTopic)
		}
	}
	for _, key := range []graph.EdgeKind{graph.LinksTo, graph.ChildOf, graph.InTopic, mapping.Resolves, mapping.Duplicates, mapping.Blocks, mapping.Supersedes} {
		if !seen[key] {
			t.Errorf("the starter set lacks %q", key)
		}
	}

	// Every rule that ships names a kind that ships.
	if err := mapping.Defaults("github").Validate(kinds); err != nil {
		t.Errorf("GitHub's defaults: %v", err)
	}
	if err := mapping.Defaults("some-other-source").Validate(kinds); err != nil {
		t.Errorf("the defaults of a source with none of its own: %v", err)
	}
}

func TestKindValidate(t *testing.T) {
	valid := mapping.Kind{Key: "owned_by", Phrase: "is owned by", Description: "The team that looks after it.", ToTopic: true}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := map[string]func(*mapping.Kind){
		"no key":             func(k *mapping.Kind) { k.Key = "" },
		"a key with a space": func(k *mapping.Kind) { k.Key = "owned by" },
		"no phrase":          func(k *mapping.Kind) { k.Phrase = "  " },
	}
	for name, spoil := range invalid {
		kind := valid
		spoil(&kind)
		if err := kind.Validate(); err == nil {
			t.Errorf("a kind with %s was accepted", name)
		}
	}
}

func TestRuleValidate(t *testing.T) {
	kinds := mapping.StarterKinds()
	valid := mapping.Rules{
		{ID: "a", Source: "github", Form: graph.FormMention, After: []string{"implements"}, Kind: mapping.Resolves},
		{ID: "b", Source: "github", Form: graph.FormMention, Alone: true, Kind: graph.ChildOf},
		{ID: "c", Source: "github", Form: graph.FormMention, Ignore: true},
		{ID: "d", Source: "github", Form: mapping.FormLabel, Label: "bug", Ignore: true},
		{ID: "e", Source: "github", Form: mapping.FormLabel, Label: "docs", Topic: "documentation"},
		{ID: "f", Form: mapping.FormLabel, Label: "*"},
		{ID: "g", Source: "jira", Form: "jira:blocks", Kind: mapping.Blocks},
	}
	if err := valid.Validate(kinds); err != nil {
		t.Fatalf("valid rules were refused: %v", err)
	}

	invalid := map[string]mapping.Rule{
		"no ID":                               {Source: "github", Form: graph.FormMention, Kind: graph.LinksTo},
		"no form":                             {ID: "x", Source: "github", Kind: graph.LinksTo},
		"a reference rule that makes nothing": {ID: "x", Form: graph.FormMention},
		"both a kind and ignore":              {ID: "x", Form: graph.FormMention, Kind: graph.LinksTo, Ignore: true},
		"after and alone together":            {ID: "x", Form: graph.FormMention, After: []string{"fixes"}, Alone: true, Kind: graph.LinksTo},
		"an empty word to come after":         {ID: "x", Form: graph.FormMention, After: []string{"fixes", " "}, Kind: graph.LinksTo},
		"a reference rule with a label":       {ID: "x", Form: graph.FormMention, Label: "bug", Kind: graph.LinksTo},
		"a reference rule with a topic":       {ID: "x", Form: graph.FormMention, Topic: "bugs", Kind: graph.LinksTo},
		"a reference rule to a topic kind":    {ID: "x", Form: graph.FormMention, Kind: graph.InTopic},
		"a label rule with words":             {ID: "x", Form: mapping.FormLabel, Label: "bug", After: []string{"fixes"}},
		"a label rule that is alone":          {ID: "x", Form: mapping.FormLabel, Label: "bug", Alone: true},
		"a label rule to a document kind":     {ID: "x", Form: mapping.FormLabel, Label: "bug", Kind: graph.LinksTo},
		"a label rule ignoring with a topic":  {ID: "x", Form: mapping.FormLabel, Label: "bug", Topic: "bugs", Ignore: true},
		"a kind the team has not defined":     {ID: "x", Form: graph.FormMention, Kind: "backports"},
	}
	for name, rule := range invalid {
		if err := (mapping.Rules{rule}).Validate(kinds); err == nil {
			t.Errorf("%s was accepted: %+v", name, rule)
		}
	}

	twice := mapping.Rules{valid[0], valid[0]}
	if err := twice.Validate(kinds); err == nil {
		t.Error("two rules with one ID were accepted")
	}
}

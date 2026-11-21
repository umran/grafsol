package mapping

import (
	"fmt"
	"strings"

	"github.com/umran/grafsol/graph"
)

// FormLabel is the Form of a rule that is about labels, not references.
const FormLabel = "label"

// Rule says what one sort of reference, or one sort of label, becomes.
//
// A rule is a form to fill in, not a pattern language: a team should be able
// to write one without knowing what a regular expression is.
type Rule struct {
	// ID names the rule in reasons and in a team's list of rules.
	ID string `json:"id"`

	// Source is the source the rule is for, such as "github". Empty means
	// every source.
	Source string `json:"source,omitempty"`

	// Form is what the rule is about: a form of reference
	// (graph.FormMention, graph.FormParent, or a relation that a source
	// names itself), or FormLabel for labels.
	Form string `json:"form"`

	// After and Alone narrow a rule about references by where the
	// reference stands on its line. At most one of them is set, and with
	// neither the rule matches the reference wherever it stands.
	//
	// After lists words, any one of which must come before the reference
	// with nothing between them but other references and joining words:
	// "implements" matches every reference in "implements #9, #16 and #17".
	// A word may be several words, as in "duplicate of".
	//
	// Alone asks that the line hold the reference and nothing else.
	After []string `json:"after,omitempty"`
	Alone bool     `json:"alone,omitempty"`

	// Label narrows a rule about labels to those of one name. Letters match
	// whatever their case, and * stands for any run of characters, so
	// "area/*" matches "area/Billing". Empty matches every label.
	Label string `json:"label,omitempty"`

	// What the rule makes of what it matches. For a reference: an edge of
	// Kind. For a label: an edge of Kind (graph.InTopic when left empty) to
	// the topic named Topic, or to the topic of the label's own name when
	// Topic is empty. With Ignore, nothing at all.
	Kind   graph.EdgeKind `json:"kind,omitempty"`
	Topic  string         `json:"topic,omitempty"`
	Ignore bool           `json:"ignore,omitempty"`
}

// Rules is a list of rules in the order they are tried: the first that
// matches settles what a reference or a label becomes.
type Rules []Rule

// Defaults returns the rules a source ships with. They come after a team's
// own rules, so they decide whatever the team's rules leave alone, and a
// graph made with no other rules is made by these.
func Defaults(source string) Rules {
	if source == "github" {
		return Rules{
			// GitHub closes an issue when a pull request says one of these
			// words of it, so the words mean the same thing everywhere.
			{ID: "github-closing-words", Source: source, Form: graph.FormMention, Kind: Resolves,
				After: []string{"close", "closes", "closed", "fix", "fixes", "fixed", "resolve", "resolves", "resolved"}},
			{ID: "github-duplicate-of", Source: source, Form: graph.FormMention, After: []string{"duplicate of"}, Kind: Duplicates},
			{ID: "github-sub-issue", Source: source, Form: graph.FormParent, Kind: graph.ChildOf},
			{ID: "github-mention", Source: source, Form: graph.FormMention, Kind: graph.LinksTo},
			{ID: "github-label", Source: source, Form: FormLabel, Kind: graph.InTopic},
		}
	}
	return Rules{
		{ID: "any-parent", Form: graph.FormParent, Kind: graph.ChildOf},
		{ID: "any-mention", Form: graph.FormMention, Kind: graph.LinksTo},
		{ID: "any-label", Form: FormLabel, Kind: graph.InTopic},
	}
}

// Validate reports what is wrong with the rules, or nil. kinds is every kind
// that has been defined: a rule may only make edges of those.
func (r Rules) Validate(kinds []Kind) error {
	if err := r.check(kinds); err != nil {
		return fmt.Errorf("mapping: %w", err)
	}
	return nil
}

// check is Validate without the package's name in front of what it reports.
func (r Rules) check(kinds []Kind) error {
	defined := make(map[graph.EdgeKind]Kind, len(kinds))
	for _, kind := range kinds {
		defined[kind.Key] = kind
	}
	ids := make(map[string]bool, len(r))
	for i, rule := range r {
		if err := rule.validate(defined); err != nil {
			return fmt.Errorf("rule %d (%q): %w", i+1, rule.ID, err)
		}
		if ids[rule.ID] {
			return fmt.Errorf("two rules are called %q", rule.ID)
		}
		ids[rule.ID] = true
	}
	return nil
}

func (r Rule) validate(defined map[graph.EdgeKind]Kind) error {
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("it has no ID")
	}
	if r.Form == "" {
		return fmt.Errorf("it does not say what it is about: a form of reference, or labels")
	}
	if r.Form == FormLabel {
		return r.validateForLabels(defined)
	}
	return r.validateForReferences(defined)
}

func (r Rule) validateForReferences(defined map[graph.EdgeKind]Kind) error {
	switch {
	case r.Label != "" || r.Topic != "":
		return fmt.Errorf("a rule about references cannot name a label or a topic")
	case len(r.After) > 0 && r.Alone:
		return fmt.Errorf("a reference cannot both come after a word and stand alone on its line")
	case r.Ignore && r.Kind != "":
		return fmt.Errorf("it both ignores the reference and makes a %q edge of it", r.Kind)
	case !r.Ignore && r.Kind == "":
		return fmt.Errorf("it makes nothing of the reference and does not say to ignore it")
	}
	for _, word := range r.After {
		if strings.TrimSpace(word) == "" {
			return fmt.Errorf("one of the words to come after is empty")
		}
	}
	if r.Kind == "" {
		return nil
	}
	kind, known := defined[r.Kind]
	switch {
	case !known:
		return fmt.Errorf("%q %w", r.Kind, errNoKind)
	case kind.ToTopic:
		return fmt.Errorf("%q joins a document to a topic, and a reference is to a document", r.Kind)
	}
	return nil
}

func (r Rule) validateForLabels(defined map[graph.EdgeKind]Kind) error {
	switch {
	case len(r.After) > 0 || r.Alone:
		return fmt.Errorf("a rule about labels cannot ask where a reference stands on its line")
	case r.Ignore && (r.Kind != "" || r.Topic != ""):
		return fmt.Errorf("it both ignores the label and files the document under a topic")
	}
	if r.Kind == "" {
		return nil
	}
	kind, known := defined[r.Kind]
	switch {
	case !known:
		return fmt.Errorf("%q %w", r.Kind, errNoKind)
	case !kind.ToTopic:
		return fmt.Errorf("%q joins a document to a document, and a label is a topic", r.Kind)
	}
	return nil
}

// about reports whether the rule is one to try for something of the given
// form that came from the given source.
func (r Rule) about(source, form string) bool {
	return (r.Source == "" || r.Source == source) && r.Form == form
}

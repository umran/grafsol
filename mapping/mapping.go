// Package mapping decides what the things a source says about a document
// become in the graph: which kind of edge a reference is, and which topic a
// label files the document under.
//
// A connector reports references and labels as it found them
// (graph.Reference, graph.Label), without deciding what they mean. Rules,
// which a team writes, say what each becomes. The first rule that matches
// settles it: the team's rules in the order the team put them, and then the
// rules a source ships with (Defaults), which are also what makes a graph
// when a team has written none.
//
// The kinds of edge are a team's to define as well. A Kind gives a key the
// phrase that a judge reads, and StarterKinds is the set every graph begins
// with.
//
// The package holds rules and kinds and applies them. It reads nothing and
// writes nothing: apply.go is the file to read to see how a rule is matched.
package mapping

import (
	"errors"
	"fmt"
	"strings"

	"github.com/umran/grafsol/graph"
)

// Kind describes one kind of edge.
type Kind struct {
	// Key is what edges of the kind carry: "resolves".
	Key graph.EdgeKind `json:"key"`

	// Phrase is the kind as a verb, read from the edge's From to its To. A
	// judge weighing a document is shown "it resolves X" for an edge that
	// leaves the document and "X resolves it" for one that arrives.
	Phrase string `json:"phrase"`

	// Description says, for the people who choose between kinds, when this
	// one applies.
	Description string `json:"description,omitempty"`

	// ToTopic reports that the kind joins a document to a topic. Every
	// other kind joins a document to a document.
	ToTopic bool `json:"to_topic,omitempty"`
}

// The kinds in the starter set that package graph does not already name.
const (
	// Resolves (document -> document): From settles what To asks for. A
	// pull request that implements, fixes or closes an issue.
	Resolves graph.EdgeKind = "resolves"

	// Duplicates (document -> document): From reports or does what To
	// already does.
	Duplicates graph.EdgeKind = "duplicates"

	// Blocks (document -> document): To cannot go ahead until From is done.
	Blocks graph.EdgeKind = "blocks"

	// Supersedes (document -> document): From takes the place of To.
	Supersedes graph.EdgeKind = "supersedes"
)

// StarterKinds returns the kinds every graph begins with. A team may reword
// them and add its own.
func StarterKinds() []Kind {
	return []Kind{
		{Key: graph.LinksTo, Phrase: "links to", Description: "The first mentions the second, and nothing says more than that."},
		{Key: graph.ChildOf, Phrase: "is a part of", Description: "The first sits under the second: a sub-issue of an issue, a sub-page of a page."},
		{Key: Resolves, Phrase: "resolves", Description: "The first settles what the second asks for: a pull request that implements, fixes or closes an issue."},
		{Key: Duplicates, Phrase: "duplicates", Description: "The first reports, or does, what the second already does."},
		{Key: Blocks, Phrase: "blocks", Description: "The second cannot go ahead until the first is done."},
		{Key: Supersedes, Phrase: "supersedes", Description: "The first takes the place of the second: a rework, a newer version."},
		{Key: graph.InTopic, Phrase: "belongs to", Description: "The document belongs to the topic: it carries its label, or was filed under it.", ToTopic: true},
	}
}

// Validate reports what is wrong with the kind, or nil.
func (k Kind) Validate() error {
	if err := k.Key.Validate(); err != nil {
		return fmt.Errorf("mapping: kind: %w", err)
	}
	if strings.TrimSpace(k.Phrase) == "" {
		return fmt.Errorf("mapping: the kind %q has no phrase for a judge to read", k.Key)
	}
	return nil
}

// errNoKind is what looking up a kind that is not defined comes to.
var errNoKind = errors.New("is not a kind that has been defined")

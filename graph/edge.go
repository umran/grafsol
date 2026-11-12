package graph

import (
	"cmp"
	"errors"
	"fmt"
)

// EdgeKind names the relationship an edge expresses. It reads as a verb
// phrase from the edge's From vertex to its To vertex. The set is open: a
// new source may introduce its own kinds alongside the well-known ones below.
type EdgeKind string

const (
	// LinksTo (document -> document): From references To, through a
	// hyperlink or a mention such as "#42".
	LinksTo EdgeKind = "links_to"

	// ChildOf (document -> document): From sits under To in a hierarchy: a
	// sub-page of a page, a sub-task of a ticket, an issue in an epic.
	ChildOf EdgeKind = "child_of"

	// InTopic (document -> topic): From belongs to the topic.
	InTopic EdgeKind = "in_topic"

	// TaggedWith (document -> topic): From carries the topic as an explicit
	// tag or label.
	TaggedWith EdgeKind = "tagged_with"
)

// Validate reports what is wrong with the kind, or nil. A kind is a
// non-empty run of lower-case letters, digits and underscores.
func (k EdgeKind) Validate() error {
	if k == "" {
		return errors.New("graph: empty edge kind")
	}
	for _, r := range k {
		lower := r >= 'a' && r <= 'z'
		digit := r >= '0' && r <= '9'
		if !lower && !digit && r != '_' {
			return fmt.Errorf("graph: edge kind %q may only contain a-z, 0-9 and _", string(k))
		}
	}
	return nil
}

// Edge is a directed, typed connection between two vertices.
//
// An edge is identified by its (From, Kind, To) triple: there is at most one
// edge of a kind between two vertices in a given direction. Summary is an
// attribute of that edge, not part of its identity.
//
// An edge may point at a vertex that is not stored: a ticket can reference a
// pull request that has not been ingested yet. Such edges are kept and start
// working as soon as the missing vertex arrives.
type Edge struct {
	From ID
	Kind EdgeKind
	To   ID

	// Summary says, in a sentence, why the edge exists: the text around a
	// link, "sub-issue of", a reviewer's stated reason. It may be empty.
	Summary string
}

// Validate reports what is wrong with the edge, or nil if it can be stored.
func (e Edge) Validate() error {
	if err := e.From.Validate(); err != nil {
		return fmt.Errorf("graph: edge From: %w", err)
	}
	if err := e.Kind.Validate(); err != nil {
		return err
	}
	if err := e.To.Validate(); err != nil {
		return fmt.Errorf("graph: edge To: %w", err)
	}
	return nil
}

// Triple returns the edge without its Summary: the (From, Kind, To) that
// identifies it. Two values describe the same edge exactly when their
// triples are equal, which makes a triple usable as a map key.
func (e Edge) Triple() Edge {
	return Edge{From: e.From, Kind: e.Kind, To: e.To}
}

// Compare orders edges by From, then Kind, then To, ignoring Summary. It
// returns a negative number, zero or a positive number, as cmp.Compare does.
func (e Edge) Compare(other Edge) int {
	return cmp.Or(
		cmp.Compare(e.From, other.From),
		cmp.Compare(e.Kind, other.Kind),
		cmp.Compare(e.To, other.To),
	)
}

// Direction says which way an edge is being looked at from a vertex.
type Direction int

const (
	// Out selects the edges that start at the vertex.
	Out Direction = iota + 1

	// In selects the edges that end at the vertex. Reading an edge kind
	// backwards gives its inverse: the In edges of kind ChildOf on a
	// document are that document's children.
	In
)

// String returns "out" or "in".
func (d Direction) String() string {
	switch d {
	case Out:
		return "out"
	case In:
		return "in"
	}
	return fmt.Sprintf("Direction(%d)", int(d))
}

// Validate reports an error unless the direction is Out or In.
func (d Direction) Validate() error {
	if d != Out && d != In {
		return fmt.Errorf("graph: %s is neither Out nor In", d)
	}
	return nil
}

// Near returns the end of the edge that a query in direction d started
// from: From for Out, To for In.
func (e Edge) Near(d Direction) ID {
	if d == In {
		return e.To
	}
	return e.From
}

// Far returns the end of the edge that a query in direction d leads to: To
// for Out, From for In.
func (e Edge) Far(d Direction) ID {
	if d == In {
		return e.From
	}
	return e.To
}

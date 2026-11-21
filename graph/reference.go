package graph

import (
	"errors"
	"fmt"
)

// Reference is one thing a document points at, told the way its source told
// it: before anything has decided what the pointing means. A connector
// reports references; rules (package mapping) decide which kind of edge each
// one becomes.
type Reference struct {
	// Target is the document pointed at, when it is known. A connector
	// names it wherever it can.
	//
	// URL is what a connector gives instead for an address that may belong
	// to another source, which only the sync engine can tell. The sync
	// engine asks every source whose the address is and, if one knows,
	// fills in Target beside it. A reference whose address no source knows
	// keeps an empty Target: it leads outside the graph.
	Target ID
	URL    string

	// Form says how the source expressed the reference: FormMention for one
	// found in the document's text, FormParent for a parent the source
	// itself records, or a relation that the source names, such as
	// "jira:blocks".
	Form string

	// Written is the reference as it stood in the text: "#19",
	// "acme/web#12", a URL. It is empty when the reference was not written
	// in text at all, as a parent is not.
	Written string

	// Context is the text around the reference: for a mention, the line it
	// was on. A rule that asks what came before the reference reads it here.
	Context string
}

// The forms of reference that every source shares. A source may add its own.
const (
	// FormMention is a reference found in a document's text.
	FormMention = "mention"

	// FormParent is the document's parent, as the source records it: the
	// issue a sub-issue belongs to, the page a sub-page sits under.
	FormParent = "parent"
)

// Validate reports what is wrong with the reference, or nil.
func (r Reference) Validate() error {
	switch {
	case r.Target == "" && r.URL == "":
		return errors.New("graph: a reference names neither a target nor a URL")
	case r.Form == "":
		return errors.New("graph: a reference does not say what form it took")
	}
	if r.Target != "" {
		if err := r.Target.Validate(); err != nil {
			return fmt.Errorf("graph: reference target: %w", err)
		}
	}
	return nil
}

// Label is a label, tag or category that a source put on a document. Rules
// decide what it becomes: usually a topic the document belongs to.
type Label struct {
	// Name is the label as people see it: "Good First Issue".
	Name string

	// Description says what the label is for, if the source knows.
	Description string
}

// Validate reports what is wrong with the label, or nil.
func (l Label) Validate() error {
	if l.Name == "" {
		return errors.New("graph: a label has no name")
	}
	return nil
}

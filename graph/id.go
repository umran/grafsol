// Package graph defines the vocabulary of the knowledge graph: its vertices
// (documents and topics), the typed edges between them, and the identifiers
// that name them.
//
// The package holds data and nothing else. It performs no I/O and imports
// nothing outside the standard library, so every other package in the module
// can depend on it without pulling anything else along.
package graph

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ID identifies a vertex. It is a plain string of the form
//
//	<kind>:<key>
//
// such as "doc:github:acme/api#42" or "topic:billing". Because the kind is
// part of the ID, code that holds nothing but an ID (the far end of an edge,
// say) already knows what sort of vertex it names, with no lookup.
type ID string

// VertexKind tells the sorts of vertex apart. It is the prefix of every ID.
type VertexKind string

const (
	// KindDocument is a unit of written work in an external system: an
	// issue, a pull request, a ticket, a wiki page.
	KindDocument VertexKind = "doc"

	// KindTopic is a subject that documents can belong to or be tagged with.
	KindTopic VertexKind = "topic"
)

// DocumentID builds a document's ID from the name of the source system that
// owns the document and that system's own key for it:
//
//	DocumentID("github", "acme/api#42") == "doc:github:acme/api#42"
func DocumentID(source, key string) ID {
	return ID(string(KindDocument) + ":" + source + ":" + key)
}

// TopicID builds a topic's ID from its name. The name is normalized first
// (trimmed, lower-cased, runs of whitespace turned into single hyphens) so
// that "Bug", "bug" and " bug " all name the same topic:
//
//	TopicID("Good First Issue") == "topic:good-first-issue"
func TopicID(name string) ID {
	normalized := strings.Join(strings.Fields(strings.ToLower(name)), "-")
	return ID(string(KindTopic) + ":" + normalized)
}

// Kind returns the kind of vertex the ID names, or "" if the ID does not
// start with a known kind.
func (id ID) Kind() VertexKind {
	prefix, _, found := strings.Cut(string(id), ":")
	if !found {
		return ""
	}
	switch kind := VertexKind(prefix); kind {
	case KindDocument, KindTopic:
		return kind
	}
	return ""
}

// Key returns the part of the ID after its kind: "github:acme/api#42" for
// "doc:github:acme/api#42". It returns "" if the ID has no kind prefix.
func (id ID) Key() string {
	_, key, _ := strings.Cut(string(id), ":")
	return key
}

// Source returns the name of the source system inside a document ID:
// "github" for "doc:github:acme/api#42". It returns "" for any other ID.
func (id ID) Source() string {
	if id.Kind() != KindDocument {
		return ""
	}
	source, _, found := strings.Cut(id.Key(), ":")
	if !found {
		return ""
	}
	return source
}

// Validate reports what is wrong with the ID, or nil if it is well formed:
// a known kind, a non-empty key, valid UTF-8 and no control characters. A
// document's key must itself have the form "<source>:<key>".
func (id ID) Validate() error {
	kind := id.Kind()
	if kind == "" {
		return fmt.Errorf("graph: ID %q does not start with %q or %q", string(id), KindDocument+":", KindTopic+":")
	}
	key := id.Key()
	if key == "" {
		return fmt.Errorf("graph: ID %q has an empty key", string(id))
	}
	if !utf8.ValidString(string(id)) {
		return fmt.Errorf("graph: ID %q is not valid UTF-8", string(id))
	}
	if strings.ContainsFunc(string(id), unicode.IsControl) {
		return fmt.Errorf("graph: ID %q contains a control character", string(id))
	}
	if kind == KindDocument {
		source, rest, found := strings.Cut(key, ":")
		if !found || source == "" || rest == "" {
			return fmt.Errorf("graph: document ID %q is not of the form doc:<source>:<key>", string(id))
		}
	}
	return nil
}

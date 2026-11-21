package mapping

import (
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/umran/grafsol/graph"
)

// Made is what the rules made of one document's references and labels.
type Made struct {
	// Edges are the edges that start at the document, in the order the
	// references and then the labels were given. Two references that come
	// to the same kind of edge to the same document are one edge.
	Edges []graph.Edge

	// Topics describes the topics those edges point at.
	Topics []graph.Topic

	// Why says, for each reference and then each label, which rule settled
	// it and what came of it. It is what lets a person see why the graph
	// holds an edge, or does not.
	Why []Reason
}

// Reason says what became of one reference or one label, and why.
type Reason struct {
	// Reference or Label is what the reason is about; the other is nil.
	Reference *graph.Reference
	Label     *graph.Label

	// Rule is the ID of the rule that settled it. It is empty when no rule
	// was asked, which How then explains.
	Rule string

	// How says in a few words why the rule matched, or why nothing was made.
	How string

	// Edge is the edge that was made, or the zero Edge when none was.
	Edge graph.Edge
}

// Apply says what one document's references and labels become: under these
// rules first, in order, and then under the rules its source ships with.
//
// A reference whose Target is empty makes nothing: no source recognized its
// address, so it leads outside the graph. Neither does a document's
// reference to itself.
func (r Rules) Apply(document graph.ID, source string, references []graph.Reference, labels []graph.Label) Made {
	rules := slices.Concat(r, Defaults(source))
	var made Made

	for i := range references {
		reference := references[i]
		reason := Reason{Reference: &reference}
		switch {
		case reference.Target == "":
			reason.How = "no source knows its address, so it leads outside the graph"
		case reference.Target == document:
			reason.How = "it points at the document itself"
		default:
			reason.Rule, reason.How, reason.Edge = rules.settle(document, source, reference, references)
			made.keep(reason.Edge)
		}
		made.Why = append(made.Why, reason)
	}

	for i := range labels {
		label := labels[i]
		reason := Reason{Label: &label}
		var topic graph.Topic
		reason.Rule, reason.How, reason.Edge, topic = rules.file(document, source, label)
		if reason.Edge != (graph.Edge{}) {
			made.keep(reason.Edge)
			made.describe(topic)
		}
		made.Why = append(made.Why, reason)
	}
	return made
}

// settle finds the first rule that matches a reference and returns what it
// makes of it. others is every reference of the document, which is how the
// references that share a line are known.
func (r Rules) settle(document graph.ID, source string, reference graph.Reference, others []graph.Reference) (rule, how string, edge graph.Edge) {
	var at *span // where the reference stands on its line, looked up when first needed
	located := false
	for _, candidate := range r {
		if !candidate.about(source, reference.Form) {
			continue
		}
		how := "any " + reference.Form
		if candidate.Alone || len(candidate.After) > 0 {
			if !located {
				at, located = locate(reference), true
			}
			if at == nil {
				continue // the reference is not in what was kept of its line
			}
			if candidate.Alone {
				if !alone(reference.Context, *at) {
					continue
				}
				how = "alone on its line"
			} else {
				word, ok := after(reference.Context, *at, candidate.After, others)
				if !ok {
					continue
				}
				how = "after " + strconv.Quote(word)
			}
		}
		if candidate.Ignore {
			return candidate.ID, how + ", which this rule ignores", graph.Edge{}
		}
		return candidate.ID, how, graph.Edge{From: document, Kind: candidate.Kind, To: reference.Target, Summary: reference.Context}
	}
	// Nothing a source says is dropped for want of a rule.
	return "", "no rule covers this form of reference", graph.Edge{From: document, Kind: graph.LinksTo, To: reference.Target, Summary: reference.Context}
}

// file finds the first rule that matches a label and returns what it makes
// of it: the edge to a topic, and that topic.
func (r Rules) file(document graph.ID, source string, label graph.Label) (rule, how string, edge graph.Edge, topic graph.Topic) {
	for _, candidate := range r {
		if !candidate.about(source, FormLabel) || !named(candidate.Label, label.Name) {
			continue
		}
		how := "any label"
		if candidate.Label != "" && candidate.Label != "*" {
			how = "a label named " + strconv.Quote(candidate.Label)
		}
		if candidate.Ignore {
			return candidate.ID, how + ", which this rule ignores", graph.Edge{}, graph.Topic{}
		}
		edge, topic = filed(document, label, candidate.Kind, candidate.Topic)
		return candidate.ID, how, edge, topic
	}
	edge, topic = filed(document, label, graph.InTopic, "")
	return "", "no rule covers labels", edge, topic
}

// filed returns the edge that files a document under a label's topic, and
// the topic. under names the topic; empty means the label's own name.
func filed(document graph.ID, label graph.Label, kind graph.EdgeKind, under string) (graph.Edge, graph.Topic) {
	if kind == "" {
		kind = graph.InTopic
	}
	topic := graph.Topic{ID: graph.TopicID(label.Name), Name: label.Name, Summary: label.Description}
	if under != "" {
		topic = graph.Topic{ID: graph.TopicID(under), Name: under}
		if topic.ID == graph.TopicID(label.Name) {
			// The label carries the topic's own name, so its description
			// is the topic's.
			topic.Summary = label.Description
		}
	}
	return graph.Edge{From: document, Kind: kind, To: topic.ID}, topic
}

// keep adds an edge, unless the same edge is already there. Of two, the
// first that has a note keeps its place.
func (m *Made) keep(edge graph.Edge) {
	if edge == (graph.Edge{}) {
		return
	}
	for i, kept := range m.Edges {
		if kept.Triple() == edge.Triple() {
			if kept.Summary == "" {
				m.Edges[i].Summary = edge.Summary
			}
			return
		}
	}
	m.Edges = append(m.Edges, edge)
}

// describe adds a topic, unless it is already there. A topic that came
// without a description takes one from a later mention of it.
func (m *Made) describe(topic graph.Topic) {
	for i, described := range m.Topics {
		if described.ID == topic.ID {
			if described.Summary == "" {
				m.Topics[i].Summary = topic.Summary
			}
			return
		}
	}
	m.Topics = append(m.Topics, topic)
}

// named reports whether a label's name matches a rule's: whatever the case
// of the letters, with * in the rule standing for any run of characters. An
// empty rule matches every name.
func named(rule, name string) bool {
	if rule == "" || rule == "*" {
		return true
	}
	parts := strings.Split(rule, "*")
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	pattern, err := regexp.Compile("(?is)^" + strings.Join(parts, ".*") + "$")
	return err == nil && pattern.MatchString(strings.TrimSpace(name))
}

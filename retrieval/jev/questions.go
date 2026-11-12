package jev

import (
	"fmt"
	"strconv"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/typesafe"
)

// This file is the wording: what Jev is shown and what it is asked.
//
// Jev reads literally and answers the question as written, so each question
// is one plain sentence, with what counts as each answer spelt out beside
// it. Names in backticks refer to fields: of the request's state, or of the
// question's own instructions.

// stateOf returns what every question in a request can see: the user's
// question, and the setting it was asked in if the caller gave one.
func stateOf(q retrieval.Question) map[string]any {
	state := map[string]any{"user_question": q.Text}
	if q.Context != "" {
		state["context"] = q.Context
	}
	return state
}

const sufficientName = "sufficient"

// sufficiencyQuestion asks whether the evidence is enough. It is the
// search's stop condition, and follows the wording TypeSafe publishes for
// checking that a set of passages can answer a question: every step from the
// question to its answer must be stated, not inferred.
func sufficiencyQuestion(evidence []retrieval.Evidence) typesafe.Question {
	documents := make([]map[string]string, len(evidence))
	for i, piece := range evidence {
		documents[i] = describe(piece.Document)
	}
	return typesafe.Noul(
		map[string]any{
			"question": "Does `evidence` state every fact needed to answer `user_question`?",
			"evidence": documents,
		},
		"Each step from `user_question` to its answer is stated in `evidence`.",
		"At least one step from `user_question` to its answer is not stated in `evidence`.",
	)
}

func leadName(i int) string {
	return fmt.Sprintf("lead_%04d", i)
}

// leadLevels are the rungs a lead is rated on, lowest first. The rating the
// search uses is the expected rung divided by the top one: about 0 for the
// first, 0.5 for the second and 1 for the third. The halfway marks between
// rungs, 0.25 and 0.75, are retrieval's defaults for following a lead and
// for counting a document as evidence.
var leadLevels = []string{
	"`candidate` has nothing to do with `user_question`.",
	"`candidate` is about the subject of `user_question`, but states nothing that answers it.",
	"`candidate` states a fact that answers `user_question`, in whole or in part.",
}

// leadQuestion asks how much the vertex at the end of a lead helps. The
// vertex travels inside the question, with a note of how it was found, so
// that it is judged on its own.
func leadQuestion(lead retrieval.Lead) typesafe.Question {
	instructions := map[string]any{
		"question":  "How much does `candidate` help to answer `user_question`?",
		"candidate": describe(lead.Target),
	}
	if !lead.Seed() {
		found := map[string]string{"how": relation(lead)}
		if lead.Edge.Summary != "" {
			found["note"] = lead.Edge.Summary
		}
		instructions["found"] = found
	}
	return typesafe.Score(instructions, leadLevels...)
}

// describe returns what Jev is shown of a vertex: what the graph keeps of
// it, less everything that is of no use in judging it. What the graph does
// not have is left out, not sent empty.
//
// A document's type and URL say what it is and where it is from, which its
// title and summary often do not: an issue rarely names the project it was
// filed against, and its URL does. On a graph of two repositories, an issue
// that answered a question about one of them, without naming it, was rated
// 0.65 from its title and summary alone and 0.93 with these two fields.
func describe(vertex graph.Vertex) map[string]string {
	described := make(map[string]string, 5)
	var summary string
	switch v := vertex.(type) {
	case *graph.Document:
		described["kind"], described["title"], summary = "document", v.Title, v.Summary
		if v.Type != "" {
			described["type"] = string(v.Type)
		}
		if v.URL != "" {
			described["url"] = v.URL
		}
	case *graph.Topic:
		described["kind"], described["name"], summary = "topic", v.Name, v.Summary
	}
	if summary != "" {
		described["summary"] = summary
	}
	return described
}

// relation says, in words, how the vertex at the end of a lead is joined to
// the vertex it was found at. "it" is the candidate.
func relation(lead retrieval.Lead) string {
	other := nameOf(lead.From)
	arrives := lead.Edge.To == lead.Target.VertexID() // the edge points at the candidate
	switch lead.Edge.Kind {
	case graph.LinksTo:
		if arrives {
			return other + " links to it"
		}
		return "it links to " + other
	case graph.ChildOf:
		if arrives {
			return other + " is a part of it"
		}
		return "it is a part of " + other
	case graph.InTopic:
		if arrives {
			return other + " belongs to this topic"
		}
		return "it belongs to " + other
	case graph.TaggedWith:
		if arrives {
			return other + " is tagged with this topic"
		}
		return "it is tagged with " + other
	}
	if arrives {
		return fmt.Sprintf("%s has a %q edge to it", other, lead.Edge.Kind)
	}
	return fmt.Sprintf("it has a %q edge to %s", lead.Edge.Kind, other)
}

// nameOf returns what to call a vertex in a sentence.
func nameOf(vertex graph.Vertex) string {
	switch v := vertex.(type) {
	case *graph.Document:
		return "the document " + strconv.Quote(v.Title)
	case *graph.Topic:
		return "the topic " + strconv.Quote(v.Name)
	}
	return "another vertex"
}

package retrieval

import (
	"context"
	"errors"
)

// Judge is a search's only source of judgment. The rest of a search is
// bookkeeping: which leads exist, which were followed, what is left.
//
// Package jev has the Judge built on TypeSafe's Jev model. A test, or a
// caller with a model of its own, can supply any other.
type Judge interface {
	// Assess is called at most once per step of a search. It is given the
	// question, the evidence gathered so far, and leads that have not been
	// judged before, and returns how the search stands.
	//
	// leads may be empty, when the evidence has grown and there is nothing
	// new to rate; evidence is empty until the first document is admitted.
	Assess(ctx context.Context, q Question, evidence []Evidence, leads []Lead) (Assessment, error)
}

// Assessment is the judge's view of a search at one step.
type Assessment struct {
	// Sufficient is how likely the evidence is to state every fact needed
	// to answer the question, from 0 to 1. It is 0 while there is no
	// evidence.
	Sufficient float64

	// Potential rates each lead, in the order the leads were given, on one
	// scale from 0 to 1:
	//
	//   - near 0: the vertex has nothing to do with the question;
	//   - near 0.5: it is on the question's subject, without stating
	//     anything that answers it;
	//   - near 1: it states part of the answer.
	//
	// The search follows a lead rated at or above Config.MinPotential, and
	// counts a followed document as evidence at or above
	// Config.MinEvidence.
	Potential []float64

	// Tokens is what the assessment cost, in whatever unit the judge
	// counts; zero if it does not say.
	Tokens int
}

// ErrTooLarge is what a Judge returns, wrapped or not, when the evidence has
// grown beyond what it can read at once. The search stops with what it has.
var ErrTooLarge = errors.New("retrieval: the evidence is more than the judge can read")

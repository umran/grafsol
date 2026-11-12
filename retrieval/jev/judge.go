// Package jev is the retrieval.Judge built on Jev, TypeSafe's System One
// model.
//
// Jev does not write text: it is given some content and questions about it,
// and answers each question with a number. One assessment is therefore one
// request that carries many questions:
//
//   - one yes/no question (a Noul) asking whether the evidence states every
//     fact needed to answer the user's question;
//   - one graded question (a Score) per lead, asking how much the vertex at
//     the end of the lead helps to answer it: not at all, by being on the
//     subject, or by stating part of the answer.
//
// The user's question travels in the request's shared state. The evidence
// travels inside the sufficiency question and each lead inside its own
// question, and Jev answers every question independently of the others. So a
// lead is judged against the user's question alone: other leads do not
// distract from it, and a document that sets out to mislead can sway nothing
// but its own rating.
//
// questions.go holds the wording, which is the part most worth reading and
// the part most likely to need tuning.
package jev

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/umran/grafsol/retrieval"
	"github.com/umran/grafsol/typesafe"
)

// Config says how the judge reaches Jev.
type Config struct {
	// Client talks to the System One API. Required.
	Client *typesafe.Client

	// Model is the model to ask; empty means typesafe.DefaultModel, an
	// alias that moves when a new Jev is released. Once the thresholds in
	// retrieval.Config have been tuned against one version, name it here.
	Model string
}

// Judge is a retrieval.Judge. It is safe for concurrent use.
type Judge struct {
	client *typesafe.Client
	model  string
}

var _ retrieval.Judge = (*Judge)(nil)

// New returns a Judge. It panics if cfg has no Client: that is a mistake in
// the program, not something that can happen at run time.
func New(cfg Config) *Judge {
	if cfg.Client == nil {
		panic("jev: Config.Client is required")
	}
	return &Judge{client: cfg.Client, model: cmp.Or(cfg.Model, typesafe.DefaultModel)}
}

// Jev reads at most 64,000 tokens in one request (the state and all the
// questions) and 32,000 for any one question (the state and that question).
// There is no way to count tokens without asking, so sizes are estimated,
// generously, and held a little under the limits. If Jev finds a request too
// long all the same, the request is split and sent again.
var (
	requestBudget  = 60_000
	questionBudget = 30_000
)

// tokens estimates how many tokens a value will be once sent. Three bytes to
// a token overstates it for English prose, which is the safe direction.
func tokens(value any) int {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0 // the request will fail to encode, and say why
	}
	return len(encoded)/3 + 1
}

// Assess implements retrieval.Judge.
//
// It normally makes one request. When the leads are too many or too long to
// fit in one, they are spread over several, sent at the same time; that
// changes nothing about the answers, since every question is answered on its
// own. Only when the evidence itself is too large to be read does Assess
// fail, with retrieval.ErrTooLarge.
func (j *Judge) Assess(ctx context.Context, q retrieval.Question, evidence []retrieval.Evidence, leads []retrieval.Lead) (retrieval.Assessment, error) {
	assessment := retrieval.Assessment{Potential: make([]float64, len(leads))}
	if len(evidence) == 0 && len(leads) == 0 {
		return assessment, nil
	}

	state := stateOf(q)
	stateSize := tokens(state)

	// Every question, by name, with its estimated size. The sufficiency
	// question comes first, so that it goes out in the first request.
	var asked []question
	if len(evidence) > 0 {
		sufficiency := question{name: sufficientName, content: sufficiencyQuestion(evidence)}
		sufficiency.size = tokens(sufficiency.content)
		if stateSize+sufficiency.size > questionBudget {
			return retrieval.Assessment{}, fmt.Errorf("jev: %d pieces of evidence come to about %d tokens: %w",
				len(evidence), stateSize+sufficiency.size, retrieval.ErrTooLarge)
		}
		asked = append(asked, sufficiency)
	}
	for i, lead := range leads {
		rating := question{name: leadName(i), content: leadQuestion(lead)}
		rating.size = tokens(rating.content)
		if stateSize+rating.size > questionBudget {
			return retrieval.Assessment{}, fmt.Errorf("jev: the lead to %s comes to about %d tokens, more than can be judged",
				lead.Target.VertexID(), stateSize+rating.size)
		}
		asked = append(asked, rating)
	}

	answers, used, err := j.ask(ctx, state, pack(asked, stateSize))
	if err != nil {
		return retrieval.Assessment{}, err
	}
	assessment.Tokens = used

	if len(evidence) > 0 {
		answer, err := answerTo(answers, sufficientName, "noul")
		if err != nil {
			return retrieval.Assessment{}, err
		}
		assessment.Sufficient = clamp(answer.Noul)
	}
	for i := range leads {
		answer, err := answerTo(answers, leadName(i), "score")
		if err != nil {
			return retrieval.Assessment{}, err
		}
		// The score is the expected level, from 0 to the top level.
		assessment.Potential[i] = clamp(answer.Score / float64(len(leadLevels)-1))
	}
	return assessment, nil
}

// question is one question on its way to Jev.
type question struct {
	name    string
	content typesafe.Question
	size    int // estimated tokens
}

// pack divides the questions among as few requests as will hold them, in
// order. Each request also carries the state, so each has that much less
// room.
func pack(asked []question, stateSize int) [][]question {
	var requests [][]question
	var current []question
	room := requestBudget - stateSize
	for _, q := range asked {
		if len(current) > 0 && q.size > room {
			requests = append(requests, current)
			current, room = nil, requestBudget-stateSize
		}
		current = append(current, q)
		room -= q.size
	}
	if len(current) > 0 {
		requests = append(requests, current)
	}
	return requests
}

// ask sends the requests, all at the same time, and gathers their answers.
func (j *Judge) ask(ctx context.Context, state any, requests [][]question) (map[string]typesafe.Answer, int, error) {
	var (
		mu       sync.Mutex
		answers  = make(map[string]typesafe.Answer)
		used     int
		failures []error
		group    sync.WaitGroup
	)
	for _, asked := range requests {
		group.Go(func() {
			got, cost, err := j.askOne(ctx, state, asked)
			mu.Lock()
			defer mu.Unlock()
			for name, answer := range got {
				answers[name] = answer
			}
			used += cost
			if err != nil {
				failures = append(failures, err)
			}
		})
	}
	group.Wait()
	if len(failures) > 0 {
		return nil, 0, failures[0]
	}
	return answers, used, nil
}

// askOne sends one request. If Jev finds it too long, which means the
// estimate fell short, the questions are split in two and each half is sent
// on its own. A single question that is too long cannot be split: for the
// sufficiency question that means the evidence is more than Jev can read.
func (j *Judge) askOne(ctx context.Context, state any, asked []question) (map[string]typesafe.Answer, int, error) {
	questions := make(map[string]typesafe.Question, len(asked))
	for _, q := range asked {
		questions[q.name] = q.content
	}
	response, err := j.client.SystemOne(ctx, typesafe.Request{Model: j.model, State: state, Questions: questions})
	if err == nil {
		return response.Answers, response.Usage.InputTokens, nil
	}

	var refused *typesafe.Error
	if !errors.As(err, &refused) || !refused.TooLong() {
		return nil, 0, fmt.Errorf("jev: %w", err)
	}
	if len(asked) == 1 {
		if asked[0].name == sufficientName {
			return nil, 0, fmt.Errorf("jev: %w: %w", retrieval.ErrTooLarge, err)
		}
		return nil, 0, fmt.Errorf("jev: one lead is more than can be judged: %w", err)
	}

	answers := make(map[string]typesafe.Answer, len(asked))
	used := 0
	for _, half := range [][]question{asked[:len(asked)/2], asked[len(asked)/2:]} {
		got, cost, err := j.askOne(ctx, state, half)
		if err != nil {
			return nil, 0, err
		}
		for name, answer := range got {
			answers[name] = answer
		}
		used += cost
	}
	return answers, used, nil
}

// answerTo finds the answer to a question and checks that it is of the kind
// the question called for.
func answerTo(answers map[string]typesafe.Answer, name, kind string) (typesafe.Answer, error) {
	answer, ok := answers[name]
	if !ok {
		return typesafe.Answer{}, fmt.Errorf("jev: the reply has no answer to the question %q", name)
	}
	if answer.Type != kind {
		return typesafe.Answer{}, fmt.Errorf("jev: the answer to the question %q is a %q, not a %q", name, answer.Type, kind)
	}
	return answer, nil
}

// clamp holds a number between 0 and 1.
func clamp(x float64) float64 {
	return max(0, min(1, x))
}

package typesafe

// Request is one call to System One: some content, and questions about it.
type Request struct {
	// Model is the model to ask, by name or alias. Empty means DefaultModel.
	Model string `json:"model"`

	// State is the content every question in the request refers to: a
	// string, or anything that encodes to a JSON object or array. The model
	// reads it once, however many questions there are.
	State any `json:"state"`

	// Questions are the questions, each under a name of the caller's
	// choosing. The names are not shown to the model; the answers come back
	// under the same names. Each question is answered on its own, as if it
	// were the only one.
	Questions map[string]Question `json:"questions"`
}

// Question is one question about the state. Make one with Noul, Score or
// Choice.
type Question struct {
	Type string `json:"type"`

	// Instructions say what to judge: a string, or a JSON object that keeps
	// the question in one field and the data it is about in others.
	Instructions any `json:"instructions,omitempty"`

	// Criteria say what each possible answer means. Their shape depends on
	// the type of question.
	Criteria any `json:"criteria,omitempty"`
}

// Noul asks a yes/no question. The answer is the probability of yes.
//
// yes and no describe what counts as each. They may both be left empty when
// the question leaves no doubt about it.
func Noul(instructions any, yes, no string) Question {
	question := Question{Type: "noul", Instructions: instructions}
	if yes != "" || no != "" {
		question.Criteria = map[string]string{"true": yes, "false": no}
	}
	return question
}

// Score asks where something falls on a scale. levels describe the points
// of the scale, lowest first; there may be up to ten. The answer is the
// expected level, counting from zero, with the probability of each.
func Score(instructions any, levels ...string) Question {
	return Question{Type: "score", Instructions: instructions, Criteria: levels}
}

// Choice asks which one of several options applies. options maps the name
// of each option to a description of when it applies; there may be up to
// 255. The answer is the name picked, with the probability of each.
func Choice(instructions any, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: options}
}

// Response is what System One returns.
type Response struct {
	// Model is the versioned model that answered, which may be more
	// specific than the alias that was asked.
	Model string `json:"model"`

	// Answers holds one answer per question, under the question's name.
	Answers map[string]Answer `json:"answers"`

	Usage Usage `json:"usage"`
}

// Answer is the answer to one question. Which fields are filled in depends
// on Type, which is the type of the question.
type Answer struct {
	Type string `json:"type"`

	// Noul is the probability, from 0 to 1, that the answer to a Noul
	// question is yes. A value near 0.5 means the model cannot tell.
	Noul float64 `json:"noul"`

	// Choice is the name of the option a Choice question settled on.
	Choice string `json:"choice"`

	// Score is the expected level of a Score question, counting from zero.
	// It is the average of the levels weighted by their probabilities, so
	// it may fall between two of them.
	Score float64 `json:"score"`

	// Confidence, for Choice and Score, says how concentrated the
	// probabilities are, from 0 to 1.
	Confidence float64 `json:"confidence"`

	// Probabilities, for Choice and Score, gives the probability of each
	// option by name, or of each level by its number.
	Probabilities map[string]float64 `json:"probabilities"`
}

// Usage is what a request cost. Input tokens are what is billed.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

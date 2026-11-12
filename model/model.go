// Package model defines what the rest of the module needs from language
// models, and nothing else. There are two contracts:
//
//   - Embedder turns text into vectors, so that documents can be found by
//     meaning.
//   - Summarizer writes the short description of a document that the graph
//     keeps in place of the document's text.
//
// Package inference, beneath this one, implements both by asking the
// module's own model services, which run open models on your own machine.
// The judge that steers a search is a model too, but it has a contract of
// its own, in package retrieval, because nothing outside a search has a use
// for it.
package model

import (
	"context"
	"errors"
)

// Embedder turns text into vectors that can be compared by direction: texts
// that mean similar things come out pointing in similar directions.
//
// Queries and documents are embedded by separate methods because some models
// treat the two differently: a question and the passage that answers it are
// not alike as texts. A model that makes no distinction implements both
// methods the same way.
//
// Either way, vectors are only comparable when the same model, set up the
// same way, made all of them. Such a model and its setup are called an
// embedding space, and Space names the one an Embedder works in, so that
// whoever keeps vectors can refuse to mix two.
//
// Implementations must be safe for concurrent use.
type Embedder interface {
	// EmbedDocuments embeds texts that will be stored and searched for. It
	// returns one vector per text, in order, all of one length. No text may
	// be empty.
	EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error)

	// EmbedQueries embeds texts that will be searched with, such as
	// questions, under the same terms as EmbedDocuments.
	EmbedQueries(ctx context.Context, texts []string) ([][]float32, error)

	// Space names the embedding space this embedder's vectors are in: the
	// model that makes them and how it is set up. Two embedders make
	// comparable vectors exactly when their Spaces are equal. The name
	// means nothing else, and is never empty.
	//
	// An Embedder stays in one space for as long as it lives. One that
	// finds its model changed underneath it returns errors, never vectors
	// from another space.
	Space(ctx context.Context) (string, error)
}

// Summarizer writes the description that the graph keeps in place of a
// document's text. The summary is what a search's judge reads to decide
// whether a document helps, and what a caller is shown as evidence, so it
// should state what the document states (decisions, causes, names, numbers)
// rather than say what the document is about.
//
// Implementations must be safe for concurrent use.
type Summarizer interface {
	// Summarize returns a summary of the document with the given title and
	// text. The text comes from a source system and is not to be trusted:
	// it is material to summarise, never instructions to follow.
	Summarize(ctx context.Context, title, text string) (string, error)
}

// ErrDeclined is returned, wrapped, by an Embedder or a Summarizer that
// cannot do anything with the text it was given: the text is longer than
// the model takes, say, or the model wrote nothing usable about it. Unlike
// a rate limit or a timeout, asking again with the same text will get the
// same answer.
var ErrDeclined = errors.New("model: the model declined")

package inference

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/umran/grafsol/model"
)

// EmbedderConfig says where the embeddings service is.
type EmbedderConfig struct {
	// BaseURL is the address of the embeddings service, such as
	// "http://127.0.0.1:3010". Required.
	BaseURL string

	// BatchSize is the most texts sent in one request; zero means 64.
	BatchSize int

	// HTTPClient makes the requests; nil means a client that gives each
	// attempt 75 seconds, a little longer than the service allows itself.
	HTTPClient *http.Client

	// MaxRetries is how many times a request that failed in a way worth
	// retrying is tried again. Zero means 2; a negative number means never.
	MaxRetries int
}

// Embedder is a model.Embedder that asks the embeddings service. It is safe
// for concurrent use.
//
// The service says which embedding space its vectors are in, in its
// description of itself and in every reply. An Embedder remembers the first
// space it is told and holds the service to it: if the service is later
// restarted with another model, its replies are refused, because their
// vectors could not be compared with the ones that came before.
type Embedder struct {
	service service
	batch   int

	mu    sync.Mutex
	space string // the space this Embedder began in; "" until the service has named it
}

var _ model.Embedder = (*Embedder)(nil)

// NewEmbedder returns an Embedder. It panics if cfg has no BaseURL: that is
// a mistake in the program, not something that can happen at run time.
func NewEmbedder(cfg EmbedderConfig) *Embedder {
	if cfg.BaseURL == "" {
		panic("inference: EmbedderConfig.BaseURL is required")
	}
	return &Embedder{
		service: newService("embeddings service", cfg.BaseURL, cfg.HTTPClient, 75*time.Second, cfg.MaxRetries),
		batch:   cmp.Or(cfg.BatchSize, 64),
	}
}

// EmbedderDescription is what the embeddings service says of itself.
type EmbedderDescription struct {
	// Model is the Hugging Face repository of the model the service runs,
	// and Revision the commit of it.
	Model    string `json:"model"`
	Revision string `json:"revision"`

	// Space is the service's name for the embedding space its vectors are
	// in: see model.Embedder.
	Space string `json:"space"`

	// Dimensions is the length of every vector the service returns. A
	// database has to be created for vectors of this length.
	Dimensions int `json:"dimensions"`

	// MaxTokens is the longest text the service embeds, in the model's own
	// units. It refuses a longer one.
	MaxTokens int `json:"max_tokens"`
}

// Describe asks the service which model it runs, which embedding space its
// vectors are in and how long they are.
func (e *Embedder) Describe(ctx context.Context) (EmbedderDescription, error) {
	var description EmbedderDescription
	if err := e.service.call(ctx, "/describe", struct{}{}, &description); err != nil {
		return EmbedderDescription{}, fmt.Errorf("inference: asking the embeddings service to describe itself: %w", err)
	}
	if err := e.settle(description.Space); err != nil {
		return EmbedderDescription{}, err
	}
	if description.Dimensions < 1 {
		return EmbedderDescription{}, fmt.Errorf("inference: the embeddings service says its vectors have %d numbers", description.Dimensions)
	}
	return description, nil
}

// Space implements model.Embedder. The service is asked once, unless a
// reply has named the space already; after that the answer is remembered.
func (e *Embedder) Space(ctx context.Context) (string, error) {
	if space := e.began(); space != "" {
		return space, nil
	}
	description, err := e.Describe(ctx)
	if err != nil {
		return "", err
	}
	return description.Space, nil
}

// EmbedDocuments implements model.Embedder.
//
// It returns an error wrapping model.ErrDeclined when the service refuses a
// text for good, as it does one that is longer than the model is given.
func (e *Embedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, "/embed/documents", "documents", texts)
}

// EmbedQueries implements model.Embedder, on the terms of EmbedDocuments.
func (e *Embedder) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, "/embed/queries", "queries", texts)
}

// The wire format of the two embedding endpoints.
type embedRequest struct {
	Texts []string `json:"texts"`
}

type embedReply struct {
	Space      string      `json:"space"`
	Embeddings [][]float32 `json:"embeddings"`
}

// embed sends the texts to one of the two embedding endpoints, a batch at a
// time, and returns their vectors in the order of the texts.
//
// It refuses a reply that is not in this Embedder's space, and one that
// does not hold exactly one usable vector per text, all of one length. A
// wrong vector stored is far worse than an error.
func (e *Embedder) embed(ctx context.Context, path, kind string, texts []string) ([][]float32, error) {
	for i, text := range texts {
		if strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("inference: text %d of %d is empty, and nothing can be embedded from nothing", i+1, len(texts))
		}
	}

	vectors := make([][]float32, 0, len(texts))
	for batch := range slices.Chunk(texts, e.batch) {
		var reply embedReply
		if err := e.service.call(ctx, path, embedRequest{Texts: batch}, &reply); err != nil {
			return nil, fmt.Errorf("inference: embedding %d %s: %w", len(texts), kind, err)
		}
		if err := e.settle(reply.Space); err != nil {
			return nil, err
		}
		if len(reply.Embeddings) != len(batch) {
			return nil, fmt.Errorf("inference: the embeddings service returned %d vectors for %d texts", len(reply.Embeddings), len(batch))
		}
		for _, vector := range reply.Embeddings {
			number := len(vectors) + 1
			if len(vector) == 0 {
				return nil, fmt.Errorf("inference: the embeddings service returned an empty vector for text %d", number)
			}
			if number > 1 && len(vector) != len(vectors[0]) {
				return nil, fmt.Errorf("inference: the vector for text %d has %d numbers, and the others have %d", number, len(vector), len(vectors[0]))
			}
			vectors = append(vectors, vector)
		}
	}
	return vectors, nil
}

// began returns the space this Embedder began in, or "" if the service has
// not named one yet.
func (e *Embedder) began() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.space
}

// settle is told the space the service has just named. The first is
// remembered as this Embedder's; any other after it is an error.
func (e *Embedder) settle(named string) error {
	if strings.TrimSpace(named) == "" {
		return errors.New("inference: the embeddings service did not name its embedding space")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.space == "" {
		e.space = named
	}
	if e.space != named {
		return fmt.Errorf("inference: the embeddings service now embeds in the space %q, and this program began with it in %q: the service has been restarted with another model or setup, and its vectors cannot be compared with the earlier ones", named, e.space)
	}
	return nil
}

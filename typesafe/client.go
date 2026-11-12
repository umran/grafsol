// Package typesafe is a small client for TypeSafe's System One API, the HTTP
// interface to Jev.
//
// Jev is a System One model: it does not write text. It is given some
// content (the state) and questions about it, and answers each question with
// a probability or a choice that code can act on. One request may carry many
// questions; the model reads the state once and answers every question
// independently of the others.
//
// The client covers what this module uses: Noul, Score and Choice questions
// over text. Images and Box questions are left out. The API is described at
// https://docs.typesafe.ai/api.
package typesafe

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/umran/grafsol/internal/httpretry"
)

const (
	// DefaultBaseURL is where the API lives.
	DefaultBaseURL = "https://api.typesafe.ai"

	// DefaultModel is an alias for the newest stable Jev. Its answers can
	// shift when a new version is released; name a version, such as
	// "jev-1.14.0", to hold them still.
	DefaultModel = "jev-latest"
)

// Config says how to reach the API.
type Config struct {
	// APIKey authenticates every request. Required. It is sent to the API
	// and nowhere else: it is never logged and never put in an error.
	APIKey string

	// BaseURL is the address of the API; empty means DefaultBaseURL.
	BaseURL string

	// HTTPClient makes the requests; nil means a client that gives each
	// attempt 30 seconds.
	HTTPClient *http.Client

	// MaxRetries is how many times a request that failed in a way worth
	// retrying is tried again. Zero means 2, as in TypeSafe's own clients;
	// a negative number means never.
	MaxRetries int

	// UserAgent identifies the caller to the API; empty means "grafsol".
	UserAgent string
}

// Client talks to the System One API. It is safe for concurrent use.
type Client struct {
	endpoint string
	header   http.Header
	poster   httpretry.Poster
}

// New returns a client. It panics if cfg has no APIKey: that is a mistake in
// the program, not something that can happen at run time.
func New(cfg Config) *Client {
	if cfg.APIKey == "" {
		panic("typesafe: Config.APIKey is required")
	}
	return &Client{
		endpoint: strings.TrimRight(cmp.Or(cfg.BaseURL, DefaultBaseURL), "/") + "/v1/systemone",
		header: http.Header{
			"Authorization": {"Bearer " + cfg.APIKey},
			"Content-Type":  {"application/json"},
			"User-Agent":    {cmp.Or(cfg.UserAgent, "grafsol")},
		},
		poster: httpretry.Poster{Client: cfg.HTTPClient, MaxRetries: cfg.MaxRetries},
	}
}

// SystemOne sends the state and the questions and returns the answers.
//
// A request that fails in a way that may pass (the API is overloaded or
// rate limiting, or the connection broke) is retried a few times with a
// growing pause. A reply that is not a success comes back as an *Error.
func (c *Client) SystemOne(ctx context.Context, req Request) (*Response, error) {
	if req.Model == "" {
		req.Model = DefaultModel
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("typesafe: encoding the request: %w", err)
	}

	reply, err := c.poster.Post(ctx, c.endpoint, c.header, body)
	if err != nil {
		return nil, fmt.Errorf("typesafe: reaching the API: %w", err)
	}
	if reply.Status != http.StatusOK {
		return nil, &Error{Status: reply.Status, Detail: detailOf(reply.Content)}
	}

	response := new(Response)
	if err := json.Unmarshal(reply.Content, response); err != nil {
		return nil, fmt.Errorf("typesafe: reading the response: %w", err)
	}
	return response, nil
}

// Error is a reply from the API that was not a success.
type Error struct {
	// Status is the HTTP status code.
	Status int

	// Detail is the API's explanation, as it sent it.
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("typesafe: the API answered %d %s: %s", e.Status, http.StatusText(e.Status), e.Detail)
}

// TooLong reports whether the request was refused for being longer than the
// model's context: the state with all the questions, or the state with one
// of them. Sending fewer questions, or less state, is the only remedy.
func (e *Error) TooLong() bool {
	return e.Status == http.StatusBadRequest && strings.Contains(e.Detail, "max_tokens_exceeded")
}

// detailOf pulls the explanation out of an error reply. The API sends
// {"detail": ...}, where the detail is a string or some JSON; anything else
// is passed on as it came. Long details are cut, and say so.
func detailOf(content []byte) string {
	detail := strings.TrimSpace(string(content))
	var reply struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(content, &reply) == nil && len(reply.Detail) > 0 {
		var text string
		if json.Unmarshal(reply.Detail, &text) == nil {
			detail = text
		} else {
			detail = string(reply.Detail)
		}
	}

	const limit = 2000
	if len(detail) > limit {
		detail = strings.ToValidUTF8(detail[:limit], "") + " … (cut short)"
	}
	return detail
}

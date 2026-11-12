// Package inference is the client for grafsol's own model services: the two
// Python programs in /inference, which run open models on your own machine.
// One embeds text and the other writes summaries.
//
//   - Embedder is a model.Embedder that asks the embeddings service.
//   - Summarizer is a model.Summarizer that asks the summaries service.
//
// Everything about the models themselves lives in the services: which
// models they are, how a question is embedded differently from a passage,
// how a long document is read. This package only carries texts there and
// results back, and is strict about what it accepts in reply.
//
// The wire format is small. Every request is a POST of a JSON object, and
// the folder /inference/contract holds an example of each request and
// reply. The tests of this package and the tests of the services are run
// against those same files.
//
// A service can refuse a text for good: one too long to embed, or one the
// model could not summarise. It then answers 422 with its reason, and the
// methods here return an error that wraps model.ErrDeclined, which the sync
// engine takes as leave to store the document without that enrichment.
package inference

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/umran/grafsol/internal/httpretry"
	"github.com/umran/grafsol/model"
)

// Error is a reply from a service that was not a success.
type Error struct {
	// Status is the HTTP status code.
	Status int

	// Reason is set when the service refused the text (status 422): why it
	// cannot be processed, in the service's own word for it, such as
	// "too_long". An error with a Reason wraps model.ErrDeclined.
	Reason string

	// Message is the service's explanation.
	Message string
}

func (e *Error) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("the service refused the text (%s): %s", e.Reason, e.Message)
	}
	return fmt.Sprintf("the service answered %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

// service is one of the model services as a client sees it: an address
// that JSON is posted to.
type service struct {
	name   string // what an error calls it, such as "embeddings service"
	base   string // its address, without a slash at the end
	header http.Header
	poster httpretry.Poster
}

// newService returns the client's view of the service at an address. With
// no HTTP client of the caller's own, each attempt is given the time named.
func newService(name, address string, client *http.Client, attempt time.Duration, maxRetries int) service {
	return service{
		name:   name,
		base:   strings.TrimRight(address, "/"),
		header: http.Header{"Content-Type": {"application/json"}},
		poster: httpretry.Poster{
			Client:     cmp.Or(client, &http.Client{Timeout: attempt}),
			MaxRetries: maxRetries,
		},
	}
}

// call posts a request to one of the service's endpoints and reads the
// reply into reply. A reply that is not a success comes back as an *Error,
// wrapped in model.ErrDeclined if it is a refusal.
func (s *service) call(ctx context.Context, path string, request, reply any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encoding the request: %w", err)
	}

	answer, err := s.poster.Post(ctx, s.base+path, s.header, body)
	if err != nil {
		return fmt.Errorf("reaching the %s at %s: %w", s.name, s.base, err)
	}
	if answer.Status != http.StatusOK {
		return failure(answer)
	}
	if err := json.Unmarshal(answer.Content, reply); err != nil {
		return fmt.Errorf("reading the reply of the %s: %w", s.name, err)
	}
	return nil
}

// failure makes an error of a reply that is not a success.
//
// Only a 422 that carries the service's own reason is a refusal. A 422 from
// anywhere else, a proxy for instance, is an ordinary failure: taking it for
// a refusal would quietly leave documents without summaries and embeddings.
func failure(answer httpretry.Reply) error {
	if answer.Status == http.StatusUnprocessableEntity {
		var refusal struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if json.Unmarshal(answer.Content, &refusal) == nil && refusal.Reason != "" {
			return fmt.Errorf("%w: %w", model.ErrDeclined, &Error{
				Status:  answer.Status,
				Reason:  refusal.Reason,
				Message: refusal.Message,
			})
		}
	}
	return &Error{Status: answer.Status, Message: messageOf(answer.Content)}
}

// messageOf pulls the explanation out of an error reply.
//
// The services are built on BentoML, which explains itself in two shapes.
// Its own complaint about a malformed request is an object, {"error": ...,
// "detail": [...]}. An error raised by a service comes as a list holding
// one such object, [{"error": ...}], which looks like an oversight in
// BentoML 1.4 and may change; both are read. Anything else is passed on as
// it came. Long messages are cut, and say so.
func messageOf(content []byte) string {
	type explanation struct {
		Error  string `json:"error"`
		Detail []struct {
			Location []any  `json:"loc"`
			Message  string `json:"msg"`
		} `json:"detail"`
	}

	message := strings.TrimSpace(string(content))
	var one explanation
	var several []explanation
	switch {
	case json.Unmarshal(content, &one) == nil && one.Error != "":
	case json.Unmarshal(content, &several) == nil && len(several) > 0 && several[0].Error != "":
		one = several[0]
	default:
		one = explanation{}
	}
	if one.Error != "" {
		message = one.Error
		// What exactly was wrong with the request, field by field.
		var details []string
		for _, detail := range one.Detail {
			var field []string
			for _, step := range detail.Location {
				field = append(field, fmt.Sprint(step))
			}
			details = append(details, strings.Join(field, ".")+": "+detail.Message)
		}
		if len(details) > 0 {
			message += " (" + strings.Join(details, "; ") + ")"
		}
	}

	const limit = 2000
	if len(message) > limit {
		message = strings.ToValidUTF8(message[:limit], "") + " … (cut short)"
	}
	return message
}

// short returns a title fit to put in an error: whole if it is brief, its
// beginning if it is not.
func short(title string) string {
	const limit = 80
	if runes := []rune(title); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return title
}

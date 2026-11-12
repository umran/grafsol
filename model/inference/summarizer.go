package inference

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/umran/grafsol/model"
)

// SummarizerConfig says where the summaries service is.
type SummarizerConfig struct {
	// BaseURL is the address of the summaries service, such as
	// "http://127.0.0.1:3011". Required.
	BaseURL string

	// HTTPClient makes the requests; nil means a client that gives each
	// attempt 16 minutes, a little longer than the service allows itself.
	// That is long because a summary is slow work: seconds for an ordinary
	// document on a laptop, minutes for a very long one.
	HTTPClient *http.Client

	// MaxRetries is how many times a request that failed in a way worth
	// retrying is tried again. Zero means 2; a negative number means never.
	// A summary that ran out of time is never asked for again, whatever
	// this says: see Summarize.
	MaxRetries int
}

// Summarizer is a model.Summarizer that asks the summaries service. It is
// safe for concurrent use.
type Summarizer struct {
	service service
}

var _ model.Summarizer = (*Summarizer)(nil)

// NewSummarizer returns a Summarizer. It panics if cfg has no BaseURL: that
// is a mistake in the program, not something that can happen at run time.
func NewSummarizer(cfg SummarizerConfig) *Summarizer {
	if cfg.BaseURL == "" {
		panic("inference: SummarizerConfig.BaseURL is required")
	}
	summaries := newService("summaries service", cfg.BaseURL, cfg.HTTPClient, 16*time.Minute, cfg.MaxRetries)
	// When a summary has not come in time the model is usually still
	// writing it. Asking again at once would queue the same work behind
	// itself, so a timeout ends the attempt for good.
	summaries.poster.GiveUpOnTimeout = true
	return &Summarizer{service: summaries}
}

// The wire format of the summarising endpoint.
type summarizeRequest struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}

type summarizeReply struct {
	Summary string `json:"summary"`
}

// Summarize implements model.Summarizer.
//
// The whole text is sent. The service reads a long document in parts and
// says so in the summary if it had to leave any of it unread.
//
// It returns an error wrapping model.ErrDeclined when the service refuses
// the document for good: the model did not finish a summary of it, or wrote
// something that is not one. A service that is busy is tried again; one
// that ran out of time is not.
func (s *Summarizer) Summarize(ctx context.Context, title, text string) (string, error) {
	var reply summarizeReply
	if err := s.service.call(ctx, "/summarize", summarizeRequest{Title: title, Text: text}, &reply); err != nil {
		return "", fmt.Errorf("inference: summarising %q: %w", short(title), err)
	}
	summary := strings.TrimSpace(reply.Summary)
	if summary == "" {
		return "", fmt.Errorf("inference: the summaries service returned no summary of %q", short(title))
	}
	return summary, nil
}

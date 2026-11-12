package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"mime"
	"net/url"
	"strings"

	"github.com/umran/grafsol/source"
)

// webhookEvent is the part of a webhook payload this package reads. An
// "issues" event carries the issue, a "pull_request" event the pull request.
type webhookEvent struct {
	Action      string `json:"action"`
	Issue       *issue `json:"issue"`
	PullRequest *issue `json:"pull_request"`
	Repository  struct {
		FullName string `json:"full_name"` // "owner/name"
	} `json:"repository"`
}

// Receive implements source.Receiver for GitHub's webhook deliveries.
//
// It handles two kinds of event, named by the X-GitHub-Event header:
// "issues" and "pull_request". Each carries the issue or pull request as it
// is after the event. Every other kind of event (including the "ping"
// GitHub sends when a webhook is created) is acknowledged and ignored, as
// are events about repositories this Source does not cover. Whatever a
// webhook does not report, the next scan picks up.
//
// For an issue whose payload does not name a parent, Receive makes one API
// request to read the issue afresh; see Config.TrustIssuePayloads for why,
// and for how to turn that off.
func (s *Source) Receive(ctx context.Context, delivery source.Delivery) ([]source.Change, error) {
	if err := s.authenticate(delivery); err != nil {
		return nil, err
	}

	kind := delivery.Header.Get("X-GitHub-Event")
	if kind != "issues" && kind != "pull_request" {
		return nil, nil
	}

	payload, err := payloadOf(delivery)
	if err != nil {
		return nil, err
	}
	var event webhookEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		return nil, fmt.Errorf("github: a %s delivery is not valid JSON: %w", kind, source.ErrMalformed)
	}

	item, isPullRequest := event.Issue, false
	if kind == "pull_request" {
		item, isPullRequest = event.PullRequest, true
	}
	repository := strings.ToLower(event.Repository.FullName)
	if item == nil || item.Number <= 0 || repository == "" {
		return nil, fmt.Errorf("github: a %s delivery names no %s or no repository: %w", kind, kind, source.ErrMalformed)
	}
	if !s.covered[repository] {
		return nil, nil
	}

	switch event.Action {
	case "deleted", "transferred":
		// Either way the issue is gone from this repository. A transferred
		// issue lives on elsewhere under a new number; if its new
		// repository is covered too, it is reported from there.
		return []source.Change{removal(repository, *item)}, nil
	}

	// A change must state everything about a document, including which
	// issue it is a sub-issue of. GitHub's API names the parent only when
	// there is one, so an issue payload without the field means either "no
	// parent" or "webhook payloads do not carry the field", and nothing in
	// the payload says which. Guessing "no parent" wrongly would remove a
	// child_of edge that is still true every time a sub-issue is edited.
	// So unless told that payloads can be trusted, read the issue from the
	// API, which is known to tell. Pull requests cannot be sub-issues, so
	// for them there is nothing to miss.
	if !isPullRequest && !item.mentionsItsParent() && !s.trustIssuePayloads {
		fetched, err := s.fetchIssue(ctx, repository, item.Number)
		if err != nil {
			return nil, err
		}
		item = &fetched
	}

	change, err := s.change(repository, *item, isPullRequest)
	if err != nil {
		return nil, err
	}
	return []source.Change{change}, nil
}

// authenticate checks that a delivery was signed with the webhook's secret,
// which only GitHub and this Source know.
//
// GitHub signs the body of every delivery with HMAC-SHA256 and sends the
// result in the X-Hub-Signature-256 header as "sha256=" followed by the
// signature in hexadecimal.
func (s *Source) authenticate(delivery source.Delivery) error {
	if len(s.secret) == 0 {
		return fmt.Errorf("github: no webhook secret is configured, so no delivery can be trusted: %w", source.ErrUnauthorized)
	}
	signature, found := strings.CutPrefix(delivery.Header.Get("X-Hub-Signature-256"), "sha256=")
	if !found {
		return fmt.Errorf("github: the delivery carries no sha256 signature: %w", source.ErrUnauthorized)
	}
	claimed, err := hex.DecodeString(signature)
	if err != nil {
		return fmt.Errorf("github: the delivery's signature is not hexadecimal: %w", source.ErrUnauthorized)
	}

	mac := hmac.New(sha256.New, s.secret)
	mac.Write(delivery.Body)
	// hmac.Equal takes the same time whatever the two values are, so the
	// comparison does not reveal how much of a forged signature was right.
	if !hmac.Equal(claimed, mac.Sum(nil)) {
		return fmt.Errorf("github: the delivery's signature does not match: %w", source.ErrUnauthorized)
	}
	return nil
}

// payloadOf returns the JSON of a delivery. A webhook can be set up to send
// it as the body itself ("application/json", GitHub's recommendation) or as
// a form with a single field named "payload".
func payloadOf(delivery source.Delivery) ([]byte, error) {
	mediaType, _, _ := mime.ParseMediaType(delivery.Header.Get("Content-Type"))
	if mediaType != "application/x-www-form-urlencoded" {
		return delivery.Body, nil
	}
	form, err := url.ParseQuery(string(delivery.Body))
	if err != nil || form.Get("payload") == "" {
		return nil, fmt.Errorf("github: a form delivery has no payload field: %w", source.ErrMalformed)
	}
	return []byte(form.Get("payload")), nil
}

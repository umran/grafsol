// Package httpretry posts a request to an HTTP API and tries again when the
// failure is of the kind that passes: the API is overloaded or rate
// limiting, or the connection broke. The clients for TypeSafe and for the
// model services all use it, so that they wait and give up in the same way.
package httpretry

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// Poster sends POST requests. The zero value is ready to use.
type Poster struct {
	// Client makes the requests; nil means a client that gives each attempt
	// 30 seconds.
	Client *http.Client

	// MaxRetries is how many times a request that failed in a way worth
	// retrying is tried again. Zero means 2; a negative number means never.
	MaxRetries int

	// GiveUpOnTimeout says not to try again after an attempt that ran out
	// of time: one the client stopped waiting for, or one the API answered
	// with 504. Set it for an API that does slow work on request. When such
	// an API has not finished in time, it is usually still at work, and
	// sending the request again at once only queues the same work behind
	// itself.
	GiveUpOnTimeout bool

	// Pause waits between attempts; nil means wait for real. Tests set it
	// to see how long the waits would have been without sitting them out.
	Pause func(ctx context.Context, wait time.Duration) error
}

// Reply is what an API answered.
type Reply struct {
	Status  int
	Header  http.Header
	Content []byte
}

// defaultClient is used when a Poster has none of its own.
var defaultClient = &http.Client{Timeout: 30 * time.Second}

// Post sends body to url with the given headers and returns the reply,
// whatever its status: turning an unsuccessful reply into an error is the
// caller's business, since only the caller knows what the API's error
// replies look like.
//
// A reply of 408, 429 or 5xx, and a request that got no reply at all, is
// retried up to MaxRetries times, unless GiveUpOnTimeout rules it out.
// Between attempts Post waits for as long as
// the API asked with a Retry-After header, or else for half a second,
// doubling each time up to five seconds. If the retries run out, the last
// reply is returned; if there never was one, the last error is.
func (p *Poster) Post(ctx context.Context, url string, header http.Header, body []byte) (Reply, error) {
	client := p.Client
	if client == nil {
		client = defaultClient
	}
	retries := p.MaxRetries
	switch {
	case retries == 0:
		retries = 2
	case retries < 0:
		retries = 0
	}
	pause := p.Pause
	if pause == nil {
		pause = wait
	}

	for attempt := 0; ; attempt++ {
		reply, err := post(ctx, client, url, header, body)
		if ctx.Err() != nil {
			return Reply{}, ctx.Err()
		}
		passing := err != nil ||
			reply.Status == http.StatusRequestTimeout ||
			reply.Status == http.StatusTooManyRequests ||
			reply.Status >= 500
		if p.GiveUpOnTimeout && (timedOut(err) || reply.Status == http.StatusGatewayTimeout) {
			passing = false
		}
		if !passing || attempt >= retries {
			return reply, err
		}
		if err := pause(ctx, cmp.Or(retryAfter(reply.Header), backoff(attempt))); err != nil {
			return Reply{}, err
		}
	}
}

// post makes one attempt.
func post(ctx context.Context, client *http.Client, url string, header http.Header, body []byte) (Reply, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Reply{}, err
	}
	for name, values := range header {
		request.Header[name] = values
	}

	response, err := client.Do(request)
	if err != nil {
		return Reply{}, err
	}
	defer response.Body.Close()

	content, err := io.ReadAll(response.Body)
	if err != nil {
		return Reply{}, err
	}
	return Reply{Status: response.StatusCode, Header: response.Header, Content: content}, nil
}

// timedOut reports whether an attempt failed because the client stopped
// waiting for a reply.
func timedOut(err error) bool {
	var limited interface{ Timeout() bool }
	return errors.As(err, &limited) && limited.Timeout()
}

// backoff is how long to wait before retrying when the API did not say:
// half a second, doubling with each attempt up to five seconds, moved by up
// to a quarter either way so that many clients do not retry in step.
func backoff(attempt int) time.Duration {
	// Past the fourth doubling the wait is over the ceiling anyway.
	base := min(500*time.Millisecond<<min(attempt, 4), 5*time.Second)
	return time.Duration(float64(base) * (0.75 + 0.5*rand.Float64()))
}

// retryAfter reads the Retry-After header, which gives either a number of
// seconds or a date. It returns zero if there is none it can read.
func retryAfter(header http.Header) time.Duration {
	value := header.Get("Retry-After")
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseFloat(value, 64); err == nil && seconds >= 0 {
		return time.Duration(seconds * float64(time.Second))
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(0, time.Until(when))
	}
	return 0
}

// wait pauses, or stops pausing when the context ends.
func wait(ctx context.Context, pause time.Duration) error {
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

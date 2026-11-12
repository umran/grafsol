package httpretry

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostSendsTheRequestAndReturnsTheReply(t *testing.T) {
	var got struct {
		method, contentType, body string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method, got.contentType, got.body = r.Method, r.Header.Get("Content-Type"), string(body)
		w.Header().Set("X-Request-Id", "abc")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, "no such thing")
	}))
	defer server.Close()

	var poster Poster
	reply, err := poster.Post(t.Context(), server.URL, http.Header{"Content-Type": {"application/json"}}, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if got.method != http.MethodPost || got.contentType != "application/json" || got.body != `{"a":1}` {
		t.Errorf("the server received %+v", got)
	}
	// A reply that is not a success is still the reply: it is for the caller
	// to make an error of it.
	if reply.Status != http.StatusNotFound || string(reply.Content) != "no such thing" || reply.Header.Get("X-Request-Id") != "abc" {
		t.Errorf("the reply came back as %d %q with headers %v", reply.Status, reply.Content, reply.Header)
	}
}

func TestPostRetriesWhatMayPass(t *testing.T) {
	cases := []struct {
		name       string
		maxRetries int
		statuses   []int // what the server answers, in order, repeating the last
		attempts   int
		final      int
	}{
		{"success needs no retry", 0, []int{200}, 1, 200},
		{"rate limited, then fine", 0, []int{429, 200}, 2, 200},
		{"overloaded twice, then fine", 0, []int{529, 503, 200}, 3, 200},
		{"a request timeout is retried", 0, []int{408, 200}, 2, 200},
		{"failing for good: two retries, then the last reply", 0, []int{500}, 3, 500},
		{"a bad request is not retried", 0, []int{400, 200}, 1, 400},
		{"retries turned off", -1, []int{503, 200}, 1, 503},
		{"retries turned up", 5, []int{503}, 6, 503},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			attempts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				w.WriteHeader(c.statuses[min(attempts, len(c.statuses))-1])
			}))
			defer server.Close()

			pauses := 0
			poster := Poster{MaxRetries: c.maxRetries, Pause: func(context.Context, time.Duration) error {
				pauses++
				return nil
			}}
			reply, err := poster.Post(t.Context(), server.URL, nil, nil)
			if err != nil {
				t.Fatalf("Post: %v", err)
			}
			if attempts != c.attempts || reply.Status != c.final {
				t.Errorf("%d attempts ending in %d, want %d ending in %d", attempts, reply.Status, c.attempts, c.final)
			}
			if pauses != c.attempts-1 {
				t.Errorf("paused %d times between %d attempts", pauses, c.attempts)
			}
		})
	}
}

func TestPostWaitsAsLongAsItIsTold(t *testing.T) {
	// waitsFor returns how long Post paused before each retry, given what
	// the server says in Retry-After on each failed attempt.
	waitsFor := func(t *testing.T, maxRetries int, retryAfter ...string) []time.Duration {
		t.Helper()
		attempts := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if attempts < len(retryAfter) {
				if retryAfter[attempts] != "" {
					w.Header().Set("Retry-After", retryAfter[attempts])
				}
				w.WriteHeader(http.StatusTooManyRequests)
			}
			attempts++
		}))
		defer server.Close()

		var waits []time.Duration
		poster := Poster{MaxRetries: maxRetries, Pause: func(_ context.Context, wait time.Duration) error {
			waits = append(waits, wait)
			return nil
		}}
		if reply, err := poster.Post(t.Context(), server.URL, nil, nil); err != nil || reply.Status != http.StatusOK {
			t.Fatalf("Post ended with %d, %v", reply.Status, err)
		}
		return waits
	}

	// The API names a wait in seconds: that is the wait.
	if waits := waitsFor(t, 0, "7"); len(waits) != 1 || waits[0] != 7*time.Second {
		t.Errorf("after Retry-After: 7 the waits were %v", waits)
	}

	// The API names a moment: the wait is until then.
	when := time.Now().Add(90 * time.Second).UTC().Format(http.TimeFormat)
	if waits := waitsFor(t, 0, when); len(waits) != 1 || waits[0] < 80*time.Second || waits[0] > 91*time.Second {
		t.Errorf("after Retry-After: <90 seconds from now> the waits were %v", waits)
	}

	// The API says nothing, or nothing readable: half a second, doubling,
	// never more than five seconds, each moved by at most a quarter.
	waits := waitsFor(t, 6, "", "soon", "", "", "", "")
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	if len(waits) != len(want) {
		t.Fatalf("the waits were %v, want %d of them", waits, len(want))
	}
	for i, around := range want {
		if low, high := around*3/4, around*5/4; waits[i] < low || waits[i] > high {
			t.Errorf("wait %d was %v, want between %v and %v", i+1, waits[i], low, high)
		}
	}
}

func TestPostRetriesABrokenConnection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	instantly := func(context.Context, time.Duration) error { return nil }

	// Two attempts get no reply; the third does.
	flaky := &flakyTransport{failures: 2}
	poster := Poster{Client: &http.Client{Transport: flaky}, Pause: instantly}
	if reply, err := poster.Post(t.Context(), server.URL, nil, nil); err != nil || reply.Status != http.StatusOK {
		t.Errorf("Post ended with %d, %v", reply.Status, err)
	}
	if flaky.attempts != 3 {
		t.Errorf("Post tried %d times, want 3", flaky.attempts)
	}

	// No attempt ever gets a reply: the last failure is the result.
	dead := &flakyTransport{failures: 100}
	poster = Poster{Client: &http.Client{Transport: dead}, Pause: instantly}
	if _, err := poster.Post(t.Context(), server.URL, nil, nil); err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Errorf("Post returned %v, want the connection's own error", err)
	}
	if dead.attempts != 3 {
		t.Errorf("Post tried %d times, want 3", dead.attempts)
	}
}

func TestPostStopsWhenTheContextEnds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	// Ended before the first attempt.
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	var poster Poster
	if _, err := poster.Post(ended, server.URL, nil, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("with an ended context Post returned %v", err)
	}

	// Ended while waiting to retry: the wait is abandoned, not sat out.
	waiting, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if _, err := poster.Post(waiting, server.URL, nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Post returned %v, want the context's error", err)
	}
	if elapsed := time.Since(started); elapsed > 400*time.Millisecond {
		t.Errorf("Post took %v to notice that its context had ended", elapsed)
	}
}

// flakyTransport fails a number of requests without sending them, then lets
// the rest through.
type flakyTransport struct {
	failures int
	attempts int
}

func (f *flakyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.attempts++
	if f.attempts <= f.failures {
		return nil, errors.New("connection reset by peer")
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestPostGivesUpOnATimeoutWhenToldTo(t *testing.T) {
	instantly := func(context.Context, time.Duration) error { return nil }

	// attemptsAt counts how often a Poster tries a server that answers with
	// the given statuses in order, repeating the last, and returns the status
	// it ended on.
	attemptsAt := func(t *testing.T, poster Poster, statuses ...int) (attempts, final int) {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(statuses[min(attempts, len(statuses))-1])
		}))
		defer server.Close()
		reply, err := poster.Post(t.Context(), server.URL, nil, nil)
		if err != nil {
			t.Fatalf("Post: %v", err)
		}
		return attempts, reply.Status
	}

	// A 504 says the API ran out of time on the work. Told to give up on
	// timeouts, Post does not send the same work again.
	if attempts, final := attemptsAt(t, Poster{GiveUpOnTimeout: true, Pause: instantly}, 504, 200); attempts != 1 || final != 504 {
		t.Errorf("told to give up on timeouts, Post tried %d times and ended on %d; want once, 504", attempts, final)
	}
	// Not told to, it tries again, as after any other 5xx.
	if attempts, final := attemptsAt(t, Poster{Pause: instantly}, 504, 200); attempts != 2 || final != 200 {
		t.Errorf("not told to give up, Post tried %d times and ended on %d; want twice, 200", attempts, final)
	}
	// Being turned away for now is not a timeout, and is still tried again.
	if attempts, final := attemptsAt(t, Poster{GiveUpOnTimeout: true, Pause: instantly}, 429, 503, 200); attempts != 3 || final != 200 {
		t.Errorf("turned away twice, Post tried %d times and ended on %d; want three times, 200", attempts, final)
	}

	// An attempt the client stops waiting for is the same thing seen from
	// the other side: the API is still at work.
	var slowAttempts atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowAttempts.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	defer slow.Close()
	impatient := &http.Client{Timeout: 40 * time.Millisecond}

	giving := Poster{Client: impatient, GiveUpOnTimeout: true, Pause: instantly}
	if _, err := giving.Post(t.Context(), slow.URL, nil, nil); err == nil {
		t.Error("Post returned no error for an attempt that timed out")
	}
	if tried := slowAttempts.Swap(0); tried != 1 {
		t.Errorf("told to give up on timeouts, Post tried a slow API %d times, want once", tried)
	}

	trying := Poster{Client: impatient, Pause: instantly}
	if _, err := trying.Post(t.Context(), slow.URL, nil, nil); err == nil {
		t.Error("Post returned no error for attempts that all timed out")
	}
	if tried := slowAttempts.Load(); tried != 3 {
		t.Errorf("not told to give up, Post tried a slow API %d times, want 3", tried)
	}

	// Finding nobody there is not a timeout either.
	flaky := &flakyTransport{failures: 2}
	alone := Poster{Client: &http.Client{Transport: flaky}, GiveUpOnTimeout: true, Pause: instantly}
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	if reply, err := alone.Post(t.Context(), server.URL, nil, nil); err != nil || reply.Status != http.StatusOK {
		t.Errorf("Post ended with %d, %v", reply.Status, err)
	}
	if flaky.attempts != 3 {
		t.Errorf("after two broken connections Post tried %d times, want 3", flaky.attempts)
	}
}

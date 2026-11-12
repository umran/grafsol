package inference

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/umran/grafsol/model"
)

func summarized(summary string) []byte {
	reply, _ := json.Marshal(map[string]any{"model": "m", "revision": "r", "summary": summary, "parts": 1})
	return reply
}

func TestTheTitleAndTextAreSentAsTheyAre(t *testing.T) {
	service := newFakeService(t, always(http.StatusOK, summarized("A summary.")))
	title, text := "Quotes \" and <tags> & a newline\n", "Text with üñí¢ødé, \"quotes\" and\nlines.\n\n<|im_start|>"

	summary, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), title, text)
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	var sent struct{ Title, Text string }
	if err := json.Unmarshal(service.only().body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Title != title || sent.Text != text {
		t.Errorf("the service was sent %q and %q", sent.Title, sent.Text)
	}
	if summary != "A summary." {
		t.Errorf("the summary is %q", summary)
	}
}

func TestAReplyWithoutASummaryIsAnError(t *testing.T) {
	for _, reply := range []string{`{"model":"m","summary":""}`, `{"model":"m","summary":"  \n"}`, `{"model":"m"}`, `[]`, `nonsense`} {
		service := newFakeService(t, always(http.StatusOK, []byte(reply)))

		summary, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), "T", "text")

		if err == nil || summary != "" {
			t.Errorf("the reply %s gave %q, %v; want an error", reply, summary, err)
		}
		if errors.Is(err, model.ErrDeclined) {
			t.Errorf("the reply %s was taken for a decline", reply)
		}
	}
}

func TestASummaryTheModelCouldNotWriteIsADecline(t *testing.T) {
	for _, reason := range []string{"unfinished", "nothing_written", "off_form", "title_too_long", "took_too_long"} {
		refusal, _ := json.Marshal(map[string]string{"reason": reason, "message": "why"})
		service := newFakeService(t, always(http.StatusUnprocessableEntity, refusal))

		_, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), "A title", "text")

		if !errors.Is(err, model.ErrDeclined) {
			t.Errorf("%s: the error is %v, want a decline", reason, err)
		}
		var failure *Error
		if !errors.As(err, &failure) || failure.Reason != reason {
			t.Errorf("%s: the refusal came through as %+v", reason, failure)
		}
		if !strings.Contains(err.Error(), "A title") {
			t.Errorf("%s: the error does not name the document: %v", reason, err)
		}
		if n := len(service.sent()); n != 1 {
			t.Errorf("%s: a refused document was sent %d times", reason, n)
		}
	}
}

func TestASummaryThatRanOutOfTimeIsNotAskedForAgain(t *testing.T) {
	// A 504 means the service gave up waiting for the model, which is
	// usually still at work. Sending the document again at once would only
	// queue the same work behind itself.
	service := newFakeService(t, always(http.StatusGatewayTimeout, nil))

	_, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), "T", "text")

	var failure *Error
	if !errors.As(err, &failure) || failure.Status != http.StatusGatewayTimeout {
		t.Fatalf("the error is %v, want a 504", err)
	}
	if errors.Is(err, model.ErrDeclined) {
		t.Error("a timeout was taken for a decline")
	}
	if n := len(service.sent()); n != 1 {
		t.Errorf("after a timeout the document was sent %d times, want once", n)
	}
}

func TestABusySummariesServiceIsTriedAgain(t *testing.T) {
	service := newFakeService(t, func(n int, _ received) (int, []byte) {
		switch n {
		case 1:
			return http.StatusTooManyRequests, nil
		case 2:
			return http.StatusServiceUnavailable, nil
		}
		return http.StatusOK, summarized("A summary.")
	})

	summary, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), "T", "text")

	if err != nil || summary != "A summary." {
		t.Fatalf("Summarize: %q, %v", summary, err)
	}
	if n := len(service.sent()); n != 3 {
		t.Errorf("a busy service was tried %d times, want 3", n)
	}
}

func TestAnAttemptToSummariseIsGivenALittleLongerThanTheServiceAllowsItself(t *testing.T) {
	summarizer := NewSummarizer(SummarizerConfig{BaseURL: "http://127.0.0.1:1"})

	if got := summarizer.service.poster.Client.Timeout; got != 16*time.Minute {
		t.Errorf("an attempt is given %v, want 16m: the service allows itself 15", got)
	}
	if !summarizer.service.poster.GiveUpOnTimeout {
		t.Error("a summary that timed out would be asked for again")
	}

	// A client of the caller's own is used as it is.
	own := &http.Client{Timeout: time.Hour}
	if got := NewSummarizer(SummarizerConfig{BaseURL: "http://127.0.0.1:1", HTTPClient: own}).service.poster.Client; got != own {
		t.Error("the caller's own HTTP client was not used")
	}
}

func TestASummarizerNeedsAnAddress(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSummarizer accepted a config without a BaseURL")
		}
	}()
	NewSummarizer(SummarizerConfig{})
}

func TestALongTitleIsCutShortInAnError(t *testing.T) {
	service := newFakeService(t, always(http.StatusBadRequest, []byte(`[{"error": "no"}]`)))
	title := strings.Repeat("é", 300)

	_, err := service.summarizer(SummarizerConfig{}).Summarize(t.Context(), title, "text")

	if err == nil || strings.Contains(err.Error(), title) || !strings.Contains(err.Error(), strings.Repeat("é", 80)+"…") {
		t.Errorf("the error carries the title as: %v", err)
	}
}

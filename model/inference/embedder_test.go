package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// The names of two embedding spaces, as the service would make them up.
const (
	spaceA = "acme/embedder-a@1f2e; query prompt search; document prompt none; 2 dimensions"
	spaceB = "acme/embedder-b@9c0d; query prompt none; document prompt none; 2 dimensions"
)

// embeddingService is a fake embeddings service that embeds for real, in
// its own way: a text's vector is its length and its first byte. It says
// its vectors are in the given space.
func embeddingService(t *testing.T, space string) *fakeService {
	t.Helper()
	return newFakeService(t, func(_ int, r received) (int, []byte) {
		if r.path == "/describe" {
			return http.StatusOK, describedReply(space)
		}
		var request struct {
			Texts []string `json:"texts"`
		}
		if err := json.Unmarshal(r.body, &request); err != nil {
			return http.StatusBadRequest, nil
		}
		return http.StatusOK, embeddedReply(space, request.Texts)
	})
}

func describedReply(space string) []byte {
	reply, _ := json.Marshal(map[string]any{"model": "acme/embedder", "revision": "1f2e", "space": space, "dimensions": 2, "max_tokens": 100})
	return reply
}

func vectorFor(text string) []float32 { return []float32{float32(len(text)), float32(text[0])} }

func embeddedReply(space string, texts []string) []byte {
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = vectorFor(text)
	}
	reply, _ := json.Marshal(map[string]any{"model": "acme/embedder", "revision": "1f2e", "space": space, "embeddings": vectors})
	return reply
}

func TestTextsAreSentInBatchesAndTheVectorsComeBackInOrder(t *testing.T) {
	service := embeddingService(t, spaceA)
	texts := make([]string, 150)
	for i := range texts {
		texts[i] = fmt.Sprintf("%c text %d", 'a'+i%26, i)
	}

	vectors, err := service.embedder(EmbedderConfig{}).EmbedDocuments(t.Context(), texts)
	if err != nil {
		t.Fatalf("EmbedDocuments: %v", err)
	}

	var sizes []int
	for _, request := range service.sent() {
		sizes = append(sizes, len(textsOf(t, request.body)))
	}
	if !slices.Equal(sizes, []int{64, 64, 22}) {
		t.Errorf("150 texts were sent in batches of %v, want 64, 64 and 22", sizes)
	}
	if len(vectors) != len(texts) {
		t.Fatalf("%d vectors for %d texts", len(vectors), len(texts))
	}
	for i, text := range texts {
		if !slices.Equal(vectors[i], vectorFor(text)) {
			t.Fatalf("vector %d is %v, want %v", i, vectors[i], vectorFor(text))
		}
	}

	// The batch size is the caller's to set.
	service = embeddingService(t, spaceA)
	if _, err := service.embedder(EmbedderConfig{BatchSize: 100}).EmbedQueries(t.Context(), texts); err != nil {
		t.Fatalf("EmbedQueries: %v", err)
	}
	if n := len(service.sent()); n != 2 {
		t.Errorf("150 texts in batches of 100 took %d requests, want 2", n)
	}
}

func TestNoTextsNeedNoRequest(t *testing.T) {
	service := embeddingService(t, spaceA)

	vectors, err := service.embedder(EmbedderConfig{}).EmbedDocuments(t.Context(), nil)

	if err != nil || len(vectors) != 0 {
		t.Errorf("embedding nothing gave %v, %v", vectors, err)
	}
	if n := len(service.sent()); n != 0 {
		t.Errorf("embedding nothing sent %d requests", n)
	}
}

func TestAnEmptyTextIsCaughtBeforeAnythingIsSent(t *testing.T) {
	service := embeddingService(t, spaceA)

	_, err := service.embedder(EmbedderConfig{}).EmbedDocuments(t.Context(), []string{"one", "", "three"})

	if err == nil || !strings.Contains(err.Error(), "text 2 of 3 is empty") {
		t.Errorf("the error is %v, want one that names the empty text", err)
	}
	if n := len(service.sent()); n != 0 {
		t.Errorf("%d requests were sent although a text was empty", n)
	}
}

func TestAReplyThatDoesNotMatchTheTextsIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		reply string
		want  string
	}{
		{"too few vectors", `{"space":"s","embeddings":[[1,2]]}`, "1 vectors for 2 texts"},
		{"too many vectors", `{"space":"s","embeddings":[[1,2],[3,4],[5,6]]}`, "3 vectors for 2 texts"},
		{"an empty vector", `{"space":"s","embeddings":[[1,2],[]]}`, "empty vector for text 2"},
		{"vectors of two lengths", `{"space":"s","embeddings":[[1,2],[3,4,5]]}`, "text 2 has 3 numbers, and the others have 2"},
		{"no vectors at all", `{"space":"s"}`, "0 vectors for 2 texts"},
		{"not JSON", `<html>`, "reading the reply"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			service := newFakeService(t, always(http.StatusOK, []byte(c.reply)))

			vectors, err := service.embedder(EmbedderConfig{}).EmbedDocuments(t.Context(), []string{"one", "two"})

			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("the error is %v, want one that says %q", err, c.want)
			}
			if vectors != nil {
				t.Errorf("vectors were returned along with the error: %v", vectors)
			}
		})
	}
}

func TestVectorsOfDifferentLengthsInDifferentBatchesAreRefused(t *testing.T) {
	service := newFakeService(t, func(n int, _ received) (int, []byte) {
		if n == 1 {
			return http.StatusOK, []byte(`{"space":"s","embeddings":[[1,2]]}`)
		}
		return http.StatusOK, []byte(`{"space":"s","embeddings":[[1,2,3]]}`)
	})

	_, err := service.embedder(EmbedderConfig{BatchSize: 1}).EmbedDocuments(t.Context(), []string{"one", "two"})

	if err == nil || !strings.Contains(err.Error(), "text 2 has 3 numbers, and the others have 2") {
		t.Errorf("the error is %v", err)
	}
}

func TestSpaceIsAskedOfTheServiceOnceAndRemembered(t *testing.T) {
	service := embeddingService(t, spaceA)
	embedder := service.embedder(EmbedderConfig{})

	for range 3 {
		space, err := embedder.Space(t.Context())
		if err != nil || space != spaceA {
			t.Fatalf("Space is %q, %v; want %q", space, err, spaceA)
		}
	}

	sent := service.sent()
	if len(sent) != 1 || sent[0].path != "/describe" {
		t.Errorf("asking for the space three times sent %d requests, want one, to /describe", len(sent))
	}
}

func TestTheFirstReplySettlesTheSpaceIfNothingHasYet(t *testing.T) {
	service := embeddingService(t, spaceA)
	embedder := service.embedder(EmbedderConfig{})

	if _, err := embedder.EmbedQueries(t.Context(), []string{"a question"}); err != nil {
		t.Fatalf("EmbedQueries: %v", err)
	}
	space, err := embedder.Space(t.Context())

	if err != nil || space != spaceA {
		t.Errorf("Space is %q, %v; want %q", space, err, spaceA)
	}
	if n := len(service.sent()); n != 1 {
		t.Errorf("%d requests were sent; the embedding's own reply should have been enough", n)
	}
}

func TestAnEmbedderStaysInTheSpaceItBeganIn(t *testing.T) {
	// The service is restarted with another model while the program runs:
	// its replies change space after the first.
	changed := func(began func(e *Embedder) error) (*Embedder, error) {
		t.Helper()
		space := spaceA
		service := newFakeService(t, func(n int, r received) (int, []byte) {
			if n > 1 {
				space = spaceB
			}
			if r.path == "/describe" {
				return http.StatusOK, describedReply(space)
			}
			return http.StatusOK, embeddedReply(space, textsOf(t, r.body))
		})
		embedder := service.embedder(EmbedderConfig{})
		return embedder, began(embedder)
	}
	embed := func(e *Embedder) error {
		_, err := e.EmbedDocuments(t.Context(), []string{"a document"})
		return err
	}
	ask := func(e *Embedder) error {
		_, err := e.Space(t.Context())
		return err
	}
	describe := func(e *Embedder) error {
		_, err := e.Describe(t.Context())
		return err
	}

	for name, began := range map[string]func(*Embedder) error{"by embedding": embed, "by asking for its space": ask} {
		for after, then := range map[string]func(*Embedder) error{"embedding": embed, "describing": describe} {
			embedder, err := changed(began)
			if err != nil {
				t.Fatalf("an embedder that began %s: %v", name, err)
			}

			err = then(embedder)

			if err == nil || !strings.Contains(err.Error(), spaceA) || !strings.Contains(err.Error(), spaceB) {
				t.Errorf("began %s, then %s after the service changed: the error is %v, want one that names both spaces", name, after, err)
			}
			// It still says the space it began in, and asks nobody.
			if space, err := embedder.Space(t.Context()); err != nil || space != spaceA {
				t.Errorf("began %s: after the service changed, Space is %q, %v; want %q still", name, space, err, spaceA)
			}
		}
	}

	// Vectors from the other space are never handed over.
	embedder, _ := changed(embed)
	if vectors, err := embedder.EmbedQueries(t.Context(), []string{"a question"}); err == nil || vectors != nil {
		t.Errorf("vectors from another space were returned: %v, %v", vectors, err)
	}
}

func TestAReplyThatDoesNotNameItsSpaceIsRefused(t *testing.T) {
	for _, reply := range []string{`{"embeddings":[[1,2]]}`, `{"space":"","embeddings":[[1,2]]}`, `{"space":"  ","embeddings":[[1,2]]}`} {
		service := newFakeService(t, always(http.StatusOK, []byte(reply)))

		vectors, err := service.embedder(EmbedderConfig{}).EmbedQueries(t.Context(), []string{"q"})

		if err == nil || vectors != nil || !strings.Contains(err.Error(), "did not name its embedding space") {
			t.Errorf("the reply %s gave %v, %v", reply, vectors, err)
		}
	}

	service := newFakeService(t, always(http.StatusOK, []byte(`{"model":"m","revision":"r","dimensions":4,"max_tokens":8}`)))
	if _, err := service.embedder(EmbedderConfig{}).Describe(t.Context()); err == nil {
		t.Error("a description that names no space was accepted")
	}
	if _, err := service.embedder(EmbedderConfig{}).Space(t.Context()); err == nil {
		t.Error("Space returned no error although the service named none")
	}
}

func TestADescriptionMustSayHowLongTheVectorsAre(t *testing.T) {
	service := newFakeService(t, always(http.StatusOK, []byte(`{"model":"m","revision":"r","space":"s","dimensions":0,"max_tokens":8}`)))

	if _, err := service.embedder(EmbedderConfig{}).Describe(t.Context()); err == nil {
		t.Error("a description with vectors of no length was accepted")
	}
}

func TestABusyEmbeddingsServiceIsTriedAgain(t *testing.T) {
	service := newFakeService(t, func(n int, r received) (int, []byte) {
		if n < 3 {
			return http.StatusTooManyRequests, nil
		}
		return http.StatusOK, embeddedReply(spaceA, textsOf(t, r.body))
	})

	vectors, err := service.embedder(EmbedderConfig{}).EmbedQueries(t.Context(), []string{"a question"})

	if err != nil || len(vectors) != 1 {
		t.Fatalf("EmbedQueries: %v, %v", vectors, err)
	}
	if n := len(service.sent()); n != 3 {
		t.Errorf("a busy service was tried %d times, want 3", n)
	}

	// Told never to retry, it does not.
	service = newFakeService(t, always(http.StatusTooManyRequests, nil))
	if _, err := service.embedder(EmbedderConfig{MaxRetries: -1}).EmbedQueries(t.Context(), []string{"q"}); err == nil {
		t.Error("a busy service gave no error")
	}
	if n := len(service.sent()); n != 1 {
		t.Errorf("with retries off the service was tried %d times", n)
	}
}

func TestTheAddressMayEndInASlash(t *testing.T) {
	service := embeddingService(t, spaceA)
	embedder := NewEmbedder(EmbedderConfig{BaseURL: service.server.URL + "/"})

	if _, err := embedder.EmbedQueries(t.Context(), []string{"q"}); err != nil {
		t.Fatalf("EmbedQueries: %v", err)
	}
	if path := service.only().path; path != "/embed/queries" {
		t.Errorf("the request went to %q", path)
	}
}

func TestAnEmbedderNeedsAnAddress(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewEmbedder accepted a config without a BaseURL")
		}
	}()
	NewEmbedder(EmbedderConfig{})
}

func TestEmbeddingStopsWhenTheContextEnds(t *testing.T) {
	service := embeddingService(t, spaceA)
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := service.embedder(EmbedderConfig{}).EmbedQueries(ended, []string{"q"})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("with an ended context the error is %v", err)
	}
}

func TestAnAttemptToEmbedIsGivenALittleLongerThanTheServiceAllowsItself(t *testing.T) {
	embedder := NewEmbedder(EmbedderConfig{BaseURL: "http://127.0.0.1:1"})

	if got := embedder.service.poster.Client.Timeout; got != 75*time.Second {
		t.Errorf("an attempt is given %v, want 75s: the service allows itself 60", got)
	}
}

package evaluation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/umran/grafsol/storage/memory"
)

// countingEmbedder is a stand-in for an embedding model. A text's vector is
// made from the text itself, differently for a query and for a document, so
// a test can tell whether the right one came back. It keeps count of what it
// was asked.
type countingEmbedder struct {
	space string

	mu        sync.Mutex
	documents []string // every text it was asked to embed as a document
	queries   []string
}

func (e *countingEmbedder) Space(context.Context) (string, error) { return e.space, nil }

func (e *countingEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.documents = append(e.documents, texts...)
	return vectorsOf(texts, 1), nil
}

func (e *countingEmbedder) EmbedQueries(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.queries = append(e.queries, texts...)
	return vectorsOf(texts, -1), nil
}

// vectorsOf gives each text a vector of three numbers that depend on the
// text, with a sign that says which way it was embedded.
func vectorsOf(texts []string, sign float32) [][]float32 {
	vectors := make([][]float32, len(texts))
	for i, text := range texts {
		vectors[i] = []float32{sign * float32(len(text)), float32(text[0]) / 7, float32(math.Pi)}
	}
	return vectors
}

func TestVectorsAskTheModelOnceAndServeFromTheFileAfter(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "vectors.json")
	model := &countingEmbedder{space: "model-a; 3 dimensions"}

	// The first time, everything is asked of the model.
	vectors, err := OpenVectors(path)
	if err != nil {
		t.Fatalf("opening a file that does not exist yet: %v", err)
	}
	recording := vectors.Recording(model)
	documents, err := recording.EmbedDocuments(ctx, []string{"alpha", "beta"})
	if err != nil {
		t.Fatal(err)
	}
	// The same text embedded as a query is another vector, kept apart.
	queries, err := recording.EmbedQueries(ctx, []string{"alpha"})
	if err != nil {
		t.Fatal(err)
	}
	if documents[0][0] != 5 || queries[0][0] != -5 {
		t.Fatalf("the document vector is %v and the query vector %v", documents[0], queries[0])
	}
	if space, _ := recording.Space(ctx); space != model.space {
		t.Errorf("recording reports the space %q", space)
	}
	if err := vectors.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Read back, the file answers without the model, bit for bit.
	reopened, err := OpenVectors(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	served := reopened.Embedder()
	again, err := served.EmbedDocuments(ctx, []string{"beta", "alpha"})
	if err != nil {
		t.Fatalf("serving from the file: %v", err)
	}
	if !slices.Equal(again[0], documents[1]) || !slices.Equal(again[1], documents[0]) {
		t.Errorf("served %v, want %v in the order asked", again, [][]float32{documents[1], documents[0]})
	}
	if again[0][2] != float32(math.Pi) {
		t.Errorf("a number came back as %v, not exactly as it was stored", again[0][2])
	}
	asQuery, err := served.EmbedQueries(ctx, []string{"alpha"})
	if err != nil || !slices.Equal(asQuery[0], queries[0]) {
		t.Errorf("the query vector was served as %v, %v", asQuery, err)
	}
	if space, err := served.Space(ctx); err != nil || space != model.space {
		t.Errorf("the file reports the space %q, %v", space, err)
	}

	// Recording again asks the model only for what the file does not hold.
	model.documents, model.queries = nil, nil
	recording = reopened.Recording(model)
	if _, err := recording.EmbedDocuments(ctx, []string{"alpha", "gamma", "beta"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(model.documents, []string{"gamma"}) || len(model.queries) != 0 {
		t.Errorf("the model was asked for documents %v and queries %v, want only gamma", model.documents, model.queries)
	}
}

func TestVectorsRefuseWhatTheyDoNotHold(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "vectors.json")

	// An empty file serves nothing, not even the name of a space.
	empty, err := OpenVectors(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Embedder().Space(ctx); !errors.Is(err, ErrNoVector) {
		t.Errorf("an empty file named a space: %v", err)
	}

	recording := empty.Recording(&countingEmbedder{space: "model-a"})
	if _, err := recording.EmbedDocuments(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	served := empty.Embedder()
	_, err = served.EmbedDocuments(ctx, []string{"alpha", "beta", "gamma"})
	if !errors.Is(err, ErrNoVector) || !strings.Contains(err.Error(), "2 of 3") {
		t.Errorf("asking for two texts the file lacks returned %v, want ErrNoVector counting them", err)
	}
	// It holds alpha as a document, not as a query.
	if _, err := served.EmbedQueries(ctx, []string{"alpha"}); !errors.Is(err, ErrNoVector) {
		t.Errorf("a text kept as a document was served as a query: %v", err)
	}
}

func TestVectorsOfAnotherModelAreThrownAway(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "vectors.json")
	vectors, err := OpenVectors(path)
	if err != nil {
		t.Fatal(err)
	}
	first := &countingEmbedder{space: "model-a"}
	if _, err := vectors.Recording(first).EmbedDocuments(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}

	// The same file, another model: vectors from two models cannot be
	// compared, so nothing made by the first may be served again.
	second := &countingEmbedder{space: "model-b"}
	recording := vectors.Recording(second)
	if _, err := recording.EmbedDocuments(ctx, []string{"alpha"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(second.documents, []string{"alpha"}) {
		t.Errorf("the second model was asked for %v, want alpha again", second.documents)
	}
	if got := vectors.Space(); got != "model-b" {
		t.Errorf("the file is now in the space %q", got)
	}
	if got := vectors.Len(); got != 1 {
		t.Errorf("the file holds %d vectors, want only the second model's one", got)
	}
}

func TestPruneDropsWhatWasNotAskedFor(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "vectors.json")
	model := &countingEmbedder{space: "model-a"}

	vectors, _ := OpenVectors(path)
	if _, err := vectors.Recording(model).EmbedDocuments(ctx, []string{"alpha", "beta", "gamma"}); err != nil {
		t.Fatal(err)
	}
	if err := vectors.Save(); err != nil {
		t.Fatal(err)
	}

	// A later session asks for two of the three: a question was reworded,
	// and the old wording's vector is of no more use.
	later, _ := OpenVectors(path)
	if _, err := later.Recording(model).EmbedDocuments(ctx, []string{"gamma", "alpha"}); err != nil {
		t.Fatal(err)
	}
	if dropped := later.Prune(); dropped != 1 || later.Len() != 2 {
		t.Errorf("Prune dropped %d and left %d, want 1 and 2", dropped, later.Len())
	}
	if err := later.Save(); err != nil {
		t.Fatal(err)
	}
	final, _ := OpenVectors(path)
	if _, err := final.Embedder().EmbedDocuments(ctx, []string{"beta"}); !errors.Is(err, ErrNoVector) {
		t.Errorf("a pruned vector was still served: %v", err)
	}
	if _, err := final.Embedder().EmbedDocuments(ctx, []string{"alpha", "gamma"}); err != nil {
		t.Errorf("a vector that was asked for was pruned: %v", err)
	}
}

func TestVectorsFileIsCheckedWhenRead(t *testing.T) {
	dir := t.TempDir()
	broken := map[string]string{
		"not JSON":                    `{"space": "m", `,
		"a vector that is not base64": `{"space": "m", "vectors": {"k": "%%%"}}`,
		"a vector of a broken length": `{"space": "m", "vectors": {"k": "AAAAAAA="}}`,
		"vectors and no space":        `{"space": "", "vectors": {"k": "AAAAAA=="}}`,
	}
	for name, content := range broken {
		path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".json")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenVectors(path); err == nil {
			t.Errorf("a file with %s was opened", name)
		}
	}
}

func TestVectorsAreSafeToUseFromManyGoroutines(t *testing.T) {
	ctx := t.Context()
	vectors, _ := OpenVectors(filepath.Join(t.TempDir(), "vectors.json"))
	recording := vectors.Recording(&countingEmbedder{space: "model-a"})

	var group sync.WaitGroup
	for i := range 8 {
		group.Go(func() {
			texts := []string{fmt.Sprint("text ", i), "shared"}
			if _, err := recording.EmbedDocuments(ctx, texts); err != nil {
				t.Error(err)
			}
			if _, err := vectors.Embedder().EmbedDocuments(ctx, texts); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	if got := vectors.Len(); got != 9 {
		t.Errorf("the file holds %d vectors, want 9", got)
	}
}

func TestVectorsForAWholeTestSet(t *testing.T) {
	ctx := t.Context()
	set := example(t)
	path := filepath.Join(t.TempDir(), "vectors.json")
	vectors, _ := OpenVectors(path)
	model := &countingEmbedder{space: "model-a"}

	// Four documents and a topic, embedded as documents, and three
	// questions, embedded as queries.
	if lacking, err := vectors.Lacking(ctx, set); err != nil || lacking != 8 {
		t.Fatalf("an empty file lacks %d texts, %v; want 8", lacking, err)
	}
	if vectors.Len() != 0 || len(model.documents) != 0 {
		t.Fatalf("counting what is lacking kept %d vectors and asked the model for %d", vectors.Len(), len(model.documents))
	}

	asked, err := vectors.Record(ctx, set, model)
	if err != nil || asked != 8 {
		t.Fatalf("Record asked the model for %d texts, %v; want 8", asked, err)
	}
	if len(model.documents) != 5 || len(model.queries) != 3 || !slices.Contains(model.queries, "Who approves the holiday rota?") {
		t.Errorf("the model embedded %d documents and the queries %q", len(model.documents), model.queries)
	}
	// A document is embedded as ingestion embeds it: its title, then its summary.
	if !slices.Contains(model.documents, "Holiday rota\n\nLists who is away in August.") {
		t.Errorf("the documents embedded were %q", model.documents)
	}
	if lacking, _ := vectors.Lacking(ctx, set); lacking != 0 {
		t.Errorf("after recording, %d texts are still lacking", lacking)
	}

	// Nothing more to ask a second time, and the file is enough to load the
	// set and embed its questions with no model at all.
	if asked, err := vectors.Record(ctx, set, model); err != nil || asked != 0 {
		t.Errorf("a second Record asked for %d texts, %v", asked, err)
	}
	if err := vectors.Save(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := OpenVectors(path)
	if err := Fill(ctx, set, memory.New(), memory.NewLedger(), reopened.Embedder()); err != nil {
		t.Errorf("loading the set from the vectors on file: %v", err)
	}

	// A question reworded is a text the file does not have.
	set.Questions[0].Text = "Why is the checkout slow?"
	if lacking, _ := reopened.Lacking(ctx, set); lacking != 1 {
		t.Errorf("after rewording one question, %d texts are lacking, want 1", lacking)
	}
}

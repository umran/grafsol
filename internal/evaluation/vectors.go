package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/umran/grafsol/model"
	"github.com/umran/grafsol/storage/memory"
)

// Vectors holds the embeddings of a test set's documents and questions, in a
// file. It is a tape for a model.Embedder: Recording asks a real embedder and
// keeps what it answers, and Embedder answers from what was kept.
//
// The point is to hold still everything about a search that is not being
// tuned. With the vectors on file, a search over the test set starts from
// the same documents every time, on any machine, with no model service
// running, and goes on doing so when the embedding model is replaced.
//
// A Vectors is safe for concurrent use.
type Vectors struct {
	path string

	mu    sync.Mutex
	space string               // the embedding space every vector is in
	kept  map[string][]float32 // by key: see keyOf
	asked map[string]bool      // the keys asked for since the file was opened
}

// ErrNoVector reports that a text was to be embedded whose vector is not on
// file. The remedy is to record it: `grafsol-eval embed`.
var ErrNoVector = errors.New("evaluation: no vector on file")

// The shape of the file. A vector is its numbers as 32-bit floats, least
// significant byte first, in base64: exact, and a fifth of the size of the
// same numbers written out in decimal.
type vectorsRecord struct {
	Space   string            `json:"space"`
	Vectors map[string]string `json:"vectors"`
}

// OpenVectors reads the vectors in a file. A file that does not exist yet is
// an empty Vectors, which Save will create.
func OpenVectors(path string) (*Vectors, error) {
	v := &Vectors{path: path, kept: make(map[string][]float32), asked: make(map[string]bool)}

	content, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return v, nil
	}
	if err != nil {
		return nil, fmt.Errorf("evaluation: %w", err)
	}
	var record vectorsRecord
	if err := json.Unmarshal(content, &record); err != nil {
		return nil, fmt.Errorf("evaluation: %s: %w", filepath.Base(path), err)
	}
	if len(record.Vectors) > 0 && record.Space == "" {
		return nil, fmt.Errorf("evaluation: %s holds vectors and does not say what embedding space they are in", filepath.Base(path))
	}

	v.space = record.Space
	for key, encoded := range record.Vectors {
		vector, err := decodeVector(encoded)
		if err != nil {
			return nil, fmt.Errorf("evaluation: %s: the vector %s: %w", filepath.Base(path), key, err)
		}
		v.kept[key] = vector
	}
	return v, nil
}

// Space returns the name of the embedding space the vectors are in, or ""
// if there are none yet.
func (v *Vectors) Space() string {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.space
}

// Len returns how many vectors are held.
func (v *Vectors) Len() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.kept)
}

// Embedder returns an embedder that answers from the vectors on file and
// never asks a model. A text whose vector is not on file is an error that
// wraps ErrNoVector.
func (v *Vectors) Embedder() model.Embedder {
	return &kept{vectors: v}
}

// Recording returns an embedder that answers from the vectors on file where
// it can, asks from for the rest, and keeps what from answers. Call Save
// afterwards to write the file.
//
// If the vectors on file were made in another embedding space than from
// works in, they are thrown away: vectors from two spaces cannot be
// compared, so none of the old ones can be served beside a new one.
func (v *Vectors) Recording(from model.Embedder) model.Embedder {
	return &kept{vectors: v, from: from}
}

// Prune drops every vector that has not been asked for since the file was
// opened, and returns how many it dropped. After everything in a test set
// has been embedded, what was not asked for belongs to a question or a
// summary that has since been reworded.
func (v *Vectors) Prune() (dropped int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for key := range v.kept {
		if !v.asked[key] {
			delete(v.kept, key)
			dropped++
		}
	}
	return dropped
}

// Save writes the vectors to the file they were opened from.
func (v *Vectors) Save() error {
	v.mu.Lock()
	record := vectorsRecord{Space: v.space, Vectors: make(map[string]string, len(v.kept))}
	for key, vector := range v.kept {
		record.Vectors[key] = encodeVector(vector)
	}
	v.mu.Unlock()

	// One vector to a line, in the order of their keys, so that the file
	// changes only where a vector did.
	encoded, err := json.MarshalIndent(record, "", " ")
	if err != nil {
		return fmt.Errorf("evaluation: encoding the vectors: %w", err)
	}
	return writeFile(v.path, append(encoded, '\n'))
}

// Record embeds everything a search over the test set needs: every document
// and topic of its graph, by the rule ingestion uses, and every question. It
// asks from only for what is not on file, and returns how many texts it had
// to ask for. Call Prune and Save afterwards.
func (v *Vectors) Record(ctx context.Context, set Set, from model.Embedder) (asked int, err error) {
	counter := &countingModel{inner: from}
	if err := v.embedSet(ctx, set, v.Recording(counter)); err != nil {
		return counter.texts, err
	}
	return counter.texts, nil
}

// Lacking returns how many of the texts a search over the test set needs
// have no vector on file. It asks no model and changes nothing.
func (v *Vectors) Lacking(ctx context.Context, set Set) (int, error) {
	probe := &probe{vectors: v}
	if err := v.embedSet(ctx, set, probe); err != nil {
		return 0, err
	}
	return probe.lacking, nil
}

// embedSet has every text of a test set embedded by embedder: the graph is
// loaded into a store that is thrown away, which embeds its documents and
// topics exactly as Fill will when it counts, and then the questions are
// embedded as a search will embed them.
func (v *Vectors) embedSet(ctx context.Context, set Set, embedder model.Embedder) error {
	if err := Fill(ctx, set, memory.New(), memory.NewLedger(), embedder); err != nil {
		return err
	}
	questions := make([]string, len(set.Questions))
	for i, q := range set.Questions {
		questions[i] = q.Text
	}
	if len(questions) == 0 {
		return nil
	}
	_, err := embedder.EmbedQueries(ctx, questions)
	return err
}

// countingModel passes everything on to another embedder and counts the
// texts it is asked to embed.
type countingModel struct {
	inner model.Embedder

	mu    sync.Mutex
	texts int
}

func (c *countingModel) Space(ctx context.Context) (string, error) { return c.inner.Space(ctx) }

func (c *countingModel) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	c.count(len(texts))
	return c.inner.EmbedDocuments(ctx, texts)
}

func (c *countingModel) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	c.count(len(texts))
	return c.inner.EmbedQueries(ctx, texts)
}

func (c *countingModel) count(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.texts += n
}

// probe is an embedder that embeds nothing: it counts the texts whose
// vectors are not on file, and answers every text with a vector that means
// nothing.
type probe struct {
	vectors *Vectors

	mu      sync.Mutex
	lacking int
}

func (p *probe) Space(context.Context) (string, error) { return "none: only counting", nil }

func (p *probe) EmbedDocuments(_ context.Context, texts []string) ([][]float32, error) {
	return p.count("document", texts), nil
}

func (p *probe) EmbedQueries(_ context.Context, texts []string) ([][]float32, error) {
	return p.count("query", texts), nil
}

func (p *probe) count(kind string, texts []string) [][]float32 {
	p.vectors.mu.Lock()
	defer p.vectors.mu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	blanks := make([][]float32, len(texts))
	for i, text := range texts {
		if _, have := p.vectors.kept[keyOf(kind, text)]; !have {
			p.lacking++
		}
		blanks[i] = []float32{1}
	}
	return blanks
}

// kept is the embedder that Embedder and Recording return. With no from it
// only serves.
type kept struct {
	vectors *Vectors
	from    model.Embedder
}

var _ model.Embedder = (*kept)(nil)

func (k *kept) Space(ctx context.Context) (string, error) {
	v := k.vectors
	if k.from == nil {
		if space := v.Space(); space != "" {
			return space, nil
		}
		return "", fmt.Errorf("%w: the file is empty and names no embedding space", ErrNoVector)
	}

	space, err := k.from.Space(ctx)
	if err != nil {
		return "", err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.space != space {
		// Another model, or the first one: nothing kept can stay.
		clear(v.kept)
		clear(v.asked)
		v.space = space
	}
	return space, nil
}

func (k *kept) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return k.embed(ctx, "document", texts)
}

func (k *kept) EmbedQueries(ctx context.Context, texts []string) ([][]float32, error) {
	return k.embed(ctx, "query", texts)
}

// embed returns the vector of each text, embedded as the kind says: as a
// document or as a query. The two are different vectors of the same text,
// and are kept apart.
func (k *kept) embed(ctx context.Context, kind string, texts []string) ([][]float32, error) {
	v := k.vectors
	// Settle which space this is before looking anything up: a recording
	// that finds another model's vectors on file starts afresh.
	if _, err := k.Space(ctx); err != nil {
		return nil, err
	}

	keys := make([]string, len(texts))
	var missing []string
	v.mu.Lock()
	for i, text := range texts {
		keys[i] = keyOf(kind, text)
		v.asked[keys[i]] = true
		if _, have := v.kept[keys[i]]; !have {
			missing = append(missing, text)
		}
	}
	v.mu.Unlock()

	if len(missing) > 0 {
		if k.from == nil {
			return nil, fmt.Errorf("%w for %d of %d texts to embed as a %s", ErrNoVector, len(missing), len(texts), kind)
		}
		ask := k.from.EmbedDocuments
		if kind == "query" {
			ask = k.from.EmbedQueries
		}
		made, err := ask(ctx, missing)
		if err != nil {
			return nil, err
		}
		if len(made) != len(missing) {
			return nil, fmt.Errorf("evaluation: the embedder returned %d vectors for %d texts", len(made), len(missing))
		}
		v.mu.Lock()
		for i, text := range missing {
			v.kept[keyOf(kind, text)] = made[i]
		}
		v.mu.Unlock()
	}

	vectors := make([][]float32, len(texts))
	v.mu.Lock()
	defer v.mu.Unlock()
	for i, key := range keys {
		vectors[i] = v.kept[key]
	}
	return vectors, nil
}

// keyOf is what a vector is filed under: a fingerprint of the text and of
// which way it was embedded.
func keyOf(kind, text string) string {
	sum := sha256.Sum256([]byte(kind + "\x00" + text))
	return hex.EncodeToString(sum[:16])
}

func encodeVector(vector []float32) string {
	raw := make([]byte, 4*len(vector))
	for i, x := range vector {
		binary.LittleEndian.PutUint32(raw[4*i:], math.Float32bits(x))
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func decodeVector(encoded string) ([]float32, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || len(raw)%4 != 0 {
		return nil, fmt.Errorf("it is %d bytes long, which is not a whole number of 32-bit floats", len(raw))
	}
	vector := make([]float32, len(raw)/4)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*i:]))
	}
	return vector, nil
}

// writeFile writes a file in one step, creating its folder if need be: the
// content goes to a temporary file beside it, which then takes its name. A
// reader never sees half a file, and a crash never leaves one.
func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("evaluation: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("evaluation: %w", err)
	}
	defer os.Remove(temporary.Name()) // does nothing once the rename has happened

	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return fmt.Errorf("evaluation: writing %s: %w", path, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("evaluation: writing %s: %w", path, err)
	}
	if err := os.Chmod(temporary.Name(), 0o644); err != nil {
		return fmt.Errorf("evaluation: %w", err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("evaluation: %w", err)
	}
	return nil
}

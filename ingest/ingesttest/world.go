// Package ingesttest checks the central promise of package ingest against
// any storage backend: that the graph converges on what the source holds,
// however badly the source's changes are delivered.
//
// World is a pretend source system that a test mutates at random. Converge
// delivers the World's changes to a Syncer as unreliably as it can, lets
// the Syncer reconcile, and then compares the graph with the World.
package ingesttest

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"math/rand/v2"
	"slices"
	"sync"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/source"
)

// World is a pretend source system: a set of documents that changes at
// random, remembers every change it ever reported, and can list what exists
// now. It implements source.Scanner. A World is safe for concurrent use.
type World struct {
	// Restores makes Mutate now and then bring a removed document back just
	// as it was, without reporting a change: the source has the document
	// again and tells no one, and the version it has is older than that of
	// its removal. Only a scan can find such a document. Set Restores before
	// the first call to Mutate.
	Restores bool

	name string

	mu      sync.Mutex
	random  *rand.Rand
	clock   uint64                     // the version given to the latest change
	created int                        // how many documents have ever been created
	ever    []graph.ID                 // every document ever created, in order
	live    map[graph.ID]source.Change // the documents that exist now, as last changed
	removed map[graph.ID]source.Change // the documents that do not, as they were when removed
	history []source.Change            // every change reported, in the order it was made
}

var _ source.Scanner = (*World)(nil)

// NewWorld returns an empty world. Two worlds made with the same name and
// seed go through exactly the same history.
func NewWorld(name string, seed uint64) *World {
	return &World{
		name:    name,
		random:  rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)),
		live:    make(map[graph.ID]source.Change),
		removed: make(map[graph.ID]source.Change),
	}
}

// Name implements source.Source.
func (w *World) Name() string { return w.name }

// Resolve implements source.Source. The world's documents link to each
// other directly, so there are no URLs to recognize.
func (w *World) Resolve(string) (graph.ID, bool) { return "", false }

// Mutate makes the given number of random changes: creating documents,
// rewriting them and their edges, removing them and, if Restores is set,
// bringing removed ones back.
func (w *World) Mutate(changes int) {
	w.mu.Lock()
	defer w.mu.Unlock()

	for range changes {
		roll := w.random.Float64()
		switch {
		case w.Restores && roll >= 0.95 && len(w.removed) > 0:
			// Back just as it was, and nothing is reported.
			id := anyOf(w.random, w.removed)
			w.live[id] = w.removed[id]
			delete(w.removed, id)
		case len(w.live) == 0 || roll < 0.35:
			w.created++
			id := graph.DocumentID(w.name, fmt.Sprintf("d%04d", w.created))
			w.ever = append(w.ever, id)
			w.record(w.describe(id))
		case roll < 0.80:
			w.record(w.describe(anyOf(w.random, w.live)))
		default:
			w.clock++
			id := anyOf(w.random, w.live)
			w.record(source.Change{
				Document: graph.Document{ID: id, Source: w.name},
				Version:  w.clock,
				Deleted:  true,
			})
		}
	}
}

// describe invents a new state for a document: fresh text and a fresh set
// of edges, which may have nothing in common with the ones it had before.
func (w *World) describe(id graph.ID) source.Change {
	w.clock++
	change := source.Change{
		Document: graph.Document{
			ID:        id,
			Type:      graph.TypeIssue,
			Source:    w.name,
			URL:       "https://world.test/" + id.Key(),
			Title:     fmt.Sprintf("%s at revision %d", id.Key(), w.clock),
			CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(w.clock) * time.Second),
		},
		Version: w.clock,
		Content: fmt.Sprintf("text number %d", w.random.IntN(1_000_000)),
	}

	// Links to a few other documents, living or not: an edge may well point
	// at a document that no longer exists.
	others := slices.DeleteFunc(slices.Clone(w.ever), func(other graph.ID) bool { return other == id })
	w.random.Shuffle(len(others), func(i, j int) { others[i], others[j] = others[j], others[i] })
	links := min(len(others), w.random.IntN(4))
	for _, other := range others[:links] {
		change.LinkTo(other, fmt.Sprintf("revision %d mentions %s", w.clock, other.Key()))
	}
	if len(others) > links && w.random.IntN(3) == 0 {
		change.ChildOf(others[links], "part of")
	}
	for _, tag := range []string{"bug", "billing", "docs", "urgent"} {
		if w.random.IntN(4) == 0 {
			change.Tag(tag, "")
		}
	}
	return change
}

// anyOf picks one of the given documents.
func anyOf(random *rand.Rand, documents map[graph.ID]source.Change) graph.ID {
	ids := slices.Sorted(mapKeys(documents)) // sorted, so that the pick depends on the seed alone
	return ids[random.IntN(len(ids))]
}

func (w *World) record(change source.Change) {
	id := change.Document.ID
	w.history = append(w.history, change)
	if change.Deleted {
		w.removed[id] = w.live[id]
		delete(w.live, id)
	} else {
		w.live[id] = change
	}
}

// History returns every change the world has reported, oldest first. A
// document brought back by Restores is not among them.
func (w *World) History() []source.Change {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.history)
}

// Live returns the documents that exist now, each as its latest change.
func (w *World) Live() map[graph.ID]source.Change {
	w.mu.Lock()
	defer w.mu.Unlock()
	live := make(map[graph.ID]source.Change, len(w.live))
	for id, change := range w.live {
		live[id] = change
	}
	return live
}

// Ever returns the ID of every document the world has ever had.
func (w *World) Ever() []graph.ID {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.ever)
}

// Scan implements source.Scanner. It lists the documents that exist at the
// moment it is called, in ID order; the checkpoint of each is its ID.
func (w *World) Scan(ctx context.Context, resume source.Checkpoint) iter.Seq2[source.Change, error] {
	listing := slices.SortedFunc(mapValues(w.Live()), func(a, b source.Change) int {
		return cmp.Compare(a.Document.ID, b.Document.ID)
	})
	return func(yield func(source.Change, error) bool) {
		for _, change := range listing {
			if err := ctx.Err(); err != nil {
				yield(source.Change{}, err)
				return
			}
			if string(change.Document.ID) <= string(resume) {
				continue // listed before the scan was interrupted
			}
			change.Checkpoint = source.Checkpoint(change.Document.ID)
			if !yield(change, nil) {
				return
			}
		}
	}
}

func mapKeys[K comparable, V any](m map[K]V) iter.Seq[K] {
	return func(yield func(K) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

func mapValues[K comparable, V any](m map[K]V) iter.Seq[V] {
	return func(yield func(V) bool) {
		for _, value := range m {
			if !yield(value) {
				return
			}
		}
	}
}

package storagetest

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/storage"
)

// RunLedger checks a Ledger against its contract. open must return a fresh,
// empty ledger every time it is called, set up so that Unseen returns
// pageSize documents per page (a small number keeps the paging tests quick).
func RunLedger(t *testing.T, pageSize int, open func(t *testing.T) storage.Ledger) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, ledger storage.Ledger, pageSize int)
	}{
		{"MarkRoundTrip", testMarkRoundTrip},
		{"RecordReplaces", testRecordReplaces},
		{"Clock", testClock},
		{"Unseen", testUnseen},
		{"UnseenIsPaged", testUnseenIsPaged},
		{"UnseenWhileRemoving", testUnseenWhileRemoving},
		{"Checkpoints", testCheckpoints},
		{"LockExcludes", testLockExcludes},
		{"LocksArePerKey", testLocksArePerKey},
		{"LockHonorsContext", testLockHonorsContext},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, open(t), pageSize)
		})
	}
}

func testMarkRoundTrip(t *testing.T, ledger storage.Ledger, _ int) {
	id := graph.ID("doc:github:acme/api#42")

	if _, found, err := ledger.Mark(t.Context(), id); err != nil || found {
		t.Fatalf("Mark on an empty ledger: found %v, err %v", found, err)
	}

	// The largest version there is must survive the trip.
	want := storage.Mark{ID: id, Source: "github", Version: ^uint64(0), Fingerprint: "abc123"}
	record(t, ledger, want)

	got := mark(t, ledger, id)
	if got.Seen == 0 {
		t.Errorf("Record did not stamp Seen")
	}
	got.Seen = 0
	if got != want {
		t.Errorf("got mark %+v, want %+v", got, want)
	}
}

func testRecordReplaces(t *testing.T, ledger storage.Ledger, _ int) {
	id := graph.ID("doc:github:acme/api#42")
	record(t, ledger, storage.Mark{ID: id, Source: "github", Version: 1, Fingerprint: "first"})
	first := mark(t, ledger, id)

	// The Seen handed to Record is ignored: the ledger reads its own clock,
	// which has not moved since the first Record.
	record(t, ledger, storage.Mark{ID: id, Source: "github", Version: 2, Deleted: true, Seen: 1 << 40})
	second := mark(t, ledger, id)

	if second.Version != 2 || !second.Deleted || second.Fingerprint != "" {
		t.Errorf("the second Record did not replace the first: %+v", second)
	}
	if second.Seen != first.Seen {
		t.Errorf("Seen went from %d to %d between two Records with no Tick in between", first.Seen, second.Seen)
	}
}

// testClock checks what the clock is for: telling the marks recorded before
// a Tick from the marks recorded after it.
func testClock(t *testing.T, ledger storage.Ledger, _ int) {
	early, late := graph.ID("doc:s:early"), graph.ID("doc:s:late")
	seen := func(id graph.ID) uint64 {
		t.Helper()
		return mark(t, ledger, id).Seen
	}

	record(t, ledger, storage.Mark{ID: early, Source: "s", Version: 1})
	first := tick(t, ledger)
	if seen(early) >= first {
		t.Errorf("a mark recorded before a tick has Seen %d, which is not below the tick's reading of %d", seen(early), first)
	}

	// Record reads the clock without moving it, so every mark recorded
	// between two ticks carries the reading of the first.
	record(t, ledger, storage.Mark{ID: late, Source: "s", Version: 1})
	record(t, ledger, storage.Mark{ID: early, Source: "s", Version: 2})
	if seen(late) != first || seen(early) != first {
		t.Errorf("marks recorded while the clock read %d have Seen %d and %d", first, seen(late), seen(early))
	}

	// Tick moves the clock and nothing else: a mark keeps the reading it
	// was recorded at until it is recorded again.
	second := tick(t, ledger)
	if second <= first {
		t.Errorf("Tick took the clock from %d to %d", first, second)
	}
	if seen(late) != first {
		t.Errorf("a tick changed the Seen of a mark from %d to %d", first, seen(late))
	}

	// A mark recorded again is stamped again, even when nothing else about
	// it has changed. That is how a scan notes that it came across a
	// document which is the same as ever.
	record(t, ledger, storage.Mark{ID: late, Source: "s", Version: 1})
	if seen(late) != second {
		t.Errorf("a mark recorded again, unchanged, while the clock read %d has Seen %d", second, seen(late))
	}

	// Ticks from several callers at once: each must return a reading above
	// the last, and none may run ahead of the clock that Record reads.
	const callers = 16
	readings := make([]uint64, callers)
	var group sync.WaitGroup
	for i := range callers {
		group.Go(func() {
			reading, err := ledger.Tick(t.Context())
			if err != nil {
				t.Errorf("Tick: %v", err)
			}
			readings[i] = reading
		})
	}
	group.Wait()
	record(t, ledger, storage.Mark{ID: late, Source: "s", Version: 1})
	afterwards := seen(late)
	for _, reading := range readings {
		if reading <= second {
			t.Errorf("a later Tick returned %d, which is not above the earlier %d", reading, second)
		}
		if afterwards < reading {
			t.Errorf("a mark recorded after Tick returned %d has Seen %d", reading, afterwards)
		}
	}
}

func testUnseen(t *testing.T, ledger storage.Ledger, _ int) {
	record(t, ledger, storage.Mark{ID: "doc:github:a", Source: "github", Version: 1})
	record(t, ledger, storage.Mark{ID: "doc:github:b", Source: "github", Version: 1})
	record(t, ledger, storage.Mark{ID: "doc:github:gone", Source: "github", Version: 1, Deleted: true})
	record(t, ledger, storage.Mark{ID: "doc:jira:e", Source: "jira", Version: 1})
	watermark := tick(t, ledger)
	record(t, ledger, storage.Mark{ID: "doc:github:d", Source: "github", Version: 1})

	requireIDs(t, unseen(t, ledger, "github", watermark), "doc:github:a", "doc:github:b")
	requireIDs(t, unseen(t, ledger, "jira", watermark), "doc:jira:e")
	requireIDs(t, unseen(t, ledger, "notion", watermark))
	requireIDs(t, unseen(t, ledger, "github", 0))

	// Recording a document again brings it up to date.
	record(t, ledger, storage.Mark{ID: "doc:github:a", Source: "github", Version: 1})
	requireIDs(t, unseen(t, ledger, "github", watermark), "doc:github:b")

	// Everything recorded so far predates a later watermark; tombstones
	// still never appear.
	requireIDs(t, unseen(t, ledger, "github", tick(t, ledger)), "doc:github:a", "doc:github:b", "doc:github:d")
}

func testUnseenIsPaged(t *testing.T, ledger storage.Ledger, pageSize int) {
	var want []graph.ID
	for i := range 2*pageSize + 1 {
		id := graph.DocumentID("github", fmt.Sprintf("acme/api#%03d", i))
		record(t, ledger, storage.Mark{ID: id, Source: "github", Version: 1})
		want = append(want, id)
	}
	watermark := tick(t, ledger)

	var got []graph.ID
	cursor := ""
	for pages := 1; ; pages++ {
		ids, next, err := ledger.Unseen(t.Context(), "github", watermark, cursor)
		if err != nil {
			t.Fatalf("Unseen: %v", err)
		}
		if len(ids) > pageSize {
			t.Fatalf("page %d holds %d documents, more than the page size of %d", pages, len(ids), pageSize)
		}
		got = append(got, ids...)
		if next == "" {
			break
		}
		if len(ids) == 0 || pages > 10*len(want) {
			t.Fatalf("Unseen is not making progress (page %d)", pages)
		}
		cursor = next
	}
	requireIDs(t, got, want...)

	if _, _, err := ledger.Unseen(t.Context(), "github", watermark, "!!! not a cursor !!!"); err == nil {
		t.Errorf("Unseen accepted a malformed cursor")
	}
}

// testUnseenWhileRemoving pages through Unseen the way a sweep does: turning
// each document into a tombstone as it goes. Every document must still be
// visited exactly once.
func testUnseenWhileRemoving(t *testing.T, ledger storage.Ledger, pageSize int) {
	var want []graph.ID
	for i := range 3*pageSize + 2 {
		id := graph.DocumentID("github", fmt.Sprintf("acme/api#%03d", i))
		record(t, ledger, storage.Mark{ID: id, Source: "github", Version: 1})
		want = append(want, id)
	}
	watermark := tick(t, ledger)

	var got []graph.ID
	cursor := ""
	for {
		ids, next, err := ledger.Unseen(t.Context(), "github", watermark, cursor)
		if err != nil {
			t.Fatalf("Unseen: %v", err)
		}
		for _, id := range ids {
			record(t, ledger, storage.Mark{ID: id, Source: "github", Version: 1, Deleted: true})
		}
		got = append(got, ids...)
		if next == "" {
			break
		}
		cursor = next
	}
	requireIDs(t, got, want...)
	requireIDs(t, unseen(t, ledger, "github", tick(t, ledger)))
}

func testCheckpoints(t *testing.T, ledger storage.Ledger, _ int) {
	load := func(name string) []byte {
		t.Helper()
		checkpoint, err := ledger.Checkpoint(t.Context(), name)
		if err != nil {
			t.Fatalf("Checkpoint(%q): %v", name, err)
		}
		return checkpoint
	}
	save := func(name string, checkpoint []byte) {
		t.Helper()
		if err := ledger.SaveCheckpoint(t.Context(), name, checkpoint); err != nil {
			t.Fatalf("SaveCheckpoint(%q): %v", name, err)
		}
	}

	if got := load("github/feed"); got != nil {
		t.Errorf("a checkpoint that was never saved came back as %q", got)
	}

	// Checkpoints are opaque bytes: nothing about them may be assumed.
	binary := []byte{0x00, 0xff, 0x10, '\n', '\'', '"'}
	save("github/feed", binary)
	save("github/scan", []byte(`{"page":7}`))
	if got := load("github/feed"); !bytes.Equal(got, binary) {
		t.Errorf("got checkpoint %q, want %q", got, binary)
	}
	if got := load("github/scan"); string(got) != `{"page":7}` {
		t.Errorf("got checkpoint %q", got)
	}

	save("github/feed", []byte("replaced"))
	if got := load("github/feed"); string(got) != "replaced" {
		t.Errorf("saving again did not replace the checkpoint: %q", got)
	}

	// An empty checkpoint is a saved checkpoint, not a missing one.
	save("github/feed", nil)
	if got := load("github/feed"); got == nil || len(got) != 0 {
		t.Errorf("an empty checkpoint came back as %#v, want empty but not nil", got)
	}
}

func testLockExcludes(t *testing.T, ledger storage.Ledger, _ int) {
	// Each worker reads the counter, waits a moment, and writes back the
	// value plus one. Without mutual exclusion, updates get lost.
	const workers, rounds = 8, 5
	var counter atomic.Int64
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for range rounds {
				unlock, err := ledger.Lock(t.Context(), "doc:s:shared")
				if err != nil {
					t.Errorf("Lock: %v", err)
					return
				}
				value := counter.Load()
				time.Sleep(time.Millisecond)
				counter.Store(value + 1)
				unlock()
			}
		})
	}
	group.Wait()
	if got := counter.Load(); got != workers*rounds {
		t.Errorf("counter reached %d, want %d: the lock let two holders in at once", got, workers*rounds)
	}
}

func testLocksArePerKey(t *testing.T, ledger storage.Ledger, _ int) {
	unlockA, err := ledger.Lock(t.Context(), "doc:s:a")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlockA()

	// Holding one key must not stand in the way of another.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	unlockB, err := ledger.Lock(ctx, "doc:s:b")
	if err != nil {
		t.Fatalf("locking a second key while holding the first: %v", err)
	}
	unlockB()
	unlockB() // releasing twice is harmless
}

func testLockHonorsContext(t *testing.T, ledger storage.Ledger, _ int) {
	unlock, err := ledger.Lock(t.Context(), "doc:s:a")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	// While the lock is held, a second caller gives up when its context does.
	impatient, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if stolen, err := ledger.Lock(impatient, "doc:s:a"); err == nil {
		stolen()
		t.Fatalf("a second caller got a lock that was still held")
	}

	// Once released, the lock can be taken again.
	unlock()
	patient, cancelPatient := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelPatient()
	again, err := ledger.Lock(patient, "doc:s:a")
	if err != nil {
		t.Fatalf("locking again after release: %v", err)
	}
	again()
}

// --- helpers ----------------------------------------------------------------

func record(t *testing.T, ledger storage.Ledger, m storage.Mark) {
	t.Helper()
	if err := ledger.Record(t.Context(), m); err != nil {
		t.Fatalf("Record(%+v): %v", m, err)
	}
}

func mark(t *testing.T, ledger storage.Ledger, id graph.ID) storage.Mark {
	t.Helper()
	m, found, err := ledger.Mark(t.Context(), id)
	if err != nil {
		t.Fatalf("Mark(%s): %v", id, err)
	}
	if !found {
		t.Fatalf("Mark(%s): nothing recorded", id)
	}
	return m
}

func tick(t *testing.T, ledger storage.Ledger) uint64 {
	t.Helper()
	value, err := ledger.Tick(t.Context())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	return value
}

// unseen follows an Unseen listing to its end.
func unseen(t *testing.T, ledger storage.Ledger, source string, before uint64) []graph.ID {
	t.Helper()
	var all []graph.ID
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 100_000 {
			t.Fatalf("Unseen(%q) never reached a last page", source)
		}
		ids, next, err := ledger.Unseen(t.Context(), source, before, cursor)
		if err != nil {
			t.Fatalf("Unseen(%q): %v", source, err)
		}
		all = append(all, ids...)
		if next == "" {
			return all
		}
		cursor = next
	}
}

// requireIDs fails the test unless got holds exactly the wanted IDs, in any
// order and without repeats.
func requireIDs(t *testing.T, got []graph.ID, want ...graph.ID) {
	t.Helper()
	sortedGot, sortedWant := slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))
	if !slices.Equal(sortedGot, sortedWant) {
		t.Fatalf("got IDs %v, want %v", sortedGot, sortedWant)
	}
}

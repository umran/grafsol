package ingesttest

import (
	"context"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/mapping"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/storage"
)

// Options size a convergence run.
type Options struct {
	Rounds  int // rounds of "change the world, deliver badly"; zero means 3
	Changes int // changes made to the world per round; zero means 150
	Workers int // goroutines applying changes at the same time; zero means 8
}

// Converge puts a Syncer through the worst a source can do and fails the
// test unless the graph comes out right.
//
// Each round it changes a World at random and delivers those changes with
// some dropped, some repeated, all shuffled, and several applied at once.
// Some removed documents come back without any change being reported at
// all. Between rounds it sometimes reconciles, so that scans and feeds
// overlap. After the last round it reconciles twice, which is what the
// Syncer says it needs, and then requires the graph to mirror the World
// exactly.
func Converge(t *testing.T, store storage.Store, ledger storage.Ledger, seed uint64, opts Options) {
	t.Helper()
	rounds, changes, workers := orDefault(opts.Rounds, 3), orDefault(opts.Changes, 150), orDefault(opts.Workers, 8)

	world := NewWorld("world", seed)
	world.Restores = true
	random := rand.New(rand.NewPCG(seed, 99))
	syncer := ingest.NewSyncer(ingest.Config{Store: store, Ledger: ledger, Sources: []source.Source{world}})
	ctx := t.Context()

	delivered := 0
	for round := range rounds {
		world.Mutate(changes)
		history := world.History()
		deliveries := mangle(random, history[delivered:])
		delivered = len(history)
		deliver(t, ctx, syncer, deliveries, workers)

		if round%2 == 0 && round < rounds-1 {
			if _, err := syncer.Reconcile(ctx, world); err != nil {
				t.Fatalf("seed %d: Reconcile between rounds: %v", seed, err)
			}
		}
	}

	for pass := range 2 {
		if _, err := syncer.Reconcile(ctx, world); err != nil {
			t.Fatalf("seed %d: final Reconcile pass %d: %v", seed, pass+1, err)
		}
	}
	RequireMirror(t, store, world)

	// With the graph in step, a further pass must find nothing to do.
	report, err := syncer.Reconcile(ctx, world)
	if err != nil {
		t.Fatalf("seed %d: Reconcile of a graph already in step: %v", seed, err)
	}
	if report.Applied != 0 || report.Removed != 0 {
		t.Errorf("seed %d: a graph already in step was changed again: %+v", seed, report)
	}
}

// ConvergeAsRulesChange is Converge with a team that keeps changing its
// mind. While each round's changes are being delivered, the settings the
// graph is made by are saved anew (other rules, other topics merged) and
// caught up with, so that documents are being applied under one set of
// rules while others are composed again under the next.
//
// When it is all over, and the graph has been caught up with the settings
// that were saved last, it must mirror the World as those settings make it:
// no edge left over from any earlier set of rules.
//
// store has to keep the graph's settings too, as the stores in this module
// do.
func ConvergeAsRulesChange(t *testing.T, store storage.Store, ledger storage.Ledger, seed uint64, opts Options) {
	t.Helper()
	rounds, changes, workers := orDefault(opts.Rounds, 4), orDefault(opts.Changes, 150), orDefault(opts.Workers, 8)
	settings, keepsThem := store.(storage.Settings)
	if !keepsThem {
		t.Fatalf("%T does not keep the graph's settings", store)
	}

	world := NewWorld("world", seed)
	world.Restores = true
	random := rand.New(rand.NewPCG(seed, 31))
	syncer := ingest.NewSyncer(ingest.Config{Store: store, Ledger: ledger, Sources: []source.Source{world}})
	composer := syncer.Composer()
	ctx := t.Context()

	delivered := 0
	for round := range rounds {
		world.Mutate(changes)
		history := world.History()
		deliveries := mangle(random, history[delivered:])
		delivered = len(history)

		// The rules change while this round's deliveries are being applied.
		// The last round always changes them, so that the run ends under
		// rules other than the ones most of it was made by.
		var mindChanged sync.WaitGroup
		if round == rounds-1 || random.IntN(2) == 0 {
			next := teamSettings(1 + (int(seed)+round)%3)
			mindChanged.Go(func() {
				save(t, settings, next)
				if _, err := composer.CatchUp(ctx); err != nil {
					t.Errorf("seed %d: CatchUp while changes arrive: %v", seed, err)
				}
			})
		}
		deliver(t, ctx, syncer, deliveries, workers)
		mindChanged.Wait()

		if round%2 == 0 && round < rounds-1 {
			if _, err := syncer.Reconcile(ctx, world); err != nil {
				t.Fatalf("seed %d: Reconcile between rounds: %v", seed, err)
			}
		}
	}

	if _, err := composer.CatchUp(ctx); err != nil {
		t.Fatalf("seed %d: the final CatchUp: %v", seed, err)
	}
	for pass := range 2 {
		if _, err := syncer.Reconcile(ctx, world); err != nil {
			t.Fatalf("seed %d: final Reconcile pass %d: %v", seed, pass+1, err)
		}
	}
	RequireMirror(t, store, world)

	// With the graph in step and caught up, neither has anything left to do.
	remade, err := composer.CatchUp(ctx)
	if err != nil || remade != 0 {
		t.Errorf("seed %d: a graph already caught up had %d documents made again (error %v)", seed, remade, err)
	}
	report, err := syncer.Reconcile(ctx, world)
	if err != nil {
		t.Fatalf("seed %d: Reconcile of a graph already in step: %v", seed, err)
	}
	if report.Applied != 0 || report.Removed != 0 {
		t.Errorf("seed %d: a graph already in step was changed again: %+v", seed, report)
	}
}

// teamSettings returns one of a few sets of settings that each make
// something else of what a World says: its mentions, its parents, its
// labels, and the topics those labels name.
func teamSettings(which int) mapping.Settings {
	settings := mapping.DefaultSettings()
	switch which {
	case 1:
		settings.Rules = mapping.Rules{
			{ID: "mentions-resolve", Source: "world", Form: graph.FormMention, After: []string{"mentions"}, Kind: mapping.Resolves},
			{ID: "no-bugs", Form: mapping.FormLabel, Label: "bug", Ignore: true},
		}
	case 2:
		settings.Rules = mapping.Rules{
			{ID: "parents-block", Form: graph.FormParent, Kind: mapping.Blocks},
			{ID: "docs", Form: mapping.FormLabel, Label: "docs", Topic: "Documentation"},
		}
		settings.Aliases = map[graph.ID]graph.ID{graph.TopicID("billing"): graph.TopicID("money")}
	case 3:
		settings.Rules = mapping.Rules{{ID: "quiet", Form: graph.FormMention, Ignore: true}}
		settings.Aliases = map[graph.ID]graph.ID{graph.TopicID("urgent"): graph.TopicID("bug")}
	}
	return settings
}

// save stores settings in place of whatever is stored.
func save(t *testing.T, settings storage.Settings, next mapping.Settings) {
	t.Helper()
	document, err := next.Encode()
	if err != nil {
		t.Errorf("encoding the settings: %v", err)
		return
	}
	_, version, err := settings.Settings(t.Context())
	if err != nil {
		t.Errorf("Settings: %v", err)
		return
	}
	if _, err := settings.SaveSettings(t.Context(), document, version); err != nil {
		t.Errorf("SaveSettings: %v", err)
	}
}

// ConvergeWithoutLoss checks the feed on its own, with no scan to fall back
// on. It changes a World at random and delivers every change at least once,
// some twice, in a random order and from several goroutines at once. Since
// nothing is lost there is nothing for a scan to repair: the graph must
// mirror the World on the strength of Apply alone. This is what tests that
// late and repeated changes are told apart from current ones.
func ConvergeWithoutLoss(t *testing.T, store storage.Store, ledger storage.Ledger, seed uint64, opts Options) {
	t.Helper()
	rounds, changes, workers := orDefault(opts.Rounds, 3), orDefault(opts.Changes, 150), orDefault(opts.Workers, 8)

	world := NewWorld("world", seed)
	random := rand.New(rand.NewPCG(seed, 7))
	syncer := ingest.NewSyncer(ingest.Config{Store: store, Ledger: ledger, Sources: []source.Source{world}})

	world.Mutate(rounds * changes)
	var deliveries []source.Change
	for _, change := range world.History() {
		deliveries = append(deliveries, change)
		if random.IntN(3) == 0 {
			deliveries = append(deliveries, change)
		}
	}
	random.Shuffle(len(deliveries), func(i, j int) {
		deliveries[i], deliveries[j] = deliveries[j], deliveries[i]
	})

	deliver(t, t.Context(), syncer, deliveries, workers)
	RequireMirror(t, store, world)
}

func orDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
}

// mangle returns changes the way an unreliable feed delivers them: one in
// five lost, one in five delivered twice, and all of them out of order.
func mangle(random *rand.Rand, changes []source.Change) []source.Change {
	var deliveries []source.Change
	for _, change := range changes {
		switch roll := random.Float64(); {
		case roll < 0.2:
			// lost
		case roll < 0.4:
			deliveries = append(deliveries, change, change)
		default:
			deliveries = append(deliveries, change)
		}
	}
	random.Shuffle(len(deliveries), func(i, j int) {
		deliveries[i], deliveries[j] = deliveries[j], deliveries[i]
	})
	return deliveries
}

// deliver applies the changes from several goroutines at once.
func deliver(t *testing.T, ctx context.Context, syncer *ingest.Syncer, changes []source.Change, workers int) {
	t.Helper()
	queue := make(chan source.Change)
	var group sync.WaitGroup
	for range workers {
		group.Go(func() {
			for change := range queue {
				if _, err := syncer.Apply(ctx, change); err != nil {
					t.Errorf("Apply(%s at version %d): %v", change.Document.ID, change.Version, err)
				}
			}
		})
	}
	for _, change := range changes {
		queue <- change
	}
	close(queue)
	group.Wait()
	if t.Failed() {
		t.FailNow()
	}
}

// RequireMirror fails the test unless the graph holds exactly what the
// world holds: every living document with its fields and precisely the
// edges that the graph's settings make of what the world says of it, every
// topic those edges point at, and no trace of the documents that have been
// removed.
func RequireMirror(t *testing.T, store storage.Store, world *World) {
	t.Helper()
	ctx := t.Context()
	live := world.Live()
	ever := world.Ever()
	settings := settingsOf(t, store)

	stored, err := store.GetVertices(ctx, ever)
	if err != nil {
		t.Fatalf("GetVertices: %v", err)
	}

	problems := 0
	problem := func(format string, args ...any) {
		t.Helper()
		if problems++; problems <= 20 { // enough to diagnose, not enough to drown in
			t.Errorf(format, args...)
		}
	}

	topics := make(map[graph.ID]bool)
	for _, id := range ever {
		edges := outEdges(t, store, id)
		change, alive := live[id]

		if !alive {
			if _, present := stored[id]; present {
				problem("%s was removed from the world but is still in the graph", id)
			}
			if len(edges) != 0 {
				problem("%s was removed from the world but still has %d edges in the graph", id, len(edges))
			}
			continue
		}

		vertex, present := stored[id]
		if !present {
			problem("%s exists in the world but not in the graph", id)
			continue
		}
		document, isDocument := vertex.(*graph.Document)
		if !isDocument || document.Title != change.Document.Title || document.URL != change.Document.URL ||
			document.Type != change.Document.Type || document.Source != change.Document.Source ||
			!document.UpdatedAt.Equal(change.Document.UpdatedAt) || !document.CreatedAt.Equal(change.Document.CreatedAt) {
			problem("%s differs\n graph: %+v\n world: %+v", id, vertex, change.Document)
		}

		want := edgesUnder(settings, change)
		if !slices.Equal(edges, want) {
			problem("the edges of %s differ\n graph: %v\n world: %v", id, edges, want)
		}
		for _, edge := range want {
			if edge.To.Kind() == graph.KindTopic {
				topics[edge.To] = true
			}
		}
	}

	topicIDs := slices.Collect(mapKeys(topics))
	storedTopics, err := store.GetVertices(ctx, topicIDs)
	if err != nil {
		t.Fatalf("GetVertices: %v", err)
	}
	for _, id := range topicIDs {
		if _, present := storedTopics[id]; !present {
			problem("topic %s is pointed at but was never created", id)
		}
	}

	if problems > 0 {
		t.Fatalf("the graph does not mirror the world: %d problems (%d documents ever, %d living)", problems, len(ever), len(live))
	}
	if len(live) == 0 || len(live) == len(ever) {
		t.Fatalf("the run was too tame to prove anything: %d documents ever, %d living", len(ever), len(live))
	}
}

// settingsOf returns the settings a graph is made by: what its store keeps,
// or the settings every graph begins with if the store keeps none.
func settingsOf(t *testing.T, store storage.Store) mapping.Settings {
	t.Helper()
	keeper, keepsThem := store.(storage.Settings)
	if !keepsThem {
		return mapping.DefaultSettings()
	}
	document, _, err := keeper.Settings(t.Context())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	settings, err := mapping.DecodeSettings(document)
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	return settings
}

// edgesUnder says, apart from the code under test, which edges a document
// should have: those its source stated, and those the settings make of its
// references and labels, with merged topics followed and repeats dropped.
func edgesUnder(settings mapping.Settings, change source.Change) []graph.Edge {
	id := change.Document.ID
	made := settings.Rules.Apply(id, id.Source(), change.References, change.Labels)

	var want []graph.Edge
	for _, edge := range slices.Concat(change.Edges, made.Edges) {
		edge.To = settings.MergedInto(edge.To)
		at := slices.IndexFunc(want, func(kept graph.Edge) bool { return kept.Triple() == edge.Triple() })
		switch {
		case edge.To == id:
			// says nothing
		case at < 0:
			want = append(want, edge)
		case want[at].Summary == "":
			want[at] = edge
		}
	}
	slices.SortFunc(want, graph.Edge.Compare)
	return want
}

// outEdges returns every edge that starts at a vertex, sorted.
func outEdges(t *testing.T, store storage.Store, id graph.ID) []graph.Edge {
	t.Helper()
	var edges []graph.Edge
	query := storage.EdgeQuery{Vertex: id, Direction: graph.Out}
	for {
		page, err := store.Edges(t.Context(), query)
		if err != nil {
			t.Fatalf("Edges(%s): %v", id, err)
		}
		edges = append(edges, page.Edges...)
		if page.Next == "" {
			slices.SortFunc(edges, graph.Edge.Compare)
			return edges
		}
		query.Cursor = page.Next
	}
}

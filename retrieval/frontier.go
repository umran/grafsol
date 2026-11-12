package retrieval

import (
	"cmp"
	"slices"

	"github.com/umran/grafsol/graph"
)

// frontier is everything a search knows of and has not visited. It holds
// three things, which between them account for every lead:
//
//   - unjudged: leads that were found and have not been put to the judge;
//   - worth: leads the judge rated worth following, waiting their turn;
//   - open: visited vertices that have neighbours the search has not read
//     yet, and so leads it has not even found.
//
// A lead the judge declines is in none of them: it is recorded in the trace
// and forgotten.
type frontier struct {
	// known is every vertex that is, or ever was, the target of a lead.
	// Each vertex becomes a lead once: through the first edge that reaches
	// it.
	known map[graph.ID]bool

	unjudged []candidate      // best estimate first
	worth    []candidate      // best rating first
	open     []*neighbourhood // in the order they were opened
}

// candidate is a lead and what the search has learnt about it.
type candidate struct {
	lead Lead

	// given marks an anchor: a vertex the caller told the search to visit,
	// which it does whatever the judge makes of it.
	given bool

	// potential is the judge's rating, once there is one.
	potential float64
}

func (c candidate) target() graph.ID { return c.lead.Target.VertexID() }

// neighbourhood is a visited vertex whose neighbours have not all been read.
type neighbourhood struct {
	vertex graph.Vertex
	cursor string // where the next page of neighbours begins

	// bound is the highest estimate an unread neighbour can have.
	// Neighbours come nearest first, so it is the estimate of the last one
	// read.
	bound float64
}

func newFrontier() *frontier {
	return &frontier{known: make(map[graph.ID]bool)}
}

// add takes in newly found leads, leaving out any whose target is already
// known: visited, or reached before by another edge.
func (f *frontier) add(leads []Lead, given bool) {
	for _, lead := range leads {
		id := lead.Target.VertexID()
		if f.known[id] {
			continue
		}
		f.known[id] = true
		f.unjudged = append(f.unjudged, candidate{lead: lead, given: given})
	}
	// Best estimate first; the ID settles ties, so that the same search
	// always goes the same way.
	slices.SortFunc(f.unjudged, func(a, b candidate) int {
		return cmp.Or(
			cmp.Compare(b.lead.Estimate, a.lead.Estimate),
			cmp.Compare(a.target(), b.target()),
		)
	})
}

// unreadAbove returns the open neighbourhood that should be read next if the
// best leads are to be judged first, or nil if none needs reading yet.
//
// The next batch will be the size best unjudged leads. An open neighbourhood
// matters to it only if one of its unread neighbours could be among them:
// that is, if its bound is above the estimate of the lead that would
// otherwise be last into the batch. When there are fewer than size leads to
// judge, any open neighbourhood may add to them.
func (f *frontier) unreadAbove(size int) *neighbourhood {
	var best *neighbourhood
	for _, hood := range f.open {
		if best == nil || hood.bound > best.bound {
			best = hood
		}
	}
	if best == nil {
		return nil
	}
	if len(f.unjudged) >= size && best.bound <= f.unjudged[size-1].lead.Estimate {
		return nil
	}
	return best
}

// close forgets an open neighbourhood whose neighbours have all been read.
func (f *frontier) close(done *neighbourhood) {
	f.open = slices.DeleteFunc(f.open, func(hood *neighbourhood) bool { return hood == done })
}

// take removes and returns the size best unjudged leads, or all of them if
// there are fewer.
func (f *frontier) take(size int) []candidate {
	size = min(size, len(f.unjudged))
	batch := slices.Clone(f.unjudged[:size])
	f.unjudged = slices.Delete(f.unjudged, 0, size)
	return batch
}

// file records the judge's ratings of a batch. A lead rated at least
// minPotential, and any anchor, joins those worth following. The rest are
// declined.
func (f *frontier) file(batch []candidate, potential []float64, minPotential float64) {
	for i, c := range batch {
		c.potential = potential[i]
		if c.given || c.potential >= minPotential {
			f.worth = append(f.worth, c)
		}
	}
	// Anchors first, then the best rating; estimate and ID settle ties.
	slices.SortFunc(f.worth, func(a, b candidate) int {
		return cmp.Or(
			compareBool(b.given, a.given),
			cmp.Compare(b.potential, a.potential),
			cmp.Compare(b.lead.Estimate, a.lead.Estimate),
			cmp.Compare(a.target(), b.target()),
		)
	})
}

// follow removes and returns the leads to follow now: every anchor, and
// the count best of the others.
func (f *frontier) follow(count int) []candidate {
	n := 0
	for n < len(f.worth) && f.worth[n].given {
		n++
	}
	n = min(n+count, len(f.worth))
	followed := slices.Clone(f.worth[:n])
	f.worth = slices.Delete(f.worth, 0, n)
	return followed
}

// left counts what the frontier still holds.
func (f *frontier) left() Left {
	return Left{Unjudged: len(f.unjudged), Unfollowed: len(f.worth), Unread: len(f.open)}
}

// compareBool orders false before true.
func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case b:
		return -1
	}
	return 1
}

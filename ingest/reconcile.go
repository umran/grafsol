package ingest

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/source"
)

// Reconcile scans everything a source has and brings the graph into line
// with it: documents that are new or different are applied, and documents
// the source no longer has are removed. It is how the graph is first filled,
// and, run periodically, how whatever the change feed missed gets repaired.
//
// A document that is already up to date costs a few small ledger operations
// (to check it, and to note that it was seen) and nothing else: the graph is
// not written and enrichers do not run. A pass over a graph that needs no
// repair is therefore cheap next to the listing of the source itself.
//
// A document is removed only when two completed passes in a row have not
// seen it and no change for it arrived in between. One pass is not proof
// enough: a source's listing can shift while it is being read and skip a
// document that is still there.
//
// If Reconcile fails part-way, the next call picks the scan up where this
// one stopped. Reconcile should not run twice at once for the same source.
func (s *Syncer) Reconcile(ctx context.Context, src source.Scanner) (Report, error) {
	var report Report
	name := src.Name()

	scan, err := s.loadScan(ctx, name)
	if err != nil {
		return report, fmt.Errorf("ingest: reconciling %s: %w", name, err)
	}
	if scan.Started == 0 {
		// No scan is under way, so one begins now, and the ledger's clock
		// moves on. Every document this scan comes across will be recorded
		// at the new reading or a later one; the rest keep an earlier one.
		if scan.Started, err = s.ledger.Tick(ctx); err != nil {
			return report, fmt.Errorf("ingest: reconciling %s: %w", name, err)
		}
		if err := s.saveScan(ctx, name, scan); err != nil {
			return report, fmt.Errorf("ingest: reconciling %s: %w", name, err)
		}
	}

	for change, err := range src.Scan(ctx, scan.Resume) {
		if err != nil {
			return report, fmt.Errorf("ingest: scanning %s: %w", name, err)
		}
		outcome, declined, err := s.apply(ctx, change, scan.Started)
		if err != nil {
			return report, err
		}
		report.count(outcome)
		if declined {
			report.Declined++
		}

		if len(change.Checkpoint) > 0 {
			scan.Resume = change.Checkpoint
			if err := s.saveScan(ctx, name, scan); err != nil {
				return report, fmt.Errorf("ingest: reconciling %s: %w", name, err)
			}
		}
	}
	// A scan that was cut short has proved nothing about what it did not
	// reach, whatever the source's iterator made of the interruption.
	if err := ctx.Err(); err != nil {
		return report, err
	}

	// The scan ran to its end. Documents that neither it nor the pass
	// before it came across are gone from the source.
	if scan.Previous != 0 {
		removed, err := s.sweep(ctx, name, scan.Previous)
		report.Removed += removed
		if err != nil {
			return report, err
		}
	}

	err = s.saveScan(ctx, name, scanState{Previous: scan.Started})
	if err != nil {
		return report, fmt.Errorf("ingest: reconciling %s: %w", name, err)
	}
	return report, nil
}

// scanState is what the Syncer remembers about the scans of one source. It
// is kept in the ledger as a checkpoint named "scan/<source>".
type scanState struct {
	// Started is what the ledger's clock read once the scan now under way
	// had begun. It is zero when no scan is under way.
	Started uint64 `json:"started,omitempty"`

	// Resume is how far the scan now under way has got, in the source's
	// own terms.
	Resume source.Checkpoint `json:"resume,omitempty"`

	// Previous is the same reading for the last scan that completed. It is
	// zero until a scan has completed.
	Previous uint64 `json:"previous,omitempty"`
}

func (s *Syncer) loadScan(ctx context.Context, sourceName string) (scanState, error) {
	var scan scanState
	saved, err := s.ledger.Checkpoint(ctx, "scan/"+sourceName)
	if err != nil || len(saved) == 0 {
		return scan, err
	}
	if err := json.Unmarshal(saved, &scan); err != nil {
		return scan, fmt.Errorf("the saved scan state is unreadable: %w", err)
	}
	return scan, nil
}

func (s *Syncer) saveScan(ctx context.Context, sourceName string, scan scanState) error {
	encoded, err := json.Marshal(scan)
	if err != nil {
		return err
	}
	return s.ledger.SaveCheckpoint(ctx, "scan/"+sourceName, encoded)
}

// sweep removes the documents of a source whose marks were last recorded
// before the ledger's clock reached the given reading, and returns how many
// it removed.
//
// The reading is the one the previous completed scan began with. A document
// not recorded since then was seen neither by that scan nor by the one that
// has just finished, and no news of it arrived from a feed in all that time.
func (s *Syncer) sweep(ctx context.Context, sourceName string, before uint64) (int, error) {
	removed := 0
	cursor := ""
	for {
		ids, next, err := s.ledger.Unseen(ctx, sourceName, before, cursor)
		if err != nil {
			return removed, fmt.Errorf("ingest: sweeping %s: %w", sourceName, err)
		}
		for _, id := range ids {
			gone, err := s.removeIfStillUnseen(ctx, id, before)
			if err != nil {
				return removed, fmt.Errorf("ingest: sweeping %s: %w", id, err)
			}
			if gone {
				removed++
			}
		}
		if next == "" {
			return removed, nil
		}
		cursor = next
	}
}

// removeIfStillUnseen removes one document found by a sweep, unless a
// change for it has arrived since the sweep listed it.
func (s *Syncer) removeIfStillUnseen(ctx context.Context, id graph.ID, before uint64) (bool, error) {
	unlock, err := s.ledger.Lock(ctx, string(id))
	if err != nil {
		return false, err
	}
	defer unlock()

	mark, known, err := s.ledger.Mark(ctx, id)
	if err != nil {
		return false, err
	}
	if !known || mark.Deleted || mark.Seen >= before {
		return false, nil
	}
	// The tombstone keeps the document's last version. A feed change of
	// that same version will not bring the document back, but a later scan
	// that finds it will; see decide.
	return true, s.remove(ctx, id, mark.Source, mark.Version)
}

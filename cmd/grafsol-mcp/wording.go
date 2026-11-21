package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/mapping"
	"github.com/umran/grafsol/storage"
)

// wording holds the words the judge is shown for each kind of edge, and
// keeps them in step with the graph's settings. The kinds are a team's to
// define and to reword, and a team may do either while the server runs.
type wording struct {
	settings atomic.Pointer[mapping.Settings]
}

// newWording returns the words of the kinds every graph begins with, which
// stand until refresh has read the graph's own.
func newWording() *wording {
	w := &wording{}
	starter := mapping.DefaultSettings()
	w.settings.Store(&starter)
	return w
}

// Phrase returns the words for a kind of edge, or "" for a kind the graph's
// settings do not define. It is what the judge is given as jev.Config.Phrase.
func (w *wording) Phrase(kind graph.EdgeKind) string {
	return w.settings.Load().Phrase(kind)
}

// refresh reads the graph's settings and takes its words from them. If they
// cannot be read, the words it had stand.
func (w *wording) refresh(ctx context.Context, from storage.Settings) error {
	document, _, err := from.Settings(ctx)
	if err != nil {
		return err
	}
	settings, err := mapping.DecodeSettings(document)
	if err != nil {
		return err
	}
	w.settings.Store(&settings)
	return nil
}

// keep refreshes the words every so often until the context ends. A failure
// is logged the first time and again only after a success, so that a
// database that stays out of reach does not fill the log.
func (w *wording) keep(ctx context.Context, from storage.Settings, every time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		err := w.refresh(ctx, from)
		switch {
		case err != nil && !failing && ctx.Err() == nil:
			logger.Warn("the graph's settings could not be read; the judge is still shown the words it had", "error", err)
		case err == nil && failing:
			logger.Info("the graph's settings can be read again")
		}
		failing = err != nil
	}
}

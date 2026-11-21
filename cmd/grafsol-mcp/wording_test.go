package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/mapping"
	"github.com/umran/grafsol/storage/memory"
)

func TestTheJudgesWordsFollowTheGraphsSettings(t *testing.T) {
	store := memory.New()
	words := newWording()

	// Before anything is read, and for a graph whose team has saved nothing,
	// the words are those every graph begins with.
	for range 2 {
		if got := words.Phrase(graph.ChildOf); got != "is a part of" {
			t.Fatalf("child_of reads %q, want the words every graph begins with", got)
		}
		if got := words.Phrase("depends_on"); got != "" {
			t.Fatalf("a kind nobody has defined reads %q", got)
		}
		if err := words.refresh(t.Context(), store); err != nil {
			t.Fatalf("refresh: %v", err)
		}
	}

	// The team defines a kind and rewords another.
	settings := mapping.DefaultSettings()
	for i := range settings.Kinds {
		if settings.Kinds[i].Key == graph.ChildOf {
			settings.Kinds[i].Phrase = "belongs under"
		}
	}
	settings.Kinds = append(settings.Kinds, mapping.Kind{Key: "depends_on", Phrase: "depends on"})
	document, err := settings.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := store.SaveSettings(t.Context(), document, 0); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	// Nothing changes until the settings are read again.
	if got := words.Phrase("depends_on"); got != "" {
		t.Errorf("the new kind reads %q before the settings were read again", got)
	}
	if err := words.refresh(t.Context(), store); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := words.Phrase("depends_on"); got != "depends on" {
		t.Errorf("the team's new kind reads %q", got)
	}
	if got := words.Phrase(graph.ChildOf); got != "belongs under" {
		t.Errorf("the kind the team reworded reads %q", got)
	}

	// Settings that cannot be read, or cannot be reached, leave the words
	// as they were.
	if _, err := store.SaveSettings(t.Context(), []byte("{ not settings"), 1); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}
	if err := words.refresh(t.Context(), store); err == nil {
		t.Error("settings that are not settings were read without complaint")
	}
	if err := words.refresh(t.Context(), unreachable{}); err == nil {
		t.Error("a store that cannot be reached was read without complaint")
	}
	if got := words.Phrase("depends_on"); got != "depends on" {
		t.Errorf("after two failed readings the team's kind reads %q", got)
	}
}

func TestTheJudgesWordsAreKeptInStepWhileTheServerRuns(t *testing.T) {
	store := memory.New()
	words := newWording()
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		words.keep(ctx, store, time.Millisecond, slog.New(slog.DiscardHandler))
	}()

	settings := mapping.DefaultSettings()
	settings.Kinds = append(settings.Kinds, mapping.Kind{Key: "depends_on", Phrase: "depends on"})
	document, err := settings.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := store.SaveSettings(t.Context(), document, 0); err != nil {
		t.Fatalf("SaveSettings: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for words.Phrase("depends_on") != "depends on" {
		if time.Now().After(deadline) {
			t.Fatal("the words never caught up with the settings")
		}
		time.Sleep(time.Millisecond)
	}

	// It stops when the server does.
	stop()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the words were still being refreshed after the context ended")
	}
}

// unreachable is a store of settings that cannot be reached.
type unreachable struct{}

func (unreachable) Settings(context.Context) ([]byte, uint64, error) {
	return nil, 0, errors.New("the database is not there")
}

func (unreachable) SaveSettings(context.Context, []byte, uint64) (uint64, error) {
	return 0, errors.New("the database is not there")
}

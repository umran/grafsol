package storagetest

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/umran/grafsol/storage"
)

// RunSettings checks a Settings against its contract. open must return one
// with nothing saved every time it is called.
func RunSettings(t *testing.T, open func(t *testing.T) storage.Settings) {
	t.Helper()
	tests := []struct {
		name string
		run  func(t *testing.T, settings storage.Settings)
	}{
		{"StartEmpty", testSettingsStartEmpty},
		{"SaveThenRead", testSettingsSaveThenRead},
		{"SaveFromAnOldVersionIsRefused", testSettingsSaveFromAnOldVersion},
		{"RivalSavesLeaveOneWinner", testSettingsRivalSaves},
		{"DocumentIsKeptByteForByte", testSettingsKeepEveryByte},
		{"StoredDocumentIsACopy", testSettingsAreCopies},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.run(t, open(t))
		})
	}
}

func testSettingsStartEmpty(t *testing.T, settings storage.Settings) {
	document, version, err := settings.Settings(t.Context())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if len(document) != 0 || version != 0 {
		t.Errorf("before anything is saved the document is %q at version %d, want an empty one at version 0", document, version)
	}
}

func testSettingsSaveThenRead(t *testing.T, settings storage.Settings) {
	first, second := []byte(`{"rules":["first"]}`), []byte(`{"rules":["second"]}`)

	if got := saveSettings(t, settings, first, 0); got != 1 {
		t.Fatalf("the first save made version %d, want 1", got)
	}
	requireSettings(t, settings, first, 1)

	if got := saveSettings(t, settings, second, 1); got != 2 {
		t.Fatalf("the second save made version %d, want 2", got)
	}
	requireSettings(t, settings, second, 2)

	// Saving what is already stored is still a save: the version moves on.
	if got := saveSettings(t, settings, second, 2); got != 3 {
		t.Fatalf("saving the same document again made version %d, want 3", got)
	}
	requireSettings(t, settings, second, 3)
}

func testSettingsSaveFromAnOldVersion(t *testing.T, settings storage.Settings) {
	refused := func(document string, replaces uint64) {
		t.Helper()
		version, err := settings.SaveSettings(t.Context(), []byte(document), replaces)
		if !errors.Is(err, storage.ErrSettingsMoved) {
			t.Fatalf("a save in place of version %d made version %d with error %v, want one that wraps ErrSettingsMoved", replaces, version, err)
		}
	}

	// Nothing has been saved, so there is no version 1 or 7 to replace.
	refused("made from a version that never was", 1)
	refused("made from a version that never was", 7)
	requireSettings(t, settings, nil, 0)

	saveSettings(t, settings, []byte("first"), 0)
	saveSettings(t, settings, []byte("second"), 1)

	// Each of these was made from a document that is no longer the one
	// stored, or that has not been stored yet.
	refused("made from nothing", 0)
	refused("made from the first", 1)
	refused("made from one yet to come", 3)
	requireSettings(t, settings, []byte("second"), 2)
}

func testSettingsRivalSaves(t *testing.T, settings storage.Settings) {
	const rivals = 10
	saveSettings(t, settings, []byte("what everyone read"), 0)
	document := func(rival int) []byte { return fmt.Appendf(nil, "what rival %d made of it", rival) }

	// Every rival read version 1 and saves its own change to it.
	versions, failures := make([]uint64, rivals), make([]error, rivals)
	var group sync.WaitGroup
	for rival := range rivals {
		group.Go(func() {
			versions[rival], failures[rival] = settings.SaveSettings(t.Context(), document(rival), 1)
		})
	}
	group.Wait()

	winner := -1
	for rival, err := range failures {
		switch {
		case err == nil && winner >= 0:
			t.Fatalf("rivals %d and %d both saved in place of version 1", winner, rival)
		case err == nil:
			winner = rival
		case !errors.Is(err, storage.ErrSettingsMoved):
			t.Errorf("rival %d failed with %v, want an error that wraps ErrSettingsMoved", rival, err)
		}
	}
	if winner < 0 {
		t.Fatal("no rival's save went through")
	}
	if versions[winner] != 2 {
		t.Errorf("the save that went through made version %d, want 2", versions[winner])
	}
	requireSettings(t, settings, document(winner), 2)
}

func testSettingsKeepEveryByte(t *testing.T, settings storage.Settings) {
	// The store keeps bytes, not text: nothing in here may be tidied,
	// re-encoded or cut short.
	document := []byte("{ \"spaced\" :\t1 }\n\x00 a zero byte, \xff\xfe not UTF-8, it's \"quoted\" \\ 🙂")
	saveSettings(t, settings, document, 0)
	requireSettings(t, settings, document, 1)

	// An empty document is a document like any other.
	saveSettings(t, settings, nil, 1)
	requireSettings(t, settings, nil, 2)
}

func testSettingsAreCopies(t *testing.T, settings storage.Settings) {
	document := []byte("as saved")
	saveSettings(t, settings, document, 0)
	copy(document, "CHANGED!") // the caller's bytes, changed after the save

	read, _, err := settings.Settings(t.Context())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if string(read) != "as saved" {
		t.Fatalf("changing the bytes that were saved changed the store: it now reads %q", read)
	}
	copy(read, "CHANGED!") // and the bytes that were read
	requireSettings(t, settings, []byte("as saved"), 1)
}

// --- helpers ------------------------------------------------------------------

func saveSettings(t *testing.T, settings storage.Settings, document []byte, replaces uint64) uint64 {
	t.Helper()
	version, err := settings.SaveSettings(t.Context(), document, replaces)
	if err != nil {
		t.Fatalf("SaveSettings in place of version %d: %v", replaces, err)
	}
	return version
}

func requireSettings(t *testing.T, settings storage.Settings, want []byte, wantVersion uint64) {
	t.Helper()
	document, version, err := settings.Settings(t.Context())
	if err != nil {
		t.Fatalf("Settings: %v", err)
	}
	if !bytes.Equal(document, want) || version != wantVersion {
		t.Fatalf("the settings are %q at version %d, want %q at version %d", document, version, want, wantVersion)
	}
}

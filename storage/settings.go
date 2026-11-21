package storage

import (
	"context"
	"errors"
)

// Settings keeps the one document in which a team's decisions about how its
// graph is made are written down: its kinds of edge, its rules, the topics
// it has merged. The store does not read the document (package mapping
// does). It keeps it, and it numbers each saving of it.
//
// The number is what stops two people who edit at the same time from undoing
// each other: a save names the version it was made from, and is refused if
// that is no longer the version stored. It also tells whoever makes the
// graph when the document they made it from has been replaced.
//
// Implementations must be safe for concurrent use, including by several
// processes that share one store.
type Settings interface {
	// Settings returns the document last saved, and its version. Until
	// something is saved the document is empty and the version is zero.
	Settings(ctx context.Context) (document []byte, version uint64, err error)

	// SaveSettings stores the document in place of the one whose version is
	// replaces, and returns the new version, which is one more.
	//
	// If the version stored is not replaces, somebody has saved since the
	// caller last read. Nothing is stored, and the error wraps
	// ErrSettingsMoved: the caller should read again, make its change to
	// what it finds, and save that.
	SaveSettings(ctx context.Context, document []byte, replaces uint64) (version uint64, err error)
}

// ErrSettingsMoved reports that the settings were saved by someone else
// between a caller's reading them and its saving them.
var ErrSettingsMoved = errors.New("storage: the settings have been saved again since they were read")

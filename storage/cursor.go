package storage

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/umran/grafsol/graph"
)

// ErrBadCursor is returned, wrapped, when a cursor was not produced by the
// backend it is handed to.
var ErrBadCursor = errors.New("storage: malformed cursor")

// EdgePosition is where a page of edges ended: the kind of the last edge
// returned and the vertex at its other end. Since edges are listed in
// (kind, neighbor) order, the next page is everything after this position.
// Backends are free to use it as the content of their cursors.
type EdgePosition struct {
	Kind     graph.EdgeKind `json:"kind"`
	Neighbor graph.ID       `json:"neighbor"`
}

// EncodeCursor turns a position into the opaque string handed to callers.
// The position can be any value that encoding/json can encode.
func EncodeCursor(position any) string {
	encoded, err := json.Marshal(position)
	if err != nil {
		// Positions are small structs of strings and numbers defined by
		// the backends themselves; failing to encode one is a programming
		// error, not something a caller can cause or handle.
		panic(fmt.Sprintf("storage: cannot encode cursor position %#v: %v", position, err))
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// DecodeCursor reads a cursor made by EncodeCursor back into position, which
// must be a pointer. An empty cursor means "from the beginning" and leaves
// position untouched.
func DecodeCursor(cursor string, position any) error {
	if cursor == "" {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	if err := json.Unmarshal(decoded, position); err != nil {
		return fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	return nil
}

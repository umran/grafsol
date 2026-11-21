package mapping

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/umran/grafsol/graph"
)

// Settings is everything a team has decided about how its graph is made: the
// kinds of edge there are, the rules that say what a source's references and
// labels become, and the topics that have been merged into others.
//
// The three are kept as one document and replaced whole (storage.Settings
// keeps it), so that they always fit together: a rule can only name a kind
// that the same document defines.
type Settings struct {
	// Kinds are the kinds of edge the team has defined.
	Kinds []Kind `json:"kinds"`

	// Rules are the team's rules, in the order they are tried. The rules a
	// source ships with (Defaults) come after them and are not listed here.
	Rules Rules `json:"rules,omitempty"`

	// Aliases names, for each topic that was merged away, the topic it was
	// merged into. Whatever would be filed under the first is filed under
	// the second.
	Aliases map[graph.ID]graph.ID `json:"aliases,omitempty"`
}

// Edition numbers the rules that ship with grafsol and the way a rule is
// matched. It is raised whenever either changes, which tells a graph that
// was made under an earlier edition to make its edges again.
const Edition = 1

// DefaultSettings returns the settings a graph has until a team saves its
// own: the starter kinds, no rules beyond those each source ships with, and
// no topic merged into another.
func DefaultSettings() Settings {
	return Settings{Kinds: StarterKinds()}
}

// shippedFor lists the sources that Defaults has rules for; the empty name
// stands for every source it does not know.
var shippedFor = []string{"github", ""}

// Validate reports what is wrong with the settings, or nil.
func (s Settings) Validate() error {
	defined := make(map[graph.EdgeKind]bool, len(s.Kinds))
	for _, kind := range s.Kinds {
		if err := kind.Validate(); err != nil {
			return err
		}
		if defined[kind.Key] {
			return fmt.Errorf("mapping: the kind %q is defined twice", kind.Key)
		}
		defined[kind.Key] = true
	}
	if err := s.Rules.Validate(s.Kinds); err != nil {
		return err
	}

	// The rules a source ships with are tried after the team's, whatever the
	// team has decided. A kind they make cannot be taken away from them.
	for _, source := range shippedFor {
		if err := Defaults(source).check(s.Kinds); err != nil {
			return fmt.Errorf("mapping: grafsol ships with rules that these kinds would break: %w", err)
		}
	}

	for _, from := range slices.Sorted(maps.Keys(s.Aliases)) {
		into := s.Aliases[from]
		switch further, chained := s.Aliases[into]; {
		case !isTopic(from):
			return fmt.Errorf("mapping: %q cannot be merged into another topic: it is not a topic", string(from))
		case !isTopic(into):
			return fmt.Errorf("mapping: the topic %s cannot be merged into %q, which is not a topic", from, string(into))
		case from == into:
			return fmt.Errorf("mapping: the topic %s is merged into itself", from)
		case chained:
			return fmt.Errorf("mapping: the topic %s is merged into %s, which was itself merged into %s", from, into, further)
		}
	}
	return nil
}

func isTopic(id graph.ID) bool {
	return id.Validate() == nil && id.Kind() == graph.KindTopic
}

// Phrase returns the phrase of a kind, or "" for a kind that the settings do
// not define.
func (s Settings) Phrase(kind graph.EdgeKind) string {
	for _, defined := range s.Kinds {
		if defined.Key == kind {
			return defined.Phrase
		}
	}
	return ""
}

// MergedInto returns the topic that a topic was merged into, or the topic
// itself if it was merged into none.
func (s Settings) MergedInto(topic graph.ID) graph.ID {
	if into, merged := s.Aliases[topic]; merged {
		return into
	}
	return topic
}

// settingsFormat is the form the encoded document takes. A document says
// which form it is in, so that a later form can be told from this one.
const settingsFormat = 1

// encodedSettings is the document as it is stored.
type encodedSettings struct {
	Format int `json:"format"`
	Settings
}

// Encode returns the settings as the document that is stored. Settings that
// do not validate are not encoded, so they are never stored. The same
// settings always encode to the same bytes.
func (s Settings) Encode() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(encodedSettings{Format: settingsFormat, Settings: s})
	if err != nil {
		return nil, fmt.Errorf("mapping: encoding the settings: %w", err)
	}
	return encoded, nil
}

// DecodeSettings reads a stored document. An empty document is what a graph
// has when no team has saved its settings yet, and decodes to
// DefaultSettings.
func DecodeSettings(document []byte) (Settings, error) {
	if len(document) == 0 {
		return DefaultSettings(), nil
	}
	var decoded encodedSettings
	if err := json.Unmarshal(document, &decoded); err != nil {
		return Settings{}, fmt.Errorf("mapping: the stored settings are unreadable: %w", err)
	}
	if decoded.Format != settingsFormat {
		return Settings{}, fmt.Errorf("mapping: the stored settings are in format %d, and this version of grafsol reads format %d", decoded.Format, settingsFormat)
	}
	return decoded.Settings, nil
}

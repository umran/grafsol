package mapping_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/mapping"
)

func TestDefaultSettings(t *testing.T) {
	settings := mapping.DefaultSettings()
	if err := settings.Validate(); err != nil {
		t.Fatalf("the settings a graph begins with do not validate: %v", err)
	}
	if !reflect.DeepEqual(settings.Kinds, mapping.StarterKinds()) {
		t.Errorf("the kinds are %+v, want the starter kinds", settings.Kinds)
	}
	if len(settings.Rules) != 0 || len(settings.Aliases) != 0 {
		t.Errorf("a graph begins with %d rules and %d aliases, want none of either", len(settings.Rules), len(settings.Aliases))
	}
}

// teamSettings is a document with one of everything in it.
func teamSettings() mapping.Settings {
	settings := mapping.DefaultSettings()
	settings.Kinds = append(settings.Kinds,
		mapping.Kind{Key: "depends_on", Phrase: "depends on", Description: "The first cannot go ahead until the second is done."},
		mapping.Kind{Key: "owned_by", Phrase: "is owned by", ToTopic: true},
	)
	settings.Rules = mapping.Rules{
		{ID: "implements", Source: "github", Form: graph.FormMention, After: []string{"implements", "addresses"}, Kind: mapping.Resolves},
		{ID: "bare", Source: "github", Form: graph.FormMention, Alone: true, Kind: graph.ChildOf},
		{ID: "blocked", Form: graph.FormMention, After: []string{"blocked by"}, Kind: "depends_on"},
		{ID: "bots", Source: "github", Form: mapping.FormLabel, Label: "dependencies", Ignore: true},
		{ID: "teams", Form: mapping.FormLabel, Label: "team/*", Kind: "owned_by"},
		{ID: "docs", Form: mapping.FormLabel, Label: "docs", Topic: "Documentation"},
	}
	settings.Aliases = map[graph.ID]graph.ID{
		graph.TopicID("docs"):   graph.TopicID("Documentation"),
		graph.TopicID("readme"): graph.TopicID("Documentation"),
	}
	return settings
}

func TestSettingsSurviveEncoding(t *testing.T) {
	want := teamSettings()
	encoded, err := want.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, err := mapping.DecodeSettings(encoded)
	if err != nil {
		t.Fatalf("DecodeSettings: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after encoding and decoding the settings are\n  %+v\nwant\n  %+v", got, want)
	}

	// The same settings always encode to the same bytes, so that two
	// documents can be compared without decoding either.
	again, err := teamSettings().Encode()
	if err != nil || string(again) != string(encoded) {
		t.Errorf("encoding the same settings twice gave different documents (error %v)", err)
	}
}

// The encoded document is kept in a database for as long as the graph is. If
// a field were renamed in the code, documents already stored would lose it.
func TestTheEncodedFormIsFixed(t *testing.T) {
	settings := mapping.Settings{
		Kinds: []mapping.Kind{
			{Key: graph.LinksTo, Phrase: "links to", Description: "A mention."},
			{Key: graph.ChildOf, Phrase: "is a part of"},
			{Key: mapping.Resolves, Phrase: "resolves"},
			{Key: mapping.Duplicates, Phrase: "duplicates"},
			{Key: graph.InTopic, Phrase: "belongs to", ToTopic: true},
		},
		Rules: mapping.Rules{
			{ID: "a", Source: "github", Form: graph.FormMention, After: []string{"implements"}, Kind: mapping.Resolves},
			{ID: "b", Form: graph.FormMention, Alone: true, Kind: graph.ChildOf},
			{ID: "c", Form: mapping.FormLabel, Label: "area/*", Kind: graph.InTopic, Topic: "Areas"},
			{ID: "d", Form: mapping.FormLabel, Ignore: true},
		},
		Aliases: map[graph.ID]graph.ID{"topic:docs": "topic:documentation"},
	}
	const want = `{"format":1,` +
		`"kinds":[` +
		`{"key":"links_to","phrase":"links to","description":"A mention."},` +
		`{"key":"child_of","phrase":"is a part of"},` +
		`{"key":"resolves","phrase":"resolves"},` +
		`{"key":"duplicates","phrase":"duplicates"},` +
		`{"key":"in_topic","phrase":"belongs to","to_topic":true}],` +
		`"rules":[` +
		`{"id":"a","source":"github","form":"mention","after":["implements"],"kind":"resolves"},` +
		`{"id":"b","form":"mention","alone":true,"kind":"child_of"},` +
		`{"id":"c","form":"label","label":"area/*","kind":"in_topic","topic":"Areas"},` +
		`{"id":"d","form":"label","ignore":true}],` +
		`"aliases":{"topic:docs":"topic:documentation"}}`

	encoded, err := settings.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if string(encoded) != want {
		t.Errorf("the settings encode as\n  %s\nwant\n  %s", encoded, want)
	}
}

func TestDecodeSettings(t *testing.T) {
	t.Run("nothing saved yet", func(t *testing.T) {
		for _, document := range [][]byte{nil, {}} {
			settings, err := mapping.DecodeSettings(document)
			if err != nil {
				t.Fatalf("DecodeSettings(%q): %v", document, err)
			}
			if !reflect.DeepEqual(settings, mapping.DefaultSettings()) {
				t.Errorf("DecodeSettings(%q) = %+v, want the settings a graph begins with", document, settings)
			}
		}
	})

	t.Run("not a document", func(t *testing.T) {
		for _, document := range []string{"{", "[]", `"rules"`, `{"format":1,"kinds":"all of them"}`} {
			if _, err := mapping.DecodeSettings([]byte(document)); err == nil {
				t.Errorf("DecodeSettings(%s) succeeded", document)
			}
		}
	})

	t.Run("written by a later grafsol", func(t *testing.T) {
		_, err := mapping.DecodeSettings([]byte(`{"format":2,"kinds":[]}`))
		if err == nil || !strings.Contains(err.Error(), "format 2") {
			t.Errorf("a document in a format this code does not know decoded with error %v, want one that names the format", err)
		}
	})

	t.Run("no format at all", func(t *testing.T) {
		if _, err := mapping.DecodeSettings([]byte(`{"kinds":[]}`)); err == nil {
			t.Errorf("a document that names no format decoded")
		}
	})
}

func TestSettingsValidate(t *testing.T) {
	without := func(key graph.EdgeKind) []mapping.Kind {
		var kinds []mapping.Kind
		for _, kind := range mapping.StarterKinds() {
			if kind.Key != key {
				kinds = append(kinds, kind)
			}
		}
		return kinds
	}

	tests := []struct {
		name   string
		change func(*mapping.Settings)
		want   string // part of the error; empty means the settings are valid
	}{
		{"the settings a graph begins with", func(*mapping.Settings) {}, ""},
		{"a team's own kinds, rules and aliases", func(s *mapping.Settings) { *s = teamSettings() }, ""},
		{"a kind the shipped rules do not need may go", func(s *mapping.Settings) { s.Kinds = without(mapping.Supersedes) }, ""},

		{"a kind with no phrase", func(s *mapping.Settings) {
			s.Kinds = append(s.Kinds, mapping.Kind{Key: "depends_on"})
		}, "no phrase"},
		{"a kind with a malformed key", func(s *mapping.Settings) {
			s.Kinds = append(s.Kinds, mapping.Kind{Key: "Depends On", Phrase: "depends on"})
		}, "Depends On"},
		{"a kind defined twice", func(s *mapping.Settings) {
			s.Kinds = append(s.Kinds, mapping.Kind{Key: mapping.Resolves, Phrase: "settles"})
		}, `defined twice`},
		{"a rule that names a kind nobody defined", func(s *mapping.Settings) {
			s.Rules = mapping.Rules{{ID: "x", Form: graph.FormMention, Kind: "depends_on"}}
		}, "depends_on"},
		{"a kind that a shipped rule makes", func(s *mapping.Settings) { s.Kinds = without(mapping.Resolves) }, "ships with"},
		{"the kind that labels become", func(s *mapping.Settings) { s.Kinds = without(graph.InTopic) }, "ships with"},
		{"a shipped kind turned to point at topics", func(s *mapping.Settings) {
			for i := range s.Kinds {
				if s.Kinds[i].Key == graph.LinksTo {
					s.Kinds[i].ToTopic = true
				}
			}
		}, "ships with"},

		{"an alias for a document", func(s *mapping.Settings) {
			s.Aliases = map[graph.ID]graph.ID{"doc:github:acme/api#1": "topic:docs"}
		}, "doc:github:acme/api#1"},
		{"an alias to a document", func(s *mapping.Settings) {
			s.Aliases = map[graph.ID]graph.ID{"topic:docs": "doc:github:acme/api#1"}
		}, "doc:github:acme/api#1"},
		{"an alias to nothing", func(s *mapping.Settings) {
			s.Aliases = map[graph.ID]graph.ID{"topic:docs": ""}
		}, "topic:docs"},
		{"a topic merged into itself", func(s *mapping.Settings) {
			s.Aliases = map[graph.ID]graph.ID{"topic:docs": "topic:docs"}
		}, "into itself"},
		{"a topic merged into a topic that was merged away", func(s *mapping.Settings) {
			s.Aliases = map[graph.ID]graph.ID{"topic:docs": "topic:documentation", "topic:documentation": "topic:guides"}
		}, "was itself merged"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			settings := mapping.DefaultSettings()
			test.change(&settings)
			err := settings.Validate()
			switch {
			case test.want == "" && err != nil:
				t.Errorf("Validate: %v", err)
			case test.want != "" && err == nil:
				t.Errorf("Validate accepted the settings, want an error about %q", test.want)
			case test.want != "" && !strings.Contains(err.Error(), test.want):
				t.Errorf("Validate: %v, want an error about %q", err, test.want)
			}

			// What does not validate is never encoded, so it is never stored.
			if _, encodeErr := settings.Encode(); (encodeErr == nil) != (err == nil) {
				t.Errorf("Validate said %v and Encode said %v", err, encodeErr)
			}
		})
	}
}

func TestPhrase(t *testing.T) {
	settings := teamSettings()
	for kind, want := range map[graph.EdgeKind]string{
		graph.LinksTo:    "links to",
		graph.ChildOf:    "is a part of",
		graph.InTopic:    "belongs to",
		mapping.Resolves: "resolves",
		"depends_on":     "depends on",
		"never_defined":  "",
		"":               "",
	} {
		if got := settings.Phrase(kind); got != want {
			t.Errorf("Phrase(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestMergedInto(t *testing.T) {
	settings := teamSettings()
	for topic, want := range map[graph.ID]graph.ID{
		"topic:docs":            "topic:documentation",
		"topic:readme":          "topic:documentation",
		"topic:documentation":   "topic:documentation", // merged into nothing
		"topic:billing":         "topic:billing",
		"doc:github:acme/api#1": "doc:github:acme/api#1",
	} {
		if got := settings.MergedInto(topic); got != want {
			t.Errorf("MergedInto(%s) = %s, want %s", topic, got, want)
		}
	}
	if got := mapping.DefaultSettings().MergedInto("topic:docs"); got != "topic:docs" {
		t.Errorf("with no aliases, MergedInto(topic:docs) = %s", got)
	}
}

package github

import (
	"slices"
	"strings"
	"testing"

	"github.com/umran/grafsol/graph"
)

func TestReferences(t *testing.T) {
	text := strings.Join([]string{
		"Fixes #12 and relates to acme/web#7.",
		"",
		"See https://example.com/spec, please (and https://acme.atlassian.net/browse/PAY-7).",
		"```css",
		"color: #333; /* not a mention, and https://ignored.example is not a link */",
		"```",
		"Not mentions: abc#5, &#39; and the end of https://github.com/acme/api/issues/9#issuecomment-123.",
		"Brackets are fine (#30), as is a list:",
		"- #31, #32",
	}, "\n")

	got := references("acme/api", text)

	type simple struct {
		target graph.ID
		url    string
	}
	var summary []simple
	for _, reference := range got {
		summary = append(summary, simple{reference.target, reference.url})
	}
	want := []simple{
		{target: "doc:github:acme/api#12"},
		{target: "doc:github:acme/web#7"},
		{url: "https://example.com/spec"},
		{url: "https://acme.atlassian.net/browse/PAY-7"},
		{url: "https://github.com/acme/api/issues/9#issuecomment-123"},
		{target: "doc:github:acme/api#30"},
		{target: "doc:github:acme/api#31"},
		{target: "doc:github:acme/api#32"},
	}
	if !slices.Equal(summary, want) {
		t.Fatalf("references found\n  %+v\nwant\n  %+v", summary, want)
	}

	// The context of a reference is the line it stands on.
	if got[0].context != "Fixes #12 and relates to acme/web#7." {
		t.Errorf("context = %q", got[0].context)
	}
	if got[7].context != "- #31, #32" {
		t.Errorf("context = %q", got[7].context)
	}
}

func TestReferencesInAwkwardText(t *testing.T) {
	if found := references("acme/api", ""); len(found) != 0 {
		t.Errorf("empty text yielded %v", found)
	}
	// A number too long to be an issue number, and a zero.
	if found := references("acme/api", "#12345678901234567890 and #0"); len(found) != 0 {
		t.Errorf("implausible numbers yielded %v", found)
	}
	// Mentions of other repositories are lower-cased like every other ID.
	found := references("acme/api", "see Acme/Web#3")
	if len(found) != 1 || found[0].target != "doc:github:acme/web#3" {
		t.Errorf("got %v", found)
	}
	// A very long line is cut to a readable length.
	long := strings.Repeat("word ", 200) + "#4"
	found = references("acme/api", long)
	if len(found) != 1 || len([]rune(found[0].context)) > maxContext || !strings.HasSuffix(found[0].context, "…") {
		t.Errorf("a long line was not shortened: %d characters", len([]rune(found[0].context)))
	}
}

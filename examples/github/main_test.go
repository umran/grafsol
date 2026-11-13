package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/source"
)

// parse reads a command line's -repository flags, as run does.
func parse(args ...string) (repositories, error) {
	var named repositories
	flags := flag.NewFlagSet("github", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Var(&named, "repository", "")
	return named, flags.Parse(args)
}

// One graph may hold several repositories, and they have to be named
// together: the flag may be repeated, may hold several names separated by
// commas, and may do both.
func TestRepositoryFlagCollectsEveryName(t *testing.T) {
	cases := map[string]struct {
		args []string
		want []string
	}{
		"none":                {nil, nil},
		"one":                 {[]string{"-repository", "acme/api"}, []string{"acme/api"}},
		"repeated":            {[]string{"-repository", "acme/api", "-repository", "acme/web"}, []string{"acme/api", "acme/web"}},
		"separated by commas": {[]string{"-repository", "acme/api,acme/web"}, []string{"acme/api", "acme/web"}},
		"both, with spaces":   {[]string{"-repository", " acme/api , acme/web", "-repository", "acme/docs"}, []string{"acme/api", "acme/web", "acme/docs"}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := parse(c.args...)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal([]string(got), c.want) {
				t.Errorf("got %q, want %q", []string(got), c.want)
			}
		})
	}
}

// An empty name is what an unset shell variable leaves behind. Passing over
// it would quietly copy fewer repositories than were asked for.
func TestRepositoryFlagRefusesAnEmptyName(t *testing.T) {
	for _, value := range []string{"", " ", "acme/api,", "acme/api,,acme/web"} {
		if got, err := parse("-repository", value); err == nil {
			t.Errorf("-repository %q was accepted, as %q", value, []string(got))
		}
	}
}

func TestRepositoriesAreNamedInASentence(t *testing.T) {
	cases := []struct {
		named repositories
		want  string
	}{
		{repositories{"acme/api"}, "the repository acme/api"},
		{repositories{"acme/api", "acme/web"}, "the repositories acme/api and acme/web"},
		{repositories{"acme/api", "acme/web", "acme/docs"}, "the repositories acme/api, acme/web and acme/docs"},
	}
	for _, c := range cases {
		if got := c.named.sentence(); got != c.want {
			t.Errorf("%q: got %q, want %q", []string(c.named), got, c.want)
		}
	}
}

// listing is a scanner that lists a fixed number of documents, and then fails
// if it was told to.
type listing struct {
	documents int
	fails     error
}

func (listing) Name() string                    { return "test" }
func (listing) Resolve(string) (graph.ID, bool) { return "", false }

func (l listing) Scan(context.Context, source.Checkpoint) iter.Seq2[source.Change, error] {
	return func(yield func(source.Change, error) bool) {
		for range l.documents {
			if !yield(source.Change{}, nil) {
				return
			}
		}
		if l.fails != nil {
			yield(source.Change{}, l.fails)
		}
	}
}

// A first run summarises every document, which takes minutes. The scan says
// how far it has got as it goes, and passes everything on untouched.
func TestProgressIsReportedEverySoManyDocuments(t *testing.T) {
	failure := errors.New("the listing broke off")
	var said strings.Builder
	scanner := progress{Scanner: listing{documents: 25, fails: failure}, out: &said, every: 10}

	documents, failures := 0, 0
	for _, err := range scanner.Scan(context.Background(), nil) {
		if err != nil {
			if !errors.Is(err, failure) {
				t.Errorf("the scan's error came through as %v", err)
			}
			failures++
			continue
		}
		documents++
	}
	if documents != 25 || failures != 1 {
		t.Errorf("passed on %d documents and %d errors, want 25 and 1", documents, failures)
	}
	if want := "  10 documents read\n  20 documents read\n"; said.String() != want {
		t.Errorf("said %q, want %q", said.String(), want)
	}
}

// A consumer that stops early stops the scan beneath it.
func TestProgressStopsWhenItsConsumerDoes(t *testing.T) {
	scanner := progress{Scanner: listing{documents: 25}, out: io.Discard, every: 10}
	taken := 0
	for range scanner.Scan(context.Background(), nil) {
		if taken++; taken == 3 {
			break
		}
	}
	if taken != 3 {
		t.Errorf("took %d documents, want 3", taken)
	}
}

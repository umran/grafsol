package github_test

import (
	"os"
	"testing"

	"github.com/umran/grafsol/source/github"
)

// TestLiveScan reads a few issues from the real GitHub API. It only runs
// when asked to, by naming a public repository:
//
//	GRAFSOL_LIVE_GITHUB=golang/go go test -run TestLiveScan -v ./source/github/
//
// It makes one unauthenticated request and changes nothing. Its purpose is
// to confirm that GitHub still describes issues the way this package reads
// them, which no test against a stand-in can.
func TestLiveScan(t *testing.T) {
	repository := os.Getenv("GRAFSOL_LIVE_GITHUB")
	if repository == "" {
		t.Skip("GRAFSOL_LIVE_GITHUB is not set")
	}
	gh, err := github.New(github.Config{
		Repositories: []string{repository},
		UserAgent:    "go-test-client",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	seen := 0
	for change, err := range gh.Scan(t.Context(), nil) {
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if err := change.Validate(); err != nil {
			t.Errorf("%s does not validate: %v", change.Document.ID, err)
		}
		document := change.Document
		if document.URL == "" || document.Title == "" || document.CreatedAt.IsZero() || document.UpdatedAt.IsZero() || change.Version == 0 {
			t.Errorf("%s came back with fields missing: %+v", document.ID, document)
		}
		t.Logf("%s  %-12s  %d edges, %d topics, %d links  %q",
			document.ID, document.Type, len(change.Edges), len(change.Topics), len(change.Links), document.Title)
		if seen++; seen == 8 {
			break
		}
	}
	if seen == 0 {
		t.Errorf("the scan of %s listed nothing", repository)
	}
}

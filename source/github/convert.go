package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/source"
)

// issue is the part of GitHub's issue object that this package reads. The
// REST API and webhooks both use this shape, and pull requests appear in it
// too: to GitHub every pull request is also an issue.
type issue struct {
	Number    int       `json:"number"`
	HTMLURL   string    `json:"html_url"`
	Title     string    `json:"title"`
	Body      string    `json:"body"` // null in JSON when empty, which leaves ""
	Labels    []label   `json:"labels"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	// PullRequest is present in a listing of issues exactly when the item
	// is really a pull request. What it holds does not matter here.
	PullRequest json.RawMessage `json:"pull_request"`

	// ParentIssueURL is the API address of the issue this one is a
	// sub-issue of. GitHub's API includes the field only for an issue that
	// has a parent and leaves it out otherwise (observed on the live API;
	// the documentation says "string or null"). It is kept raw so that a
	// field that is present can be told from one that is absent.
	ParentIssueURL json.RawMessage `json:"parent_issue_url"`
}

// label is one label on an issue.
type label struct {
	Name        string
	Description string
}

// UnmarshalJSON accepts both forms GitHub uses for a label: an object with
// a name and a description, or just the name as a string.
func (l *label) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		return json.Unmarshal(data, &l.Name)
	}
	var object struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	l.Name, l.Description = object.Name, object.Description
	return nil
}

func (i issue) isPullRequest() bool {
	return len(i.PullRequest) > 0 && string(i.PullRequest) != "null"
}

// mentionsItsParent reports whether the payload carries the parent_issue_url
// field at all.
func (i issue) mentionsItsParent() bool {
	return len(i.ParentIssueURL) > 0
}

// parent returns the document that the issue is a sub-issue of, if any.
// The parent is given as the API address of an issue, such as
// "https://api.github.com/repos/acme/api/issues/7".
func (i issue) parent() (graph.ID, bool) {
	var address string
	if err := json.Unmarshal(i.ParentIssueURL, &address); err != nil || address == "" {
		return "", false
	}
	parsed, err := url.Parse(address)
	if err != nil {
		return "", false
	}
	// Find ".../repos/<owner>/<name>/issues/<number>". A GitHub Enterprise
	// Server puts "/api/v3" in front, so look for "repos" rather than
	// counting from the start.
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for at := 0; at+4 < len(parts); at++ {
		if parts[at] != "repos" || parts[at+3] != "issues" {
			continue
		}
		if number, ok := parseNumber(parts[at+4]); ok {
			return documentID(parts[at+1]+"/"+parts[at+2], number), true
		}
	}
	return "", false
}

// change describes an issue or pull request of a repository as a
// source.Change: the document, and every edge that starts at it.
func (s *Source) change(repository string, item issue, pullRequest bool) (source.Change, error) {
	if item.Number <= 0 {
		return source.Change{}, fmt.Errorf("github: an item of %s has no number", repository)
	}
	documentType := graph.TypeIssue
	if pullRequest || item.isPullRequest() {
		documentType = graph.TypePullRequest
	}

	change := source.Change{
		Document: graph.Document{
			ID:        documentID(repository, item.Number),
			Type:      documentType,
			Source:    sourceName,
			URL:       item.HTMLURL,
			Title:     item.Title,
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
		},
		Version: versionAt(item.UpdatedAt),
		Content: strings.TrimSpace(item.Title + "\n\n" + item.Body),
	}

	for _, label := range item.Labels {
		if label.Name != "" {
			change.Tag(label.Name, label.Description)
		}
	}
	if parent, ok := item.parent(); ok {
		change.ChildOf(parent, "sub-issue of")
	}
	for _, reference := range references(repository, item.Title+"\n"+item.Body) {
		switch {
		case reference.url == "":
			change.LinkTo(reference.target, reference.context)
		default:
			// A link to another issue or pull request on GitHub is ours to
			// recognize. Anything else may belong to another source, which
			// only the sync engine can tell.
			if target, ours := s.Resolve(reference.url); ours {
				change.LinkTo(target, reference.context)
			} else {
				change.LinkURL(reference.url, reference.context)
			}
		}
	}
	return change, nil
}

// removal describes an issue or pull request that no longer exists.
func removal(repository string, item issue) source.Change {
	return source.Change{
		Document: graph.Document{ID: documentID(repository, item.Number), Source: sourceName},
		Version:  versionAt(item.UpdatedAt),
		Deleted:  true,
	}
}

// versionAt turns an issue's updated_at into a document version: the number
// of nanoseconds since 1970. GitHub moves updated_at forward whenever an
// issue changes, which is exactly what a version needs to do.
func versionAt(updatedAt time.Time) uint64 {
	if nanoseconds := updatedAt.UnixNano(); nanoseconds > 0 {
		return uint64(nanoseconds)
	}
	return 0
}

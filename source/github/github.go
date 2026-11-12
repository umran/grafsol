// Package github connects GitHub issues and pull requests to the graph.
//
// A Source has two of the three capabilities in package source:
//
//   - Receiver: Receive turns GitHub webhook deliveries into changes. This
//     is the prompt path. Create a webhook on the repository or the
//     organization with the "Issues" and "Pull requests" events selected,
//     and point it at the handler returned by ingest.Syncer.Handler.
//   - Scanner: Scan lists every issue and pull request of the configured
//     repositories through the REST API. This is the first backfill and the
//     periodic repair.
//
// GitHub offers no change log that can be read reliably from a saved
// position (its events API is delayed and incomplete), so a Source is not a
// source.Subscriber.
//
// # What becomes what
//
//	issue or pull request #42 in acme/api   Document "doc:github:acme/api#42"
//	each label                              Topic, and a TaggedWith edge to it
//	the parent of a sub-issue               ChildOf edge
//	"#12" or "acme/web#12" in the text      LinksTo edge
//	any other URL in the text               a Link, for the sync engine to resolve
//	title and body                          Content, for enrichers
//
// A document's Version is its updated_at time. That orders the changes of
// one issue, which is all a version is for; it is never used to decide what
// to fetch next.
package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/source"
)

// Config says which repositories a Source covers and how it reaches GitHub.
type Config struct {
	// Repositories lists the repositories to cover, each as "owner/name".
	// Scan lists them in this order, and Receive ignores deliveries about
	// any other repository. At least one is required.
	Repositories []string

	// Token authenticates API requests. It may be left empty for public
	// repositories, at the price of a much lower rate limit.
	Token string

	// WebhookSecret is the secret entered when the webhook was created on
	// GitHub. Receive uses it to check that deliveries really come from
	// GitHub, and rejects every delivery when it is empty.
	WebhookSecret string

	// TrustIssuePayloads says that a webhook's issue payload can be taken
	// as the whole truth about the issue.
	//
	// GitHub's API names an issue's parent (parent_issue_url) only when the
	// issue has one. It has not been confirmed that webhook payloads do the
	// same, so by default an issue payload that names no parent is not
	// taken to mean "no parent": Receive reads the issue from the API
	// instead, at the cost of one request per such event.
	//
	// To find out, open the webhook's "Recent Deliveries" on GitHub and
	// look at a delivery for an issue that is a sub-issue. If its payload
	// contains parent_issue_url, set this to true and Receive will stop
	// making those requests.
	TrustIssuePayloads bool

	// APIURL and WebURL are where GitHub's API and its web pages live. They
	// default to "https://api.github.com" and "https://github.com"; set
	// both for a GitHub Enterprise Server.
	APIURL string
	WebURL string

	// HTTPClient makes the API requests; nil means http.DefaultClient.
	HTTPClient *http.Client

	// UserAgent is how API requests identify their sender. GitHub refuses
	// requests that do not say, and asks that the value name the
	// application. Empty means "grafsol".
	UserAgent string
}

// Source is the GitHub connector. Create one with New.
type Source struct {
	repositories []string        // lower-case "owner/name", in configured order
	covered      map[string]bool // the same, for looking up
	token        string
	secret       []byte
	apiURL       string // without a trailing slash
	webHost      string // lower-case host of the web URL
	client       *http.Client
	userAgent    string

	trustIssuePayloads bool
}

var (
	_ source.Receiver = (*Source)(nil)
	_ source.Scanner  = (*Source)(nil)
)

// sourceName is the name of this source, and so the middle part of the ID
// of every document it produces.
const sourceName = "github"

var repositoryPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// New returns a Source for the given configuration.
func New(cfg Config) (*Source, error) {
	if len(cfg.Repositories) == 0 {
		return nil, errors.New("github: Config.Repositories is empty")
	}
	s := &Source{
		covered:            make(map[string]bool, len(cfg.Repositories)),
		token:              cfg.Token,
		secret:             []byte(cfg.WebhookSecret),
		client:             cfg.HTTPClient,
		trustIssuePayloads: cfg.TrustIssuePayloads,
	}
	for _, repository := range cfg.Repositories {
		if !repositoryPattern.MatchString(repository) {
			return nil, fmt.Errorf("github: repository %q is not of the form owner/name", repository)
		}
		// GitHub treats owner and repository names without regard to case.
		// Lower case is the one spelling used in document IDs.
		repository = strings.ToLower(repository)
		if !s.covered[repository] {
			s.covered[repository] = true
			s.repositories = append(s.repositories, repository)
		}
	}
	if s.client == nil {
		s.client = http.DefaultClient
	}
	s.userAgent = cfg.UserAgent
	if s.userAgent == "" {
		s.userAgent = "grafsol"
	}

	apiURL, err := parseBaseURL(cfg.APIURL, "https://api.github.com")
	if err != nil {
		return nil, fmt.Errorf("github: Config.APIURL: %w", err)
	}
	s.apiURL = strings.TrimSuffix(apiURL.String(), "/")

	webURL, err := parseBaseURL(cfg.WebURL, "https://github.com")
	if err != nil {
		return nil, fmt.Errorf("github: Config.WebURL: %w", err)
	}
	s.webHost = strings.ToLower(webURL.Host)
	return s, nil
}

func parseBaseURL(configured, fallback string) (*url.URL, error) {
	if configured == "" {
		configured = fallback
	}
	parsed, err := url.Parse(configured)
	if err != nil {
		return nil, err
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("%q is not an http or https URL", configured)
	}
	return parsed, nil
}

// Name implements source.Source.
func (s *Source) Name() string { return sourceName }

// documentID returns the ID of issue or pull request number in a repository.
// Issues and pull requests share one series of numbers within a repository,
// so the number alone tells them apart from each other.
func documentID(repository string, number int) graph.ID {
	return graph.DocumentID(sourceName, strings.ToLower(repository)+"#"+strconv.Itoa(number))
}

// Resolve implements source.Source. It recognizes the web addresses of
// issues and pull requests,
//
//	https://github.com/acme/api/issues/42
//	https://github.com/acme/api/pull/42/files
//
// in any repository, covered by this Source or not: a link is a link,
// whether or not its target is ingested.
func (s *Source) Resolve(rawURL string) (graph.ID, bool) {
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.ToLower(parsed.Host) != s.webHost {
		return "", false
	}
	// "/acme/api/issues/42" -> ["acme", "api", "issues", "42", ...]
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) < 4 || (parts[2] != "issues" && parts[2] != "pull") {
		return "", false
	}
	repository := parts[0] + "/" + parts[1]
	number, ok := parseNumber(parts[3])
	if !ok || !repositoryPattern.MatchString(repository) {
		return "", false
	}
	return documentID(repository, number), true
}

// parseNumber reads an issue number: one to nine digits, not zero.
func parseNumber(digits string) (int, bool) {
	if len(digits) == 0 || len(digits) > 9 {
		return 0, false
	}
	number, err := strconv.Atoi(digits)
	if err != nil || number <= 0 {
		return 0, false
	}
	return number, true
}

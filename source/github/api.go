package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// perPage is how many issues are asked for at a time. GitHub allows no more
// than 100.
const perPage = 100

// apiVersion is the version of GitHub's REST API this package was written
// against, sent with every request so that GitHub keeps answering in the
// shape the package expects.
const apiVersion = "2022-11-28"

// listIssues fetches one page of a repository's issues and pull requests.
//
// The listing is ordered by creation time, oldest first. Creation time
// never changes, so an issue's place in the listing is fixed; only a
// deletion can shift the items after it. (Ordering by last update, the
// obvious alternative, would move an issue to the end every time it was
// edited and shuffle the pages under a scan in progress.)
func (s *Source) listIssues(ctx context.Context, repository string, page int) ([]issue, error) {
	query := url.Values{
		"state":     {"all"},
		"sort":      {"created"},
		"direction": {"asc"},
		"per_page":  {strconv.Itoa(perPage)},
		"page":      {strconv.Itoa(page)},
	}
	var issues []issue
	err := s.get(ctx, "/repos/"+repository+"/issues?"+query.Encode(), &issues)
	if err != nil {
		return nil, fmt.Errorf("github: listing the issues of %s, page %d: %w", repository, page, err)
	}
	return issues, nil
}

// fetchIssue fetches one issue or pull request as it is right now.
func (s *Source) fetchIssue(ctx context.Context, repository string, number int) (issue, error) {
	var item issue
	err := s.get(ctx, "/repos/"+repository+"/issues/"+strconv.Itoa(number), &item)
	if err != nil {
		return issue{}, fmt.Errorf("github: fetching %s#%d: %w", repository, number, err)
	}
	return item, nil
}

// get sends a GET request to the API and decodes the JSON answer into out.
func (s *Source) get(ctx context.Context, path string, out any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.apiURL+path, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", apiVersion)
	request.Header.Set("User-Agent", s.userAgent) // GitHub refuses requests without one
	if s.token != "" {
		request.Header.Set("Authorization", "Bearer "+s.token)
	}

	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(out); err != nil {
			return fmt.Errorf("the answer could not be decoded: %w", err)
		}
		return nil
	}
	if limited := rateLimited(response); limited != nil {
		return limited
	}
	// Quote the start of GitHub's own explanation; it is usually short and
	// to the point ("Not Found", "Bad credentials").
	explanation, _ := io.ReadAll(io.LimitReader(response.Body, 300))
	return fmt.Errorf("GitHub answered %s: %s", response.Status, explanation)
}

// RateLimitError reports that GitHub refused a request because the caller
// has used up its allowance. A scan that ends with this error can simply be
// run again after RetryAt: it resumes from its last checkpoint.
type RateLimitError struct {
	// RetryAt is when GitHub says requests will be accepted again. It is
	// the zero time if GitHub did not say.
	RetryAt time.Time
}

func (e *RateLimitError) Error() string {
	if e.RetryAt.IsZero() {
		return "GitHub's rate limit is used up"
	}
	return "GitHub's rate limit is used up until " + e.RetryAt.UTC().Format(time.RFC3339)
}

// rateLimited returns a *RateLimitError if the response is GitHub refusing
// a request over its rate limit, and nil if it is anything else.
//
// GitHub signals this with status 429 or, more often, 403, and says when to
// come back either as a number of seconds (Retry-After) or as a moment in
// time (X-RateLimit-Reset, in seconds since 1970).
func rateLimited(response *http.Response) *RateLimitError {
	retryAfter := response.Header.Get("Retry-After")
	exhausted := response.Header.Get("X-RateLimit-Remaining") == "0"
	isLimit := response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden && (exhausted || retryAfter != ""))
	if !isLimit {
		return nil
	}

	limited := &RateLimitError{}
	if seconds, err := strconv.Atoi(retryAfter); err == nil {
		limited.RetryAt = time.Now().Add(time.Duration(seconds) * time.Second)
	} else if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		limited.RetryAt = time.Unix(reset, 0)
	}
	return limited
}

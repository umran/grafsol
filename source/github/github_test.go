package github_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/umran/grafsol/graph"
	"github.com/umran/grafsol/ingest"
	"github.com/umran/grafsol/source"
	"github.com/umran/grafsol/source/github"
	"github.com/umran/grafsol/storage/memory"
)

const (
	secret = "a secret only GitHub and we know"
	token  = "test-token"
)

// --- New and Resolve ----------------------------------------------------------

func TestNewChecksItsConfiguration(t *testing.T) {
	bad := map[string]github.Config{
		"no repositories":            {},
		"not owner/name":             {Repositories: []string{"api"}},
		"too many parts":             {Repositories: []string{"acme/api/extra"}},
		"an odd character":           {Repositories: []string{"acme/a pi"}},
		"an API URL with no scheme":  {Repositories: []string{"acme/api"}, APIURL: "api.github.com"},
		"a web URL that is not http": {Repositories: []string{"acme/api"}, WebURL: "ftp://github.com"},
	}
	for name, cfg := range bad {
		if _, err := github.New(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	gh, err := github.New(github.Config{Repositories: []string{"Acme/API", "acme/api", "acme/web"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if gh.Name() != "github" {
		t.Errorf("Name = %q", gh.Name())
	}
}

func TestResolve(t *testing.T) {
	gh := newSource(t, "")

	recognized := map[string]graph.ID{
		"https://github.com/acme/api/issues/42":                  "doc:github:acme/api#42",
		"https://github.com/Acme/API/pull/7":                     "doc:github:acme/api#7",
		"https://github.com/acme/api/pull/7/files#diff-abc":      "doc:github:acme/api#7",
		"http://GitHub.com/acme/api/issues/42?notification=1":    "doc:github:acme/api#42",
		"https://github.com/someone-else/elsewhere/issues/1":     "doc:github:someone-else/elsewhere#1",
		"https://github.com/acme/api/issues/42#issuecomment-123": "doc:github:acme/api#42",
	}
	for address, want := range recognized {
		if got, ok := gh.Resolve(address); !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", address, got, ok, want)
		}
	}

	unrecognized := []string{
		"",
		"not a url",
		"https://github.com/acme/api",
		"https://github.com/acme/api/issues",
		"https://github.com/acme/api/issues/abc",
		"https://github.com/acme/api/issues/0",
		"https://github.com/acme/api/tree/main",
		"https://gitlab.com/acme/api/issues/1",
		"https://api.github.com/repos/acme/api/issues/1",
		"ftp://github.com/acme/api/issues/1",
	}
	for _, address := range unrecognized {
		if got, ok := gh.Resolve(address); ok {
			t.Errorf("Resolve(%q) = %q, want it not recognized", address, got)
		}
	}

	// A GitHub Enterprise Server lives at an address of its own.
	enterprise, err := github.New(github.Config{Repositories: []string{"acme/api"}, WebURL: "https://git.acme.test"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got, ok := enterprise.Resolve("https://git.acme.test/acme/api/issues/5"); !ok || got != "doc:github:acme/api#5" {
		t.Errorf("the enterprise address was not recognized: %q, %v", got, ok)
	}
	if _, ok := enterprise.Resolve("https://github.com/acme/api/issues/5"); ok {
		t.Errorf("github.com was recognized by a Source configured for another host")
	}
}

// --- Scan ---------------------------------------------------------------------

func TestScanListsEveryIssueOfEveryRepository(t *testing.T) {
	api := newFakeAPI(t)
	api.fill("acme/api", 103) // two pages: 100 and 3
	api.fill("acme/web", 1)
	gh := newSource(t, api.URL)

	changes := scan(t, gh, nil)
	if len(changes) != 104 {
		t.Fatalf("the scan listed %d documents, want 104", len(changes))
	}

	// Requests went out as GitHub wants them.
	for _, request := range api.requests() {
		query := request.URL.Query()
		if query.Get("state") != "all" || query.Get("sort") != "created" || query.Get("direction") != "asc" || query.Get("per_page") != "100" {
			t.Errorf("unexpected query: %s", request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("the request carried no bearer token")
		}
		if request.Header.Get("Accept") != "application/vnd.github+json" || request.Header.Get("X-GitHub-Api-Version") == "" || request.Header.Get("User-Agent") == "" {
			t.Errorf("the request lacks a header GitHub expects: %v", request.Header)
		}
	}
	wantPages := []string{"acme/api?1", "acme/api?2", "acme/web?1"}
	if got := api.pages(); !slices.Equal(got, wantPages) {
		t.Errorf("pages fetched: %v, want %v", got, wantPages)
	}

	// Only the last document of each page carries a checkpoint, and it
	// names the page to carry on from.
	wantCheckpoints := map[int]string{
		99:  `{"repository":"acme/api","page":2}`,
		102: `{"repository":"acme/web","page":1}`,
		103: `{"done":true}`,
	}
	for i, change := range changes {
		if want, ok := wantCheckpoints[i]; ok {
			if string(change.Checkpoint) != want {
				t.Errorf("document %d has checkpoint %s, want %s", i, change.Checkpoint, want)
			}
		} else if len(change.Checkpoint) != 0 {
			t.Errorf("document %d has checkpoint %s, want none", i, change.Checkpoint)
		}
	}
}

func TestScanDescribesAnIssueInFull(t *testing.T) {
	api := newFakeAPI(t)
	api.add("acme/api", sampleIssue())
	gh := newSource(t, api.URL)

	changes := scan(t, gh, nil)
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	requireSampleChange(t, changes[0])
	if err := changes[0].Validate(); err != nil {
		t.Errorf("the change does not validate: %v", err)
	}
}

func TestScanTellsPullRequestsFromIssues(t *testing.T) {
	api := newFakeAPI(t)
	issue := sampleIssue()
	pull := sampleIssue()
	pull["number"] = 43
	pull["html_url"] = "https://github.com/acme/api/pull/43"
	pull["pull_request"] = map[string]any{"url": "https://api.github.com/repos/acme/api/pulls/43"}
	api.add("acme/api", issue, pull)
	gh := newSource(t, api.URL)

	changes := scan(t, gh, nil)
	if changes[0].Document.Type != graph.TypeIssue || changes[1].Document.Type != graph.TypePullRequest {
		t.Errorf("types: %s and %s, want issue and pull_request", changes[0].Document.Type, changes[1].Document.Type)
	}
}

func TestScanResumesFromACheckpoint(t *testing.T) {
	api := newFakeAPI(t)
	api.fill("acme/api", 103)
	api.fill("acme/web", 1)
	gh := newSource(t, api.URL)

	// From the second page of the first repository.
	changes := scan(t, gh, source.Checkpoint(`{"repository":"acme/api","page":2}`))
	if len(changes) != 4 {
		t.Errorf("the resumed scan listed %d documents, want the 4 that remained", len(changes))
	}
	if got, want := api.pages(), []string{"acme/api?2", "acme/web?1"}; !slices.Equal(got, want) {
		t.Errorf("pages fetched: %v, want %v", got, want)
	}

	// From the end: nothing to do, and nothing asked of GitHub.
	api.forget()
	if changes := scan(t, gh, source.Checkpoint(`{"done":true}`)); len(changes) != 0 {
		t.Errorf("a finished scan listed %d documents when resumed", len(changes))
	}
	if len(api.pages()) != 0 {
		t.Errorf("a finished scan still asked GitHub for %v", api.pages())
	}

	// A checkpoint for a repository no longer configured: start over.
	api.forget()
	changes = scan(t, gh, source.Checkpoint(`{"repository":"acme/gone","page":5}`))
	if len(changes) != 104 {
		t.Errorf("a scan resumed from an unknown repository listed %d documents, want all 104", len(changes))
	}

	for _, err := range gh.Scan(t.Context(), source.Checkpoint("not a checkpoint")) {
		if err == nil {
			t.Errorf("a garbled checkpoint was accepted")
		}
		break
	}
}

func TestScanReportsARateLimitAsSuch(t *testing.T) {
	reset := time.Now().Add(17 * time.Minute).Truncate(time.Second)
	responses := map[string]func(w http.ResponseWriter){
		"403 with no allowance left": func(w http.ResponseWriter) {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			w.WriteHeader(http.StatusForbidden)
		},
		"429 with Retry-After": func(w http.ResponseWriter) {
			w.Header().Set("Retry-After", "1020")
			w.WriteHeader(http.StatusTooManyRequests)
		},
	}
	for name, respond := range responses {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { respond(w) }))
		gh := newSource(t, server.URL)

		var limited *github.RateLimitError
		for _, err := range gh.Scan(t.Context(), nil) {
			if !errors.As(err, &limited) {
				t.Errorf("%s: the scan ended with %v, want a *RateLimitError", name, err)
			}
			break
		}
		if limited != nil && limited.RetryAt.Sub(reset).Abs() > 5*time.Second {
			t.Errorf("%s: RetryAt is %v, want about %v", name, limited.RetryAt, reset)
		}
		server.Close()
	}
}

func TestScanReportsOtherFailuresWithoutLeakingTheToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// A plain 403: forbidden, not rate limited.
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"Resource not accessible by personal access token"}`)
	}))
	defer server.Close()
	gh := newSource(t, server.URL)

	for _, err := range gh.Scan(t.Context(), nil) {
		if err == nil {
			t.Fatalf("the scan yielded a document from a failing API")
		}
		var limited *github.RateLimitError
		if errors.As(err, &limited) {
			t.Errorf("a plain 403 was reported as a rate limit")
		}
		if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "Resource not accessible") {
			t.Errorf("the error does not say what GitHub said: %v", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("the error leaks the token: %v", err)
		}
		break
	}
}

func TestScanStopsWhenAskedTo(t *testing.T) {
	api := newFakeAPI(t)
	api.fill("acme/api", 250)
	gh := newSource(t, api.URL)

	// Breaking out early fetches no more pages than were needed.
	seen := 0
	for _, err := range gh.Scan(t.Context(), nil) {
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		if seen++; seen == 5 {
			break
		}
	}
	if got := api.pages(); len(got) != 1 {
		t.Errorf("after stopping within the first page, %d pages had been fetched", len(got))
	}

	// An ended context ends the scan with an error, not a quiet stop: a
	// scan that ends quietly is taken as complete.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	var failure error
	for _, err := range gh.Scan(ctx, nil) {
		failure = err
	}
	if failure == nil {
		t.Errorf("a scan with an ended context ended without an error")
	}
}

// --- Receive ------------------------------------------------------------------

// GitHub publishes a test vector for checking a signature implementation:
// docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries
func TestReceiveAcceptsGitHubsPublishedSignatureExample(t *testing.T) {
	gh, err := github.New(github.Config{
		Repositories:  []string{"acme/api"},
		WebhookSecret: "It's a Secret to Everybody",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	delivery := source.Delivery{
		Header: http.Header{
			"X-Github-Event":      {"ping"},
			"X-Hub-Signature-256": {"sha256=757107ea0eb2509fc211221cce984b8a37570b6d7586c22c46f4379c8b043e17"},
		},
		Body: []byte("Hello, World!"),
	}
	if _, err := gh.Receive(t.Context(), delivery); err != nil {
		t.Errorf("GitHub's own example was rejected: %v", err)
	}

	delivery.Body = []byte("Hello, World?")
	if _, err := gh.Receive(t.Context(), delivery); !errors.Is(err, source.ErrUnauthorized) {
		t.Errorf("a tampered body was answered with %v, want ErrUnauthorized", err)
	}
}

func TestReceiveRejectsWhatItCannotAuthenticate(t *testing.T) {
	gh := newSource(t, "")
	body := eventBody("opened", "issue", sampleIssue(), "acme/api")

	rejected := map[string]http.Header{
		"no signature":                   {"X-Github-Event": {"issues"}},
		"a signature of another body":    signedHeader("issues", []byte("something else")),
		"only the old SHA-1 signature":   {"X-Github-Event": {"issues"}, "X-Hub-Signature": {"sha1=abc"}},
		"a signature that is not hex":    {"X-Github-Event": {"issues"}, "X-Hub-Signature-256": {"sha256=not-hex"}},
		"a signature without its prefix": {"X-Github-Event": {"issues"}, "X-Hub-Signature-256": {signature(body)}},
	}
	for name, header := range rejected {
		changes, err := gh.Receive(t.Context(), source.Delivery{Header: header, Body: body})
		if !errors.Is(err, source.ErrUnauthorized) || len(changes) != 0 {
			t.Errorf("%s: got %d changes and error %v, want ErrUnauthorized", name, len(changes), err)
		}
	}

	// Without a secret there is nothing to check a signature against, so
	// nothing can be trusted, not even a delivery with a valid one.
	unconfigured, err := github.New(github.Config{Repositories: []string{"acme/api"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := unconfigured.Receive(t.Context(), source.Delivery{Header: signedHeader("issues", body), Body: body}); !errors.Is(err, source.ErrUnauthorized) {
		t.Errorf("a Source without a secret accepted a delivery: %v", err)
	}
}

func TestReceiveTurnsAnIssueEventIntoAChange(t *testing.T) {
	gh := newSource(t, "")

	for _, action := range []string{"opened", "edited", "labeled", "closed", "reopened"} {
		changes := receive(t, gh, "issues", eventBody(action, "issue", sampleIssue(), "Acme/API"))
		if len(changes) != 1 {
			t.Fatalf("%s: got %d changes, want 1", action, len(changes))
		}
		requireSampleChange(t, changes[0])
	}
}

func TestReceiveTurnsRemovalsIntoTombstones(t *testing.T) {
	gh := newSource(t, "")

	for _, action := range []string{"deleted", "transferred"} {
		changes := receive(t, gh, "issues", eventBody(action, "issue", sampleIssue(), "acme/api"))
		if len(changes) != 1 {
			t.Fatalf("%s: got %d changes, want 1", action, len(changes))
		}
		change := changes[0]
		if !change.Deleted || change.Document.ID != "doc:github:acme/api#42" || change.Version == 0 {
			t.Errorf("%s: got %+v, want a removal of #42 with a version", action, change)
		}
		if err := change.Validate(); err != nil {
			t.Errorf("%s: the removal does not validate: %v", action, err)
		}
	}
}

func TestReceiveHandlesPullRequestEvents(t *testing.T) {
	// No API address is configured: a pull request event must never need one.
	gh := newSource(t, "")

	pull := sampleIssue()
	pull["number"] = 43
	pull["html_url"] = "https://github.com/acme/api/pull/43"
	delete(pull, "parent_issue_url") // the pull request object has no such field

	changes := receive(t, gh, "pull_request", eventBody("opened", "pull_request", pull, "acme/api"))
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	if changes[0].Document.ID != "doc:github:acme/api#43" || changes[0].Document.Type != graph.TypePullRequest {
		t.Errorf("got %s of type %s", changes[0].Document.ID, changes[0].Document.Type)
	}
}

// GitHub names an issue's parent only when there is one, so a payload that
// names none might mean "no parent" or might mean that webhooks leave the
// field out. Taking it for "no parent" wrongly would remove a child_of edge
// that is still true, so by default the issue is read from the API instead.
func TestReceiveReadsAnIssueAfreshWhenItsPayloadNamesNoParent(t *testing.T) {
	api := newFakeAPI(t)
	api.add("acme/api", sampleIssue()) // the API knows the parent
	gh := newSource(t, api.URL)

	silent := sampleIssue()
	delete(silent, "parent_issue_url")
	silent["title"] = "a title that is already out of date"
	body := eventBody("edited", "issue", silent, "acme/api")

	changes := receive(t, gh, "issues", body)
	if len(changes) != 1 {
		t.Fatalf("got %d changes, want 1", len(changes))
	}
	// The change describes what the API returned, parent included.
	requireSampleChange(t, changes[0])
	if got := len(api.requests()); got != 1 {
		t.Errorf("the API was asked %d times, want once", got)
	}

	// A payload that does name a parent leaves nothing to ask.
	api.forget()
	changes = receive(t, gh, "issues", eventBody("edited", "issue", sampleIssue(), "acme/api"))
	requireSampleChange(t, changes[0])
	if len(api.requests()) != 0 {
		t.Errorf("the API was asked although the payload named the parent")
	}

	// Told that payloads can be trusted, the Source takes a payload that
	// names no parent at its word, and asks nothing.
	trusting, err := github.New(github.Config{
		Repositories:       []string{"acme/api"},
		WebhookSecret:      secret,
		APIURL:             api.URL,
		TrustIssuePayloads: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	changes = receive(t, trusting, "issues", body)
	if len(api.requests()) != 0 {
		t.Errorf("a trusting Source still asked the API")
	}
	if changes[0].Document.Title != "a title that is already out of date" {
		t.Errorf("a trusting Source did not use the payload: %+v", changes[0].Document)
	}
	for _, edge := range changes[0].Edges {
		if edge.Kind == graph.ChildOf {
			t.Errorf("an issue whose trusted payload names no parent got a child_of edge: %+v", edge)
		}
	}
}

func TestReceiveIgnoresWhatItDoesNotTrack(t *testing.T) {
	gh := newSource(t, "")

	ignored := map[string]struct {
		kind string
		body []byte
	}{
		"a ping":                         {"ping", []byte(`{"zen":"Keep it logically awesome.","hook_id":1}`)},
		"a comment":                      {"issue_comment", eventBody("created", "issue", sampleIssue(), "acme/api")},
		"a push":                         {"push", []byte(`{"ref":"refs/heads/main"}`)},
		"an issue of another repository": {"issues", eventBody("opened", "issue", sampleIssue(), "acme/elsewhere")},
	}
	for name, delivery := range ignored {
		changes, err := gh.Receive(t.Context(), source.Delivery{Header: signedHeader(delivery.kind, delivery.body), Body: delivery.body})
		if err != nil || len(changes) != 0 {
			t.Errorf("%s: got %d changes and error %v, want it quietly ignored", name, len(changes), err)
		}
	}
}

func TestReceiveRejectsPayloadsItCannotRead(t *testing.T) {
	gh := newSource(t, "")

	malformed := map[string][]byte{
		"not JSON":      []byte("{ this is not JSON"),
		"no issue":      []byte(`{"action":"opened","repository":{"full_name":"acme/api"}}`),
		"no repository": []byte(`{"action":"opened","issue":{"number":42}}`),
		"no number":     []byte(`{"action":"opened","issue":{"title":"x"},"repository":{"full_name":"acme/api"}}`),
	}
	for name, body := range malformed {
		_, err := gh.Receive(t.Context(), source.Delivery{Header: signedHeader("issues", body), Body: body})
		if !errors.Is(err, source.ErrMalformed) {
			t.Errorf("%s: got error %v, want ErrMalformed", name, err)
		}
	}
}

func TestReceiveReadsFormEncodedDeliveries(t *testing.T) {
	gh := newSource(t, "")

	payload := eventBody("opened", "issue", sampleIssue(), "acme/api")
	body := []byte(url.Values{"payload": {string(payload)}}.Encode())
	header := signedHeader("issues", body) // the signature covers the body as sent
	header.Set("Content-Type", "application/x-www-form-urlencoded")

	changes, err := gh.Receive(t.Context(), source.Delivery{Header: header, Body: body})
	if err != nil || len(changes) != 1 {
		t.Fatalf("got %d changes and error %v", len(changes), err)
	}
	requireSampleChange(t, changes[0])
}

// --- with the sync engine -----------------------------------------------------

// A webhook and a scan describe the same issue through different routes. If
// their descriptions differed in any way, every scan would rewrite (and
// re-enrich) every document a webhook had touched. They must agree exactly.
func TestWebhookAndScanAgree(t *testing.T) {
	api := newFakeAPI(t)
	api.add("acme/api", sampleIssue())
	gh := newSource(t, api.URL)

	store, ledger := memory.New(), memory.NewLedger()
	syncer := ingest.NewSyncer(ingest.Config{
		Store:   store,
		Ledger:  ledger,
		Sources: []source.Source{gh},
		Logger:  slog.New(slog.DiscardHandler),
	})

	// The issue arrives by webhook, through the HTTP handler...
	hook := httptest.NewServer(syncer.Handler(gh))
	defer hook.Close()
	body := eventBody("opened", "issue", sampleIssue(), "acme/api")
	request, _ := http.NewRequest(http.MethodPost, hook.URL, strings.NewReader(string(body)))
	request.Header = signedHeader("issues", body)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("delivering the webhook: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("the webhook was answered with %d, want 204", response.StatusCode)
	}
	stored, err := store.GetVertices(t.Context(), []graph.ID{"doc:github:acme/api#42", "topic:bug"})
	if err != nil || len(stored) != 2 {
		t.Fatalf("after the webhook the graph holds %d of the 2 expected vertices (error %v)", len(stored), err)
	}

	// ...and a scan then finds nothing to change.
	report, err := syncer.Reconcile(t.Context(), gh)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if report != (ingest.Report{Unchanged: 1}) {
		t.Errorf("the scan after the webhook reported %+v, want exactly one unchanged document", report)
	}

	// A forged delivery gets nowhere.
	forged, _ := http.NewRequest(http.MethodPost, hook.URL, strings.NewReader(string(body)))
	forged.Header.Set("X-GitHub-Event", "issues")
	forged.Header.Set("X-Hub-Signature-256", "sha256="+strings.Repeat("0", 64))
	response, err = http.DefaultClient.Do(forged)
	if err != nil {
		t.Fatalf("delivering the forged webhook: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("a forged webhook was answered with %d, want 401", response.StatusCode)
	}
}

// --- helpers ------------------------------------------------------------------

// newSource returns a Source covering acme/api and acme/web whose API
// requests go to apiURL.
func newSource(t *testing.T, apiURL string) *github.Source {
	t.Helper()
	gh, err := github.New(github.Config{
		Repositories:  []string{"acme/api", "acme/web"},
		Token:         token,
		WebhookSecret: secret,
		APIURL:        apiURL,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return gh
}

func scan(t *testing.T, gh *github.Source, resume source.Checkpoint) []source.Change {
	t.Helper()
	var changes []source.Change
	for change, err := range gh.Scan(t.Context(), resume) {
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		changes = append(changes, change)
	}
	return changes
}

func receive(t *testing.T, gh *github.Source, kind string, body []byte) []source.Change {
	t.Helper()
	changes, err := gh.Receive(t.Context(), source.Delivery{Header: signedHeader(kind, body), Body: body})
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	return changes
}

// signature signs a body the way GitHub does, without the "sha256=" prefix.
func signature(body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// signedHeader returns the headers of an authentic delivery of body.
func signedHeader(kind string, body []byte) http.Header {
	header := http.Header{}
	header.Set("X-GitHub-Event", kind)
	header.Set("X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958")
	header.Set("X-Hub-Signature-256", "sha256="+signature(body))
	header.Set("Content-Type", "application/json")
	return header
}

// eventBody builds a webhook payload: {"action", <field>: item, "repository"}.
func eventBody(action, field string, item map[string]any, repository string) []byte {
	body, err := json.Marshal(map[string]any{
		"action":     action,
		field:        item,
		"repository": map[string]any{"full_name": repository},
		"sender":     map[string]any{"login": "octocat"},
	})
	if err != nil {
		panic(err)
	}
	return body
}

// sampleIssue returns issue #42 of acme/api as GitHub's API describes one:
// labelled, a sub-issue of #7, and with a body that points at several things.
func sampleIssue() map[string]any {
	return map[string]any{
		"number":   42,
		"html_url": "https://github.com/acme/api/issues/42",
		"title":    "Refunds time out",
		"body": "Refunds fail after 30s, see #12 and acme/web#3.\n\n" +
			"Tracked in https://acme.atlassian.net/browse/PAY-7 and discussed in https://github.com/acme/api/pull/40.",
		"state": "open",
		"labels": []any{
			map[string]any{"id": 1, "name": "Bug", "description": "Something is not working", "color": "d73a4a"},
			map[string]any{"id": 2, "name": "area/billing", "description": nil},
		},
		"created_at":       "2026-03-14T09:26:53Z",
		"updated_at":       "2026-06-28T16:03:07Z",
		"parent_issue_url": "https://api.github.com/repos/acme/api/issues/7",
		"user":             map[string]any{"login": "octocat"},
	}
}

// requireSampleChange fails the test unless a change describes sampleIssue
// exactly.
func requireSampleChange(t *testing.T, change source.Change) {
	t.Helper()
	const id = graph.ID("doc:github:acme/api#42")
	updated := time.Date(2026, time.June, 28, 16, 3, 7, 0, time.UTC)

	document := change.Document
	if document.ID != id || document.Type != graph.TypeIssue || document.Source != "github" ||
		document.URL != "https://github.com/acme/api/issues/42" || document.Title != "Refunds time out" ||
		!document.CreatedAt.Equal(time.Date(2026, time.March, 14, 9, 26, 53, 0, time.UTC)) || !document.UpdatedAt.Equal(updated) {
		t.Errorf("the document is %+v", document)
	}
	if change.Deleted || change.Version != uint64(updated.UnixNano()) {
		t.Errorf("Deleted = %v and Version = %d, want a live document at version %d", change.Deleted, change.Version, updated.UnixNano())
	}
	if !strings.HasPrefix(change.Content, "Refunds time out\n\nRefunds fail after 30s") {
		t.Errorf("the content is %q", change.Content)
	}

	firstLine := "Refunds fail after 30s, see #12 and acme/web#3."
	secondLine := "Tracked in https://acme.atlassian.net/browse/PAY-7 and discussed in https://github.com/acme/api/pull/40."
	wantEdges := []graph.Edge{
		{From: id, Kind: graph.TaggedWith, To: "topic:bug"},
		{From: id, Kind: graph.TaggedWith, To: "topic:area/billing"},
		{From: id, Kind: graph.ChildOf, To: "doc:github:acme/api#7", Summary: "sub-issue of"},
		{From: id, Kind: graph.LinksTo, To: "doc:github:acme/api#12", Summary: firstLine},
		{From: id, Kind: graph.LinksTo, To: "doc:github:acme/web#3", Summary: firstLine},
		{From: id, Kind: graph.LinksTo, To: "doc:github:acme/api#40", Summary: secondLine}, // a GitHub link, resolved on the spot
	}
	if !slices.Equal(change.Edges, wantEdges) {
		t.Errorf("the edges are\n  %+v\nwant\n  %+v", change.Edges, wantEdges)
	}

	wantTopics := []graph.Topic{
		{ID: "topic:bug", Name: "Bug", Summary: "Something is not working"},
		{ID: "topic:area/billing", Name: "area/billing"},
	}
	if !slices.EqualFunc(change.Topics, wantTopics, func(a, b graph.Topic) bool {
		return a.ID == b.ID && a.Name == b.Name && a.Summary == b.Summary
	}) {
		t.Errorf("the topics are %+v, want %+v", change.Topics, wantTopics)
	}

	// The link into Jira is not GitHub's to recognize; it is passed on.
	wantLinks := []source.Link{{URL: "https://acme.atlassian.net/browse/PAY-7", Context: secondLine}}
	if !slices.Equal(change.Links, wantLinks) {
		t.Errorf("the links are %+v, want %+v", change.Links, wantLinks)
	}
}

// fakeAPI is a stand-in for GitHub's REST API that serves issues from memory
// and remembers what it was asked.
type fakeAPI struct {
	*httptest.Server

	mu     sync.Mutex
	issues map[string][]map[string]any // by repository, in creation order
	seen   []*http.Request
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	api := &fakeAPI{issues: make(map[string][]map[string]any)}
	api.Server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.Close)
	return api
}

// add appends issues to a repository.
func (a *fakeAPI) add(repository string, issues ...map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.issues[repository] = append(a.issues[repository], issues...)
}

// fill gives a repository count plain issues, numbered from 1.
func (a *fakeAPI) fill(repository string, count int) {
	for number := 1; number <= count; number++ {
		a.add(repository, map[string]any{
			"number":     number,
			"html_url":   fmt.Sprintf("https://github.com/%s/issues/%d", repository, number),
			"title":      fmt.Sprintf("Issue %d", number),
			"body":       nil,
			"labels":     []any{},
			"created_at": "2026-01-01T00:00:00Z",
			"updated_at": "2026-01-02T00:00:00Z",
			// No parent_issue_url: GitHub leaves the field out of an issue
			// that is nobody's sub-issue.
		})
	}
}

// serve answers the two requests the connector makes:
//
//	GET /repos/{owner}/{name}/issues?page=N&per_page=M
//	GET /repos/{owner}/{name}/issues/{number}
func (a *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = append(a.seen, r)

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "repos" || parts[3] != "issues" {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	issues := a.issues[parts[1]+"/"+parts[2]]

	if len(parts) == 5 { // one issue
		number, _ := strconv.Atoi(parts[4])
		for _, issue := range issues {
			if issue["number"] == number {
				json.NewEncoder(w).Encode(issue)
				return
			}
		}
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	start := min((page-1)*perPage, len(issues))
	end := min(start+perPage, len(issues))
	json.NewEncoder(w).Encode(issues[start:end])
}

func (a *fakeAPI) requests() []*http.Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.seen)
}

// pages lists the listing requests received, as "owner/name?page".
func (a *fakeAPI) pages() []string {
	var pages []string
	for _, request := range a.requests() {
		parts := strings.Split(strings.Trim(request.URL.Path, "/"), "/")
		if len(parts) == 4 {
			pages = append(pages, parts[1]+"/"+parts[2]+"?"+request.URL.Query().Get("page"))
		}
	}
	return pages
}

// forget clears the record of requests received.
func (a *fakeAPI) forget() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seen = nil
}

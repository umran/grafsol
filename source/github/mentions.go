package github

import (
	"regexp"
	"strings"

	"github.com/umran/grafsol/graph"
)

// reference is one thing the text of an issue points at.
type reference struct {
	target  graph.ID // an issue or pull request, for "#12" and "acme/web#12"
	url     string   // set instead of target when the reference is a URL
	context string   // the line the reference was found on
}

var (
	// A URL runs until whitespace or a character that Markdown wraps
	// around links.
	urlPattern = regexp.MustCompile("https?://[^\\s<>\\[\\]()\"'`]+")

	// "#12" or "acme/web#12". The first group is the character before the
	// mention, which must not make it part of a longer word or path: that
	// keeps "abc#12" and the fragment of a URL from counting.
	mentionPattern = regexp.MustCompile(`(^|[^\w/#&])([\w.-]+/[\w.-]+)?#(\d+)\b`)
)

// maxContext bounds how much of a line is kept as an edge's summary.
const maxContext = 240

// references finds what a piece of GitHub Markdown points at: mentions of
// issues and pull requests, and URLs. repository is where the text lives,
// which is what a bare "#12" refers to.
//
// Lines inside fenced code blocks are skipped: there a "#12" is far more
// likely a comment or a colour than a mention. Beyond that the text is not
// parsed as Markdown, so the occasional false mention gets through. It
// becomes an edge to a document that does not exist, which is harmless.
func references(repository, text string) []reference {
	var found []reference
	fenced := false
	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		if fenced || trimmed == "" {
			continue
		}
		context := shorten(trimmed, maxContext)

		for _, url := range urlPattern.FindAllString(line, -1) {
			// Punctuation that ends a sentence is not part of the link.
			url = strings.TrimRight(url, ".,;:!?")
			found = append(found, reference{url: url, context: context})
		}

		// Blank out the URLs before looking for mentions, so that the
		// "#issuecomment-12" at the end of a link is not taken for one.
		withoutURLs := urlPattern.ReplaceAllStringFunc(line, func(url string) string {
			return strings.Repeat(" ", len(url))
		})
		for _, match := range mentionPattern.FindAllStringSubmatch(withoutURLs, -1) {
			mentioned := match[2]
			if mentioned == "" {
				mentioned = repository
			}
			number, ok := parseNumber(match[3])
			if !ok {
				continue
			}
			found = append(found, reference{target: documentID(mentioned, number), context: context})
		}
	}
	return found
}

// shorten cuts text to at most limit characters, marking the cut.
func shorten(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

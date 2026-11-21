package mapping

import (
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/umran/grafsol/graph"
)

// This file reads the line a reference was found on: where the reference
// stands in it, whether it stands alone, and what comes before it.

// span is where something stands in a line: the bytes from start up to end.
type span struct{ start, end int }

var (
	// What a reference looks like when all there is to go on is the text:
	// an address, or a number after a "#", with or without "owner/name"
	// before it.
	addressPattern = regexp.MustCompile("https?://[^\\s<>\\[\\]()\"'`]+")
	numberPattern  = regexp.MustCompile(`(?:[\w.-]+/[\w.-]+)?#\d+`)
)

// joiners are the words that may stand between a word and the reference it
// governs, or between the references of a list: "fixes the issue #12",
// "closes #2, #3 and #4".
var joiners = map[string]bool{
	"and": true, "or": true, "also": true, "both": true, "plus": true,
	"the": true, "this": true, "these": true,
	"issue": true, "issues": true, "pr": true, "prs": true, "pull": true, "request": true, "requests": true,
	"bug": true, "bugs": true, "ticket": true, "tickets": true, "task": true, "tasks": true,
}

// betweenWords splits what stands between a word and a reference into the
// words it holds, dropping the punctuation that lists and Markdown bring.
func betweenWords(text string) []string {
	return strings.FieldsFunc(text, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",;:&+()[]*~`", r)
	})
}

// locate finds where a reference stands in the line it was found on. It
// returns nil when what was kept of the line does not hold the reference:
// a line is cut short when it is long, and some references were never
// written in text at all.
func locate(reference graph.Reference) *span {
	line := reference.Context
	if reference.Written != "" {
		return find(line, reference.Written)
	}
	// A graph made before references kept their own text has only the line
	// and the target. Look for something in the line that names the target.
	_, key, _ := strings.Cut(reference.Target.Key(), ":") // "acme/api#19"
	if key == "" {
		return nil
	}
	owner, number, numbered := cutLast(key, "#")
	if !numbered {
		return find(line, key)
	}
	for _, found := range numberPattern.FindAllStringIndex(line, -1) {
		written := line[found[0]:found[1]]
		within, n, _ := cutLast(written, "#")
		if n == number && (within == "" || strings.EqualFold(within, owner)) && standsApart(line, found[0], found[1]) {
			return &span{found[0], found[1]}
		}
	}
	for _, found := range addressPattern.FindAllStringIndex(line, -1) {
		address := strings.ToLower(line[found[0]:found[1]])
		if strings.Contains(address, strings.ToLower(owner)+"/") && endsWithNumber(address, number) {
			return &span{found[0], found[1]}
		}
	}
	return nil
}

// find returns the first place in the line where the text stands apart from
// the words around it, so that "#1" is not found inside "#12" or "web#1".
func find(line, text string) *span {
	for from := 0; from <= len(line)-len(text); {
		at := strings.Index(line[from:], text)
		if at < 0 {
			return nil
		}
		start, end := from+at, from+at+len(text)
		if standsApart(line, start, end) {
			return &span{start, end}
		}
		from = start + 1
	}
	return nil
}

// standsApart reports whether what stands at [start, end) in the line is a
// whole reference and not a piece of a longer one.
func standsApart(line string, start, end int) bool {
	if before, size := utf8.DecodeLastRuneInString(line[:start]); size > 0 {
		if isWordRune(before) || strings.ContainsRune("/#&", before) {
			return false
		}
	}
	if after, size := utf8.DecodeRuneInString(line[end:]); size > 0 && isWordRune(after) {
		return false
	}
	return true
}

// endsWithNumber reports whether an address's path ends in the number, as
// the address of issue 1287 ends in "/1287", whatever follows the path.
func endsWithNumber(address, number string) bool {
	path, _, _ := strings.Cut(address, "#")
	path, _, _ = strings.Cut(path, "?")
	return strings.HasSuffix(strings.TrimRight(path, "/.,;:!"), "/"+number)
}

// cutLast cuts text around the last separator in it.
func cutLast(text, separator string) (before, after string, found bool) {
	at := strings.LastIndex(text, separator)
	if at < 0 {
		return text, "", false
	}
	return text[:at], text[at+len(separator):], true
}

// alone reports whether the line holds the reference and nothing else.
func alone(line string, at span) bool {
	return strings.TrimSpace(line[:at.start]) == "" && strings.TrimSpace(line[at.end:]) == ""
}

// after reports whether one of the words comes before the reference on its
// line with nothing between them but other references and joining words. It
// returns the word as the line has it.
//
// others is every reference of the document: the ones on the same line are
// what may stand in between.
func after(line string, at span, words []string, others []graph.Reference) (string, bool) {
	before := line[:at.start]
	blanked := blankReferences(before, line, others)
	for _, word := range words {
		for _, found := range wordPattern(word).FindAllStringIndex(before, -1) {
			start, end := found[0], found[1]
			if wholeWord(before, start, end) && onlyJoiners(blanked[end:]) {
				return before[start:end], true
			}
		}
	}
	return "", false
}

// blankReferences returns the text with every reference in it turned to
// spaces: the document's other references on the same line, and anything
// else that reads as a reference.
func blankReferences(text, line string, others []graph.Reference) string {
	blanked := []byte(text)
	blank := func(start, end int) {
		for i := start; i < end && i < len(blanked); i++ {
			blanked[i] = ' '
		}
	}
	for _, other := range others {
		if other.Context != line {
			continue
		}
		if at := locate(other); at != nil {
			blank(at.start, at.end)
		}
	}
	for _, pattern := range []*regexp.Regexp{addressPattern, numberPattern} {
		for _, found := range pattern.FindAllStringIndex(text, -1) {
			blank(found[0], found[1])
		}
	}
	return string(blanked)
}

// onlyJoiners reports whether the text holds nothing but joining words.
func onlyJoiners(text string) bool {
	for _, word := range betweenWords(text) {
		if !joiners[strings.ToLower(word)] {
			return false
		}
	}
	return true
}

// wholeWord reports whether what stands at [start, end) is a word of its
// own: "fixes", and not the end of "prefixes" or the start of "fixes-up".
func wholeWord(text string, start, end int) bool {
	if before, size := utf8.DecodeLastRuneInString(text[:start]); size > 0 && (isWordRune(before) || before == '-') {
		return false
	}
	if after, size := utf8.DecodeRuneInString(text[end:]); size > 0 && (isWordRune(after) || after == '-') {
		return false
	}
	return true
}

func isWordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

// wordPatterns keeps the pattern for each word a rule has asked for.
var wordPatterns sync.Map // string -> *regexp.Regexp

// wordPattern returns the pattern that finds a rule's word in a line:
// whatever the case of its letters, and however its own words are spaced.
func wordPattern(word string) *regexp.Regexp {
	if kept, ok := wordPatterns.Load(word); ok {
		return kept.(*regexp.Regexp)
	}
	parts := strings.Fields(word)
	for i, part := range parts {
		parts[i] = regexp.QuoteMeta(part)
	}
	pattern := regexp.MustCompile(`(?i)` + strings.Join(parts, `\s+`))
	wordPatterns.Store(word, pattern)
	return pattern
}

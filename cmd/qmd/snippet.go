package main

import (
	"sort"
	"strings"
	"unicode"
)

// Snippets show the passage a hit came from, not the top of the file. The
// anchor is either the offset of the chunk whose vector matched, or, for a
// full-text hit, the first occurrence of the longest query term.
//
// Both are characters. The line number is derived from the document so an agent
// can ask for the lines around the hit with `get file:line`.

// queryTerms returns the searchable words of a query, longest first, so the
// most specific one anchors the snippet.
func queryTerms(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-'
	})
	seen := make(map[string]bool, len(fields))
	terms := make([]string, 0, len(fields))
	for _, f := range fields {
		if len(f) < 3 || seen[f] {
			continue
		}
		seen[f] = true
		terms = append(terms, f)
	}
	sort.SliceStable(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	return terms
}

// anchorFor picks where a centered snippet starts. A chunk offset wins because
// it is where the embedding matched; otherwise the longest query term that
// appears in the body wins. Zero means "no anchor": show the head.
func anchorFor(body, query string, chunkPos int) int {
	if chunkPos > 0 && chunkPos < len(body) {
		return chunkPos
	}
	if body == "" {
		return 0
	}
	lower := strings.ToLower(body)
	for _, term := range queryTerms(query) {
		if i := strings.Index(lower, term); i >= 0 {
			return i
		}
	}
	return 0
}

// centeredSnippet returns at most maxLen characters around anchor, starting at a
// line boundary, plus the 1-based line that excerpt starts on. An ellipsis marks
// a cut at either end.
func centeredSnippet(body string, anchor, maxLen int) (string, int) {
	if body == "" || maxLen <= 0 {
		return "", 1
	}
	if len(body) <= maxLen {
		return body, 1
	}
	if anchor >= len(body) {
		anchor = len(body) - 1
	}
	if anchor < 0 {
		anchor = 0
	}
	lead := maxLen / 3
	start := anchor - lead
	if start < 0 {
		start = 0
	}
	// Start at the beginning of the line so the excerpt reads as the file does.
	if nl := strings.LastIndexByte(body[:start], '\n'); nl >= 0 {
		start = nl + 1
	}
	end := start + maxLen
	if end > len(body) {
		end = len(body)
	}
	line := 1 + strings.Count(body[:start], "\n")
	text := body[start:end]
	if start > 0 {
		text = "…" + text
	}
	if end < len(body) {
		text += "…"
	}
	return text, line
}

package main

import (
	"strings"
	"testing"
)

func TestQueryTermsLongestFirst(t *testing.T) {
	got := queryTerms("How does the Embedding Model work, embedding?")
	if len(got) == 0 || got[0] != "embedding" {
		t.Fatalf("terms = %v, want the longest term first", got)
	}
	for _, term := range got {
		if len(term) < 3 {
			t.Errorf("short term survived: %q", term)
		}
	}
}

func TestAnchorForPrefersChunkThenTerm(t *testing.T) {
	body := strings.Repeat("x", 100) + "needle in a haystack"

	if got := anchorFor(body, "needle", 0); got != 100 {
		t.Errorf("term anchor = %d, want 100", got)
	}
	if got := anchorFor(body, "needle", 40); got != 40 {
		t.Errorf("chunk anchor = %d, want 40", got)
	}
	if got := anchorFor(body, "absent words", 0); got != 0 {
		t.Errorf("anchor without a match = %d, want 0", got)
	}
	if got := anchorFor(body, "needle", len(body)+10); got != 100 {
		t.Errorf("out-of-range chunk anchor = %d, want the term anchor", got)
	}
}

func TestCenteredSnippetCutsAroundTheAnchor(t *testing.T) {
	body := strings.Repeat("filler line with nothing on it\n", 200) +
		"the needle sits here\n" +
		strings.Repeat("tail line\n", 100)
	anchor := strings.Index(body, "needle")

	text, line := centeredSnippet(body, anchor, 200)
	if !strings.Contains(text, "needle sits here") {
		t.Fatalf("excerpt lost the anchor:\n%s", text)
	}
	if !strings.HasPrefix(text, "…") || !strings.HasSuffix(text, "…") {
		t.Errorf("excerpt is not marked as cut at both ends:\n%s", text)
	}
	if line <= 1 {
		t.Errorf("start line = %d, want the real line of the anchor", line)
	}
	if got := 1 + strings.Count(body[:strings.Index(body, "the needle sits here")], "\n"); line > got {
		t.Errorf("start line %d is past the anchor line %d", line, got)
	}
}

func TestCenteredSnippetHeadWhenNoAnchor(t *testing.T) {
	body := strings.Repeat("a", 1000)
	text, line := centeredSnippet(body, 0, 100)
	if line != 1 || len(text) == 0 || !strings.HasSuffix(text, "…") {
		t.Errorf("head fallback = %q (line %d)", text, line)
	}
}

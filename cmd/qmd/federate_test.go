package main

import (
	"strings"
	"testing"
)

func TestFuseListsKeepsSingleIndexScores(t *testing.T) {
	one := []fusedHit{{Index: "public", Filepath: "qmd://pub/a.md", Score: 7.5}}
	got := fuseLists([][]fusedHit{one})
	if len(got) != 1 || got[0].Score != 7.5 {
		t.Fatalf("single list was altered: %+v", got)
	}
}

func TestFuseListsMergesByRankAcrossIndexes(t *testing.T) {
	pub := []fusedHit{
		{Index: "public", Filepath: "qmd://pub/a.md"},
		{Index: "public", Filepath: "qmd://pub/b.md"},
	}
	priv := []fusedHit{
		{Index: "private", Filepath: "qmd://priv/b.md"},
		{Index: "private", Filepath: "qmd://pub/a.md"},
	}
	got := fuseLists([][]fusedHit{pub, priv})
	// Four entries: the same path under two indexes is two documents, so the
	// index belongs in the key.
	if len(got) != 4 {
		t.Fatalf("got %d hits, want 4: %+v", len(got), got)
	}
	// Both rank-one entries tie at the normalized top.
	if got[0].Score != 1.0 || got[1].Score != 1.0 {
		t.Errorf("winner scores = %v, %v; want 1.0 each", got[0].Score, got[1].Score)
	}
	seen := make(map[string]bool)
	for _, h := range got {
		if h.Index == "" {
			t.Errorf("hit lost its index: %+v", h)
		}
		seen[h.Index+"|"+h.Filepath] = true
	}
	if len(seen) != 4 {
		t.Errorf("hits were merged across indexes: %+v", got)
	}

	// Within one index the same path from two lists (hybrid) is one result.
	fts := []fusedHit{{Index: "public", Filepath: "qmd://pub/a.md"}}
	vec := []fusedHit{{Index: "public", Filepath: "qmd://pub/a.md"}}
	if merged := fuseLists([][]fusedHit{fts, vec}); len(merged) != 1 {
		t.Fatalf("hybrid lists were not merged: %+v", merged)
	}
}

func TestFuseListsScoresFallWithRank(t *testing.T) {
	var list []fusedHit
	for i := 0; i < 5; i++ {
		list = append(list, fusedHit{Index: "public", Filepath: "qmd://pub/doc" + string(rune('a'+i)) + ".md"})
	}
	other := []fusedHit{{Index: "private", Filepath: "qmd://priv/x.md"}}
	got := fuseLists([][]fusedHit{list, other})
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Fatalf("scores are not descending: %+v", got)
		}
	}
	if got[len(got)-1].Score <= 0 {
		t.Errorf("last hit scored %v", got[len(got)-1].Score)
	}
}

func TestParseMountTargets(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"scalar", "/mcp: public", []string{"public"}},
		{"scalar empty", "/mcp:", []string{"index"}},
		{"list", "/mcp: [public, private]", []string{"public", "private"}},
		{"empty list", "/mcp: []", []string{"index"}},
	}
	for _, tc := range cases {
		sc := parseYAML(t, "servers:\n  "+tc.body+"\nauth:\n  public: [/mcp]\n")
		got := sc.Servers["/mcp"]
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
			}
		}
	}

	if _, err := parseServerConfig([]byte("servers:\n  /mcp: [public, public]\nauth:\n"),
		"test.yml"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("duplicate index in a mount list: %v", err)
	}
	if _, err := parseServerConfig([]byte("servers:\n  /mcp: [public, \"\"]\nauth:\n"),
		"test.yml"); err == nil {
		t.Error("empty index name in a mount list was accepted")
	}
}

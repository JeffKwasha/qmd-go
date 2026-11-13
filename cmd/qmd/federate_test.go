package main

import (
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

func TestReachableIndexesFollowGrants(t *testing.T) {
	body := `servers:
  /mcp: public
  /mcp/pub: public
  /mcp/private: private
auth:
  public: [/mcp, /mcp/pub]
  groups:
    hermes-private:
      allow: [/mcp, /mcp/pub, /mcp/private]
  keys:
    - hash: "` + hashKey("key") + `"
      group: hermes-private
`
	sc := parseYAML(t, body)

	anon := reachableIndexes(sc, "")
	if len(anon) != 1 || anon[0] != "public" {
		t.Errorf("anonymous indexes = %v, want [public]", anon)
	}

	keyed := reachableIndexes(sc, "key")
	if len(keyed) != 2 || keyed[0] != "private" || keyed[1] != "public" {
		t.Errorf("keyed indexes = %v, want [private public]", keyed)
	}

	// A key for another group reaches only the public indexes.
	other := parseYAML(t, `servers:
  /mcp: public
  /mcp/private: private
auth:
  public: [/mcp]
  groups:
    hr:
      allow: [/mcp]
  keys:
    - hash: "`+hashKey("hr-key")+`"
      group: hr
`)
	got := reachableIndexes(other, "hr-key")
	if len(got) != 1 || got[0] != "public" {
		t.Errorf("limited key indexes = %v, want [public]", got)
	}
}

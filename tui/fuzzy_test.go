package main

import "testing"

func TestFuzzyRankEmptyQuery(t *testing.T) {
	items := []string{"alpha", "beta", "gamma"}
	got := fuzzyRank("", items, 0)
	if len(got) != 3 {
		t.Fatalf("empty query: want 3 results, got %d", len(got))
	}
	for i, m := range got {
		if m.Idx != i {
			t.Errorf("empty query: result[%d].Idx = %d, want %d", i, m.Idx, i)
		}
	}
}

func TestFuzzyRankSubsequenceFilter(t *testing.T) {
	items := []string{"deploy prod", "deploy staging", "list groups", "delete pod"}
	got := fuzzyRank("dp", items, 0)
	// "deploy prod" and "delete pod" both contain d...p as a subsequence;
	// "deploy staging" does too (d-e-p). "list groups" does not.
	for _, m := range got {
		if items[m.Idx] == "list groups" {
			t.Errorf("non-match leaked: %q", items[m.Idx])
		}
	}
	if len(got) == 0 {
		t.Fatal("expected at least one match for 'dp'")
	}
}

func TestFuzzyRankWordBoundaryBonus(t *testing.T) {
	// "git push" wins for query "gp" because g sits at position 0 and p
	// lands right after a space — both high-bonus positions. "fugitive
	// plot" has g buried mid-word and a wider gap before p, so under the
	// post-fix gap penalty it falls below zero and is filtered out. Either
	// way "git push" must be the top result.
	items := []string{"fugitive plot", "git push"}
	got := fuzzyRank("gp", items, 0)
	if len(got) == 0 {
		t.Fatal("want at least one match")
	}
	if items[got[0].Idx] != "git push" {
		t.Errorf("top result = %q, want %q", items[got[0].Idx], "git push")
	}
}

func TestFuzzyRankNoMatch(t *testing.T) {
	got := fuzzyRank("xyz", []string{"hello", "world"}, 0)
	if len(got) != 0 {
		t.Errorf("want no matches, got %d", len(got))
	}
}

func TestFuzzyRankCaseInsensitive(t *testing.T) {
	got := fuzzyRank("HELLO", []string{"hello world", "nope"}, 0)
	if len(got) != 1 || got[0].Idx != 0 {
		t.Errorf("case-insensitive match failed: %+v", got)
	}
}

// The "story" bug: subsequence matching alone surfaced unrelated prompts
// because s,t,o,r,y appears as a loose subsequence in plenty of normal
// English. The tiered scorer must (a) keep the substring hit, (b) reject
// the noisy subsequence hit outright.
func TestFuzzyRankStoryBugRegression(t *testing.T) {
	items := []string{
		"tell me a story about news",                 // substring hit
		"set up tom orrow for the year",              // scattered s-t-o-r-y noise
		"show today's monthly summary for next year", // similarly loose
		"investigate why the news feed is broken",    // no match at all
	}
	got := fuzzyRank("story", items, 0)
	if len(got) == 0 {
		t.Fatal("want at least the substring match")
	}
	if items[got[0].Idx] != items[0] {
		t.Errorf("substring hit should be top, got %q", items[got[0].Idx])
	}
	for _, m := range got {
		got := items[m.Idx]
		if got == items[2] || got == items[3] {
			t.Errorf("noisy/non-match leaked into results: %q", got)
		}
	}
}

// Tier 1 vs tier 2: a substring hit must beat any subsequence hit even
// when the subsequence is "almost contiguous". Otherwise the tiering is
// pointless.
func TestFuzzyRankSubstringBeatsSubsequence(t *testing.T) {
	items := []string{
		"git push origin", // substring "git" at boundary
		"g-i-t-push",      // also substring (so both hit tier 1)
		"giant push",      // subsequence g-i-t via "gian-t"
	}
	got := fuzzyRank("git", items, 0)
	if len(got) < 2 {
		t.Fatalf("want at least 2 hits, got %d", len(got))
	}
	// The two substring hits must come before the subsequence hit.
	for _, m := range got[:2] {
		if items[m.Idx] == "giant push" {
			t.Errorf("subsequence hit ranked among substring hits")
		}
	}
}

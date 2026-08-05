package main

import (
	"sort"
	"strings"
	"unicode"
)

// fuzzyMatch is one ranked result. Idx points back into the source slice
// passed to fuzzyRank; Indices lists the rune positions in the source string
// that matched (kept for future highlight rendering, unused by v1 view).
type fuzzyMatch struct {
	Idx     int
	Score   int
	Indices []int
}

// fuzzyRank returns subsequence-matched items ordered best-first. Scoring
// rewards matches at the start of the string, after a word boundary, and in
// runs of consecutive characters — same broad strokes as fzf's v1 scorer.
// Case-insensitive. Empty query returns every item in reverse-input order
// (caller passes items newest-first so this matches shell Ctrl+R recall).
func fuzzyRank(query string, items []string, limit int) []fuzzyMatch {
	if limit <= 0 {
		limit = len(items)
	}
	if query == "" {
		out := make([]fuzzyMatch, 0, min(limit, len(items)))
		for i := range items {
			if i >= limit {
				break
			}
			out = append(out, fuzzyMatch{Idx: i})
		}
		return out
	}
	q := []rune(strings.ToLower(query))
	out := make([]fuzzyMatch, 0, len(items))
	for i, s := range items {
		if m, ok := scoreOne(q, s); ok {
			m.Idx = i
			out = append(out, m)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Score != out[b].Score {
			return out[a].Score > out[b].Score
		}
		return len(items[out[a].Idx]) < len(items[out[b].Idx])
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// scoreOne implements a two-tier scorer:
//
//	Tier 1 — case-insensitive contiguous substring. Massive base score so
//	any substring hit outranks every subsequence hit, no matter the length
//	of either. Earlier matches and word-boundary starts get small bumps so
//	"story night" beats "a story" for query "story".
//
//	Tier 2 — subsequence fallback with gap penalty. Each character skipped
//	between two matched query characters costs 1 point; consecutive matches
//	get a +12 bonus. A negative or zero total is rejected — that filters
//	out the "loose noise" matches (e.g. "set tom oh really yes" matching
//	"story") that pure subsequence scoring would surface.
//
// The greedy left-to-right scan in tier 2 is suboptimal in the textbook
// sense (it can pick a long-gap match when a tighter one exists later),
// but tier 1 already catches the cases users actually care about; the
// fallback only needs to be "good enough" for genuine typo-style recall.
func scoreOne(q []rune, s string) (fuzzyMatch, bool) {
	sLower := strings.ToLower(s)
	qStr := string(q)

	if idx := strings.Index(sLower, qStr); idx >= 0 {
		// Tier 1: contiguous substring.
		score := 10000
		score -= idx
		if idx == 0 {
			score += 500
		} else if isBoundary(rune(sLower[idx-1])) {
			score += 200
		}
		score -= len(s) / 10
		indices := make([]int, len(q))
		for i := range q {
			indices[i] = idx + i
		}
		return fuzzyMatch{Score: score, Indices: indices}, true
	}

	// Tier 2: subsequence with gap penalty.
	runes := []rune(s)
	idxs := make([]int, 0, len(q))
	score := 0
	qi := 0
	lastMatch := -1
	for i, r := range runes {
		if qi >= len(q) {
			break
		}
		if unicode.ToLower(r) != q[qi] {
			continue
		}
		bonus := 1
		if i == 0 {
			bonus += 16
		} else if isBoundary(runes[i-1]) {
			bonus += 8
		}
		if lastMatch >= 0 {
			gap := i - lastMatch - 1
			// -2 per skipped char, no separate consecutive bonus. An
			// earlier draft handed out a +12 "contiguous run" bonus when
			// gap==0, but that bonus chained off the previous character
			// regardless of whether *its* match had been penalized —
			// letting a stray y next to a bad r resurrect a "story"
			// match deep inside an unrelated sentence. Without the
			// bonus, contiguous matches already win by avoiding gap
			// penalties; the asymmetry is a feature, not a bug.
			bonus -= 2 * gap
		}
		score += bonus
		idxs = append(idxs, i)
		lastMatch = i
		qi++
	}
	if qi < len(q) {
		return fuzzyMatch{}, false
	}
	if score <= 0 {
		return fuzzyMatch{}, false
	}
	return fuzzyMatch{Score: score, Indices: idxs}, true
}

func isBoundary(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r)
}

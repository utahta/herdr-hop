package tui

import (
	"sort"
	"strings"
	"unicode"
)

// fuzzyFilter returns indexes of items whose text matches query as a
// case-insensitive subsequence, best matches first. Empty query keeps order.
func fuzzyFilter(query string, items []string) []int {
	if strings.TrimSpace(query) == "" {
		idx := make([]int, len(items))
		for i := range items {
			idx[i] = i
		}
		return idx
	}
	type scored struct{ i, score int }
	var res []scored
	q := []rune(strings.ToLower(query))
	for i, it := range items {
		if sc, ok := fuzzyScore(q, []rune(strings.ToLower(it))); ok {
			res = append(res, scored{i, sc})
		}
	}
	sort.SliceStable(res, func(a, b int) bool { return res[a].score > res[b].score })
	out := make([]int, len(res))
	for i, r := range res {
		out[i] = r.i
	}
	return out
}

// fuzzyScore: subsequence match; rewards consecutive matches and matches
// right after a separator, penalizes span length. Higher is better.
// Every occurrence of the first query rune is tried as a start so that
// "hw" prefers "herdr-warm" over the 'h' in "github".
func fuzzyScore(q, s []rune) (int, bool) {
	sc, _, ok := fuzzyMatch(q, s)
	return sc, ok
}

// fuzzyMatch is fuzzyScore with the matched rune indexes of the best start,
// for highlighting the match in the row.
func fuzzyMatch(q, s []rune) (int, []int, bool) {
	if len(q) == 0 {
		return 0, nil, true
	}
	best, found := 0, false
	var bestPos []int
	for start := range s {
		if s[start] != q[0] {
			continue
		}
		if sc, pos, ok := scoreFrom(q, s, start); ok && (!found || sc > best) {
			best, bestPos, found = sc, pos, true
		}
	}
	return best, bestPos, found
}

func scoreFrom(q, s []rune, start int) (int, []int, bool) {
	score, qi, prev := 0, 0, -2
	pos := make([]int, 0, len(q))
	for si := start; si < len(s); si++ {
		if qi < len(q) && s[si] == q[qi] {
			score++
			if si == prev+1 {
				score += 3
			}
			if si == 0 || isSep(s[si-1]) {
				score += 2
			}
			pos = append(pos, si)
			prev = si
			qi++
			if qi == len(q) {
				return score - (si - start - len(q) + 1), pos, true
			}
		}
	}
	return 0, nil, false
}

func isSep(r rune) bool {
	return r == '/' || r == '-' || r == '_' || r == '.' || r == '@' || unicode.IsSpace(r)
}

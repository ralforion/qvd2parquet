package qvd

import "strings"

// MatchGlob reports whether name matches a shell-style wildcard pattern.
// Supported metacharacters are '*' (any run of characters, possibly empty) and
// '?' (exactly one character). Matching is case-insensitive, consistent with
// how column names are selected.
//
// Unlike path.Match this has no path semantics: '/' and '\' are ordinary
// characters, because QVD field names routinely contain them.
func MatchGlob(pattern, name string) bool {
	return globMatch([]rune(strings.ToLower(pattern)), []rune(strings.ToLower(name)))
}

// globMatch is an iterative matcher with backtracking on '*', which avoids the
// exponential blowup a naive recursive version has on patterns like "*a*a*a*".
func globMatch(pat, s []rune) bool {
	var (
		p, i        int
		star        = -1
		startOfStar int
	)
	for i < len(s) {
		switch {
		case p < len(pat) && (pat[p] == '?' || pat[p] == s[i]):
			p++
			i++
		case p < len(pat) && pat[p] == '*':
			star = p
			startOfStar = i
			p++
		case star >= 0:
			// Backtrack: let the last '*' absorb one more character.
			p = star + 1
			startOfStar++
			i = startOfStar
		default:
			return false
		}
	}
	for p < len(pat) && pat[p] == '*' {
		p++
	}
	return p == len(pat)
}

// MatchesAnyGlob reports whether name matches at least one pattern.
func MatchesAnyGlob(patterns []string, name string) bool {
	for _, p := range patterns {
		if MatchGlob(p, name) {
			return true
		}
	}
	return false
}

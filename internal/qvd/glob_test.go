package qvd

import "testing"

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		// The motivating case: drop QlikView's internal "%" key fields.
		{"%*", "%A057_PKEY", true},
		{"%*", "A057_PKEY", false},
		{"%*", "%", true},
		// Literals.
		{"Amount", "Amount", true},
		{"Amount", "amount", true}, // case-insensitive
		{"Amount", "Amounts", false},
		// Wildcards.
		{"*", "anything", true},
		{"*", "", true},
		{"*key", "PKEY", true},
		{"*KEY*", "A_key_B", true},
		{"?A*", "%A057", true},
		{"?A*", "A057", false},
		{"A???", "A057", true},
		{"A???", "A05", false},
		// Field names with path-like characters are ordinary text.
		{"*-||-*", "A057-||-DATBI-||-Ende", true},
		{"*/*", "a/b", true},
		{`*\*`, `a\b`, true},
		// Backtracking.
		{"*a*a*a*", "banana", true},
		{"*a*a*a*a*", "banana", false},
	}
	for _, tc := range tests {
		if got := MatchGlob(tc.pattern, tc.name); got != tc.want {
			t.Errorf("MatchGlob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestMatchesAnyGlob(t *testing.T) {
	pats := []string{"%*", "*_TMP"}
	if !MatchesAnyGlob(pats, "%KEY") || !MatchesAnyGlob(pats, "X_TMP") {
		t.Error("expected a match")
	}
	if MatchesAnyGlob(pats, "Amount") {
		t.Error("Amount should not match")
	}
	if MatchesAnyGlob(nil, "Amount") {
		t.Error("an empty pattern list should match nothing")
	}
}

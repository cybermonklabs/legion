package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestAHolderStateIsCutBetweenTwoCharacters holds the commit-order lock table's cut of a holder
// state to 70 code points: an ASCII state 80 long keeps its first 70, and a state whose 70th byte
// falls inside a two-byte "é" keeps 70 whole code points, where a cut at 70 bytes split it and
// wrote a byte that is not UTF-8 into the report.
func TestAHolderStateIsCutBetweenTwoCharacters(t *testing.T) {
	rows := []struct {
		name  string
		state string
		want  string
	}{
		{name: "a short state stays whole", state: "idle in transaction (Client:ClientRead)", want: "idle in transaction (Client:ClientRead)"},
		{name: "an ASCII state keeps its first 70", state: strings.Repeat("a", 80), want: strings.Repeat("a", 70)},
		{name: "a state under 70 code points stays whole, though its 70th byte is inside an é",
			state: "active:" + strings.Repeat("é", 40), want: "active:" + strings.Repeat("é", 40)},
		{name: "a state over 70 code points keeps 70 whole ones, though its 70th byte is inside an é",
			state: "a" + strings.Repeat("é", 80), want: "a" + strings.Repeat("é", 69)},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			got := holderState(row.state)
			if !utf8.ValidString(got) {
				t.Fatalf("holderState(%q) = %q, which is not UTF-8", row.state, got)
			}
			if got != row.want {
				t.Fatalf("holderState(%q) = %q (%d code points), want %q (%d code points)",
					row.state, got, utf8.RuneCountInString(got), row.want, utf8.RuneCountInString(row.want))
			}
		})
	}
}

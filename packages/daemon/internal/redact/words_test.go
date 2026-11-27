package redact

import "testing"

// Withheld replaces each word wherever it stands, the longest first, and withholds a text that still
// holds a 10-character run of a word, compared without case, unless the word is Exact; a word
// shorter than the run never withholds. The secret is made up.
func TestWithheldReplacesEachWordAndWithholdsARunOfOne(t *testing.T) {
	const secret = "madeUpWithheldSecret0123"
	for _, tc := range []struct {
		name, text string
		words      []Word
		want       string
		ok         bool
	}{
		{"an exact copy, inside a longer word too", "no " + secret + " and x" + secret + "y",
			[]Word{{Text: secret, Shown: "[w]"}}, "no [w] and x[w]y", true},
		{"the longest first", "OMP=" + secret, []Word{{Text: secret, Shown: "[v]"}, {Text: "OMP=" + secret, Shown: "[whole]", Exact: true}},
			"[whole]", true},
		{"a folded copy is withheld", "no MADEUPWITHHELDSECRET0123", []Word{{Text: secret, Shown: "[w]"}}, "", false},
		{"a cut copy is withheld", "no madeUpWithheldSe…", []Word{{Text: secret, Shown: "[w]"}}, "", false},
		{"a run of an Exact word stays", "OMP=other", []Word{{Text: "OMP=" + secret, Shown: "[whole]", Exact: true}}, "OMP=other", true},
		{"a short word never withholds", "short--x", []Word{{Text: "short--", Shown: "[s]"}}, "[s]x", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Withheld(tc.text, tc.words)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("Withheld(%q) = %q, %v; want %q, %v", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}

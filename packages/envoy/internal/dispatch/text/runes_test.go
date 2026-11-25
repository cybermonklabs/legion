package text

import "testing"

// HeadRunes answers what cutting the whole value's []rune conversion answers, for a value under,
// at and over the limit, with runes of several widths and bytes that are no UTF-8.
func TestHeadRunesCutsAsARuneConversion(t *testing.T) {
	whole := func(value string, limit int) string {
		runes := []rune(value)
		if len(runes) <= limit {
			return value
		}
		return string(runes[:limit])
	}
	for _, value := range []string{"", "plain", "naïve café", "日本語のテキスト", "a\xffb\xfe\xfdc", "\xff\xff"} {
		for limit := 0; limit <= 12; limit++ {
			if got, want := HeadRunes(value, limit), whole(value, limit); got != want {
				t.Errorf("HeadRunes(%q, %d) = %q, want %q", value, limit, got, want)
			}
		}
	}
}

package text

import (
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// HeadBytes keeps a text at or under the limit whole and uncut, and cuts a longer one to at most
// the limit, before the character the limit lands in: a multibyte character across the cut is
// left out whole, whichever of its bytes the limit lands on, so the head is valid UTF-8.
//
// The negative control is the cut made at the limit itself, which splits the character.
func TestHeadBytesCutsBeforeTheCharacterTheLimitLandsIn(t *testing.T) {
	for _, value := range []string{"", "abc", "aé", "a€"} {
		if got, cut := HeadBytes(value, 4); got != value || cut {
			t.Errorf("HeadBytes(%q, 4) = %q, %v; want it whole and uncut", value, got, cut)
		}
	}
	const limit = 8
	for _, character := range []string{"é", "€", "😀"} {
		for into := 1; into < len(character); into++ {
			t.Run(character+" cut after byte "+strconv.Itoa(into), func(t *testing.T) {
				head := strings.Repeat("a", limit-into)
				got, cut := HeadBytes(head+character+"b", limit)
				if got != head || !cut || !utf8.ValidString(got) {
					t.Fatalf("HeadBytes = %q, %v; want %q, cut", got, cut, head)
				}
			})
		}
	}
	if got, cut := HeadBytes("abcdefghij", limit); got != "abcdefgh" || !cut {
		t.Errorf("HeadBytes(%q, %d) = %q, %v; want its first %d bytes, cut", "abcdefghij", limit, got, cut, limit)
	}
}

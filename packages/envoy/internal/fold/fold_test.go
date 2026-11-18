package fold

import (
	"strings"
	"testing"
)

// ASCII lowers A-Z and keeps every other character, so a Unicode letter that strings.ToLower maps
// onto an ASCII one keeps an address apart from the address it would otherwise fold onto.
func TestASCIILowersASCIILettersAlone(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"ASCII capitals", "Kate@Example.COM", "kate@example.com"},
		{"U+212A KELVIN SIGN, which Unicode lowers to k", "\u212aate@example.com", "\u212aate@example.com"},
		{"U+0130, which Unicode lowers to i with a combining dot", "\u0130@example.com", "\u0130@example.com"},
		{"a non-ASCII capital", "\u00d6@example.com", "\u00d6@example.com"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ASCII(tc.in); got != tc.want {
				t.Fatalf("ASCII(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// On ASCII text the fold is strings.ToLower's, byte for byte, so every login and address written
// in ASCII compares exactly as it did when strings.ToLower folded it.
func TestASCIIIsToLowerOnASCIIText(t *testing.T) {
	var every strings.Builder
	for b := range 128 {
		every.WriteByte(byte(b))
	}
	if got, want := ASCII(every.String()), strings.ToLower(every.String()); got != want {
		t.Fatalf("ASCII and strings.ToLower differ on ASCII text:\n%q\n%q", got, want)
	}
}

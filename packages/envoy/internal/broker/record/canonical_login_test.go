package record

import "testing"

// Every login comparison in the broker goes through CanonicalLogin on both sides: trimmed, with
// its ASCII letters lowered and nothing else folded. Unicode lowers U+212A KELVIN SIGN to k, so a
// Unicode fold would make "\u212aate@example.com" an operator equal to the owner kate@example.com,
// which releases an agent-tier secret with no approval.
func TestCanonicalLoginLowersASCIILettersAlone(t *testing.T) {
	for _, tc := range []struct{ name, login, want string }{
		{"white space is trimmed and ASCII capitals lowered", "  Kate@Example.com\n", "kate@example.com"},
		{"a Kelvin sign is not the k of another login", "\u212aate@example.com", "\u212aate@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := CanonicalLogin(tc.login); got != tc.want {
				t.Fatalf("CanonicalLogin(%q) = %q, want %q", tc.login, got, tc.want)
			}
		})
	}
}

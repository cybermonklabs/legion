package model

import "testing"

// The stored form of a login is trimmed with its ASCII letters lowered and nothing else folded.
// Unicode lowers U+212A KELVIN SIGN to k, so a Unicode fold would make "\u212aate@example.com"
// compare equal to kate@example.com wherever two logins are compared, as the check that only an
// answer's own author may change it does.
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

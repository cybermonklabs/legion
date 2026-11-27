package model

import (
	"strings"
	"testing"
)

// A login's hash is the first 16 hex digits of the SHA-256 of its canonical form: the values here
// are what `printf %s <login> | sha256sum | cut -c1-16` prints, the command LoginHash's comment
// gives an operator, so each spelling of one login prints the value that command finds, and the
// hash holds no part of the address.
func TestLoginHashIsTheSHA256OfTheCanonicalLogin(t *testing.T) {
	for _, tc := range []struct{ login, want string }{
		{"alice@example.com", "ff8d9819fc0e12bf"},
		{"  Alice@Example.COM\n", "ff8d9819fc0e12bf"},
		{"ada@example.com", "b5fc85e55755f9e0"},
	} {
		got := LoginHash(tc.login)
		if got != tc.want {
			t.Errorf("LoginHash(%q) = %q, want %q", tc.login, got, tc.want)
		}
		if strings.Contains(got, "@") || strings.Contains(got, "example") {
			t.Errorf("LoginHash(%q) = %q holds part of the address", tc.login, got)
		}
	}
}

package oidc

import "testing"

// A login is the email with its ASCII letters lowered and nothing else folded. Unicode lowers
// U+212A KELVIN SIGN to k, so a Unicode fold would sign the owner of "\u212aate@example.com" in
// as kate@example.com, a person that address does not name. Every address Google issues signs in
// as before: an ASCII one in any case, and one holding the accented letter a Workspace username
// may hold, which Google writes in lower case.
func TestWorkspaceLoginLowersASCIILettersAlone(t *testing.T) {
	for _, tc := range []struct{ name, email, want string }{
		{"an ASCII mixed-case address signs in as before", "Kate@Example.com", "kate@example.com"},
		{"a lowercase accented letter signs in as before", "Zo\u00e9@Example.com", "zo\u00e9@example.com"},
		{"a Kelvin sign is not the k of the listed address", "\u212aate@example.com", "\u212aate@example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := workspaceAccount()
			claims.Email = tc.email
			login, err := WorkspaceLogin(claims, flowDomain)
			if err != nil || login != tc.want {
				t.Fatalf("WorkspaceLogin(%q) = %q, %v; want %q", tc.email, login, err, tc.want)
			}
		})
	}
}

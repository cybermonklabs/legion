// Package fold holds the module's one case fold that lowers ASCII letters and nothing else.
// strings.ToLower and strings.EqualFold fold Unicode, and Unicode lowers some non-ASCII letters
// onto ASCII ones — U+212A KELVIN SIGN to k, U+0130 to i with a combining dot — so an address
// nobody listed would compare equal to one somebody did. A comparison is sound only while both of
// its sides fold the same way, so every person's login and every account address an admission
// compares goes through this one function: the sign-in that writes a login (oidc.WorkspaceLogin,
// the Cognito username rule), the canonical forms that compare logins (Dispatch's and the broker's
// CanonicalLogin) and the listener's account lists. So do the airlock's host and header names
// (internal/cipher/proxyrules and its proxy port), whose Lean model folds exactly this way, so a
// change here is a change to that model's contract, and Dispatch's document-name match, which
// envoy-client ranks the same way.
package fold

// ASCII is s with A-Z lowered and every other character kept. On ASCII text it is
// strings.ToLower, byte for byte, so a login written in ASCII compares exactly as it did when
// strings.ToLower folded it.
func ASCII(s string) string {
	for i := range len(s) {
		if 'A' <= s[i] && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if 'A' <= b[j] && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

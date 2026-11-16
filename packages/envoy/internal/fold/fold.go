// Package fold holds the one case fold every person's login and every account address an
// admission compares is written in: ASCII letters lowered, and nothing else. strings.ToLower and
// strings.EqualFold fold Unicode, and Unicode lowers some non-ASCII letters onto ASCII ones —
// U+212A KELVIN SIGN to k, U+0130 to i with a combining dot — so an address nobody listed would
// compare equal to one somebody did. A comparison is sound only while both of its sides fold the
// same way, so the sign-in that writes a login (oidc.WorkspaceLogin, the Cognito username rule),
// the canonical forms that compare logins (Dispatch's and the broker's CanonicalLogin) and the
// listener's account lists all go through this one function.
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

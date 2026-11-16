package githubapptest

import (
	"strings"
	"testing"
)

// TestDescribeAuthorizationKeepsTheCredentialOutOfTheText holds DescribeAuthorization to the
// contract its own comment states: a failure's text says what scheme the header carried and never
// the credential behind it. A header that is a credential with anything after it — a trailing
// space from a configured value, or a second word — is the case the scheme/one-word split misses:
// strings.Cut finds a space, so the first word is printed, and the first word is the credential.
func TestDescribeAuthorizationKeepsTheCredentialOutOfTheText(t *testing.T) {
	const credential = "ghs_examplecredential"
	for _, row := range []struct {
		name   string
		header string
		want   string
		secret string
	}{
		{name: "no header", header: "", want: "no Authorization header"},
		{
			name:   "a scheme and a credential",
			header: "Bearer " + credential,
			want:   `the "Bearer" scheme`,
			secret: credential,
		},
		{
			name:   "one word, which could be the credential itself",
			header: credential,
			secret: credential,
		},
		{
			name:   "a credential with a trailing space",
			header: credential + " ",
			secret: credential,
		},
		{
			name:   "a credential with a word after it",
			header: credential + " extra",
			secret: credential,
		},
		{
			name:   "a credential with a leading space",
			header: " " + credential,
			secret: credential,
		},
	} {
		t.Run(row.name, func(t *testing.T) {
			text := DescribeAuthorization(row.header)
			if row.secret != "" && strings.Contains(text, row.secret) {
				t.Errorf("DescribeAuthorization(%q) = %q, which carries the credential", row.header, text)
			}
			if row.want != "" && text != row.want {
				t.Errorf("DescribeAuthorization(%q) = %q, want %q", row.header, text, row.want)
			}
		})
	}
}

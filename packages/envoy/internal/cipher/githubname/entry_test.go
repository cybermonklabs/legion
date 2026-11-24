package githubname_test

import (
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/githubname"
)

// TestValidEntry pins the whole test of an account/name entry: the account a login and the name a
// repository's own, split at the first slash, with neither . nor .., which name no repository in a
// path, and nothing holding a second slash, which is no <account>/<name> at all.
func TestValidEntry(t *testing.T) {
	for entry, want := range map[string]bool{
		"ada/example-repo":                true,
		"ada/example.repo":                true,
		"mona-cat_octo/example-repo":      true,
		"Ada/Example-Repo":                true,
		"ada/x":                           true,
		"example-repo":                    false,
		"ada/":                            false,
		"/example-repo":                   false,
		"ada/.":                           false,
		"ada/..":                          false,
		"ada/a/b":                         false,
		"a.b/example-repo":                false,
		"-ada/example-repo":               false,
		"_ada/example-repo":               false,
		strings.Repeat("a", 40) + "/x":    false,
		"ada/" + strings.Repeat("r", 101): false,
	} {
		if got := githubname.ValidEntry(entry); got != want {
			t.Errorf("ValidEntry(%q) = %t; want %t", entry, got, want)
		}
	}
}

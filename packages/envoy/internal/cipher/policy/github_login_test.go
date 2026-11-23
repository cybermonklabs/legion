package policy_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cipher/policy"
)

// TestParseGitHubLoginReadsOneLogin: one GitHub login, lowercased, and nothing else: a second
// login, even the same one again, is no login, nor is white space around it, which the caller
// trims, nor the word none. An underscore after the first character is a login's, since a managed
// user's login ends in `_<shortcode>` (`mona-cat_octo`); what GitHub allows in no login is refused.
func TestParseGitHubLoginReadsOneLogin(t *testing.T) {
	for raw, want := range map[string]string{
		"Ada": "ada", "x-": "x-", "A-b-C9": "a-b-c9", strings.Repeat("a", 39): strings.Repeat("a", 39),
		// a_b was refused here while the broker held a login to letters, digits and hyphens, which
		// left a managed user's account one no github-app secret could name.
		"a_b":           "a_b",
		"mona-cat_octo": "mona-cat_octo",
	} {
		if got, err := policy.ParseGitHubLogin(raw); err != nil || got != want {
			t.Fatalf("ParseGitHubLogin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{
		"",                                       // no login is empty
		"ada ada", "Ada ADA", "ada acme", " ada", // white space is no part of a login
		strings.Repeat("a", 40), // GitHub's logins are at most 39 characters
		"-ada",                  // a login starts with a letter or digit, never a hyphen
		"_ada",                  // nor an underscore: a managed user's _<shortcode> follows a name
		"a.b",                   // a dot is a repository name's, never a login's
		"a/b",                   // a slash is a path's, never a login's
		"none", "NONE",          // none removes the tag and names no account
	} {
		if got, err := policy.ParseGitHubLogin(raw); !errors.Is(err, policy.ErrGitHubOwnersLogin) {
			t.Fatalf("ParseGitHubLogin(%q) = %q, %v; want ErrGitHubOwnersLogin", raw, got, err)
		}
	}
}

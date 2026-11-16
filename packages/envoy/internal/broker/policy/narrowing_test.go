package policy_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/broker/policy"
	"github.com/sjawhar/envoy/internal/broker/secrets"
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

// TestParseGitHubNarrowingAnswersTheOneCanonicalForm: a narrowing as a caller writes it is checked
// and answered in the one form both the CLI and the broker build: the owner trimmed and
// lowercased, the repositories sorted, each once, and none left none.
func TestParseGitHubNarrowingAnswersTheOneCanonicalForm(t *testing.T) {
	want := secrets.GitHubNarrowing{Owner: "ada", Repositories: []string{"proofs", "widgets"}, ReadOnly: true}
	got, refusal := policy.ParseGitHubNarrowing(" Ada ", []string{"widgets", "proofs", "widgets"}, true)
	if refusal != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseGitHubNarrowing = %+v, %v; want %+v", got, refusal, want)
	}
	if got, refusal := policy.ParseGitHubNarrowing("ada", nil, false); refusal != nil || got.Repositories != nil {
		t.Fatalf("ParseGitHubNarrowing with no repositories = %+v, %v; want no repositories", got, refusal)
	}
	atMost := repositoryNames(secrets.MaxNarrowedRepositories)
	if got, refusal := policy.ParseGitHubNarrowing("ada", atMost, true); refusal != nil || len(got.Repositories) != secrets.MaxNarrowedRepositories {
		t.Fatalf("ParseGitHubNarrowing with %d repositories = %d repositories, %v; want all of them", len(atMost), len(got.Repositories), refusal)
	}
}

// repositoryNames is n distinct repository names.
func repositoryNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("repo-%d", i)
	}
	return names
}

// TestParseGitHubNarrowingRefusesWhatGitHubCannotHold: the owner is one GitHub login and the
// repositories, when present, at least one and at most secrets.MaxNarrowedRepositories names
// GitHub can hold; each refusal names the field it refuses, so the API and the CLI can name it in
// their own terms. This table is the narrowing rules' one test; the API's test shows only how a
// refusal reaches its caller.
func TestParseGitHubNarrowingRefusesWhatGitHubCannotHold(t *testing.T) {
	for _, tc := range []struct {
		name         string
		owner        string
		repositories []string
		field        string
		mentions     string
	}{
		{"no owner", "", nil, "owner", `""`},
		{"a blank owner", "   ", nil, "owner", `"   "`},
		{"two owners", "ada acme", nil, "owner", "ada acme"},
		{"one owner twice", "ada ada", nil, "owner", "ada ada"},
		{"an owner no login can be", "-ada", nil, "owner", "-ada"},
		{"a repository with its owner", "ada", []string{"ada/proofs"}, "repositories", "ada/proofs"},
		{"a repository named .", "ada", []string{"."}, "repositories", `"."`},
		{"a repository named ..", "ada", []string{".."}, "repositories", `".."`},
		{"an empty repository name", "ada", []string{""}, "repositories", `""`},
		{"an empty repository list", "ada", []string{}, "repositories", "is empty"},
		{"one repository past the maximum", "ada", repositoryNames(secrets.MaxNarrowedRepositories + 1), "repositories", fmt.Sprint(secrets.MaxNarrowedRepositories)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, refusal := policy.ParseGitHubNarrowing(tc.owner, tc.repositories, true)
			if refusal == nil || refusal.Field != tc.field || !strings.Contains(refusal.Reason, tc.mentions) {
				t.Fatalf("ParseGitHubNarrowing(%q, %d repositories) = %+v, %v; want a refusal of %s naming %q", tc.owner, len(tc.repositories), got, refusal, tc.field, tc.mentions)
			}
		})
	}
}

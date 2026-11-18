package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// The owner half of an issue's repository is read as GitHub serves logins, not as GitHub's
// sign-up form accepts new ones: the form's rule binds new accounts alone, and GitHub still
// serves a login with a doubled hyphen, one ending in a hyphen and a managed user's
// `_<shortcode>`, whose repositories an issue has to be able to name. This is the rule the
// daemon reads the same names by (packages/daemon/internal/ghrepo's ownerName), and these rows
// are the ones that tell the two readings apart, which the grammar's own test does not hold.
// What the looser reading still refuses is here beside them: an owner that opens with something
// other than a letter or a digit, and one past GitHub's 39 characters.
func TestAnOwnerGitHubServesIsTakenThoughTheSignUpFormWouldRefuseIt(t *testing.T) {
	for value, want := range map[string]string{
		"hello--world/x":               "hello--world/x",
		"foo--bar/x":                   "foo--bar/x",
		"mona-cat_octo/x":              "mona-cat_octo/x",
		"ArtOfCode-/x":                 "artofcode-/x",
		strings.Repeat("a", 39) + "/x": strings.Repeat("a", 39) + "/x",
	} {
		raw, _ := json.Marshal(value)
		repo, _, err := parseIssueRepo(raw)
		if err != nil || repo == nil || *repo != want {
			t.Errorf("parseIssueRepo(%q) = %v, %v, want %q", value, repo, err, want)
		}
	}
	for _, value := range []string{
		"_a/x",
		"-a/x",
		".a/x",
		strings.Repeat("a", 40) + "/x",
	} {
		raw, _ := json.Marshal(value)
		if repo, _, err := parseIssueRepo(raw); err == nil {
			t.Errorf("parseIssueRepo(%q) = %q, want it refused: GitHub serves no login of that name", value, *repo)
		}
	}
}

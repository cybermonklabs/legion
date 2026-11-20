package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// An issue's own repository is any `owner/name` GitHub could hold, and nothing else. Since no
// mapping row stands behind an issue's repository (migration 20261112004945), the shape check in
// parseIssueRepo is the whole gate on the value, and the value goes on to the CI repair
// (cireconcile's covered), which asks GitHub about every distinct repository an issue names, and
// to the Legion daemon's preparation. A name GitHub's own grammar refuses — an owner that starts
// with a hyphen, a repository named `.` or `..`, a repository name longer than GitHub's 100
// characters, a character GitHub allows in neither half — is refused at the write, as any other
// value that is not `owner/name` is (R3), and a name GitHub holds is taken whatever its case or
// `.git` suffix.
func TestAnIssuesRepositoryIsRefusedUnlessGitHubCouldHoldIt(t *testing.T) {
	for _, value := range []string{
		"x/..",
		"x/.",
		"-bad/x",
		"own%er/x",
		"owner/na me",
		"owner/" + strings.Repeat("a", 101),
	} {
		raw, _ := json.Marshal(value)
		if repo, _, err := parseIssueRepo(raw); err == nil {
			t.Errorf("parseIssueRepo(%q) = %q, want it refused: GitHub holds no repository of that name", value, *repo)
		}
	}
	for value, want := range map[string]string{
		"acme/widgets":                  "acme/widgets",
		"Acme/Widgets.git":              "acme/widgets",
		"sjawhar/.knives-ledger":        "sjawhar/.knives-ledger",
		"a-b/c_d.e":                     "a-b/c_d.e",
		"o/" + strings.Repeat("a", 100): "o/" + strings.Repeat("a", 100),
	} {
		raw, _ := json.Marshal(value)
		repo, _, err := parseIssueRepo(raw)
		if err != nil || repo == nil || *repo != want {
			t.Errorf("parseIssueRepo(%q) = %v, %v, want %q", value, repo, err, want)
		}
	}
}

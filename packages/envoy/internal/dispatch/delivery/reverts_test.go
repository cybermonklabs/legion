package delivery

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readCapturedPullRequest decodes one of GitHub's own REST answers for a pull request, captured
// into testdata/github (dispatch://INFRA-978, R7), as fetchPullRequestPayload decodes GitHub's
// answer.
func readCapturedPullRequest(t *testing.T, name string) pullRequestPayload {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "github", name))
	if err != nil {
		t.Fatalf("read the captured pull request %s: %v", name, err)
	}
	var payload pullRequestPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode the captured pull request %s: %v", name, err)
	}
	return payload
}

// TestTheRevertParseReadsGitHubsRevertButtonPullRequest: the revert of cli/cli#14437 that GitHub's
// revert button made (cli/cli#14462, captured with `gh api repos/cli/cli/pulls/14462`) is read as a
// revert of cli/cli#14437, with the reverted pull request's merge commit read from GitHub's own
// answer for it (`gh api repos/cli/cli/pulls/14437`), both from the captured body, which holds the
// line inside a longer description, and from that line alone, cut from the capture, as the button
// writes it bare. A body that names the pull request any other way, and an unmerged pull request,
// read as no revert.
func TestTheRevertParseReadsGitHubsRevertButtonPullRequest(t *testing.T) {
	revert := readCapturedPullRequest(t, "revert-pull-cli-14462.json")
	reverted := readCapturedPullRequest(t, "reverted-pull-cli-14437.json")
	if !strings.Contains(revert.Body, "\r\n\r\nReverts cli/cli#14437\r\n\r\n") {
		t.Fatalf("the captured body is not the longer description holding the revert line: %q", revert.Body)
	}
	var bare string
	for line := range strings.SplitSeq(revert.Body, "\n") {
		if strings.HasPrefix(line, "Reverts ") {
			bare = strings.TrimSpace(line)
		}
	}
	if bare != "Reverts cli/cli#14437" {
		t.Fatalf("the line cut from the capture = %q", bare)
	}

	want := Revert{
		Repo: "cli/cli", Number: 14462, RevertsRepo: "cli/cli", RevertsNumber: 14437,
		RevertsMergeCommitSHA: new("38316c1c4f275030e3df6666382922e75410d68b"),
		MergedAt:              time.Date(2026, 9, 15, 14, 11, 21, 0, time.UTC),
		MergeCommitSHA:        new("0cf1092493af067646fc5f3db9421c6a6ec9c938"),
	}
	for name, body := range map[string]string{"the captured body": revert.Body, "the bare line": bare} {
		t.Run(name, func(t *testing.T) {
			repo, number, ok := ParseRevert(body)
			if !ok || repo != "cli/cli" || number != 14437 {
				t.Fatalf("ParseRevert = %q #%d, %v; want cli/cli #14437", repo, number, ok)
			}
			payload := revert
			payload.Body = body
			got, ok := revertOf("cli/cli", payload, &reverted)
			if !ok {
				t.Fatalf("revertOf read no revert")
			}
			if got.Repo != want.Repo || got.Number != want.Number || got.RevertsRepo != want.RevertsRepo ||
				got.RevertsNumber != want.RevertsNumber || !got.MergedAt.Equal(want.MergedAt) ||
				*got.RevertsMergeCommitSHA != *want.RevertsMergeCommitSHA || *got.MergeCommitSHA != *want.MergeCommitSHA {
				t.Fatalf("revertOf = %+v (reverted %s, merge %s), want %+v", got, *got.RevertsMergeCommitSHA, *got.MergeCommitSHA, want)
			}
		})
	}

	for _, body := range []string{"Reverts #14437", "This reverts cli/cli#14437 for now", "Reverts cli/cli#14437 and more", "Reverts https://github.com/cli/cli/pull/14437", ""} {
		if repo, number, ok := ParseRevert(body); ok {
			t.Errorf("ParseRevert(%q) = %s#%d, want no revert", body, repo, number)
		}
	}
	unmerged := revert
	unmerged.MergedAt = nil
	if got, ok := revertOf("cli/cli", unmerged, &reverted); ok {
		t.Errorf("an unmerged pull request read as the revert %+v", got)
	}
}

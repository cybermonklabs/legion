package leakguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A gh call that carries its body in a file publishes the file's text, not the file's name, and
// this repository's own rules must read that text.
//
// The shared public leak check already does: measured at leak-check-v15 from a pod on
// 2026-11-10, `public-leak-check.sh gh-guard pr comment 1 --body-file <file>` over a file holding
// a credential exits 3, so the check opens the file a body option names (its own BODY_FILES, and
// the `-F <key>=@<path>` form `gh api` takes). What it does not read is any of this repository's
// four classes: with PUBLIC_LEAK_CHECK_REPO_NAMES and PUBLIC_LEAK_CHECK_INTERNAL off, which R6
// of dispatch://INFRA-1527 keeps them, the same call over a file holding a Tailscale MagicDNS
// host exits 0. So for those four classes the rule file is the whole of the cover, and a body
// option's file is a hole in it: the same host inline in `--body` is refused.
//
// This is the surface the issue was written for — 43 of its 60 measured refusals are text an
// agent posted to GitHub — and a file is how a Legion role posts it:
// skills/legion-retro/references/pr-body-content.md writes a pull request's body with
// `gh api --method PATCH repos/<owner>/<repo>/pulls/<n> -F body=@"$dir/body.md"`.
func TestAGhCallsBodyFileIsRead(t *testing.T) {
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("the box is at "+theHost+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "--body-file", args: []string{"pr", "comment", "1", "--body-file", body}},
		{name: "--notes-file", args: []string{"release", "create", "v0", "--notes-file", body}},
		{name: "-F body=@", args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "--method", "PATCH", "-F", "body=@" + body}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, standIn(t, passes), Public)
			_, err := guard.GhCall(bound(t), GhRequest{Args: tc.args, Repo: "invented-org/invented-repo"})
			if err == nil {
				t.Fatal("the call was cleared, so the file's text reaches GitHub unread")
			}
			report := refusal(t, err)
			if !strings.Contains(report.Error(), theClass) {
				t.Fatalf("the refusal does not name the class: %v", report)
			}
		})
	}
}

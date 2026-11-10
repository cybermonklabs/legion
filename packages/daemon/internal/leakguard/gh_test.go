package leakguard

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
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

// A file a body option names that the guard cannot read is a setup failure naming the option,
// never a call passed through: a guard that did not read what the call publishes has cleared
// nothing, and a pass here would be the hole the test above closes by another door. The bound on
// the read (maxGhFile) is the second door: a file past it is refused rather than pulled into
// memory.
func TestAGhCallsBodyFileThatCannotBeReadIsASetupFailure(t *testing.T) {
	dir := t.TempDir()
	absent := filepath.Join(dir, "absent.md")
	huge := filepath.Join(dir, "huge.md")
	if err := os.WriteFile(huge, bytes.Repeat([]byte("a"), maxGhFile+1), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "a file that is not there",
			args: []string{"pr", "comment", "1", "--body-file", absent},
			want: "the file --body-file names",
		},
		{
			name: "a file past the bound",
			args: []string{"release", "create", "v0", "--notes-file", huge},
			want: "the file --notes-file names",
		},
		{
			name: "a field's file that is not there",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-F", "body=@" + absent},
			want: "the file -F names",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guard := guardWith(t, standIn(t, passes), Public)
			_, err := guard.GhCall(bound(t), GhRequest{Args: tc.args, Repo: "invented-org/invented-repo"})
			failed := setupFailure(t, err)
			if !strings.Contains(failed.Error(), tc.want) {
				t.Fatalf("the failure is %q, want it to name the option in %q", failed, tc.want)
			}
		})
	}
}

// A body option whose value is `-` is standard input, which the shared check's exit-4 handshake
// buffers and the guard records already (TestTheGhShapeScansWhatItBuffered): there is no file of
// that name to open, and a guard that tried would turn every such call into a setup failure.
func TestAGhCallsBodyFileOfStandardInputOpensNoFile(t *testing.T) {
	guard := guardWith(t, standIn(t, passes), Public)
	if _, err := guard.GhCall(bound(t), GhRequest{
		Args: []string{"pr", "comment", "1", "--body-file", "-"},
		Repo: "invented-org/invented-repo",
	}); err != nil {
		t.Fatalf("a call whose body is standard input: %v", err)
	}
}

// A body file naming nothing private is cleared, which is what makes the refusal above a report
// of the text rather than of the option.
func TestAGhCallsCleanBodyFileIsCleared(t *testing.T) {
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("the box is in the other room\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := guardWith(t, standIn(t, passes), Public)
	if _, err := guard.GhCall(bound(t), GhRequest{
		Args: []string{"pr", "comment", "1", "--body-file", body},
		Repo: "invented-org/invented-repo",
	}); err != nil {
		t.Fatalf("a clean body file: %v", err)
	}
}

// The option table itself: which arguments name a file gh would send as content, in both
// spellings of each, and which name no file at all. The entries that answer nothing are the
// table's boundary, and each is a call that must not be refused over a file nobody named.
func TestTheFilesAGhCallWouldSend(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []ghFile
	}{
		{
			name: "a separate --body-file",
			args: []string{"pr", "comment", "1", "--body-file", "body.md"},
			want: []ghFile{{option: "--body-file", path: "body.md"}},
		},
		{
			name: "an attached --body-file",
			args: []string{"pr", "create", "--body-file=body.md"},
			want: []ghFile{{option: "--body-file", path: "body.md"}},
		},
		{
			name: "--notes-file",
			args: []string{"release", "create", "v0", "--notes-file", "notes.md"},
			want: []ghFile{{option: "--notes-file", path: "notes.md"}},
		},
		{
			name: "gh api's --input",
			args: []string{"api", "repos/invented-org/invented-repo/pulls", "--input", "request.json"},
			want: []ghFile{{option: "--input", path: "request.json"}},
		},
		{
			name: "gh agent-task create's --from-file",
			args: []string{"agent-task", "create", "--from-file", "task.md"},
			want: []ghFile{{option: "--from-file", path: "task.md"}},
		},
		{
			name: "a short -F whose value holds no =",
			args: []string{"pr", "comment", "1", "-F", "body.md"},
			want: []ghFile{{option: "-F", path: "body.md"}},
		},
		{
			name: "a short -F with its value attached",
			args: []string{"pr", "comment", "1", "-Fbody.md"},
			want: []ghFile{{option: "-F", path: "body.md"}},
		},
		{
			name: "a field read from a file",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-F", "body=@body.md"},
			want: []ghFile{{option: "-F", path: "body.md"}},
		},
		{
			name: "an attached --field read from a file",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "--field=body=@body.md"},
			want: []ghFile{{option: "--field", path: "body.md"}},
		},
		{
			name: "the same file named twice",
			args: []string{"api", "repos/invented-org/invented-repo/pulls/1", "-F", "body=@body.md", "--body-file", "body.md"},
			want: []ghFile{{option: "-F", path: "body.md"}},
		},
		{name: "a field whose value is a literal", args: []string{"api", "x", "-F", "body=text"}},
		{name: "a raw field, which sends its value literally", args: []string{"api", "x", "-f", "body=@body.md"}},
		{name: "a --field naming a project's field", args: []string{"project", "field-list", "1", "--field", "Status"}},
		{name: "a body option whose value is standard input", args: []string{"pr", "comment", "1", "--body-file", "-"}},
		{name: "a field read from standard input", args: []string{"api", "x", "-F", "body=@-"}},
		{name: "a secret's env file", args: []string{"secret", "set", "NAME", "--env-file", "secrets.env"}},
		{name: "a template of the repository", args: []string{"pr", "create", "--template", "pull_request_template.md"}},
		{name: "a gist's positional files", args: []string{"gist", "create", "note.md"}},
		{name: "a short option whose own value is F", args: []string{"gist", "create", "-dF", "note.md"}},
		{name: "an option past a double dash", args: []string{"repo", "clone", "invented-org/invented-repo", "--", "--body-file", "body.md"}},
		{name: "an option with no value at all", args: []string{"pr", "comment", "1", "--body-file"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ghFileArgs(tc.args); !slices.Equal(got, tc.want) {
				t.Fatalf("ghFileArgs(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

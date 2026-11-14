package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// trailerCommit runs `git <args…>` in dir with env and stdin through `legion git --real <git>`,
// the pod's form, and answers its exit status and stderr.
func trailerCommit(t *testing.T, legion, git, dir string, env []string, stdin string, args ...string) (int, string) {
	t.Helper()
	code, _, stderr := trailerRun(t, dir, env, stdin, legion, append([]string{"git", "--real", git, "--"}, args...)...)
	return code, stderr
}

// assertTrailersAdded fails unless wrapped is alone with each of lines added once: each line is in
// wrapped once more than in alone, and wrapped without the last copy of each, and both without the
// blank lines at their ends, is alone. With no lines, wrapped must be alone.
func assertTrailersAdded(t *testing.T, alone, wrapped string, lines []string) {
	t.Helper()
	if len(lines) == 0 {
		if wrapped != alone {
			t.Fatalf("through the wrapper the message is %q, want git alone's %q", wrapped, alone)
		}
		return
	}
	got := strings.Split(wrapped, "\n")
	aloneLines := strings.Split(alone, "\n")
	for _, line := range lines {
		count := func(of []string) int {
			n := 0
			for _, l := range of {
				if l == line {
					n++
				}
			}
			return n
		}
		if count(got) != count(aloneLines)+1 {
			t.Fatalf("through the wrapper the message %q holds %q %d times, git alone's %q %d times", wrapped, line, count(got), alone, count(aloneLines))
		}
		last := 0
		for i, l := range got {
			if l == line {
				last = i
			}
		}
		got = slices.Delete(got, last, last+1)
	}
	if rest, want := strings.TrimRight(strings.Join(got, "\n"), "\n"), strings.TrimRight(alone, "\n"); rest != want {
		t.Fatalf("through the wrapper the message is %q, which without %q is %q, not git alone's %q", wrapped, lines, rest, want)
	}
}

// Every form of a commit's message, once with git alone and once through `legion git --real
// <git>`, each in a new repository with GIT_EDITOR=true: both refuse or both commit, and a commit
// through the wrapper holds git alone's message with the expected lines added, none where the
// wrapper adds none. The forms are every form of c-git-trailers steps 1 to 5 and 7, the refusals
// git's empty-message rules make, each other source of a message, the option spellings git takes,
// and the cases where the wrapper adds nothing although git commits.
func TestGitWrapperTrailerAgreesWithGitOnEveryMessageForm(t *testing.T) {
	git := trailerGit(t)
	legion := trailerLegion(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	session := []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=form-session", "LEGION_ROLE=implementer"}
	pair := []string{"Omp-Session: form-session", "Legion-Role: implementer"}
	base := [][]string{{"commit", "--allow-empty", "-m", "base"}}
	files := func(name, text string) func(t *testing.T, dir string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tc := range []struct {
		name string
		// setup runs in the repository with git alone before the commit. {files} in a word is a
		// directory of the case's own, outside the repository, and files writes into it.
		setup [][]string
		files func(t *testing.T, dir string)
		// args is the command after the setup, `git` left out.
		args  []string
		stdin string
		// fifo runs the command under bash with ` <(printf x)` after args.
		fifo bool
		// env is the session's variables, the session pair where it is nil.
		env []string
		// commits is whether git alone commits, and lines what the wrapper adds then.
		commits bool
		lines   []string
	}{
		{name: "step 1: -m and two trailers of the caller's", commits: true, lines: pair,
			args: []string{"commit", "--allow-empty", "-m", "one", "--trailer", "Co-authored-by: a <a@example.invalid>", "--trailer", "Co-authored-by: b <b@example.invalid>"}},
		{name: "step 2: --amend", setup: base, args: []string{"commit", "--amend", "--allow-empty"}, commits: true, lines: pair},
		{name: "step 3: --amend with the other role's variables", setup: base, args: []string{"commit", "--amend", "--allow-empty"},
			env: []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=other-session", "LEGION_ROLE=tester"}, commits: true,
			lines: []string{"Omp-Session: other-session", "Legion-Role: tester"}},
		{name: "step 4: a new file and -m base", setup: [][]string{{"sh", "-c", "echo one > f"}, {"add", "f"}},
			args: []string{"commit", "-m", "base"}, commits: true, lines: pair},
		{name: "step 4: -am two", setup: [][]string{{"sh", "-c", "echo one > f"}, {"add", "f"}, {"commit", "-m", "base"}, {"sh", "-c", "echo two > f"}},
			args: []string{"commit", "-am", "two"}, commits: true, lines: pair},
		{name: "step 4: -mthree", args: []string{"commit", "--allow-empty", "-mthree"}, commits: true, lines: pair},
		{name: "step 4: --message=four", args: []string{"commit", "--allow-empty", "--message=four"}, commits: true, lines: pair},
		{name: "step 4: --file=<five>", files: files("five", "five\n"), args: []string{"commit", "--allow-empty", "--file={files}/five"}, commits: true, lines: pair},
		{name: "step 4: -F - with six", stdin: "six", args: []string{"commit", "--allow-empty", "-F", "-"}, commits: true, lines: pair},
		{name: "step 5: no message", args: []string{"commit", "--allow-empty"}},
		{name: "step 5: -m ''", args: []string{"commit", "--allow-empty", "-m", ""}},
		{name: "step 5: -m '   '", args: []string{"commit", "--allow-empty", "-m", "   "}},
		{name: "step 5: -F <an empty file>", files: files("empty", ""), args: []string{"commit", "--allow-empty", "-F", "{files}/empty"}},
		{name: "step 5: -F - with empty input", args: []string{"commit", "--allow-empty", "-F", "-"}},
		{name: "step 5: -e -m '#x'", args: []string{"commit", "--allow-empty", "-e", "-m", "#x"}},
		{name: "step 5: --cleanup=strip -m '#x'", args: []string{"commit", "--allow-empty", "--cleanup=strip", "-m", "#x"}},
		{name: "step 7: --amend of a message that ends with three trailers", setup: [][]string{{"commit", "--allow-empty", "-m", "seven\n\nOmp-Session: S1\nLegion-Role: tester\nOmp-Session: S2"}},
			args: []string{"commit", "--amend", "--allow-empty"}, env: []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=S3", "LEGION_ROLE=implementer"},
			commits: true, lines: []string{"Omp-Session: S3", "Legion-Role: implementer"}},
		{name: "a Signed-off-by line alone", args: []string{"commit", "--allow-empty", "-m", "Signed-off-by: x <x@x.invalid>"}},
		{name: "-s -m ''", args: []string{"commit", "--allow-empty", "-s", "-m", ""}},
		{name: "-s --cleanup=verbatim -m ''", args: []string{"commit", "--allow-empty", "-s", "--cleanup=verbatim", "-m", ""}, commits: true, lines: pair},
		{name: "--cleanup=verbatim -m ' '", args: []string{"commit", "--allow-empty", "--cleanup=verbatim", "-m", " "}, commits: true, lines: pair},
		{name: "-c commit.cleanup=verbatim -m ' '", args: []string{"-c", "commit.cleanup=verbatim", "commit", "--allow-empty", "-m", " "}, commits: true, lines: pair},
		{name: "-t <file> and no message", files: files("template", "A template subject\n\n# A comment\n"), args: []string{"commit", "--allow-empty", "-t", "{files}/template"}},
		{name: "-c commit.template=<file> and no message", files: files("template", "A template subject\n\n# A comment\n"),
			args: []string{"-c", "commit.template={files}/template", "commit", "--allow-empty"}},
		{name: "-c commit.template=<file> and -m", files: files("template", "A template subject\n"),
			args: []string{"-c", "commit.template={files}/template", "commit", "--allow-empty", "-m", "A template subject"}, commits: true, lines: pair},
		{name: "--allow-empty-message -m ''", args: []string{"commit", "--allow-empty", "--allow-empty-message", "-m", ""}, commits: true, lines: pair},
		{name: "-C HEAD", setup: base, args: []string{"commit", "--allow-empty", "-C", "HEAD"}, commits: true, lines: pair},
		{name: "-c HEAD", setup: base, args: []string{"commit", "--allow-empty", "-c", "HEAD"}, commits: true, lines: pair},
		{name: "--fixup=HEAD", setup: base, args: []string{"commit", "--allow-empty", "--fixup=HEAD"}, commits: true, lines: pair},
		{name: "--fixup=amend: of a commit with an empty body", setup: base, args: []string{"commit", "--allow-empty", "--fixup=amend:HEAD"}, commits: true, lines: pair},
		{name: "--fixup=amend: of an amend! commit with an empty body", setup: [][]string{{"commit", "--allow-empty", "-m", "amend! base"}},
			args: []string{"commit", "--allow-empty", "--fixup=amend:HEAD"}},
		{name: "--squash=HEAD", setup: base, args: []string{"commit", "--allow-empty", "--squash=HEAD"}, commits: true, lines: pair},
		{name: "-F <(printf x), a FIFO", fifo: true, args: []string{"commit", "--allow-empty", "-F"}, commits: true, lines: pair},
		{name: "-F /dev/stdin from a pipe", stdin: "x\n", args: []string{"commit", "--allow-empty", "-F", "/dev/stdin"}, commits: true, lines: pair},
		{name: "a relative -F under git -C <dir>", setup: [][]string{{"sh", "-c", "mkdir sub && echo relative > sub/message"}},
			args: []string{"-C", "sub", "commit", "--allow-empty", "-F", "message"}, commits: true, lines: pair},
		{name: "a global option that takes a value", args: []string{"--shallow-file", "{files}/shallow", "-c", "x.y=z", "commit", "--allow-empty", "-m", "x"}, commits: true, lines: pair},
		{name: "MERGE_MSG after git merge --no-commit", setup: [][]string{
			{"commit", "--allow-empty", "-m", "base"}, {"checkout", "-q", "-b", "side"}, {"commit", "--allow-empty", "-m", "side"},
			{"checkout", "-q", "main"}, {"commit", "--allow-empty", "-m", "main"}, {"merge", "-q", "--no-commit", "--no-ff", "side"},
		}, args: []string{"commit"}, commits: true, lines: pair},
		{name: "--mess=x, an abbreviation", args: []string{"commit", "--allow-empty", "--mess=x"}, commits: true, lines: pair},
		{name: "--amen, an abbreviation", setup: base, args: []string{"commit", "--amen", "--allow-empty"}, commits: true, lines: pair},
		{name: "-h", args: []string{"commit", "-h"}},
		{name: "an option of no git commit", args: []string{"commit", "--allow-empty", "--bogus", "-m", "x"}},
		{name: "--cleanup=scissors -e keeps a comment line", args: []string{"commit", "--allow-empty", "--cleanup=scissors", "-e", "-m", "#x"}, commits: true, lines: pair},
		{name: "a cut line under -v adds nothing", args: []string{"commit", "--allow-empty", "-v", "-m", "a\n# ------------------------ >8 ------------------------\nb"}, commits: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.files != nil {
				tc.files(t, dir)
			}
			env := trailerEnv(t, trailerPath(git), session...)
			if tc.env != nil {
				env = trailerEnv(t, trailerPath(git), tc.env...)
			}
			args := make([]string, len(tc.args))
			for i, arg := range tc.args {
				args[i] = strings.ReplaceAll(arg, "{files}", dir)
			}
			outcome := func(wrapper bool) (int, string) {
				repo := trailerRepo(t, git)
				for _, step := range tc.setup {
					name, stepArgs := git, step
					if step[0] == "sh" {
						name, stepArgs = "/bin/sh", step[1:]
					}
					if code, _, stderr := trailerRun(t, repo, trailerEnv(t, trailerPath(git)), "", name, stepArgs...); code != 0 {
						t.Fatalf("setup %q: %s", step, stderr)
					}
				}
				command := append([]string{git}, args...)
				if wrapper {
					command = append([]string{legion, "git", "--real", git, "--"}, args...)
				}
				var code int
				var stderr string
				if tc.fifo {
					code, _, stderr = trailerRun(t, repo, env, tc.stdin, bash, append([]string{"-c", `"$@" <(printf x)`, "bash"}, command...)...)
				} else {
					code, _, stderr = trailerRun(t, repo, env, tc.stdin, command[0], command[1:]...)
				}
				if code != 0 {
					return code, stderr
				}
				return 0, trailerMessage(t, git, repo)
			}
			aloneCode, alone := outcome(false)
			wrappedCode, wrapped := outcome(true)
			if (aloneCode == 0) != tc.commits {
				t.Fatalf("git alone exited %d (%s); want a commit: %v", aloneCode, alone, tc.commits)
			}
			if (wrappedCode == 0) != (aloneCode == 0) {
				t.Fatalf("git alone exited %d and through the wrapper %d: %s", aloneCode, wrappedCode, wrapped)
			}
			if aloneCode == 0 {
				assertTrailersAdded(t, alone, wrapped, tc.lines)
			}
		})
	}
}

// Each environment of c-git-trailers step 6 gives the lines the session that the dispatch command
// acts as and the role decide: the role line only for a Legion role, and the session line only
// for the id of the host's variable, and only where it is a valid id.
func TestGitWrapperTrailerTakesTheSessionDispatchActsAs(t *testing.T) {
	git := trailerGit(t)
	legion := trailerLegion(t)
	role := "Legion-Role: implementer"
	for _, tc := range []struct {
		name  string
		env   []string
		lines []string
	}{
		{"LEGION_ROLE unset", []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=dispatch-id"}, []string{"Omp-Session: dispatch-id"}},
		{"LEGION_ROLE=bogus", []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=dispatch-id", "LEGION_ROLE=bogus"}, []string{"Omp-Session: dispatch-id"}},
		{"LEGION_ROLE=controller", []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=dispatch-id", "LEGION_ROLE=controller"},
			[]string{"Omp-Session: dispatch-id", "Legion-Role: controller"}},
		{"CLAUDE_CODE_SESSION_ID beside DISPATCH_SESSION_ID", []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=dispatch-id", "CLAUDE_CODE_SESSION_ID=claude-id", "LEGION_ROLE=implementer"},
			[]string{"Omp-Session: dispatch-id", role}},
		{"DISPATCH_HOST=claude and both session variables", []string{"DISPATCH_HOST=claude", "DISPATCH_SESSION_ID=dispatch-id", "CLAUDE_CODE_SESSION_ID=claude-id", "LEGION_ROLE=implementer"},
			[]string{"Omp-Session: claude-id", role}},
		{"DISPATCH_HOST unset, CLAUDE_CODE_CHILD_SESSION=1 and both session variables", []string{"CLAUDE_CODE_CHILD_SESSION=1", "DISPATCH_SESSION_ID=dispatch-id", "CLAUDE_CODE_SESSION_ID=claude-id", "LEGION_ROLE=implementer"},
			[]string{"Omp-Session: claude-id", role}},
		{"DISPATCH_HOST=claude and ENVOY_SESSION_ID beside CLAUDE_CODE_SESSION_ID", []string{"DISPATCH_HOST=claude", "ENVOY_SESSION_ID=envoy-id", "CLAUDE_CODE_SESSION_ID=claude-id", "LEGION_ROLE=implementer"},
			[]string{"Omp-Session: envoy-id", role}},
		{"both session variables unset", []string{"DISPATCH_HOST=omp", "LEGION_ROLE=implementer"}, []string{role}},
		{"a line break in DISPATCH_SESSION_ID", []string{"DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=a\nLegion-Role: merger", "LEGION_ROLE=implementer"}, []string{role}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := trailerRepo(t, git)
			if code, stderr := trailerCommit(t, legion, git, repo, trailerEnv(t, trailerPath(git), tc.env...), "", "commit", "--allow-empty", "-m", "seven"); code != 0 {
				t.Fatalf("commit = %d: %s", code, stderr)
			}
			if got, want := trailerMessage(t, git, repo), "seven\n\n"+strings.Join(tc.lines, "\n")+"\n"; got != want {
				t.Fatalf("message = %q, want %q", got, want)
			}
		})
	}
}

// c-git-trailers steps 2, 3 and 7: an amend adds each of the two lines the message lacks and
// nothing else. The same session and role add nothing, another role's pair goes after every line
// of the message, and a message with other sessions' lines keeps them all.
func TestGitWrapperTrailerAmendAddsOnlyTheLinesTheMessageLacks(t *testing.T) {
	git := trailerGit(t)
	legion := trailerLegion(t)
	repo := trailerRepo(t, git)
	implementer := trailerEnv(t, trailerPath(git), "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=implementer-session", "LEGION_ROLE=implementer")
	commit := func(env []string, args ...string) string {
		t.Helper()
		if code, stderr := trailerCommit(t, legion, git, repo, env, "", args...); code != 0 {
			t.Fatalf("git %q = %d: %s", args, code, stderr)
		}
		return trailerMessage(t, git, repo)
	}

	step1 := commit(implementer, "commit", "--allow-empty", "-m", "one",
		"--trailer", "Co-authored-by: a <a@example.invalid>", "--trailer", "Co-authored-by: b <b@example.invalid>")
	want := "one\n\nOmp-Session: implementer-session\nLegion-Role: implementer\nCo-authored-by: a <a@example.invalid>\nCo-authored-by: b <b@example.invalid>\n"
	if step1 != want {
		t.Fatalf("step 1: message = %q, want %q", step1, want)
	}
	if got := commit(implementer, "commit", "--amend", "--allow-empty"); got != step1 {
		t.Fatalf("step 2: the same session's amend gives %q, want %q", got, step1)
	}
	tester := trailerEnv(t, trailerPath(git), "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=tester-session", "LEGION_ROLE=tester")
	if got, want := commit(tester, "commit", "--amend", "--allow-empty"), step1+"Omp-Session: tester-session\nLegion-Role: tester\n"; got != want {
		t.Fatalf("step 3: the other role's amend gives %q, want %q", got, want)
	}

	if code, _, stderr := trailerRun(t, repo, trailerEnv(t, trailerPath(git)), "", git, "commit", "--allow-empty", "-m", "seven\n\nOmp-Session: S1\nLegion-Role: tester\nOmp-Session: S2"); code != 0 {
		t.Fatalf("git alone: %s", stderr)
	}
	s3 := trailerEnv(t, trailerPath(git), "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=S3", "LEGION_ROLE=implementer")
	if got, want := commit(s3, "commit", "--amend", "--allow-empty"), "seven\n\nOmp-Session: S1\nLegion-Role: tester\nOmp-Session: S2\nOmp-Session: S3\nLegion-Role: implementer\n"; got != want {
		t.Fatalf("step 7: message = %q, want %q", got, want)
	}
}

// A helper git that answers `git version 2.31.0`, below the first git with `commit --trailer`,
// gets no read but its version, and the commit's arguments as they were written.
func TestGitWrapperTrailerNeedsGit232(t *testing.T) {
	legion := trailerLegion(t)
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	helper := filepath.Join(dir, "git")
	writeTrailerScript(t, helper, trailerLogging(log)+"case $1 in version) echo 'git version 2.31.0' ;; esac\n")
	env := trailerEnv(t, "/usr/bin:/bin", "DISPATCH_HOST=omp", "DISPATCH_SESSION_ID=old-git-session", "LEGION_ROLE=implementer")
	if code, stderr := trailerCommit(t, legion, helper, dir, env, "", "commit", "--allow-empty", "-m", "x"); code != 0 {
		t.Fatalf("commit = %d: %s", code, stderr)
	}
	calls := trailerCalls(t, log)
	if want := [][]string{{"version"}, {"commit", "--allow-empty", "-m", "x"}}; !slices.EqualFunc(calls, want, slices.Equal) {
		t.Fatalf("the helper git received %q, want %q", calls, want)
	}
}

// The wrapper reads a commit's options as git 2.47.3's parse_options reads them: bundled short
// options, attached values, unique prefixes of long names, --no- forms, paths before options,
// and `--`. It refuses what git refuses: an unknown or ambiguous option, a missing value, a value
// a switch does not take, and a long name written with one dash.
func TestGitWrapperTrailerReadsCommitOptionsAsGitDoes(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		ok    bool
		check func(r commitRead) bool
	}{
		{[]string{"-am", "two"}, true, func(r commitRead) bool { return r.haveMessage && r.message == "two\n" }},
		{[]string{"-mthree"}, true, func(r commitRead) bool { return r.message == "three\n" }},
		{[]string{"--message=four"}, true, func(r commitRead) bool { return r.message == "four\n" }},
		{[]string{"--mess=x"}, true, func(r commitRead) bool { return r.message == "x\n" }},
		{[]string{"-m", "a", "-m", "b"}, true, func(r commitRead) bool { return r.message == "a\n\nb\n" }},
		{[]string{"-m", ""}, true, func(r commitRead) bool { return r.haveMessage && r.message == "" }},
		{[]string{"-m", "a", "--no-message"}, true, func(r commitRead) bool { return !r.haveMessage && r.message == "" }},
		{[]string{"-m", "--"}, true, func(r commitRead) bool { return r.message == "--\n" }},
		{[]string{"path", "-m", "after a path"}, true, func(r commitRead) bool { return r.message == "after a path\n" }},
		{[]string{"--", "-m", "x"}, true, func(r commitRead) bool { return !r.haveMessage }},
		{[]string{"--amen"}, true, func(r commitRead) bool { return r.amend }},
		{[]string{"-F-"}, true, func(r commitRead) bool { return r.file == commitFile{set: true, value: "-", arg: 0, at: 2} }},
		{[]string{"-q", "-F", "-"}, true, func(r commitRead) bool { return r.file == commitFile{set: true, value: "-", arg: 2, at: 0} }},
		{[]string{"--file=five"}, true, func(r commitRead) bool { return r.file == commitFile{set: true, value: "five", arg: 0, at: 7} }},
		{[]string{"-aF", "five"}, true, func(r commitRead) bool { return r.file == commitFile{set: true, value: "five", arg: 1, at: 0} }},
		{[]string{"-F", ""}, true, func(r commitRead) bool { return !r.file.set }},
		{[]string{"-F", "x", "--no-file"}, true, func(r commitRead) bool { return !r.file.set }},
		{[]string{"-e", "--no-edit"}, true, func(r commitRead) bool { return r.edit == 0 }},
		{[]string{"--no-edit", "-e"}, true, func(r commitRead) bool { return r.edit == 1 }},
		{[]string{"-v", "--verbose"}, true, func(r commitRead) bool { return r.verbose == 2 }},
		{[]string{"-v", "--no-verbose"}, true, func(r commitRead) bool { return r.verbose == 0 }},
		{[]string{}, true, func(r commitRead) bool { return r.verbose == -1 && r.edit == -1 }},
		{[]string{"--no-verify", "--verify", "--no-no-verify"}, true, func(r commitRead) bool { return true }},
		{[]string{"-Skey", "-uno", "-S", "--untracked-files"}, true, func(r commitRead) bool { return true }},
		{[]string{"--short", "--no-long"}, true, func(r commitRead) bool { return !r.status }},
		{[]string{"--cleanup=verbatim", "--no-cleanup"}, true, func(r commitRead) bool { return r.cleanupWritten && !r.cleanup.set }},
		{[]string{"-t", "x", "--no-template"}, true, func(r commitRead) bool { return r.templateWritten && !r.template.set }},
		{[]string{"--fixup=amend:HEAD"}, true, func(r commitRead) bool { return r.fixup == commitString{true, "amend:HEAD"} }},
		{[]string{"-h"}, true, func(r commitRead) bool { return r.help }},
		{[]string{"-ah"}, true, func(r commitRead) bool { return r.help }},
		{[]string{"-m", "x", "--help"}, true, func(r commitRead) bool { return r.help }},
		{[]string{"--fi=x"}, false, nil},
		{[]string{"--a"}, false, nil},
		{[]string{"--no"}, false, nil},
		{[]string{"-amend"}, false, nil},
		{[]string{"--bogus"}, false, nil},
		{[]string{"-x"}, false, nil},
		{[]string{"-m"}, false, nil},
		{[]string{"--no-trailer"}, false, nil},
		{[]string{"--all=x"}, false, nil},
		{[]string{"--no-message=x"}, false, nil},
	} {
		r, ok := readCommitOptions(tc.args)
		if ok != tc.ok || ok && !tc.check(r) {
			t.Errorf("readCommitOptions(%q) = %+v, %v", tc.args, r, ok)
		}
	}
}

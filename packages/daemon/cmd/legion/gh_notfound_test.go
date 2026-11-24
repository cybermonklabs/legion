package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/leakguard"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// The tests of what `legion gh` and `legion leak-check pre-push` say when GitHub answers the leak
// guard's REST visibility read with 404, through gh or through the curl read the guard makes after
// gh could not reach GitHub. Both refuse the write, exit 2, as for every read that did not answer,
// with words that say GitHub answered 404 for the repository, and with neither the setup error's
// marker nor a refusal's: the write named no private text, and no check failed to run. Every other
// failure of the read keeps the setup error it was, a 500 among them.

// noSuchRepo is the repository these tests write to: one of an owner gitCredentialBundle serves,
// acme, under a name GitHub answers 404 for.
const noSuchRepo = "acme/no-such-repo"

// notFoundSays is the whole of what the guard says of a write refused because GitHub answered its
// visibility read of noSuchRepo with 404, what being the write in the guard's words.
func notFoundSays(what string) string {
	return what + ": GitHub answers 404 for " + noSuchRepo + ", so the repository does not exist or this credential cannot read it"
}

// guardMarkers are the setup error's marker and a refusal's, which no not-found answer holds.
var guardMarkers = []string{"LEAK CHECK SETUP ERROR", "LEAK CHECK: refusing"}

// restRead is the guard's REST visibility read of noSuchRepo, as the stand-in real gh records it.
var restRead = strings.Join(leakguard.VisibilityReadWithHeadersArgs(noSuchRepo), " ")

// visibilityReadGh writes a stand-in real gh, for LEGION_GH_PATH. It answers `[<command>] --help`
// with gh's saved help (ghStandInHelp), appends the arguments of every other run to record, one
// line each, and does what read says for the guard's REST visibility read of noSuchRepo; any other
// run, the call itself among them, prints "gh ran" and exits 0.
func visibilityReadGh(t *testing.T, read string) (stub, record string) {
	t.Helper()
	help, err := filepath.Abs(ghHelpDir)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	stub, record = filepath.Join(dir, "real-gh"), filepath.Join(dir, "record")
	script := ghStandInHelp(help) +
		"printf '%s\\n' \"$*\" >> " + shellWord(record) + "\n" +
		"if [ \"$*\" = " + shellWord(restRead) + " ]; then\n" + read + "fi\n" +
		"echo 'gh ran'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return stub, record
}

// ghAnswers is a REST read of the stand-in real gh that prints answer, a text of gh's standard
// output and standard error as the two print together (leakguardtest.GhReadNotFound is one), and
// exits 1.
func ghAnswers(t *testing.T, answer string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "answer")
	if err := os.WriteFile(path, []byte(answer), 0o600); err != nil {
		t.Fatal(err)
	}
	return printsAnswer(path, 1)
}

// ghCannotReachGitHub is a REST read of the stand-in real gh that fails as every gh call failed in
// the busy one-CPU pod: Go's error for the request, naming its URL, and exit 1.
var ghCannotReachGitHub = "printf '%s\\n' " + shellWord(`Get "https://api.github.com/repos/`+noSuchRepo+`": net/http: TLS handshake timeout`) + " >&2\nexit 1\n"

// curlServerError is a stand-in curl to which GitHub answered 500.
const curlServerError = "printf 'HTTP/2 500\\r\\ncontent-type: application/json; charset=utf-8\\r\\n\\r\\n{\"message\":\"Server Error\"}'\nexit 0\n"

// visibilityReadCase is one row of these tests: what the stand-in real gh does for the guard's
// REST read, what the stand-in curl does where the guard reads through it, how often curl runs,
// and, for a read that is not GitHub's 404, what the refusal holds in place of notFoundSays.
type visibilityReadCase struct {
	name     string
	gh       func(t *testing.T) string
	curl     func(t *testing.T) string
	curlRuns int
	says     string
}

// curlNotFound is a stand-in curl to which GitHub answered 404 (testdata/fork-read/not-found.http).
func curlNotFound(t *testing.T) string { return printsAnswer(forkReadFixture(t, "not-found.http"), 0) }

// visibilityReadCases are the rows both tests read the visibility through, what being the write
// in the guard's words: GitHub's 404 through gh (leakguardtest.GhReadNotFound), after which curl
// never runs; GitHub's 404 through curl, after gh could not reach GitHub; and a 500 each way
// (leakguardtest.GhReadServerError, and curl's), which stays a setup error.
func visibilityReadCases(what string) []visibilityReadCase {
	setupError := "LEAK CHECK SETUP ERROR: " + what + ": the visibility of " + noSuchRepo + " could not be read"
	ghNotFound := func(t *testing.T) string { return ghAnswers(t, leakguardtest.GhReadNotFound) }
	ghServerError := func(t *testing.T) string { return ghAnswers(t, leakguardtest.GhReadServerError) }
	unreached := func(*testing.T) string { return ghCannotReachGitHub }
	return []visibilityReadCase{
		{name: "gh's 404", gh: ghNotFound, curl: curlNotFound},
		{name: "curl's 404 after gh could not reach GitHub", gh: unreached, curl: curlNotFound, curlRuns: 1},
		{name: "gh's 500", gh: ghServerError, curl: curlNotFound, says: setupError},
		{name: "curl's 500 after gh could not reach GitHub", gh: unreached, curl: func(*testing.T) string { return curlServerError }, curlRuns: 1, says: setupError},
	}
}

// saysWhatWasRead fails the test unless printed is what tc's read makes the guard say of what: the
// not-found words with neither marker, or, for a read that is not GitHub's 404, tc.says and no
// not-found words.
func saysWhatWasRead(t *testing.T, tc visibilityReadCase, printed, what string) {
	t.Helper()
	if tc.says != "" {
		if !strings.Contains(printed, tc.says) || strings.Contains(printed, "GitHub answers 404") {
			t.Errorf("printed %q, want it to hold %q and no 404 of GitHub's", printed, tc.says)
		}
		return
	}
	if want := notFoundSays(what); !strings.Contains(printed, want) {
		t.Errorf("printed %q, want it to hold %q", printed, want)
	}
	for _, marker := range guardMarkers {
		if strings.Contains(printed, marker) {
			t.Errorf("printed %q, which holds %q", printed, marker)
		}
	}
}

// `legion gh` in a process of its own, for a GET of the repository and for a PATCH of one of its
// pull requests. The stand-in check passes both and is no copy of the one the read rule was
// proven against, so the guard reads the destination's visibility for the GET as for the write.
// That read goes through the pane's `gh` wrapper to `legion gh`, which runs the stand-in real gh;
// after gh could not reach GitHub, the guard reads through the stand-in curl that
// LEGION_CMD_TEST_CURL names, with the token `legion gh auth git-credential get` answers from acme's
// directory, which the stand-in checks. GitHub's 404 refuses the call, exit 2, with `legion gh: `
// and the not-found words; a 500 each way refuses the GET, exit 2, with the setup error. The real
// gh ran the guard's REST read and nothing else: not the call, and no GraphQL read.
func TestGhTellsAVisibilityRead404AsNotFound(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	get := []string{"api", "-i", "repos/" + noSuchRepo}
	patch := []string{"api", "-X", "PATCH", "repos/" + noSuchRepo + "/pulls/1", "-f", "title=x"}
	for _, read := range visibilityReadCases("the gh call") {
		calls := [][]string{get, patch}
		if read.says != "" {
			// A read that is not GitHub's 404 is control enough through one call.
			calls = calls[:1]
		}
		for _, args := range calls {
			t.Run(read.name+", gh "+strings.Join(args, " "), func(t *testing.T) {
				stub, record := visibilityReadGh(t, read.gh(t))
				curl := newFakeGitHub(t, "ghs_acme_write", read.curl(t))
				code, stdout, stderr := legionGh(t, map[string]string{
					"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
					"GH_TOKEN": "", "GITHUB_TOKEN": "", "XDG_STATE_HOME": t.TempDir(),
					"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
					testCurlEnv:         curl.program,
				}, args...)
				if code != 2 || stdout != "" {
					t.Errorf("legion gh %q = exit %d, stdout %q, stderr %q; want exit 2 and nothing on stdout", args, code, stdout, stderr)
				}
				if !strings.HasPrefix(stderr, "legion gh: ") {
					t.Errorf("legion gh %q printed %q on stderr, want it to start `legion gh: `", args, stderr)
				}
				saysWhatWasRead(t, read, stderr, "the gh call")
				if got := readRecord(t, record); got != restRead+"\n" {
					t.Errorf("the real gh ran\n%q\nwant the guard's REST read alone:\n%q", got, restRead+"\n")
				}
				if got := len(curl.runs(t)); got != read.curlRuns {
					t.Errorf("curl ran %d times, want %d", got, read.curlRuns)
				}
			})
		}
	}
}

// `legion leak-check pre-push origin https://github.com/acme/no-such-repo.git`, in process, as the
// image's hook runs it, with git's line for a ref the push deletes. The guard's REST read goes
// through the pane's `gh` wrapper to `legion gh`, which runs the stand-in real gh, and after gh
// could not reach GitHub through the stand-in curl leakguardCurl names. GitHub's 404 refuses the
// push, exit 2, with the not-found words for the push to origin; a 500 each way refuses it, exit
// 2, with the setup error. The real gh ran the guard's REST read and nothing else, and git never
// ran: no ls-remote reads a repository GitHub answered 404 for.
func TestLeakCheckPrePushTellsAVisibilityRead404AsNotFound(t *testing.T) {
	root, defaultDir := gitCredentialBundle(t)
	for _, read := range visibilityReadCases("the push to origin") {
		t.Run(read.name, func(t *testing.T) {
			record, curl, gitRecord := pushVisibilityReadEnv(t, root, defaultDir, read)
			code, printed := leakCheck(t, refusedRefDeletion, "pre-push", "origin", "https://github.com/"+noSuchRepo+".git")
			if code != leakguard.StatusSetupFailure {
				t.Errorf("exited %d, want %d: %s", code, leakguard.StatusSetupFailure, printed)
			}
			saysWhatWasRead(t, read, printed, "the push to origin")
			if got := readRecord(t, record); got != restRead+"\n" {
				t.Errorf("the real gh ran\n%q\nwant the guard's REST read alone:\n%q", got, restRead+"\n")
			}
			if got := len(curl.runs(t)); got != read.curlRuns {
				t.Errorf("curl ran %d times, want %d", got, read.curlRuns)
			}
			if got := readRecord(t, gitRecord); got != "" {
				t.Errorf("git ran as %q, want never", got)
			}
		})
	}
}

// refusedRefDeletion is git's pre-push line for a push that deletes a ref named for a host the
// rules refuse: its name is the push's one record, so a scan refuses the push and no scan passes
// it, and no git reads a commit.
var refusedRefDeletion = "refs/heads/work " + noSHA + " refs/heads/legion/" + theLeakCheckHost + " " + noSHA + "\n"

// pushVisibilityReadEnv readies the in-process `legion leak-check pre-push` of a push to
// noSuchRepo, whose visibility the guard reads as read says: through the pane's `gh` wrapper to
// `legion gh`, which runs the stand-in real gh, and after gh could not reach GitHub through the
// stand-in curl leakguardCurl names, under a stand-in check that passes, in a working directory of
// the test's own, with a git that records each run and fails. It answers the stand-in real gh's
// record, the stand-in curl and git's record.
func pushVisibilityReadEnv(t *testing.T, root, defaultDir string, read visibilityReadCase) (record string, curl fakeGitHub, gitRecord string) {
	t.Helper()
	stub, record := visibilityReadGh(t, read.gh(t))
	curl = newFakeGitHub(t, "ghs_acme_write", read.curl(t))
	useForkRead(t, curl.program, forkReadBound)
	bin := t.TempDir()
	gitRecord = filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+shellWord(gitRecord)+"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	for name, value := range map[string]string{
		testMainEnv: "1", "PATH": ghWrapperDir(t) + ":" + bin + ":" + os.Getenv("PATH"),
		"LEGION_GH_PATH": stub, "GH_CONFIG_DIR": defaultDir, "LEGION_GH_OWNERS_DIR": root,
		"GH_TOKEN": "", "GITHUB_TOKEN": "", "XDG_STATE_HOME": t.TempDir(),
		"PUBLIC_LEAK_CHECK": leakguardtest.Write(t, t.TempDir(), leakguardtest.Passes),
	} {
		t.Setenv(name, value)
	}
	return record, curl, gitRecord
}

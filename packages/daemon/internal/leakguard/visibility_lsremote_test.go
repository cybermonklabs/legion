package leakguard

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// theRepo is the repository these tests read the visibility of, and theURL the HTTPS URL the
// read without REST hands git for it.
const (
	theRepo = "invented-org/invented-repo"
	theURL  = githubOrigin + "/" + theRepo + ".git"
)

// githubAsksForACredential is what git 2.47.3 printed, exit 128, for an anonymous ls-remote of a
// private repository on github.com, with the guard's askpass and terminal prompt settings: the
// askpass answers nothing, and the terminal prompt is off.
var githubAsksForACredential = []string{
	"error: unable to read askpass response from '/usr/bin/false'",
	"fatal: could not read Username for 'https://github.com': terminal prompts disabled",
}

// The three things a stand-in git does after it records its call (recordsItsCall): list the
// repository's HEAD as ls-remote of a public repository does, print what git prints when GitHub
// asks for a credential, and fail the way a git that cannot reach GitHub fails.
var (
	answersTheRefs     = "printf '%s\\tHEAD\\n' " + strings.Repeat("1", 40) + "\nexit 0\n"
	asksForACredential = printsToStderr(githubAsksForACredential...) + "exit 128\n"
	cannotConnect      = printsToStderr("fatal: unable to access '"+theURL+"/': Could not resolve host: github.com") + "exit 128\n"
)

// printsToStderr is a line of sh that prints lines to standard error, each quoted for the shell.
func printsToStderr(lines ...string) string {
	quoted := make([]string, len(lines))
	for i, line := range lines {
		quoted[i] = "'" + strings.ReplaceAll(line, "'", `'\''`) + "'"
	}
	return "printf '%s\\n' " + strings.Join(quoted, " ") + " >&2\n"
}

// writeProgram writes body as an executable sh script called name in dir, which a test puts in
// front of PATH: the guard looks its programs up on the PATH of the environment it was given.
func writeProgram(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o700); err != nil {
		t.Fatal(err)
	}
}

// pathWith is the PATH entry of an environment whose programs are first looked up in dir and
// then on the test's own PATH, where the shared check's bash and cat are.
func pathWith(dir string) string {
	return "PATH=" + dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

// spentRateLimit is a gh that answers the guard's visibility read as GitHub answers it once the
// installation's core limit is spent: 403, X-RateLimit-Remaining 0 and the reset, the status line
// and the headers first as --include prints them, then the body and gh's own error, exit 1. Each
// read appends a line to the file reads names, which is how a test counts them.
func spentRateLimit(reset time.Time, reads string) string {
	return "echo read >> '" + reads + "'\n" +
		"printf '%s\\r\\n' 'HTTP/2.0 403 Forbidden' 'Content-Type: application/json; charset=utf-8' " +
		"'X-Ratelimit-Limit: 15000' 'X-Ratelimit-Remaining: 0' " +
		fmt.Sprintf("'X-Ratelimit-Reset: %d' ", reset.Unix()) +
		"'X-Ratelimit-Resource: core' 'X-Ratelimit-Used: 15000' ''\n" +
		`printf '%s\n' '{"message":"API rate limit exceeded for installation ID 1.","status":"403"}'` + "\n" +
		printsToStderr("gh: API rate limit exceeded for installation ID 1. (HTTP 403)") +
		"exit 1\n"
}

// recordsItsCall is the start of a stand-in git: it appends to record one line per call, with
// its arguments, the variables the read without REST sets or must not inherit, and the directory
// it runs in, each as key=value and separated by tabs (readCalls).
func recordsItsCall(record string) string {
	return `printf 'argv=%s\tprompt=%s\taskpass=%s\tnosystem=%s\tglobal=%s\tcount=%s\tkey0=%s\tparameters=%s\tgitdir=%s\tprotocol=%s\tlang=%s\tdir=%s\n' ` +
		`"$*" "${GIT_TERMINAL_PROMPT-unset}" "${GIT_ASKPASS-unset}" "${GIT_CONFIG_NOSYSTEM-unset}" ` +
		`"${GIT_CONFIG_GLOBAL-unset}" "${GIT_CONFIG_COUNT-unset}" "${GIT_CONFIG_KEY_0-unset}" ` +
		`"${GIT_CONFIG_PARAMETERS-unset}" "${GIT_DIR-unset}" "${GIT_ALLOW_PROTOCOL-unset}" ` +
		`"${LC_ALL-unset}" "$(pwd -P)" >> '` + record + "'\n"
}

// readCalls is each call recordsItsCall wrote to record, as its keys and values, and none where
// git never ran.
func readCalls(t *testing.T, record string) []map[string]string {
	t.Helper()
	content, err := os.ReadFile(record)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls []map[string]string
	for line := range strings.Lines(string(content)) {
		call := map[string]string{}
		for field := range strings.SplitSeq(strings.TrimRight(line, "\n"), "\t") {
			key, value, _ := strings.Cut(field, "=")
			call[key] = value
		}
		calls = append(calls, call)
	}
	return calls
}

// countLines is how many lines the file at path holds, and 0 where there is no file.
func countLines(t *testing.T, path string) int {
	t.Helper()
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(content), "\n")
}

// When GitHub refuses the visibility read for a spent core limit whose reset is 30 minutes away,
// past the wait bound, the guard reads the visibility through git's ls-remote, which costs no
// REST request. An answer is public, and the push is scanned; GitHub asking for a credential is
// private, and the push is not; any other failure refuses the push, naming the spent limit and
// its reset. In every case gh is read once and nothing is waited for, and git is run once, with
// the credential helper emptied on its command line and with GIT_TERMINAL_PROMPT=0.
func TestLsRemoteReadsTheVisibilityWhenTheRateLimitIsSpent(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	for _, tc := range []struct {
		name string
		// git is what the stand-in git does after it records its call.
		git string
		// check reads the push's answer and the sources the guard reported.
		check func(t *testing.T, err error, sources []VisibilitySource)
	}{
		{
			name: "ls-remote answers",
			git:  answersTheRefs,
			check: func(t *testing.T, err error, sources []VisibilitySource) {
				if refused := refusal(t, err); refused.By != byRules {
					t.Errorf("the push was refused by %q, want the rules: a public destination is scanned", refused.By)
				}
				if want := []VisibilitySource{VisibilityFromLsRemote}; !slices.Equal(sources, want) {
					t.Errorf("the guard reported the sources %q, want %q", sources, want)
				}
			},
		},
		{
			name: "GitHub asks for a credential",
			git:  asksForACredential,
			check: func(t *testing.T, err error, sources []VisibilitySource) {
				if err != nil {
					t.Fatalf("the push to a private destination: %v; want it passed, unscanned", err)
				}
				if want := []VisibilitySource{VisibilityFromLsRemote}; !slices.Equal(sources, want) {
					t.Errorf("the guard reported the sources %q, want %q", sources, want)
				}
			},
		},
		{
			name: "ls-remote fails another way",
			git:  cannotConnect,
			check: func(t *testing.T, err error, sources []VisibilitySource) {
				says(t, setupFailure(t, err),
					"the visibility of "+theRepo+" could not be read",
					"the installation's rate limit is spent until "+reset.Format(time.RFC3339),
					"Could not resolve host")
				if len(sources) != 0 {
					t.Errorf("the guard reported the sources %q for a read that failed, want none", sources)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			reads, record := filepath.Join(t.TempDir(), "reads"), filepath.Join(t.TempDir(), "record")
			writeProgram(t, bin, "gh", spentRateLimit(reset, reads))
			writeProgram(t, bin, "git", recordsItsCall(record)+tc.git)
			clock := &visibilityTestClock{now: now}
			var sources []VisibilitySource
			guard := New(Options{
				Check:        leakguardtest.Write(t, t.TempDir(), passes),
				Clock:        clock,
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
			})
			// A ref the push deletes, named for a host the rules refuse: its name is the push's one
			// record, so a scan refuses the push and no scan passes it, and no git reads a commit.
			err := guard.Push(bound(t), PushRequest{
				Dir:     t.TempDir(),
				Remote:  "origin",
				URL:     theURL,
				Updates: []Update{{LocalRef: "refs/heads/work", LocalSHA: noSHA, RemoteRef: "refs/heads/legion/" + theHost, RemoteSHA: noSHA}},
			})
			tc.check(t, err, sources)
			if got := countLines(t, reads); got != 1 {
				t.Errorf("gh read the visibility %d times, want once", got)
			}
			if len(clock.waits) != 0 {
				t.Errorf("the waits are %v, want none: the reset is past the wait bound", clock.waits)
			}
			calls := readCalls(t, record)
			if len(calls) != 1 {
				t.Fatalf("git ran %d times, want the one ls-remote", len(calls))
			}
			want := "--no-replace-objects -c core.quotePath=false -c core.hooksPath=/dev/null -c core.fsmonitor=false " +
				"-c credential.helper= ls-remote " + theURL + " HEAD"
			if calls[0]["argv"] != want {
				t.Errorf("git ran as\n%s\nwant\n%s", calls[0]["argv"], want)
			}
			if calls[0]["prompt"] != "0" {
				t.Errorf("git ran with GIT_TERMINAL_PROMPT=%s, want 0", calls[0]["prompt"])
			}
		})
	}
}

// The read without REST carries no credential of the pod's. Of the caller's GIT_ variables only
// the transport one reaches git, so neither the GIT_CONFIG_COUNT block a pod's git gets its
// credential helper from, nor GIT_CONFIG_PARAMETERS holding an extra Authorization header, nor a
// GIT_DIR whose configuration could hold either does. git reads no system or user configuration
// file, runs in / outside any repository, has an askpass that answers nothing, and prints its
// messages in the English lsRemoteRefused reads.
func TestLsRemoteReadsWithNoCredentialOfThePods(t *testing.T) {
	bin, record := t.TempDir(), filepath.Join(t.TempDir(), "record")
	writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()),
		pathWith(bin),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0=!invented-helper",
		"GIT_CONFIG_PARAMETERS='http.extraheader'='Authorization: Basic aW52ZW50ZWQ6aW52ZW50ZWQ='",
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_DIR="+t.TempDir(),
		"GIT_ALLOW_PROTOCOL=https",
		"LC_ALL=fr_FR.UTF-8",
	)})
	visibility, err := guard.lsRemote(bound(t), githubOrigin, theRepo)
	if err != nil || visibility != Public {
		t.Fatalf("read %q, %v; want public", visibility, err)
	}
	calls := readCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("git ran %d times, want once", len(calls))
	}
	for key, want := range map[string]string{
		"prompt":     "0",
		"nosystem":   "1",
		"global":     "/dev/null",
		"count":      "unset",
		"key0":       "unset",
		"parameters": "unset",
		"gitdir":     "unset",
		"protocol":   "https",
		"lang":       "C",
		"dir":        "/",
	} {
		if got := calls[0][key]; got != want {
			t.Errorf("git ran with %s=%q, want %q", key, got, want)
		}
	}
	if !strings.Contains(" "+calls[0]["argv"]+" ", " -c credential.helper= ls-remote "+theURL+" ") {
		t.Errorf("git ran as %q, want the credential helper emptied in front of ls-remote %s", calls[0]["argv"], theURL)
	}
	askpass := calls[0]["askpass"]
	if askpass == "unset" || askpass == "" {
		t.Fatalf("git ran with no askpass program: %q", askpass)
	}
	if status, _, err := guard.run(bound(t), child{argv: []string{askpass}}); err != nil || status == 0 {
		t.Errorf("the askpass program %s answered %d, %v; want a program that answers no credential", askpass, status, err)
	}
}

// An answer of the read without REST is kept by the rules a gh answer is (Guard.keepVisibility):
// public for the pod's life, so the next call reads neither gh nor git, and private not at all,
// so the next call reads both again. Each call builds a guard of its own, as every call site
// does.
func TestVisibilityKeepsAnLsRemoteAnswerAsItKeepsGhs(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	for _, tc := range []struct {
		name    string
		git     string
		want    Visibility
		reads   int
		sources []VisibilitySource
	}{
		{
			name:    "public",
			git:     answersTheRefs,
			want:    Public,
			reads:   1,
			sources: []VisibilitySource{VisibilityFromLsRemote, VisibilityFromCache},
		},
		{
			name:    "private",
			git:     asksForACredential,
			want:    Private,
			reads:   2,
			sources: []VisibilitySource{VisibilityFromLsRemote, VisibilityFromLsRemote},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, state := t.TempDir(), t.TempDir()
			reads, record := filepath.Join(t.TempDir(), "reads"), filepath.Join(t.TempDir(), "record")
			writeProgram(t, bin, "gh", spentRateLimit(reset, reads))
			writeProgram(t, bin, "git", recordsItsCall(record)+tc.git)
			var sources []VisibilitySource
			for range 2 {
				visibility, err := New(Options{
					Clock:        &visibilityTestClock{now: now},
					OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
					Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+state),
				}).Visibility(bound(t), theRepo)
				if err != nil || visibility != tc.want {
					t.Fatalf("read %q, %v; want %s", visibility, err, tc.want)
				}
			}
			if !slices.Equal(sources, tc.sources) {
				t.Errorf("the guards reported the sources %q, want %q", sources, tc.sources)
			}
			if got := countLines(t, reads); got != tc.reads {
				t.Errorf("gh read the visibility %d times, want %d", got, tc.reads)
			}
			if got := len(readCalls(t, record)); got != tc.reads {
				t.Errorf("git ran %d times, want %d", got, tc.reads)
			}
		})
	}
}

// Only git's own line for a server that asked for a credential, naming the URL's origin, after
// exit 128, reads as private. Each near miss can come from a public repository, and reads as no
// answer.
func TestLsRemoteReadsOnlyARequestForACredentialAsPrivate(t *testing.T) {
	asked := strings.Join(githubAsksForACredential, "\n") + "\n"
	for _, tc := range []struct {
		name    string
		status  int
		printed string
		want    bool
	}{
		{name: "git asks github.com for a user name", status: 128, printed: asked, want: true},
		{
			name:    "a credential was sent and refused",
			status:  128,
			printed: "remote: Invalid username or token.\nfatal: Authentication failed for '" + theURL + "/'\n",
		},
		{
			name:    "a 403",
			status:  128,
			printed: "fatal: unable to access '" + theURL + "/': The requested URL returned error: 403\n",
		},
		{
			name:    "git asks a proxy for a user name",
			status:  128,
			printed: "fatal: could not read Username for 'http://proxy.invented:3128': terminal prompts disabled\n",
		},
		{
			name:    "git asks a host whose name begins with github.com",
			status:  128,
			printed: "fatal: could not read Username for 'https://github.com.invented': terminal prompts disabled\n",
		},
		{
			name:    "the server wrote the line",
			status:  128,
			printed: "remote: " + githubAsksForACredential[1] + "\nfatal: the remote end hung up unexpectedly\n",
		},
		{name: "the line with another exit status", status: 1, printed: asked},
		{
			name:    "a host git cannot resolve",
			status:  128,
			printed: "fatal: unable to access '" + theURL + "/': Could not resolve host: github.com\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lsRemoteRefused(githubOrigin, tc.status, tc.printed); got != tc.want {
				t.Errorf("lsRemoteRefused = %v, want %v", got, tc.want)
			}
		})
	}
}

// pktLine is payload as one pkt-line of git's protocol: its length, the four hex digits
// included, then the payload.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// The lines lsRemote reads are the ones a real git prints. A server of the test's own stands in
// for GitHub's smart HTTP endpoint, and the git on this machine reads it with the environment the
// guard gives it: a repository whose refs it lists is public, one it answers 401 for is private,
// and a 403 or a 404 is no answer.
func TestLsRemoteReadsWhatARealGitPrints(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git on this machine")
	}
	head := strings.Repeat("1", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch owner {
		case "public":
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("# service=git-upload-pack\n")+"0000"+
				pktLine(head+" HEAD\x00agent=invented\n")+pktLine(head+" refs/heads/main\n")+"0000")
		case "private":
			w.Header().Set("WWW-Authenticate", `Basic realm="invented"`)
			w.WriteHeader(http.StatusUnauthorized)
		case "forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	// The process's own git may use local files alone (TestMain); this test's server is http.
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()), "GIT_ALLOW_PROTOCOL=http")})
	for _, tc := range []struct {
		owner string
		want  Visibility
		// fails is what the error says where the read answers nothing.
		fails string
	}{
		{owner: "public", want: Public},
		{owner: "private", want: Private},
		{owner: "forbidden", fails: "The requested URL returned error: 403"},
		{owner: "missing", fails: "not found"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			visibility, err := guard.lsRemote(bound(t), server.URL, tc.owner+"/invented-repo")
			if tc.fails == "" {
				if err != nil || visibility != tc.want {
					t.Fatalf("read %q, %v; want %s", visibility, err, tc.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("read %q, want no answer", visibility)
			}
			if !strings.Contains(err.Error(), tc.fails) {
				t.Errorf("the error %q does not say %q", err, tc.fails)
			}
		})
	}
}

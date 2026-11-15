package leakguard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/fixtureenv"
	"github.com/sjawhar/legion/daemon/internal/leakguard/leakguardtest"
)

// theRepo is the repository these tests read the visibility of, theURL the HTTPS URL the read
// with no credential hands git for it, and theResets what that read puts after the package's
// pins and before ls-remote.
const (
	theRepo   = "invented-org/invented-repo"
	theURL    = githubOrigin + "/" + theRepo + ".git"
	theResets = "-c credential.helper= -c http.extraHeader= -c http." + theURL + ".extraHeader= -c http." + theURL + "//.extraHeader= " +
		"-c http.proactiveAuth=none -c http." + theURL + "//.proactiveAuth=none"
)

// githubAsksForACredential is what git 2.47.3 printed, exit 128, for an anonymous ls-remote of a
// private repository on github.com, with the guard's askpass and terminal prompt settings: the
// askpass answers nothing, and the terminal prompt is off. GitHub's 401 for a public repository
// under unusual traffic makes git print the same.
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

// shQuote is s as one word of sh.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// printsToStderr is a line of sh that prints lines to standard error, each quoted for the shell.
func printsToStderr(lines ...string) string {
	quoted := make([]string, len(lines))
	for i, line := range lines {
		quoted[i] = shQuote(line)
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

// ghStandIn is a stand-in gh whose REST visibility read does what rest says and whose GraphQL
// read does what graphql says. The GraphQL read is the one whose fourth argument is graphql
// (VisibilityGraphQLArgs); the REST read's is the repos/ endpoint.
func ghStandIn(rest, graphql string) string {
	return "if [ \"$4\" = graphql ]; then\n" + graphql + "fi\n" + rest
}

// graphQLReset is when the GraphQL limit of the stand-in GraphQL reads resets.
var graphQLReset = time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)

// graphQLRead is the GraphQL read of a stand-in gh. It appends its call to record, with its
// arguments and the two variables the guard sets on it (readCalls reads the line), then prints
// status, GraphQL's rate-limit headers with remaining points left, a blank line and body, as `gh
// api --include` prints them, then gh's own error line where stderr is one, and exits with exit.
func graphQLRead(record, status string, remaining int, body, stderr string, exit int) string {
	script := `printf 'argv=%s\tread=%s\thost=%s\n' "$*" "${` + VisibilityReadVar + `-unset}" "${GH_HOST-unset}" >> ` + shQuote(record) + "\n" +
		"printf '%s\\r\\n' " + shQuote(status) + " 'Content-Type: application/json; charset=utf-8' 'X-Ratelimit-Limit: 12500' " +
		fmt.Sprintf("'X-Ratelimit-Remaining: %d' 'X-Ratelimit-Reset: %d' ", remaining, graphQLReset.Unix()) +
		"'X-Ratelimit-Resource: graphql' ''\n" +
		"printf '%s' " + shQuote(body) + "\n"
	if stderr != "" {
		script += printsToStderr(stderr)
	}
	return script + fmt.Sprintf("exit %d\n", exit)
}

// graphQLAnswers is a GraphQL read that answers visibility, as GitHub answers for a repository
// the installation can read.
func graphQLAnswers(record, visibility string) string {
	return graphQLRead(record, "HTTP/2.0 200 OK", 12221, `{"data":{"repository":{"visibility":"`+visibility+`"}}}`, "", 0)
}

// graphQLRateLimited is a GraphQL read GitHub refuses for GraphQL's own spent limit: the status
// 200 with no points left and an error of type RATE_LIMITED, then gh's error, exit 1.
func graphQLRateLimited(record string) string {
	const message = "API rate limit already exceeded for installation ID 1."
	return graphQLRead(record, "HTTP/2.0 200 OK", 0, `{"errors":[{"type":"RATE_LIMITED","message":"`+message+`"}]}`, "gh: "+message, 1)
}

// graphQLNotFound is a GraphQL read of a repository GitHub could not resolve, as GitHub answered
// it on 2026-11-15 for a name no repository has: no repository, an error of type NOT_FOUND, and
// gh's error, exit 1.
func graphQLNotFound(record string) string {
	const message = "Could not resolve to a Repository with the name '" + theRepo + "'."
	return graphQLRead(record, "HTTP/2.0 200 OK", 12221,
		`{"data":{"repository":null},"errors":[{"type":"NOT_FOUND","path":["repository"],"locations":[{"line":1,"column":42}],"message":"`+message+`"}]}`,
		"gh: "+message, 1)
}

// recordsItsCall is the start of a stand-in git: it appends to record one line per call, with
// its arguments, the variables the read with no credential sets, must not inherit or must keep,
// what its HOME holds, and the directory it runs in, each as key=value and separated by tabs
// (readCalls).
func recordsItsCall(record string) string {
	return `printf 'argv=%s\tprompt=%s\taskpass=%s\tnosystem=%s\tglobal=%s\tcount=%s\tkey0=%s\tparameters=%s\tgitdir=%s\tprotocol=%s\tcainfo=%s\tcapath=%s\tproxy=%s\tlang=%s\thome=%s\tnetrc=%s\thomeholds=%s\tdir=%s\n' ` +
		`"$*" "${GIT_TERMINAL_PROMPT-unset}" "${GIT_ASKPASS-unset}" "${GIT_CONFIG_NOSYSTEM-unset}" ` +
		`"${GIT_CONFIG_GLOBAL-unset}" "${GIT_CONFIG_COUNT-unset}" "${GIT_CONFIG_KEY_0-unset}" ` +
		`"${GIT_CONFIG_PARAMETERS-unset}" "${GIT_DIR-unset}" "${GIT_ALLOW_PROTOCOL-unset}" ` +
		`"${GIT_SSL_CAINFO-unset}" "${GIT_SSL_CAPATH-unset}" "${HTTPS_PROXY-unset}" ` +
		`"${LC_ALL-unset}" "${HOME-unset}" "${NETRC-unset}" "$(ls -A "${HOME-/nonexistent}" 2>&1 | tr '\n' ' ')" ` +
		`"$(pwd -P)" >> '` + record + "'\n"
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
// past the wait bound, the guard reads the visibility through GitHub's GraphQL API, and only where
// GraphQL refuses the read for a rate limit of its own through git's ls-remote. GraphQL's public
// is scanned, and its private and internal are not; ls-remote's public is scanned. A GraphQL that
// could not resolve the repository, and an ls-remote that fails, git's own line for a server that
// asked for a credential included, refuse the push, naming the spent limit and its reset. In
// every case gh's REST read and its GraphQL read run once each, as the guard's own exact argv
// with the variable naming the repository, and nothing is waited for. git runs only after
// GraphQL's rate limit, once, with the credential helper, the extra headers and
// http.proactiveAuth emptied after the package's pins and before ls-remote, and with
// GIT_TERMINAL_PROMPT=0.
func TestVisibilityWithoutRESTDecidesThePush(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	scanned := func(source VisibilitySource) func(*testing.T, error, []VisibilitySource) {
		return func(t *testing.T, err error, sources []VisibilitySource) {
			t.Helper()
			if refused := refusal(t, err); refused.By != byRules {
				t.Errorf("the push was refused by %q, want the rules: a public destination is scanned", refused.By)
			}
			if want := []VisibilitySource{source}; !slices.Equal(sources, want) {
				t.Errorf("the guard reported the sources %q, want %q", sources, want)
			}
		}
	}
	unscanned := func(t *testing.T, err error, sources []VisibilitySource) {
		t.Helper()
		if err != nil {
			t.Fatalf("the push to a private or internal destination: %v; want it passed, unscanned", err)
		}
		if want := []VisibilitySource{VisibilityFromGraphQL}; !slices.Equal(sources, want) {
			t.Errorf("the guard reported the sources %q, want %q", sources, want)
		}
	}
	refusedSaying := func(wants ...string) func(*testing.T, error, []VisibilitySource) {
		return func(t *testing.T, err error, sources []VisibilitySource) {
			t.Helper()
			says(t, setupFailure(t, err), append([]string{
				"the visibility of " + theRepo + " could not be read",
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
			}, wants...)...)
			if len(sources) != 0 {
				t.Errorf("the guard reported the sources %q for a read that failed, want none", sources)
			}
		}
	}
	answers := func(visibility string) func(string) string {
		return func(record string) string { return graphQLAnswers(record, visibility) }
	}
	for _, tc := range []struct {
		name string
		// graphql is what the stand-in gh's GraphQL read does, given the file it records its call
		// in, and git what the stand-in git does after it records its call.
		graphql func(record string) string
		git     string
		// gitRuns is how many times git runs: once where GraphQL is rate-limited, else never.
		gitRuns int
		// check reads the push's answer and the sources the guard reported.
		check func(t *testing.T, err error, sources []VisibilitySource)
	}{
		{name: "GraphQL answers public", graphql: answers("PUBLIC"), git: answersTheRefs, check: scanned(VisibilityFromGraphQL)},
		{name: "GraphQL answers private", graphql: answers("PRIVATE"), git: answersTheRefs, check: unscanned},
		{name: "GraphQL answers internal", graphql: answers("INTERNAL"), git: answersTheRefs, check: unscanned},
		{
			name:    "GraphQL could not resolve the repository",
			graphql: graphQLNotFound,
			git:     answersTheRefs,
			check:   refusedSaying("GraphQL could not answer", "NOT_FOUND"),
		},
		{
			name:    "GraphQL is rate-limited and ls-remote answers",
			graphql: graphQLRateLimited,
			git:     answersTheRefs,
			gitRuns: 1,
			check:   scanned(VisibilityFromLsRemote),
		},
		{
			name:    "GraphQL is rate-limited and GitHub asks ls-remote for a credential",
			graphql: graphQLRateLimited,
			git:     asksForACredential,
			gitRuns: 1,
			check:   refusedSaying("GraphQL's rate limit is spent too", "could not read Username"),
		},
		{
			name:    "GraphQL is rate-limited and ls-remote fails another way",
			graphql: graphQLRateLimited,
			git:     cannotConnect,
			gitRuns: 1,
			check:   refusedSaying("GraphQL's rate limit is spent too", "Could not resolve host"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			reads, graphql, record := filepath.Join(records, "reads"), filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", ghStandIn(spentRateLimit(reset, reads), tc.graphql(graphql)))
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
				t.Errorf("gh's REST read ran %d times, want once", got)
			}
			if len(clock.waits) != 0 {
				t.Errorf("the waits are %v, want none: the reset is past the wait bound", clock.waits)
			}
			queries := readCalls(t, graphql)
			if len(queries) != 1 {
				t.Fatalf("gh's GraphQL read ran %d times, want once", len(queries))
			}
			if want := strings.Join(VisibilityGraphQLArgs(theRepo), " "); queries[0]["argv"] != want {
				t.Errorf("gh's GraphQL read ran as\n%s\nwant\n%s", queries[0]["argv"], want)
			}
			if queries[0]["read"] != theRepo || queries[0]["host"] != "github.com" {
				t.Errorf("gh's GraphQL read ran with %s=%q and GH_HOST=%q, want %q and github.com", VisibilityReadVar, queries[0]["read"], queries[0]["host"], theRepo)
			}
			calls := readCalls(t, record)
			if len(calls) != tc.gitRuns {
				t.Fatalf("git ran %d times, want %d", len(calls), tc.gitRuns)
			}
			if tc.gitRuns == 0 {
				return
			}
			if want := strings.Join(gitPins, " ") + " " + theResets + " ls-remote " + theURL + " HEAD"; calls[0]["argv"] != want {
				t.Errorf("git ran as\n%s\nwant\n%s", calls[0]["argv"], want)
			}
			if calls[0]["prompt"] != "0" {
				t.Errorf("git ran with GIT_TERMINAL_PROMPT=%s, want 0", calls[0]["prompt"])
			}
		})
	}
}

// Of what GraphQL says, PUBLIC, PRIVATE and INTERNAL alone are an answer, and a rate limit of
// GraphQL's own alone sends the read on to ls-remote: an error of type RATE_LIMITED, the status
// 403 with no points left, and GitHub's secondary rate limit. Everything else refuses the call,
// naming the spent core limit, its reset and what GraphQL said, and runs no ls-remote: a
// visibility GitHub does not define or in another case, NOT_FOUND, an error beside the data, no
// repository, no data, a body that is no JSON, a refused credential and a gh that cannot reach
// GitHub. None is read as private or public.
func TestVisibilityTakesOnlyAnAnswerGraphQLGives(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	answers := func(visibility string) func(string) string {
		return func(record string) string { return graphQLAnswers(record, visibility) }
	}
	prints := func(status string, remaining int, body, stderr string) func(string) string {
		return func(record string) string { return graphQLRead(record, status, remaining, body, stderr, 1) }
	}
	for _, tc := range []struct {
		name    string
		graphql func(record string) string
		// want is the answer, empty where the call is refused; lsRemote is whether the read went
		// on to ls-remote, whose stand-in answers public; fails is what a refusal says of GraphQL.
		want     Visibility
		lsRemote bool
		fails    string
	}{
		{name: "PUBLIC", graphql: answers("PUBLIC"), want: Public},
		{name: "PRIVATE", graphql: answers("PRIVATE"), want: Private},
		{name: "INTERNAL", graphql: answers("INTERNAL"), want: Internal},
		{name: "a visibility in lower case", graphql: answers("public"), fails: `"public" as the visibility`},
		{name: "an empty visibility", graphql: answers(""), fails: `"" as the visibility`},
		{name: "a visibility GitHub does not define", graphql: answers("SECRET"), fails: `"SECRET" as the visibility`},
		{name: "NOT_FOUND", graphql: graphQLNotFound, fails: "NOT_FOUND: Could not resolve to a Repository"},
		{
			name: "an error beside the data",
			graphql: prints("HTTP/2.0 200 OK", 12221,
				`{"data":{"repository":{"visibility":"PUBLIC"}},"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by integration"}]}`,
				"gh: Resource not accessible by integration"),
			fails: "FORBIDDEN: Resource not accessible by integration",
		},
		{
			name: "no repository",
			graphql: func(record string) string {
				return graphQLRead(record, "HTTP/2.0 200 OK", 12221, `{"data":{"repository":null}}`, "", 0)
			},
			fails: "GraphQL answered no repository",
		},
		{
			name: "no data",
			graphql: func(record string) string {
				return graphQLRead(record, "HTTP/2.0 200 OK", 12221, `{"data":null}`, "", 0)
			},
			fails: "GraphQL answered no repository",
		},
		{
			name:    "a body that is no JSON",
			graphql: prints("HTTP/2.0 502 Bad Gateway", 12221, "<html>bad gateway</html>", "gh: HTTP 502"),
			fails:   "with no GraphQL answer",
		},
		{
			name:    "a refused credential",
			graphql: prints("HTTP/2.0 401 Unauthorized", 12221, `{"message":"Bad credentials","status":"401"}`, "gh: Bad credentials (HTTP 401)"),
			fails:   "exited 1",
		},
		{
			name: "a gh that cannot reach GitHub",
			graphql: func(record string) string {
				return "echo read >> " + shQuote(record) + "\n" + printsToStderr("error connecting to api.github.com") + "exit 1\n"
			},
			fails: "error connecting to api.github.com",
		},
		{name: "RATE_LIMITED", graphql: graphQLRateLimited, want: Public, lsRemote: true},
		{
			name:     "the status 403 with no points left",
			graphql:  prints("HTTP/2.0 403 Forbidden", 0, `{"message":"API rate limit exceeded for installation ID 1."}`, "gh: API rate limit exceeded for installation ID 1. (HTTP 403)"),
			want:     Public,
			lsRemote: true,
		},
		{
			name: "the secondary rate limit",
			graphql: prints("HTTP/2.0 403 Forbidden", 12221,
				`{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`,
				"gh: You have exceeded a secondary rate limit. (HTTP 403)"),
			want:     Public,
			lsRemote: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, records := t.TempDir(), t.TempDir()
			graphql, record := filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", tc.graphql(graphql))
			writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
			var sources []VisibilitySource
			visibility, err := New(Options{
				Clock:        &visibilityTestClock{now: now},
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					return "", &rateLimitError{reset: reset}
				},
			}).Visibility(bound(t), theRepo)
			if got := countLines(t, graphql); got != 1 {
				t.Errorf("gh's GraphQL read ran %d times, want once", got)
			}
			gitRuns, source := 0, VisibilityFromGraphQL
			if tc.lsRemote {
				gitRuns, source = 1, VisibilityFromLsRemote
			}
			if got := len(readCalls(t, record)); got != gitRuns {
				t.Errorf("git ran %d times, want %d", got, gitRuns)
			}
			if tc.want != "" {
				if err != nil || visibility != tc.want {
					t.Fatalf("read %q, %v; want %s", visibility, err, tc.want)
				}
				if want := []VisibilitySource{source}; !slices.Equal(sources, want) {
					t.Errorf("the guard reported the sources %q, want %q", sources, want)
				}
				return
			}
			if err == nil {
				t.Fatalf("read %q, want the call refused", visibility)
			}
			for _, want := range []string{
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
				"GraphQL could not answer: ",
				tc.fails,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			}
			if len(sources) != 0 {
				t.Errorf("the guard reported the sources %q for a read that failed, want none", sources)
			}
		})
	}
}

// A GraphQL read that the caller's bound stops refuses the call at once, naming the spent limit,
// its reset and gh's stop, and does not go on to ls-remote: no bound is left for it. The bound
// ends only once the stand-in gh has started, so it is a running gh the bound stops.
func TestVisibilityRefusesAGraphQLReadTheCallersBoundStops(t *testing.T) {
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(30 * time.Minute)
	bin, records := t.TempDir(), t.TempDir()
	started, record := filepath.Join(records, "started"), filepath.Join(records, "git")
	writeProgram(t, bin, "gh", ": > "+shQuote(started)+"\nsleep 30\n")
	writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	ctx := &deadlineOnceStarted{Context: deadline, done: make(chan struct{})}
	whenWritten(bound(t), started, func() {
		<-deadline.Done()
		close(ctx.done)
	})
	began := time.Now()
	_, err := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			return "", &rateLimitError{reset: reset}
		},
	}).Visibility(ctx, theRepo)
	answeredWithin10s(t, began)
	if err == nil {
		t.Fatal("the visibility read passed while gh never answered")
	}
	for _, want := range []string{
		"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
		"GraphQL could not answer: gh ran ",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not say %q", err, want)
		}
	}
	if got := stopBound(t, err); got > 2 {
		t.Errorf("gh was stopped by a bound of %d s, want the caller's deadline of 2 s", got)
	}
	if calls := readCalls(t, record); len(calls) != 0 {
		t.Errorf("git ran %d times, want none: the caller's bound is spent", len(calls))
	}
}

// The read with no credential carries no credential of the pod's. Of the caller's GIT_ variables
// only the transport one reaches git, so neither the GIT_CONFIG_COUNT block a pod's git gets its
// credential helper from, nor GIT_CONFIG_PARAMETERS holding an extra Authorization header and
// http.proactiveAuth, nor a GIT_DIR whose configuration could hold either does. Neither the
// caller's HOME, whose .netrc libcurl would read, nor NETRC does: git's HOME is an empty
// directory, made for the read and removed after it. git reads no system or user configuration
// file, runs in / outside any repository, has the credential helper, the extra headers and
// http.proactiveAuth emptied in front of ls-remote, has an askpass that answers nothing, and
// prints its messages in English.
func TestLsRemoteReadsWithNoCredentialOfThePods(t *testing.T) {
	bin, record, home := t.TempDir(), filepath.Join(t.TempDir(), "record"), t.TempDir()
	writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
	netrc := "machine github.com\nlogin invented-user\npassword invented-password\n"
	if err := os.WriteFile(filepath.Join(home, ".netrc"), []byte(netrc), 0o600); err != nil {
		t.Fatal(err)
	}
	guard := New(Options{Env: append(fixtureenv.Isolated(home),
		pathWith(bin),
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=credential.https://github.com.helper",
		"GIT_CONFIG_VALUE_0=!invented-helper",
		"GIT_CONFIG_PARAMETERS='http.extraheader'='Authorization: Basic aW52ZW50ZWQ6aW52ZW50ZWQ=' 'http.proactiveauth'='basic'",
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "gitconfig"),
		"GIT_DIR="+t.TempDir(),
		"GIT_ALLOW_PROTOCOL=https",
		"NETRC="+filepath.Join(home, ".netrc"),
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
		"netrc":      "unset",
		"homeholds":  "",
		"dir":        "/",
	} {
		if got := calls[0][key]; got != want {
			t.Errorf("git ran with %s=%q, want %q", key, got, want)
		}
	}
	gitHome := calls[0]["home"]
	if gitHome == "unset" || gitHome == "" || gitHome == home {
		t.Errorf("git ran with HOME=%q, want an empty directory of the read's own, not the caller's %s", gitHome, home)
	} else if _, err := os.Stat(gitHome); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("git's HOME %s is still there after the read (%v), want it removed", gitHome, err)
	}
	if resets := " " + theResets + " ls-remote " + theURL + " "; !strings.Contains(" "+calls[0]["argv"]+" ", resets) {
		t.Errorf("git ran as %q, want the credential helper, the extra headers and http.proactiveAuth emptied in front of ls-remote %s", calls[0]["argv"], theURL)
	}
	askpass := calls[0]["askpass"]
	if askpass == "unset" || askpass == "" {
		t.Fatalf("git ran with no askpass program: %q", askpass)
	}
	if status, _, err := guard.run(bound(t), child{argv: []string{askpass}}); err != nil || status == 0 {
		t.Errorf("the askpass program %s answered %d, %v; want a program that answers no credential", askpass, status, err)
	}
}

// The read with no credential reaches GitHub the way the caller's environment says to, and asks
// for no credential however the caller asks for one. The CA bundle git trusts (GIT_SSL_CAINFO,
// GIT_SSL_CAPATH) and libcurl's proxy (HTTPS_PROXY), none of which carries a credential, reach
// git as the caller set them. A caller's GIT_TERMINAL_PROMPT=1, and a GIT_ASKPASS that would
// print a credential, do not: git sees GIT_TERMINAL_PROMPT=0 and the guard's own askpass.
func TestLsRemoteKeepsTheCallersTransportButNotItsPrompts(t *testing.T) {
	bin, record, askpasses := t.TempDir(), filepath.Join(t.TempDir(), "record"), t.TempDir()
	writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
	writeProgram(t, askpasses, "answers", "echo invented-token\n")
	cainfo, capath := filepath.Join(t.TempDir(), "ca.pem"), t.TempDir()
	guard := New(Options{Env: append(fixtureenv.Isolated(t.TempDir()),
		pathWith(bin),
		"GIT_SSL_CAINFO="+cainfo,
		"GIT_SSL_CAPATH="+capath,
		"HTTPS_PROXY=http://proxy.invented:3128",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_ASKPASS="+filepath.Join(askpasses, "answers"),
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
		"cainfo":  cainfo,
		"capath":  capath,
		"proxy":   "http://proxy.invented:3128",
		"prompt":  "0",
		"askpass": guard.noAnswer,
	} {
		if got := calls[0][key]; got != want {
			t.Errorf("git ran with %s=%q, want %q", key, got, want)
		}
	}
}

// No netrc credential reaches the real git of the read with no credential, whether the .netrc is
// in the caller's home or NETRC names it (libcurl reads NETRC from curl 8.16; an older libcurl
// ignores it). A server of the test's own answers every request 401 with a challenge, as GitHub
// answers an anonymous read of a private repository, and both netrc files name that server's
// host. libcurl answers such a challenge with a netrc credential it finds by sending the request
// again with an Authorization header (measured by hand with git 2.47.3 and the caller's HOME);
// here the server sees no such header, and the read is no answer.
func TestLsRemoteSendsNoNetrcCredential(t *testing.T) {
	git := realGit(t)
	var mu sync.Mutex
	var requests, authorized int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		if r.Header.Get("Authorization") != "" {
			authorized++
		}
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="invented"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()
	host, _, _ := strings.Cut(strings.TrimPrefix(server.URL, "http://"), ":")
	netrc := []byte("machine " + host + "\nlogin invented-user\npassword invented-password\n")
	home, named := t.TempDir(), filepath.Join(t.TempDir(), "netrc")
	for _, path := range []string{filepath.Join(home, ".netrc"), named} {
		if err := os.WriteFile(path, netrc, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	guard := New(Options{Env: loopbackEnv(home, pathWith(filepath.Dir(git)), "NETRC="+named)})
	visibility, err := guard.lsRemote(bound(t), server.URL, "private/invented-repo")
	if err == nil {
		t.Fatalf("read %q, want no answer: a 401 is not one", visibility)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests == 0 {
		t.Fatal("git sent the server no request")
	}
	if authorized != 0 {
		t.Errorf("%d of git's %d requests carried an Authorization header: a netrc credential reached the read", authorized, requests)
	}
}

// The read with no credential ends at its own cap when git never answers, inside a caller's bound
// that would let it run longer, and the call is refused naming the spent limit and git's stop by
// a bound no longer than that cap. GraphQL's own limit is spent, which is what sends the read on
// to git.
func TestLsRemoteThatHangsEndsAtItsOwnBound(t *testing.T) {
	saved := lsRemoteBound
	lsRemoteBound = 2 * time.Second
	t.Cleanup(func() { lsRemoteBound = saved })
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(30 * time.Minute)
	bin := t.TempDir()
	writeProgram(t, bin, "gh", graphQLRateLimited(filepath.Join(t.TempDir(), "graphql")))
	writeProgram(t, bin, "git", "sleep 30\n")
	started := time.Now()
	_, err := New(Options{
		Clock: clock,
		Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
		Visibility: func(context.Context, string) (Visibility, error) {
			return "", &rateLimitError{reset: reset}
		},
	}).Visibility(bound(t), theRepo)
	answeredWithin10s(t, started)
	if err == nil {
		t.Fatal("the visibility read passed while git never answered")
	}
	for _, want := range []string{
		"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
		"GraphQL's rate limit is spent too, and git ls-remote could not answer: git ran ",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure %q does not say %q", err, want)
		}
	}
	if got := stopBound(t, err); got > 2 {
		t.Errorf("git was stopped by a bound of %d s, want the read's own %s, not the caller's minute", got, lsRemoteBound)
	}
}

// stopBound is the bound, in whole seconds, that err says a child (git or gh) was stopped by, and
// fails the test where err says no such stop. The bound is read against a ceiling, not for its
// exact seconds: on a loaded machine the child can start late in its bound, which then gives it
// less than the whole.
func stopBound(t *testing.T, err error) int64 {
	t.Helper()
	match := regexp.MustCompile(`(?:git|gh) ran -?\d+ s and was stopped by its bound of (-?\d+) s`).FindStringSubmatch(err.Error())
	if match == nil {
		t.Fatalf("the failure %q says no stop by a bound", err)
	}
	seconds, parseErr := strconv.ParseInt(match[1], 10, 64)
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	return seconds
}

// The read with no credential also ends when the caller's context ends, before its own cap: a
// caller that cancels while git hangs, and one whose deadline comes before lsRemoteBound, each
// get the call back at once, refused naming the spent limit, its reset and git's stop. Nothing is
// kept, so the next call reads the visibility again. lsRemoteBound keeps its own two minutes
// here. Each caller's context ends only once the stand-in git has started, so it is a running git
// the end stops on a loaded machine too. GraphQL's own limit is spent, which is what sends the
// read on to git.
func TestLsRemoteStopsWithTheCallersContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		// caller is the caller's context, given the file the stand-in git writes before it
		// hangs.
		caller func(t *testing.T, started string) context.Context
		// stop checks what the failure says of git's stop.
		stop func(t *testing.T, err error)
	}{
		{
			name: "the caller cancels",
			caller: func(t *testing.T, started string) context.Context {
				ctx, cancel := context.WithCancel(bound(t))
				t.Cleanup(cancel)
				whenWritten(ctx, started, cancel)
				return ctx
			},
			stop: func(t *testing.T, err error) {
				if want := "and was stopped by a cancel"; !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			},
		},
		{
			name: "the caller's deadline comes first",
			caller: func(t *testing.T, started string) context.Context {
				deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				t.Cleanup(cancel)
				ctx := &deadlineOnceStarted{Context: deadline, done: make(chan struct{})}
				whenWritten(bound(t), started, func() {
					<-deadline.Done()
					close(ctx.done)
				})
				return ctx
			},
			stop: func(t *testing.T, err error) {
				if got := stopBound(t, err); got > 2 {
					t.Errorf("git was stopped by a bound of %d s, want the caller's deadline of 2 s, not the read's own %s", got, lsRemoteBound)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
			reset := clock.Now().Add(30 * time.Minute)
			bin, started := t.TempDir(), filepath.Join(t.TempDir(), "started")
			writeProgram(t, bin, "gh", graphQLRateLimited(filepath.Join(t.TempDir(), "graphql")))
			writeProgram(t, bin, "git", ": > "+shQuote(started)+"\nsleep 30\n")
			guard := New(Options{
				Clock: clock,
				Env:   append(fixtureenv.Isolated(t.TempDir()), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					return "", &rateLimitError{reset: reset}
				},
			})
			ctx := tc.caller(t, started)
			began := time.Now()
			_, err := guard.Visibility(ctx, theRepo)
			answeredWithin10s(t, began)
			if err == nil {
				t.Fatal("the visibility read passed while git never answered")
			}
			for _, want := range []string{
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
				"GraphQL's rate limit is spent too, and git ls-remote could not answer: git ran ",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			}
			tc.stop(t, err)
			if _, err := os.Stat(guard.visibilityCachePath(theRepo)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a visibility was kept for the refused read (%v), want none", err)
			}
		})
	}
}

// whenWritten calls then once the file at path is there, which it looks for every 10 ms, and
// never where ctx ends first.
func whenWritten(ctx context.Context, path string, then func()) {
	go func() {
		for {
			if _, err := os.Stat(path); err == nil {
				then()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()
}

// deadlineOnceStarted is a caller's context with a deadline, which ends at that deadline but not
// before the test closes done once the stand-in program (git or gh) has started: on a loaded
// machine it can start after a short deadline has passed, and an end before it starts stops
// nothing. Its Deadline and Value are the embedded context's, so the guard reads the deadline as
// the caller's bound.
type deadlineOnceStarted struct {
	context.Context
	done chan struct{}
}

func (c *deadlineOnceStarted) Done() <-chan struct{} { return c.done }

func (c *deadlineOnceStarted) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

// An answer read without REST is kept by the rules a gh answer is (Guard.keepVisibility): public
// for the pod's life, so the next call reads neither gh nor git, and private and internal not at
// all, so the next call reads them again. That holds for GraphQL's answers and for ls-remote's
// public. Each call builds a guard of its own, as every call site does.
func TestVisibilityKeepsAnAnswerWithoutRESTAsItKeepsGhs(t *testing.T) {
	now := time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)
	reset := now.Add(30 * time.Minute)
	answers := func(visibility string) func(string) string {
		return func(record string) string { return graphQLAnswers(record, visibility) }
	}
	for _, tc := range []struct {
		name    string
		graphql func(record string) string
		want    Visibility
		// reads is how many times each of gh's two reads runs over the two calls, and gitRuns how
		// many times git does.
		reads   int
		gitRuns int
		sources []VisibilitySource
	}{
		{
			name:    "GraphQL's public",
			graphql: answers("PUBLIC"),
			want:    Public,
			reads:   1,
			sources: []VisibilitySource{VisibilityFromGraphQL, VisibilityFromCache},
		},
		{
			name:    "GraphQL's private",
			graphql: answers("PRIVATE"),
			want:    Private,
			reads:   2,
			sources: []VisibilitySource{VisibilityFromGraphQL, VisibilityFromGraphQL},
		},
		{
			name:    "GraphQL's internal",
			graphql: answers("INTERNAL"),
			want:    Internal,
			reads:   2,
			sources: []VisibilitySource{VisibilityFromGraphQL, VisibilityFromGraphQL},
		},
		{
			name:    "ls-remote's public",
			graphql: graphQLRateLimited,
			want:    Public,
			reads:   1,
			gitRuns: 1,
			sources: []VisibilitySource{VisibilityFromLsRemote, VisibilityFromCache},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, state, records := t.TempDir(), t.TempDir(), t.TempDir()
			reads, graphql, record := filepath.Join(records, "reads"), filepath.Join(records, "graphql"), filepath.Join(records, "git")
			writeProgram(t, bin, "gh", ghStandIn(spentRateLimit(reset, reads), tc.graphql(graphql)))
			writeProgram(t, bin, "git", recordsItsCall(record)+answersTheRefs)
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
				t.Errorf("gh's REST read ran %d times, want %d", got, tc.reads)
			}
			if got := len(readCalls(t, graphql)); got != tc.reads {
				t.Errorf("gh's GraphQL read ran %d times, want %d", got, tc.reads)
			}
			if got := len(readCalls(t, record)); got != tc.gitRuns {
				t.Errorf("git ran %d times, want %d", got, tc.gitRuns)
			}
		})
	}
}

// pktLine is payload as one pkt-line of git's protocol: its length, the four hex digits
// included, then the payload.
func pktLine(payload string) string {
	return fmt.Sprintf("%04x%s", len(payload)+4, payload)
}

// gitHubStandIn is a server of the test's own that stands in for GitHub's smart HTTP endpoint,
// answering by the repository's owner. "public" lists its refs to a request that carries no
// credential, and refuses one that carries any 401 with a challenge, as GitHub refuses a bad
// credential for a public repository. "private" answers every request 401 with a challenge, as
// GitHub answers an anonymous read of a private repository. "throttled" answers as GitHub did a
// public repository's anonymous clone under unusual traffic in GitHub's community discussion
// #206581: the advertisement's GET is answered, here with protocol version 2's, so git goes on
// to POST its ls-refs command, and the POST is answered 401 with GitHub's challenge. "forged"
// answers the GET with an ERR packet carrying git's own line for a server that asked for a
// credential. "forbidden" answers 403, and any other owner 404.
func gitHubStandIn(t *testing.T) *httptest.Server {
	t.Helper()
	head := strings.Repeat("1", 40)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		switch {
		case owner == "public" && r.Header.Get("Authorization") == "" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("# service=git-upload-pack\n")+"0000"+
				pktLine(head+" HEAD\x00agent=invented\n")+pktLine(head+" refs/heads/main\n")+"0000")
		case owner == "public", owner == "private":
			w.Header().Set("WWW-Authenticate", `Basic realm="invented"`)
			w.WriteHeader(http.StatusUnauthorized)
		case owner == "throttled" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("version 2\n")+pktLine("agent=invented\n")+pktLine("ls-refs=unborn\n")+
				pktLine("fetch=shallow\n")+pktLine("object-format=sha1\n")+"0000")
		case owner == "throttled":
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
			w.WriteHeader(http.StatusUnauthorized)
		case owner == "forged":
			w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
			fmt.Fprint(w, pktLine("# service=git-upload-pack\n")+"0000"+
				pktLine("ERR invented\nfatal: could not read Username for 'http://"+r.Host+"': terminal prompts disabled"))
		case owner == "forbidden":
			w.WriteHeader(http.StatusForbidden)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// realGit is the absolute path of this machine's git, resolved once for the test, which is
// skipped where there is none.
func realGit(t *testing.T) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git on this machine")
	}
	return git
}

// loopbackEnv is the environment of a guard whose real git reads a server of the test's own on
// 127.0.0.1, with home as the caller's home, path as its PATH entry and extra on top. The
// process's own git may use local files alone (TestMain) and the server is http, and no proxy the
// machine names may stand between git and the server.
func loopbackEnv(home, path string, extra ...string) []string {
	env := append(fixtureenv.Isolated(home), path, "GIT_ALLOW_PROTOCOL=http",
		"NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	return append(env, extra...)
}

// The read with no credential answers public where a real git lists the refs, and nothing else.
// A server of the test's own stands in for GitHub's smart HTTP endpoint (gitHubStandIn), and the
// git on this machine reads it with the environment the guard gives it. A 401 for a private
// repository, a 401 for a public one under GitHub's throttle, and a server's ERR packet each make
// git print its line for a server that asked for a credential, the line this read once took for
// private; each is now no answer, as are a 403 and a 404.
func TestLsRemoteReadsWhatARealGitPrints(t *testing.T) {
	git := realGit(t)
	server := gitHubStandIn(t)
	guard := New(Options{Env: loopbackEnv(t.TempDir(), pathWith(filepath.Dir(git)))})
	for _, tc := range []struct {
		owner string
		// fails is what the error says, empty where the read answers public.
		fails string
	}{
		{owner: "public"},
		{owner: "private", fails: "could not read Username"},
		{owner: "throttled", fails: "could not read Username"},
		{owner: "forged", fails: "remote error: invented"},
		{owner: "forbidden", fails: "The requested URL returned error: 403"},
		{owner: "missing", fails: "not found"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			visibility, err := guard.lsRemote(bound(t), server.URL, tc.owner+"/invented-repo")
			if tc.fails == "" {
				if err != nil || visibility != Public {
					t.Fatalf("read %q, %v; want public", visibility, err)
				}
				return
			}
			if err == nil || visibility != "" {
				t.Fatalf("read %q, %v; want no answer", visibility, err)
			}
			if !strings.Contains(err.Error(), tc.fails) {
				t.Errorf("the error %q does not say %q", err, tc.fails)
			}
		})
	}
}

// No unauthenticated refusal reads as private, from the call's start: GitHub refuses the REST
// read for a spent core limit, GraphQL for a spent limit of its own, and ls-remote is made by the
// machine's real git against the stand-in server (gitHubStandIn), which a git wrapper first on
// PATH points https://github.com at with url.<base>.insteadOf. A public repository answers
// public; a throttled public repository's 401 and a server's ERR packet carrying git's old line
// refuse the call, naming the spent limit and its reset, and keep nothing.
func TestVisibilityNeverReadsAnUnauthenticatedRefusalAsPrivate(t *testing.T) {
	git := realGit(t)
	server := gitHubStandIn(t)
	clock := &visibilityTestClock{now: time.Date(2026, 11, 14, 5, 0, 0, 0, time.UTC)}
	reset := clock.Now().Add(30 * time.Minute)
	for _, tc := range []struct {
		owner string
		// fails is what the refusal says of git, empty where the call answers public.
		fails string
	}{
		{owner: "public"},
		{owner: "throttled", fails: "could not read Username"},
		{owner: "forged", fails: "remote error: invented"},
	} {
		t.Run(tc.owner, func(t *testing.T) {
			slug := tc.owner + "/invented-repo"
			bin := t.TempDir()
			writeProgram(t, bin, "gh", graphQLRateLimited(filepath.Join(t.TempDir(), "graphql")))
			writeProgram(t, bin, "git", "exec "+shQuote(git)+" -c "+shQuote("url."+server.URL+"/.insteadOf="+githubOrigin+"/")+" \"$@\"\n")
			var sources []VisibilitySource
			guard := New(Options{
				Clock:        clock,
				OnVisibility: func(source VisibilitySource) { sources = append(sources, source) },
				Env:          loopbackEnv(t.TempDir(), pathWith(bin), "XDG_STATE_HOME="+t.TempDir()),
				Visibility: func(context.Context, string) (Visibility, error) {
					return "", &rateLimitError{reset: reset}
				},
			})
			visibility, err := guard.Visibility(bound(t), slug)
			if tc.fails == "" {
				if err != nil || visibility != Public {
					t.Fatalf("read %q, %v; want public", visibility, err)
				}
				return
			}
			if err == nil || visibility != "" {
				t.Fatalf("read %q, %v; want the call refused", visibility, err)
			}
			for _, want := range []string{
				"the installation's rate limit is spent until " + reset.Format(time.RFC3339),
				"GraphQL's rate limit is spent too, and git ls-remote could not answer",
				tc.fails,
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the failure %q does not say %q", err, want)
				}
			}
			if len(sources) != 0 {
				t.Errorf("the guard reported the sources %q for a refused call, want none", sources)
			}
			if _, err := os.Stat(guard.visibilityCachePath(slug)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a visibility was kept for the refused call (%v), want none", err)
			}
		})
	}
}

// A setting a git wrapper adds does not make the read with no credential refuse a public
// repository, whether it is an extra header or http.proactiveAuth, and whether the wrapper sets it
// for no URL, for the host, or for the repository's URL with none, one or two slashes after it;
// nor does http.proactiveAuth in the caller's environment, which the read does not keep.
// `legion git --after` runs the host's next git on PATH, and a wrapper there can put a -c of its
// own in front of the guard's arguments. Here the machine's real git sits behind such a wrapper.
// The stand-in server refuses an Authorization header 401 for its public repository, as GitHub
// refuses a bad credential, and proactiveAuth makes git ask for a credential before it sends a
// request, so each setting that reached git would make it exit 128; the read answers public in
// every case. Each is a refusal the resets spare, never a private answer: with the generic header
// reset alone, the header for the host exits 128, and without the `//` header reset the header
// for the repository's URL and two slashes does; with the generic proactiveAuth reset alone, each
// proactiveAuth for a URL exits 128 (measured with git 2.47.3).
func TestLsRemoteClearsTheSettingsAGitWrapperAdds(t *testing.T) {
	git := realGit(t)
	server := gitHubStandIn(t)
	repo := server.URL + "/public/invented-repo.git"
	const header = "=Authorization: Basic aW52ZW50ZWQ6aW52ZW50ZWQ="
	for _, tc := range []struct {
		name string
		// setting is the -c the wrapper adds, empty for none, and env what the caller's
		// environment holds beyond the test's.
		setting string
		env     []string
	}{
		{name: "a header for no URL", setting: "http.extraHeader" + header},
		{name: "a header for the host", setting: "http." + server.URL + "/.extraHeader" + header},
		{name: "a header for the repository's URL", setting: "http." + repo + ".extraHeader" + header},
		{name: "a header for the repository's URL and a slash", setting: "http." + repo + "/.extraHeader" + header},
		{name: "a header for the repository's URL and two slashes", setting: "http." + repo + "//.extraHeader" + header},
		{name: "proactiveAuth basic for no URL", setting: "http.proactiveAuth=basic"},
		{name: "proactiveAuth basic for the host", setting: "http." + server.URL + "/.proactiveAuth=basic"},
		{name: "proactiveAuth auto for the host", setting: "http." + server.URL + "/.proactiveAuth=auto"},
		{name: "proactiveAuth basic for the repository's URL", setting: "http." + repo + ".proactiveAuth=basic"},
		{name: "proactiveAuth basic for the repository's URL and two slashes", setting: "http." + repo + "//.proactiveAuth=basic"},
		{name: "proactiveAuth basic in the caller's environment", env: []string{"GIT_CONFIG_PARAMETERS='http.proactiveauth'='basic'"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := t.TempDir(), filepath.Join(t.TempDir(), "record")
			wrapper := "echo ran >> " + shQuote(record) + "\nexec " + shQuote(git)
			if tc.setting != "" {
				wrapper += " -c " + shQuote(tc.setting)
			}
			writeProgram(t, bin, "git", wrapper+" \"$@\"\n")
			guard := New(Options{Env: loopbackEnv(t.TempDir(), pathWith(bin), tc.env...)})
			visibility, err := guard.lsRemote(bound(t), server.URL, "public/invented-repo")
			if err != nil || visibility != Public {
				t.Fatalf("read %q, %v; want public: the setting reached the request", visibility, err)
			}
			if got := countLines(t, record); got != 1 {
				t.Errorf("the wrapper ran %d times, want once: the read must go through it", got)
			}
		})
	}
}

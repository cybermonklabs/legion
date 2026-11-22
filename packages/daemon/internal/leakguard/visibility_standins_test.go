package leakguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The stand-ins and helpers the visibility tests share: visibility_graphql_test.go's reads of
// GraphQL and visibility_lsremote_test.go's reads with no credential, beside visibility_cache_test.go.

// stopBound is the bound, in whole seconds, that a child (git or gh) was stopped by, read from the
// *stoppedError err carries, and fails the test where err carries none or a cancel stopped the
// child. The bound is read against a ceiling, not for its exact seconds: on a loaded machine the
// child can start late in its bound, which then gives it less than the whole.
func stopBound(t *testing.T, err error) int64 {
	t.Helper()
	var stopped *stoppedError
	if !errors.As(err, &stopped) || stopped.cancelled {
		t.Fatalf("the failure %q is no stop of a child by its bound", err)
	}
	return wholeSeconds(stopped.bound)
}

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

// printsResponse is the lines of sh that print a response as `gh api --include` and `curl
// --include` print one: the status line, the headers and a blank line, each ended by CRLF, then
// body.
func printsResponse(status string, headers []string, body string) string {
	lines := append(append([]string{status}, headers...), "")
	quoted := make([]string, len(lines))
	for i, line := range lines {
		quoted[i] = shQuote(line)
	}
	return "printf '%s\\r\\n' " + strings.Join(quoted, " ") + "\nprintf '%s' " + shQuote(body) + "\n"
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
	return "echo read >> " + shQuote(reads) + "\n" +
		printsResponse("HTTP/2.0 403 Forbidden", []string{"Content-Type: application/json; charset=utf-8",
			"X-Ratelimit-Limit: 15000", "X-Ratelimit-Remaining: 0", fmt.Sprintf("X-Ratelimit-Reset: %d", reset.Unix()),
			"X-Ratelimit-Resource: core", "X-Ratelimit-Used: 15000"},
			`{"message":"API rate limit exceeded for installation ID 1.","status":"403"}`) +
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
// arguments and the variables the guard sets on it (readCalls reads the line), then prints
// status, GraphQL's rate-limit headers with remaining points left, a blank line and body, as `gh
// api --include` prints them, then gh's own error line where stderr is one, and exits with exit.
func graphQLRead(record, status string, remaining int, body, stderr string, exit int) string {
	script := `printf 'argv=%s\tread=%s\thost=%s\tnocolor=%s\tclicolor=%s\n' "$*" "${` + VisibilityReadVar + `-unset}" "${GH_HOST-unset}" ` +
		`"${NO_COLOR-unset}" "${CLICOLOR_FORCE-unset}" >> ` + shQuote(record) + "\n" +
		printsResponse(status, []string{"Content-Type: application/json; charset=utf-8", "X-Ratelimit-Limit: 12500",
			fmt.Sprintf("X-Ratelimit-Remaining: %d", remaining), fmt.Sprintf("X-Ratelimit-Reset: %d", graphQLReset.Unix()),
			"X-Ratelimit-Resource: graphql"}, body)
	if stderr != "" {
		script += printsToStderr(stderr)
	}
	return script + fmt.Sprintf("exit %d\n", exit)
}

// graphQLAnswers is a GraphQL read that answers visibility for theRepo, as GitHub answers for a
// repository the installation can read.
func graphQLAnswers(record, visibility string) string {
	return graphQLRead(record, "HTTP/2.0 200 OK", 12221, answerBody(theRepo, visibility), "", 0)
}

// answerBody is the body of a GraphQL answer naming the repository nameWithOwner and its
// visibility.
func answerBody(nameWithOwner, visibility string) string {
	return `{"data":{"repository":{"nameWithOwner":"` + nameWithOwner + `","visibility":"` + visibility + `"}}}`
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
		`"$(pwd -P)" >> ` + shQuote(record) + "\n"
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

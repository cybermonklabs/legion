package main

//ci:reads packages/envoy-client/src/dispatch-commands/job-result.ts jobResultSource joins ../../../envoy-client/src/dispatch-commands/job-result.ts, where TestTheCallBoundIsOverTheCommandsOwnBound reads RESULT_BOUND's totalMs
//ci:reads packages/envoy-client/src/dispatch-http.ts clientSource joins ../../../envoy-client/src/dispatch-http.ts, where TestTheCallBoundIsOverTheCommandsOwnBound reads the constant RESULT_BOUND's totalMs names

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// The dispatch command's source, from this package's directory: job-result.ts holds RESULT_BOUND,
// the one bound of `dispatch result --project`, and dispatch-http.ts the client deadline that its
// totalMs names.
var (
	jobResultSource = filepath.Join("..", "..", "..", "envoy-client", "src", "dispatch-commands", "job-result.ts")
	clientSource    = filepath.Join("..", "..", "..", "envoy-client", "src", "dispatch-http.ts")
)

// callBound is over the dispatch command's own one bound, RESULT_BOUND's totalMs, which this row
// reads from the command's source: at or under it, the step would end a call that the command may
// still answer. So a change to either bound that leaves callBound at or under the command's fails
// here, and so does a source this row can no longer read the bound from.
func TestTheCallBoundIsOverTheCommandsOwnBound(t *testing.T) {
	t.Parallel()
	if command := commandBound(t); callBound <= command {
		t.Errorf("callBound is %s, want more than the dispatch command's own bound of %s (RESULT_BOUND's totalMs in %s)", callBound, command, jobResultSource)
	}
}

// commandBound is RESULT_BOUND's totalMs as job-result.ts spells it: a number of milliseconds, or
// the name of a constant that dispatch-http.ts exports as one. It fails the test for any other
// spelling, since it then read no bound.
func commandBound(t *testing.T) time.Duration {
	t.Helper()
	read := func(path string, pattern *regexp.Regexp) string {
		t.Helper()
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		match := pattern.FindSubmatch(source)
		if match == nil {
			t.Fatalf("%s holds no line matching %s, so this row read no bound of the dispatch command", path, pattern)
		}
		return string(match[1])
	}
	value := read(jobResultSource, regexp.MustCompile(`(?m)^const RESULT_BOUND: ResultBound = \{\n  totalMs: ([0-9][0-9_]*|[A-Z][A-Z0-9_]*),$`))
	if !unicode.IsDigit(rune(value[0])) {
		value = read(clientSource, regexp.MustCompile(`(?m)^export const `+regexp.QuoteMeta(value)+` = ([0-9][0-9_]*);$`))
	}
	ms, err := strconv.ParseInt(strings.ReplaceAll(value, "_", ""), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return time.Duration(ms) * time.Millisecond
}

// A call that does not end within its bound is ended there: its line says that the step ended
// it, that the write may have gone out and what the command printed, with exit -1, and it is no
// stored result. The stub in mode hang prints a line and then sleeps for 30 s; the bound here is
// half a second, so the call must end long before the stub would. sh runs the stub, which this
// process wrote: a file this process writes and executes itself can be busy while a parallel test
// starts a process.
func TestACallThatDoesNotEndWithinItsBoundIsEnded(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	c := caller{
		cli: []string{"sh", s.path}, project: "ACME", commit: otherSHA,
		env: append(os.Environ(), s.env("hang")...), bound: 500 * time.Millisecond, waitDelay: 500 * time.Millisecond,
	}
	start := time.Now()
	code, id, why, stored := c.record(Planned{Check: "a-check", Outcome: "pass", Observed: "the test passed\n"})
	elapsed := time.Since(start)
	want := "the dispatch command did not end within 500ms, so the step ended it: the write may have gone out, and no answer said whether Dispatch stored it; a second run of the step is safe, since Dispatch keeps each result. The dispatch command printed: the write went out"
	if code != -1 || id != "" || why != want || stored {
		t.Errorf("record answers exit %d, id %q, why %q, stored %t; want exit -1, no id, why %q and not stored", code, id, why, stored, want)
	}
	if elapsed > 10*time.Second {
		t.Errorf("record took %s, want the bound of 500ms and the wait for its output", elapsed)
	}
	if calls := s.calls(t); len(calls) != 1 {
		t.Errorf("the stub got %d calls, want 1", len(calls))
	}
}

// The command gets the reporter's environment less GitHub's OIDC token request, and every other
// variable as it is, the result grant's placeholder among them.
func TestTheCommandGetsNoOIDCRequest(t *testing.T) {
	t.Parallel()
	environ := slices.Concat([]string{"PATH=/usr/bin", "DISPATCH_RESULTS_TOKEN=placeholder", "ACTIONS_ID_TOKEN_REQUEST=other"}, oidcEnv)
	if got, want := callEnv(environ), []string{"PATH=/usr/bin", "DISPATCH_RESULTS_TOKEN=placeholder", "ACTIONS_ID_TOKEN_REQUEST=other"}; !slices.Equal(got, want) {
		t.Errorf("callEnv gives %q, want %q", got, want)
	}
	if len(environ) != 5 {
		t.Errorf("callEnv changed the environment it read: %q", environ)
	}
}

// A line holds no character a reader of the log cannot see: each control character and each
// format character of a value is a \u escape, and the line reads back as the same value.
func TestALineEscapesWhatALogReaderCannotSee(t *testing.T) {
	t.Parallel()
	why := "a\u202eb\u0085c\u007fd\u200de\ufefff\U000E0001g\u2028h\x1bi ##[error]j"
	var out bytes.Buffer
	if err := writeLine(&out, checkLine{Check: "a-check", Why: why}); err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSuffix(out.String(), "\n")
	if strings.Count(out.String(), "\n") != 1 || strings.ContainsFunc(text, func(r rune) bool {
		return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp)
	}) || strings.Contains(text, "##[") {
		t.Errorf("the line holds a character a reader of the log cannot see, or a workflow command: %q", out.String())
	}
	for _, escape := range []string{`\u202e`, `\u0085`, `\u007f`, `\u200d`, `\ufeff`, `\udb40\udc01`, `\u2028`, `\u001b`, `##\u005b`} {
		if !strings.Contains(text, escape) {
			t.Errorf("the line holds no %s: %s", escape, text)
		}
	}
	var back checkLine
	if err := json.Unmarshal(out.Bytes(), &back); err != nil || back.Why != why {
		t.Errorf("the line reads back as %q (%v), want %q", back.Why, err, why)
	}
}

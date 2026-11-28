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
// stored result. The stub in mode hang prints a line, starts a child that holds its output open
// and never ends, and never ends itself: the bound ends the stub, and the wait delay the wait for
// the output its child holds. Without either, record never returns, and go test's -timeout ends
// the run.
func TestACallThatDoesNotEndWithinItsBoundIsEnded(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	c := caller{
		cli: []string{s.path}, project: "ACME", commit: otherSHA,
		env: append(os.Environ(), s.env("hang")...), bound: 500 * time.Millisecond, waitDelay: 500 * time.Millisecond,
	}
	code, id, why, stored := c.record(Planned{Check: "a-check", Outcome: "pass", Observed: "the test passed\n"})
	want := "the dispatch command did not end within 500ms, so the step ended it: the write may have gone out, and no answer said whether Dispatch stored it; a second run of the step is safe, since Dispatch keeps each result. The dispatch command printed: the write went out"
	if code != -1 || id != "" || why != want || stored {
		t.Errorf("record answers exit %d, id %q, why %q, stored %t; want exit -1, no id, why %q and not stored", code, id, why, stored, want)
	}
	if calls := s.calls(t); len(calls) != 1 {
		t.Errorf("the stub got %d calls, want 1", len(calls))
	}
}

// A command that exits while a child of its own holds its output open is read once the wait
// delay ends that wait: the stub in mode store-held prints its stored row and exits 0, and its
// child never ends. The bound is a minute, so only the wait delay can end the wait; without it,
// record never returns, and go test's -timeout ends the run.
func TestACommandWhoseChildHoldsItsOutputIsReadAfterTheWaitDelay(t *testing.T) {
	t.Parallel()
	s := newStub(t)
	c := caller{
		cli: []string{s.path}, project: "ACME", commit: otherSHA,
		env: append(os.Environ(), s.env("store-held")...), bound: time.Minute, waitDelay: 500 * time.Millisecond,
	}
	code, id, why, stored := c.record(Planned{Check: "a-check", Outcome: "pass", Observed: "the test passed\n"})
	if code != 0 || id != "row-a-check" || why != "" || !stored {
		t.Errorf("record answers exit %d, id %q, why %q, stored %t; want exit 0, id row-a-check, no why and stored", code, id, why, stored)
	}
}

// The command gets the reporter's environment less GitHub's OIDC token request, and every other
// variable as it is, the result grant's placeholder among them; the environment it read is as it
// was, element by element, since callEnv shares its backing array with the caller.
func TestTheCommandGetsNoOIDCRequest(t *testing.T) {
	t.Parallel()
	environ := slices.Concat([]string{"PATH=/usr/bin", "DISPATCH_RESULTS_TOKEN=placeholder", "ACTIONS_ID_TOKEN_REQUEST=other"}, oidcEnv)
	before := slices.Clone(environ)
	if got, want := callEnv(environ), []string{"PATH=/usr/bin", "DISPATCH_RESULTS_TOKEN=placeholder", "ACTIONS_ID_TOKEN_REQUEST=other"}; !slices.Equal(got, want) {
		t.Errorf("callEnv gives %q, want %q", got, want)
	}
	if !slices.Equal(environ, before) {
		t.Errorf("callEnv changed the environment it read: %q, was %q", environ, before)
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

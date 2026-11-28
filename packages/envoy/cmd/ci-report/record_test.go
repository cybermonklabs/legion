package main

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode"
)

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

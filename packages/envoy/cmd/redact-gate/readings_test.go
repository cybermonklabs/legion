package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// standIn writes an executable script named name into a directory first on PATH, for this test.
func standIn(t *testing.T, name, script string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// recordedGo is a go that passes each start-up run (-run '^$') and answers each other go test
// with the events in $RECORDED_EVENTS.
const recordedGo = `#!/usr/bin/env bash
for a in "$@"; do [ "$a" = '^$' ] && exit 0; done
cat "$RECORDED_EVENTS"
exit "${RECORDED_STATUS:-0}"
`

// TestARowWhoseTestQuotesTheTimeoutPanicIsNotPastItsBound: a run went past its bound when a line
// of its output starts with the test binary's timeout panic or the go command's kill line, where
// they print them (ranPast). A row's test whose t.Log line quotes the panic, and which passed,
// passes stage 4. Before, stages 4 and 5 counted any output event holding the phrase as a run past
// its bound (UNKNOWN), while the check script's judge read only a line's start. The control, the
// panic go1.27.3 prints, is still a run past its bound.
func TestARowWhoseTestQuotesTheTimeoutPanicIsNotPastItsBound(t *testing.T) {
	standIn(t, "go", recordedGo)
	for _, tc := range []struct {
		name   string
		events string
		late   bool
	}{
		{"a t.Log line quotes the panic", `{"Action":"run","Package":"example.com/probe","Test":"TestQuotes"}
{"Action":"output","Package":"example.com/probe","Test":"TestQuotes","Output":"=== RUN   TestQuotes\n"}
{"Action":"output","Package":"example.com/probe","Test":"TestQuotes","Output":"    quotes_test.go:13: panic: test timed out after 1s\n"}
{"Action":"output","Package":"example.com/probe","Test":"TestQuotes","Output":"--- PASS: TestQuotes (0.00s)\n"}
{"Action":"pass","Package":"example.com/probe","Test":"TestQuotes"}
{"Action":"output","Package":"example.com/probe","Output":"PASS\n"}
{"Action":"pass","Package":"example.com/probe"}
`, false},
		{"control: the binary's own panic", `{"Action":"run","Package":"example.com/probe","Test":"TestQuotes"}
{"Action":"output","Package":"example.com/probe","Test":"TestQuotes","Output":"=== RUN   TestQuotes\n"}
{"Action":"output","Package":"example.com/probe","Test":"TestQuotes","Output":"panic: test timed out after 3s\n"}
{"Action":"output","Package":"example.com/probe","Test":"TestQuotes","Output":"\trunning tests:\n"}
{"Action":"output","Package":"example.com/probe","Test":"TestQuotes","Output":"\t\tTestQuotes (3s)\n"}
{"Action":"output","Package":"example.com/probe","Output":"FAIL\texample.com/probe\t3.012s\n"}
{"Action":"fail","Package":"example.com/probe"}
`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := filepath.Join(t.TempDir(), "events.json")
			if err := os.WriteFile(events, []byte(tc.events), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RECORDED_EVENTS", events)
			copyDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(copyDir, "packages", "envoy"), 0o755); err != nil {
				t.Fatal(err)
			}
			g := &gate{rowTimeout: time.Minute}
			res := g.runTest(copyDir, &row{ID: "probe", Package: "./probe", Test: "TestQuotes", Capture: "probe"})
			var late lateRun
			isLate := errors.As(res.baselineErr(), &late)
			if res.timedOut != tc.late || isLate != tc.late {
				t.Fatalf("the run is past its bound: %v (stage 4 reads %v), want %v", res.timedOut, res.baselineErr(), tc.late)
			}
			if tc.late && !strings.Contains(res.stall, "panic: test timed out after 3s") {
				t.Fatalf("the run past its bound does not show its panic line:\n%s", res.stall)
			}
		})
	}
}

// recordedBun is a bun that writes $RECORDED_REPORT where --reporter-outfile names.
const recordedBun = `#!/usr/bin/env bash
for a in "$@"; do case "$a" in --reporter-outfile=*) cp "$RECORDED_REPORT" "${a#--reporter-outfile=}" ;; esac; done
exit "${RECORDED_STATUS:-0}"
`

// TestABunRowIsReadFromItsReportAsTheCheckReadsIt: a Bun row's testcase is found by its full name
// with each XML reference in it decoded, a numeric one too, and of testcases that share the name
// the first that did not pass decides, a self-closing one after it included (junitStates). Before,
// stages 4 and 5 decoded only the five named references, so a row whose name Bun wrote with &#39;
// was missing, and counted a passing self-closing testcase after a failed one of the same name a
// pass, where the check script's judge read both as it does now. The controls, a passing and a
// skipped testcase, read as before.
func TestABunRowIsReadFromItsReportAsTheCheckReadsIt(t *testing.T) {
	standIn(t, "bun", recordedBun)
	for _, tc := range []struct {
		name, test, report   string
		ran, failed, skipped bool
	}{
		{"a numeric reference in its name", "group > it's fine",
			`<testsuites><testsuite name="a.test.ts"><testsuite name="group"><testcase name="it&#39;s fine" /></testsuite></testsuite></testsuites>`, true, false, false},
		{"a failed testcase, then a passing one of the same name", "group > twice",
			`<testsuites><testsuite name="a.test.ts"><testsuite name="group"><testcase name="twice"><failure message="x" /></testcase><testcase name="twice" /></testsuite></testsuite></testsuites>`, true, true, false},
		{"control: a passing testcase", "group > fine",
			`<testsuites><testsuite name="a.test.ts"><testsuite name="group"><testcase name="fine" /></testsuite></testsuite></testsuites>`, true, false, false},
		{"control: a skipped testcase", "group > later",
			`<testsuites><testsuite name="a.test.ts"><testsuite name="group"><testcase name="later"><skipped /></testcase></testsuite></testsuite></testsuites>`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := filepath.Join(t.TempDir(), "report.xml")
			if err := os.WriteFile(report, []byte(tc.report), 0o644); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RECORDED_REPORT", report)
			copyDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(copyDir, "packages", "contracts"), 0o755); err != nil {
				t.Fatal(err)
			}
			g := &gate{rowTimeout: time.Minute, work: t.TempDir()}
			res := g.runTest(copyDir, &row{ID: "probe", File: "packages/contracts/src/a.test.ts", Test: tc.test, Capture: "probe", bun: true})
			if res.ran != tc.ran || res.failed != tc.failed || (len(res.skipped) > 0) != tc.skipped {
				t.Fatalf("the row reads ran %v, failed %v, skipped %v; want %v, %v, %v", res.ran, res.failed, res.skipped, tc.ran, tc.failed, tc.skipped)
			}
		})
	}
}

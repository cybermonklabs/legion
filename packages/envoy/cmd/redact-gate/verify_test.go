package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// event is one event of go test -json as go1.27.3 writes it, its time left out.
type event struct {
	ImportPath  string `json:",omitempty"`
	Action      string
	Package     string `json:",omitempty"`
	Test        string `json:",omitempty"`
	Output      string `json:",omitempty"`
	FailedBuild string `json:",omitempty"`
}

func eventLines(t *testing.T, events []event) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, e := range events {
		line, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

const (
	okPkg     = "./ok"
	okName    = "github.com/sjawhar/envoy/ok"
	daemonPkg = "github.com/sjawhar/legion/daemon/internal/probe"
	wordsFile = "packages/contracts/src/words.test.ts"
	// wordsTest is a Bun row's test whose name Bun's report writes with each of XML's references.
	wordsTest = `a group's "words" & <tags> > it's fine`
)

// ok is one event of the ./ok package.
func ok(action, test, output string) event {
	return event{Action: action, Package: okName, Test: test, Output: output}
}

// okPassed is the start and the end of a run of ./ok in which every test passed, around middle.
func okPassed(middle ...event) []event {
	return slices.Concat([]event{ok("start", "", "")}, middle, []event{
		ok("output", "", "PASS\n"),
		ok("output", "", "ok  \tgithub.com/sjawhar/envoy/ok\t0.008s\n"),
		ok("pass", "", ""),
	})
}

// okTests are TestPasses and TestTable/case passing.
var okTests = []event{
	ok("run", "TestPasses", ""),
	ok("output", "TestPasses", "=== RUN   TestPasses\n"),
	ok("output", "TestPasses", "--- PASS: TestPasses (0.00s)\n"),
	ok("pass", "TestPasses", ""),
	ok("run", "TestTable", ""),
	ok("run", "TestTable/case", ""),
	ok("pass", "TestTable/case", ""),
	ok("pass", "TestTable", ""),
}

// goPrintLine is the REDACT-PRINT line of the Go holder, as redacttest.PrintTest logs it.
const goPrintLine = "REDACT-PRINT Holder rows=2 same=1 reshaped=1 relocated=0 blind=0"

func daemonPassed(printed bool) []event {
	p := daemonPkg
	events := []event{
		{Action: "start", Package: p},
		{Action: "run", Package: p, Test: "TestDaemon"},
		{Action: "output", Package: p, Test: "TestDaemon", Output: "=== RUN   TestDaemon\n"},
	}
	if printed {
		events = append(events, event{Action: "output", Package: p, Test: "TestDaemon", Output: "    printtest.go:237: " + goPrintLine + "\n"})
	}
	return append(events,
		event{Action: "pass", Package: p, Test: "TestDaemon"},
		event{Action: "output", Package: p, Output: "PASS\n"},
		event{Action: "pass", Package: p},
	)
}

// wordsPassed is Bun's JUnit report of words.test.ts with wordsTest passing, as bun 1.4.4 writes it.
const wordsPassed = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="bun test" tests="1" assertions="1" failures="0" skipped="0" time="0.02">
  <testsuite name="words.test.ts" file="words.test.ts" tests="1" assertions="1" failures="0" skipped="0" time="0.002">
    <testsuite name="a group&apos;s &quot;words&quot; &amp; &lt;tags&gt;" file="words.test.ts" line="3" tests="1" assertions="1" failures="0" skipped="0" time="0">
      <testcase name="it&apos;s fine" classname="a group&apos;s &quot;words&quot; &amp; &lt;tags&gt;" time="0.000016" file="words.test.ts" line="4" assertions="1" />
    </testsuite>
  </testsuite>
</testsuites>`

// statesReport is bun 1.4.4's JUnit report of a file with a passing, a failing, a skipped and a
// todo test in one describe block, and a passing test outside it.
const statesReport = `<?xml version="1.0" encoding="UTF-8"?>
<testsuites name="bun test" tests="5" assertions="2" failures="1" skipped="2" time="0.020013875">
  <testsuite name="states.test.ts" file="states.test.ts" tests="5" assertions="2" failures="1" skipped="2" time="0.002072736">
    <testsuite name="a group&apos;s &quot;words&quot; &amp; &lt;tags&gt;" file="states.test.ts" line="3" tests="4" assertions="2" failures="1" skipped="2" time="0">
      <testcase name="it&apos;s fine" classname="a group&apos;s &quot;words&quot; &amp; &lt;tags&gt;" time="0.000016" file="states.test.ts" line="4" assertions="1" />
      <testcase name="it fails" classname="a group&apos;s &quot;words&quot; &amp; &lt;tags&gt;" time="0.000335" file="states.test.ts" line="7" assertions="1">
        <failure type="AssertionError" message="expect(received).toBe(expected)&#10;&#10;Expected: 2&#10;Received: 1&#10;">AssertionError: expect(received).toBe(expected)&#10;&#10;Expected: 2&#10;Received: 1&#10;&#10;      at states.test.ts:8:15&#10;</failure>
      </testcase>
      <testcase name="skipped one" classname="a group&apos;s &quot;words&quot; &amp; &lt;tags&gt;" time="0" file="states.test.ts" line="10" assertions="0">
        <skipped />
      </testcase>
      <testcase name="todo one" classname="a group&apos;s &quot;words&quot; &amp; &lt;tags&gt;" time="0" file="states.test.ts" line="11" assertions="0">
        <skipped message="TODO" />
      </testcase>
    </testsuite>
    <testcase name="top level" classname="" time="0.000021" file="states.test.ts" line="14" assertions="0" />
  </testsuite>
</testsuites>`

// verifyRuns is what judgeVerify judges: an inventory and the recorded runs of its rows.
type verifyRuns struct {
	inv  inventory
	gos  []goRun
	buns []bunRun
}

// passedRuns are runs of the fixture inventory in which every package, row and Bun file passed and
// the print test's Go and Bun holders printed their REDACT-PRINT lines.
func passedRuns(t *testing.T) verifyRuns {
	return verifyRuns{
		inv: inventory{
			Gate: gateConfig{RowTimeout: "48s"},
			Go: []row{
				{ID: "ok-passes", Requirement: "r3-each-site", Package: okPkg, Test: "TestPasses"},
				{ID: "ok-case", Requirement: "r3-each-site", Package: okPkg, Test: "TestTable/case"},
				{ID: "daemon-passes", Requirement: "r4-unchanged", Package: daemonPkg, Test: "TestDaemon"},
			},
			Bun: []row{{ID: "words-fine", Requirement: "r3-each-site", File: wordsFile, Test: wordsTest, bun: true}},
		},
		gos: []goRun{
			{pkg: okPkg, events: eventLines(t, okPassed(okTests...))},
			{pkg: daemonPkg, events: eventLines(t, daemonPassed(true))},
		},
		buns: []bunRun{{file: wordsFile, reported: true, report: []byte(wordsPassed),
			output: []byte("bun test v1.4.4 (38afee6c8)\n\nwords.test.ts:\nREDACT-PRINT BunHolder rows=3 same=3 reshaped=0 relocated=0 blind=0\n\n 1 pass\n")}},
	}
}

// verifyCase is one judgement of recorded runs: the runs are passedRuns changed by change; the
// output must hold each of lines as a line of its own, and each of holds, and none of lacks.
type verifyCase struct {
	name         string
	change       func(t *testing.T, v *verifyRuns)
	exit         int
	lines, holds []string
	lacks        []string
}

// verifyCases are the judgements of the runs a check meets: each kind of failure, each run past
// its bound, each Bun state, and a run that printed no REDACT-PRINT line.
func verifyCases() []verifyCase {
	okRows := func(state, suffix string) []string {
		return []string{
			state + " ok-passes (r3-each-site) ./ok TestPasses" + suffix,
			state + " ok-case (r3-each-site) ./ok TestTable/case" + suffix,
		}
	}
	return []verifyCase{
		{name: "every run passed", exit: 0, lines: []string{
			"PASS step go test ./ok",
			"PASS step go test " + daemonPkg,
			"PASS ok-passes (r3-each-site) ./ok TestPasses",
			"PASS ok-case (r3-each-site) ./ok TestTable/case",
			"PASS daemon-passes (r4-unchanged) " + daemonPkg + " TestDaemon",
			"PASS step bun test " + wordsFile,
			"PASS words-fine (r3-each-site) " + wordsFile + " " + wordsTest,
			"PRINT 2 holders: 5 printer and placement rows, 4 with a twin at their own placement and shape, 1 reshaped and 0 relocated (each shows only that the printer output is captured there), 0 blind",
			"  " + goPrintLine,
			"  REDACT-PRINT BunHolder rows=3 same=3 reshaped=0 relocated=0 blind=0",
			"verify: 4 rows, 0 failed, 0 UNKNOWN (8 verdicts: one for each of 2 Go packages, 3 Go rows, 1 Bun files and 1 Bun rows, and the print test's)",
		}, lacks: []string{"why "}},

		{name: "a killed go command cut its last event line short", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[0].status = 137
				v.gos[0].events = append(eventLines(t, []event{ok("start", "", ""), ok("run", "TestPasses", ""), ok("output", "TestPasses", "=== RUN   TestPasses\n")}), `{"Action":"output","Package":`...)
			},
			lines: append(okRows("FAIL", ": test did not run, package did not end"),
				"FAIL step go test ./ok (exit 137): unreadable event lines: 1 (a go command killed mid-line cuts its last line short), so no reading of its run is whole",
				"    its first event line that does not parse: {\"Action\":\"output\",\"Package\":",
				"PASS step go test "+daemonPkg),
		},

		{name: "an event line cut short in a run that exited 0 and ended in its pass", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				all := okPassed(okTests...)
				v.gos[0].events = slices.Concat(eventLines(t, all[:3]), []byte("{\"Action\":\"pass\",\"Package\":\"github.com/sje\n"), eventLines(t, all[3:]))
			},
			lines: append(okRows("PASS", ""),
				"FAIL step go test ./ok (exit 0): unreadable event lines: 1 (a go command killed mid-line cuts its last line short), so no reading of its run is whole"),
		},

		{name: "a row's test failed", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[0].status = 1
				v.gos[0].stderr = []byte("go: a line of the go command's own\n")
				v.gos[0].events = eventLines(t, []event{
					ok("start", "", ""),
					ok("run", "TestPasses", ""),
					ok("pass", "TestPasses", ""),
					ok("run", "TestTable", ""),
					ok("run", "TestTable/case", ""),
					ok("output", "TestTable/case", "=== RUN   TestTable/case\n"),
					ok("output", "TestTable/case", "    table_test.go:12: first\n"),
					ok("output", "TestTable/case", "    table_test.go:13: REDACT-ASSERT probe: the output prints the secret\n"),
					ok("output", "TestTable/case", "--- FAIL: TestTable/case (0.00s)\n"),
					ok("fail", "TestTable/case", ""),
					ok("output", "TestTable", "--- FAIL: TestTable (0.00s)\n"),
					ok("fail", "TestTable", ""),
					ok("output", "", "FAIL\n"),
					ok("output", "", "FAIL\tgithub.com/sjawhar/envoy/ok\t0.008s\n"),
					ok("fail", "", ""),
				})
			},
			lines: []string{
				"FAIL step go test ./ok (exit 1): 1 tests failed",
				"FAIL ok-passes (r3-each-site) ./ok TestPasses: test pass, package fail",
				"FAIL ok-case (r3-each-site) ./ok TestTable/case: test fail, package fail",
				"PASS daemon-passes (r4-unchanged) " + daemonPkg + " TestDaemon",
				"  why " + okName + " did not pass:",
				"    TestTable/case: table_test.go:12: first",
				"      last: table_test.go:13: REDACT-ASSERT probe: the output prints the secret",
				"    its stderr, last lines:",
				"      go: a line of the go command's own",
			},
		},

		{name: "a row's test was skipped", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[0].events = eventLines(t, okPassed(
					ok("run", "TestPasses", ""),
					ok("pass", "TestPasses", ""),
					ok("run", "TestTable", ""),
					ok("run", "TestTable/case", ""),
					ok("output", "TestTable/case", "    table_test.go:9: needs DISPATCH_TEST_DATABASE_URL\n"),
					ok("output", "TestTable/case", "--- SKIP: TestTable/case (0.00s)\n"),
					ok("skip", "TestTable/case", ""),
					ok("pass", "TestTable", ""),
				))
			},
			lines: []string{
				"PASS step go test ./ok",
				"PASS ok-passes (r3-each-site) ./ok TestPasses",
				"FAIL ok-case (r3-each-site) ./ok TestTable/case: test skip, package pass",
			},
			lacks: []string{"why "},
		},

		{name: "a package past its bound: the binary's timeout panic", exit: 3,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[0].status = 1
				v.gos[0].events = eventLines(t, []event{
					ok("start", "", ""),
					ok("run", "TestPasses", ""),
					ok("output", "TestPasses", "=== RUN   TestPasses\n"),
					ok("output", "TestPasses", "panic: test timed out after 3s\n"),
					ok("output", "TestPasses", "\trunning tests:\n"),
					ok("output", "TestPasses", "\t\tTestPasses (3s)\n"),
					ok("output", "TestPasses", "\n"),
					ok("output", "TestPasses", "goroutine 17 [running]:\n"),
					ok("output", "TestPasses", "testing.(*M).startAlarm.func1()\n"),
					ok("output", "TestPasses", "created by time.goFunc\n"),
					ok("output", "TestPasses", "\n"),
					ok("output", "TestPasses", "goroutine 7 [sleep]:\n"),
					ok("output", "TestPasses", "time.Sleep(0x34630b8a000)\n"),
					ok("output", "TestPasses", "github.com/sjawhar/envoy/ok.TestPasses(0x39735b922248?)\n"),
					ok("output", "TestPasses", "\t/ok/ok_test.go:8 +0x1d\n"),
					ok("output", "TestPasses", "testing.tRunner(0x39735b922248, 0x6dbe18)\n"),
					ok("output", "", "FAIL\tgithub.com/sjawhar/envoy/ok\t3.012s\n"),
					ok("fail", "", ""),
				})
			},
			lines: append(okRows("UNKNOWN", ": test did not run, package ran past its bound"),
				"UNKNOWN step go test ./ok (exit 1): it ran past its bound and no test failed, so its rows neither passed nor failed: run it again",
				"    panic: test timed out after 3s",
				"    \t\tTestPasses (3s)",
				"    goroutine 7 [sleep]:",
				"    github.com/sjawhar/envoy/ok.TestPasses(0x39735b922248?)",
				"verify: 4 rows, 0 failed, 3 UNKNOWN (8 verdicts: one for each of 2 Go packages, 3 Go rows, 1 Bun files and 1 Bun rows, and the print test's)"),
			lacks: []string{"goroutine 17 [running]:"},
		},

		{name: "a package past its bound: the go command's kill", exit: 3,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[0].status = 1
				v.gos[0].events = eventLines(t, slices.Concat([]event{ok("start", "", "")}, okTests, []event{
					ok("output", "", "PASS\n"),
					ok("output", "", "SIGQUIT: quit\n"),
					ok("output", "", "PC=0x48e4a1 m=0 sigcode=0\n"),
					ok("output", "", "\n"),
					ok("output", "", "goroutine 1 gp=0x5884db921e0 m=nil [sleep]:\n"),
					ok("output", "", "time.Sleep(0x34630b8a000)\n"),
					ok("output", "", "github.com/sjawhar/envoy/ok.TestMain(0x725280?)\n"),
					ok("output", "", "\t/ok/main_test.go:12 +0x38\n"),
					ok("output", "", "main.main()\n"),
					ok("output", "", "\n"),
					ok("output", "", "goroutine 2 gp=0x5884db92780 m=nil [force gc (idle)]:\n"),
					ok("output", "", "runtime.forcegchelper()\n"),
					ok("output", "", "\n"),
					ok("output", "", "*** Test killed with quit: ran too long (1m1s).\n"),
					ok("output", "", "FAIL\tgithub.com/sjawhar/envoy/ok\t61.066s\n"),
					ok("fail", "", ""),
				}))
			},
			lines: append(okRows("UNKNOWN", ": test pass, package ran past its bound"),
				"UNKNOWN step go test ./ok (exit 1): it ran past its bound and no test failed, so its rows neither passed nor failed: run it again",
				"    *** Test killed with quit: ran too long (1m1s).",
				"    github.com/sjawhar/envoy/ok.TestMain(0x725280?)"),
			lacks: []string{"goroutine 2 gp="},
		},

		{name: "a t.Log line that quotes the timeout's panic is no run past its bound", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[0].status = 1
				v.gos[0].events = eventLines(t, slices.Concat([]event{
					ok("start", "", ""),
					ok("output", "TestPasses", "    ok_test.go:13: panic: test timed out after 1s\n"),
				}, okTests, []event{
					ok("output", "", "FAIL\tgithub.com/sjawhar/envoy/ok\t0.010s\n"),
					ok("fail", "", ""),
				}))
			},
			lines: append(okRows("FAIL", ": test pass, package fail"),
				"FAIL step go test ./ok (exit 1): no test failed, and it ended in fail",
				"    FAIL\tgithub.com/sjawhar/envoy/ok\t0.010s"),
			lacks: []string{"\nUNKNOWN "},
		},

		{name: "a package does not build", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				target := okName + " [" + okName + ".test]"
				v.gos[0].status = 1
				v.gos[0].events = eventLines(t, []event{
					{ImportPath: target, Action: "build-output", Output: "# " + target + "\n"},
					{ImportPath: target, Action: "build-output", Output: "ok/table_test.go:5:33: undefined: declaredNowhere\n"},
					{ImportPath: target, Action: "build-fail"},
					ok("start", "", ""),
					ok("output", "", "FAIL\t"+okName+" [build failed]\n"),
					{Action: "fail", Package: okName, FailedBuild: target},
				})
			},
			lines: append(okRows("FAIL", ": test did not run, package fail"),
				"FAIL step go test ./ok (exit 1): it does not build ("+okName+" [github.com/sjawhar/envoy/ok.test])",
				"    ok/table_test.go:5:33: undefined: declaredNowhere"),
		},

		{name: "a package's start-up run failed", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[0].status = 1
				v.gos[0].events = eventLines(t, []event{
					{ImportPath: okPkg, Action: "build-output", Output: "# ./ok\n"},
					{ImportPath: okPkg, Action: "build-output", Output: "stat /repo/packages/envoy/ok: directory not found\n"},
					{ImportPath: okPkg, Action: "build-fail"},
					{Action: "start", Package: okPkg},
					{Action: "output", Package: okPkg, Output: "FAIL\t./ok [setup failed]\n"},
					{Action: "fail", Package: okPkg, FailedBuild: okPkg},
				})
			},
			lines: append(okRows("FAIL", ": test did not run, package fail"),
				"FAIL step go test ./ok (exit 1): it does not build (./ok)",
				"    # ./ok | stat /repo/packages/envoy/ok: directory not found"),
		},

		{name: "each Bun state", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				const states, unread = "packages/contracts/src/states.test.ts", "packages/contracts/src/unread.test.ts"
				group := `a group's "words" & <tags> > `
				v.inv.Bun = []row{
					{ID: "bun-passed", Requirement: "r3-each-site", File: states, Test: group + "it's fine", bun: true},
					{ID: "bun-failed", Requirement: "r3-each-site", File: states, Test: group + "it fails", bun: true},
					{ID: "bun-skipped", Requirement: "r3-each-site", File: states, Test: group + "skipped one", bun: true},
					{ID: "bun-todo", Requirement: "r3-each-site", File: states, Test: group + "todo one", bun: true},
					{ID: "bun-top", Requirement: "r3-each-site", File: states, Test: "top level", bun: true},
					{ID: "bun-absent", Requirement: "r3-each-site", File: states, Test: group + "never written", bun: true},
					{ID: "bun-prefix", Requirement: "r3-each-site", File: states, Test: group + "it", bun: true},
					{ID: "bun-no-report", Requirement: "r3-each-site", File: unread, Test: "anything", bun: true},
				}
				v.buns = []bunRun{
					{file: states, status: 1, reported: true, report: []byte(statesReport), output: []byte("REDACT-PRINT BunHolder rows=3 same=3 reshaped=0 relocated=0 blind=0\n")},
					{file: unread, status: 1, output: []byte("error: Cannot find module './gone'\n")},
				}
			},
			lines: []string{
				"FAIL step bun test packages/contracts/src/states.test.ts (exit 1)",
				"FAIL step bun test packages/contracts/src/unread.test.ts (exit 1)",
				`PASS bun-passed (r3-each-site) packages/contracts/src/states.test.ts a group's "words" & <tags> > it's fine`,
				`FAIL bun-failed (r3-each-site) packages/contracts/src/states.test.ts a group's "words" & <tags> > it fails: the test failed`,
				`FAIL bun-skipped (r3-each-site) packages/contracts/src/states.test.ts a group's "words" & <tags> > skipped one: the test was skipped`,
				`FAIL bun-todo (r3-each-site) packages/contracts/src/states.test.ts a group's "words" & <tags> > todo one: the test was skipped`,
				"PASS bun-top (r3-each-site) packages/contracts/src/states.test.ts top level",
				`FAIL bun-absent (r3-each-site) packages/contracts/src/states.test.ts a group's "words" & <tags> > never written: the test is not in the report`,
				`FAIL bun-prefix (r3-each-site) packages/contracts/src/states.test.ts a group's "words" & <tags> > it: the test is not in the report`,
				"FAIL bun-no-report (r3-each-site) packages/contracts/src/unread.test.ts anything: the test has no JUnit report",
			},
		},

		{name: "no holder printed a REDACT-PRINT line", exit: 1,
			change: func(t *testing.T, v *verifyRuns) {
				v.gos[1].events = eventLines(t, daemonPassed(false))
				v.buns[0].output = []byte("bun test v1.4.4 (38afee6c8)\n\n 1 pass\n")
			},
			lines: []string{
				"PASS step go test " + daemonPkg,
				"FAIL print-test: no holder printed a REDACT-PRINT line, so no print row ran",
				"verify: 4 rows, 1 failed, 0 UNKNOWN (8 verdicts: one for each of 2 Go packages, 3 Go rows, 1 Bun files and 1 Bun rows, and the print test's)",
			},
		},
	}
}

func judgeCase(t *testing.T, c verifyCase) (verifyRuns, string, int) {
	t.Helper()
	v := passedRuns(t)
	if c.change != nil {
		c.change(t, &v)
	}
	var out bytes.Buffer
	code := judgeVerify(v.inv, v.gos, v.buns, &out)
	return v, out.String(), code
}

// TestTheVerifyModeJudgesEachRunFromItsEventsAndReports: -verify's verdicts of recorded go test
// events and Bun JUnit reports: a package passes only when its go exited 0 and its events, each
// of which parses, end in its pass; a row only when its test and its package passed; a package
// past its bound (its binary's panic, or the go command's kill a minute later, at a line's start)
// with no test failed is UNKNOWN, and a t.Log line quoting the panic is no such run; each failure
// prints why; a Bun row passes only on a passing testcase of its full name; the print test only
// on a REDACT-PRINT line. The exit status is 0, 1, or 3 for UNKNOWN with nothing failed.
func TestTheVerifyModeJudgesEachRunFromItsEventsAndReports(t *testing.T) {
	for _, c := range verifyCases() {
		t.Run(c.name, func(t *testing.T) {
			_, out, code := judgeCase(t, c)
			if code != c.exit {
				t.Errorf("exit %d, want %d", code, c.exit)
			}
			lines := strings.Split(out, "\n")
			for _, want := range c.lines {
				if !slices.Contains(lines, want) {
					t.Errorf("no line %q", want)
				}
			}
			for _, want := range c.holds {
				if !strings.Contains(out, want) {
					t.Errorf("no %q", want)
				}
			}
			for _, unwanted := range c.lacks {
				if strings.Contains(out, unwanted) {
					t.Errorf("it holds %q", unwanted)
				}
			}
			if t.Failed() {
				t.Logf("its output:\n%s", out)
			}
		})
	}
}

// TestTheVerifyModePrintsOneVerdictForEachPackageRowBunFileAndThePrintTest: for every run
// verifyCases judges, -verify prints exactly one verdict line for each Go package, Go row, Bun
// file and Bun row and one for the print test, then its count, and exits 0 only when each
// verdict passed, 1 when one failed and 3 when none failed and one was UNKNOWN. Where it would
// print another number of verdicts - a package's run missing, or a run no row names - it refuses
// with exit 2 and prints no count, so it cannot pass a row it owes no verdict.
func TestTheVerifyModePrintsOneVerdictForEachPackageRowBunFileAndThePrintTest(t *testing.T) {
	count := func(out string, pattern string) int {
		return len(regexp.MustCompile("(?m)"+pattern).FindAllString(out, -1))
	}
	for _, c := range verifyCases() {
		t.Run(c.name, func(t *testing.T) {
			v, out, code := judgeCase(t, c)
			owe := map[string]bool{}
			for _, r := range v.inv.Go {
				owe[`^(PASS|FAIL|UNKNOWN) step go test `+regexp.QuoteMeta(r.Package)+`( \(|$)`] = true
				owe[`^(PASS|FAIL|UNKNOWN) `+regexp.QuoteMeta(r.ID)+` \(`] = true
			}
			for _, r := range v.inv.Bun {
				owe[`^(PASS|FAIL) step bun test `+regexp.QuoteMeta(r.File)+`( \(|$)`] = true
				owe[`^(PASS|FAIL) `+regexp.QuoteMeta(r.ID)+` \(`] = true
			}
			owe[`^(PRINT [0-9]+ holders: |FAIL print-test: )`] = true
			owe[`^verify: [0-9]+ rows, [0-9]+ failed, [0-9]+ UNKNOWN \(`] = true
			for pattern := range owe {
				if n := count(out, pattern); n != 1 {
					t.Errorf("%d lines match %s, want 1", n, pattern)
				}
			}
			verdictLines := count(out, `^(PASS|FAIL|UNKNOWN|PRINT) `)
			if want := len(owe) - 1; verdictLines != want {
				t.Errorf("%d verdict lines, want %d", verdictLines, want)
			}
			failed, unknown := count(out, `^FAIL `), count(out, `^UNKNOWN `)
			want := 0
			switch {
			case failed > 0:
				want = 1
			case unknown > 0:
				want = 3
			}
			if code != want {
				t.Errorf("exit %d with %d FAIL and %d UNKNOWN verdicts, want %d", code, failed, unknown, want)
			}
			if summary := fmt.Sprintf("verify: %d rows, %d failed, %d UNKNOWN (", len(v.inv.Go)+len(v.inv.Bun), failed, unknown); !strings.Contains(out, summary) {
				t.Errorf("no count %q", summary)
			}
			if t.Failed() {
				t.Logf("its output:\n%s", out)
			}
		})
	}
	for _, c := range []verifyCase{
		{name: "a package's run is missing", change: func(t *testing.T, v *verifyRuns) { v.gos = v.gos[:1] }},
		{name: "a run of a package no row names", change: func(t *testing.T, v *verifyRuns) {
			v.gos = append(v.gos, goRun{pkg: "./other", events: eventLines(t, okPassed())})
		}},
		{name: "a Bun file's run is missing", change: func(t *testing.T, v *verifyRuns) { v.buns = nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, out, code := judgeCase(t, c)
			if code != 2 || !strings.Contains(out, "verify: refused: it printed ") || strings.Contains(out, "\nverify: 4 rows") {
				t.Errorf("exit %d, want 2 with the refusal and no count; its output:\n%s", code, out)
			}
		})
	}
}

// TestTheVerifyModeRefusesAnInventoryItCannotJudge: -verify refuses (exit 2) an inventory it
// cannot parse, one with no Go row or no Bun row, one with a row lacking a field or holding a line
// break in one, one that repeats an id, and one whose gate.row_timeout is not a whole count of
// seconds; the check script's own parser refused each before. The control, the fixture inventory,
// is read.
func TestTheVerifyModeRefusesAnInventoryItCannotJudge(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{
			"gate": map[string]any{"row_timeout": "48s"},
			"go":   []any{map[string]any{"id": "g", "requirement": "r", "package": "./ok", "test": "TestA"}},
			"bun":  []any{map[string]any{"id": "b", "requirement": "r", "file": "packages/contracts/src/a.test.ts", "test": "a"}},
		}
	}
	field := func(kind, name string, value any) func(map[string]any) {
		return func(inv map[string]any) { inv[kind].([]any)[0].(map[string]any)[name] = value }
	}
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		raw    string
		want   string
	}{
		{name: "control: the fixture", want: ""},
		{name: "not JSON", raw: `{"go": [`, want: "cannot be parsed"},
		{name: "no Go row", change: func(inv map[string]any) { inv["go"] = []any{} }, want: "names no Go row or no Bun row"},
		{name: "no Bun row", change: func(inv map[string]any) { delete(inv, "bun") }, want: "names no Go row or no Bun row"},
		{name: "a Go row with no id", change: field("go", "id", ""), want: `an inventory row ("") has no id`},
		{name: "a Go row with no requirement", change: field("go", "requirement", ""), want: `an inventory row ("g") has no requirement`},
		{name: "a Go row with no package", change: field("go", "package", ""), want: `an inventory row ("g") has no package`},
		{name: "a Bun row with no file", change: field("bun", "file", ""), want: `an inventory row ("b") has no file`},
		{name: "a Bun row with no test", change: field("bun", "test", ""), want: `an inventory row ("b") has no test`},
		{name: "a test holding a line break", change: field("go", "test", "TestA\nPASS g"), want: `an inventory row ("g") has no test`},
		{name: "a repeated id", change: field("bun", "id", "g"), want: "the inventory repeats the id g"},
		{name: "no row_timeout", change: func(inv map[string]any) { delete(inv, "gate") }, want: "names no gate.row_timeout in whole seconds"},
		{name: "a row_timeout in minutes", change: func(inv map[string]any) { inv["gate"] = map[string]any{"row_timeout": "1m"} }, want: "names no gate.row_timeout in whole seconds"},
		{name: "a row_timeout with no unit", change: func(inv map[string]any) { inv["gate"] = map[string]any{"row_timeout": "48"} }, want: "names no gate.row_timeout in whole seconds"},
		{name: "a row_timeout of 0s", change: func(inv map[string]any) { inv["gate"] = map[string]any{"row_timeout": "0s"} }, want: "names no gate.row_timeout in whole seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.raw)
			if tc.raw == "" {
				inv := good()
				if tc.change != nil {
					tc.change(inv)
				}
				var err error
				if data, err = json.Marshal(inv); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(t.TempDir(), "redact-check.json")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			inv, rowTimeout, err := verifyInventory(path)
			switch {
			case tc.want == "" && (err != nil || rowTimeout != 48*time.Second || len(inv.Go) != 1 || !inv.Bun[0].bun):
				t.Fatalf("the fixture reads as %v, %s, %+v", err, rowTimeout, inv)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("the refusal is %v, want it to say %q", err, tc.want)
			}
			if tc.want != "" {
				var out bytes.Buffer
				if code := verify(t.TempDir(), path, "", &out); code != 2 || !strings.HasPrefix(out.String(), "verify: refused: ") {
					t.Fatalf("-verify exited %d, want 2 with its refusal: %s", code, out.String())
				}
			}
		})
	}
}

// standInGo is a go for TestTheVerifyModeRunsEachPackageUnderItsBound: it logs each call's
// arguments to $STAND_IN_LOG, fails the start-up run (-run '^$') of ./broken with exit 3 and the
// events of a package that does not build, passes every other start-up run, and answers each other
// go test with the events of a run in which TestA and TestB/sub passed.
const standInGo = `#!/usr/bin/env bash
echo "go $*" >>"$STAND_IN_LOG"
pkg=${@: -1}
for a in "$@"; do
  if [ "$a" = '^$' ]; then
    if [ "$pkg" = ./broken ]; then
      echo '{"ImportPath":"./broken","Action":"build-output","Output":"broken_test.go:3:1: syntax error\n"}'
      echo '{"Action":"fail","Package":"./broken","FailedBuild":"./broken"}'
      echo 'go: a line of the go command' >&2
      exit 3
    fi
    exit 0
  fi
done
for t in TestA TestB TestB/sub; do printf '{"Action":"pass","Package":"%s","Test":"%s"}\n' "$pkg" "$t"; done
printf '{"Action":"pass","Package":"%s"}\n' "$pkg"
`

// standInBun writes a report with one passing testcase, "a > b", where --reporter-outfile names,
// and prints a REDACT-PRINT line.
const standInBun = `#!/usr/bin/env bash
echo "bun $* in $PWD" >>"$STAND_IN_LOG"
for a in "$@"; do case "$a" in --reporter-outfile=*)
  echo '<testsuites><testsuite name="a.test.ts"><testsuite name="a"><testcase name="b" /></testsuite></testsuite></testsuites>' >"${a#--reporter-outfile=}" ;;
esac; done
echo 'REDACT-PRINT BunHolder rows=1 same=1 reshaped=0 relocated=0 blind=0'
`

// TestTheVerifyModeRunsEachPackageUnderItsBound: -verify runs each Go package's start-up run, and
// then its inventory tests' top-level names, escaped and anchored, under runBound's bound for
// them; a package whose start-up run failed has that run's events and stderr judged, and its rows
// do not run; each Bun file runs in its package with the JUnit reporter; with -keep, each run's
// record stays. Stand-ins take go's and bun's place.
func TestTheVerifyModeRunsEachPackageUnderItsBound(t *testing.T) {
	standIn(t, "go", standInGo)
	standIn(t, "bun", standInBun)
	repo := t.TempDir()
	for _, dir := range []string{"packages/envoy", "packages/contracts"} {
		if err := os.MkdirAll(filepath.Join(repo, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(t.TempDir(), "calls.log")
	t.Setenv("STAND_IN_LOG", log)
	inv := map[string]any{
		"gate": map[string]any{"row_timeout": "7s"},
		"go": []any{
			map[string]any{"id": "a", "requirement": "r", "package": "./ok", "test": "TestA"},
			map[string]any{"id": "b", "requirement": "r", "package": "./ok", "test": "TestB/sub"},
			map[string]any{"id": "c", "requirement": "r", "package": "./broken", "test": "TestC"},
		},
		"bun": []any{map[string]any{"id": "d", "requirement": "r", "file": "packages/contracts/src/a.test.ts", "test": "a > b"}},
	}
	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "redact-check.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(t.TempDir(), "kept")
	var out bytes.Buffer
	code := verify(repo, path, keep, &out)
	text := out.String()
	defer func() {
		if t.Failed() {
			calls, _ := os.ReadFile(log)
			t.Logf("its output:\n%s\nits calls:\n%s", text, calls)
		}
	}()
	if code != 1 {
		t.Fatalf("exit %d, want 1 (./broken failed)", code)
	}
	for _, want := range []string{
		"PASS step go test ./ok", "PASS a (r) ./ok TestA", "PASS b (r) ./ok TestB/sub",
		"FAIL step go test ./broken (exit 3): it does not build (./broken)", "FAIL c (r) ./broken TestC: test did not run, package fail",
		"    broken_test.go:3:1: syntax error", "      go: a line of the go command",
		"PASS step bun test packages/contracts/src/a.test.ts", "PASS d (r) packages/contracts/src/a.test.ts a > b",
		"PRINT 1 holders: 1 printer and placement rows, 1 with a twin at their own placement and shape, 0 reshaped and 0 relocated (each shows only that the printer output is captured there), 0 blind",
	} {
		if !slices.Contains(strings.Split(text, "\n"), want) {
			t.Errorf("no line %q", want)
		}
	}
	if !regexp.MustCompile(`(?m)^go test \./broken: its start-up run \(-run '\^\$'\) exited 3 after \d+s, so its rows did not run$`).MatchString(text) {
		t.Errorf("no line saying ./broken's start-up run failed")
	}
	m := regexp.MustCompile(`(?m)^go test \./ok: \d+s under a bound of (\d+)s \(2 top-level tests at 7s, and four times its start-up run's (\d+)s\)$`).FindStringSubmatch(text)
	if m == nil {
		t.Fatalf("no line naming ./ok's bound")
	}
	bound, _ := strconv.Atoi(m[1])
	startup, _ := strconv.Atoi(m[2])
	if startup < 1 || bound != 2*7+4*startup {
		t.Errorf("./ok's bound is %ds after a start-up run of %ds, want 2*7s plus four times it", bound, startup)
	}
	calls, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"go test -json -count=1 -run ^$ ./broken",
		"go test -json -count=1 -run ^$ ./ok",
		fmt.Sprintf("go test -json -count=1 -timeout %s -run ^(TestA|TestB)$ ./ok", time.Duration(bound)*time.Second),
		"bun test --reporter=junit --reporter-outfile=" + filepath.Join(keep, "bun-0.xml") + " src/a.test.ts in " + filepath.Join(repo, "packages/contracts"),
	} {
		if !slices.Contains(strings.Split(string(calls), "\n"), want) {
			t.Errorf("no call %q", want)
		}
	}
	if strings.Contains(string(calls), "-timeout") && strings.Contains(string(calls), "TestC") {
		t.Errorf("./broken's rows ran after its start-up run failed")
	}
	steps, err := os.ReadFile(filepath.Join(keep, "go-steps.tsv"))
	if want := fmt.Sprintf("./broken\t3\t%s\n./ok\t0\t%s\n", filepath.Join(keep, "go-0.err"), filepath.Join(keep, "go-1.err")); err != nil || string(steps) != want {
		t.Errorf("go-steps.tsv is %q (%v), want %q", steps, err, want)
	}
	if steps, err := os.ReadFile(filepath.Join(keep, "bun-steps.tsv")); err != nil || string(steps) != "packages/contracts/src/a.test.ts\t0\n" {
		t.Errorf("bun-steps.tsv is %q (%v), want the file and its exit status 0", steps, err)
	}
	for _, name := range []string{"go-0.json", "go-0.err", "go-1.json", "go-1.err", "bun-0.xml", "bun-0.txt"} {
		if info, err := os.Stat(filepath.Join(keep, name)); err != nil || (name != "go-1.err" && info.Size() == 0) {
			t.Errorf("-keep holds no %s with its record (%v)", name, err)
		}
	}
}

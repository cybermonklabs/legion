package ghautomerge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/githubrest"
)

// failingJob is the Actions job the integration check run of a merge group ran as, the id GitHub
// answers as that check run's databaseId and the id its log is read by. The number is the one
// measured on run 42397923554 of cybermonklabs/legion.
const failingJob = 128479266994

// jobLogFiller is the line a log is padded with to pass a byte bound, and jobLogPath where a
// job's log is read, the Actions jobs route under the REST base of acme/widgets.
const (
	jobLogFiller = "a line of a log that says nothing about what failed\n"
	jobLogPath   = "/repos/acme/widgets/actions/jobs/"
)

// goLog is the end of a Go test binary's log, where it prints a line for each test that failed,
// its subtests among them.
const goLog = `=== RUN   TestTheQueueTakesTheEntryOut
    removal_test.go:42: want the entry taken out, got it queued
--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)
--- FAIL: TestTheNoticeNamesTheRun/a_run_that_failed (0.00s)
FAIL
exit status 1
FAIL	github.com/sjawhar/legion/daemon/internal/ghautomerge	0.241s
`

// browserLog is the end of a browser runner's log: a progress row of the kind it prints as it
// goes, then the summary block it ends with, which is measured verbatim from job 128479266994's
// log of run 42397923554, bytes 426,973 to the end. Its failing row is the longest name shape
// measured here, 141 characters, which the bound on one job's line is read off
// (failingTestsNamed).
const browserLog = `  ✘ 1463 [firefox] › e2e/deep-links.e2e.ts:442:1 › a deep link to a step (3.1s)

  1 failed
    [chromium] › e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone
  37 skipped
  55 did not run
  1193 passed (15.4m)
`

// massFailureLog is the end of a Go log for a run whose every test failed: count failure lines,
// one a test, which is the shape a mass failure prints and far more names than one run's line
// carries.
func massFailureLog(count int) string {
	var log strings.Builder
	for i := range count {
		fmt.Fprintf(&log, "--- FAIL: TestTheRowFailed%02d (0.01s)\n", i)
	}
	log.WriteString("FAIL\nexit status 1\n")
	return log.String()
}

// longFailureLog is the end of a Go log whose failure line, and the name on it, run far past
// what a run's line carries: a table-driven subtest named for the whole sentence its case is,
// which Go prints with that sentence's spaces as underscores.
var longFailureLog = "--- FAIL: TestTheRowTellsTheArchitect/" +
	strings.Repeat("a_case_named_for_the_whole_sentence_it_is_", 8) + "end (0.01s)\nFAIL\n"

// longBrowserRowLog is a browser runner's summary block whose one row runs as far past that
// bound: a spec names its test for the sentence it checks, and the row carries the file, the
// line and that whole title.
var longBrowserRowLog = "  1 failed\n    [chromium] › e2e/gates.e2e.ts:12:1 › " +
	strings.Repeat("a title as long as the sentence this case is ", 8) + "end\n  2 skipped\n"

// markdownNameLog is a browser runner's summary block whose one row carries markdown's own
// punctuation in its title: a spec file writes that title, and the line this read makes of it is
// rendered into an implementer's prompt and an issue's markdown.
const markdownNameLog = "  1 failed\n" +
	"    [chromium] › e2e/prompt-blocks.e2e.ts:12:1 › a title with `code`, [a link](x) and **bold**\n" +
	"  2 skipped\n"

// mentionLog is a log that says Go's failure marker with no line opening with it: a step that
// greps for the marker prints the pattern it looked for, and a pattern is no failing test.
const mentionLog = "Run grep -c -- '--- FAIL: ' test.log\n" +
	"0 lines match --- FAIL: TestNothingFailedHere\n" +
	"Process completed with exit code 1.\n"

// stamped is log as GitHub's own job-log route carries it, which is what the daemon reads: every
// line prefixed with the UTC timestamp GitHub writes before it and one space. Measured on job
// 128479266994 of run 42397923554 of cybermonklabs/legion, the job browserLog is quoted from:
// its summary block's count line reads "2026-11-07T18:04:24.8502586Z   1 failed" and its failing
// row "2026-11-07T18:04:24.8503263Z     [chromium] › e2e/gated-step-approvals.e2e.ts:386:1 › …",
// so no line of a real job log begins with the indentation a summary block prints (INFRA-1442).
func stamped(log string) string {
	lines := strings.Split(strings.TrimSuffix(log, "\n"), "\n")
	for i, line := range lines {
		lines[i] = fmt.Sprintf("2026-11-07T18:04:%02d.8502586Z %s", i%60, line)
	}
	return strings.Join(lines, "\n") + "\n"
}

// What the end of a job's log states failed, read out of the bytes themselves: Go's "--- FAIL:"
// lines and a browser runner's summary block are the two shapes read, each off GitHub's job-log
// route, which writes a timestamp before every line, and off a runner's own output, which writes
// none. The "✘" rows a browser runner prints as it goes are not read: they stand in the middle of
// a stream this read only reaches the end of. A log naming more failures than a line carries has
// the rest counted, and a name as long as the line it was printed on is cut, since one generated
// name would otherwise fill the whole block a Dispatch post holds the removal in. A tail stating
// neither shape names no test at all (INFRA-1442).
func TestTheEndOfAJobsLogSaysWhichTestsFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		log  string
		// names is every test the read must name, first the line it must quote, and more how
		// many it must count past the names. cut is the names and the line being held to
		// failureLineUnits runes with "…" after them, where the log states one longer.
		names []string
		first string
		more  int
		cut   bool
	}{
		{name: "a Go log naming the tests that failed", log: goLog,
			names: []string{"TestTheQueueTakesTheEntryOut", "TestTheNoticeNamesTheRun/a_run_that_failed"},
			first: "--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)"},
		// Go's shape off GitHub's own route, measured on job 128558893568, whose first failure
		// line reads "2026-11-07T23:22:24.4790233Z --- FAIL: …": the names are the names, and
		// the line is the one a reader would quote, with no timestamp in front of it.
		{name: "a Go log as GitHub's job log carries it", log: stamped(goLog),
			names: []string{"TestTheQueueTakesTheEntryOut", "TestTheNoticeNamesTheRun/a_run_that_failed"},
			first: "--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)"},
		{name: "a browser runner's summary block", log: browserLog,
			names: []string{"e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone"},
			first: "[chromium] › e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone"},
		{name: "a browser runner's summary block as GitHub's job log carries it", log: stamped(browserLog),
			names: []string{"e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone"},
			first: "[chromium] › e2e/gated-step-approvals.e2e.ts:386:1 › a gate with no step for the primary document, and the longest awaiting label, fits every width @phone"},
		// A run whose every test failed states a name a line. Ten are named and the rest counted,
		// so the line stays inside the one block Dispatch holds a comment, a message and an ask
		// part to, which a log read whole would pass.
		{name: "a log naming more failures than the line carries", log: massFailureLog(37),
			names: []string{"TestTheRowFailed00", "TestTheRowFailed01", "TestTheRowFailed02", "TestTheRowFailed03", "TestTheRowFailed04",
				"TestTheRowFailed05", "TestTheRowFailed06", "TestTheRowFailed07", "TestTheRowFailed08", "TestTheRowFailed09"},
			first: "--- FAIL: TestTheRowFailed00 (0.01s)", more: 27},
		{name: "a Go name and failure line longer than the line carries", log: longFailureLog, cut: true},
		{name: "a browser row longer than the line carries", log: longBrowserRowLog, cut: true},
		// The marker states a failure where a line opens with it. A line that only says it — a
		// step that greps for it prints the pattern it looked for — states no test name.
		{name: "a line that says Go's marker without opening with it", log: mentionLog},
		{name: "a log stating no test name at all", log: "Process completed with exit code 1.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			names, first, more := failingTests([]byte(tc.log))
			if more != tc.more {
				t.Fatalf("failingTests counted %d failures past the names, want %d", more, tc.more)
			}
			if tc.cut {
				if len(names) != 1 {
					t.Fatalf("failingTests named %q, want the one test the log states", names)
				}
				for what, text := range map[string]string{"name": names[0], "first failure line": first} {
					if runes := []rune(text); len(runes) != failureLineUnits+1 || !strings.HasSuffix(text, "…") {
						t.Fatalf("the %s is %d runes (%q), want %d and the mark of where it was cut", what, len(runes), text, failureLineUnits)
					}
				}
				return
			}
			if !slices.Equal(names, tc.names) {
				t.Fatalf("failingTests named %q, want %q", names, tc.names)
			}
			if first != tc.first {
				t.Fatalf("failingTests quoted %q as the first failure line, want %q", first, tc.first)
			}
		})
	}
}

// heldJobLog is what a fake GitHub answers a read of one job's log with: the log itself, or the
// status it refuses every read with, the redirect it answers instead (the pre-signed blob URL
// GitHub answers a real read with), how many bytes of filler it writes before the log, which is
// how a log past githubrest.JobLogBound is made, and how long it holds the answer open, which is
// how a read past a time bound is made. arrived is closed as the read reaches it, for a test
// that ends its own reads while one is in flight.
type heldJobLog struct {
	log      string
	status   int
	redirect string
	fill     int
	delay    time.Duration
	arrived  chan struct{}
}

// jobLogsOf is a client reading job logs off a server answering held: GET
// /actions/jobs/{id}/logs under the REST base of acme/widgets, the one route a job's log lives
// on (githubrest.RepositoryAPI). A job the fake holds no log for is GitHub's 404, as a check run
// no Actions job backs would answer.
func jobLogsOf(t *testing.T, held heldJobLog) githubrest.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if held.arrived != nil {
			close(held.arrived)
			held.arrived = nil
		}
		switch {
		case r.Method != http.MethodGet || r.URL.Path != fmt.Sprintf("%s%d/logs", jobLogPath, failingJob):
			t.Errorf("unexpected job log request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
		case held.status != 0:
			http.Error(w, `{"message":"Resource not accessible by integration"}`, held.status)
		case held.redirect != "":
			http.Redirect(w, r, held.redirect, http.StatusFound)
		default:
			if held.delay > 0 {
				select {
				case <-time.After(held.delay):
				case <-r.Context().Done():
					return
				}
			}
			for written := 0; written < held.fill; written += len(jobLogFiller) {
				if _, err := io.WriteString(w, jobLogFiller); err != nil {
					return
				}
			}
			_, _ = io.WriteString(w, held.log)
		}
	}))
	t.Cleanup(server.Close)
	return githubrest.Client{Token: "token", API: githubrest.RepositoryAPI(server.URL, ghrepo.MustParse("acme/widgets"))}
}

// logBound is the time bound the row proving a read past it builds its own log client with
// (githubrest.Client's JobLogTimeout): the production bound is a minute, and a row that waited it
// out would cost every run of this suite that minute, for a value that is not what the row proves.
const logBound = 50 * time.Millisecond

// signedBlob is the pre-signed URL GitHub redirects a read of a job's log to, as the row proving
// a failed fetch names no URL answers one: the shape GitHub's store answers (a blob path with
// GitHub's se=, sp= and sig= query, whose sig is the signature that stands for the token) on a
// host nothing listens on, so the GET that follows the redirect fails and Go's client answers a
// *url.Error carrying this whole URL. Port 1 is privileged and bound by nothing, so the connect
// is refused at once rather than waited out. blobSignature is that signature, the one string no
// line the daemon renders may carry.
const (
	blobSignature = "S3CR3T-signature-of-the-read"
	signedBlob    = "http://127.0.0.1:1/legion-logs/128479266994?se=2026-11-08T19%3A00%3A00Z&sp=r&sig=" + blobSignature
)

// The end of a failing run's line is what the log of the Actions job it ran as says failed: the
// tests its end states as failed, with the first line stating one, or which way the daemon could
// not read them — the end states no test name, the body passed githubrest.JobLogBound, the read
// passed the log client's time bound, the deadline this removal's reads share ran out, the
// daemon stopped, the fetch failed, or GitHub refused the read because this deployment's App was
// never granted GitHub's optional Actions read. A fetch that failed on the URL GitHub redirects
// the read to says no URL of its own: that one is pre-signed, and a signature belongs in no
// message. A name is quoted where it carries markdown's own punctuation, since the line is
// rendered into an implementer's prompt and an issue's markdown (INFRA-1442).
// The name is the one the issue's check k4 runs (`go test ./internal/ghautomerge/ -run
// TestTheFailingRuns`), since this is the table of the five logs that check names: a `-run`
// pattern matching nothing exits 0, so a later move keeps this prefix or the check runs nothing.
func TestTheFailingRunsLineSaysWhatEachJobsLogStatesFailed(t *testing.T) {
	for _, tc := range []struct {
		name string
		held heldJobLog
		// bound is the time bound the row's own log client is built with, so a read that has to
		// pass it waits a moment rather than the minute production allows; budget is the
		// deadline the row gives the removal's reads, and stop ends them as the daemon stopping
		// does, while the read is in flight.
		bound, budget time.Duration
		stop          bool
		// wants is what the line must say, and absent what it must not.
		wants, absent []string
	}{
		{name: "a Go log naming the tests that failed", held: heldJobLog{log: goLog},
			wants: []string{"its log names TestTheQueueTakesTheEntryOut, TestTheNoticeNamesTheRun/a_run_that_failed as failing",
				`its first failure line "--- FAIL: TestTheQueueTakesTheEntryOut (0.01s)"`},
			absent: []string{"could not read", "removal_test.go"}},
		{name: "a log naming more failures than the line carries", held: heldJobLog{log: massFailureLog(37)},
			wants:  []string{"its log names TestTheRowFailed00, ", "TestTheRowFailed09 and 27 more as failing"},
			absent: []string{"could not read", "TestTheRowFailed10", "TestTheRowFailed36"}},
		// A name is text out of a log on its way into an implementer's prompt and an issue's
		// markdown, so one carrying markdown's own punctuation is quoted rather than rendered.
		{name: "a name carrying markdown and a backtick", held: heldJobLog{log: markdownNameLog},
			wants: []string{"its log names \"e2e/prompt-blocks.e2e.ts:12:1 › a title with `code`, " +
				"[a link](x) and **bold**\" as failing"},
			absent: []string{"could not read", "2 skipped"}},
		{name: "a log stating no test name", held: heldJobLog{log: "Process completed with exit code 1.\n"},
			wants:  []string{"could not read its failing tests", "the end of its log states no test name"},
			absent: []string{"as failing"}},
		{name: "a fetch that failed", held: heldJobLog{status: http.StatusBadGateway},
			wants: []string{"could not read its failing tests", "the fetch of its log failed", "502"}},
		{name: "a fetch refused for want of the Actions read", held: heldJobLog{status: http.StatusForbidden},
			wants: []string{"could not read its failing tests", "GitHub refused the log",
				"never granted GitHub's optional Actions read"}},
		{name: "a log past the byte bound", held: heldJobLog{log: goLog, fill: githubrest.JobLogBound + 1},
			wants:  []string{"could not read its failing tests", "passed the 160 MiB this read takes"},
			absent: []string{"as failing"}},
		{name: "a read past the time bound", held: heldJobLog{log: goLog, delay: time.Second}, bound: logBound,
			wants: []string{"could not read its failing tests",
				fmt.Sprintf("the read passed its %s bound", logBound)},
			absent: []string{"as failing", "fetch of its log failed"}},
		// The deadline every read of one removal shares is the daemon's removalLogReads, and a
		// read it cut short is no fetch that failed: GitHub answered, and the row simply had no
		// more time to spend on this log.
		{name: "a read the removal's own reads ran out of time for", held: heldJobLog{log: goLog, delay: time.Second}, budget: logBound,
			wants:  []string{"could not read its failing tests", "the removal's reads ran out of time"},
			absent: []string{"as failing", "fetch of its log failed", "passed its"}},
		// The daemon stopping is not the log's doing either. Its caller decides what becomes of
		// a removal read under a stop (the daemon's readFailures): this line only says what
		// happened.
		{name: "a read the daemon's own stop ended", held: heldJobLog{log: goLog, delay: time.Second}, stop: true,
			wants:  []string{"could not read its failing tests", "the daemon stopped before its log was read"},
			absent: []string{"as failing", "fetch of its log failed"}},
		{name: "a fetch that failed on the pre-signed URL GitHub redirects it to", held: heldJobLog{redirect: signedBlob},
			wants:  []string{"could not read its failing tests", "the fetch of its log failed"},
			absent: []string{"as failing", "sig=", blobSignature, "legion-logs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			arrived := make(chan struct{})
			tc.held.arrived = arrived
			logs := jobLogsOf(t, tc.held)
			logs.JobLogTimeout = tc.bound
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			if tc.budget > 0 {
				bounded, done := context.WithTimeout(ctx, tc.budget)
				defer done()
				ctx = bounded
			}
			if tc.stop {
				go func() {
					<-arrived
					stop()
				}()
			}
			line := jobFailures(ctx, logs, failingJob)
			for _, want := range tc.wants {
				if !strings.Contains(line, want) {
					t.Fatalf("the failing job's line %q, want it to say %q", line, want)
				}
			}
			for _, absent := range tc.absent {
				if strings.Contains(line, absent) {
					t.Fatalf("the failing job's line %q says %q, want it left out", line, absent)
				}
			}
		})
	}
}

// One removal names as many of its failing runs as fit a bound of their own and counts the rest,
// however many jobs a merge group failed across. One job's line is held under some 2,400 units
// by the names and the first failure line it carries (failingTestsNamed, failureLineUnits), so a
// base broken across twenty jobs would write fifty thousand into the one block Dispatch holds a
// post to, which Dispatch refuses whole (INFRA-1442). The first run is named whatever its own
// line costs: a removal that named none would say nothing of what failed.
func TestARemovalNamesItsRunsUpToABoundAndCountsTheRest(t *testing.T) {
	// Each line is the worst a job's line runs to: ten cut names and a cut first failure line.
	line := func(i int) FailingRun {
		return FailingRun{Name: fmt.Sprintf("integration-%02d", i), Result: "failure", URL: failedRun,
			Log: ": its log names " + strings.Repeat(cutLine(strings.Repeat("x", failureLineUnits+1))+", ", failingTestsNamed) +
				"as failing, its first failure line " + cutLine(strings.Repeat("y", failureLineUnits+1))}
	}
	runs := make([]FailingRun, 20)
	for i := range runs {
		runs[i] = line(i)
	}
	named := FailingRuns(runs)
	counted := strings.Count(named, "integration-")
	switch {
	case counted == 0 || counted == len(runs):
		t.Fatalf("the removal named %d of its %d runs, want some of them named and the rest counted", counted, len(runs))
	case !strings.HasSuffix(named, fmt.Sprintf(" and %d more runs", len(runs)-counted)):
		t.Fatalf("the removal's runs end %q, want the runs it did not name counted", named[max(0, len(named)-80):])
	case len(named) > failingRunsUnits+len(line(0).String())+32:
		t.Fatalf("the removal names %d units of runs, want it held to %d and what the last line it named costs", len(named), failingRunsUnits)
	}
	// Every run it does name is named whole: a line cut in the middle would leave a run with
	// half a link and half a test name.
	for i := range counted {
		if !strings.Contains(named, runs[i].String()) {
			t.Fatalf("the removal names run %d in part, want each run it names named whole", i)
		}
	}
	few := []FailingRun{line(0), line(1)}
	if joined := FailingRuns(few); joined != few[0].String()+"; "+few[1].String() {
		t.Fatalf("two runs are named %q, want both of them, \"; \" between", joined)
	}
	if one := FailingRuns([]FailingRun{line(0), line(1)}[:1]); !strings.Contains(one, "integration-00") || strings.Contains(one, "more runs") {
		t.Fatalf("one run of a line past the bound is named %q, want the run itself and nothing counted", one)
	}
}

// A removal's logs are read at once rather than one after another, so a merge group with a
// dozen failing jobs costs the row about one read's wall time: the reads a removal makes are
// capped at jobLogReads, and the process's own cap stands above them
// (githubrest's jobLogReadsAtOnce). A run GitHub named no Actions job for is left as it stands,
// with no read made for it at all.
func TestTheLogsOfARemovalsFailingJobsAreReadAtOnce(t *testing.T) {
	var reading, most int
	var mu sync.Mutex
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reading++
		most = max(most, reading)
		mu.Unlock()
		<-release
		mu.Lock()
		reading--
		mu.Unlock()
		_, _ = io.WriteString(w, goLog)
	}))
	defer server.Close()
	logs := githubrest.Client{Token: "token", API: githubrest.RepositoryAPI(server.URL, ghrepo.MustParse("acme/widgets"))}
	runs := make([]FailingRun, jobLogReads+2)
	for i := range runs {
		runs[i] = FailingRun{Name: fmt.Sprintf("job-%d", i), Result: "failure", URL: failedRun, JobID: int64(i + 1)}
	}
	runs = append(runs, FailingRun{Name: "licence-scan", Result: "failure", URL: "https://scanner.example/checks/9"})
	done := make(chan struct{})
	go func() {
		ReadFailingTests(context.Background(), logs, runs)
		close(done)
	}()
	for {
		mu.Lock()
		waiting := reading
		mu.Unlock()
		if waiting >= jobLogReads {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	<-done
	if most > jobLogReads {
		t.Fatalf("%d of a removal's logs were read at once, want no more than %d", most, jobLogReads)
	}
	for i, run := range runs {
		switch {
		case run.JobID == 0 && run.Log != "":
			t.Fatalf("run %d is an Actions job of no workflow run and says %q, want no log read for it", i, run.Log)
		case run.JobID != 0 && !strings.Contains(run.Log, "its log names TestTheQueueTakesTheEntryOut"):
			t.Fatalf("run %d says %q, want what its own log states failed", i, run.Log)
		}
	}
}

// A line of a log is as long as whatever printed it made it, and a name on it as long as the
// word it is: both are held to failureLineUnits runes with the mark of where they were cut, and
// what is shorter than that is left exactly as it stands.
func TestALogsLineIsCutToWhatARunsLineCarries(t *testing.T) {
	for _, tc := range []struct{ name, line, want string }{
		{name: "a line inside the bound", line: "--- FAIL: TestShort (0.01s)", want: "--- FAIL: TestShort (0.01s)"},
		{name: "a line at the bound", line: strings.Repeat("x", failureLineUnits), want: strings.Repeat("x", failureLineUnits)},
		{name: "a line past the bound", line: strings.Repeat("x", failureLineUnits+1), want: strings.Repeat("x", failureLineUnits) + "…"},
		{name: "a line of runes past the bound", line: strings.Repeat("é", failureLineUnits+1), want: strings.Repeat("é", failureLineUnits) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := cutLine(tc.line); got != tc.want {
				t.Fatalf("cutLine of %d runes = %d runes, want %d", len([]rune(tc.line)), len([]rune(got)), len([]rune(tc.want)))
			}
		})
	}
}

// A read of a log GitHub never answered is told apart from one it did: the error a fetch failed
// with says the request's own operation and what failed under it, and never the URL, since the
// URL GitHub redirects a job-log read to is pre-signed.
func TestAFailedFetchOfALogNamesNoPreSignedURL(t *testing.T) {
	route := &url.Error{Op: "Get", URL: signedBlob, Err: errors.New("dial tcp 127.0.0.1:1: connect: connection refused")}
	said := fetchFailure(route)
	switch {
	case strings.Contains(said, blobSignature) || strings.Contains(said, "legion-logs"):
		t.Fatalf("a failed fetch says %q, want no pre-signed URL in it", said)
	case !strings.Contains(said, "Get") || !strings.Contains(said, "connection refused"):
		t.Fatalf("a failed fetch says %q, want the request's operation and what failed under it", said)
	}
	if plain := fetchFailure(errors.New("the answer was no JSON")); plain != "the answer was no JSON" {
		t.Fatalf("an error of another shape says %q, want it kept as it is", plain)
	}
}

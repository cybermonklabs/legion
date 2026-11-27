package specgraph

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// R48 with R8: a test that did not run at the commit has no result there, and go test can answer
// a run of a package from its test cache. Go 1.27 marks such a package in one place, its summary
// line "ok  \t<import path>\t(cached)", with what came after the elapsed time after it, and it
// replays the run's events as they were, frames and all. Each report is a real run of Go 1.27.3
// that testdata/gotest/cached/make-reports.sh makes: each run of a replayed package reads cached,
// in each form of the line, and a package of the same report that ran again reads its outcome. The
// control is first.jsonl, the first run of the same package, which reads pass and skip. The
// package of cached-long.jsonl has an import path of 1,133 bytes, so go test's converter writes
// its summary line in two output events, and "(cached)" is in the second; its control,
// first-long.jsonl, is the first run of that package, whose summary line comes in two events too.
// The TestMain of the package of cached-mainline.jsonl prints a marked run line of TestZ after the
// tests, so go's own summary line is an event of TestZ, and it still marks each run of the package
// cached: the event's test name cannot tell a printed summary line from go's own. Its control,
// first-mainline.jsonl, is the first run, whose summary line holds no "(cached)": the converter
// writes the package's end as an end of TestZ there, so the report holds no end of the package,
// and each run reads could_not_run.
func TestARunThatGoTestReplaysFromItsCacheIsCached(t *testing.T) {
	t.Parallel()
	const cached = "example.com/acme/gotest/cached "
	const mainline = "example.com/acme/gotest/mainline "
	long := "example.com/acme/gotest/long" + strings.Repeat("/x"+strings.Repeat("y", 219), 5) + " "
	readReports(t, []reportRow{
		{"cached/first.jsonl", []string{cached + "TestA pass", cached + "TestB skip"}},
		{"cached/cached.jsonl", []string{cached + "TestA cached", cached + "TestB cached"}},
		{"cached/cached-cover.jsonl", []string{cached + "TestA cached", cached + "TestB cached"}},
		{"cached/cached-two.jsonl", []string{cached + "TestA cached", cached + "TestB cached", "example.com/acme/gotest/fresh TestFresh pass"}},
		{"cached/first-long.jsonl", []string{long + "TestA pass"}},
		{"cached/cached-long.jsonl", []string{long + "TestA cached"}},
		{"cached/first-mainline.jsonl", []string{mainline + "TestA could_not_run", mainline + "TestZ could_not_run"}},
		{"cached/cached-mainline.jsonl", []string{mainline + "TestA cached", mainline + "TestZ cached"}},
	})
	// The form " [no tests to run]" holds no run, so it gives no entry.
	if got := readReport(t, "cached/cached-norun.jsonl"); len(got) != 0 {
		t.Errorf("ReadGoTestJSON(cached/cached-norun.jsonl) = %q, want no entry", outcomes(got))
	}
}

// A report can end before its package's cached summary line. The prefix then cannot prove that the
// passed test ran at this commit, so it reads could_not_run.
func TestACachedReportBeforeItsSummaryDoesNotCountAsFresh(t *testing.T) {
	t.Parallel()
	content, err := os.ReadFile("testdata/gotest/cached/cached.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	events := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	got, err := ReadGoTestJSON(strings.NewReader(strings.Join(events[:6], "\n") + "\n"))
	if err != nil || !reflect.DeepEqual(outcomes(got), []string{"example.com/acme/gotest/cached TestA could_not_run"}) {
		t.Errorf("ReadGoTestJSON(the cached report before its summary) = %q, %v; want TestA could_not_run", outcomes(got), err)
	}
}

// The summary line has no marker, so a test can print it. A stated limit, in the safe direction: a
// test that prints its own package's cached summary line makes each run of its package in that go
// test run read as replayed from the cache, TestB, which prints nothing, too. The event of a test's
// name does not tell the two apart, since go's own summary line can come in such an event too,
// after a TestMain that prints a run line (cached-mainline.jsonl of
// TestARunThatGoTestReplaysFromItsCacheIsCached). For another package the line marks nothing. Each
// report ends with the package's own end event, as go test writes it.
func TestAPrintedCachedLineMarksOnlyItsOwnPackage(t *testing.T) {
	t.Parallel()
	// run is a report of package pkg where TestA prints line and passes, and then TestB prints
	// nothing and passes.
	run := func(pkg, line string) string {
		test := func(name, printed string) string {
			of := `"Package":"` + pkg + `","Test":"` + name + `"`
			report := `{"Action":"run",` + of + `}` + "\n" +
				`{"Action":"output",` + of + `,"Output":"=== RUN   ` + name + `\n","OutputType":"frame"}` + "\n"
			if printed != "" {
				report += `{"Action":"output",` + of + `,"Output":"` + printed + `\n"}` + "\n"
			}
			return report + `{"Action":"output",` + of + `,"Output":"--- PASS: ` + name + ` (0.00s)\n","OutputType":"frame"}` + "\n" +
				`{"Action":"pass",` + of + `,"Elapsed":0}` + "\n"
		}
		return test("TestA", line) + test("TestB", "") + `{"Action":"pass","Package":"` + pkg + `","Elapsed":0}` + "\n"
	}
	for _, row := range []struct {
		name, report string
		want         []string
	}{
		{"a test that prints its own package's cached summary line makes each run of its package read as replayed from the cache",
			run("p", `ok  \tp\t(cached)`), []string{"p TestA cached", "p TestB cached"}},
		{"another package's line", run("p", `ok  \tq\t(cached)`), []string{"p TestA pass", "p TestB pass"}},
		{"a line of its own package that ran", run("p", `ok  \tp\t0.01s`), []string{"p TestA pass", "p TestB pass"}},
	} {
		got, err := ReadGoTestJSON(strings.NewReader(row.report))
		if err != nil || !reflect.DeepEqual(outcomes(got), row.want) {
			t.Errorf("%s: ReadGoTestJSON = %q, %v; want %q", row.name, outcomes(got), err, row.want)
		}
	}
}

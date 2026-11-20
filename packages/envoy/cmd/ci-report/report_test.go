package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// The reports in testdata are what go test -json wrote, not text written by hand.
// testdata/report.jsonl is `go test -json -count=1 -trimpath -timeout 5s ./...` over a module,
// example.com/acme/probe, of four packages: claims, where one test passes, one fails and one
// skips; slow, where one test passes and one sleeps for 30 s, so go test stops it at the 5 s time
// limit; broken, whose test file does not build; and twice, which defines one test name in
// package twice and in package twice_test. testdata/killed.jsonl is go test of the sleeping test
// alone, with a time limit of 10 minutes, killed with SIGKILL after 4 s, as the end of a job
// kills it: the report stops after the test's run event. testdata/spec holds one check for each
// of those tests, one check of a test the report does not hold, one check of runner bun, and one
// check of another job.

// job is the job that the checks of testdata/spec name, all but a-read-of-one-claim-ends-in-time.
const job = "go-checks"

// TestOnlyTheChecksOfTheJobArePlanned plans the checks of testdata/spec for go-checks: each check
// that names go-checks is planned, in the tree's order, and the check of web-checks is not, though
// its test ran and passed in the report.
func TestOnlyTheChecksOfTheJobArePlanned(t *testing.T) {
	t.Parallel()
	var got []string
	for _, p := range planReports(specTree(t), job, readReport(t, "report.jsonl")) {
		got = append(got, p.Check)
	}
	want := []string{
		"a-read-of-many-claims-ends-in-time", "the-build-reads-the-claim", "the-claim-has-one-name",
		"the-claim-stays-after-a-restart", "the-daemon-waits-for-the-database", "the-lock-stays-after-a-restart",
		"the-owner-stays-after-a-restart", "the-page-shows-the-claim",
	}
	if !slices.Equal(got, want) {
		t.Errorf("planned checks = %q, want %q", got, want)
	}
	got = nil
	for _, p := range planReports(specTree(t), "web-checks", readReport(t, "report.jsonl")) {
		got = append(got, p.Check+" "+p.Outcome)
	}
	if want := []string{"a-read-of-one-claim-ends-in-time pass"}; !slices.Equal(got, want) {
		t.Errorf("web-checks plans %q, want %q", got, want)
	}
}

// TestEachCheckHasTheOutcomeOfItsTest plans each check of go-checks over the report and holds it
// to what its test did there.
func TestEachCheckHasTheOutcomeOfItsTest(t *testing.T) {
	t.Parallel()
	ran := readReport(t, "report.jsonl")
	planned := map[string]Planned{}
	for _, p := range planReports(specTree(t), job, ran) {
		planned[p.Check] = p
	}
	output := func(pkg, test string) string {
		t.Helper()
		i := slices.IndexFunc(ran, func(o specgraph.TestOutcome) bool {
			return o.Package == "example.com/acme/probe/"+pkg && o.Test == test
		})
		if i < 0 {
			t.Fatalf("the report holds no run of %s %s", pkg, test)
		}
		return ran[i].Output
	}
	for _, row := range []struct {
		name, check string
		want        Planned
		// observed is a line that the test's output, and so Observed, must hold.
		observed string
	}{
		{"one matching entry that passed gives its pass and its output", "the-claim-stays-after-a-restart", Planned{
			Test: "example.com/acme/probe/claims TestTheClaimStaysAfterARestart", Outcome: "pass",
			Observed: output("claims", "TestTheClaimStaysAfterARestart"),
		}, "--- PASS: TestTheClaimStaysAfterARestart"},
		{"one matching entry that failed gives its failure and its output", "the-owner-stays-after-a-restart", Planned{
			Test: "example.com/acme/probe/claims TestTheOwnerStaysAfterARestart", Outcome: "fail",
			Observed: output("claims", "TestTheOwnerStaysAfterARestart"),
		}, "--- FAIL: TestTheOwnerStaysAfterARestart"},
		{"a test that go test stopped at its time limit could not run, and did not fail", "a-read-of-many-claims-ends-in-time", Planned{
			Test: "example.com/acme/probe/slow TestTheSlowReadEnds", Outcome: "could_not_run",
			Observed: output("slow", "TestTheSlowReadEnds"),
			Why:      "the report gives the test no one end: go test stopped it at its time limit, its process or the job ended inside it, or the report gives it two ends",
		}, "panic: test timed out after 5s"},
		{"a skipped test gives a skip", "the-daemon-waits-for-the-database", Planned{
			Test: "example.com/acme/probe/claims TestTheWaitIsSkipped", Outcome: "skip",
			Observed: output("claims", "TestTheWaitIsSkipped"), Why: "the test skipped at this commit",
		}, "--- SKIP: TestTheWaitIsSkipped"},
		{"a test the report does not hold is absent", "the-lock-stays-after-a-restart", Planned{
			Test: "example.com/acme/probe/claims TestTheLockStaysAfterARestart", Outcome: "absent",
			Why: "the test did not run at this commit",
		}, ""},
		{"a test of a package that did not build is absent", "the-build-reads-the-claim", Planned{
			Test: "example.com/acme/probe/broken TestTheBuildReadsTheClaim", Outcome: "absent",
			Why: "the test did not run at this commit",
		}, ""},
		{"two matching entries are two tests", "the-claim-has-one-name", Planned{
			Test: "example.com/acme/probe/twice TestTheNameRunsTwice", Outcome: "two_tests",
			Why: "the report holds 2 runs of the test, and a result is the result of one test",
		}, ""},
		{"a test of runner bun is absent, and the reason names the runner", "the-page-shows-the-claim", Planned{
			Test: "packages/web/src/claims.test.ts", Outcome: "absent",
			Why: `the reporter reads go test reports only; this check's runner is bun`,
		}, ""},
	} {
		t.Run(row.name, func(t *testing.T) {
			row.want.Check = row.check
			if got := planned[row.check]; got != row.want {
				t.Errorf("planReports gives %+v\nwant %+v", got, row.want)
			}
			if row.observed != "" && !strings.Contains(row.want.Observed, row.observed) {
				t.Errorf("the output of %s holds no %q, so the fixture does not show the case", row.check, row.observed)
			}
		})
	}
}

// TestATestTheJobsEndKilledCouldNotRun plans the sleeping test's check over the report that the
// end of the job cut after the test started: the test could not run, and its result is written,
// never as a failure.
func TestATestTheJobsEndKilledCouldNotRun(t *testing.T) {
	t.Parallel()
	ran := readReport(t, "killed.jsonl")
	var got []Planned
	for _, p := range planReports(specTree(t), job, ran) {
		if p.Check == "a-read-of-many-claims-ends-in-time" {
			got = append(got, p)
		}
	}
	want := []Planned{{
		Check: "a-read-of-many-claims-ends-in-time", Test: "example.com/acme/probe/slow TestTheSlowReadEnds",
		Outcome: "could_not_run", Observed: "=== RUN   TestTheSlowReadEnds\n    slow_test.go:13: the read starts\n",
		Why: "the report gives the test no one end: go test stopped it at its time limit, its process or the job ended inside it, or the report gives it two ends",
	}}
	if !slices.Equal(got, want) {
		t.Errorf("planReports gives %+v\nwant %+v", got, want)
	}
	if !recorded(want[0].Outcome) {
		t.Errorf("a check whose test could not run is not recorded, want a result")
	}
}

// specTree is the tree of testdata/spec, which specgraph.Judge must take with no problem.
func specTree(t *testing.T) specgraph.Tree {
	t.Helper()
	tree, problems := specgraph.Judge(filesOf(t, filepath.Join("testdata", "spec")), specgraph.Options{})
	if len(problems) > 0 {
		t.Fatalf("testdata/spec has problems: %+v", problems)
	}
	return tree
}

// filesOf is each file directly in dir by its name, with its content.
func filesOf(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{}
	for _, entry := range entries {
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[entry.Name()] = content
	}
	return files
}

// readReport is testdata/<name> read with specgraph.ReadGoTestJSON.
func readReport(t *testing.T, name string) []specgraph.TestOutcome {
	t.Helper()
	report, err := os.Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	defer report.Close()
	ran, err := specgraph.ReadGoTestJSON(report)
	if err != nil {
		t.Fatal(err)
	}
	return ran
}

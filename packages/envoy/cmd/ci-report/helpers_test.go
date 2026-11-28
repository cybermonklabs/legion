package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// otherSHA is a commit that is not the checkout's, set as GITHUB_SHA.
const otherSHA = "0123456789abcdef0123456789abcdef01234567"

// oidcEnv is a stand-in of GitHub's OIDC token request, which `agent-secrets result-grant` passes
// on to the reporter: runReporter sets it in every run, and no call of the dispatch command may
// get it. Each value is a stand-in of this test's, built as name and value.
var oidcEnv = []string{
	envVar("ACTIONS_ID_TOKEN_REQUEST_URL", "https://oidc.example.test/request"),
	envVar("ACTIONS_ID_TOKEN_REQUEST_TOKEN", "a-stand-in-of-the-request-credential"),
}

// envVar is the environment entry that sets name to value.
func envVar(name, value string) string { return name + "=" + value }

// The reasons a line of testdata/spec over testdata/report.jsonl gives for a check with no call.
const (
	whyNotRun   = "the test did not run at this commit"
	whyNotEnded = "the report gives the test no one end: go test stopped it at its time limit, its process or the job ended inside it, or the report gives it two ends"
	whyBun      = `the reporter reads go test reports only; this check's runner is bun`
)

// recordedChecks are the checks of go-checks in testdata/spec whose test ran in
// testdata/report.jsonl, in the tree's order: one call each.
var recordedChecks = []string{"a-read-of-many-claims-ends-in-time", "the-claim-stays-after-a-restart", "the-owner-stays-after-a-restart"}

// probe is the run that most tests make: testdata/spec, checked out, over testdata/report.jsonl,
// for job go-checks and project ACME.
type probe struct {
	ran    []specgraph.TestOutcome
	report string // the report's absolute path
}

// newProbe reads testdata/report.jsonl.
func newProbe(t *testing.T) probe {
	t.Helper()
	report, err := filepath.Abs(filepath.Join("testdata", "report.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return probe{ran: readReport(t, "report.jsonl"), report: report}
}

// args is the reporter's argv over the probe's report, with the stub at cli as its dispatch
// command.
func (p probe) args(cli string) []string {
	return []string{"--dir", ".dispatch/spec", "--job", job, "--report", p.report, "--project", "ACME", "--dispatch-cli", cli}
}

// output is what the test pkg/test wrote in the report.
func (p probe) output(t *testing.T, pkg, test string) string {
	t.Helper()
	i := slices.IndexFunc(p.ran, func(o specgraph.TestOutcome) bool {
		return o.Package == "example.com/acme/probe/"+pkg && o.Test == test
	})
	if i < 0 {
		t.Fatalf("the report holds no run of %s %s", pkg, test)
	}
	return p.ran[i].Output
}

// lines is every line ci-report prints for the probe, with each recorded check's exit, result id
// and why as answered, by its check: a check missing from exits gets no exit, so the line of a
// check that got a call and is missing there differs from the line printed.
func (p probe) lines(exits map[string]*int, ids, whys map[string]string) []line {
	all := []line{
		{Check: "a-read-of-many-claims-ends-in-time", Test: "example.com/acme/probe/slow TestTheSlowReadEnds", Outcome: "could_not_run", Why: whyNotEnded},
		{Check: "the-build-reads-the-claim", Test: "example.com/acme/probe/broken TestTheBuildReadsTheClaim", Outcome: "absent", Why: whyNotRun},
		{Check: "the-claim-has-one-name", Test: "example.com/acme/probe/twice TestTheNameRunsTwice", Outcome: "two_tests", Why: "the report holds 2 runs of the test, and a result is the result of one test"},
		{Check: "the-claim-stays-after-a-restart", Test: "example.com/acme/probe/claims TestTheClaimStaysAfterARestart", Outcome: "pass"},
		{Check: "the-daemon-waits-for-the-database", Test: "example.com/acme/probe/claims TestTheWaitIsSkipped", Outcome: "skip", Why: "the test skipped at this commit"},
		{Check: "the-lock-stays-after-a-restart", Test: "example.com/acme/probe/claims TestTheLockStaysAfterARestart", Outcome: "absent", Why: whyNotRun},
		{Check: "the-owner-stays-after-a-restart", Test: "example.com/acme/probe/claims TestTheOwnerStaysAfterARestart", Outcome: "fail"},
		{Check: "the-page-shows-the-claim", Test: "packages/web/src/claims.test.ts", Outcome: "absent", Why: whyBun},
	}
	for i := range all {
		if code, ok := exits[all[i].Check]; ok {
			all[i].Exit, all[i].ResultID = code, ids[all[i].Check]
			if why, ok := whys[all[i].Check]; ok {
				all[i].Why = why
			}
		}
	}
	return all
}

// answered is the probe's lines where each recorded check's call exited code with the why given
// and no id.
func (p probe) answered(code int, why string) []line {
	exits, whys := map[string]*int{}, map[string]string{}
	for _, check := range recordedChecks {
		exits[check], whys[check] = new(code), why
	}
	return p.lines(exits, nil, whys)
}

// stored is every line of a run where each call stored its result as the stub prints it.
func (p probe) stored() []line {
	ids, exits := map[string]string{}, map[string]*int{}
	for _, check := range recordedChecks {
		ids[check], exits[check] = "row-"+check, new(0)
	}
	return p.lines(exits, ids, nil)
}

// endsAfterFirst is every line of a run whose grant ends after its first call (the stub's mode
// ends-after-first): the first recorded check in the tree's order is stored, and each later one
// is refused 407 at the CONNECT, its why naming the grant's end.
func (p probe) endsAfterFirst() []line {
	ended := grantEnded + " The dispatch command printed: " + tunnelRefused407Line
	first, later := recordedChecks[0], recordedChecks[1:]
	exits, whys := map[string]*int{first: new(0)}, map[string]string{}
	for _, check := range later {
		exits[check], whys[check] = new(1), ended
	}
	return p.lines(exits, map[string]string{first: "row-" + first}, whys)
}

// wantCalls is the one call for each test that ran, at commit: pass, fail and could not run, in
// the tree's order, each with the test's output on standard input and no ACTIONS_ID_TOKEN_*
// variable.
func (p probe) wantCalls(t *testing.T, commit string) []call {
	t.Helper()
	args := func(check, verdict string) []string {
		return []string{"result", "--project", "ACME", "--check", check, "--verdict", verdict, "--commit", commit, "--observed-file", "-"}
	}
	return []call{
		{args: args("a-read-of-many-claims-ends-in-time", "could_not_run"), stdin: p.output(t, "slow", "TestTheSlowReadEnds")},
		{args: args("the-claim-stays-after-a-restart", "pass"), stdin: p.output(t, "claims", "TestTheClaimStaysAfterARestart")},
		{args: args("the-owner-stays-after-a-restart", "fail"), stdin: p.output(t, "claims", "TestTheOwnerStaysAfterARestart")},
	}
}

// expectCalls fails the test unless the stub got exactly want.
func expectCalls(t *testing.T, s stub, want []call) {
	t.Helper()
	if calls := s.calls(t); !slices.EqualFunc(calls, want, equalCall) {
		t.Errorf("the stub got %d calls %+v\nwant %d calls %+v", len(calls), calls, len(want), want)
	}
}

// result is what one run of the binary did.
type result struct {
	code           int
	stdout, stderr string
}

// runReporter runs binary with args in dir, with this process's environment less any
// AGENT_SECRETS_MACHINE_IDENTITY, which the action sets and the command must not, GITHUB_SHA set
// to otherSHA, GitHub's OIDC token request (oidcEnv) set, and env added.
func runReporter(t *testing.T, binary, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "AGENT_SECRETS_MACHINE_IDENTITY=")
	})
	cmd.Env = slices.Concat(cmd.Env, []string{"GITHUB_SHA=" + otherSHA}, oidcEnv, env)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitError *exec.ExitError
	if err != nil && !errors.As(err, &exitError) {
		t.Fatal(err)
	}
	return result{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// line is one JSON line that ci-report prints, with exit null for a check with no call.
type line struct {
	Check    string `json:"check"`
	Test     string `json:"test"`
	Outcome  string `json:"outcome"`
	Exit     *int   `json:"exit"`
	ResultID string `json:"result_id"`
	Why      string `json:"why"`
}

// parseLines reads stdout as one JSON line for each check, refusing any other text.
func parseLines(t *testing.T, stdout string) []line {
	t.Helper()
	var lines []line
	for _, text := range strings.SplitAfter(stdout, "\n") {
		if text == "" {
			continue
		}
		var l line
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&l); err != nil || !strings.HasSuffix(text, "\n") {
			t.Fatalf("stdout holds a line that is not one JSON line of a check: %q (%v)", text, err)
		}
		lines = append(lines, l)
	}
	return lines
}

// expectLines fails the test unless stdout is exactly want, one JSON line each.
func expectLines(t *testing.T, stdout string, want []line) {
	t.Helper()
	if got := parseLines(t, stdout); !slices.EqualFunc(got, want, equalLine) {
		t.Errorf("stdout:\n%s\nwant the lines of %s", stdout, describe(want))
	}
}

// equalLine reports whether two lines are the same, their exits compared by value.
func equalLine(a, b line) bool {
	return a.Check == b.Check && a.Test == b.Test && a.Outcome == b.Outcome && a.ResultID == b.ResultID && a.Why == b.Why &&
		(a.Exit == nil) == (b.Exit == nil) && (a.Exit == nil || *a.Exit == *b.Exit)
}

// describe is lines as JSON, one a line, for a failure message.
func describe(lines []line) string {
	var b strings.Builder
	for _, l := range lines {
		encoded, _ := json.Marshal(l)
		b.Write(encoded)
		b.WriteByte('\n')
	}
	return b.String()
}

// checkout is a new git repository with one commit whose .dispatch/spec is a copy of spec (no
// such directory when spec is empty), and that commit.
func checkout(t *testing.T, spec string) (dir, commit string) {
	t.Helper()
	dir = t.TempDir()
	if spec != "" {
		if err := os.CopyFS(filepath.Join(dir, ".dispatch", "spec"), os.DirFS(spec)); err != nil {
			t.Fatal(err)
		}
	}
	git(t, dir, "init", "-q")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "the probe checks")
	return dir, strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
}

// git runs git with args in dir, with no configuration of this machine's, and answers its output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=ACME CI", "-c", "user.email=ci@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// copyOf is a copy of the files of dir in a new temporary directory.
func copyOf(t *testing.T, dir string) string {
	t.Helper()
	copied := filepath.Join(t.TempDir(), filepath.Base(dir))
	if err := os.CopyFS(copied, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	return copied
}

// writeFile writes content to path.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

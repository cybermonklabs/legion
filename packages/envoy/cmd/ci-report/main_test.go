package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/dispatch/architecture/notes"
	"github.com/sjawhar/envoy/internal/dispatch/specgraph"
)

// stubDispatch is the stand-in for the dispatch command that the tests hand ci-report through
// --dispatch-cli. It is this test's own program, not the real command. Each call writes its
// arguments, NUL-separated, to <n>.args, its standard input to <n>.stdin and the value of
// AGENT_SECRETS_MACHINE_IDENTITY to <n>.identity, in the directory CI_REPORT_STUB_DIR names, n
// counting from 0. With CI_REPORT_STUB_MODE=refuse it prints the broker's refusal of an exchange
// on standard output and exits 1, as the real command prints every refusal of Dispatch or of the
// broker (packages/envoy-client/src/dispatch-cli.ts, `print`, which writes to standard output).
// With CI_REPORT_STUB_MODE=landed it prints what the real command prints for a write that landed
// with its answer lost, {"landed_without_answer":"KEY_COMMITTED"}, and exits 0; with
// CI_REPORT_STUB_MODE=answer-lost it prints {"landed_without_answer":"ANSWER_LOST"}, what the real
// command prints for a write's 2xx whose body it could not read, and exits 0; with
// CI_REPORT_STUB_MODE=no-id it prints that JSON with no code, and exits 0. With
// CI_REPORT_STUB_MODE=unknown it prints what the real command prints for a write that went out
// and got no answer, {"write_outcome_unknown":"ECONNRESET"}, and exits 1; with unknown-exit-0 it
// prints the same line and exits 0. Otherwise it prints a stored row as the real command prints
// it, whole and over many lines, with the id row-<check> and the run that recorded it, its session
// and principal, and exits 0.
const stubDispatch = `#!/bin/sh
calls="$CI_REPORT_STUB_DIR"
n=0
while [ -e "$calls/$n.args" ]; do n=$((n + 1)); done
printf '%s\0' "$@" > "$calls/$n.args"
cat > "$calls/$n.stdin"
printf '%s' "${AGENT_SECRETS_MACHINE_IDENTITY-unset}" > "$calls/$n.identity"
check=
while [ $# -gt 0 ]; do
  if [ "$1" = --check ]; then check=$2; fi
  shift
done
if [ "$CI_REPORT_STUB_MODE" = refuse ]; then
  echo "` + brokerRefusal + `"
  exit 1
fi
if [ "$CI_REPORT_STUB_MODE" = landed ]; then
  echo '{"landed_without_answer":"` + contracts.IdempotencyKeyCommitted + `"}'
  exit 0
fi
if [ "$CI_REPORT_STUB_MODE" = answer-lost ]; then
  echo '{"landed_without_answer":"` + answerLost + `"}'
  exit 0
fi
if [ "$CI_REPORT_STUB_MODE" = no-id ]; then
  echo '{"landed_without_answer":""}'
  exit 0
fi
if [ "$CI_REPORT_STUB_MODE" = unknown ]; then
  echo '{"write_outcome_unknown":"ECONNRESET"}'
  exit 1
fi
if [ "$CI_REPORT_STUB_MODE" = unknown-exit-0 ]; then
  echo '{"write_outcome_unknown":"ECONNRESET"}'
  exit 0
fi
cat <<EOF
{
  "id": "row-$check",
  "seq": $n,
  "kind": "run",
  "attestation": {
    "_type": "https://in-toto.io/Statement/v1",
    "predicate": {
      "project": "ACME",
      "check": "$check",
      "recorder": {
        "session": "` + recorderSession + `",
        "principal": "workflow-results:example-org/example-repo/.github/workflows/ci-results.yaml"
      }
    }
  }
}
EOF
`

// brokerRefusal is what the stub prints in refuse mode: the broker's refusal of
// an exchange for a ref that no result entry admits.
const brokerRefusal = "BROKER_EXCHANGE_REFUSED: the broker refused the exchange of the job's token: invalid_request: no result entry admits ref refs/heads/a/b/c/d"

// answerLost is the code that the dispatch command gives a write whose 2xx it got and whose body
// it could not read: the client's own code, DISPATCH_ANSWER_LOST in
// packages/envoy-client/src/dispatch-http.ts, and none of Dispatch's.
const answerLost = "ANSWER_LOST"

// recorderSession is the session of the run that recorded every row the stub prints: a part of
// the row that is not its id.
const recorderSession = "ci-results:example-org/example-repo/.github/workflows/ci-results.yaml"

// otherSHA is a commit that is not the checkout's, set as GITHUB_SHA.
const otherSHA = "0123456789abcdef0123456789abcdef01234567"

// line is one JSON line that ci-report prints, with exit null for a check with no call.
type line struct {
	Check    string `json:"check"`
	Test     string `json:"test"`
	Outcome  string `json:"outcome"`
	Exit     *int   `json:"exit"`
	ResultID string `json:"result_id"`
	Why      string `json:"why"`
}

// call is one call of the stub: its arguments, its standard input and its identity variable.
type call struct {
	args            []string
	stdin, identity string
}

// TestTheBuiltBinary runs ci-report as a process over a checkout of testdata/spec and the report
// of testdata/report.jsonl, with the stub as its dispatch command.
func TestTheBuiltBinary(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "ci-report")
	ran := readReport(t, "report.jsonl")
	output := func(pkg, test string) string {
		for _, o := range ran {
			if o.Package == "example.com/acme/probe/"+pkg && o.Test == test {
				return o.Output
			}
		}
		t.Fatalf("the report holds no run of %s %s", pkg, test)
		return ""
	}
	report, err := filepath.Abs(filepath.Join("testdata", "report.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	const (
		notRun    = "the test did not run at this commit"
		notEnded  = "the report gives the test no one end: go test stopped it at its time limit, its process or the job ended inside it, or the report gives it two ends"
		bunRunner = `the reporter reads go test reports only; this check's runner is bun`
	)
	// lines is every line ci-report prints for testdata/spec at a run where each call stored its
	// result, with each recorded check's exit and result id given.
	lines := func(exits map[string]*int, ids map[string]string, whys map[string]string) []line {
		all := []line{
			{Check: "a-read-of-many-claims-ends-in-time", Test: "example.com/acme/probe/slow TestTheSlowReadEnds", Outcome: "could_not_run", Why: notEnded},
			{Check: "the-build-reads-the-claim", Test: "example.com/acme/probe/broken TestTheBuildReadsTheClaim", Outcome: "absent", Why: notRun},
			{Check: "the-claim-has-one-name", Test: "example.com/acme/probe/twice TestTheNameRunsTwice", Outcome: "two_tests", Why: "the report holds 2 runs of the test, and a result is the result of one test"},
			{Check: "the-claim-stays-after-a-restart", Test: "example.com/acme/probe/claims TestTheClaimStaysAfterARestart", Outcome: "pass"},
			{Check: "the-daemon-waits-for-the-database", Test: "example.com/acme/probe/claims TestTheWaitIsSkipped", Outcome: "skip", Why: "the test skipped at this commit"},
			{Check: "the-lock-stays-after-a-restart", Test: "example.com/acme/probe/claims TestTheLockStaysAfterARestart", Outcome: "absent", Why: notRun},
			{Check: "the-owner-stays-after-a-restart", Test: "example.com/acme/probe/claims TestTheOwnerStaysAfterARestart", Outcome: "fail"},
			{Check: "the-page-shows-the-claim", Test: "packages/web/src/claims.test.ts", Outcome: "absent", Why: bunRunner},
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
	recordedChecks := []string{"a-read-of-many-claims-ends-in-time", "the-claim-stays-after-a-restart", "the-owner-stays-after-a-restart"}
	// wantCalls is the one call for each test that ran: pass, fail and could not run, in the
	// tree's order, each with the test's output on standard input.
	wantCalls := func(commit string) []call {
		args := func(check, verdict string) []string {
			return []string{"result", "--project", "ACME", "--check", check, "--verdict", verdict, "--commit", commit, "--observed-file", "-"}
		}
		return []call{
			{args: args("a-read-of-many-claims-ends-in-time", "could_not_run"), stdin: output("slow", "TestTheSlowReadEnds"), identity: "unset"},
			{args: args("the-claim-stays-after-a-restart", "pass"), stdin: output("claims", "TestTheClaimStaysAfterARestart"), identity: "unset"},
			{args: args("the-owner-stays-after-a-restart", "fail"), stdin: output("claims", "TestTheOwnerStaysAfterARestart"), identity: "unset"},
		}
	}
	// stored is every line of a run where each call stored its result as the stub prints it.
	stored := func() []line {
		ids, exits := map[string]string{}, map[string]*int{}
		for _, check := range recordedChecks {
			ids[check], exits[check] = "row-"+check, new(0)
		}
		return lines(exits, ids, nil)
	}
	args := []string{"--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME"}

	t.Run("each test that ran writes one result, at the commit git names, and no other check is called", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		if got.code != 0 {
			t.Fatalf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
		}
		expectLines(t, got.stdout, stored())
		if calls := stub.calls(t); !slices.EqualFunc(calls, wantCalls(commit), equalCall) {
			t.Errorf("the stub got %+v\nwant %+v", calls, wantCalls(commit))
		}
		if want := fmt.Sprintf("ci-report: the commit is %s, which git rev-parse HEAD names; GITHUB_SHA is %s\n", commit, otherSHA); !strings.Contains(got.stderr, want) {
			t.Errorf("stderr holds no line %q:\n%s", want, got.stderr)
		}
		if want := fmt.Sprintf("ci-report: 8 checks of %s at %s name job %s; 3 results written, 0 calls failed\n", filepath.Join(dir, ".dispatch", "spec"), commit, job); !strings.HasSuffix(got.stderr, want) {
			t.Errorf("stderr does not end with %q:\n%s", want, got.stderr)
		}
	})

	// The step reads the check nodes of the files at its commit, through git, and never the files
	// of the working tree (R12). A step before it that changes the files and commits nothing
	// changes no line and no call.
	for _, row := range []struct {
		name   string
		change func(t *testing.T, spec string)
	}{
		{"a spec file that the working tree deleted after the commit is read as the commit holds it", func(t *testing.T, spec string) {
			if err := os.Remove(filepath.Join(spec, "probe.md")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a check's test that the working tree changed after the commit is read as the commit names it", func(t *testing.T, spec string) {
			file := filepath.Join(spec, "probe.md")
			text, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			// The commit's check names the test that failed; the working tree's names one that passed.
			const committed = `selector="example.com/acme/probe/claims TestTheOwnerStaysAfterARestart"`
			if !strings.Contains(string(text), committed) {
				t.Fatalf("the fixture holds no %s", committed)
			}
			writeFile(t, file, strings.Replace(string(text), committed, `selector="example.com/acme/probe/claims TestTheClaimStaysAfterARestart"`, 1))
		}},
		{"a spec file that the working tree added and the commit does not hold is not read", func(t *testing.T, spec string) {
			// A check of the job whose test ran and passed: read, it would get a line and a call.
			writeFile(t, filepath.Join(spec, "added.md"), `:::check{#a-read-of-one-claim-ends-in-time-here serves="a-read-of-the-claims-ends-in-time" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/slow TestTheQuickReadEnds" job="go-checks"}
Claim: A read of one claim ends in 1 second or less.
Procedure: The program writes one claim and reads it.
The read must end in 1 second or less.
Failing example: A read that waits for a lock that nobody holds.
:::
`)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir, commit := checkout(t, filepath.Join("testdata", "spec"))
			row.change(t, filepath.Join(dir, ".dispatch", "spec"))
			stub := newStub(t)
			got := runReporter(t, binary, dir, stub.env("store"), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
			if got.code != 0 {
				t.Fatalf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
			}
			expectLines(t, got.stdout, stored())
			if calls := stub.calls(t); !slices.EqualFunc(calls, wantCalls(commit), equalCall) {
				t.Errorf("the stub got %+v\nwant %+v", calls, wantCalls(commit))
			}
		})
	}

	t.Run("each entry of the directory at the commit that is not a spec file is a warning, a submodule included", func(t *testing.T) {
		t.Parallel()
		spec := copyOf(t, filepath.Join("testdata", "spec"))
		writeFile(t, filepath.Join(spec, "notes.txt"), "Not a spec file.\n")
		if err := os.Mkdir(filepath.Join(spec, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(spec, "sub", "inner.md"), "# Inside a directory\n")
		if err := os.Symlink("probe.md", filepath.Join(spec, "link.md")); err != nil {
			t.Fatal(err)
		}
		dir, first := checkout(t, spec)
		// A submodule exists only in the commit: git keeps it as a commit entry of the tree.
		git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+first+",.dispatch/spec/module")
		git(t, dir, "commit", "-q", "-m", "a submodule")
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
		if got.code != 0 {
			t.Fatalf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
		}
		specDir := filepath.Join(dir, ".dispatch", "spec")
		var warnings []string
		for _, text := range strings.Split(got.stderr, "\n") {
			if strings.HasPrefix(text, "ci-report: warning: ") {
				warnings = append(warnings, text)
			}
		}
		want := []string{
			"ci-report: warning: " + filepath.Join(specDir, "link.md") + ": not a spec file (symbolic link); the importer passes over it",
			"ci-report: warning: " + filepath.Join(specDir, "module") + ": not a spec file (submodule); the importer passes over it",
			"ci-report: warning: " + filepath.Join(specDir, "notes.txt") + ": not a spec file (file); the importer passes over it",
			"ci-report: warning: " + filepath.Join(specDir, "sub") + ": not a spec file (directory); the importer passes over it",
		}
		if !slices.Equal(warnings, want) {
			t.Errorf("the warnings are\n%s\nwant\n%s", strings.Join(warnings, "\n"), strings.Join(want, "\n"))
		}
		expectLines(t, got.stdout, stored())
	})

	t.Run("the spec directory is read from the working directory, a directory of the checkout below its root", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, filepath.Join(dir, ".dispatch"), stub.env("store"), "--dir", "spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		if got.code != 0 {
			t.Fatalf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
		}
		expectLines(t, got.stdout, stored())
		if calls := stub.calls(t); !slices.EqualFunc(calls, wantCalls(commit), equalCall) {
			t.Errorf("the stub got %+v\nwant %+v", calls, wantCalls(commit))
		}
	})

	t.Run("outside a git checkout the step refuses with one line and makes no call", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.CopyFS(filepath.Join(dir, ".dispatch", "spec"), os.DirFS(filepath.Join("testdata", "spec"))); err != nil {
			t.Fatal(err)
		}
		stub := newStub(t)
		// GIT_CEILING_DIRECTORIES keeps git from finding a repository above the directory.
		got := runReporter(t, binary, dir, append(stub.env("store"), "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir)), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
		if got.code != 2 || got.stdout != "" || strings.Count(got.stderr, "\n") != 1 || !strings.HasPrefix(got.stderr, "ci-report: git rev-parse HEAD: ") {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no line on stdout and one line naming git rev-parse HEAD", got.code, got.stdout, got.stderr)
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	t.Run("a spec directory outside the checkout is refused, and no result is written", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		outside := copyOf(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), "--dir", outside, "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		if want := "ci-report: " + outside + ": is outside the checkout, so its files at the commit cannot be read\n"; got.code != 2 || got.stdout != "" || !strings.HasSuffix(got.stderr, want) {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no line and stderr ending %q", got.code, got.stdout, got.stderr, want)
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	t.Run("a spec directory that the commit holds as a file is refused, and no result is written", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, "")
		if err := os.Mkdir(filepath.Join(dir, ".dispatch"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, ".dispatch", "spec"), "A file where the directory goes.\n")
		git(t, dir, "add", "-A")
		git(t, dir, "commit", "-q", "-m", "a file")
		commit := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
		want := "ci-report: " + filepath.Join(dir, ".dispatch", "spec") + ": is a file at " + commit + ", not a directory\n"
		if got.code != 2 || got.stdout != "" || !strings.HasSuffix(got.stderr, want) {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no line and stderr ending %q", got.code, got.stdout, got.stderr, want)
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	t.Run("a spec directory that the commit holds as a submodule is refused in the reporter's words, and no result is written", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, "")
		// A submodule's commit is another repository's, so this one holds no object of it.
		git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+otherSHA+",.dispatch/spec")
		git(t, dir, "commit", "-q", "-m", "a submodule where the spec directory goes")
		commit := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
		want := "ci-report: " + filepath.Join(dir, ".dispatch", "spec") + ": is a submodule at " + commit + ", not a directory\n"
		if got.code != 2 || got.stdout != "" || !strings.HasSuffix(got.stderr, want) {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no line and stderr ending %q", got.code, got.stdout, got.stderr, want)
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	// The command prints {"landed_without_answer":"<code>"} and exits 0 when the write landed and
	// its answer is gone: Dispatch's KEY_COMMITTED for a repeat of a write that it stored, and the
	// client's own ANSWER_LOST for a 2xx whose body it could not read. The reporter reads each code
	// as a stored result, Dispatch's or not.
	for _, landing := range []struct{ mode, code string }{
		{"landed", contracts.IdempotencyKeyCommitted},
		{"answer-lost", answerLost},
	} {
		t.Run("a write that landed with its answer lost is a result: its line says so, with no id, and it is no failure ("+landing.code+")", func(t *testing.T) {
			t.Parallel()
			dir, commit := checkout(t, filepath.Join("testdata", "spec"))
			stub := newStub(t)
			got := runReporter(t, binary, dir, stub.env(landing.mode), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
			if got.code != 0 {
				t.Errorf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
			}
			landed := "the result was stored, and its answer was lost on the way back (" + landing.code + "), so its id is unknown"
			exits, whys := map[string]*int{}, map[string]string{}
			for _, check := range recordedChecks {
				exits[check], whys[check] = new(0), landed
			}
			expectLines(t, got.stdout, lines(exits, nil, whys))
			if calls := stub.calls(t); !slices.EqualFunc(calls, wantCalls(commit), equalCall) {
				t.Errorf("the stub got %+v\nwant %+v", calls, wantCalls(commit))
			}
			if want := "; 3 results written, 0 calls failed\n"; !strings.HasSuffix(got.stderr, want) {
				t.Errorf("stderr does not end with %q:\n%s", want, got.stderr)
			}
		})
	}

	// The command prints {"write_outcome_unknown":"<code>"} and exits 1 when the write went out and
	// no answer said whether it stored the result: a failure that names the code, and never a
	// stored result, whatever the command's exit status.
	for _, mode := range []string{"unknown", "unknown-exit-0"} {
		t.Run("a write whose outcome is unknown is a failure that names its code, and never a stored result ("+mode+")", func(t *testing.T) {
			t.Parallel()
			dir, _ := checkout(t, filepath.Join("testdata", "spec"))
			stub := newStub(t)
			got := runReporter(t, binary, dir, stub.env(mode), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
			if got.code != 1 {
				t.Errorf("exit %d, want 1; stderr:\n%s", got.code, got.stderr)
			}
			code := 1
			if mode == "unknown-exit-0" {
				code = 0
			}
			exits, whys := map[string]*int{}, map[string]string{}
			for _, check := range recordedChecks {
				exits[check], whys[check] = new(code), "the write went out and no answer said whether Dispatch stored it (ECONNRESET): a second run of the step is safe, since Dispatch keeps each result"
			}
			expectLines(t, got.stdout, lines(exits, nil, whys))
			if want := "; 0 results written, 3 calls failed\n"; !strings.HasSuffix(got.stderr, want) {
				t.Errorf("stderr does not end with %q:\n%s", want, got.stderr)
			}
		})
	}

	t.Run("a call that exits 0 and prints neither an id nor a landed write fails its check and exits 1", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("no-id"), append(slices.Clone(args), "--dispatch-cli", stub.path)...)
		if got.code != 1 {
			t.Errorf("exit %d, want 1; stderr:\n%s", got.code, got.stderr)
		}
		exits, whys := map[string]*int{}, map[string]string{}
		for _, check := range recordedChecks {
			exits[check], whys[check] = new(0), "the dispatch command exited 0 and printed no stored result with an id"
		}
		expectLines(t, got.stdout, lines(exits, nil, whys))
		if want := "; 0 results written, 3 calls failed\n"; !strings.HasSuffix(got.stderr, want) {
			t.Errorf("stderr does not end with %q:\n%s", want, got.stderr)
		}
	})

	t.Run("-h names each exit status and exits 0", func(t *testing.T) {
		t.Parallel()
		got := runReporter(t, binary, t.TempDir(), nil, "-h")
		if got.code != 0 || got.stdout != "" || !strings.HasPrefix(got.stderr, usage+"\n") {
			t.Fatalf("exit %d, stdout %q, stderr %q; want exit 0 and the usage first on stderr", got.code, got.stdout, got.stderr)
		}
		_, statuses, _ := strings.Cut(got.stderr, "\nExit status:\n")
		for _, want := range []string{
			"  0  each call stored its result",
			"  1  a call of the dispatch command failed",
			"  2  no call was made:",
		} {
			if !strings.Contains("\n"+statuses, "\n"+want) {
				t.Errorf("the help names no exit status %q under \"Exit status:\":\n%s", want, got.stderr)
			}
		}
		// A set over a bound of notes.CheckSet exits 2 with no call, as the row "a spec directory
		// with … is refused by notes.CheckSet" shows, so exit status 2 names it.
		if want := "that is over a bound of the spec set"; !strings.Contains(statuses, want) {
			t.Errorf("exit status 2 does not name a set over a bound (%q):\n%s", want, got.stderr)
		}
		// R7: a commit holds one answer, so the help never says that a later result decides it.
		if want := "At one commit, a pass and a failure of one check make its state unknown,\nand a later pass does not end the report of the failure (R7)."; !strings.Contains(got.stderr, want) || strings.Contains(got.stderr, "newest") {
			t.Errorf("the help gives a second run's results no R7 rule %q, or says that the newest counts:\n%s", want, got.stderr)
		}
	})

	t.Run("a line holds the id of the stored row and no other part of it", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		if len(stub.calls(t)) == 0 {
			t.Fatal("the stub got no call, so no row reached the command")
		}
		for name, stream := range map[string]string{"stdout": got.stdout, "stderr": got.stderr} {
			for _, text := range strings.SplitAfter(stream, "\n") {
				if strings.Contains(text, recorderSession) {
					t.Errorf("a line of %s holds the row's recorder %s: %q", name, recorderSession, text)
				}
			}
		}
		for _, l := range parseLines(t, got.stdout) {
			if slices.Contains(recordedChecks, l.Check) && l.ResultID != "row-"+l.Check {
				t.Errorf("%s: result_id %q, want the row's id %q", l.Check, l.ResultID, "row-"+l.Check)
			}
		}
	})

	t.Run("a refused exchange prints the broker's words on each recorded check and exits 1", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("refuse"), "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		if got.code != 1 {
			t.Errorf("exit %d, want 1; stderr:\n%s", got.code, got.stderr)
		}
		exits, whys := map[string]*int{}, map[string]string{}
		for _, check := range recordedChecks {
			exits[check], whys[check] = new(1), brokerRefusal
		}
		expectLines(t, got.stdout, lines(exits, nil, whys))
		if calls := stub.calls(t); !slices.EqualFunc(calls, wantCalls(commit), equalCall) {
			t.Errorf("the stub got %+v\nwant %+v", calls, wantCalls(commit))
		}
		if want := "; 0 results written, 3 calls failed\n"; !strings.HasSuffix(got.stderr, want) {
			t.Errorf("stderr does not end with %q:\n%s", want, got.stderr)
		}
	})

	t.Run("a dispatch command that cannot start fails each recorded check and exits 1", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		missing := filepath.Join(t.TempDir(), "no-dispatch")
		got := runReporter(t, binary, dir, nil, "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", missing)
		if got.code != 1 {
			t.Errorf("exit %d, want 1; stderr:\n%s", got.code, got.stderr)
		}
		for _, l := range parseLines(t, got.stdout) {
			if !slices.Contains(recordedChecks, l.Check) {
				continue
			}
			if l.Exit == nil || *l.Exit != -1 || l.ResultID != "" || !strings.Contains(l.Why, missing) {
				t.Errorf("%s: exit %v, result_id %q, why %q; want exit -1, no id and a why naming %s", l.Check, l.Exit, l.ResultID, l.Why, missing)
			}
		}
	})

	t.Run("a refused tree writes no result, names each problem of Parse and of Validate in specgraph.Judge's order, and exits 2", func(t *testing.T) {
		t.Parallel()
		spec := copyOf(t, filepath.Join("testdata", "spec"))
		writeFile(t, filepath.Join(spec, "lonely.md"), ":::requirement{#a-lonely-statement kind=\"behavior\"}\nThe daemon keeps one claim.\n:::\n")
		writeFile(t, filepath.Join(spec, "loose.md"), "A line outside any block.\n")
		dir, _ := checkout(t, spec)
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		if got.code != 2 || got.stdout != "" {
			t.Errorf("exit %d, stdout %q; want exit 2 and no line", got.code, got.stdout)
		}
		specDir := filepath.Join(dir, ".dispatch", "spec")
		problem := "ci-report: " + filepath.Join(specDir, "lonely.md") + ":1: a-lonely-statement: leaf-without-check: no statement is under it and no check serves it\n"
		if !strings.Contains(got.stderr, problem) {
			t.Errorf("stderr holds no line %q:\n%s", problem, got.stderr)
		}
		// Dispatch reads the files with specgraph.Judge too, so the lines are its problems in its
		// order, the first first. The files show the case: Validate's problem is first in that
		// order, and Parse's, which Parse finds first, is after it.
		_, problems := specgraph.Judge(filesOf(t, spec), specgraph.Options{})
		var want, rules []string
		for _, p := range problems {
			want = append(want, fmt.Sprintf("ci-report: %s:%d: %s: %s", filepath.Join(specDir, p.Pos.File), p.Pos.Line, p.Node, p.Detail))
			rules = append(rules, p.Rule)
		}
		if !slices.Equal(rules, []string{"leaf-without-check", "text-outside-block"}) {
			t.Fatalf("Judge finds %q in the files, want the problem of Validate, then the one of Parse", rules)
		}
		var printed []string
		for _, text := range strings.Split(got.stderr, "\n") {
			if strings.HasPrefix(text, "ci-report: "+specDir+string(filepath.Separator)) {
				printed = append(printed, text)
			}
		}
		if !slices.Equal(printed, want) {
			t.Errorf("the problems on stderr are\n%s\nwant\n%s", strings.Join(printed, "\n"), strings.Join(want, "\n"))
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	// The set's bounds are notes.CheckSet's, which every reader of spec files applies from its
	// listing before it reads a file: more than notes.MaxFiles files, or more than
	// notes.MaxSetSize bytes in sum, refuses the tree with the shared reader's words, and no file is
	// read. A git of the test's own, first on PATH, logs each git command the reporter runs.
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name, want string
		files      int
		size       int
	}{
		{"more files than the set holds", fmt.Sprintf("holds %d files (limit %d)", notes.MaxFiles+1, notes.MaxFiles), notes.MaxFiles + 1, 1},
		{"more bytes in sum than the set holds", fmt.Sprintf("holds %d bytes in its files (limit %d)", (notes.MaxSetSize/notes.MaxFileSize+1)*notes.MaxFileSize, notes.MaxSetSize), notes.MaxSetSize/notes.MaxFileSize + 1, notes.MaxFileSize},
	} {
		t.Run("a spec directory with "+row.name+" is refused by notes.CheckSet, and no result is written", func(t *testing.T) {
			t.Parallel()
			dir, _ := checkout(t, "")
			// One blob, listed once for each file: the listing holds every size, and git stores one.
			blob := filepath.Join(t.TempDir(), "blob")
			writeFile(t, blob, strings.Repeat("x", row.size))
			sha := strings.TrimSpace(git(t, dir, "hash-object", "-w", blob))
			var index strings.Builder
			for i := range row.files {
				fmt.Fprintf(&index, "100644 %s\t.dispatch/spec/f%04d.md\n", sha, i)
			}
			cmd := exec.Command("git", "update-index", "--index-info")
			cmd.Dir, cmd.Stdin = dir, strings.NewReader(index.String())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git update-index: %v: %s", err, out)
			}
			git(t, dir, "commit", "-q", "-m", "a large set")
			stub := newStub(t)
			bin, gitLog := t.TempDir(), filepath.Join(t.TempDir(), "git.log")
			writeFile(t, filepath.Join(bin, "git"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+gitLog+"'\nexec '"+realGit+"' \"$@\"\n")
			if err := os.Chmod(filepath.Join(bin, "git"), 0o755); err != nil {
				t.Fatal(err)
			}
			env := append(stub.env("store"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			got := runReporter(t, binary, dir, env, append(slices.Clone(args), "--dispatch-cli", stub.path)...)
			specDir := filepath.Join(dir, ".dispatch", "spec")
			want := "ci-report: " + notes.ErrSetTooLarge.Error() + ": " + specDir + " " + row.want + "\nci-report: " + specDir + ": the tree is refused, so no result is written\n"
			if got.code != 2 || got.stdout != "" || !strings.HasSuffix(got.stderr, want) {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no line and stderr ending %q", got.code, got.stdout, got.stderr, want)
			}
			if calls := stub.calls(t); len(calls) != 0 {
				t.Errorf("the stub got %d calls, want none", len(calls))
			}
			commands, err := os.ReadFile(gitLog)
			if err != nil || !strings.Contains(string(commands), "ls-tree") {
				t.Fatalf("the git of the test logged no ls-tree, so it saw none of the reporter's commands: %v\n%s", err, commands)
			}
			if strings.Contains(string(commands), "cat-file --batch") {
				t.Errorf("the reporter read the files of a set over a bound: its git commands are\n%s", commands)
			}
		})
	}

	t.Run("a spec file over the size bound refuses the tree before specgraph.Judge reads it, and no result is written", func(t *testing.T) {
		t.Parallel()
		spec := copyOf(t, filepath.Join("testdata", "spec"))
		writeFile(t, filepath.Join(spec, "big.md"), strings.Repeat("\n", notes.MaxFileSize+1))
		dir, _ := checkout(t, spec)
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		if got.code != 2 || got.stdout != "" {
			t.Errorf("exit %d, stdout %q; want exit 2 and no line", got.code, got.stdout)
		}
		specDir := filepath.Join(dir, ".dispatch", "spec")
		for _, want := range []string{
			fmt.Sprintf("ci-report: file too large: %s is %d bytes (limit %d)\n", filepath.Join(specDir, "big.md"), notes.MaxFileSize+1, notes.MaxFileSize),
			"ci-report: " + specDir + ": the tree is refused, so no result is written\n",
		} {
			if !strings.Contains(got.stderr, want) {
				t.Errorf("stderr holds no line %q:\n%s", want, got.stderr)
			}
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	t.Run("a report that holds no event writes no result and exits 2", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		broken := filepath.Join(t.TempDir(), "report.jsonl")
		writeFile(t, broken, "ok  \texample.com/acme/probe/claims\t0.01s\n")
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), "--dir", ".dispatch/spec", "--job", job, "--report", broken, "--project", "ACME", "--dispatch-cli", stub.path)
		if got.code != 2 || got.stdout != "" {
			t.Errorf("exit %d, stdout %q; want exit 2 and no line", got.code, got.stdout)
		}
		if !strings.Contains(got.stderr, "ci-report: "+broken+": specgraph: the go test report holds no event at byte") {
			t.Errorf("stderr names no unreadable report:\n%s", got.stderr)
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	t.Run("a checkout with no spec directory has no check, and says so", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, "")
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", stub.path)
		spec := filepath.Join(dir, ".dispatch", "spec")
		if got.code != 0 || got.stdout != "" {
			t.Errorf("exit %d, stdout %q; want exit 0 and no line", got.code, got.stdout)
		}
		for _, want := range []string{
			"ci-report: " + spec + ": does not exist at " + commit + ", so it is read as the empty tree\n",
			fmt.Sprintf("ci-report: 0 checks of %s at %s name job %s; 0 results written, 0 calls failed\n", spec, commit, job),
		} {
			if !strings.Contains(got.stderr, want) {
				t.Errorf("stderr holds no line %q:\n%s", want, got.stderr)
			}
		}
		if calls := stub.calls(t); len(calls) != 0 {
			t.Errorf("the stub got %d calls, want none", len(calls))
		}
	})

	// A job's step passes each flag from a variable (.github/actions/dispatch-results), so an empty
	// variable gives the flag with an empty value: refused as a flag left out is, before any call,
	// and never sent on as an empty --project.
	t.Run("each required flag left out or given empty exits 2 with the usage", func(t *testing.T) {
		t.Parallel()
		all := []string{"--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME"}
		for i := 0; i < len(all); i += 2 {
			for form, args := range map[string][]string{
				"without": slices.Concat(all[:i], all[i+2:]),
				"empty":   slices.Concat(all[:i+1], []string{""}, all[i+2:]),
			} {
				got := runReporter(t, binary, t.TempDir(), nil, args...)
				want := "ci-report: " + all[i] + " is required\n" + usage + "\n"
				if got.code != 2 || got.stdout != "" || got.stderr != want {
					t.Errorf("%s %s: exit %d, stdout %q, stderr %q; want exit 2 and stderr %q", form, all[i], got.code, got.stdout, got.stderr, want)
				}
			}
		}
	})

	t.Run("a dispatch command of no word exits 2 with the usage", func(t *testing.T) {
		t.Parallel()
		got := runReporter(t, binary, t.TempDir(), nil, "--dir", ".dispatch/spec", "--job", job, "--report", report, "--project", "ACME", "--dispatch-cli", " ")
		if want := "ci-report: --dispatch-cli names no command\n" + usage + "\n"; got.code != 2 || got.stdout != "" || got.stderr != want {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2 and stderr %q", got.code, got.stdout, got.stderr, want)
		}
	})
}

// result is what one run of the binary did.
type result struct {
	code           int
	stdout, stderr string
}

// runReporter runs binary with args in dir, with this process's environment less any
// AGENT_SECRETS_MACHINE_IDENTITY, which the action sets and the command must not, GITHUB_SHA set
// to otherSHA, and env added.
func runReporter(t *testing.T, binary, dir string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = dir
	cmd.Env = slices.DeleteFunc(os.Environ(), func(variable string) bool {
		return strings.HasPrefix(variable, "AGENT_SECRETS_MACHINE_IDENTITY=")
	})
	cmd.Env = append(append(cmd.Env, "GITHUB_SHA="+otherSHA), env...)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitError *exec.ExitError
	if err != nil && !errors.As(err, &exitError) {
		t.Fatal(err)
	}
	return result{code: cmd.ProcessState.ExitCode(), stdout: stdout.String(), stderr: stderr.String()}
}

// stub is one copy of stubDispatch, with a directory of its own for its calls.
type stub struct {
	path, dir string
}

// newStub writes stubDispatch into a new directory, beside an empty directory for its calls.
func newStub(t *testing.T) stub {
	t.Helper()
	root := t.TempDir()
	s := stub{path: filepath.Join(root, "dispatch"), dir: filepath.Join(root, "calls")}
	if err := os.Mkdir(s.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path, []byte(stubDispatch), 0o755); err != nil {
		t.Fatal(err)
	}
	return s
}

// env is the environment that points ci-report's calls of the stub at its directory, in mode.
func (s stub) env(mode string) []string {
	return []string{"CI_REPORT_STUB_DIR=" + s.dir, "CI_REPORT_STUB_MODE=" + mode}
}

// calls is each call the stub got, in order.
func (s stub) calls(t *testing.T) []call {
	t.Helper()
	var calls []call
	for n := 0; ; n++ {
		args, err := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("%d.args", n)))
		if errors.Is(err, os.ErrNotExist) {
			return calls
		} else if err != nil {
			t.Fatal(err)
		}
		stdin, err := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("%d.stdin", n)))
		if err != nil {
			t.Fatal(err)
		}
		identity, err := os.ReadFile(filepath.Join(s.dir, fmt.Sprintf("%d.identity", n)))
		if err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call{args: strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00"), stdin: string(stdin), identity: string(identity)})
	}
}

// equalCall reports whether two calls are the same.
func equalCall(a, b call) bool {
	return slices.Equal(a.args, b.args) && a.stdin == b.stdin && a.identity == b.identity
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
	got := parseLines(t, stdout)
	if !slices.EqualFunc(got, want, func(a, b line) bool {
		return a.Check == b.Check && a.Test == b.Test && a.Outcome == b.Outcome && a.ResultID == b.ResultID && a.Why == b.Why &&
			(a.Exit == nil) == (b.Exit == nil) && (a.Exit == nil || *a.Exit == *b.Exit)
	}) {
		t.Errorf("stdout:\n%s\nwant the lines of %s", stdout, describe(want))
	}
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

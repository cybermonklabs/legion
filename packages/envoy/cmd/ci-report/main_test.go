package main

import (
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
	"github.com/sjawhar/envoy/testexec"
)

func TestMain(m *testing.M) { os.Exit(cmdtest.Main(m)) }

// quickReadCheck is a spec file's one check of the job go-checks whose test ran and passed in
// testdata/report.jsonl: a file of it that the reporter reads gives one more line and one more
// call than probe.stored holds.
const quickReadCheck = `---
type: check
id: a-read-of-one-claim-ends-in-time-here
serves: a-read-of-the-claims-ends-in-time
subject: code
procedure: program
test:
  runner: go
  selector: example.com/acme/probe/slow TestTheQuickReadEnds
  job: go-checks
claim: A read of one claim ends in 1 second or less.
procedure_text:
  - The program writes one claim and reads it.
  - The read must end in 1 second or less.
failing_example:
  - A read that waits for a lock that nobody holds.
`

// TestEachTestThatRanWritesOneResult runs ci-report as a process over a checkout of testdata/spec
// and the report testdata/report.jsonl, with the stub as its dispatch command, storing each result.
func TestEachTestThatRanWritesOneResult(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "ci-report")
	p := newProbe(t)

	t.Run("each test that ran writes one result, at the commit git names, and no other check is called", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
		if got.code != 0 {
			t.Fatalf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
		}
		expectLines(t, got.stdout, p.stored())
		expectCalls(t, stub, p.wantCalls(t, commit))
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
			if err := os.Remove(filepath.Join(spec, "probe.yaml")); err != nil {
				t.Fatal(err)
			}
		}},
		{"a check's test that the working tree changed after the commit is read as the commit names it", func(t *testing.T, spec string) {
			// The commit's check names the test that failed; the working tree's names one that passed.
			replaceIn(t, filepath.Join(spec, "probe.yaml"), "selector: example.com/acme/probe/claims TestTheOwnerStaysAfterARestart\n",
				"selector: example.com/acme/probe/claims TestTheClaimStaysAfterARestart\n")
		}},
		{"a check's job that the working tree changed after the commit is read as the commit names it", func(t *testing.T, spec string) {
			// The commit gives the check whose test failed to go-checks; the working tree gives it
			// to lint, which would take its line and its call away.
			replaceIn(t, filepath.Join(spec, "probe.yaml"), "selector: example.com/acme/probe/claims TestTheOwnerStaysAfterARestart\n  job: go-checks\n",
				"selector: example.com/acme/probe/claims TestTheOwnerStaysAfterARestart\n  job: lint\n")
		}},
		{"a spec file that the working tree added and the commit does not hold is not read", func(t *testing.T, spec string) {
			// A check of the job whose test ran and passed: read, it would get a line and a call.
			writeFile(t, filepath.Join(spec, "added.yaml"), quickReadCheck)
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir, commit := checkout(t, filepath.Join("testdata", "spec"))
			row.change(t, filepath.Join(dir, ".dispatch", "spec"))
			stub := newStub(t)
			got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
			if got.code != 0 {
				t.Fatalf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
			}
			expectLines(t, got.stdout, p.stored())
			expectCalls(t, stub, p.wantCalls(t, commit))
		})
	}

	// A spec file is a regular file directly in the directory whose name ends in .yaml
	// (specgraph.IsFile), the rule Dispatch's results route reads a commit's spec directory by. A
	// spec file of the old markdown form and a node written to a file ending in .yml are no spec
	// files: each holds a check of the job whose test ran and passed, so either one read would add
	// a line and a call.
	t.Run("each entry of the directory at the commit that is not a spec file is a warning, a submodule included", func(t *testing.T) {
		t.Parallel()
		spec := copyOf(t, filepath.Join("testdata", "spec"))
		writeFile(t, filepath.Join(spec, "notes.txt"), "Not a spec file.\n")
		writeFile(t, filepath.Join(spec, "added.md"), `:::check{#a-read-of-one-claim-ends-in-time-here serves="a-read-of-the-claims-ends-in-time" subject="code" procedure="program" runner="go" selector="example.com/acme/probe/slow TestTheQuickReadEnds" job="go-checks"}
Claim: A read of one claim ends in 1 second or less.
Procedure: The program writes one claim and reads it.
The read must end in 1 second or less.
Failing example: A read that waits for a lock that nobody holds.
:::
`)
		writeFile(t, filepath.Join(spec, "added.yml"), quickReadCheck)
		if err := os.Mkdir(filepath.Join(spec, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(spec, "sub", "inner.yaml"), quickReadCheck)
		if err := os.Symlink("probe.yaml", filepath.Join(spec, "link.yaml")); err != nil {
			t.Fatal(err)
		}
		dir, first := checkout(t, spec)
		// A submodule exists only in the commit: git keeps it as a commit entry of the tree.
		git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+first+",.dispatch/spec/module")
		git(t, dir, "commit", "-q", "-m", "a submodule")
		commit := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
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
		var want []string
		for _, entry := range []string{"added.md (file)", "added.yml (file)", "link.yaml (symbolic link)", "module (submodule)", "notes.txt (file)", "sub (directory)"} {
			name, kind, _ := strings.Cut(entry, " ")
			want = append(want, "ci-report: warning: "+filepath.Join(specDir, name)+": not a spec file "+kind+"; the importer passes over it")
		}
		if !slices.Equal(warnings, want) {
			t.Errorf("the warnings are\n%s\nwant\n%s", strings.Join(warnings, "\n"), strings.Join(want, "\n"))
		}
		expectLines(t, got.stdout, p.stored())
		expectCalls(t, stub, p.wantCalls(t, commit))
	})

	t.Run("the spec directory is read from the working directory, a directory of the checkout below its root", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		args := p.args(stub.path)
		args[slices.Index(args, ".dispatch/spec")] = "spec"
		got := runReporter(t, binary, filepath.Join(dir, ".dispatch"), stub.env("store"), args...)
		if got.code != 0 {
			t.Fatalf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
		}
		expectLines(t, got.stdout, p.stored())
		expectCalls(t, stub, p.wantCalls(t, commit))
	})

	t.Run("a line holds the id of the stored row and no other part of it", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
		expectCalls(t, stub, p.wantCalls(t, commit))
		for name, stream := range map[string]string{"stdout": got.stdout, "stderr": got.stderr} {
			for _, text := range strings.SplitAfter(stream, "\n") {
				if strings.Contains(text, recorderSession) {
					t.Errorf("a line of %s holds the row's recorder %s: %q", name, recorderSession, text)
				}
			}
		}
		expectLines(t, got.stdout, p.stored())
	})

	// testdata/cached.jsonl is the second of two runs of ./slow/'s TestTheQuickReadEnds, which go
	// test replayed from its test cache: the test did not run at the commit, so its check, the one
	// of web-checks, gets no call, and its line says why.
	t.Run("a test of a package that go test replayed from its cache gets no call, and its line says so", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		cached, err := filepath.Abs(filepath.Join("testdata", "cached.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"),
			"--dir", ".dispatch/spec", "--job", "web-checks", "--report", cached, "--project", "ACME", "--dispatch-cli", stub.path)
		if got.code != 0 {
			t.Errorf("exit %d, want 0; stderr:\n%s", got.code, got.stderr)
		}
		expectLines(t, got.stdout, []line{{
			Check: "a-read-of-one-claim-ends-in-time", Test: "example.com/acme/probe/slow TestTheQuickReadEnds", Outcome: "cached",
			Why: "go test replayed the test's package from its test cache: the test did not run at this commit",
		}})
		expectCalls(t, stub, nil)
	})
}

// TestEachAnswerOfTheDispatchCommand runs ci-report over the probe with the stub in each of its
// modes but store: each row holds the lines, the exit status, the summary and every call, so a
// reporter that calls a check twice, or skips one, fails every row.
func TestEachAnswerOfTheDispatchCommand(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "ci-report")
	p := newProbe(t)
	landed := func(code string) string {
		return "the result was stored, and its answer was lost on the way back (" + code + "), so its id is unknown"
	}
	const unknownWhy = "the write went out and no answer said whether Dispatch stored it (ECONNRESET): a second run of the step is safe, since Dispatch keeps each result"
	grantEndedWhy := func(printed string) string { return grantEnded + " The dispatch command printed: " + printed }
	for _, row := range []struct {
		name, mode string
		// code is the reporter's exit status, and counted the end of its last line.
		code    int
		lines   []line
		counted string
		// grantEnds is how many printed lines name the end of the result grant: their why starts
		// with "GRANT_NOT_LIVE: ".
		grantEnds int
	}{
		// The command prints {"landed_without_answer":"<code>"} and exits 0 when the write landed
		// and its answer is gone: Dispatch's KEY_COMMITTED for a repeat of a write that it stored,
		// and the client's own ANSWER_LOST for a 2xx whose body it could not read. The reporter
		// reads each code as a stored result, Dispatch's or not.
		{"a write that landed with its answer lost is a result: its line says so, with no id, and it is no failure (KEY_COMMITTED)",
			"landed", 0, p.answered(0, landed(contracts.IdempotencyKeyCommitted)), "3 results written, 0 calls failed", 0},
		{"a write that landed with its answer lost is a result: its line says so, with no id, and it is no failure (ANSWER_LOST)",
			"answer-lost", 0, p.answered(0, landed(answerLost)), "3 results written, 0 calls failed", 0},
		// The command prints {"write_outcome_unknown":"<code>"} and exits 1 when the write went
		// out and no answer said whether it stored the result: a failure that names the code, and
		// never a stored result, whatever the command's exit status.
		{"a write whose outcome is unknown is a failure that names its code, and never a stored result",
			"unknown", 1, p.answered(1, unknownWhy), "0 results written, 3 calls failed", 0},
		{"a write whose outcome is unknown is a failure that names its code, and never a stored result, when the command exits 0",
			"unknown-exit-0", 1, p.answered(0, unknownWhy), "0 results written, 3 calls failed", 0},
		{"a call that exits 0 and prints neither an id nor a landed write fails its check and exits 1",
			"no-id", 1, p.answered(0, "the dispatch command exited 0 and printed no stored result with an id"), "0 results written, 3 calls failed", 0},
		{"a write the airlock refused prints its words on each recorded check and exits 1",
			"refuse", 1, p.answered(1, noPlaceholderLine), "0 results written, 3 calls failed", 0},
		// The result grant lasts at most 15 minutes from the step's start, and the airlock refuses
		// each write after its end with its code GRANT_NOT_LIVE, 407 at the CONNECT and inside an
		// open tunnel, which the command prints as the code of its airlock_refused line. The line of
		// such a call names the grant's end and the code, and so does the line of a write whose
		// outcome GRANT_NOT_LIVE left unknown. The reporter reads the code and not the status: the
		// airlock's 503 at the CONNECT is a grant it could not read just then, and its 407 with
		// another code is no end of the grant, and the line of each holds the command's words
		// alone. A write whose outcome a refused tunnel left unknown carries no status, so its line
		// names both and not the grant's end alone.
		{"a write refused 407 at the CONNECT names the grant's end and GRANT_NOT_LIVE on its line",
			"grant-ended", 1, p.answered(1, grantEndedWhy(tunnelRefused407Line)), "0 results written, 3 calls failed", 3},
		{"a write the airlock refused GRANT_NOT_LIVE inside an open tunnel names the grant's end on its line",
			"grant-refused", 1, p.answered(1, grantEndedWhy(grantRefusedLine)), "0 results written, 3 calls failed", 3},
		{"a write whose outcome GRANT_NOT_LIVE left unknown names the grant's end on its line",
			"grant-ended-unknown", 1, p.answered(1, grantEndedWhileOut), "0 results written, 3 calls failed", 3},
		{"a write whose outcome a refused tunnel left unknown names the refusal and both of its statuses, and not the grant's end alone",
			"tunnel-unknown", 1, p.answered(1, tunnelRefusedWhileOut), "0 results written, 3 calls failed", 0},
		{"a 503 of the tunnel is no end of the grant, and its line holds the command's words alone",
			"tunnel-503", 1, p.answered(1, tunnelRefused503Line), "0 results written, 3 calls failed", 0},
		{"a 407 of the tunnel whose code is not GRANT_NOT_LIVE is no end of the grant, and its line holds the command's words alone",
			"tunnel-407-other", 1, p.answered(1, tunnelRefused407Line), "0 results written, 3 calls failed", 0},
		{"a grant that ends after the first call keeps that call's result and names the end on each later line",
			"ends-after-first", 1, p.endsAfterFirst(), "1 results written, 2 calls failed", 2},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir, commit := checkout(t, filepath.Join("testdata", "spec"))
			stub := newStub(t)
			got := runReporter(t, binary, dir, stub.env(row.mode), p.args(stub.path)...)
			if got.code != row.code {
				t.Errorf("exit %d, want %d; stderr:\n%s", got.code, row.code, got.stderr)
			}
			expectLines(t, got.stdout, row.lines)
			expectCalls(t, stub, p.wantCalls(t, commit))
			if want := "; " + row.counted + "\n"; !strings.HasSuffix(got.stderr, want) {
				t.Errorf("stderr does not end with %q:\n%s", want, got.stderr)
			}
			ends := 0
			for _, l := range parseLines(t, got.stdout) {
				if strings.HasPrefix(l.Why, grantNotLive+": ") {
					ends++
				}
			}
			if ends != row.grantEnds {
				t.Errorf("%d printed lines name the grant's end with GRANT_NOT_LIVE, want %d:\n%s", ends, row.grantEnds, got.stdout)
			}
		})
	}

	t.Run("a dispatch command that cannot start fails each recorded check and exits 1", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		missing := filepath.Join(t.TempDir(), "no-dispatch")
		got := runReporter(t, binary, dir, nil, p.args(missing)...)
		if got.code != 1 {
			t.Errorf("exit %d, want 1; stderr:\n%s", got.code, got.stderr)
		}
		// Each recorded check's why is the error of the start, which names the command: read, it
		// is set to one text, so the lines compare whole.
		lines := parseLines(t, got.stdout)
		const named = "an error that names the command"
		for i, l := range lines {
			if slices.Contains(recordedChecks, l.Check) && strings.Contains(l.Why, missing) {
				lines[i].Why = named
			}
		}
		if want := p.answered(-1, named); !slices.EqualFunc(lines, want, equalLine) {
			t.Errorf("stdout:\n%s\nwant the lines of %s, each recorded check's why naming %s", got.stdout, describe(want), missing)
		}
		if want := "; 0 results written, 3 calls failed\n"; !strings.HasSuffix(got.stderr, want) {
			t.Errorf("stderr does not end with %q:\n%s", want, got.stderr)
		}
	})
}

// TestAnInputItCannotReadWritesNoResult holds ci-report to exit 2 with no call, and no line, for
// a checkout, a spec directory, a tree or a report it cannot read.
func TestAnInputItCannotReadWritesNoResult(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "ci-report")
	p := newProbe(t)
	// refused fails the test unless got is exit 2, with no line, and stderr ends with want.
	refused := func(t *testing.T, got result, stub stub, want string) {
		t.Helper()
		if got.code != 2 || got.stdout != "" || !strings.HasSuffix(got.stderr, want) {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no line and stderr ending %q", got.code, got.stdout, got.stderr, want)
		}
		expectCalls(t, stub, nil)
	}

	t.Run("outside a git checkout the step refuses with one line and makes no call", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.CopyFS(filepath.Join(dir, ".dispatch", "spec"), os.DirFS(filepath.Join("testdata", "spec"))); err != nil {
			t.Fatal(err)
		}
		stub := newStub(t)
		// GIT_CEILING_DIRECTORIES keeps git from finding a repository above the directory.
		got := runReporter(t, binary, dir, append(stub.env("store"), "GIT_CEILING_DIRECTORIES="+filepath.Dir(dir)), p.args(stub.path)...)
		if got.code != 2 || got.stdout != "" || strings.Count(got.stderr, "\n") != 1 || !strings.HasPrefix(got.stderr, "ci-report: git rev-parse HEAD: ") {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2, no line on stdout and one line naming git rev-parse HEAD", got.code, got.stdout, got.stderr)
		}
		expectCalls(t, stub, nil)
	})

	t.Run("a spec directory outside the checkout is refused, and no result is written", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		outside := copyOf(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		args := p.args(stub.path)
		args[slices.Index(args, ".dispatch/spec")] = outside
		got := runReporter(t, binary, dir, stub.env("store"), args...)
		refused(t, got, stub, "ci-report: "+outside+": is outside the checkout, so its files at the commit cannot be read\n")
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
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
		refused(t, got, stub, "ci-report: "+filepath.Join(dir, ".dispatch", "spec")+": is a file at "+commit+", not a directory\n")
	})

	t.Run("a spec directory that the commit holds as a submodule is refused in the reporter's words, and no result is written", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, "")
		// A submodule's commit is another repository's, so this one holds no object of it.
		git(t, dir, "update-index", "--add", "--cacheinfo", "160000,"+otherSHA+",.dispatch/spec")
		git(t, dir, "commit", "-q", "-m", "a submodule where the spec directory goes")
		commit := strings.TrimSpace(git(t, dir, "rev-parse", "HEAD"))
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
		refused(t, got, stub, "ci-report: "+filepath.Join(dir, ".dispatch", "spec")+": is a submodule at "+commit+", not a directory\n")
	})

	t.Run("a refused tree writes no result, names each problem of Parse and of Validate in specgraph.Judge's order, and exits 2", func(t *testing.T) {
		t.Parallel()
		spec := copyOf(t, filepath.Join("testdata", "spec"))
		writeFile(t, filepath.Join(spec, "lonely.yaml"), "---\ntype: requirement\nid: a-lonely-statement\nkind: behavior\ntext:\n  - The daemon keeps one claim.\n")
		writeFile(t, filepath.Join(spec, "loose.yaml"), "A line outside any document.\n")
		dir, _ := checkout(t, spec)
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
		specDir := filepath.Join(dir, ".dispatch", "spec")
		refused(t, got, stub, "ci-report: "+specDir+": the tree is refused, so no result is written\n")
		problem := "ci-report: " + filepath.Join(specDir, "lonely.yaml") + ":1: a-lonely-statement: leaf-without-check: no statement is under it and no check serves it\n"
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
		if !slices.Equal(rules, []string{"leaf-without-check", "schema"}) {
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
				fmt.Fprintf(&index, "100644 %s\t.dispatch/spec/f%04d.yaml\n", sha, i)
			}
			cmd := exec.Command("git", "update-index", "--index-info")
			cmd.Dir, cmd.Stdin = dir, strings.NewReader(index.String())
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("git update-index: %v: %s", err, out)
			}
			git(t, dir, "commit", "-q", "-m", "a large set")
			stub := newStub(t)
			bin, gitLog := t.TempDir(), filepath.Join(t.TempDir(), "git.log")
			fakeGit := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '" + gitLog + "'\nexec '" + realGit + "' \"$@\"\n"
			if err := testexec.WriteFile(filepath.Join(bin, "git"), []byte(fakeGit), 0o755); err != nil {
				t.Fatal(err)
			}
			env := append(stub.env("store"), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			got := runReporter(t, binary, dir, env, p.args(stub.path)...)
			specDir := filepath.Join(dir, ".dispatch", "spec")
			refused(t, got, stub, "ci-report: "+notes.ErrSetTooLarge.Error()+": "+specDir+" "+row.want+"\nci-report: "+specDir+": the tree is refused, so no result is written\n")
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
		writeFile(t, filepath.Join(spec, "big.yaml"), strings.Repeat("\n", notes.MaxFileSize+1))
		dir, _ := checkout(t, spec)
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
		specDir := filepath.Join(dir, ".dispatch", "spec")
		refused(t, got, stub, fmt.Sprintf("ci-report: file too large: %s is %d bytes (limit %d)\n", filepath.Join(specDir, "big.yaml"), notes.MaxFileSize+1, notes.MaxFileSize)+
			"ci-report: "+specDir+": the tree is refused, so no result is written\n")
	})

	t.Run("a report that holds no event writes no result and exits 2", func(t *testing.T) {
		t.Parallel()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		broken := filepath.Join(t.TempDir(), "report.jsonl")
		writeFile(t, broken, "ok  \texample.com/acme/probe/claims\t0.01s\n")
		stub := newStub(t)
		args := p.args(stub.path)
		args[slices.Index(args, p.report)] = broken
		got := runReporter(t, binary, dir, stub.env("store"), args...)
		refused(t, got, stub, "")
		if !strings.Contains(got.stderr, "ci-report: "+broken+": specgraph: the go test report holds no event at byte") {
			t.Errorf("stderr names no unreadable report:\n%s", got.stderr)
		}
	})

	t.Run("a checkout with no spec directory has no check, and says so", func(t *testing.T) {
		t.Parallel()
		dir, commit := checkout(t, "")
		stub := newStub(t)
		got := runReporter(t, binary, dir, stub.env("store"), p.args(stub.path)...)
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
		expectCalls(t, stub, nil)
	})
}

// TestTheArguments holds ci-report's flags: -h names each exit status, and a flag it cannot use
// exits 2 with the usage before any call.
func TestTheArguments(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "ci-report")
	p := newProbe(t)

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

	// A job's step passes each flag from a variable (.github/actions/dispatch-results), so an empty
	// variable gives the flag with an empty value: refused as a flag left out is, before any call,
	// and never sent on as an empty --project.
	t.Run("each required flag left out or given empty exits 2 with the usage", func(t *testing.T) {
		t.Parallel()
		all := p.args("dispatch")[:8]
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
		got := runReporter(t, binary, t.TempDir(), nil, p.args(" ")...)
		if want := "ci-report: --dispatch-cli names no command\n" + usage + "\n"; got.code != 2 || got.stdout != "" || got.stderr != want {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 2 and stderr %q", got.code, got.stdout, got.stderr, want)
		}
	})
}

// replaceIn replaces the one from in the file at path with to, and fails the test when the file
// does not hold it, since the row would then not show its case.
func replaceIn(t *testing.T, path, from, to string) {
	t.Helper()
	text, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(text), from) != 1 {
		t.Fatalf("%s does not hold %q once", path, from)
	}
	writeFile(t, path, strings.Replace(string(text), from, to, 1))
}

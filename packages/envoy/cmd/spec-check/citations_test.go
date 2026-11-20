package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
)

// The check of every citations row, whose block opens at line 5 of spec.md, and its statement.
const (
	citedCheck   = "the-answer-is-forty-two"
	citedSpecTop = ":::requirement{#the-answer-is-known kind=\"behavior\"}\nThe service answers each question with the known answer.\n:::\n\n"
	citedSpecEnd = "Claim: The service answers forty-two.\nProcedure: The program asks the service and reads the answer.\nIt must find forty-two.\nFailing example: A service that answers zero.\n:::\n"
)

// citedSpec is spec.md with the program check whose test part is attrs: runner, selector and job,
// or "" for none.
func citedSpec(attrs string) string {
	if attrs != "" {
		attrs = " " + attrs
	}
	return citedSpecTop + ":::check{#" + citedCheck + " serves=\"the-answer-is-known\" subject=\"code\" procedure=\"program\"" + attrs + "}\n" + citedSpecEnd
}

// testPart is the test part of the check: go, selector and job.
func testPart(selector, job string) string {
	return "runner=\"go\" selector=\"" + selector + "\" job=\"" + job + "\""
}

// cited is the result of a run over the spec directory dir where the one program check is cited.
func cited(dir string) result {
	return result{stdout: "spec-check: " + dir + ": 1 program check; each is cited\n"}
}

// refused is the result of a run over the spec directory dir where the one program check is
// refused with detail, which begins with its rule.
func refused(dir, detail string) result {
	return result{code: 1, stdout: "spec-check: " + dir + ": 1 program check; 1 refused\n",
		stderr: "spec-check: " + filepath.Join(dir, "spec.md") + ":5: " + citedCheck + ": " + detail + "\n"}
}

// goReport is a go test -json report of TestA of package a with outcome, pass, fail or skip, or a
// run with no end for "", as go test of Go 1.27 writes it: the run and its end each with go test's
// own line, a frame, before the end event (testdata/gotest/pass.jsonl of specgraph is one such
// report), and last the package's own end event, without which no run of the package counts. A
// report of a Go before 1.27 has no frame, and ReadGoTestJSON refuses it. "cached" is a pass that
// go test replayed from its cache, with the package's summary line that says so
// (testdata/gotest/cached/cached.jsonl of specgraph).
func goReport(outcome string) string {
	const pkg = `"Package":"example.com/citations/a","Test":"TestA"`
	report := `{"Action":"run",` + pkg + `}` + "\n" +
		`{"Action":"output",` + pkg + `,"Output":"=== RUN   TestA\n","OutputType":"frame"}` + "\n"
	end := outcome
	if outcome == "cached" {
		end = "pass"
	}
	if outcome != "" {
		report += `{"Action":"output",` + pkg + `,"Output":"--- ` + strings.ToUpper(end) + `: TestA (0.01s)\n","OutputType":"frame"}` + "\n" +
			`{"Action":"` + end + `",` + pkg + `,"Elapsed":0.01}` + "\n"
	}
	if outcome == "cached" {
		report += `{"Action":"output","Package":"example.com/citations/a","Output":"ok  \texample.com/citations/a\t(cached)\n"}` + "\n"
	}
	if end == "fail" || end == "" {
		return report + packageEnd("fail")
	}
	return report + packageEnd("pass")
}

// packageEnd is the end event of package a with action, the last event that go test writes of a
// package.
func packageEnd(action string) string {
	return `{"Action":"` + action + `","Package":"example.com/citations/a","Elapsed":0.01}` + "\n"
}

// needsOf is toJSON(needs) of a run where changes succeeded and go-checks ended with result.
func needsOf(result string) string {
	return `{"changes":{"result":"success","outputs":{}},"go-checks":{"result":"` + result + `","outputs":{}}}`
}

// citationRun is the input of one run of citations: a git repository whose base commit holds a
// copy of testdata/citations/repo with spec.md in its .dispatch/spec, and whose head commit, HEAD,
// has the same tree, and the files of the flags.
type citationRun struct {
	spec       string
	changed    string            // the paths of changed.txt, after its base line; ignored with allChanged
	allChanged bool              // --all-changed in place of --changed
	reports    map[string]string // each directory of reports/ by name, with its report.jsonl
	needs      string
	ties       string // the ties of the head, {"<check>":[…]}, or "" for no --ties
	baseTies   string // the ties of the base, as ties; "" for the head's, "-" for none of the base
	extra      []string
}

// newCitationRun is the base: the check's test is TestA of package a, run by go-checks; nothing
// changed; go-checks succeeded and its report holds TestA as a pass; the tie is a/* and
// fixtures/a.json, at the head and at the base.
func newCitationRun() citationRun {
	return citationRun{
		spec:    citedSpec(testPart("example.com/citations/a TestA", "go-checks")),
		reports: map[string]string{"spec-report--go-checks": goReport("pass")},
		needs:   needsOf("success"),
		ties:    `{"` + citedCheck + `":["a/*","fixtures/a.json"]}`,
	}
}

// citationFiles is where one run's files are, and the base and head commits of its repository.
type citationFiles struct {
	root, repo, dir, changed, reports, needs, ties, json string
	base, head                                           string
}

// write writes the run's files in a directory of the test's own and answers where they are.
func (r citationRun) write(t *testing.T) citationFiles {
	t.Helper()
	root := t.TempDir()
	f := citationFiles{root: root, repo: filepath.Join(root, "repo"), changed: filepath.Join(root, "changed.txt"),
		reports: filepath.Join(root, "reports"), needs: filepath.Join(root, "needs.json"), ties: filepath.Join(root, "ties.json"),
		json: filepath.Join(root, "out", "faults.json")}
	f.dir = filepath.Join(f.repo, ".dispatch", "spec")
	copyCitationsRepo(t, f.repo, r.spec)
	f.base = commitAll(t, f.repo, "", "base")
	f.head = commitAll(t, f.repo, f.base, "head")
	writeFile(t, f.changed, []byte("base "+f.base+"\n"+r.changed))
	if r.reports != nil {
		if err := os.Mkdir(f.reports, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, report := range r.reports {
		if err := os.Mkdir(filepath.Join(f.reports, name), 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(f.reports, name, "report.jsonl"), []byte(report))
	}
	writeFile(t, f.needs, []byte(r.needs))
	if r.ties != "" {
		ties := `{"` + f.head + `":` + r.ties
		switch r.baseTies {
		case "":
			ties += `,"` + f.base + `":` + r.ties
		case "-":
		default:
			ties += `,"` + f.base + `":` + r.baseTies
		}
		writeFile(t, f.ties, []byte(ties+"}"))
	}
	return f
}

// args is the command line of the run.
func (r citationRun) args(f citationFiles, workflow string) []string {
	args := []string{"citations", "--dir", f.dir, "--repo", f.repo, "--workflow", workflow}
	if r.allChanged {
		args = append(args, "--all-changed")
	} else {
		args = append(args, "--changed", f.changed)
	}
	args = append(args, "--reports", f.reports, "--needs", f.needs, "--json", f.json)
	if r.ties != "" {
		args = append(args, "--ties", f.ties)
	}
	return append(args, r.extra...)
}

// copyCitationsRepo copies testdata/citations/repo to repo, with spec in its
// .dispatch/spec/spec.md ("" for no spec file), and makes it a git repository with no commit.
func copyCitationsRepo(t *testing.T, repo, spec string) {
	t.Helper()
	if err := os.CopyFS(repo, os.DirFS(fixture(t, "citations", "repo"))); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(repo, ".dispatch", "spec")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if spec != "" {
		writeFile(t, filepath.Join(dir, "spec.md"), []byte(spec))
	}
	gitIn(t, repo, "init", "-q")
}

// gitEnv makes each git command, the test's own and the command under test's, read no
// configuration of this machine and commit as ACME.
var gitEnv = []string{"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
	"GIT_AUTHOR_NAME=ACME", "GIT_AUTHOR_EMAIL=ci@example.com", "GIT_COMMITTER_NAME=ACME", "GIT_COMMITTER_EMAIL=ci@example.com"}

// gitIn runs git in dir, reading no configuration of this machine, and answers its output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(cmd.Environ(), gitEnv...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %q in %s: %v\n%s%s", args, dir, err, out, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

// commitAll commits every file of dir's working tree, on parent ("" for none), with git's plumbing
// (write-tree, commit-tree), makes it HEAD and answers it: a git wrapper that checks a commit's
// text then has no say over a fixture's commit.
func commitAll(t *testing.T, dir, parent, message string) string {
	t.Helper()
	gitIn(t, dir, "add", "-A")
	args := []string{"commit-tree", gitIn(t, dir, "write-tree"), "-m", message}
	if parent != "" {
		args = append(args, "-p", parent)
	}
	commit := gitIn(t, dir, args...)
	gitIn(t, dir, "update-ref", "HEAD", commit)
	return commit
}

// TestCitationsCommand runs the built binary over each fixture of specgraph's TestCitations
// (v-citations, each run alone), through files on disk: a copy of testdata/citations/repo outside
// the root go.work, so its go commands need no GOWORK setting, a changed.txt, a reports/ tree, a
// needs.json and a ties.json through --ties. Each row asserts the exit status, both streams and the
// --json file byte for byte.
func TestCitationsCommand(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	workflow := fixture(t, "citations", "workflow.yaml")
	const notRun = "test-not-run"
	for _, row := range []struct {
		name   string
		change func(r *citationRun)
		rule   string // "" when no fault is wanted
		detail string // what follows the rule
	}{
		{name: "v-citations 1: the check names a test that someone renamed", change: func(r *citationRun) {
			r.spec = citedSpec(testPart("example.com/citations/a TestRenamed", "go-checks"))
		}, rule: "test-not-in-code", detail: "example.com/citations/a TestRenamed is not in the code at this commit"},
		{name: "v-citations 2: code of the tie changed and the job skipped the test", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.reports["spec-report--go-checks"] = goReport("skip")
		}, rule: notRun, detail: "a/a.go changed, and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
		{name: "v-citations 3: a program check names no test", change: func(r *citationRun) {
			r.spec = citedSpec("")
		}, rule: "no-test", detail: "the check names no test"},
		{name: "v-citations 5: no code of the tie changed and the job skipped the test", change: func(r *citationRun) {
			r.changed = "docs/x.md\n"
			r.reports["spec-report--go-checks"] = goReport("skip")
		}},
		{name: "v-citations 6: the selector picks two tests", change: func(r *citationRun) {
			r.spec = citedSpec(testPart("example.com/citations/b TestB", "go-checks"))
		}, rule: "two-tests", detail: "example.com/citations/b TestB picks 2 tests"},
		{name: "code of the tie changed and the job did not run", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.needs = needsOf("skipped")
			r.reports = map[string]string{}
		}, rule: notRun, detail: "a/a.go changed, and go-checks did not run example.com/citations/a TestA (the job did not run)"},
		{name: "code of the tie changed and the job ran and uploaded no report", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.reports = map[string]string{}
		}, rule: notRun, detail: "a/a.go changed, and go-checks did not run example.com/citations/a TestA (the job left no report)"},
		{name: "a missing reports directory is no report", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.reports = nil
		}, rule: notRun, detail: "a/a.go changed, and go-checks did not run example.com/citations/a TestA (the job left no report)"},
		{name: "B5: a read that the repository records for the test changed", change: func(r *citationRun) {
			r.changed = "fixtures/a.json\n"
			r.reports["spec-report--go-checks"] = goReport("skip")
		}, rule: notRun, detail: "fixtures/a.json changed, and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
		{name: "a file moves out of the tie", change: func(r *citationRun) {
			r.changed = "a/data.json\ndocs/data.json\n"
			r.reports["spec-report--go-checks"] = goReport("skip")
		}, rule: notRun, detail: "a/data.json changed, and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
		{name: "the check names a job that the citations job does not wait for", change: func(r *citationRun) {
			r.spec = citedSpec(testPart("example.com/citations/a TestA", "envoy-go"))
		}, rule: "job-not-awaited", detail: `the citations job does not wait for "envoy-go", so it cannot read its report`},
		{name: "the check names a job that the workflow does not have", change: func(r *citationRun) {
			r.spec = citedSpec(testPart("example.com/citations/a TestA", "gone-job"))
		}, rule: "job-not-in-workflow", detail: `no job "gone-job"`},
		{name: "the tie unknown, nothing changed, the test did not run", change: func(r *citationRun) {
			r.ties = "{}"
			r.reports["spec-report--go-checks"] = ""
		}, rule: notRun, detail: "its tie is unknown (--ties holds no tie of the check at commit {head}), and go-checks did not run example.com/citations/a TestA (its report does not hold it)"},
		{name: "no base commit, the test ran and passed", change: func(r *citationRun) {
			r.allChanged = true
		}},
		{name: "no base commit, the test skipped", change: func(r *citationRun) {
			r.allChanged = true
			r.reports["spec-report--go-checks"] = goReport("skip")
		}, rule: notRun, detail: "no base commit, and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
		{name: "code of the tie changed and the test could not run", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.reports["spec-report--go-checks"] = goReport("")
		}, rule: notRun, detail: "a/a.go changed, and go-checks did not run example.com/citations/a TestA (it could not run)"},
		{name: "code of the tie changed and one leg of the job skipped the test", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.reports = map[string]string{"spec-report--go-checks--amd64": goReport("pass"), "spec-report--go-checks--arm64": goReport("skip")}
		}, rule: notRun, detail: "a/a.go changed, and go-checks (leg arm64) did not run example.com/citations/a TestA (its report shows it skipped)"},
		{name: "code of the tie changed and the test ran", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
		}},
		// A pass that go test replayed from its cache did not run at this commit.
		{name: "code of the tie changed and go test replayed the test from its cache", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.reports["spec-report--go-checks"] = goReport("cached")
		}, rule: notRun, detail: "a/a.go changed, and go-checks did not run example.com/citations/a TestA (its report holds a result that go test replayed from its cache, which did not run at this commit)"},
		{name: "no code of the tie changed and go test replayed the test from its cache", change: func(r *citationRun) {
			r.changed = "docs/x.md\n"
			r.reports["spec-report--go-checks"] = goReport("cached")
		}},
		// A run counts only when the report holds the end of its package, after which go test writes
		// nothing of it: a report cut before that end can lack the line that says the run was cached.
		// Its control is the whole report, the row "code of the tie changed and the test ran".
		{name: "code of the tie changed and the report ends before its package's end", change: func(r *citationRun) {
			r.changed = "a/a.go\n"
			r.reports["spec-report--go-checks"] = strings.TrimSuffix(goReport("pass"), packageEnd("pass"))
		}, rule: notRun, detail: "a/a.go changed, and go-checks did not run example.com/citations/a TestA (it could not run)"},
		// A changed path counts when the tie at either end holds it.
		{name: "the change deleted the test's testdata directory, which only the tie at the base holds", change: func(r *citationRun) {
			r.baseTies = `{"` + citedCheck + `":["a/*","a/testdata/**","fixtures/a.json"]}`
			r.changed = "a/testdata/answer.json\n"
			r.reports["spec-report--go-checks"] = goReport("skip")
		}, rule: notRun, detail: "a/testdata/answer.json changed (the tie at the base commit holds it), and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
		{name: "--ties holds no ties of the base commit", change: func(r *citationRun) {
			r.baseTies = "-"
			r.changed = "docs/x.md\n"
			r.reports["spec-report--go-checks"] = goReport("skip")
		}, rule: notRun, detail: "its tie at the base commit is unknown (--ties holds no ties of commit {base}), and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
		// A result carries from the base only while the tie is known at both ends, also when no path
		// changed. The control: both ties known, nothing changed, and the skipped test is cited.
		{name: "--ties holds no ties of the base commit, and nothing changed", change: func(r *citationRun) {
			r.baseTies = "-"
			r.reports["spec-report--go-checks"] = goReport("skip")
		}, rule: notRun, detail: "its tie at the base commit is unknown (--ties holds no ties of commit {base}), and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
		{name: "control: --ties holds both ties, and nothing changed", change: func(r *citationRun) {
			r.reports["spec-report--go-checks"] = goReport("skip")
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newCitationRun()
			row.change(&r)
			f := r.write(t)
			got := runBinary(t, binary, "", r.args(f, workflow)...)
			want := cited(f.dir)
			wantJSON := "[]\n"
			if row.rule != "" {
				detail := row.rule + ": " + strings.NewReplacer("{head}", f.head, "{base}", f.base).Replace(row.detail)
				want = refused(f.dir, detail)
				wantJSON = `[{"check":"` + citedCheck + `","rule":"` + row.rule + `","detail":"` + strings.ReplaceAll(detail, `"`, `\"`) + `"}]` + "\n"
			}
			expect(t, got, want)
			if content, err := os.ReadFile(f.json); err != nil || string(content) != wantJSON {
				t.Errorf("--json file %q, %v; want %q", content, err, wantJSON)
			}
		})
	}

	t.Run("no program check: each of none is cited", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.spec = citedSpecTop + ":::check{#the-answer-is-in-the-records serves=\"the-answer-is-known\" subject=\"production\" procedure=\"query\"}\n" +
			"Claim: The records hold the answer.\nProcedure: The query reads the records.\nIt returns each record with no answer.\nFailing example: A copy of the records with one record with no answer.\n:::\n"
		r.changed = "a/a.go\n"
		r.reports = nil
		f := r.write(t)
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), result{stdout: "spec-check: " + f.dir + ": 0 program checks; each is cited\n"})
	})

	// Without --ties the tie of a go check is what the repository's go-test-reads.sh says its test
	// reads. The copy holds no such script, so every tie is unknown, and fixture 5, which passes with
	// a known tie, names the check: the instrument says that it could not read the tie.
	t.Run("without --ties, a repository with no go-test-reads.sh has no known tie", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.changed = "docs/x.md\n"
		r.reports["spec-report--go-checks"] = goReport("skip")
		r.ties = ""
		f := r.write(t)
		script := filepath.Join(f.repo, ".github", "scripts", "go-test-reads.sh")
		// The words after the script's path are os/exec's and the system's, so the row reads ours.
		prefix := "spec-check: " + filepath.Join(f.dir, "spec.md") + ":5: " + citedCheck + ": " + notRun + ": its tie is unknown (specgraph: " + script + ": "
		suffix := "), and go-checks did not run example.com/citations/a TestA (its report shows it skipped)\n"
		got := runBinary(t, binary, "", r.args(f, workflow)...)
		if got.code != 1 || got.stdout != "spec-check: "+f.dir+": 1 program check; 1 refused\n" ||
			!strings.HasPrefix(got.stderr, prefix) || !strings.HasSuffix(got.stderr, suffix) || strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, the count line, and one line %q…%q", got.code, got.stdout, got.stderr, prefix, suffix)
		}
	})

	t.Run("a test that the code cannot be searched for is refused, naming go's answer", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.spec = citedSpec(testPart("example.com/citations/gone TestA", "go-checks"))
		f := r.write(t)
		got := runBinary(t, binary, "", r.args(f, workflow)...)
		prefix := "spec-check: " + filepath.Join(f.dir, "spec.md") + ":5: " + citedCheck +
			": test-lookup-failed: example.com/citations/gone TestA could not be looked up in the code at this commit (specgraph: go list example.com/citations/gone in " + f.repo + ": "
		if got.code != 1 || got.stdout != "spec-check: "+f.dir+": 1 program check; 1 refused\n" || !strings.HasPrefix(got.stderr, prefix) ||
			strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, the count line, and one line starting %q", got.code, got.stdout, got.stderr, prefix)
		}
		if content, err := os.ReadFile(f.json); err != nil || !strings.HasPrefix(string(content), `[{"check":"`+citedCheck+`","rule":"test-lookup-failed","detail":"test-lookup-failed: `) {
			t.Errorf("--json file %q, %v; want the test-lookup-failed fault", content, err)
		}
	})

	t.Run("a report of a job that the citations job does not wait for is passed over with a warning", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.reports["spec-report--envoy-go"] = goReport("skip")
		f := r.write(t)
		want := cited(f.dir)
		want.stderr = "spec-check: warning: " + filepath.Join(f.reports, "spec-report--envoy-go") + ": names no job that spec-citations waits for; passed over\n"
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), want)
	})

	// The --json file is written by printJSON, the writer of the JSON of checks and state: the "[" of
	// each "##[" in a value is \u005b, which a JSON reader reads back as "[".
	t.Run("the --json file writes the [ of a ##[ as checks writes its JSON", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.spec = citedSpec(testPart("example.com/citations/a TestA", "##[error]forged"))
		f := r.write(t)
		got := runBinary(t, binary, "", r.args(f, workflow)...)
		if got.code != 1 || got.stdout != "spec-check: "+f.dir+": 1 program check; 1 refused\n" || strings.Contains(got.stderr, "##[") {
			t.Errorf("exit %d, stdout %q, stderr %q; want exit 1, the count line, and no \"##[\"", got.code, got.stdout, got.stderr)
		}
		content, err := os.ReadFile(f.json)
		want := `[{"check":"` + citedCheck + `","rule":"job-not-in-workflow","detail":"job-not-in-workflow: no job \"##\u005berror]forged\""}]` + "\n"
		if err != nil || string(content) != want {
			t.Fatalf("--json file %q, %v; want %q", content, err, want)
		}
		var faults []struct{ Check, Rule, Detail string }
		if err := json.Unmarshal(content, &faults); err != nil || len(faults) != 1 || faults[0].Detail != `job-not-in-workflow: no job "##[error]forged"` {
			t.Errorf("the --json file reads as %+v, %v; want the job as written", faults, err)
		}
	})

	t.Run("a directory with no spec file", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.spec = ""
		f := r.write(t)
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), result{code: 2, stderr: "spec-check: " + f.dir + ": holds no spec file\n"})
		r.extra = []string{"--allow-empty"}
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), result{stdout: "spec-check: " + f.dir + ": holds no spec file; no check to cite\n"})
		if content, err := os.ReadFile(f.json); err != nil || string(content) != "[]\n" {
			t.Errorf("--json file %q, %v; want []", content, err)
		}
		// Git keeps no empty directory, so a repository with no spec file has no directory.
		removeAll(t, f.dir)
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), result{stdout: "spec-check: " + f.dir + ": holds no spec file; no check to cite\n"})
		r.extra = nil
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), result{code: 2, stderr: "spec-check: " + f.dir + ": does not exist\n"})
	})

	t.Run("a spec file that does not parse exits 2 with its problem", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.spec = "A line outside a block.\n"
		f := r.write(t)
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), result{code: 2,
			stderr: "spec-check: " + filepath.Join(f.dir, "spec.md") + ":1: -: text-outside-block: a spec file holds only blocks, headings and blank lines\n"})
	})

	// An input that the command cannot read exits 2 with one line naming it, nothing on stdout, and
	// no --json file. The line starts with the words of the command (line); where a library's words
	// follow them (more), the row reads the command's alone, since a library may word its errors
	// otherwise in another version or process.
	for _, row := range []struct {
		name   string
		change func(t *testing.T, f citationFiles)
		line   func(f citationFiles) string
		more   bool
	}{
		{"needs.json that is no JSON", func(t *testing.T, f citationFiles) { writeFile(t, f.needs, []byte("x")) },
			func(f citationFiles) string { return f.needs + ": cannot be read: " }, true},
		{"needs.json with no result of an awaited job", func(t *testing.T, f citationFiles) {
			writeFile(t, f.needs, []byte(`{"changes":{"result":"success","outputs":{}}}`))
		}, func(f citationFiles) string {
			return f.needs + `: holds no result of job "go-checks", which spec-citations waits for`
		}, false},
		{"needs.json with a job that spec-citations does not wait for", func(t *testing.T, f citationFiles) {
			writeFile(t, f.needs, []byte(`{"changes":{"result":"success"},"go-checks":{"result":"success"},"envoy-go":{"result":"success"}}`))
		}, func(f citationFiles) string {
			return f.needs + `: holds job "envoy-go", which spec-citations does not wait for`
		}, false},
		{"needs.json with a job of no result", func(t *testing.T, f citationFiles) {
			writeFile(t, f.needs, []byte(`{"changes":{"result":"success"},"go-checks":{"outputs":{}}}`))
		}, func(f citationFiles) string {
			return f.needs + `: job "go-checks" has no result`
		}, false},
		{"a report that is no go test report", func(t *testing.T, f citationFiles) {
			writeFile(t, filepath.Join(f.reports, "spec-report--go-checks", "report.jsonl"), []byte("ok  \texample.com/citations/a\n"))
		}, func(f citationFiles) string {
			return filepath.Join(f.reports, "spec-report--go-checks", "report.jsonl") +
				": cannot be read: specgraph: the go test report holds no event at byte 0: "
		}, true},
		{"a report directory with no report.jsonl", func(t *testing.T, f citationFiles) {
			removeAll(t, filepath.Join(f.reports, "spec-report--go-checks", "report.jsonl"))
		}, func(f citationFiles) string {
			return filepath.Join(f.reports, "spec-report--go-checks", "report.jsonl") + ": cannot be read: "
		}, true},
		{"an entry of reports that is no report artifact", func(t *testing.T, f citationFiles) {
			writeFile(t, filepath.Join(f.reports, "notes.txt"), []byte("A note.\n"))
		}, func(f citationFiles) string {
			return filepath.Join(f.reports, "notes.txt") + ": is no directory named spec-report--<job> or spec-report--<job>--<leg>"
		}, false},
		{"a ties file with an entry in another form", func(t *testing.T, f citationFiles) {
			writeFile(t, f.ties, []byte(`{"`+f.head+`":{"`+citedCheck+`":["p/*.go"]}}`))
		}, func(f citationFiles) string {
			return f.ties + `: commit "` + f.head + `": check "` + citedCheck + `": specgraph: tie entry "p/*.go" is not a repository path, <dir>/* or <dir>/**`
		}, false},
		// A value of another type is refused in this package's words, with the form and the place.
		{"a ties file in the form by check, with no commit", func(t *testing.T, f citationFiles) {
			writeFile(t, f.ties, []byte(`{"`+citedCheck+`":["a/*"]}`))
		}, func(f citationFiles) string {
			return f.ties + `: it is not in the form {"<commit>":{"<check id>":["<path or pattern>", …]}}: the value at "/` + citedCheck + `" is of another type`
		}, false},
		{"a ties file whose tie is a string, not a list", func(t *testing.T, f citationFiles) {
			writeFile(t, f.ties, []byte(`{"`+f.head+`":{"`+citedCheck+`":"a/*"}}`))
		}, func(f citationFiles) string {
			return f.ties + `: it is not in the form {"<commit>":{"<check id>":["<path or pattern>", …]}}: the value at "/` + f.head + `/` + citedCheck + `" is of another type`
		}, false},
		{"a ties file that names a commit by a short name", func(t *testing.T, f citationFiles) {
			writeFile(t, f.ties, []byte(`{"`+f.head[:12]+`":{"`+citedCheck+`":["a/*"]}}`))
		}, func(f citationFiles) string {
			return f.ties + `: commit "` + f.head[:12] + `": not a full object name of 40 or 64 lower-case hex digits`
		}, false},
		// Of the commits of one file, the first in sorted order is judged first, so a refusal names
		// the same commit on each run, though a map holds the ten in no order.
		{"a ties file of ten commits by short names names the first in sorted order", func(t *testing.T, f citationFiles) {
			var entries []string
			for _, commit := range strings.Fields("c9 c8 c7 c6 c5 c4 c3 c2 c1 c0") {
				entries = append(entries, `"`+commit+`":{"`+citedCheck+`":["a/*"]}`)
			}
			writeFile(t, f.ties, []byte("{"+strings.Join(entries, ",")+"}"))
		}, func(f citationFiles) string {
			return f.ties + `: commit "c0": not a full object name of 40 or 64 lower-case hex digits`
		}, false},
		{"a changed list with an empty line", func(t *testing.T, f citationFiles) {
			writeFile(t, f.changed, []byte("base "+f.base+"\na/a.go\n\ndocs/x.md\n"))
		}, func(f citationFiles) string {
			return f.changed + ":3: an empty line, where a path was due"
		}, false},
		{"a changed list with no base line", func(t *testing.T, f citationFiles) {
			writeFile(t, f.changed, []byte("a/a.go\n"))
		}, func(f citationFiles) string {
			return f.changed + `:1: no base line "base <commit>", which spec-check changed writes first`
		}, false},
		{"a changed list whose base is a short name", func(t *testing.T, f citationFiles) {
			writeFile(t, f.changed, []byte("base "+f.base[:12]+"\na/a.go\n"))
		}, func(f citationFiles) string {
			return f.changed + `:1: no base line "base <commit>", which spec-check changed writes first`
		}, false},
		{"an empty changed list", func(t *testing.T, f citationFiles) {
			writeFile(t, f.changed, nil)
		}, func(f citationFiles) string {
			return f.changed + `:1: no base line "base <commit>", which spec-check changed writes first`
		}, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newCitationRun()
			f := r.write(t)
			row.change(t, f)
			got := runBinary(t, binary, "", r.args(f, workflow)...)
			if line := "spec-check: " + row.line(f); row.more {
				if got.code != 2 || got.stdout != "" || !strings.HasPrefix(got.stderr, line) || strings.Count(got.stderr, "\n") != 1 {
					t.Errorf("exit %d, stdout %q, stderr %q; want exit 2 and one line that starts %q", got.code, got.stdout, got.stderr, line)
				}
			} else {
				expect(t, got, result{code: 2, stderr: line + "\n"})
			}
			if _, err := os.Stat(f.json); err == nil {
				t.Errorf("an input it could not read, and a --json file at %s", f.json)
			}
		})
	}
	t.Run("a commit that two ties files hold", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		f := r.write(t)
		second := filepath.Join(f.root, "second.json")
		writeFile(t, second, []byte(`{"`+f.head+`":{"`+citedCheck+`":["a/*"]}}`))
		r.extra = []string{"--ties", second}
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), result{code: 2,
			stderr: "spec-check: " + second + ": commit " + f.head + ": " + f.ties + " holds its ties too\n"})
	})
	t.Run("the ties of the head and of the base in two files", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		r.changed = "a/testdata/answer.json\n"
		r.reports["spec-report--go-checks"] = goReport("skip")
		r.baseTies = "-"
		f := r.write(t)
		second := filepath.Join(f.root, "base.json")
		writeFile(t, second, []byte(`{"`+f.base+`":{"`+citedCheck+`":["a/*","a/testdata/**"]}}`))
		r.extra = []string{"--ties", second}
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), refused(f.dir, notRun+
			": a/testdata/answer.json changed (the tie at the base commit holds it), and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"))
	})
	t.Run("a workflow with no spec-citations job", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		f := r.write(t)
		other := filepath.Join(f.root, "other.yaml")
		writeFile(t, other, []byte("jobs:\n  go-checks:\n    runs-on: ubuntu-24.04\n"))
		expect(t, runBinary(t, binary, "", r.args(f, other)...), result{code: 2,
			stderr: "spec-check: " + other + `: specgraph: the workflow has no job "spec-citations"` + "\n"})
	})

	// --repo is the top of its git clone, from which changed names each path: a directory below the
	// top is refused before the command reads an input (here a workflow that does not exist), with
	// the top that git names to pass instead, and so is a directory where git finds no work tree,
	// such as the clone's .git. The top passes, also through a link to it.
	t.Run("a --repo that is not the top of its clone is refused before a read", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		f := r.write(t)
		top := gitIn(t, f.repo, "rev-parse", "--show-toplevel")
		withRepo := func(repo, workflow string) []string {
			args := r.args(f, workflow)
			args[slices.Index(args, "--repo")+1] = repo
			return args
		}
		missing := filepath.Join(f.root, "missing.yaml")
		below := filepath.Join(f.repo, "a")
		expect(t, runBinary(t, binary, "", withRepo(below, missing)...), result{code: 2,
			stderr: "spec-check: " + below + ": is not the top of its git clone; pass the top, --repo " + top + "\n"})
		gitDir := filepath.Join(f.repo, ".git")
		if got, line := runBinary(t, binary, "", withRepo(gitDir, missing)...), "spec-check: "+gitDir+": git finds no work tree there: "; got.code != 2 ||
			got.stdout != "" || !strings.HasPrefix(got.stderr, line) || strings.Count(got.stderr, "\n") != 1 {
			t.Errorf("citations --repo %s: exit %d, stdout %q, stderr %q; want exit 2 and one line that starts %q", gitDir, got.code, got.stdout, got.stderr, line)
		}
		if _, err := os.Stat(f.json); err == nil {
			t.Errorf("a refused --repo, and a --json file at %s", f.json)
		}
		link := filepath.Join(f.root, "link")
		if err := os.Symlink(f.repo, link); err != nil {
			t.Fatal(err)
		}
		expect(t, runBinary(t, binary, "", withRepo(link, workflow)...), cited(f.dir))
		expect(t, runBinary(t, binary, "", r.args(f, workflow)...), cited(f.dir))
	})

	t.Run("a bad argument exits 2 with the line that names it and the usage", func(t *testing.T) {
		t.Parallel()
		r := newCitationRun()
		f := r.write(t)
		base := []string{"citations", "--dir", f.dir, "--repo", f.repo, "--workflow", workflow, "--reports", f.reports, "--needs", f.needs}
		for _, row := range []struct {
			args []string
			line string
		}{
			{[]string{"citations"}, "citations: --dir is required"},
			{append(slices.Clone(base), "extra"), `citations: takes no argument but its flags, got "extra"`},
			{base, "citations: give --changed <file> or --all-changed"},
			{append(slices.Clone(base), "--changed", f.changed, "--all-changed"), "citations: give --changed <file> or --all-changed, not both"},
			{[]string{"changed"}, "changed: --repo is required"},
			{[]string{"changed", "--repo", f.repo}, "changed: --base is required"},
		} {
			expect(t, runBinary(t, binary, "", row.args...), result{code: 2, stderr: "spec-check: " + row.line + "\n" + usage + "\n"})
		}
	})
}

// R8, R48: a submodule is one path of its superproject, the gitlink, and git names that path alone
// when the submodule moves to another commit, so changed prints "sub" and no path under it, while
// a tie names the files that the test reads under it, as "sub/*". A changed path counts for a tie
// that holds a path under it, so the move ends the carry of a check whose tie reaches below the
// submodule. The superproject is a copy of testdata/citations/repo with a real submodule, sub, whose
// answer.txt changes between the base and the head; --ties gives one tie at both. The control is a
// tie that holds no path under sub: the move changes no code that the check runs, and the skipped
// test is cited. The superproject is made twice: once as git submodule add writes .gitmodules, and
// once with ignore = all for sub in the .gitmodules of both commits, a setting that the writer of
// a change controls and with which git diff leaves a moved submodule out of its list; changed must
// list sub whatever .gitmodules says. The head's gitlink is written by update-index, which no
// ignore setting changes: git add leaves the gitlink of a submodule with ignore = all as it is in
// git 2.56.1, the git of the CI runner, where git 2.43.0 stages the move. Before the command, each
// row asserts its fixture: the gitlink of each commit, and git diff's list under the setting.
func TestCitationsSeesAChangedSubmodule(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	workflow := fixture(t, "citations", "workflow.yaml")
	for _, setting := range []struct{ name, ignore string }{
		{"the .gitmodules of git submodule add", ""},
		{"ignore = all for sub in the committed .gitmodules", "all"},
	} {
		t.Run(setting.name, func(t *testing.T) {
			t.Parallel()
			r := newCitationRun()
			r.reports["spec-report--go-checks"] = goReport("skip")
			f := r.write(t)
			source := filepath.Join(f.root, "sub-source")
			if err := os.Mkdir(source, 0o755); err != nil {
				t.Fatal(err)
			}
			gitIn(t, source, "init", "-q")
			writeFile(t, filepath.Join(source, "answer.txt"), []byte("42\n"))
			commitAll(t, source, "", "the answer")
			// git clones a submodule from a local path only when the file protocol is allowed.
			gitIn(t, f.repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", source, "sub")
			if setting.ignore != "" {
				gitIn(t, f.repo, "config", "-f", ".gitmodules", "submodule.sub.ignore", setting.ignore)
			}
			base := commitAll(t, f.repo, f.head, "add the submodule")
			sub := filepath.Join(f.repo, "sub")
			before := gitIn(t, sub, "rev-parse", "HEAD")
			writeFile(t, filepath.Join(sub, "answer.txt"), []byte("0\n"))
			after := commitAll(t, sub, before, "another answer")
			gitIn(t, f.repo, "update-index", "--cacheinfo", "160000,"+after+",sub")
			head := commitAll(t, f.repo, base, "move the submodule")
			for _, at := range []struct{ name, commit, gitlink string }{{"base", base, before}, {"head", head, after}} {
				if entry, want := gitIn(t, f.repo, "ls-tree", at.commit, "sub"), "160000 commit "+at.gitlink+"\tsub"; entry != want {
					t.Fatalf("the fixture: sub at the %s is %q; want %q", at.name, entry, want)
				}
			}
			plain := "sub"
			if setting.ignore == "all" {
				plain = ""
			}
			if got := gitIn(t, f.repo, "diff", "--name-only", base, head); got != plain {
				t.Fatalf("the fixture: git diff --name-only from the base to the head lists %q; want %q under this setting", got, plain)
			}
			list := runBinaryIn(t, binary, "", gitEnv, "changed", "--repo", f.repo, "--base", base)
			expect(t, list, result{stdout: "base " + base + "\nsub\n", stderr: "spec-check: " + f.repo + ": 1 path changed from " + base + " to HEAD\n"})
			writeFile(t, f.changed, []byte(list.stdout))
			for _, row := range []struct{ name, tie, detail string }{
				{"a tie that holds the files of the submodule", `["a/*","fixtures/a.json","sub/*"]`,
					"test-not-run: sub changed (the tie holds sub/*, a path under it), and go-checks did not run example.com/citations/a TestA (its report shows it skipped)"},
				{"control: a tie that holds no path under the submodule", `["a/*","fixtures/a.json"]`, ""},
			} {
				t.Run(row.name, func(t *testing.T) {
					t.Parallel()
					ties := filepath.Join(t.TempDir(), "ties.json")
					writeFile(t, ties, []byte(`{"`+head+`":{"`+citedCheck+`":`+row.tie+`},"`+base+`":{"`+citedCheck+`":`+row.tie+`}}`))
					want := cited(f.dir)
					if row.detail != "" {
						want = refused(f.dir, row.detail)
					}
					expect(t, runBinaryIn(t, binary, "", gitEnv, "citations", "--dir", f.dir, "--repo", f.repo, "--workflow", workflow,
						"--changed", f.changed, "--reports", f.reports, "--needs", f.needs, "--ties", ties), want)
				})
			}
		})
	}
}

// The stated limits of R48 about a report, pinned so that a change of one is seen: a report is the
// account of the process under test. go's own converter writes the run and pass events of a test
// for the lines that the test binary prints with the testing package's marker byte, 0x16, so a
// test that prints the marked run and pass lines of another test makes that test read as run,
// though go test did not run it, and the check of that test is cited. Under go test -json -v, and
// under go test -json with -test.v=true given to the binary after -args, the binary writes no
// marker, and the converter reads each line of the testing package's form as its own: the same
// lines with no marker do the same. Each report is a real one, made here by `go test -json
// -count=1 -run '^TestPrinter$' ./a` over a copy of testdata/citations/repo with a test TestPrinter
// added, which prints TestA's two lines, with the marker when PRINT_TESTA is marked and with none
// when it is plain; the -run pattern leaves TestA out. The controls: TestPrinter quiet, and the
// plain lines under go test -json alone, where they are output; the report does not hold TestA,
// and the check is refused.
func TestCitationsStatedLimitAForgedRun(t *testing.T) {
	t.Parallel()
	binary := cmdtest.Build(t, "spec-check")
	workflow := fixture(t, "citations", "workflow.yaml")
	const printer = "package a\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"testing\"\n)\n\n" +
		"// TestPrinter prints the run and pass lines of TestA, with the testing package's marker when\n" +
		"// PRINT_TESTA is marked and with none when it is plain.\n" +
		"func TestPrinter(t *testing.T) {\n\tswitch os.Getenv(\"PRINT_TESTA\") {\n" +
		"\tcase \"marked\":\n\t\tfmt.Print(\"\\x16=== RUN   TestA\\n\\x16--- PASS: TestA (0.00s)\\n\")\n" +
		"\tcase \"plain\":\n\t\tfmt.Print(\"=== RUN   TestA\\n--- PASS: TestA (0.00s)\\n\")\n\t}\n}\n"
	const notRun = "test-not-run: no base commit, and go-checks did not run example.com/citations/a TestA (its report does not hold it)"
	for _, row := range []struct {
		name, print    string
		flags, binFlag []string
		detail         string
	}{
		{name: "stated limit: a test that prints the marked run and pass lines of TestA makes TestA read as run", print: "marked"},
		{name: "stated limit: under -v, a test that prints them with no marker makes TestA read as run", print: "plain",
			flags: []string{"-v"}},
		{name: "stated limit: with -test.v=true after -args and no -v, the same", print: "plain",
			binFlag: []string{"-args", "-test.v=true"}},
		{name: "control: with no -v and no -test.v, the lines with no marker are output", print: "plain", detail: notRun},
		{name: "control: the test prints nothing, and the report does not hold TestA", print: "none", detail: notRun},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			r := newCitationRun()
			r.allChanged = true
			f := r.write(t)
			writeFile(t, filepath.Join(f.repo, "a", "printer_test.go"), []byte(printer))
			args := append(append([]string{"test", "-json", "-count=1", "-run", "^TestPrinter$"}, row.flags...), "./a")
			cmd := exec.Command("go", append(args, row.binFlag...)...)
			cmd.Dir = f.repo
			cmd.Env = append(cmd.Environ(), "GOWORK=off", "GOFLAGS=", "PRINT_TESTA="+row.print)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			report, err := cmd.Output()
			if err != nil {
				t.Fatalf("go %s: %v\n%s%s", strings.Join(cmd.Args[1:], " "), err, report, stderr.String())
			}
			const pass = `"Action":"pass","Package":"example.com/citations/a","Test":"TestA"`
			if forged := strings.Contains(string(report), pass); forged != (row.detail == "") {
				t.Fatalf("the report holds the pass event of TestA: %t; want %t\n%s", forged, row.detail == "", report)
			}
			writeFile(t, filepath.Join(f.reports, "spec-report--go-checks", "report.jsonl"), report)
			want := cited(f.dir)
			if row.detail != "" {
				want = refused(f.dir, row.detail)
			}
			expect(t, runBinary(t, binary, "", r.args(f, workflow)...), want)
		})
	}
}

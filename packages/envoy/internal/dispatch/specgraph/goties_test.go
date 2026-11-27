package specgraph

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// listenerReads is what the repository records that the test of checkStateCheck, in package
// cmd/listener, reads: its package, its testdata, the workspace file and one fixture read through
// "..".
var listenerReads = []string{
	"go.work",
	"packages/contracts/fixtures/dispatch-targeted-delivery.json",
	"packages/envoy/cmd/listener/*",
	"packages/envoy/cmd/listener/testdata/**",
}

// readsCall is one call of a stub TestReads.
type readsCall struct {
	workdir   string
	pkgs, env []string
}

// stubReads is a TestReads that records each call and answers the reads that answers holds for the
// one package of the call, or err.
func stubReads(calls *[]readsCall, answers map[string][]string, err error) TestReads {
	return func(workdir string, pkgs []string, env []string) ([]string, error) {
		*calls = append(*calls, readsCall{workdir, slices.Clone(pkgs), slices.Clone(env)})
		if err != nil {
			return nil, err
		}
		return answers[strings.Join(pkgs, " ")], nil
	}
}

// The tie of a go check is what its test reads by the repository's own record (R18: "The code that
// each check runs, from the test that the check names").
func TestGoTiesTakesTheReadsOfTheTest(t *testing.T) {
	t.Parallel()
	var calls []readsCall
	reads := stubReads(&calls, map[string][]string{"github.com/sjawhar/envoy/cmd/listener": listenerReads}, nil)
	ties := GoTies([]Check{checkStateCheck, checkStateProduction}, ".", nil, reads)

	tie, ok := ties[checkStateCheck.ID]
	if !ok || !tie.Known || !slices.Equal(tie.Paths, listenerReads) {
		t.Fatalf("GoTies gave %+v, %v for %s; want the known tie %q", tie, ok, checkStateCheck.ID, listenerReads)
	}
	if !tie.Covers("packages/contracts/fixtures/dispatch-targeted-delivery.json") {
		t.Error("the tie does not cover the fixture that the test reads through ..")
	}
	if tie.Covers("packages/envoy/cmd/listener/sub/x.go") {
		t.Error("the tie covers a file of a package under the test's package, which the test does not read")
	}
	if _, ok := ties[checkStateProduction.ID]; ok || len(ties) != 1 {
		t.Errorf("GoTies gave %d ties, one for the query check %s too; want one tie, of the program check", len(ties), checkStateProduction.ID)
	}
	want := []readsCall{{workdir: ".", pkgs: []string{"github.com/sjawhar/envoy/cmd/listener"}}}
	if len(calls) != 1 || calls[0].workdir != want[0].workdir || !slices.Equal(calls[0].pkgs, want[0].pkgs) || len(calls[0].env) != 0 {
		t.Errorf("the reads were asked %+v; want %+v", calls, want)
	}
}

// R18: "A tie that Dispatch cannot derive reads as unknown, not as none." Each cause is named.
func TestATieThatCannotBeDerivedIsUnknown(t *testing.T) {
	t.Parallel()
	check := func(runner, selector string) Check {
		c := checkStateCheck
		c.Test = &Test{Runner: runner, Selector: selector, Job: "go-checks"}
		return c
	}
	for _, row := range []struct {
		name    string
		check   Check
		answers map[string][]string
		err     error
		why     string
	}{
		{name: "the reads cannot be derived", check: checkStateCheck,
			err: errors.New("go list failed"), why: "go list failed"},
		{name: "an answer in a form that is no tie", check: checkStateCheck,
			answers: map[string][]string{"github.com/sjawhar/envoy/cmd/listener": {"go.work", "p/*.go"}}, why: `"p/*.go"`},
		{name: "an empty answer", check: checkStateCheck,
			answers: map[string][]string{"github.com/sjawhar/envoy/cmd/listener": {}}, why: "holds no path"},
		{name: "runner bun", check: check("bun", "src/a.test.ts"), why: `runner "bun" has no tie reader`},
		{name: "a selector that picks no one test", check: check("go", "TestX"), why: "selector is not"},
		{name: "no test part", check: func() Check { c := checkStateCheck; c.Test = nil; return c }(), why: "names no test"},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			var calls []readsCall
			ties := GoTies([]Check{row.check}, ".", nil, stubReads(&calls, row.answers, row.err))
			tie, ok := ties[row.check.ID]
			if !ok || tie.Known || len(tie.Paths) != 0 || !strings.Contains(tie.Why, row.why) {
				t.Errorf("GoTies gave %+v, %v; want an unknown tie whose Why holds %q", tie, ok, row.why)
			}
			if tie.Covers("packages/envoy/cmd/listener/main.go") {
				t.Error("an unknown tie covers a path")
			}
		})
	}
}

// GoTies hands the path of a check's selector to reads, which runs go with it, so a path that
// starts with "-" would reach go as a flag, such as -toolexec, which names a program that go runs.
// ParseSelector takes such a path, since Find runs go list -- <path>, which reads it as no flag.
// GoTies refuses it as a tie that is not known, with a cause that says so, and asks reads nothing.
// The stub reads answers a tie for each path, so a path that reached it reads as a known tie. The
// control is the same check with the path of a package, whose tie is the answer.
func TestGoTiesHandsReadsNoPathThatStartsWithADash(t *testing.T) {
	t.Parallel()
	for _, row := range []struct {
		path, why string // why is "" for a known tie
	}{
		{path: "-toolexec=/bin/false", why: `the selector's path "-toolexec=/bin/false" starts with "-", which go would read as a flag`},
		{path: "-exec=/bin/false", why: `the selector's path "-exec=/bin/false" starts with "-", which go would read as a flag`},
		{path: "example.com/citations/a"},
	} {
		t.Run(row.path, func(t *testing.T) {
			t.Parallel()
			c := checkStateCheck
			c.Test = &Test{Runner: "go", Selector: row.path + " TestA", Job: "go-checks"}
			var calls []readsCall
			tie := GoTies([]Check{c}, ".", nil, stubReads(&calls, map[string][]string{row.path: {"a/*"}}, nil))[c.ID]
			switch {
			case row.why == "" && (!tie.Known || !slices.Equal(tie.Paths, []string{"a/*"}) || len(calls) != 1):
				t.Errorf("the control: GoTies gave %+v after the reads were asked %+v; want the known tie a/*, asked once", tie, calls)
			case row.why != "" && (tie.Known || tie.Why != row.why || len(calls) != 0):
				t.Errorf("GoTies gave %+v after the reads were asked %+v; want an unknown tie whose Why is %q, and no ask", tie, calls, row.why)
			}
		})
	}
}

// ScriptTestReads runs the repository's go-test-reads.sh and reads its one JSON object. The test
// writes each script and then runs it, so it runs alone: a fork of a parallel test while a script
// is open for writing holds the file open in the child, and running the script then fails with
// "text file busy" (ETXTBSY).
func TestScriptTestReadsReadsTheScript(t *testing.T) {
	for _, row := range []struct {
		name, script string
		want         []string
		err          string // "" when no error is wanted
	}{
		{name: "reads", script: `printf '{"reads":["a/*"],"problems":[]}\n'`, want: []string{"a/*"}},
		{name: "its arguments", script: `args=; for a in "$@"; do args="$args${args:+,}\"$a\""; done; printf '{"reads":[%s],"problems":[]}\n' "$args"`,
			want: []string{"--env", "GOWORK=off", ".", "example.com/citations/a"}},
		{name: "a problem", script: `printf '{"reads":[],"problems":["go list failed for ./gone"]}\n'; exit 1`, err: "go list failed for ./gone"},
		{name: "no JSON", script: `printf 'x\n'`, err: "printed no reads object"},
		{name: "another exit", script: `echo 'usage: go-test-reads.sh' >&2; exit 2`, err: "usage: go-test-reads.sh"},
		{name: "a problem and exit 0", script: `printf '{"reads":["a/*"],"problems":["p"]}\n'`, err: "p"},
		{name: "no reads", script: `printf '{"problems":[]}\n'`, err: "printed no reads for"},
	} {
		t.Run(row.name, func(t *testing.T) {
			root := t.TempDir()
			script := filepath.Join(root, ".github", "scripts", "go-test-reads.sh")
			if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(script, []byte("#!/bin/sh\n"+row.script+"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			got, err := ScriptTestReads(root, nil)(".", []string{"example.com/citations/a"}, []string{"GOWORK=off"})
			switch {
			case row.err == "" && (err != nil || !slices.Equal(got, row.want)):
				t.Errorf("reads = %q, %v; want %q, nil", got, err, row.want)
			case row.err != "" && (err == nil || !strings.Contains(err.Error(), row.err) || got != nil):
				t.Errorf("reads = %q, %v; want no reads and an error holding %q", got, err, row.err)
			}
		})
	}
}

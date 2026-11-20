package main

import (
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/cmdtest"
	"gopkg.in/yaml.v3"
)

// actionPath is the composite action that runs ci-report as the last step of a job, from this
// package's directory.
var actionPath = filepath.Join("..", "..", "..", "..", ".github", "actions", "dispatch-results", "action.yml")

// compositeAction is the part of an action.yml that this test reads.
type compositeAction struct {
	Inputs map[string]struct {
		Required bool   `yaml:"required"`
		Default  string `yaml:"default"`
	} `yaml:"inputs"`
	Runs struct {
		Using string `yaml:"using"`
		Steps []struct {
			Name  string            `yaml:"name"`
			Shell string            `yaml:"shell"`
			Env   map[string]string `yaml:"env"`
			Run   string            `yaml:"run"`
			Uses  string            `yaml:"uses"`
		} `yaml:"steps"`
	} `yaml:"runs"`
}

// fakeGo stands in for go on PATH: it records its arguments, NUL-separated, in go.args of
// CI_REPORT_STUB_DIR, and runs the binary this test built for `go run ./packages/envoy/cmd/ci-report`
// with the arguments after those two. Any other go command exits 97.
const fakeGo = `#!/bin/sh
printf '%s\0' "$@" > "$CI_REPORT_STUB_DIR/go.args"
if [ "$1" != run ] || [ "$2" != ./packages/envoy/cmd/ci-report ]; then
  echo "fake go: not go run ./packages/envoy/cmd/ci-report: $*" >&2
  exit 97
fi
shift 2
exec "$CI_REPORT_BINARY" "$@"
`

// fakeBun stands in for bun on PATH: `bun packages/envoy-client/bin/dispatch.ts <args>` runs the
// stub dispatch command with <args>, and any other bun command exits 97.
const fakeBun = `#!/bin/sh
if [ "$1" != packages/envoy-client/bin/dispatch.ts ]; then
  echo "fake bun: not the dispatch command: $*" >&2
  exit 97
fi
shift
exec "$CI_REPORT_STUB" "$@"
`

// TestTheActionRunsTheReporter runs the one step of .github/actions/dispatch-results as a runner
// runs a `shell: bash` step, in a checkout of testdata/spec, with its inputs resolved as the
// runner resolves them. go and bun are fakes on PATH: go runs the binary this test built, and bun
// runs the stub dispatch command. So the step's own script, its environment and its file of
// lines are what the test holds, and the command is the real one.
func TestTheActionRunsTheReporter(t *testing.T) {
	t.Parallel()
	text, err := os.ReadFile(actionPath)
	if err != nil {
		t.Fatal(err)
	}
	var action compositeAction
	if err := yaml.Unmarshal(text, &action); err != nil {
		t.Fatal(err)
	}
	if action.Runs.Using != "composite" || len(action.Runs.Steps) != 1 || action.Runs.Steps[0].Shell != "bash" || action.Runs.Steps[0].Uses != "" {
		t.Fatalf("the action is not one composite step of shell bash: %+v", action.Runs)
	}
	step := action.Runs.Steps[0]
	for name, want := range map[string]struct {
		required bool
		dflt     string
	}{"report": {required: true}, "project": {required: true}, "job": {dflt: "${{ github.job }}"}} {
		if got, ok := action.Inputs[name]; !ok || got.Required != want.required || got.Default != want.dflt {
			t.Errorf("input %s: %+v (held %v), want required %v and default %q", name, got, ok, want.required, want.dflt)
		}
	}
	if len(action.Inputs) != 3 {
		t.Errorf("the action has inputs %v, want report, project and job alone", slices.Sorted(maps.Keys(action.Inputs)))
	}

	binary := cmdtest.Build(t, "ci-report")
	report, err := filepath.Abs(filepath.Join("testdata", "report.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// runStep runs the step in a new checkout with the inputs given, github.job being go-checks,
	// and answers its exit status, its standard output, the file of lines it wrote, go's arguments
	// and the stub's calls.
	runStep := func(t *testing.T, mode string, inputs map[string]string) (code int, stdout, jsonl string, goArgs []string, calls []call) {
		t.Helper()
		dir, _ := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		bin := t.TempDir()
		writeFile(t, filepath.Join(bin, "go"), fakeGo)
		writeFile(t, filepath.Join(bin, "bun"), fakeBun)
		for _, name := range []string{"go", "bun"} {
			if err := os.Chmod(filepath.Join(bin, name), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		runnerTemp := t.TempDir()
		script := filepath.Join(t.TempDir(), "step.sh")
		writeFile(t, script, step.Run)
		cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", script)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), stub.env(mode)...)
		cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "RUNNER_TEMP="+runnerTemp,
			"CI_REPORT_BINARY="+binary, "CI_REPORT_STUB="+stub.path)
		for name, value := range step.Env {
			cmd.Env = append(cmd.Env, name+"="+resolve(t, value, action, inputs))
		}
		var out, errOut strings.Builder
		cmd.Stdout, cmd.Stderr = &out, &errOut
		err := cmd.Run()
		var exitError *exec.ExitError
		if err != nil && !errors.As(err, &exitError) {
			t.Fatal(err)
		}
		written, err := os.ReadFile(filepath.Join(runnerTemp, "dispatch-results.jsonl"))
		if err != nil {
			t.Fatalf("the step wrote no $RUNNER_TEMP/dispatch-results.jsonl: %v; stderr:\n%s", err, errOut.String())
		}
		args, err := os.ReadFile(filepath.Join(stub.dir, "go.args"))
		if err != nil {
			t.Fatal(err)
		}
		return cmd.ProcessState.ExitCode(), out.String(), string(written), strings.Split(strings.TrimSuffix(string(args), "\x00"), "\x00"), stub.calls(t)
	}
	wantGo := func(job string) []string {
		return []string{"run", "./packages/envoy/cmd/ci-report", "--dir", ".dispatch/spec", "--job", job, "--report", report,
			"--project", "ACME", "--dispatch-cli", "bun packages/envoy-client/bin/dispatch.ts"}
	}

	t.Run("the step reports the job's checks under the job's own identity and keeps each line", func(t *testing.T) {
		t.Parallel()
		code, stdout, jsonl, goArgs, calls := runStep(t, "store", map[string]string{"report": report, "project": "ACME"})
		if code != 0 {
			t.Errorf("the step exits %d, want 0", code)
		}
		if !slices.Equal(goArgs, wantGo(job)) {
			t.Errorf("go got %q, want %q", goArgs, wantGo(job))
		}
		if len(calls) != 3 {
			t.Errorf("the stub got %d calls, want one for each of the 3 tests of go-checks that ran", len(calls))
		}
		for _, c := range calls {
			if c.identity != "github-actions" {
				t.Errorf("a call of %q ran with AGENT_SECRETS_MACHINE_IDENTITY %q, want github-actions", c.args, c.identity)
			}
		}
		if got := parseLines(t, jsonl); len(got) != 8 || jsonl != stdout {
			t.Errorf("dispatch-results.jsonl holds %d lines and the log %q, want the 8 lines of go-checks in both:\n%s", len(got), stdout, jsonl)
		}
	})

	t.Run("the job input names the job whose checks the step reports", func(t *testing.T) {
		t.Parallel()
		_, _, jsonl, goArgs, calls := runStep(t, "store", map[string]string{"report": report, "project": "ACME", "job": "web-checks"})
		if !slices.Equal(goArgs, wantGo("web-checks")) {
			t.Errorf("go got %q, want %q", goArgs, wantGo("web-checks"))
		}
		if got := parseLines(t, jsonl); len(got) != 1 || got[0].Check != "a-read-of-one-claim-ends-in-time" || len(calls) != 1 {
			t.Errorf("the step wrote %+v with %d calls, want the one check of web-checks and one call", got, len(calls))
		}
	})

	t.Run("a failed call fails the step and keeps the lines", func(t *testing.T) {
		t.Parallel()
		code, _, jsonl, _, _ := runStep(t, "refuse", map[string]string{"report": report, "project": "ACME"})
		if code != 1 {
			t.Errorf("the step exits %d, want 1", code)
		}
		failed := 0
		for _, l := range parseLines(t, jsonl) {
			if l.Exit != nil && *l.Exit == 1 && l.Why == brokerRefusal {
				failed++
			}
		}
		if failed != 3 {
			t.Errorf("dispatch-results.jsonl holds %d lines with the broker's refusal, want 3:\n%s", failed, jsonl)
		}
	})
}

// expression is one ${{ ... }} of a workflow file.
var expression = regexp.MustCompile(`\$\{\{\s*([^}]*?)\s*\}\}`)

// resolve is value with each expression replaced as the runner replaces it for this test: an
// input by the value given, or by the input's default, and github.job by go-checks. Any other
// expression fails the test, since the test would then not know what the step gets.
func resolve(t *testing.T, value string, action compositeAction, inputs map[string]string) string {
	t.Helper()
	return expression.ReplaceAllStringFunc(value, func(match string) string {
		name := expression.FindStringSubmatch(match)[1]
		switch {
		case name == "github.job":
			return job
		case strings.HasPrefix(name, "inputs."):
			input := strings.TrimPrefix(name, "inputs.")
			if given, ok := inputs[input]; ok {
				return given
			}
			if declared, ok := action.Inputs[input]; ok {
				return resolve(t, declared.Default, action, inputs)
			}
		}
		t.Fatalf("the step's environment holds %s, which this test does not resolve", match)
		return ""
	})
}

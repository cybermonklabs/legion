package main

//ci:reads .github/actions/dispatch-results/action.yml actionPath joins ../../../../.github/actions/dispatch-results/action.yml, which TestTheActionRunsTheReporter reads and whose steps it runs

import (
	"errors"
	"fmt"
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
		Using string       `yaml:"using"`
		Steps []actionStep `yaml:"steps"`
	} `yaml:"runs"`
}

// actionStep is one step of a composite action.
type actionStep struct {
	ID    string            `yaml:"id"`
	Shell string            `yaml:"shell"`
	Env   map[string]string `yaml:"env"`
	Run   string            `yaml:"run"`
	Uses  string            `yaml:"uses"`
}

// buildLines is the whole script of the step build: agent-secrets from cmd/cipher and ci-report,
// each named by its own -o, since cmd/cipher's base name is not its binary's.
var buildLines = []string{
	`go build -o "$RUNNER_TEMP/dispatch-results/bin/agent-secrets" ./packages/envoy/cmd/cipher`,
	`go build -o "$RUNNER_TEMP/dispatch-results/bin/ci-report" ./packages/envoy/cmd/ci-report`,
}

// reportCommand is the command line of the step report: ci-report under agent-secrets
// result-grant, so the reporter and each dispatch command it runs hold the airlock's placeholder
// and no Dispatch credential.
const reportCommand = `"$RUNNER_TEMP/dispatch-results/bin/agent-secrets" result-grant -- "$RUNNER_TEMP/dispatch-results/bin/ci-report" --dir .dispatch/spec --job "$JOB" --report "$REPORT" --project "$PROJECT" --dispatch-cli "bun packages/envoy-client/bin/dispatch.ts"`

// reportScript is the whole script of the step report: reportCommand, every line it prints to the
// log through descriptor 3, and each line but agent-secrets' mask line to
// $RUNNER_TEMP/dispatch-results.jsonl.
const reportScript = `{ ` + reportCommand + ` | tee /dev/fd/3 | sed '/^::add-mask::/d' > "$RUNNER_TEMP/dispatch-results.jsonl"; } 3>&1` + "\n"

// reportEnv is the whole environment the step report sets: the inputs, so no input is text of its
// script, and the identity agent-secrets trades.
var reportEnv = map[string]string{
	"REPORT":                         "${{ inputs.report }}",
	"PROJECT":                        "${{ inputs.project }}",
	"JOB":                            "${{ inputs.job }}",
	"AGENT_SECRETS_MACHINE_IDENTITY": "github-actions",
}

// placeholder is the result grant's placeholder that the fake agent-secrets gives its command.
const placeholder = "envoy-placeholder-results-test-7f3a"

// fakeGo stands in for go on PATH: each call records its arguments, NUL-separated, in go.<n>.args
// of CI_REPORT_STUB_DIR, n counting from 0. `go build -o <path> ./packages/envoy/cmd/ci-report`
// copies the binary this test built to <path>, and `go build -o <path> ./packages/envoy/cmd/cipher`
// copies fakeAgentSecrets there, each making <path>'s directory as go build does. Any other go
// command exits 97.
const fakeGo = `#!/bin/sh
n=0
while [ -e "$CI_REPORT_STUB_DIR/go.$n.args" ]; do n=$((n + 1)); done
printf '%s\0' "$@" > "$CI_REPORT_STUB_DIR/go.$n.args"
built=
if [ $# -eq 4 ] && [ "$1" = build ] && [ "$2" = -o ]; then
  case $4 in
  ./packages/envoy/cmd/ci-report) built=$CI_REPORT_BINARY ;;
  ./packages/envoy/cmd/cipher) built=$CI_REPORT_AGENT_SECRETS ;;
  esac
fi
if [ -z "$built" ]; then
  echo "fake go: not go build -o <path> of ./packages/envoy/cmd/ci-report or ./packages/envoy/cmd/cipher: $*" >&2
  exit 97
fi
mkdir -p "$(dirname "$3")" && exec cp "$built" "$3"
`

// fakeAgentSecrets stands in for agent-secrets: it records its arguments, NUL-separated, in
// agent-secrets.args of CI_REPORT_STUB_DIR, and its AGENT_SECRETS_MACHINE_IDENTITY in
// agent-secrets.identity. `result-grant -- COMMAND [ARGS...]` runs as packages/envoy/cmd/cipher's
// does, less the exchange: it refuses to run anything without AGENT_SECRETS_URL or without the
// github-actions identity; under GITHUB_ACTIONS=true it first prints ::add-mask:: with the
// placeholder on standard output; and it executes the command with the placeholder in
// DISPATCH_RESULTS_TOKEN and in the airlock's proxy URL, and with no AGENT_SECRETS_* variable but
// the broker's URL and the helper's (sessionIdentityVars of cmd/cipher). Like cmd/cipher's
// buildChildEnv, it passes GitHub's ACTIONS_ID_TOKEN_REQUEST_* on to the command, so the reporter
// is the one that keeps them from the dispatch command. Any other command exits 2.
const fakeAgentSecrets = `#!/bin/sh
printf '%s\0' "$@" > "$CI_REPORT_STUB_DIR/agent-secrets.args"
printf '%s' "${AGENT_SECRETS_MACHINE_IDENTITY-unset}" > "$CI_REPORT_STUB_DIR/agent-secrets.identity"
if [ $# -lt 3 ] || [ "$1" != result-grant ] || [ "$2" != -- ]; then
  echo "agent-secrets result-grant: use result-grant -- COMMAND [ARGS...]" >&2
  exit 2
fi
shift 2
if [ -z "$AGENT_SECRETS_URL" ]; then
  echo "agent-secrets result-grant: AGENT_SECRETS_URL is required" >&2
  exit 2
fi
if [ "$AGENT_SECRETS_MACHINE_IDENTITY" != github-actions ]; then
  echo "agent-secrets result-grant: a GitHub workflow identity is required" >&2
  exit 2
fi
if [ "$GITHUB_ACTIONS" = true ]; then
  echo "::add-mask::` + placeholder + `"
fi
for name in $(env | sed -n 's/^\(AGENT_SECRETS_[A-Za-z0-9_]*\)=.*/\1/p'); do
  case $name in
  AGENT_SECRETS_URL | AGENT_SECRETS_HELPER_SOCK | AGENT_SECRETS_KEY_DIR) ;;
  *) unset "$name" ;;
  esac
done
export DISPATCH_RESULTS_TOKEN=` + placeholder + ` HTTPS_PROXY=http://agent-secrets:` + placeholder + `@127.0.0.1:9
exec "$@"
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

// actionRun is what one run of the action's two steps did.
type actionRun struct {
	buildCode, reportCode int
	// log is what the step report printed on standard output, the job's log; jsonl the file
	// $RUNNER_TEMP/dispatch-results.jsonl it wrote; stderr what both steps printed there.
	log, jsonl, stderr string
	// runnerTemp is the run's $RUNNER_TEMP, and commit the checkout's commit.
	runnerTemp, commit string
	// goCalls are go's arguments, a call each; grantArgs and grantIdentity agent-secrets'
	// arguments and its AGENT_SECRETS_MACHINE_IDENTITY, nil and "" when it did not run.
	goCalls       [][]string
	grantArgs     []string
	grantIdentity string
	calls         []call
}

// TestTheActionRunsTheReporter reads .github/actions/dispatch-results and holds its step build to
// the two go builds and its step report to ci-report under agent-secrets result-grant, and runs
// both steps as a runner runs a `shell: bash` step, in a checkout of testdata/spec, with their
// inputs resolved as the runner resolves them and the environment a calling job gives them. go,
// agent-secrets and bun are fakes on PATH: go copies in the ci-report this test built and a fake
// agent-secrets, which gives its command the placeholder as cmd/cipher's result-grant does, and
// bun runs the stub dispatch command. So the steps' own scripts, their environment and the file
// of lines are what the test holds, the reporter is the real one, and each dispatch call shows
// the credential it held.
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
	steps := action.Runs.Steps
	if action.Runs.Using != "composite" || len(steps) != 2 || steps[0].ID != "build" || steps[1].ID != "report" {
		t.Fatalf("the action is not the two composite steps build and report: %+v", action.Runs)
	}
	for _, step := range steps {
		if step.Shell != "bash" || step.Uses != "" {
			t.Fatalf("step %s is not a run: step of shell bash: %+v", step.ID, step)
		}
	}
	build, report := steps[0], steps[1]
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
	// The text of each script and environment, whole: a step that ran anything else, or an input
	// that reached a script as its text, would be another action. The runs below show what the
	// text does, and say so on their own when a text check fails.
	if got := strings.Split(strings.TrimSuffix(build.Run, "\n"), "\n"); !slices.Equal(got, buildLines) || len(build.Env) != 0 {
		t.Errorf("step build runs %q with environment %v, want exactly %q and no environment of its own", got, build.Env, buildLines)
	}
	if report.Run != reportScript {
		t.Errorf("step report runs\n%s\nwant\n%s", report.Run, reportScript)
	}
	if !maps.Equal(report.Env, reportEnv) {
		t.Errorf("step report sets the environment %v, want %v", report.Env, reportEnv)
	}

	binary := cmdtest.Build(t, "ci-report")
	p := newProbe(t)
	fakes := t.TempDir()
	for name, content := range map[string]string{"go": fakeGo, "bun": fakeBun, "agent-secrets": fakeAgentSecrets} {
		writeFile(t, filepath.Join(fakes, name), content)
		if err := os.Chmod(filepath.Join(fakes, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The job's environment: this process's, less every variable of Dispatch, of agent-secrets and
	// of GitHub's OIDC token, which this machine's own settings could give, and with what a
	// calling job sets: GitHub's GITHUB_ACTIONS, the three settings the action's description asks
	// the calling step for, and the OIDC token request of a job with `id-token: write`
	// (oidcEnv). go and bun are the fakes; agent-secrets is reached by the path the step build
	// wrote.
	jobEnv := slices.DeleteFunc(os.Environ(), func(variable string) bool {
		name, _, _ := strings.Cut(variable, "=")
		return strings.HasPrefix(name, "DISPATCH_") || strings.HasPrefix(name, "AGENT_SECRETS_") ||
			strings.HasPrefix(name, "ACTIONS_ID_TOKEN_") || name == "GITHUB_ACTIONS"
	})
	jobEnv = slices.Concat(jobEnv, oidcEnv, []string{"GITHUB_ACTIONS=true", "DISPATCH_URL=https://dispatch.example.test",
		"AGENT_SECRETS_URL=https://broker.example.test", "AGENT_SECRETS_MACHINE_AUDIENCE=https://broker.example.test",
		"PATH=" + fakes + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CI_REPORT_BINARY=" + binary, "CI_REPORT_AGENT_SECRETS=" + filepath.Join(fakes, "agent-secrets")})

	// runAction runs both steps in a new checkout with the inputs given, github.job being
	// go-checks, and the stub in mode.
	runAction := func(t *testing.T, mode string, inputs map[string]string) actionRun {
		t.Helper()
		dir, commit := checkout(t, filepath.Join("testdata", "spec"))
		stub := newStub(t)
		run := actionRun{runnerTemp: t.TempDir(), commit: commit}
		env := append(slices.Clone(jobEnv), "RUNNER_TEMP="+run.runnerTemp, "CI_REPORT_STUB="+stub.path)
		env = append(env, stub.env(mode)...)
		var stderr strings.Builder
		runScript := func(step actionStep) (int, string) {
			script := filepath.Join(t.TempDir(), step.ID+".sh")
			writeFile(t, script, step.Run)
			cmd := exec.Command("bash", "--noprofile", "--norc", "-eo", "pipefail", script)
			cmd.Dir = dir
			cmd.Env = slices.Clone(env)
			for name, value := range step.Env {
				cmd.Env = append(cmd.Env, name+"="+resolve(t, value, action, inputs))
			}
			var out strings.Builder
			cmd.Stdout, cmd.Stderr = &out, &stderr
			err := cmd.Run()
			var exitError *exec.ExitError
			if err != nil && !errors.As(err, &exitError) {
				t.Fatal(err)
			}
			return cmd.ProcessState.ExitCode(), out.String()
		}
		run.buildCode, _ = runScript(build)
		if run.buildCode != 0 {
			t.Fatalf("step build exits %d, want 0; stderr:\n%s", run.buildCode, stderr.String())
		}
		run.reportCode, run.log = runScript(report)
		run.stderr = stderr.String()
		written, err := os.ReadFile(filepath.Join(run.runnerTemp, "dispatch-results.jsonl"))
		if err != nil {
			t.Fatalf("the step wrote no $RUNNER_TEMP/dispatch-results.jsonl: %v; stderr:\n%s", err, run.stderr)
		}
		run.jsonl = string(written)
		nulSplit := func(content []byte) []string {
			return strings.Split(strings.TrimSuffix(string(content), "\x00"), "\x00")
		}
		for n := 0; ; n++ {
			args, err := os.ReadFile(filepath.Join(stub.dir, fmt.Sprintf("go.%d.args", n)))
			if errors.Is(err, os.ErrNotExist) {
				break
			} else if err != nil {
				t.Fatal(err)
			}
			run.goCalls = append(run.goCalls, nulSplit(args))
		}
		if args, err := os.ReadFile(filepath.Join(stub.dir, "agent-secrets.args")); err == nil {
			run.grantArgs = nulSplit(args)
			identity, err := os.ReadFile(filepath.Join(stub.dir, "agent-secrets.identity"))
			if err != nil {
				t.Fatal(err)
			}
			run.grantIdentity = string(identity)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		run.calls = stub.calls(t)
		return run
	}
	bin := func(run actionRun, name string) string {
		return filepath.Join(run.runnerTemp, "dispatch-results", "bin", name)
	}
	wantGrant := func(run actionRun, job string) []string {
		return []string{"result-grant", "--", bin(run, "ci-report"), "--dir", ".dispatch/spec", "--job", job, "--report", p.report,
			"--project", "ACME", "--dispatch-cli", "bun packages/envoy-client/bin/dispatch.ts"}
	}
	// heldThePlaceholder fails the test unless each call held the result grant's placeholder, which
	// the reporter hands on from its own environment, no other Dispatch credential, and no
	// variable of GitHub's OIDC token request, which the fake agent-secrets passes on as cmd/cipher
	// does and the reporter keeps from the command. Whether the command got the run's
	// AGENT_SECRETS_MACHINE_IDENTITY is cmd/cipher's to show: the fake takes it away itself, so a
	// check of it here would read the fake.
	heldThePlaceholder := func(t *testing.T, calls []call) {
		t.Helper()
		for _, c := range calls {
			if c.token != placeholder || len(c.bearers) != 0 || len(c.oidc) != 0 {
				t.Errorf("a call of %q held DISPATCH_RESULTS_TOKEN %q, the variables %q and %q; want the placeholder %q and none",
					c.args, c.token, c.bearers, c.oidc, placeholder)
			}
		}
	}
	// fromTheFile fails the test unless the step wrote exactly want, the reporter's lines, to its
	// file, and the stub got the probe's calls, at the run's commit.
	fromTheFile := func(t *testing.T, run actionRun, want []line) {
		t.Helper()
		expectLines(t, run.jsonl, want)
		if !slices.EqualFunc(run.calls, p.wantCalls(t, run.commit), equalCall) {
			t.Errorf("the stub got %d calls %+v\nwant the probe's %d", len(run.calls), run.calls, len(p.wantCalls(t, run.commit)))
		}
		heldThePlaceholder(t, run.calls)
	}

	t.Run("the step builds both commands, reports the job's checks under the result grant and keeps each line", func(t *testing.T) {
		t.Parallel()
		run := runAction(t, "store", map[string]string{"report": p.report, "project": "ACME"})
		wantGo := [][]string{
			{"build", "-o", bin(run, "agent-secrets"), "./packages/envoy/cmd/cipher"},
			{"build", "-o", bin(run, "ci-report"), "./packages/envoy/cmd/ci-report"},
		}
		if !slices.EqualFunc(run.goCalls, wantGo, slices.Equal) {
			t.Errorf("go got %q, want %q", run.goCalls, wantGo)
		}
		if run.reportCode != 0 {
			t.Errorf("step report exits %d, want 0; stderr:\n%s", run.reportCode, run.stderr)
		}
		if want := wantGrant(run, job); !slices.Equal(run.grantArgs, want) || run.grantIdentity != "github-actions" {
			t.Errorf("agent-secrets got %q with AGENT_SECRETS_MACHINE_IDENTITY %q, want %q with github-actions", run.grantArgs, run.grantIdentity, want)
		}
		fromTheFile(t, run, p.stored())
		if strings.Contains(run.jsonl, placeholder) {
			t.Errorf("dispatch-results.jsonl holds the placeholder:\n%s", run.jsonl)
		}
		if want := "::add-mask::" + placeholder + "\n" + run.jsonl; run.log != want {
			t.Errorf("the log is\n%s\nwant agent-secrets' mask line, then the lines of the file:\n%s", run.log, want)
		}
	})

	t.Run("the job input names the job whose checks the step reports", func(t *testing.T) {
		t.Parallel()
		run := runAction(t, "store", map[string]string{"report": p.report, "project": "ACME", "job": "web-checks"})
		if want := wantGrant(run, "web-checks"); !slices.Equal(run.grantArgs, want) {
			t.Errorf("agent-secrets got %q, want %q", run.grantArgs, want)
		}
		const check = "a-read-of-one-claim-ends-in-time"
		expectLines(t, run.jsonl, []line{{Check: check, Test: "example.com/acme/probe/slow TestTheQuickReadEnds", Outcome: "pass", Exit: new(0), ResultID: "row-" + check}})
		wantCalls := []call{{
			args:  []string{"result", "--project", "ACME", "--check", check, "--verdict", "pass", "--commit", run.commit, "--observed-file", "-"},
			stdin: p.output(t, "slow", "TestTheQuickReadEnds"),
		}}
		if !slices.EqualFunc(run.calls, wantCalls, equalCall) {
			t.Errorf("the stub got %+v\nwant %+v", run.calls, wantCalls)
		}
		heldThePlaceholder(t, run.calls)
	})

	t.Run("a write the airlock refused fails the step and keeps the lines", func(t *testing.T) {
		t.Parallel()
		run := runAction(t, "refuse", map[string]string{"report": p.report, "project": "ACME"})
		if run.reportCode != 1 {
			t.Errorf("step report exits %d, want 1", run.reportCode)
		}
		fromTheFile(t, run, p.answered(1, airlockRefusal))
	})

	t.Run("a write after the grant's end fails the step, and its line names the end", func(t *testing.T) {
		t.Parallel()
		run := runAction(t, "ends-after-first", map[string]string{"report": p.report, "project": "ACME"})
		if run.reportCode != 1 {
			t.Errorf("step report exits %d, want 1", run.reportCode)
		}
		fromTheFile(t, run, p.endsAfterFirst())
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

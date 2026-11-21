package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	configpkg "github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// takeBound is the first bound a reading judges by when --take-timeout is not given:
// worker_take_timeout_seconds's default.
const takeBound = taskCostTakeTimeout * time.Second

// parseTaskCostArgs parses `legion task-cost`'s arguments with the command's flags
// (addTaskCostFlags) as the command's own parse reads them: a flag may follow a word, and a word
// is refused. A help flag answers pflag.ErrHelp.
func parseTaskCostArgs(args []string) (supervise.TaskCostArgs, error) {
	flags := pflag.NewFlagSet("task-cost", pflag.ContinueOnError)
	flags.SetOutput(io.Discard)
	given := addTaskCostFlags(flags)
	if err := flags.Parse(args); err != nil {
		return supervise.TaskCostArgs{}, err
	}
	if flags.NArg() > 0 {
		return supervise.TaskCostArgs{}, fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	return given.Args()
}

// taskCostLog is three lines of a daemon's log, as its JSON handler writes them: a task's value
// written, its take accepted, and its turn started.
const taskCostLog = `{"time":"2026-11-14T09:00:05.000Z","level":"INFO","msg":"supervise: a task value was written","claim":"legion-infra-infra-2811-tester","task":1,"origin":"phase-start","finished":false,"session":"ses_tester","epoch":"2026-11-14T08:00:00Z","revision":7,"cause":"queued"}
{"time":"2026-11-14T09:00:06.000Z","level":"INFO","msg":"supervise: a take was answered","claim":"legion-infra-infra-2811-tester","task":1,"revision":7,"outcome":"accepted","hold":"","repeat":false}
{"time":"2026-11-14T09:00:40.000Z","level":"INFO","msg":"supervise: a turn started","claim":"legion-infra-infra-2811-tester","generation":1,"task":1}
`

// taskCostPushPathLog is four lines of a daemon from before the task value, as its JSON handler
// wrote them: its start, a turn of no cause, a charge of the push path, which names no wait, and
// that turn's end. It holds no task value written and no take answered.
const taskCostPushPathLog = `{"time":"2026-11-14T09:00:01.000Z","level":"INFO","msg":"legion daemon started","project":"INFRA"}
{"time":"2026-11-14T09:01:00.000Z","level":"INFO","msg":"supervise: a turn started","claim":"legion-infra-infra-2811-tester","generation":1}
{"time":"2026-11-14T09:01:05.000Z","level":"WARN","msg":"supervise: prompt failure","claim":"legion-infra-infra-2811-tester","why":"acknowledged, and no turn started within 5s","promptFailures":1,"limit":3}
{"time":"2026-11-14T09:01:35.000Z","level":"INFO","msg":"supervise: a turn ended","claim":"legion-infra-infra-2811-tester","generation":1}
`

// taskCostChargedLog is taskCostLog with a charge of the second wait made after the take's turn
// started, as the daemon's JSON handler writes it: a charge of a worker whose turn began within
// the second bound after its take, which fails the first verdict.
const taskCostChargedLog = taskCostLog + `{"time":"2026-11-14T09:01:00.000Z","level":"WARN","msg":"supervise: prompt failure","claim":"legion-infra-infra-2811-tester","wait":"turn-start","bound":1500000000000,"why":"turn-start: the take of task 1 was accepted and no turn started within 25m0s","promptFailures":1,"limit":3}
`

// taskCost runs `legion task-cost` with stdin piped in, as the documentation site's command
// pipes the daemon's log to it.
func taskCost(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	previous := taskCostStdin
	t.Cleanup(func() { taskCostStdin = previous })
	taskCostStdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), append([]string{"legion", "task-cost"}, args...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// `legion task-cost` prints what the daemon's own reader answers for the window and the claim it
// is given, and nothing of its own: its output is supervise's reading of the same lines, and its
// exit status is 0, or 1 for a reading the reader says does not judge every verdict (a claim no
// line names).
func TestTaskCostPrintsTheDaemonsOwnReading(t *testing.T) {
	for _, args := range [][]string{
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "--claim", "legion-infra-infra-2811-tester"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "--claim", "legion-infra-infra-2811-implementer"},
	} {
		parsed, err := parseTaskCostArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		want, err := supervise.ReadTaskCost(strings.NewReader(taskCostLog), parsed)
		if err != nil {
			t.Fatal(err)
		}
		wantCode := 0
		if !want.EveryVerdictJudged() {
			wantCode = 1
		}
		code, stdout, stderr := taskCost(t, taskCostLog, args...)
		if code != wantCode || stderr != "" || stdout != want.Text() {
			t.Errorf("legion task-cost %q = %d, stdout %q, stderr %q; want %d and the reading %q", args, code, stdout, stderr, wantCode, want.Text())
		}
	}
	if _, stdout, _ := taskCost(t, taskCostLog, "--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z"); !strings.Contains(stdout, "turns started: 1 (by a task number 1, with no cause 0)\n") {
		t.Errorf("legion task-cost over the three lines printed %q, want the turn its task started", stdout)
	}
}

// A window that does not judge every verdict, of the claim asked for, prints "no evidence" for
// each verdict it holds no evidence for, and exits 1. Each of the three verdicts reads "no
// evidence" in a window that holds no daemon line — an empty standard input, as a wrong kubectl
// selector pipes, a window the log does not reach, a claim no line names — and in one whose daemon
// lines hold no task value written, no take answered and no fail carried into the window, as the
// log of a daemon from before the task value does. A window whose daemon lines hold no task value
// written and no take answered, but a fail carried into it — a charge of a worker whose turn began
// within the second bound after a take accepted before the window — judges the first verdict by
// that fail alone, and the other two read "no evidence". Each prints the line under the count of
// its lines that says what it judges, and no verdict as a pass, so a check reading the command
// cannot take a reading of nothing, or of a daemon the change has not reached, for a pass. Exit 2
// stays the usage error's, for arguments the command cannot take.
//
// The negative controls are the windows that hold the take's own lines, which judge every verdict
// and exit 0: the three lines, each verdict a pass, and the same lines with the charge, whose first
// verdict fails and the other two pass, since a fail does not make a judged window exit 1.
func TestTaskCostExitsOneWhenTheWindowHoldsNoDaemonLine(t *testing.T) {
	const noLine = "daemon lines in the window: 0\n"
	const charged = "verdict: no charge of a worker whose turn began within the second bound after its take: fail\n" +
		"  legion-infra-infra-2811-tester task 1: taken 2026-11-14T09:00:06.000Z, a turn started 2026-11-14T09:00:40.000Z, charged 2026-11-14T09:01:00.000Z (wait turn-start, bound 25m0s)\n"
	for _, tc := range []struct {
		name, stdin string
		args        []string
		// says is what the output says of the window's lines.
		says string
		// fails is the verdict a fail carried into the window judges, with what fails it, "" where
		// none does.
		fails string
	}{
		{"an empty standard input", "", []string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z"}, noLine, ""},
		{"a window the log does not reach", taskCostLog, []string{"--from", "2026-11-13T09:00:00Z", "--to", "2026-11-13T10:00:00Z"}, noLine, ""},
		{"a claim no line names", taskCostLog, []string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z",
			"--claim", "legion-infra-infra-2811-implementer"}, noLine, ""},
		{"daemon lines with no task value written and no take answered", taskCostPushPathLog,
			[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z"},
			"daemon lines in the window: 4, the first at 2026-11-14T09:00:01.000Z, the last at 2026-11-14T09:01:35.000Z\n" +
				"task values written and takes answered in the window: 0, so no verdict is judged\n", ""},
		{"daemon lines with no task value written and no take answered, and a fail carried into the window", taskCostChargedLog,
			[]string{"--from", "2026-11-14T09:00:30Z", "--to", "2026-11-14T10:00:00Z"},
			"daemon lines in the window: 2, the first at 2026-11-14T09:00:40.000Z, the last at 2026-11-14T09:01:00.000Z\n" +
				"task values written and takes answered in the window: 0, so only a fail carried into the window is judged\n", charged},
	} {
		code, stdout, stderr := taskCost(t, tc.stdin, tc.args...)
		noEvidence := 3
		if tc.fails != "" {
			noEvidence = 2
		}
		if code != 1 || stderr != "" || !strings.Contains(stdout, tc.says) || !strings.Contains(stdout, tc.fails) ||
			strings.Count(stdout, ": no evidence\n") != noEvidence || strings.Contains(stdout, ": pass\n") {
			t.Errorf("%s: legion task-cost %q = %d, stdout %q, stderr %q; want 1, %q, %d verdicts %q and %q, none a pass",
				tc.name, tc.args, code, stdout, stderr, tc.says, noEvidence, "no evidence", tc.fails)
		}
	}
	for _, tc := range []struct {
		name, stdin string
		// fails is the verdict the window fails, with what fails it, "" where it fails none.
		fails string
	}{
		{"the three lines", taskCostLog, ""},
		{"the three lines and the charge", taskCostChargedLog, charged},
	} {
		code, stdout, stderr := taskCost(t, tc.stdin, "--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z")
		passes := 3
		if tc.fails != "" {
			passes = 2
		}
		if code != 0 || stderr != "" || strings.Contains(stdout, "no evidence") || strings.Contains(stdout, "task values written and takes answered") ||
			!strings.Contains(stdout, tc.fails) || strings.Count(stdout, ": pass\n") != passes {
			t.Errorf("legion task-cost over %s = %d, stdout %q, stderr %q; want 0 and its three verdicts judged, %d a pass and %q",
				tc.name, code, stdout, stderr, passes, tc.fails)
		}
	}
}

// Arguments the reading cannot take are a usage error, exit 2, naming what is wrong and the
// command's help, and no reading is printed; a help flag prints the usage on stdout and exits 0
// without reading the log.
func TestTaskCostRefusesArgumentsItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--from", "2026-11-14T09:00:00Z"}, "--to is required"},
		{[]string{"--from", "an hour ago", "--to", "2026-11-14T10:00:00Z"}, `--from "an hour ago" is not an RFC 3339 time`},
		{[]string{"--from", "2026-11-14T10:00:00Z", "--to", "2026-11-14T09:00:00Z"}, "is not after --from"},
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "daemon.log"}, `unexpected argument "daemon.log"`},
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "--take-timeout", "0"}, "--take-timeout 0 is not a positive number of seconds"},
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "--since", "30m"}, "unknown flag: --since"},
	} {
		code, stdout, stderr := taskCost(t, taskCostLog, tc.args...)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "legion task-cost: ") || !strings.Contains(stderr, tc.want) ||
			!strings.HasSuffix(stderr, "Run 'legion task-cost --help' for usage.\n") {
			t.Errorf("legion task-cost %q = %d, stdout %q, stderr %q; want 2 naming %q and the command's help", tc.args, code, stdout, stderr, tc.want)
		}
	}
	code, stdout, stderr := taskCost(t, "", "--help")
	if code != 0 || stderr != "" ||
		!strings.Contains(stdout, "legion task-cost --from <RFC 3339> --to <RFC 3339> [--claim <token>] [--take-timeout <seconds>]") ||
		!strings.Contains(stdout, "kubectl -n legion logs deploy/legion-<project>-daemon -c legion | legion task-cost") ||
		strings.Contains(stdout, "--since-time") {
		t.Errorf("legion task-cost --help = %d, stdout %q, stderr %q; want 0 and the usage with the documented pipe, which reads the container's whole log", code, stdout, stderr)
	}
	for _, name := range []string{"--from", "--to", "--claim", "--take-timeout"} {
		if !strings.Contains(stdout, "      "+name+" ") {
			t.Errorf("legion task-cost --help lists no %s flag:\n%s", name, stdout)
		}
	}
}

// The first bound the reading judges takes by when --take-timeout is not given is the setting's
// own default: the bound a daemon runs with whose legion.yaml names no
// worker_take_timeout_seconds, as the shipped example names none.
func TestTaskCostTakeTimeoutDefaultsToTheSetting(t *testing.T) {
	defaults, err := configpkg.LoadForValidation(stageShippedExample(t, func(example string) string { return example }), nil)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseTaskCostArgs([]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z"})
	if err != nil || parsed.Take != defaults.WorkerTakeTimeout {
		t.Fatalf("legion task-cost with no --take-timeout judges by %s (%v), want worker_take_timeout_seconds's default %s",
			parsed.Take, err, defaults.WorkerTakeTimeout)
	}
}

// The reading's arguments are the window, both ends RFC 3339 times with the second after the
// first, optionally one claim, and optionally the first bound, a positive number of seconds that
// is the setting's default when not given; nothing else is taken, and a help flag is the command's
// usage.
func TestParseTaskCostArgsReadsTheLogWindow(t *testing.T) {
	from, to := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		args []string
		want supervise.TaskCostArgs
	}{
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z"}, supervise.TaskCostArgs{From: from, To: to, Take: takeBound}},
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z", "--claim", "legion-infra-infra-2811-tester"},
			supervise.TaskCostArgs{From: from, To: to, Claim: "legion-infra-infra-2811-tester", Take: takeBound}},
		{[]string{"--to=2026-11-14T10:30:00+01:00", "--from=2026-11-14T09:00:00.000Z", "--take-timeout=240"},
			supervise.TaskCostArgs{From: from, To: to, Take: 4 * time.Minute}},
	} {
		got, err := parseTaskCostArgs(tc.args)
		if err != nil || !got.From.Equal(tc.want.From) || !got.To.Equal(tc.want.To) || got.Claim != tc.want.Claim || got.Take != tc.want.Take {
			t.Errorf("parseTaskCostArgs(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"--to", "2026-11-14T09:30:00Z"},
		{"--from", "2026-11-14T09:00:00Z"},
		{"--from", "yesterday", "--to", "2026-11-14T09:30:00Z"},
		{"--from", "2026-11-14T09:30:00Z", "--to", "2026-11-14T09:30:00Z"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z", "daemon.log"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z", "--take-timeout", "0"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z", "--take-timeout", "-300"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z", "--take-timeout", "5m"},
		{"--since", "30m"},
	} {
		if got, err := parseTaskCostArgs(args); err == nil || errors.Is(err, pflag.ErrHelp) {
			t.Errorf("parseTaskCostArgs(%q) = %+v, %v; want a refusal", args, got, err)
		}
	}
	for _, help := range []string{"-h", "-help", "--help"} {
		if _, err := parseTaskCostArgs([]string{help}); !errors.Is(err, pflag.ErrHelp) {
			t.Errorf("parseTaskCostArgs(%q) = %v, want pflag.ErrHelp", help, err)
		}
	}
}

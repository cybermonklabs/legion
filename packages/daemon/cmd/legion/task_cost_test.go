package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// taskCostLog is three lines of a daemon's log, as its JSON handler writes them: a task's value
// written, its take accepted, and its turn started.
const taskCostLog = `{"time":"2026-11-14T09:00:05.000Z","level":"INFO","msg":"supervise: a task value was written","claim":"legion-infra-infra-2811-tester","task":1,"origin":"phase-start","finished":false,"session":"ses_tester","epoch":"2026-11-14T08:00:00Z","revision":7,"cause":"queued"}
{"time":"2026-11-14T09:00:06.000Z","level":"INFO","msg":"supervise: a take was answered","claim":"legion-infra-infra-2811-tester","task":1,"revision":7,"outcome":"accepted","hold":"","repeat":false}
{"time":"2026-11-14T09:00:40.000Z","level":"INFO","msg":"supervise: a turn started","claim":"legion-infra-infra-2811-tester","generation":1,"task":1}
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
// is given, and nothing of its own: its output is supervise's reading of the same lines.
func TestTaskCostPrintsTheDaemonsOwnReading(t *testing.T) {
	for _, args := range [][]string{
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "--claim", "legion-infra-infra-2811-tester"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "--claim", "legion-infra-infra-2811-implementer"},
	} {
		parsed, err := supervise.ParseTaskCostArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		want, err := supervise.ReadTaskCost(strings.NewReader(taskCostLog), parsed.From, parsed.To, parsed.Claim)
		if err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := taskCost(t, taskCostLog, args...)
		if code != 0 || stderr != "" || stdout != want.Text() {
			t.Errorf("legion task-cost %q = %d, stdout %q, stderr %q; want 0 and the reading %q", args, code, stdout, stderr, want.Text())
		}
	}
	if _, stdout, _ := taskCost(t, taskCostLog, "--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z"); !strings.Contains(stdout, "turns started: 1 (by a task number 1, with no cause 0)\n") {
		t.Errorf("legion task-cost over the three lines printed %q, want the turn its task started", stdout)
	}
}

// Arguments the reading cannot take are a usage error, exit 2, naming what is wrong beside the
// usage, and no reading is printed; a help flag prints the usage and exits 0 without reading the
// log.
func TestTaskCostRefusesArgumentsItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--from", "2026-11-14T09:00:00Z"}, "--to is required"},
		{[]string{"--from", "an hour ago", "--to", "2026-11-14T10:00:00Z"}, `--from "an hour ago" is not an RFC 3339 time`},
		{[]string{"--from", "2026-11-14T10:00:00Z", "--to", "2026-11-14T09:00:00Z"}, "is not after --from"},
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T10:00:00Z", "daemon.log"}, `unexpected argument "daemon.log"`},
	} {
		code, stdout, stderr := taskCost(t, taskCostLog, tc.args...)
		if code != 2 || stdout != "" || !strings.HasPrefix(stderr, "legion task-cost: ") || !strings.Contains(stderr, tc.want) ||
			!strings.Contains(stderr, "usage: legion task-cost ") {
			t.Errorf("legion task-cost %q = %d, stdout %q, stderr %q; want 2 naming %q beside the usage", tc.args, code, stdout, stderr, tc.want)
		}
	}
	code, stdout, stderr := taskCost(t, "", "--help")
	if code != 0 || stdout != "" || !usageOf(stderr, "task-cost") ||
		!strings.Contains(stderr, "kubectl -n legion logs deploy/legion-<project>-daemon -c legion --since-time <from> | legion task-cost") {
		t.Errorf("legion task-cost --help = %d, stdout %q, stderr %q; want 0 and the usage with the documented pipe", code, stdout, stderr)
	}
}

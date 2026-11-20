// The check of the step records live driver's read of `dispatch steps` (parseSteps and holdsTen,
// step_records_live_test.go). It is a file of its own so that it runs in the daemon-go job with
// the package's other tests, while the daemon-live job takes step_records_live_test.go whole and
// runs its one test by name (DAEMON_LIVE_FILES in .github/scripts/ci_job_reads.py).
package daemon

import (
	"fmt"
	"strings"
	"testing"
)

// The driver's end check, which the proof runs over what `dispatch steps` prints: a line ends its
// run only when it prints a moment as its end. steps.ts prints `running` there, and as the result,
// for a run that has not ended; `-`, which a printer that wrote the end through orDash would print,
// and any other word are no end either, so each fails the walk's read, naming the line. The control
// is the same ten lines with every end a moment, which the check passes.
func TestTheDriverReadsAnEndOnlyFromAMoment(t *testing.T) {
	const key = "SR-1"
	printed := func(end, result string) string {
		lines := []string{key + ": 10 steps in 1 tasks"}
		for _, w := range stepRecordsTen {
			model, lineEnd, lineResult := "-", "2026-11-20T09:40:00.123456Z", w.result
			if w.kind == "step" {
				model = stepRecordsModel
			}
			if w.kind == "step" && w.step == 2 && w.attempt == 2 {
				lineEnd, lineResult = end, result
			}
			lines = append(lines, fmt.Sprintf("default %d %s %s %s attempt %d agent-%s %s 2026-11-20T09:00:00Z %s %s",
				w.step, w.kind, w.role, printedType(w), w.attempt, w.role, model, lineEnd, lineResult))
		}
		return strings.Join(lines, "\n") + "\n"
	}
	read := func(out string) error {
		parsed, err := parseSteps(key, out)
		if err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		return holdsTen(parsed)
	}
	if err := read(printed("2026-11-20T09:40:00Z", "passed")); err != nil {
		t.Fatalf("control: the ten lines, each ended at a moment, read as %v, want the walk's ten", err)
	}
	for _, no := range []struct{ end, result string }{{"running", "running"}, {"-", "passed"}, {"unknown", "passed"}} {
		err := read(printed(no.end, no.result))
		if err == nil || !strings.Contains(err.Error(), "2 step implementer implementation attempt 2") || !strings.Contains(err.Error(), "an end") {
			t.Errorf("the ten lines with implementing's second run ending %q %q read as %v, want an error naming that run and its end", no.end, no.result, err)
		}
	}
}

// packages/envoy/internal/cipher/judge/judge_write_test.go
// Round 5's minors on cybermonklabs/legion#4135, on the judge's write path: what a run logs when
// its approval does not answer — the broker stopping under it, a COMMIT whose answer was lost after
// it applied, and one where whether it applied could not be read.
package judge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/cipher/requests"
)

// capturedLog is every line slog writes while f runs, at every level.
func capturedLog(t *testing.T, f func()) []string {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	f()
	return strings.Split(strings.TrimSpace(logs.String()), "\n")
}

// lineWith is the first of lines holding every one of words, or "".
func lineWith(lines []string, words ...string) string {
	for _, l := range lines {
		found := true
		for _, w := range words {
			found = found && strings.Contains(l, w)
		}
		if found {
			return l
		}
	}
	return ""
}

// TestAShutdownDuringTheApprovalIsNoRefusal: the broker stopping while the judge's approval is in
// its transaction ends it with context.Canceled, which is the broker going away, not the state
// machine refusing the judge. It is logged as that, at INFO, and never as the ERROR "refused"; the
// verdict's row is still written, on its own context, saying the approval was not applied.
func TestAShutdownDuringTheApprovalIsNoRefusal(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	machine.commitTakes = 3 * time.Second
	runner := New(t.Context(), testOptions(t, dispatch, model, machine))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stop := time.AfterFunc(200*time.Millisecond, cancel)
	defer stop.Stop()

	lines := capturedLog(t, func() {
		runner.write(ctx, pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})
	})
	if line := lineWith(lines, "record=record-1", "the broker stopped"); !strings.Contains(line, "level=INFO") {
		t.Errorf("the approval's line is %q; want an INFO line saying the broker stopped under it (lines: %q)", line, lines)
	}
	if line := lineWith(lines, "refused"); line != "" {
		t.Errorf("a line says %q; want no refusal: the broker stopped, nothing refused the judge", line)
	}
	if applied, logged := machine.rows(); len(applied) != 0 || len(logged) != 1 || logged[0]["applied"] != false {
		t.Errorf("the run applied %d approvals and logged %+v; want the row alone, applied false", len(applied), logged)
	}
}

// TestAnApprovalWhoseAnswerWasLostWritesNoSecondRow: an approval whose COMMIT applied and whose
// answer was lost errors here, and LogJudged then finds the approval's own row and writes nothing
// (requests.JudgedAlreadyApplied). The run says the approval applied, at INFO, and no line calls
// it refused or not applied. Where LogJudged could not read whether it applied
// (requests.JudgedAppliedUnknown), the run says so at WARN and its verdict line reads applied
// unknown, never false.
func TestAnApprovalWhoseAnswerWasLostWritesNoSecondRow(t *testing.T) {
	for _, c := range []struct {
		wrote       requests.JudgedWrite
		level, says string
		verdictLine string
	}{
		{requests.JudgedAlreadyApplied, "level=INFO", "its answer was lost", ""},
		{requests.JudgedAppliedUnknown, "level=WARN", "could not be read", "applied=" + requests.AppliedUnknown},
	} {
		t.Run(string(c.wrote), func(t *testing.T) {
			dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
			machine.applyErr = errors.New("unexpected EOF")
			machine.wrote = c.wrote
			runner := New(t.Context(), testOptions(t, dispatch, model, machine))
			lines := capturedLog(t, func() {
				runner.write(t.Context(), pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})
			})
			if line := lineWith(lines, "record=record-1", c.says); !strings.Contains(line, c.level) {
				t.Errorf("the approval's line is %q; want a %s line saying %q (lines: %q)", line, c.level, c.says, lines)
			}
			if line := lineWith(lines, "refused"); line != "" {
				t.Errorf("a line says %q; want no refusal", line)
			}
			verdict := lineWith(lines, "wrote a verdict")
			switch {
			case c.verdictLine == "" && verdict != "":
				t.Errorf("the run logged %q; want no verdict line: it wrote no row", verdict)
			case c.verdictLine != "" && !strings.Contains(verdict, c.verdictLine):
				t.Errorf("the verdict line is %q; want it to read %s", verdict, c.verdictLine)
			}
		})
	}
}

// TestAnApprovalWhoseCommitGotNoAnswerIsNoRefusal is the tester's minor on cybermonklabs/legion#4135
// at 032301a2c4: a judge approval whose COMMIT the database never answered — its connection lost
// mid-commit — is no refusal, and the row LogJudged then wrote says whether it applied. One that
// did not apply is logged at WARN as that, and never as the ERROR "refused".
func TestAnApprovalWhoseCommitGotNoAnswerIsNoRefusal(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	machine.applyErr = fmt.Errorf("%w: conn closed", requests.ErrCommitUncertain)
	runner := New(t.Context(), testOptions(t, dispatch, model, machine))
	lines := capturedLog(t, func() {
		runner.write(t.Context(), pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})
	})
	if line := lineWith(lines, "record=record-1", "COMMIT"); !strings.Contains(line, "level=WARN") {
		t.Errorf("the approval's line is %q; want a WARN line saying its COMMIT got no answer (lines: %q)", line, lines)
	}
	if line := lineWith(lines, "refused"); line != "" {
		t.Errorf("a line says %q; want no refusal: nothing refused the judge, its COMMIT got no answer", line)
	}
	if _, logged := machine.rows(); len(logged) != 1 {
		t.Errorf("the run logged %d rows; want the verdict's row", len(logged))
	}
}

// TestAnUnexpectedRefusalDuringAStopIsStillAnError is round 6's m-e on cybermonklabs/legion#4135:
// a refusal the judge should not have reached is an ERROR even when the broker stops while the
// approval runs. Only the approval's own answer says whether the stop cut it (context.Canceled in
// that error); the run's context being cancelled says nothing of why this approval failed.
func TestAnUnexpectedRefusalDuringAStopIsStillAnError(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	machine.onApply = cancel
	machine.applyErr = errors.New("the state machine refused the approval for a reason the judge never meets")
	runner := New(t.Context(), testOptions(t, dispatch, model, machine))
	lines := capturedLog(t, func() {
		runner.write(ctx, pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})
	})
	if line := lineWith(lines, "record=record-1", "refused"); !strings.Contains(line, "level=ERROR") {
		t.Errorf("the approval's line is %q; want the ERROR \"refused\": the stop did not cause it (lines: %q)", line, lines)
	}
	if line := lineWith(lines, "the broker stopped"); line != "" {
		t.Errorf("a line says %q; want none: the approval's own error names no stop", line)
	}
}

// TestTheVerdictLineWaitsForTheRowItNames is round 6's m-e on cybermonklabs/legion#4135: an
// approval's not-applied line describes the row LogJudged wrote, so a row that could not be written
// gets the one ERROR saying so, with the approval's error beside it, and no line that claims a row.
func TestTheVerdictLineWaitsForTheRowItNames(t *testing.T) {
	dispatch, model, machine := newDispatch(t, "doing", testSpec), newModel(t, verdictReply(Grant, "yes")), newRequests()
	machine.applyErr = fmt.Errorf("%w: unexpected EOF", requests.ErrCommitUncertain)
	machine.logErr = errors.New("the database refused the row")
	runner := New(t.Context(), testOptions(t, dispatch, model, machine))
	lines := capturedLog(t, func() {
		runner.write(t.Context(), pending(), verdictRow{asked: true, verdict: Verdict{Word: Grant, Reason: "yes"}})
	})
	if line := lineWith(lines, "record=record-1", "could not be written"); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "not_applied=") {
		t.Errorf("the line is %q; want the ERROR that the verdict could not be written, naming the approval's error", line)
	}
	if line := lineWith(lines, "its own row shows"); line != "" {
		t.Errorf("a line says %q; want none: no row was written to show anything", line)
	}
}

package supervise

import (
	"strconv"
	"strings"
	"testing"
)

// The cases of this file are the provider-error rule of the forward merge that brought
// dispatch://INFRA-4508 onto the take (dispatch://INFRA-2811): every turn-outcome report that
// carries a provider error is judged once, in the claim's one run of provider-error turns, by the
// take's ladder where the report names the take's task (reportedTask, providerErrored), and as a
// turn that was not the take's where it names no task or is marked unprompted
// (unpromptedTurnEnded); a clean report ends that run whoever ran the turn (turnOutcome,
// providerErrorRunOver). Each case is named so that c2's `-run 'Take'` selects it.

// promptRetryArmed is whether the ladder's wait stands for any task of the claim.
func (h *harness) promptRetryArmed() bool {
	h.t.Helper()
	_, armed := h.m.timers[TimerPromptRetry]
	return armed
}

// A report that names the take's task and carries a provider error is the take's turn reported
// over: the turn counts in the claim's run of provider-error turns, and a status that can clear
// puts the task on the ladder under its number, its take gone, to be written again for the same
// session after the ladder's wait, while a status that cannot clear fails the claim at once with
// the task kept for its architect, and the turn-outcome answer says the report failed it.
func TestTakeReportWithAProviderErrorGoesToTheLadderInTheOneRun(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		fails  bool
	}{
		{"a status that can clear", 529, false},
		{"a status that cannot clear", 400, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := workingClaim(t)
			number := h.pending().Number
			h.mustQuiet(StreamTurnEnd{Claim: h.token})

			answer, err := h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(),
				Sequence: 1, Refused: true, Task: number, ProviderError: &ProviderError{Status: tc.status, Message: "refused"}})
			h.m.Wait()

			if err != nil || answer.Failed != tc.fails {
				t.Fatalf("the take's report answered %+v, %v; want failed %t", answer, err, tc.fails)
			}
			c := h.claim()
			if c.ProviderErrorRun != 1 {
				t.Errorf("the run of provider errors is %d, want the take's turn counted once", c.ProviderErrorRun)
			}
			if c.Pending == nil || c.Pending.Number != number {
				t.Fatalf("the claim holds %+v, want task %d kept under its number", c.Pending, number)
			}
			if tc.fails {
				if c.State != StateFailed || c.FailureCause != CauseProviderError ||
					!strings.Contains(c.Failure, "cannot clear ended this claim's turn") || !strings.Contains(c.Failure, "HTTP 400") {
					t.Fatalf("the claim is %s (%q, %q), want it failed by the take's turn's 400", c.State, c.FailureCause, c.Failure)
				}
				if h.promptRetryArmed() {
					t.Error("the ladder's wait is armed for a status that cannot clear, want nothing written again")
				}
				return
			}
			if c.State == StateFailed || !c.Pending.ConfirmedAt.IsZero() || c.Pending.ProviderErrorRetries != 1 || !h.promptRetryArmed() {
				t.Fatalf("the claim is %s holding %+v, ladder armed %t; want the task taken back onto the ladder's first step",
					c.State, c.Pending, h.promptRetryArmed())
			}
			if message := h.climbs(1, promptRetryDelay(1)); !strings.HasSuffix(message, providerErrorTaskText) {
				t.Fatalf("the ladder's take was answered %q, want the task again", message)
			}
			if got := h.pending().Number; got != number {
				t.Fatalf("the task written again is %d, want %d under its own number", got, number)
			}
		})
	}
}

// A report of a turn that was not the take's — one marked unprompted, as a plugin from before the
// task's number sends it with `httpStatus`, and one that names no task, as a current plugin sends
// it with the status in its provider error — is judged from that report alone: it counts in the
// claim's run of provider-error turns and writes no task again, since that turn carried none; a
// status that can clear fails nothing, and one that cannot fails the claim at once, with the
// turn-outcome answer saying this report failed it (main's
// TestAPluginTurnRefusedWithA400FailsTheClaimAtOnce and its siblings hold the marked form at
// more statuses).
func TestTakeOfNoTaskATurnsRefusedReportIsCountedAndWritesNothingAgain(t *testing.T) {
	for _, form := range []struct {
		name   string
		report func(h *harness, status int) RequestTurnOutcome
	}{
		{"marked unprompted", func(h *harness, status int) RequestTurnOutcome {
			return RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence + 1, Refused: true,
				HTTPStatus: status, Unprompted: true}
		}},
		{"naming no task", func(h *harness, status int) RequestTurnOutcome {
			return RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: h.worker.sequence + 1, Refused: true,
				ProviderError: &ProviderError{Status: status, Message: "refused"}}
		}},
	} {
		for _, tc := range []struct {
			status int
			fails  bool
		}{{529, false}, {400, true}} {
			t.Run(form.name+" "+strconv.Itoa(tc.status), func(t *testing.T) {
				h := idleClaim(t)

				answer, err := h.m.HandleTurnOutcome(h.ctx, form.report(h, tc.status))
				h.m.Wait()

				if err != nil || answer.Failed != tc.fails {
					t.Fatalf("the report answered %+v, %v; want failed %t", answer, err, tc.fails)
				}
				c := h.claim()
				if c.ProviderErrorRun != 1 {
					t.Errorf("the run of provider errors is %d, want the turn counted once", c.ProviderErrorRun)
				}
				if c.Pending != nil || h.promptRetryArmed() {
					t.Errorf("the claim holds %+v with the ladder armed %t, want no task written again: the turn carried none", c.Pending, h.promptRetryArmed())
				}
				if !tc.fails {
					if c.State != StateIdle || c.Failure != "" {
						t.Fatalf("the claim is %s (%q), want it idle: a status that can clear fails nothing", c.State, c.Failure)
					}
					return
				}
				if c.State != StateFailed || c.FailureCause != CauseProviderError ||
					!strings.Contains(c.Failure, "that ran none of the daemon's tasks") || !strings.Contains(c.Failure, "HTTP 400") {
					t.Fatalf("the claim is %s (%q, %q), want it failed by the 400 of a turn that was not the take's", c.State, c.FailureCause, c.Failure)
				}
			})
		}
	}
}

// A report that names the take's task and is marked unprompted is read as naming no task (the
// architect's precedence, turnOutcome): its provider error is judged as that of a turn that was
// not the take's (unpromptedTurnEnded), not by the ladder. A status that can clear is counted in
// the run and takes nothing back: the take stands, no ladder step is made and no wait is armed,
// and the window keeps the report as one of no task. A status that cannot clear fails the claim
// with the words for a turn that ran no task of the daemon's, and the answer says this report
// failed it.
func TestTakeTaskNamedByAReportMarkedUnpromptedIsJudgedAsATurnOfNoTask(t *testing.T) {
	for _, tc := range []struct {
		status int
		fails  bool
	}{{529, false}, {400, true}} {
		t.Run(strconv.Itoa(tc.status), func(t *testing.T) {
			h := workingClaim(t)
			number := h.pending().Number

			answer, err := h.m.HandleTurnOutcome(h.ctx, RequestTurnOutcome{Claim: h.token, Generation: h.generation(),
				Sequence: 1, Refused: true, Task: number, HTTPStatus: tc.status, Unprompted: true})
			h.m.Wait()

			if err != nil || answer.Failed != tc.fails {
				t.Fatalf("the report answered %+v, %v; want failed %t", answer, err, tc.fails)
			}
			c := h.claim()
			if c.ProviderErrorRun != 1 {
				t.Errorf("the run of provider errors is %d, want the turn counted once", c.ProviderErrorRun)
			}
			if kept := c.TurnOutcomes; len(kept) != 1 || kept[0].Task != 0 {
				t.Errorf("the window keeps %+v, want the report kept as one of no task", kept)
			}
			if tc.fails {
				if c.State != StateFailed || !strings.Contains(c.Failure, "that ran none of the daemon's tasks") || !strings.Contains(c.Failure, "HTTP 400") {
					t.Fatalf("the claim is %s (%q), want it failed with the words of a turn that was not the take's", c.State, c.Failure)
				}
				return
			}
			if c.State == StateFailed || c.Pending == nil || c.Pending.Number != number || c.Pending.ConfirmedAt.IsZero() ||
				c.Pending.ProviderErrorRetries != 0 || h.promptRetryArmed() {
				t.Fatalf("the claim is %s holding %+v, ladder armed %t; want the take standing and no ladder step",
					c.State, c.Pending, h.promptRetryArmed())
			}
		})
	}
}

// A clean report ends the claim's run of provider-error turns whoever ran the turn: the take's own
// end report, which finishes its task, and the report of a turn of no task, which finishes
// nothing and leaves the take standing.
func TestTakeCleanReportEndsTheRunOfProviderErrorsWhoeverRanTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ownTask  bool
		finishes bool
	}{
		{"the take's own end report", true, true},
		{"a report of a turn of no task", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := workingClaim(t)
			h.refusedTurn(1, 529, "529 overloaded_error: Overloaded")
			h.climbs(1, promptRetryDelay(1))
			if got := h.claim().ProviderErrorRun; got != 1 {
				t.Fatalf("the run of provider errors is %d, want 1 before the clean report", got)
			}
			number := h.pending().Number
			report := RequestTurnOutcome{Claim: h.token, Generation: h.generation(), Sequence: 2}
			if tc.ownTask {
				report.Task = number
				h.mustQuiet(StreamTurnEnd{Claim: h.token})
			}

			h.must(report)

			c := h.claim()
			if c.ProviderErrorRun != 0 {
				t.Fatalf("the run of provider errors is %d after a clean report, want 0", c.ProviderErrorRun)
			}
			if stored := h.store.load(testToken).ProviderErrorRun; stored != 0 {
				t.Errorf("the stored run of provider errors is %d, want 0", stored)
			}
			if finished := c.Pending == nil; finished != tc.finishes {
				t.Fatalf("the claim holds %+v after the clean report, want the task finished %t", c.Pending, tc.finishes)
			}
		})
	}
}

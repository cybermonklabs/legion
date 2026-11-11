package api

import "github.com/sjawhar/legion/daemon/internal/supervise"

// UpdateState is where an update round stands on one claim, as that claim's view names it
// (ClaimView.UpdateState). UpdateStateOf reads it from the claim's own update columns, and it is
// the one place that reads them: a reader that spells the same reading out of `updateRound` and
// `updateHead` cannot tell a worker that answered with no head from one nobody asked, which is
// the misreading dispatch://INFRA-2694 reports.
type UpdateState string

const (
	// UpdateStateNone is a claim no round is open over. The round's end clears every update
	// column of the claim (supervise's clearUpdate), so this is also every claim whose last
	// round ended.
	UpdateStateNone UpdateState = "none"
	// UpdateStateOpen is a round open over this claim whose task the daemon has not sent yet,
	// and a round whose ask a turn took back. The take-back clears the ask's own clock
	// (supervise's takeBackSignal), so the next post of the round asks again at once.
	UpdateStateOpen UpdateState = "open"
	// UpdateStateAsked is a round that sent this claim its task and waits for the answer.
	UpdateStateAsked UpdateState = "asked"
	// UpdateStatePushCheckReask is the round that asked this claim's worker a second time
	// because the pod's shim could not run the daemon's push check, and that has not sent the
	// second task yet (supervise.Claim.UpdateReaskedForPushCheck, internal/daemon's
	// checkUnpushed). It is the one history the columns tell apart: the mark stands and the ask
	// clock is empty, which no other way of reaching an open round leaves behind.
	UpdateStatePushCheckReask UpdateState = "push-check-reask"
	// UpdateStateAnswered is a claim whose worker answered the round that is open over it. The
	// daemon holds the claim's next task from this moment, and replaces the process once every
	// claim of the round has answered.
	UpdateStateAnswered UpdateState = "answered"
)

// UpdateStateOf is the state of the update round that stands over c, one of the five above.
//
// The order of the questions is the rule. Asked in another order the same columns give other
// answers:
//
//   - An answer stands wherever UpdateSignalledAt is set, whatever the ask clock holds. A worker
//     may answer after a take-back or a push-check re-ask cleared UpdateTaskedAt, so the answer
//     is asked for first and the clock decides nothing after it.
//   - A claim with no round is `none`. It is asked after the answer, so a claim that somehow
//     holds an answer without a round still reads `answered` rather than `none`.
//   - A standing push-check mark with no ask clock is `push-check-reask`: the round means to ask
//     this worker a second time and has not sent that task yet.
//   - An ask clock is `asked`.
//   - Everything left is `open`.
//
// A claim that answered and reported no head reads `answered` like any other. That is the case
// `updateHead` alone cannot tell from a claim nobody ever asked, because a planner before its
// first push answers with an empty head (supervise.Claim.UpdateHead).
func UpdateStateOf(c supervise.Claim) UpdateState {
	switch {
	case c.UpdateSignalledAt != nil:
		return UpdateStateAnswered
	case c.UpdateRound == "":
		return UpdateStateNone
	case c.UpdateReaskedForPushCheck && c.UpdateTaskedAt == nil:
		return UpdateStatePushCheckReask
	case c.UpdateTaskedAt != nil:
		return UpdateStateAsked
	default:
		return UpdateStateOpen
	}
}

// ReaskedForPushCheck is whether this claim's open round has already asked its worker a second
// time because the pod's shim could not run the daemon's push check
// (supervise.Claim.UpdateReaskedForPushCheck).
//
// It is the mark itself, and a report carries it beside whatever state the claim has reached.
// The mark stands to the end of the round and the state moves on without it: a claim the daemon
// re-asked and then asked again reads `asked` with the mark set, and one that then answers reads
// `answered` with the mark set. Neither is a claim the daemon still waits on for a second ask.
// The round's end clears the column with the round (supervise's clearUpdate), so this is false
// wherever UpdateStateOf answers `none`.
func ReaskedForPushCheck(c supervise.Claim) bool { return c.UpdateReaskedForPushCheck }

// UnpromptedTurnTakesAnswerBack is whether a turn the daemon sent nothing for would take this
// claim's standing answer back — an Envoy notice delivered into the session, a person's steer, or
// a background result (supervise's takeBackSignal, which decides it for real).
//
// The turn this answers for is a turn the daemon sent nothing for, and no other turn. Two turns
// the daemon did send are no part of the question. The round's own task's turn is the turn the
// worker answers in, and takeBackSignal keeps the answer through it. A turn of some other task
// the daemon sent takes the answer back whatever the marks hold, and takeBackSignal decides that
// turn by the delivery rather than by anything reported here.
//
// It answers takeBackSignal's own rule for such a turn, by the three exits that keep the answer
// and the one that takes it back:
//
//   - false where no round is open, or where no answer stands: there is nothing to take back;
//   - false where the claim left its round's pod (supervise.Claim.LeftPodRound), since no turn
//     of the pod that replaced it can move the branch the dead pod held;
//   - false where the standing answer was given by the end of the task's own turn
//     (supervise.Claim.UpdateAnsweredByTurnEnd), since such a worker's plugin reminds it on its
//     own and the daemon cannot tell that reminder's turn from a notice;
//   - true otherwise, which is an answer the worker gave with the ready_for_update call.
func UnpromptedTurnTakesAnswerBack(c supervise.Claim) bool {
	if c.UpdateRound == "" || c.UpdateSignalledAt == nil {
		return false
	}
	if c.LeftPodRound(c.UpdateRound) {
		return false
	}
	return !c.UpdateAnsweredByTurnEnd
}

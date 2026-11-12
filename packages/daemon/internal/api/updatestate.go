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

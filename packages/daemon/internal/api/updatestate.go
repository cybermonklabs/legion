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
	// UpdateStateOpen is a round open over this claim whose ask has reached no session of the
	// claim's role yet. The sweep asks it again until one does, while the claim has not failed
	// and is still in the round's pod (the daemon's unaskedClaims); a failed claim, or one that
	// left the pod, keeps the round and reads `open` until the round ends.
	UpdateStateOpen UpdateState = "open"
	// UpdateStateAsked is a round whose ask reached a session of this claim's role, and which
	// waits for the answer. UpdateTaskedAt marks that delivery (supervise's
	// Machine.PublishUpdateAsk, which the outbox's update-ask row calls and which marks the claim
	// only for a publish a session of the role took).
	UpdateStateAsked UpdateState = "asked"
	// UpdateStateAnswered is a claim whose worker answered the round that is open over it. The
	// daemon holds the claim's next task from this moment. The round ends when every claim of
	// the round has answered and no turn runs, or at its bound, whichever comes first. Then the
	// daemon replaces the process.
	UpdateStateAnswered UpdateState = "answered"
)

// UpdateStateOf is the state of the update round that stands over c, one of the four above.
//
// The order of the questions is the rule. Asked in another order the same columns give other
// answers:
//
//   - An answer stands wherever UpdateSignalledAt is set, whatever the ask clock holds. A worker
//     may answer before the round's ask reaches a session of its role, with UpdateTaskedAt still
//     empty, so the answer is asked for first and the clock decides nothing after it.
//   - A claim with no round is `none`. It is asked after the answer, so a claim that somehow
//     holds an answer without a round still reads `answered` rather than `none`.
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
	case c.UpdateTaskedAt != nil:
		return UpdateStateAsked
	default:
		return UpdateStateOpen
	}
}

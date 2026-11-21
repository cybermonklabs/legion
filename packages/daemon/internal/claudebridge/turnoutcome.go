package claudebridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// turnOutcomePath is the route each turn's outcome is reported on.
const turnOutcomePath = "/legion/v1/claims/turn-outcome"

// turnReports is the bridge as the Claude Code backend's producer of turn outcomes: every turn the
// bridge ends is reported to the daemon, clean or refused, with the bridge's own sequence within
// its launch generation (POST /legion/v1/claims/turn-outcome). The daemon derives the claim's run
// of refused turns from those reports and fails the claim once the run reaches its launch failure
// limit, which is the fault that left an implementer's every turn refused by its provider with its
// claim never failing (dispatch://INFRA-714). A report also names the task whose turn it was, which
// is what finishes that task, and how the provider failed the turn, which keeps the task under its
// number for the provider-error ladder instead (supervise's reportedTask).
//
// The bridge is the producer because it is what sees the refusal: a step Claude Code ends with an
// error is the assistant message it writes with stopReason "error" (result), and the pane's own
// Claude Code hook for a stopped turn is not measured to fire in a pod
// (packages/claude-envoy/src/legion-phase-hook.ts). It names the claim with the launch's boot
// token (daemonLink).
//
// For a worker on the current contract only this report finishes a task, so it is sent until the
// daemon answers it (daemonLink.untilAnswered), as Oh My Pi's reporter sends its own
// (packages/pi-legion/src/turn-outcome.ts): every failure daemonLink.unanswered names, any failure
// of an attempt that got no status among them, whatever the transport names it, and also any 500
// the route answers, which is the daemon's failure to record the report (recordFailed). No list of
// the transport's failures decides it: a report a failure stopped would leave its task's turn
// started on the daemon, which arms no bound over it, while the loop holds the number finished
// from the turn's end, so nothing would finish the task or take it again until a newer task or the
// process's end. The daemon stores one outcome per (generation, sequence) and applies a repeat
// once, so a report sent again whose first attempt landed changes nothing, and one whose write
// failed is applied. A 2xx ends the sending, and so does a refusal; a 4xx applied nothing, so the
// task a clean report named is still owed, and the loop is told (refused) to hold that number
// finished no longer (take.go's reportRefused).
//
// Each report is sent from a goroutine of its own and nothing waits for it: the bridge's loop must
// stay answerable to the shim, to Claude Code and to its own end at every moment. The sending
// stops once Run returns (ctx): a report the bridge's end cuts short is a turn the daemon hears of
// through the process's death instead, which takes its task back, and the bridge is torn down only
// when its pane is ending.
type turnReports struct {
	daemon *daemonLink
	ctx    context.Context
	// refused tells the loop the number of the task whose clean report the daemon refused with a
	// 4xx.
	refused func(task uint64)
	// sequence is the bridge's own count of the turns it has ended at generation, from 1. It is
	// atomic because the posts run off the loop, though only the loop ever takes a number.
	sequence atomic.Uint64
}

// newTurnReports is the reporter for the daemon d, sending until ctx ends and telling refused of
// a clean task report the daemon refused, or nil for a pane that names no daemon.
func newTurnReports(ctx context.Context, d *daemonLink, refused func(task uint64)) *turnReports {
	if d == nil {
		return nil
	}
	return &turnReports{daemon: d, ctx: ctx, refused: refused}
}

// turnEnded reports the turn the bridge has just ended: the task whose turn it was, 0 for a turn of
// no task; held, the number of the take the daemon accepted during that turn whose text the
// bridge holds unrun for a turn of its own (bridge.held), 0 for none, which tells the daemon
// for certain that this turn was not that take's (claim.TurnOutcomeRequest.Held); and refused,
// with how the provider failed it, when a step of it failed, and clean otherwise. It takes the
// next sequence and returns at once.
func (r *turnReports) turnEnded(task, held uint64, refusal *claim.ProviderError) {
	if r == nil {
		return
	}
	request := claim.TurnOutcomeRequest{
		BootCaller: claim.BootCaller{BootToken: r.daemon.bootToken},
		Generation: r.daemon.generation,
		Sequence:   r.sequence.Add(1),
		Outcome:    claim.TurnClean,
		Task:       task,
		Held:       held,
	}
	if refusal != nil {
		request.Outcome, request.Message, request.ProviderError = claim.TurnRefused, refusal.Message, refusal
	}
	go r.post(request)
}

// recordFailed is the turn-outcome route's own failure to record a report (api's claimFailure:
// 500 `turn outcome failed: the daemon could not record it`, with no code), which is sent again as
// the failures daemonLink.unanswered names are (the plan's decision 8).
func recordFailed(status int) string {
	if status != http.StatusInternalServerError {
		return ""
	}
	return "was answered 500, the daemon's failure to record it"
}

// post sends one report until the daemon answers it, and logs what the daemon made of it: the run
// of refused turns the claim's reports now end with, and whether this one failed the claim, which
// is the one place the pane shows the count being made. A report the daemon refuses is logged, and
// a 4xx refusal of a clean report naming a task is told to the loop (refused): that report applied
// nothing, so the claim still owes the task.
func (r *turnReports) post(request claim.TurnOutcomeRequest) {
	logger := r.daemon.log
	status, body, err := r.daemon.untilAnswered(r.ctx, fmt.Sprintf("claims/turn-outcome of turn %d", request.Sequence),
		turnOutcomePath, request, recordFailed)
	var decoded claim.TurnOutcomeResponse
	switch {
	case r.ctx.Err() != nil:
	case err != nil:
		logger.Printf("could not report the outcome of turn %d (%s): %v", request.Sequence, request.Outcome, err)
	case status/100 != 2:
		logger.Printf("the daemon refused the outcome of turn %d (%s): %s", request.Sequence, request.Outcome, refusalText(status, body))
		if status/100 == 4 && request.Task != 0 && request.ProviderError == nil {
			r.refused(request.Task)
		}
	case json.Unmarshal(body, &decoded) != nil:
		logger.Printf("reported turn %d as %s; the daemon's answer could not be read: %s", request.Sequence, request.Outcome, refusalText(status, body))
	case decoded.Failed:
		logger.Printf("the daemon failed this claim: its provider refused %d turns in a row", decoded.RefusedRun)
	case decoded.RefusedRun > 0:
		logger.Printf("reported turn %d as %s; the daemon counts %d refused turns in a row", request.Sequence, request.Outcome, decoded.RefusedRun)
	}
}

package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// R3 of dispatch://INFRA-3686: "A list in a daemon text names each item its reader acts on, such
// as ... the claims an `update-stalled` names", and R8: "a fact or a step that it has the text give
// stays as a field". Main's notice of an operator's hold named every claim the hold is of
// (operatorHoldReason over heldByOperator: "An operator suspended implementer (working). The daemon
// does not replace the pod until the operator resumes each of them."). heldByOperator reads
// supervise's Claim.RuleHolds, which is also true of an operator's suspension held for its
// agent's turn (SuspensionHeld, SuspensionHeldBy: operator) while the claim is still working. The
// change replaced that list with suspended_by on each waiting claim, read from Claim.SuspendedBy,
// which is "" until the turn ends. The controller's prompt says the claims `suspended_by: operator`
// names are "the ones the operator must resume" and reads them in the reason's place. So an
// operator's suspension that reaches an implementer in a turn, past the round's bound, writes
// `holds: operator` and names no claim the operator holds.
func TestAnOperatorsHoldOverARunningTurnNamesTheClaimTheOperatorSuspended(t *testing.T) {
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
	rig.drifts(podUpdateDrift)

	rig.tick()
	rig.awaitRound(claim.RoleImplementer, podUpdateRound)
	rig.settleAll()
	rig.clockAt(podUpdateBound.Add(time.Hour))

	rig.envoyTurn(claim.RoleImplementer)
	m, ok := rig.sup.Machine(rig.tokens[claim.RoleImplementer])
	if !ok {
		t.Fatal("the implementer is not supervised")
	}
	err := m.Handle(rig.ctx, supervise.RequestSuspend{Claim: rig.tokens[claim.RoleImplementer], By: supervise.SuspendedByOperator,
		Reason: "operator"})
	if !errors.Is(err, supervise.ErrSuspendHeld) {
		t.Fatalf("the operator's suspension of the implementer in a turn answered %v, want it held for the turn", err)
	}
	if c := rig.claimOf(claim.RoleImplementer); !c.RuleHolds() || c.State != supervise.StateWorking {
		t.Fatalf("the implementer = %s, held %t by %q; want working under the operator's held suspension", c.State, c.SuspensionHeld, c.SuspensionHeldBy)
	}

	rig.tick()
	rig.tick()

	notices := rig.notices()
	if len(notices) != 1 || notices[0].Holds != texts.HoldsOperator {
		t.Fatalf("the controller was told %+v, want one notice of the operator's hold", notices)
	}
	if notices[0].Update == nil {
		t.Fatalf("the notice names no round: %+v", notices[0])
	}
	var byOperator []string
	for _, w := range notices[0].Update.Waiting {
		if w.SuspendedBy == string(supervise.SuspendedByOperator) {
			byOperator = append(byOperator, string(w.Role))
		}
	}
	if len(byOperator) != 1 || byOperator[0] != string(claim.RoleImplementer) {
		t.Errorf("the update-stalled notice of the operator's hold (holds: %s) lists %+v, and names %v with suspended_by: operator; want the implementer the operator suspended, which main's text named (\"An operator suspended implementer (working).\")",
			notices[0].Holds, notices[0].Update.Waiting, byOperator)
	}
}

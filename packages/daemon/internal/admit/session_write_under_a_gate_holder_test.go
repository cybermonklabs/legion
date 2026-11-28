package admit

import (
	"slices"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// reassertedOf says whether rows hold the daemon's set-back of a session's write of status on key
// at seq, over the record's own status own (record.ReassertStatusWrite).
func reassertedOf(rows []effect, key, own, status string, seq int64) bool {
	return slices.Contains(payloadsOf[record.StatusWrite](rowsFor(rows, key)), record.ReassertStatusWrite(own, status, seq))
}

// R26 of dispatch://INFRA-3320: "The architect's other writes on a Legion issue stay refused: a
// status in work, which only release_children and rerun_child give (R19, R20)", and the repair
// "sets back every other session's write as today". R19: an issue under a parent at work starts
// on a todo "written by the release_children of an architect above it or by a person". The root's
// architect writes todo in Dispatch on the grandchild, which a stop left at backlog in its phase.
// That architect is an architect above the grandchild on its Dispatch chain, and its todo is no
// release_children: the repair sets it back and nothing starts. The middle issue holding its own
// design gate, as an issue that held its gate as a root before it got a parent in Legion's work
// keeps it (R15, R17, R21's first boot), changes none of that; the case without it is the control.
func TestARootArchitectsTodoUnderAGateHolderIsSetBack(t *testing.T) {
	for _, tc := range []struct {
		name       string
		gateHolder bool
	}{
		{name: "no gate holder between them", gateHolder: false},
		{name: "the middle issue holds its own gate", gateHolder: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := seedOwnerTree(t, func(issues map[string]*record.Issue) {
				issues[ownerChild].HoldsGate = tc.gateHolder
				grandchild := issues[ownerGrandchild]
				grandchild.Status, grandchild.DispatchStatus = "backlog", "backlog"
			})
			before := issue(t, rig.pool, ownerGrandchild)
			if record.InLegionsWork(before) || before.Phase != phase.Implementing || before.Status != "backlog" {
				t.Fatalf("%s before the write = phase %s, status %s, want it stopped at backlog in implementing", ownerGrandchild, before.Phase, before.Status)
			}
			fact := rig.write(ownerGrandchild, "todo", rootArchitect)
			after := issue(t, rig.pool, ownerGrandchild)
			if record.InLegionsWork(after) || after.Status != "backlog" || after.Generation != before.Generation {
				t.Errorf("%s after the root architect's todo = phase %s, status %s, generation %d, in Legion's work %t; want it set back, out of work at backlog, generation %d",
					ownerGrandchild, after.Phase, after.Status, after.Generation, record.InLegionsWork(after), before.Generation)
			}
			if !reassertedOf(rig.sinceSettle(), ownerGrandchild, "backlog", "todo", fact.Seq) {
				t.Errorf("the status writes of %s after the root architect's todo = %+v, want the set-back %+v among them", ownerGrandchild,
					payloadsOf[record.StatusWrite](rowsFor(rig.sinceSettle(), ownerGrandchild)), record.ReassertStatusWrite("backlog", "todo", fact.Seq))
			}
			for _, start := range pwWorkerStarts(t, rig.pool, ownerGrandchild) {
				if start.claimable {
					t.Errorf("%s has a worker start that can run after the root architect's todo: %+v", ownerGrandchild, start)
				}
			}
		})
	}
}

// R26 of dispatch://INFRA-3320: "Only the daemon, a person and the architect of an issue above it
// write the status of a Legion issue", and the repair "sets back every other session's write as
// today". The root's implementer, a phase worker and no architect, writes done or backlog in
// Dispatch on the grandchild at work: the repair sets the write back, and no claim of the
// grandchild is suspended or closed. The middle issue holding its own design gate (R15, R17)
// changes none of that; the case without it is the control.
func TestARootWorkersStatusUnderAGateHolderIsSetBack(t *testing.T) {
	for _, gateHolder := range []bool{false, true} {
		for _, status := range []string{record.DoneStatus, "backlog"} {
			name := "no gate holder between them, " + status
			if gateHolder {
				name = "the middle issue holds its own gate, " + status
			}
			t.Run(name, func(t *testing.T) {
				rig := seedOwnerTree(t, func(issues map[string]*record.Issue) { issues[ownerChild].HoldsGate = gateHolder })
				before := issue(t, rig.pool, ownerGrandchild)
				if !record.InLegionsWork(before) || before.Phase != phase.Implementing {
					t.Fatalf("%s before the write = phase %s, status %s, want it at work in implementing", ownerGrandchild, before.Phase, before.Status)
				}
				fact := rig.write(ownerGrandchild, status, rootImplementer)
				after := issue(t, rig.pool, ownerGrandchild)
				if !record.InLegionsWork(after) || after.Status != before.Status || after.Phase != before.Phase {
					t.Errorf("%s after the root implementer's %s = phase %s, status %s, in Legion's work %t; want it set back, at work at %s in %s",
						ownerGrandchild, status, after.Phase, after.Status, record.InLegionsWork(after), before.Status, before.Phase)
				}
				if !reassertedOf(rig.sinceSettle(), ownerGrandchild, before.Status, status, fact.Seq) {
					t.Errorf("the status writes of %s after the root implementer's %s = %+v, want the set-back among them",
						ownerGrandchild, status, payloadsOf[record.StatusWrite](rowsFor(rig.sinceSettle(), ownerGrandchild)))
				}
				for _, op := range []record.SuperviseOp{"suspend", "issue_close"} {
					if roles := rolesOf(rowsFor(rig.sinceSettle(), ownerGrandchild), op); len(roles) != 0 {
						t.Errorf("%s requests for %s after the root implementer's %s = %v, want none", op, ownerGrandchild, status, roles)
					}
				}
			})
		}
	}
}

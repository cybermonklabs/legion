package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// A re-admitted root whose re-admission start was lost is sent that start again, although its
// architect's claim still holds the unconfirmed task of the run before (dispatch://INFRA-2614 R4,
// R5). The root ran at generation 1 and its architect was sent that run's task and confirmed none
// of it; a person parked the root and set it todo again, which re-admits it at generation 2 and
// raises its architect's entry to 2 in the same pass. The start that pass wrote is lost before any
// lane runs it.
//
// The task the claim holds is of a run re-admission ended, so it is no start of entry 2 on its way:
// the admission pass passes over it (workflow.ArchitectStartPending) and writes the start of entry
// 2, once however many passes read it. The outbox runs that start, the start's task takes the
// held task's place (supervise.Delivery.SupersededBy), and the relaunched architect's first prompt
// is generation 2's task, the claim holding nothing of generation 1.
func TestALostReadmissionStartIsWrittenAgainPastTheTaskOfTheRunBefore(t *testing.T) {
	r := newMainPathRig(t, true)
	r.put(r.waitingRoot())
	r.admitPass()
	r.runStarts()
	r.bringUp()
	if held := r.machine().Claim().Pending; held == nil || held.Generation != 1 || held.Entry != 1 || !held.ConfirmedAt.IsZero() {
		t.Fatalf("the architect's task before the park = %#v, want its generation-1 task of entry 1, sent and unconfirmed", held)
	}

	r.park()
	r.runSuspend()
	r.handBack()
	dropArchitectStarts(t, r)
	r.recordClaim()
	if row := rolePhaseRow(t, r.pool, mainPathRoot, claim.RoleArchitect); row.Entry != 2 {
		t.Fatalf("the architect's phases row after the re-admission names entry %d, want the 2 its promotion raised", row.Entry)
	}
	if held := r.machine().Claim().Pending; held == nil || held.Generation != 1 {
		t.Fatalf("the architect holds %#v after the re-admission's start was lost, want the generation-1 task still: the case's premise", held)
	}
	sent := len(r.prompts())

	for range 3 {
		r.admitPass()
	}
	starts := roleStartRows(t, r.pool, mainPathRoot, claim.RoleArchitect)
	if len(starts) != 1 {
		t.Fatalf("three admission passes wrote %d architect start rows of %s, want the one start of entry 2: a task of the run "+
			"re-admission ended is no start on its way", len(starts), mainPathRoot)
	}
	if got := starts[0].request; got.Entry != 2 || got.Generation != 2 || got.Task == "" {
		t.Fatalf("the start the passes wrote = entry %d of generation %d carrying %q, want the work of entry 2 of generation 2",
			got.Entry, got.Generation, got.Task)
	}

	r.runStarts()
	r.bringUp()
	prompts := r.prompts()[sent:]
	if len(prompts) == 0 {
		t.Fatalf("the re-admitted architect was sent nothing; it holds %#v", r.machine().Claim().Pending)
	}
	if first := prompts[0]; strings.Contains(first.Message, workflow.ArchitectFreshTreeLabel+" 1") ||
		!strings.Contains(first.Message, workflow.ArchitectFreshTreeLabel+" 2") {
		t.Fatalf("the re-admitted architect's first prompt is %s: %q, want the task of generation 2", first.DeliveryID, first.Message)
	}
	held := r.machine().Claim()
	if held.Pending == nil || held.Pending.Generation != 2 || held.Pending.Entry != 2 || held.GivenEntry != 2 {
		t.Fatalf("the re-admitted architect holds %#v and was given entry %d, want the task of entry 2 of generation 2 and nothing of generation 1",
			held.Pending, held.GivenEntry)
	}
}

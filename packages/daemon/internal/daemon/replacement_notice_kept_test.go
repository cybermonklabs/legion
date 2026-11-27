package daemon

// R7 of #5285, GPT-6.1 Sol's point 2 on #5285 at 0259f8af (and the Legion
// reviewer's point 10, its enqueue part), which the spec's architect ruled a fix inside R7: the
// issue's architect's word of what a replacement took is a durable pending row, keyed by the
// replacement's own identity, which only the commit of its outbox row clears. A store that refuses
// the outbox row once, after the replacement took the pod, loses nothing: the launch goes on, and
// once the store takes rows again the notice goes out, once. A round stays open until then
// (the Legion reviewer's point D on fa9deb04): its end clears what each claim records of the
// round, which is what the notice names, so a notice queued after the end would name no role that
// had not answered, and would say that the pod's disk is gone where it is a kept volume that is
// still there (roundKeptVolume reads when the round opened from the claims).

import (
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// refuseWorkspaceTaken has the store refuse every outbox row of a workspace-taken notice, as a
// store that fails the write does, until the func it answers lifts the refusal.
func (r *podUpdateRig) refuseWorkspaceTaken() (lift func()) {
	r.t.Helper()
	for _, statement := range []string{
		`create function refuse_workspace_taken() returns trigger language plpgsql as $$
		begin
			if new.kind = '` + string(record.OutboxKindNotice) + `' and (new.payload::jsonb)->>'kind' = '` + string(record.WorkspaceTakenNotice) + `' then
				raise exception 'the store refuses the workspace-taken notice';
			end if;
			return new;
		end $$`,
		`create trigger refuse_workspace_taken before insert on outbox for each row execute function refuse_workspace_taken()`,
	} {
		if _, err := r.pool.Exec(r.ctx, statement); err != nil {
			r.t.Fatalf("make the store refuse the workspace-taken notice: %v", err)
		}
	}
	return func() {
		r.t.Helper()
		if _, err := r.pool.Exec(r.ctx, `drop trigger refuse_workspace_taken on outbox`); err != nil {
			r.t.Fatalf("lift the store's refusal: %v", err)
		}
	}
}

// wantToldOnce sweeps roundSweeps times and holds the outbox to one workspace-taken notice to the
// issue's architect, naming the commit the read before the replacement found.
func (r *podUpdateRig) wantToldOnce(what string) {
	r.t.Helper()
	r.wantToldOnceNaming(what, passedOverCommit)
}

// wantToldOnceNaming sweeps roundSweeps times and holds the outbox to one workspace-taken notice to
// the issue's architect, naming naming, what the read before the replacement found.
func (r *podUpdateRig) wantToldOnceNaming(what, naming string) {
	r.t.Helper()
	for range roundSweeps {
		r.tick()
	}
	told := r.architectNotices(record.WorkspaceTakenNotice)
	if len(told) != 1 || !strings.Contains(told[0].Reason, naming) {
		r.t.Fatalf("the architect was told %+v %s, want one workspace-taken notice naming %s", told, what, naming)
	}
}

func TestAWorkspaceTakenNoticeTheStoreRefusedGoesOutOnceTheStoreTakesIt(t *testing.T) {
	t.Parallel()

	// A round over a drift that breaks the pod replaces it over a workspace that holds an unpushed
	// commit. The store refuses the notice: the round stays open until the store takes it, and
	// then ends.
	t.Run("a_round's_replacement", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
		rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
		lift := rig.refuseWorkspaceTaken()
		rig.answerRound(roundBreakingDrift, claim.RoleImplementer, claim.RoleTester)
		rig.awaitReplacement(1, "of a round over a drift that breaks the pod")
		rig.wantRoundOpenWhileRefused()
		lift()
		rig.wantToldOnce("once the store takes the row")
		rig.wantRoundEnded()
	})

	// A launch's own replacement of the issue's pod records what it is about to take
	// (recordLaunchTakes) and hands the daemon what it took once its suspension landed
	// (tellLaunchTook). The store refuses the notice; the launch is done, and the pod is gone.
	t.Run("a_launch's_own_replacement", func(t *testing.T) {
		t.Parallel()
		rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
		lift := rig.refuseWorkspaceTaken()
		taken := movedFacetTaken(rig, "launch-op-1", roundUnpushed, nil)
		if err := rig.s.recordLaunchTakes(rig.ctx, taken); err != nil {
			t.Fatalf("record what the launch takes: %v", err)
		}
		rig.s.tellLaunchTook(rig.ctx, taken)
		if told := rig.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
			t.Fatalf("the store refused every workspace-taken notice, yet the outbox holds %+v", told)
		}
		rig.rt.ScriptSuspendedOver(podUpdatePod)
		lift()
		rig.wantToldOnce("once the store takes the row")
	})
}

// wantRoundOpenWhileRefused sweeps roundSweeps times and holds every role of the issue to its
// round, and the outbox to no workspace-taken notice: the store refuses the notice, and the round
// ends only once the notice is queued. The round's pod is replaced by then, so it then takes the
// scripted drift back, as the pod that replaced it drifts from nothing
// (TestPodUpdateTellsItsWorkerNothingHoweverTheRoundEnds). Left in place, the drift is every
// claim's, and the first sweep after the round's end that reads a relaunched worker in a pod
// opens a new round over that pod, which nobody answers, so wantRoundEnded waits for good.
func (r *podUpdateRig) wantRoundOpenWhileRefused() {
	r.t.Helper()
	for range roundSweeps {
		r.tick()
	}
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		if c := r.claimOf(role); c.UpdateRound != podUpdateRound {
			r.t.Fatalf("the %s records round %q while the store refuses the round's notice, want the round still open", role, c.UpdateRound)
		}
	}
	if told := r.architectNotices(record.WorkspaceTakenNotice); len(told) != 0 {
		r.t.Fatalf("the store refused every workspace-taken notice, yet the outbox holds %+v", told)
	}
	r.drifts()
}

// wantRoundEnded sweeps until the implementer and the tester record no round.
func (r *podUpdateRig) wantRoundEnded() {
	r.t.Helper()
	for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
		r.awaitSwept(role, "its round to end once its notice is queued", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	}
}

// The Legion reviewer's point D on fa9deb04: a round over a drift that breaks the pod replaces a pod
// whose disk is a kept volume that the issue's next pod mounts, over a read that found an unpushed
// commit. The store refuses the notice. The round stays open, and once the store takes
// the notice, the notice is the one the replacement built: the work is on the kept volume, which is
// still there.
func TestARoundWhoseNoticeTheStoreRefusedIsToldWhatItsReplacementFound(t *testing.T) {
	t.Parallel()
	rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer), readyWorker(claim.RoleTester))
	rig.rt.ScriptKeptVolume(podUpdateIssue, podUpdateStart.Add(-time.Hour))
	rig.rt.ScriptWorkspaceWork(podUpdateIssue, roundUnpushed)
	lift := rig.refuseWorkspaceTaken()
	rig.answerRound(roundBreakingDrift, claim.RoleImplementer, claim.RoleTester)
	rig.awaitReplacement(1, "of a pod whose disk is a kept volume, over a drift that breaks the pod")
	rig.wantRoundOpenWhileRefused()
	lift()
	rig.wantToldOnce("once the store takes the row")
	rig.wantRoundEnded()
	told := rig.architectNotices(record.WorkspaceTakenNotice)
	if reason := told[0].Reason; !strings.Contains(reason, "the volume is still there") {
		t.Fatalf("the architect was told %q, want the notice the replacement built: the work is on the kept volume, which is still there", reason)
	}
}

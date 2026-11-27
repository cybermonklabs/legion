package daemon

// R7 of #5285 (verification v15's c-forced-replacement-told), the daemon's side of a
// launch's own replacement of an issue pod: the Sandbox runtime hands the daemon what the
// replacement is about to take before it acts (sandbox.Options.LaunchTakes) and what it took once
// it has acted (sandbox.Options.LaunchTook, runtime.TakenPod; internal/runtime/sandbox's
// launch_took_test.go holds each kind of replacement to the hand-off), and the daemon tells the
// issue's architect once, by a workspace-taken notice that names each commit the read found, or
// says that no read could be made. A read that found nothing tells nobody and records nothing. The
// hand-off here goes through a holder bound as the boot binds plan.launchRounds (daemon.run), to
// the same hooks of the supervision; the record between the two hooks is the replacement's row
// (pod_replacements), gone once the notice is queued.

import (
	"errors"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

func TestALaunchsOwnReplacementTellsTheArchitectWhatItTook(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		work  runtime.WorkspaceWork
		err   error
		says  string
		notes int
	}{
		{name: "a_read_that_found_a_commit", work: roundUnpushed, says: passedOverCommit, notes: 1},
		{name: "no_read_could_be_made", err: errors.New("the pod's launchers are not bound to it, so no launcher of the pod can read the workspace"),
			says: "No read of the workspace could be made", notes: 1},
		{name: "control:_a_read_that_found_nothing", work: roundClean, notes: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newPodUpdateRig(t, readyWorker(claim.RoleImplementer))
			holder := &launchRounds{}
			holder.bind(rig.s.roundPodEndsAtLaunch, rig.s.recordLaunchTakes, rig.s.tellLaunchTook)
			taken := movedFacetTaken(rig, "launch-op-"+tc.name, tc.work, tc.err)
			if err := holder.takes(rig.ctx, taken); err != nil {
				t.Fatalf("the record of what the launch takes: %v", err)
			}
			if records := rig.replacementRecords(); records != tc.notes {
				t.Fatalf("the daemon holds %d records of the launch's replacement before it acts, want %d", records, tc.notes)
			}
			holder.took(rig.ctx, taken)
			if records := rig.replacementRecords(); records != 0 {
				t.Fatalf("the daemon holds %d records of the launch's replacement once its notice is queued, want none", records)
			}
			told := rig.architectNotices(record.WorkspaceTakenNotice)
			if len(told) != tc.notes {
				t.Fatalf("the architect was told %+v, want %d workspace-taken notice(s)", told, tc.notes)
			}
			if tc.notes == 1 && (!strings.Contains(told[0].Reason, tc.says) || !strings.Contains(told[0].Reason, podUpdatePod)) {
				t.Fatalf("the architect was told %q, want a notice naming the pod %s and %q", told[0].Reason, podUpdatePod, tc.says)
			}
		})
	}
}

// replacementRecords is how many records of a replacement of an issue pod the store holds
// (pod_replacements).
func (r *podUpdateRig) replacementRecords() int {
	r.t.Helper()
	var n int
	if err := r.pool.QueryRow(r.ctx, `select count(*) from pod_replacements`).Scan(&n); err != nil {
		r.t.Fatalf("count the records of pod replacements: %v", err)
	}
	return n
}

// movedFacetTaken is the implementer's launch's own replacement of the issue's pod, under
// operation, because a facet of the pod fixed for its life moved, over what the read through a
// launcher of the pod found (work) or why no read could be made (err).
func movedFacetTaken(r *podUpdateRig, operation string, work runtime.WorkspaceWork, err error) runtime.TakenPod {
	return runtime.TakenPod{Operation: operation, Claim: r.tokens[claim.RoleImplementer], Issue: podUpdateIssue, PodUID: podUpdatePod,
		Why: "a facet of the pod that is fixed for its life moved, so the launching role cannot start in that pod",
		Act: runtime.PodAct(podUpdatePod), Work: work, Err: err}
}

// The Legion reviewer's Minor on #5285 at 7da847c0 (podkeep.go:409): the notice of a launch's own
// replacement opens on what its act took, and says the loss once. An act that deletes the issue's
// Sandbox and the kept volume it owns opens on that Sandbox and its volume, also where the Sandbox
// ran no pod of its own; an act that takes the pod opens on the pod.
func TestALaunchsNoticeOpensOnWhatItsActTookAndSaysTheLossOnce(t *testing.T) {
	t.Parallel()
	token := mustClaimToken(t, podUpdateIssue, claim.RoleImplementer)
	unread := errors.New("the issue's Sandbox runs no live pod, so no process can read the workspace on its volume")
	deleted := "A launch of the implementer deleted the Sandbox of " + podUpdateIssue + " (uid " + keptSandboxUID + ") and the kept volume it owned"
	for _, tc := range []struct {
		name  string
		taken runtime.TakenPod
		// opens is how the notice opens, and never what it must not say.
		opens, never string
	}{
		{name: "a_delete_of_a_sandbox_that_ran_no_pod", opens: deleted, never: "replaced the pod",
			taken: runtime.TakenPod{Claim: token, Issue: podUpdateIssue, Why: "the volume carries the reset mark", Act: runtime.VolumeAct(keptSandboxUID), Err: unread}},
		{name: "a_delete_that_names_the_last_pod_of_the_volume", opens: deleted, never: "replaced the pod",
			taken: runtime.TakenPod{Claim: token, Issue: podUpdateIssue, PodUID: podUpdatePod, Why: "the volume carries the reset mark",
				Act: runtime.VolumeAct(keptSandboxUID), Err: unread}},
		{name: "a_replacement_of_the_pod", opens: "A launch of the implementer replaced the pod of " + podUpdateIssue + " (pod " + podUpdatePod + ")",
			never: "Sandbox of", taken: runtime.TakenPod{Claim: token, Issue: podUpdateIssue, PodUID: podUpdatePod,
				Why: "a facet of the pod that is fixed for its life moved", Act: runtime.PodAct(podUpdatePod), Work: roundUnpushed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			notice := launchTakenMessage(tc.taken)
			if !strings.HasPrefix(notice, tc.opens) || strings.Contains(notice, tc.never) {
				t.Fatalf("the notice reads %q, want it to open %q and never say %q", notice, tc.opens, tc.never)
			}
			if lost := strings.Count(notice, "lost"); lost != 1 {
				t.Fatalf("the notice says the loss %d times, want once: %q", lost, notice)
			}
		})
	}
}

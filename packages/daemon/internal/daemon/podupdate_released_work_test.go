package daemon

// Every end of a pod update round reads whether an earlier pod mounted the same kept volume.
// This file checks that the record survives a daemon restart and goes away at issue close.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The tester and reviewer register in P0. An idle release suspends them and keeps the volume,
// and the tester wakes and registers in P, the next pod of that volume. The tester answers the
// round. The implementer's first launch is refused for P. After the daemon restarts, P is
// recreated, and the implementer's retry registers in the pod in P's place.
//
// At the end the reviewer's set is unknown, but the implementer's known set contains only the
// pod in P's place. The end names the implementer only because it reads P's kept-volume mark.
func TestARestartBetweenTheKeptPodRecordAndTheRoundsEndStillNamesTheRoles(t *testing.T) {
	t.Parallel()
	implementer, tester, reviewer := claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer
	rig := newPodUpdateRig(t, podUpdateRole{role: implementer, state: supervise.StateQueued},
		podUpdateRole{role: tester, state: supervise.StateQueued}, podUpdateRole{role: reviewer, state: supervise.StateQueued})
	refusals := rig.refuseLaunches()
	rig.ownsKeptVolume(keptVolumeMadeAt)
	rig.startInPod(tester, releasedPod)
	rig.startInPod(reviewer, releasedPod)
	rig.releases(releasedPod, tester, reviewer)
	rig.wakesInto(tester, podUpdatePod)
	refusals.refuse(implementer, podUpdatePod)
	rig.refusedFirstLaunch(implementer)
	rig.drifts(podUpdateExchangeDrift)

	rig.tick()
	rig.awaitRound(implementer, podUpdateRound)
	rig.awaitRound(reviewer, podUpdateRound)
	rig.awaitAsked(tester, podUpdateRound)
	rig.signal(tester, "c0ffee")

	rig, refusals = rig.restarted()
	rig.ownsKeptVolume(keptVolumeMadeAt)
	// The restarted daemon's read answers true on a failed read too, so the store is read itself.
	if holds, err := rig.st.PodHoldsEarlierWork(rig.ctx, podUpdateIssue, podUpdatePod); err != nil || !holds {
		t.Fatalf("after restart the store says %s holds earlier work: %t (%v), want the kept-volume mark", podUpdatePod, holds, err)
	}
	if !rig.s.roundPodHoldsEarlierWork(rig.ctx, podUpdateIssue, podUpdateRound) {
		t.Fatalf("the restarted daemon did not read the kept-volume mark for pod %s", podUpdatePod)
	}
	if c := rig.claimOf(implementer); c.Session != "" || c.RegisteredPods.Unknown || len(c.RegisteredPods.Pods) != 0 {
		t.Fatalf("after the restart the implementer has session %q and set %+v, want no session and a known, empty set", c.Session, c.RegisteredPods)
	}
	refusals.refuse(implementer, podUpdatePod)
	rig.refusedFirstLaunch(implementer)

	rig.displacedInto(tester, podUpdateReplacementPod)
	if pod := rig.retriesInto(refusals, implementer, podUpdateReplacementPod); pod != podUpdateReplacementPod {
		t.Fatalf("the implementer's retry came up in pod %s, want the pod in P's place, %s", pod, podUpdateReplacementPod)
	}
	rig.registerInto(implementer, podUpdateReplacementPod)
	for _, role := range []claim.Role{implementer, tester} {
		rig.rt.ScriptPodDrift(rig.tokens[role])
	}
	rig.awaitSwept(implementer, "its round to end", func(c supervise.Claim) bool { return c.UpdateRound == "" })
	rig.settleAll()

	lost := loggedLines(t, rig.logs.String(), podWentKeptOnVolumeLine)
	if len(lost) != 1 {
		t.Fatalf("the round's end was logged %+v, want one line that the members that had not answered lost what they had not pushed", lost)
	}
	wantField(t, lost[0], "lost", implementer, reviewer)
	messages := rig.messages()
	if len(messages) != 1 {
		t.Fatalf("the issue was written %q, want one message that its pod went", messages)
	}
	for _, want := range []string{"The pod that ran this issue's roles is gone", "implementer (ready)", "reviewer (suspended)"} {
		if !strings.Contains(messages[0], want) {
			t.Errorf("the message does not say %q:\n%s", want, messages[0])
		}
	}
	if strings.Contains(messages[0], "tester (") {
		t.Errorf("the message names the tester, which answered the round:\n%s", messages[0])
	}
}

// Suspending one role, as a move to backlog does, keeps the pod disk records because the issue
// can come back. Closing the issue deletes both the marked and unmarked rows (forgetIdleRelease).
func TestTheKeptPodRecordsGoWithTheIssuesWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runner, sup, _ := hookRunner(t, &outboxPublisher{})
	st, ok := runner.trees.(*store.Store)
	if !ok {
		t.Fatalf("the runner's tree store is %T, want the store", runner.trees)
	}
	releasedClaim(t, sup, claim.RoleImplementer)
	releasedClaim(t, sup, claim.RoleArchitect)
	const first, later = "first-kept-pod", "later-kept-pod"
	keptSince := time.Now().Add(-time.Hour)
	for _, pod := range []string{first, later} {
		if err := st.StartKeptPod(ctx, "legion", pauseIssue, pod, keptSince); err != nil {
			t.Fatalf("record kept pod %s: %v", pod, err)
		}
	}
	wantRows := func(when string, want int) {
		t.Helper()
		var got int
		err := st.Pool().QueryRow(ctx, `select count(*) from issue_pod_disks where issue = $1`, pauseIssue).Scan(&got)
		if err != nil || got != want {
			t.Fatalf("%s: got %d pod disk rows (%v), want %d", when, got, err, want)
		}
	}
	wantRows("after both starts", 2)
	if holds, err := st.PodHoldsEarlierWork(ctx, pauseIssue, later); err != nil || !holds {
		t.Fatalf("later pod holds earlier work = %t (%v), want true", holds, err)
	}

	if err := runner.execute(ctx, mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "suspend", Tree: pauseIssue,
		Role: claim.RoleArchitect, Generation: 1, Reason: "the tree moved to backlog"}, time.Now())); err != nil {
		t.Fatalf("the architect's suspend: %v", err)
	}
	wantRows("after a move to backlog suspends a role", 2)

	if err := runner.execute(ctx, mustOutboxRow(t, pauseIssue, record.SuperviseRequest{Op: "issue_close", Tree: pauseIssue,
		Role: claim.RoleImplementer, Generation: 1}, time.Now())); err != nil {
		t.Fatalf("the implementer's issue close: %v", err)
	}
	wantRows("after closing the issue", 0)
}

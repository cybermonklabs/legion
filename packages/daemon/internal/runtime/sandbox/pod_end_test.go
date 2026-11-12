package sandbox

// What the daemon reads about the cause of a pod's end (runtime.Observation.PodEnd): the reading
// is captured where the informer delivers the end (capturePodEnd), because neither fact it rests
// on survives the object leaving the store, and every claim of that pod carries it from there.
// An end something outside the pod made is continued uncharged by the supervisor (its judge); an
// end the daemon's own record names, and one of neither reading, are charged as they always were.

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// runningIssuePod is a rig holding one Running issue Sandbox and the tester's pod in it, with no
// controller stand-in: what happens to that pod is what the test writes and nothing else.
func runningIssuePod(t *testing.T, options ...rigOption) *rig {
	t.Helper()
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	return newRig(t, []k8sruntime.Object{
		sandboxObject(t, name, sandboxUID, modeRunning, labels),
		podObject(name, recorded, sandboxUID, labels, runningStatus()),
	}, append([]rigOption{withoutController()}, options...)...)
}

// endedPodStatus is a pod the kubelet has failed, its one container ended, carrying reason as the
// pod's own status reason ("" for none) and conditions as the cluster left them on it.
func endedPodStatus(reason string, conditions ...corev1.PodCondition) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodFailed, Reason: reason, Conditions: conditions,
		ContainerStatuses: []corev1.ContainerStatus{
			endedContainer(runtime.IssuePodContainer, corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}),
		},
	}
}

// disruptionTarget is the condition the control plane and the kubelet set on a pod they are
// ending for a reason of the cluster's: an eviction, a preemption, a node going down or draining.
func disruptionTarget(reason string) corev1.PodCondition {
	return corev1.PodCondition{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: reason}
}

// podEndRead is what the runtime has read about the end of the pod uid, waiting for the informer
// event carrying that end to be handled.
func (g *rig) podEndRead(uid string) podEnd {
	g.t.Helper()
	var end podEnd
	g.eventually("the runtime to read what ended pod "+uid, func() bool {
		read, ok := g.r.podEndOf(uid)
		end = read
		return ok
	})
	return end
}

// podEndHeld is the record the runtime keeps for the pod uid, whatever reading it holds, waiting
// for the informer event that makes it to be handled. It reads the map rather than podEndOf
// because an end read neither way is kept with an empty reading and answered to no caller: that
// record is what makes the first end event of a pod the one that decides.
func (g *rig) podEndHeld(uid string) podEnd {
	g.t.Helper()
	var end podEnd
	g.eventually("the runtime to record what ended pod "+uid, func() bool {
		g.r.mu.Lock()
		defer g.r.mu.Unlock()
		read, ok := g.r.podEnds[uid]
		end = read
		return ok
	})
	return end
}

// intendedEnds is how many records of an end the daemon means to make the runtime holds now.
func (g *rig) intendedEnds() int {
	g.r.mu.Lock()
	defer g.r.mu.Unlock()
	return len(g.r.intendedEnds)
}

// observed is one observation of loc from the stores, as the supervisor's sweep reads it.
func (g *rig) observed(loc runtime.Locator) runtime.Observation {
	g.t.Helper()
	obs, err := g.r.Probe(g.ctx, loc)
	if err != nil {
		g.t.Fatalf("probe %s: %v", loc.Claim, err)
	}
	return obs
}

// A pod being deleted with no act of the daemon's to account for it is read as an end from outside
// the pod, and the reading is made the moment the informer delivers that deletion timestamp: the
// pod is gone from the store before any claim of it is evaluated, which is exactly the order the
// cluster produces — the mapping reads a pod that still runs under a deletion timestamp as alive,
// and by the time the end is final there is no object left to read it from. Nothing of the daemon
// replaced that pod, so the end is no displacement either.
func TestAPodDeletedByNoActOfTheDaemonsIsObservedAsAnEndFromOutsideIt(t *testing.T) {
	g := runningIssuePod(t)
	name := SandboxName(workerToken)
	deleted := metav1.NewTime(rigNow)
	g.update(g.pod(name), func(p *corev1.Pod) { p.DeletionTimestamp = &deleted })
	if end := g.podEndRead(recorded); end.reading != runtime.PodEndNotOurs || end.cause != deletedByAnother {
		t.Fatalf("read %+v of the pod being deleted, want %s from its deletion timestamp", end, runtime.PodEndNotOurs)
	}
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	g.eventually("the pod store to drop the deleted pod", func() bool { return g.r.storedPod(name) == nil })

	obs := g.observed(rolesLocator(workerToken, recorded))

	if obs.Kind != runtime.Gone || obs.PodEnd != runtime.PodEndNotOurs || obs.Displaced {
		t.Fatalf("observed %s PodEnd=%q Displaced=%t (%s), want Gone %s and no displacement",
			obs.Kind, obs.PodEnd, obs.Displaced, obs.Detail, runtime.PodEndNotOurs)
	}
	if !strings.Contains(obs.Detail, deletedByAnother) {
		t.Errorf("the observation's detail is %q, want it to say the pod was being deleted with no act of the daemon's behind it", obs.Detail)
	}
}

// The cluster says it ended a pod in one of two places, and the daemon reads both and nothing
// else: the DisruptionTarget condition it marks such a pod with, and the pod's own status reason
// when that reason is one of the four the cluster gives an end of its own. A reason outside those
// four — UnexpectedAdmissionError, a node refusing a pod before it ran — is no disruption: it is
// read as neither, which is the end the supervisor charges a death for
// (TestAnEndOutsideTheClustersOwnReasonsIsChargedADeathAndFailsTheClaimAtTheThird).
func TestAnEvictedPodsEndIsObservedFromItsConditionOrItsReason(t *testing.T) {
	for _, tc := range []struct {
		name       string
		reason     string
		conditions []corev1.PodCondition
		want       runtime.PodEndReading
		wantReason string
	}{
		{name: "a disruption target", conditions: []corev1.PodCondition{disruptionTarget("EvictionByEvictionAPI")},
			want: runtime.PodEndNotOurs, wantReason: "EvictionByEvictionAPI"},
		{name: "Evicted", reason: "Evicted", want: runtime.PodEndNotOurs, wantReason: "Evicted"},
		{name: "Terminated", reason: "Terminated", want: runtime.PodEndNotOurs, wantReason: "Terminated"},
		{name: "NodeShutdown", reason: "NodeShutdown", want: runtime.PodEndNotOurs, wantReason: "NodeShutdown"},
		{name: "Preempted", reason: "Preempted", want: runtime.PodEndNotOurs, wantReason: "Preempted"},
		{name: "a reason that is none of the cluster's own", reason: "UnexpectedAdmissionError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := runningIssuePod(t)
			name := SandboxName(workerToken)
			g.update(g.pod(name), func(p *corev1.Pod) { p.Status = endedPodStatus(tc.reason, tc.conditions...) })
			g.eventually("the pod store to see the ended pod", func() bool {
				pod := g.r.storedPod(name)
				return pod != nil && terminal(pod) && pod.Status.Reason == tc.reason
			})
			if tc.want != "" {
				if end := g.podEndRead(recorded); end.reading != tc.want || end.reason != tc.wantReason {
					t.Fatalf("read %+v of the ended pod, want %s with the cluster's reason %q", end, tc.want, tc.wantReason)
				}
			}

			obs := g.observed(rolesLocator(workerToken, recorded))

			if obs.Kind != runtime.Gone || obs.PodEnd != tc.want {
				t.Fatalf("observed %s PodEnd=%q (%s), want Gone PodEnd=%q", obs.Kind, obs.PodEnd, obs.Detail, tc.want)
			}
			if tc.wantReason != "" && !strings.Contains(obs.Detail, tc.wantReason) {
				t.Errorf("the observation's detail is %q, want the reason %q the cluster gave in it", obs.Detail, tc.wantReason)
			}
		})
	}
}

// An end the daemon itself made reads as its own and is charged exactly as it was before the
// reading existed: the record the daemon writes before the act names the pod, and that record is
// what tells its own ends from an eviction's, since the daemon deletes no pod itself and every one
// of its ends reaches the pod as a deletion timestamp it did not write. Three acts are driven here
// as the daemon makes them — an issue's close or idle release through SuspendIssue, a launch
// that cannot use the pod it found through the relaunch that replaces it, whose roles are reported
// displaced as they always were, and the bare suspending patch every other suspension of the
// daemon's makes (setMode): a launch whose pod is not live enough to displace, the suspension of a
// failed launch and a repository cache's own all end their pod through that one patch, and the
// record is written there so that none of them can end a pod without one. The record is consumed
// by the end it was written for, so the runtime holds none afterwards.
func TestTheDaemonsOwnEndOfAPodIsObservedAsItsOwn(t *testing.T) {
	t.Run("a suspending patch of the sandbox", func(t *testing.T) {
		g := runningIssuePod(t)
		name := SandboxName(workerToken)

		if err := g.r.setMode(g.ctx, g.sandbox(name), modeSuspended); err != nil {
			t.Fatalf("suspend the sandbox: %v", err)
		}
		// The Agent Sandbox controller answers the patch by deleting the pod, which is how every
		// end of the daemon's reaches it: a deletion timestamp the daemon did not write.
		deleted := metav1.NewTime(rigNow)
		g.update(g.pod(name), func(p *corev1.Pod) { p.DeletionTimestamp = &deleted })

		if end := g.podEndRead(recorded); end.reading != runtime.PodEndOurs {
			t.Fatalf("read %+v of the suspended sandbox's pod, want %s: a suspension with no record reads as an end from outside the pod",
				end, runtime.PodEndOurs)
		}
		if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			t.Fatal(err)
		}
		g.eventually("the pod store to drop the deleted pod", func() bool { return g.r.storedPod(name) == nil })
		obs := g.observed(rolesLocator(workerToken, recorded))
		if obs.Kind != runtime.Gone || obs.PodEnd != runtime.PodEndOurs {
			t.Fatalf("observed %s PodEnd=%q (%s), want Gone %s", obs.Kind, obs.PodEnd, obs.Detail, runtime.PodEndOurs)
		}
		if held := g.intendedEnds(); held != 0 {
			t.Errorf("the runtime holds %d records of an intended end, want none: the record goes with the pod it names", held)
		}
	})
	t.Run("an issue's close or idle release", func(t *testing.T) {
		g := newRig(t, nil)
		loc := g.spawn(workerSpec(t))

		if err := g.r.SuspendIssue(g.ctx, testTree, testTree, func(context.Context) (bool, error) { return true, nil }); err != nil {
			t.Fatalf("suspend the issue: %v", err)
		}

		if end := g.podEndRead(loc.Sandbox.PodUID); end.reading != runtime.PodEndOurs {
			t.Fatalf("read %+v of the closed issue's pod, want %s", end, runtime.PodEndOurs)
		}
		obs := g.observed(loc)
		if obs.Kind != runtime.Gone || obs.PodEnd != runtime.PodEndOurs {
			t.Fatalf("observed %s PodEnd=%q (%s), want Gone %s", obs.Kind, obs.PodEnd, obs.Detail, runtime.PodEndOurs)
		}
		if held := g.intendedEnds(); held != 0 {
			t.Errorf("the runtime holds %d records of an intended end, want none: the record goes with the pod it names", held)
		}
	})
	t.Run("a relaunch replacing the pod", func(t *testing.T) {
		g := newRig(t, nil)
		tester := g.spawn(workerSpec(t))
		// The merger's Secret, one of the bindings a start reads, names another pod, so the
		// reviewer's start finds the pod unbound and replaces it (launcherBound, relaunch).
		editSecret(g, roleSecretName(tester.Sandbox.Name, claim.RoleMerger), func(s *corev1.Secret) {
			s.Annotations[launcherPodUIDAnnotation] = "uid-another-pod"
		})

		reviewer := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))

		if reviewer.Sandbox.PodUID == tester.Sandbox.PodUID {
			t.Fatal("the reviewer's start kept the pod; this case is about the launch that replaces one")
		}
		if end := g.podEndRead(tester.Sandbox.PodUID); end.reading != runtime.PodEndOurs {
			t.Fatalf("read %+v of the replaced pod, want %s", end, runtime.PodEndOurs)
		}
		obs := g.observed(tester)
		if !endsProcess(obs.Kind) || obs.PodEnd != runtime.PodEndOurs || !obs.Displaced {
			t.Fatalf("observed %s PodEnd=%q Displaced=%t (%s), want an end reading %s and displaced as it always was",
				obs.Kind, obs.PodEnd, obs.Displaced, obs.Detail, runtime.PodEndOurs)
		}
		if held := g.intendedEnds(); held != 0 {
			t.Errorf("the runtime holds %d records of an intended end, want none: the record goes with the pod it names", held)
		}
	})
}

// A record of an end the daemon meant to make but never made — the Sandbox patched, the patch not
// taking, the pod running on — must not answer for the pod's next genuine end: it would make that
// end read as the daemon's own for ever after. The record stands for podEndRecordLife and no
// longer, and past it the pod's end is read neither way, which is the end the supervisor charges a
// death for (TestAnEndOutsideTheClustersOwnReasonsIsChargedADeathAndFailsTheClaimAtTheThird). The
// expired record is dropped where the next one is written, so the map cannot grow without bound.
func TestAnIntendedEndThatNeverCameExpiresSoTheNextDeathIsCharged(t *testing.T) {
	name := SandboxName(workerToken)
	act := "the daemon suspended the pod's sandbox to close or release its issue"
	crash := func(g *rig) {
		g.t.Helper()
		g.update(g.pod(name), func(p *corev1.Pod) { p.Status = endedPodStatus("") })
		g.eventually("the pod store to see the crashed pod", func() bool {
			pod := g.r.storedPod(name)
			return pod != nil && terminal(pod)
		})
	}

	t.Run("while the record stands", func(t *testing.T) {
		g := runningIssuePod(t)
		g.r.meanToEndPod(recorded, act)

		crash(g)

		if end := g.podEndRead(recorded); end.reading != runtime.PodEndOurs || end.cause != act {
			t.Fatalf("read %+v of the pod, want %s naming the act the daemon recorded", end, runtime.PodEndOurs)
		}
	})
	t.Run("once it has expired", func(t *testing.T) {
		g := runningIssuePod(t)
		g.r.meanToEndPod(recorded, act)
		g.advance(podEndRecordLife)

		crash(g)

		obs := g.observed(rolesLocator(workerToken, recorded))
		if obs.Kind != runtime.Gone || obs.PodEnd != "" {
			t.Fatalf("observed %s PodEnd=%q (%s), want Gone read neither way: a stale record waives no death",
				obs.Kind, obs.PodEnd, obs.Detail)
		}
		g.r.meanToEndPod("uid-another-pod", act)
		if held := g.intendedEnds(); held != 1 {
			t.Errorf("the runtime holds %d records of an intended end, want only the one just written: an expired record is pruned where one is written", held)
		}
	})
}

// One line per pod end, not one per role: it names the pod, which of the two readings was made,
// the reason the cluster gave, and every role of that pod the runtime watches — the roles the
// supervisor continues uncharged. Both roles of this issue pod run in the one container, so both
// are named by the one line their shared pod's end gets.
func TestOneLineNamesThePodTheReadingTheReasonAndEachRoleObserved(t *testing.T) {
	logs := &lockedLog{}
	g := newRig(t, nil, withLog(logs))
	tester := g.spawn(workerSpec(t))
	reviewer := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if reviewer.Sandbox.PodUID != tester.Sandbox.PodUID {
		t.Fatalf("the two roles run in pods %s and %s; this case is about the one pod they share",
			tester.Sandbox.PodUID, reviewer.Sandbox.PodUID)
	}
	g.update(g.pod(tester.Sandbox.Name), func(p *corev1.Pod) { p.Status = endedPodStatus("Evicted") })

	g.podEndRead(tester.Sandbox.PodUID)

	var lines []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, podEndLine) {
			lines = append(lines, line)
		}
	}
	if len(lines) != 1 {
		t.Fatalf("the pod's end was logged %d times, want one line for the pod and not one per role:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{
		"pod=" + tester.Sandbox.Name, "uid=" + tester.Sandbox.PodUID, "reading=" + string(runtime.PodEndNotOurs),
		"reason=Evicted", string(claim.RoleTester), string(claim.RoleReviewer),
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the pod's end line is %q, want %q in it", lines[0], want)
		}
	}
}

// A pod whose first end event is read neither way is remembered as such, so the next event for
// the same pod cannot be read as something else. The end that must be charged is exactly this
// one: a reason outside the cluster's own four — UnexpectedAdmissionError, a node refusing the
// pod before it ran — which the roles of the next pod would meet again, and which an uncharged
// continue would relaunch for ever. The event after it is the deletion the controller, pod
// garbage collection or an operator makes, and with nothing recorded that deletion timestamp
// would read as one no act of the daemon's accounts for and continue every role of the pod
// uncharged.
func TestAnEndReadNeitherWayIsRememberedSoALaterDeletionIsNotObservedAsOneFromOutsideThePod(t *testing.T) {
	g := runningIssuePod(t)
	name := SandboxName(workerToken)
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = endedPodStatus("UnexpectedAdmissionError") })
	if end := g.podEndHeld(recorded); end.reading != "" {
		t.Fatalf("read %+v of the refused pod, want it recorded with no reading: it is neither the daemon's own end nor one the cluster says it made", end)
	}
	if _, answered := g.r.podEndOf(recorded); answered {
		t.Fatal("an end read neither way is answered to a caller; it must read as nothing was read")
	}

	// The controller's own delete of the pod, the event that follows every end.
	deleted := metav1.NewTime(rigNow)
	g.update(g.pod(name), func(p *corev1.Pod) { p.DeletionTimestamp = &deleted })
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	g.eventually("the pod store to drop the deleted pod", func() bool { return g.r.storedPod(name) == nil })

	if end := g.podEndHeld(recorded); end.reading != "" {
		t.Fatalf("the pod's end now reads %+v, want the first end event's reading of nothing: the delete that follows an end is no reading of its own", end)
	}
	obs := g.observed(rolesLocator(workerToken, recorded))
	if obs.Kind != runtime.Gone || obs.PodEnd != "" {
		t.Fatalf("observed %s PodEnd=%q (%s), want Gone read neither way, which the supervisor charges", obs.Kind, obs.PodEnd, obs.Detail)
	}
	if strings.Contains(obs.Detail, deletedByAnother) {
		t.Errorf("the observation's detail is %q, want nothing of a deletion no act of the daemon's accounts for", obs.Detail)
	}
}

// The line is the record of the roles an end continued, so a pod this runtime watches no claim of
// gets none. The pods informer carries the image probe's pod and each repository cache's pod
// beside the issue and controller pods, and a probe pod ends at every boot: with no such rule
// every boot would write, at warning level, that the roles of a pod_end_not_ours end are
// continued uncharged, with roles=[]. The reading itself is made all the same, since a claim of
// the pod may be watched by the time the end is evaluated.
func TestNoLineIsWrittenForTheEvictedPodNoWatchedClaimRunsIn(t *testing.T) {
	logs := &lockedLog{}
	g := runningIssuePod(t, withLog(logs))
	name := SandboxName(workerToken)

	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = endedPodStatus("Evicted") })

	if end := g.podEndRead(recorded); end.reading != runtime.PodEndNotOurs {
		t.Fatalf("read %+v of the evicted pod, want %s: the reading is made whether or not a line is written", end, runtime.PodEndNotOurs)
	}
	if strings.Contains(logs.String(), podEndLine) {
		t.Errorf("the end of a pod no watched claim runs in was logged as roles continued uncharged:\n%s", logs.String())
	}
}

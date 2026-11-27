package sandbox

// What the daemon reads about the cause of a pod's end (runtime.Observation.EndReading): the
// reading is captured where the informer delivers the end (captureEndReading), because neither fact
// it rests on survives the object leaving the store, and every claim of that pod carries it from
// there. An end something outside the pod made is continued uncharged by the supervisor (its
// judge), however often it comes; an end the daemon's own record names, and one of neither reading,
// are charged as they always were.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

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

// unseenIssuePod is runningIssuePod with the tester's pod in the API alone: the pod is made once
// the informers have synced, and its add, with every pod event behind it, is kept from the
// runtime's pod store until release is closed (withHeldPodEvents), as an informer that has not yet
// delivered a pod the Agent Sandbox controller made a moment ago keeps it.
func unseenIssuePod(t *testing.T, release <-chan struct{}, options ...rigOption) *rig {
	t.Helper()
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels)},
		append([]rigOption{withoutController(), withHeldPodEvents(release, heldPodAdd(name))}, options...)...)
	if err := g.kube.Tracker().Add(podObject(name, recorded, sandboxUID, labels, runningStatus())); err != nil {
		t.Fatal(err)
	}
	if g.pod(name) == nil || g.r.storedPod(name) != nil {
		t.Fatal("the tester's pod is not in the API alone; this rig is about an act the pod store lags")
	}
	return g
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

// awaitEndReading is what the runtime has read about the end of the pod uid, waiting for the
// informer event carrying that end to be handled.
func (g *rig) awaitEndReading(uid string) endReading {
	g.t.Helper()
	var end endReading
	g.eventually("the runtime to read what ended pod "+uid, func() bool {
		read, ok := g.r.endReadingOf(uid)
		end = read
		return ok
	})
	return end
}

// awaitEndReadingRecord is the record the runtime keeps for the pod uid, whatever reading it holds,
// waiting for the informer event that makes it to be handled. It reads the map rather than
// endReadingOf because an end read neither way is kept with an empty reading and answered to no
// caller: that record is what each later event of the pod is read against.
func (g *rig) awaitEndReadingRecord(uid string) endReading {
	g.t.Helper()
	var end endReading
	g.eventually("the runtime to record what ended pod "+uid, func() bool {
		g.r.mu.Lock()
		defer g.r.mu.Unlock()
		read, ok := g.r.endReadings[uid]
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
	t.Parallel()
	g := runningIssuePod(t)
	name := SandboxName(workerToken)
	deleted := metav1.NewTime(rigNow)
	g.update(g.pod(name), func(p *corev1.Pod) { p.DeletionTimestamp = &deleted })
	if end := g.awaitEndReading(recorded); end.reading != runtime.PodEndNotOurs || end.words != deletedByAnother {
		t.Fatalf("read %+v of the pod being deleted, want %s from its deletion timestamp", end, runtime.PodEndNotOurs)
	}
	g.leaveTheStore()

	obs := g.observed(rolesLocator(workerToken, recorded))

	if obs.Kind != runtime.Gone || obs.EndReading != runtime.PodEndNotOurs || obs.Displaced {
		t.Fatalf("observed %s EndReading=%q Displaced=%t (%s), want Gone %s and no displacement",
			obs.Kind, obs.EndReading, obs.Displaced, obs.Detail, runtime.PodEndNotOurs)
	}
	if !strings.Contains(obs.Detail, deletedByAnother) {
		t.Errorf("the observation's detail is %q, want it to say the pod was being deleted with no act of the daemon's behind it", obs.Detail)
	}
}

// taintEviction is the reason of the DisruptionTarget condition the taint manager sets on a pod of
// a node that stopped answering, before it deletes that pod.
const taintEviction = "DeletionByTaintManager"

// awaitEndReadingGone waits for the runtime to hear that the pod uid left the informer's store, the
// moment its reading's endReadingRecordLife is counted from.
func (g *rig) awaitEndReadingGone(uid string) {
	g.t.Helper()
	g.eventually("the runtime to hear pod "+uid+" leave the store", func() bool {
		g.r.mu.Lock()
		defer g.r.mu.Unlock()
		return !g.r.endReadings[uid].goneAt.IsZero()
	})
}

// A node that fails holds its pods in the store under their deletion timestamps for longer than
// endReadingRecordLife: the taint manager marks each a disruption target and deletes it, the node
// goes NotReady, the taint manager evicts the pod again, and only once the node's grace has run out
// does the pod leave the store and the Sandbox controller make a pod of a new uid. No claim of the
// pod is judged for its end before then, since the mapping reads a pod that still runs under a
// deletion timestamp as alive, so the reading made at the first eviction must still answer for the
// claim when it is: timed from its making, it would have expired, and the claim would be charged a
// death for an end the cluster made. The second eviction leaves the reading as the first made it.
func TestAnOutsideEndOfAPodStillInTheStoreOutlivesTheTenMinutes(t *testing.T) {
	t.Parallel()
	const replacing = "uid-pod-replacing"
	const again = "Taint manager: deleting due to NoExecute taint"
	g := runningIssuePod(t)
	name := SandboxName(workerToken)
	evicted := metav1.NewTime(rigNow)
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.DeletionTimestamp = &evicted
		p.Status.Conditions = append(p.Status.Conditions, disruptionTarget(taintEviction))
	})
	if end := g.awaitEndReading(recorded); end.reading != runtime.PodEndNotOurs || end.reason != taintEviction {
		t.Fatalf("read %+v of the pod the taint manager evicted, want %s with the reason %s", end, runtime.PodEndNotOurs, taintEviction)
	}

	g.advance(endReadingRecordLife + 5*time.Minute)
	g.editPod(name, "the pod store to see the second eviction", func(p *corev1.Pod) {
		podCondition(p, corev1.DisruptionTarget).Message = again
	})
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	if err := g.kube.Tracker().Add(podObject(name, replacing, sandboxUID, claimLabels(claim.RoleTester), runningStatus())); err != nil {
		t.Fatal(err)
	}
	g.eventually("the pod store to show the Sandbox's new pod", func() bool {
		pod := g.r.storedPod(name)
		return pod != nil && string(pod.UID) == replacing
	})

	obs := g.observed(rolesLocator(workerToken, recorded))

	if obs.Kind != runtime.NotRecordedProcess || obs.EndReading != runtime.PodEndNotOurs || obs.Displaced {
		t.Fatalf("observed %s EndReading=%q Displaced=%t (%s) %s after the eviction, want NotRecordedProcess %s and no displacement: "+
			"a reading made while the pod was in the store must stand until it leaves",
			obs.Kind, obs.EndReading, obs.Displaced, obs.Detail, endReadingRecordLife+5*time.Minute, runtime.PodEndNotOurs)
	}
	if !strings.Contains(obs.Detail, taintEviction) {
		t.Errorf("the observation's detail is %q, want the reason %q the cluster gave in it", obs.Detail, taintEviction)
	}
}

// A reading stands for endReadingRecordLife after its pod leaves the informer's store and no
// longer, so a daemon that watches pods end for weeks keeps readings only of the pods in its store
// and of those that left it in the last ten minutes: past that a reading is answered to no one and
// is dropped where the next record is written. Time the pod spent in the store before it left does
// not count. A pod leaves the store by the informer's delete, or, where the watch missed both its
// delete and its successor's add, by a relist that finds the new pod under its name and hands it to
// the handlers as an update of the old one, so no delete of the old uid ever comes; that reading is
// timed from the first prune that finds the store holding another uid.
func TestAnOutsideEndsReadingIsDroppedTenMinutesAfterItsPodLeavesTheStore(t *testing.T) {
	t.Parallel()
	// act is a record the runtime writes for another pod, which prunes the records it holds.
	const act = "a launch replaced the pod"
	for _, tc := range []struct {
		name  string
		leave func(g *rig, name string)
	}{
		{name: "the informer delivered its delete", leave: func(g *rig, name string) {
			if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
				g.t.Fatal(err)
			}
			g.awaitEndReadingGone(recorded)
		}},
		{name: "a relist found a new pod under its name", leave: func(g *rig, name string) {
			relisted := podObject(name, "uid-pod-relisted", sandboxUID, claimLabels(claim.RoleTester), runningStatus())
			if err := g.r.pods.GetStore().Update(relisted); err != nil {
				g.t.Fatal(err)
			}
			g.r.meanToEndPod("uid-another-pod", act)
			g.awaitEndReadingGone(recorded)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := runningIssuePod(t)
			name := SandboxName(workerToken)
			held := func() bool {
				g.r.mu.Lock()
				defer g.r.mu.Unlock()
				_, ok := g.r.endReadings[recorded]
				return ok
			}
			evicted := metav1.NewTime(rigNow)
			g.update(g.pod(name), func(p *corev1.Pod) {
				p.DeletionTimestamp = &evicted
				p.Status.Conditions = append(p.Status.Conditions, disruptionTarget(taintEviction))
			})
			g.awaitEndReading(recorded)
			g.advance(endReadingRecordLife)

			tc.leave(g, name)

			g.advance(endReadingRecordLife - time.Second)
			if _, answered := g.r.endReadingOf(recorded); !answered {
				t.Fatalf("the reading was answered to no one %s after its pod left the store, want it standing for %s",
					endReadingRecordLife-time.Second, endReadingRecordLife)
			}
			g.advance(time.Second)
			if end, answered := g.r.endReadingOf(recorded); answered {
				t.Fatalf("the reading %+v was answered %s after its pod left the store, want it answered to no one", end, endReadingRecordLife)
			}
			g.r.meanToEndPod("uid-another-pod", act)
			if held() {
				t.Errorf("the runtime still holds the reading of pod %s after a write, %s after the pod left the store; want it pruned",
					recorded, endReadingRecordLife)
			}
		})
	}
}

// The cluster says it ended a pod in one of two places, and the daemon reads both and nothing
// else: the DisruptionTarget condition it marks such a pod with, and the pod's own status reason
// when that reason is one of the three the cluster gives an end of its own. `Evicted` is read as
// an end from outside the pod only with the condition: the kubelet gives that reason with no
// condition to a pod over its own ephemeral-storage limit, a container's or an emptyDir's
// (clusterEndedReasons), and such a pod is read as neither, as a reason outside the three is —
// UnexpectedAdmissionError, a node refusing a pod before it ran. Both are the ends the supervisor
// charges a death for
// (TestAnEvictionWithNoDisruptionTargetIsChargedADeathAndFailsTheClaimAtTheThird).
func TestAnEvictedPodsEndIsObservedFromItsConditionOrItsReason(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		reason     string
		conditions []corev1.PodCondition
		want       runtime.PodEndReading
		wantReason string
	}{
		{name: "a disruption target", conditions: []corev1.PodCondition{disruptionTarget("EvictionByEvictionAPI")},
			want: runtime.PodEndNotOurs, wantReason: "EvictionByEvictionAPI"},
		{name: "Evicted under node pressure, a disruption target", reason: evictedReason,
			conditions: []corev1.PodCondition{disruptionTarget(corev1.PodReasonTerminationByKubelet)},
			want:       runtime.PodEndNotOurs, wantReason: corev1.PodReasonTerminationByKubelet},
		{name: "Evicted over a limit of its own, no disruption target", reason: evictedReason},
		{name: "Terminated", reason: "Terminated", want: runtime.PodEndNotOurs, wantReason: "Terminated"},
		{name: "NodeShutdown", reason: "NodeShutdown", want: runtime.PodEndNotOurs, wantReason: "NodeShutdown"},
		{name: "Preempted", reason: "Preempted", want: runtime.PodEndNotOurs, wantReason: "Preempted"},
		{name: "a reason that is none of the cluster's own", reason: "UnexpectedAdmissionError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := runningIssuePod(t)
			name := SandboxName(workerToken)
			g.editPod(name, "the pod store to see the ended pod", func(p *corev1.Pod) { p.Status = endedPodStatus(tc.reason, tc.conditions...) })
			if tc.want != "" {
				if end := g.awaitEndReading(recorded); end.reading != tc.want || end.reason != tc.wantReason {
					t.Fatalf("read %+v of the ended pod, want %s with the cluster's reason %q", end, tc.want, tc.wantReason)
				}
			}

			obs := g.observed(rolesLocator(workerToken, recorded))

			if obs.Kind != runtime.Gone || obs.EndReading != tc.want {
				t.Fatalf("observed %s EndReading=%q (%s), want Gone EndReading=%q", obs.Kind, obs.EndReading, obs.Detail, tc.want)
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
// of its ends reaches the pod as a deletion timestamp it did not write. Four acts are driven here
// as the daemon makes them — an issue's close or idle release through SuspendIssue, a launch
// that cannot use the pod it found through the relaunch that replaces it, whose roles are reported
// displaced as they always were, the bare JSON patch of the mode that a suspension naming nothing
// beside the mode sends (suspend, sendSuspended), and the relabel of a Sandbox an issue's old tree
// left Running, which suspends it in the relabel's own patch and records its end beside it
// (ensureSandbox). A launch whose pod is not live enough to displace and the suspension of a
// failed launch, which name nothing beside the mode (suspendOver, daemonEnd), and a repository
// cache's own (suspend) end their pod through that patch, and whoever sends it records the end
// under daemonSuspendedAct first, so that none of them can end a pod without one. The record is
// consumed by the end it was written for, so the runtime holds none afterwards. The pod store can
// lag the API: the Agent Sandbox controller may have made the pod a moment ago, and the informer
// not yet delivered it. The relabel, the suspending patch and a delete of the Sandbox each record
// the end of such a pod too, which they read from the API (meanToEndPodOf), so its end reads as
// the daemon's own, in the act's words, when the informer delivers it with the end behind it. CI
// run 45956159434 caught the relabel's case unrecorded: its row waits for the pod in the API, and
// the store had not yet caught up when the relabel ran.
func TestTheDaemonsOwnEndOfAPodIsObservedAsItsOwn(t *testing.T) {
	t.Parallel()
	t.Run("a suspending patch of the sandbox", func(t *testing.T) {
		g := runningIssuePod(t)
		name := SandboxName(workerToken)

		if err := g.r.suspend(g.ctx, g.sandbox(name)); err != nil {
			t.Fatalf("suspend the sandbox: %v", err)
		}
		// The Agent Sandbox controller answers the patch by deleting the pod, which is how every
		// end of the daemon's reaches it: a deletion timestamp the daemon did not write.
		deleted := metav1.NewTime(rigNow)
		g.update(g.pod(name), func(p *corev1.Pod) { p.DeletionTimestamp = &deleted })

		if end := g.awaitEndReading(recorded); end.reading != runtime.PodEndOurs {
			t.Fatalf("read %+v of the suspended sandbox's pod, want %s: a suspension with no record reads as an end from outside the pod",
				end, runtime.PodEndOurs)
		}
		g.leaveTheStore()
		obs := g.observed(rolesLocator(workerToken, recorded))
		if obs.Kind != runtime.Gone || obs.EndReading != runtime.PodEndOurs {
			t.Fatalf("observed %s EndReading=%q (%s), want Gone %s", obs.Kind, obs.EndReading, obs.Detail, runtime.PodEndOurs)
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

		if end := g.awaitEndReading(loc.Sandbox.PodUID); end.reading != runtime.PodEndOurs {
			t.Fatalf("read %+v of the closed issue's pod, want %s", end, runtime.PodEndOurs)
		}
		obs := g.observed(loc)
		if obs.Kind != runtime.Gone || obs.EndReading != runtime.PodEndOurs {
			t.Fatalf("observed %s EndReading=%q (%s), want Gone %s", obs.Kind, obs.EndReading, obs.Detail, runtime.PodEndOurs)
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
		if end := g.awaitEndReading(tester.Sandbox.PodUID); end.reading != runtime.PodEndOurs {
			t.Fatalf("read %+v of the replaced pod, want %s", end, runtime.PodEndOurs)
		}
		obs := g.observed(tester)
		if !endsProcess(obs.Kind) || obs.EndReading != runtime.PodEndOurs || !obs.Displaced {
			t.Fatalf("observed %s EndReading=%q Displaced=%t (%s), want an end reading %s and displaced as it always was",
				obs.Kind, obs.EndReading, obs.Displaced, obs.Detail, runtime.PodEndOurs)
		}
		if held := g.intendedEnds(); held != 0 {
			t.Errorf("the runtime holds %d records of an intended end, want none: the record goes with the pod it names", held)
		}
	})
	t.Run("a relabel for the issue's new tree", func(t *testing.T) {
		orphan := claim.Token("legion-legion-legion-209-architect")
		name := SandboxName(orphan)
		oldTree := map[string]string{labelProject: testProject, labelTree: labelValue(testTree), labelIssue: labelValue("LEGION-209")}
		g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-old-tree", modeRunning, oldTree)})
		// No claim of another tree runs in the Sandbox, so the orphan root's launch relabels it and
		// suspends it in the same patch, ending the old tree's pod.
		g.store.running["LEGION-209"] = runningClaim{tree: "LEGION-209", claim: "legion-legion-legion-209-planner:launching"}
		g.eventually("the old tree's pod", func() bool { return g.pod(name) != nil })
		old := string(g.pod(name).UID)
		spec := testSpec(t, orphan, claim.RoleArchitect, "LEGION-209")
		spec.Tree = "LEGION-209"

		g.spawn(spec)

		end := g.awaitEndReadingRecord(old)
		if end.reading != runtime.PodEndOurs || !strings.Contains(end.words, "relabel") {
			t.Fatalf("read %+v of the pod the relabel suspended, want %s naming the relabel: a suspension with no record reads as an end from outside the pod",
				end, runtime.PodEndOurs)
		}
		if held := g.intendedEnds(); held != 0 {
			t.Errorf("the runtime holds %d records of an intended end, want none: the record goes with the pod it names", held)
		}
	})
	t.Run("a relabel for the issue's new tree, its pod not yet in the store", func(t *testing.T) {
		orphan := claim.Token("legion-legion-legion-209-architect")
		name := SandboxName(orphan)
		oldTree := map[string]string{labelProject: testProject, labelTree: labelValue(testTree), labelIssue: labelValue("LEGION-209")}
		release := make(chan struct{})
		// The controller stand-in makes the old tree's pod once the informers have synced, and the
		// runtime's pod store hears nothing of it until release is closed.
		g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, "uid-sandbox-old-tree", modeRunning, oldTree)},
			withHeldPodEvents(release, heldPodAdd(name)))
		g.store.running["LEGION-209"] = runningClaim{tree: "LEGION-209", claim: "legion-legion-legion-209-planner:launching"}
		g.eventually("the old tree's pod in the API", func() bool { return g.pod(name) != nil })
		old := string(g.pod(name).UID)
		spec := testSpec(t, orphan, claim.RoleArchitect, "LEGION-209")
		spec.Tree = "LEGION-209"
		l, err := g.r.prepare(spec)
		if err != nil {
			t.Fatal(err)
		}
		if g.r.storedPod(name) != nil {
			t.Fatal("the pod store holds the old tree's pod; this case is about a relabel the store lags")
		}

		// The launch's own step that relabels the Sandbox and suspends it in the same patch.
		if _, err := g.r.ensureSandbox(g.ctx, l); err != nil {
			t.Fatalf("relabel the sandbox: %v", err)
		}
		close(release)

		end := g.awaitEndReadingRecord(old)
		if end.reading != runtime.PodEndOurs || !strings.Contains(end.words, "relabel") {
			t.Fatalf("read %+v of the pod the relabel suspended before the store held it, want %s naming the relabel: an act whose pod the store lags must read it from the API",
				end, runtime.PodEndOurs)
		}
		if held := g.intendedEnds(); held != 0 {
			t.Errorf("the runtime holds %d records of an intended end, want none: the record goes with the pod it names", held)
		}
	})
	for _, tc := range []struct {
		name, act string
		end       func(g *rig, s *sandbox) error
	}{
		{name: "a suspending patch of the sandbox, its pod not yet in the store", act: daemonSuspendedAct,
			end: func(g *rig, s *sandbox) error { return g.r.suspend(g.ctx, s) }},
		{name: "a delete of the sandbox, its pod not yet in the store", act: daemonDeletedAct,
			end: func(g *rig, s *sandbox) error {
				return g.r.deleteFenced(g.ctx, s, "sandbox runtime: deleted an orphaned sandbox")
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			release := make(chan struct{})
			g := unseenIssuePod(t, release)
			name := SandboxName(workerToken)

			if err := tc.end(g, g.sandbox(name)); err != nil {
				t.Fatalf("end the sandbox's pod: %v", err)
			}
			// The Agent Sandbox controller, or the garbage collector for a delete, answers the act
			// by deleting the pod, and the informer then delivers the pod and its end together.
			deleted := metav1.NewTime(rigNow)
			g.update(g.pod(name), func(p *corev1.Pod) { p.DeletionTimestamp = &deleted })
			close(release)

			if end := g.awaitEndReading(recorded); end.reading != runtime.PodEndOurs || end.words != tc.act {
				t.Fatalf("read %+v of the pod the act ended before the store held it, want %s under the act %q: an act whose pod the store lags must read it from the API",
					end, runtime.PodEndOurs, tc.act)
			}
			if held := g.intendedEnds(); held != 0 {
				t.Errorf("the runtime holds %d records of an intended end, want none: the record goes with the pod it names", held)
			}
		})
	}
}

// An act whose pod the store does not yet hold reads the pod from the API (meanToEndPodOf), and a
// read that fails records nothing: the act goes ahead, the failure is logged at warning level with
// the Sandbox and the error, and the pod's end then reads as one from outside the pod, which is
// what a restart that lost the record costs (meanToEndPod).
func TestAnActThatCannotReadItsUnseenPodRecordsNothingAndWarns(t *testing.T) {
	t.Parallel()
	refused := apierrors.NewInternalError(errors.New("etcdserver: request timed out"))
	log := &logBuffer{}
	release := make(chan struct{})
	g := unseenIssuePod(t, release, withLog(log), func(g *rig, _ *Options) {
		g.kube.PrependReactor("get", "pods", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, refused
		})
	})
	name := SandboxName(workerToken)

	if err := g.r.suspend(g.ctx, g.sandbox(name)); err != nil {
		t.Fatalf("suspend the sandbox: %v", err)
	}
	if s := g.sandbox(name); s == nil || s.mode() != modeSuspended {
		t.Fatalf("the sandbox is %+v after the suspension, want it Suspended: a failed read stops no act", s)
	}
	if held := g.intendedEnds(); held != 0 {
		t.Fatalf("the runtime holds %d records of an intended end, want none: no read named the pod", held)
	}
	var line string
	for _, l := range strings.Split(log.String(), "\n") {
		if strings.Contains(l, "could not read the pod an act of the daemon's is about to end") {
			line = l
		}
	}
	for _, want := range []string{"level=WARN", "sandbox=" + name, "etcdserver: request timed out"} {
		if !strings.Contains(line, want) {
			t.Errorf("the failed read's line is %q, want it to hold %q; the log:\n%s", line, want, log.String())
		}
	}

	deleted := metav1.NewTime(rigNow)
	g.update(g.pod(name), func(p *corev1.Pod) { p.DeletionTimestamp = &deleted })
	close(release)
	if end := g.awaitEndReading(recorded); end.reading != runtime.PodEndNotOurs || end.words != deletedByAnother {
		t.Fatalf("read %+v of the pod the unrecorded suspension ended, want %s from its deletion timestamp", end, runtime.PodEndNotOurs)
	}
}

// A pod already terminal is read from its status alone, whatever deletion timestamp the event that
// first shows its end carries (captureEndReading): the Agent Sandbox controller deletes a failed
// pod, and that delete can reach the informer in the same event as the terminal status. Read from
// the deletion timestamp, the end would be one no act of the daemon's accounts for, and a failure
// the supervisor charges when its status comes alone
// (TestAnEvictedPodsEndIsObservedFromItsConditionOrItsReason) would be continued uncharged whenever
// the two came together: a task that fills its own disk, the kubelet's `Evicted` with no
// DisruptionTarget condition, or a pod a node keeps refusing, UnexpectedAdmissionError, would be
// relaunched pod after pod for ever. The words the end was read from are kept on its record, though
// an end read neither way writes no line, and no caller is answered with them. What the cluster
// says ended the pod is read first, and is an end from outside it with a deletion timestamp beside
// it as without one.
func TestATerminalPodIsObservedFromItsStatusAloneWhateverDeletionTimestampItCarries(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  corev1.PodStatus
		reading runtime.PodEndReading
		// words are the words the end must be read from, where a case names them; reason is the
		// reason it must carry, which is the cluster's own where the cluster gave one.
		words, reason string
	}{
		{name: "Evicted over a limit of its own, no disruption target", status: endedPodStatus(evictedReason),
			words: evictedForItsOwnUse, reason: evictedReason},
		{name: "a reason that is none of the cluster's own", status: endedPodStatus("UnexpectedAdmissionError"),
			words: terminalWithNoClusterEnd, reason: "UnexpectedAdmissionError"},
		{name: "no reason at all", status: endedPodStatus(""), words: terminalWithNoClusterEnd},
		{name: "a disruption target", status: endedPodStatus(evictedReason, disruptionTarget(corev1.PodReasonTerminationByKubelet)),
			reading: runtime.PodEndNotOurs, reason: corev1.PodReasonTerminationByKubelet},
		{name: "one of the cluster's own reasons", status: endedPodStatus("Preempted"),
			reading: runtime.PodEndNotOurs, reason: "Preempted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := runningIssuePod(t)
			name := SandboxName(workerToken)
			deleted := metav1.NewTime(rigNow)
			g.update(g.pod(name), func(p *corev1.Pod) {
				p.Status = tc.status
				p.DeletionTimestamp = &deleted
			})

			end := g.awaitEndReadingRecord(recorded)
			if end.reading != tc.reading || end.reason != tc.reason || tc.words != "" && end.words != tc.words {
				t.Fatalf("read %+v of the terminal pod under a deletion timestamp, want the reading %q, the reason %q and the words %q",
					end, tc.reading, tc.reason, tc.words)
			}
			if _, answered := g.r.endReadingOf(recorded); answered != (tc.reading != "") {
				t.Fatalf("the end was answered to a caller: %v, want %v; an end read neither way must read as nothing was read",
					answered, tc.reading != "")
			}
			g.leaveTheStore()
			obs := g.observed(rolesLocator(workerToken, recorded))
			if obs.Kind != runtime.Gone || obs.EndReading != tc.reading {
				t.Fatalf("observed %s EndReading=%q (%s), want Gone EndReading=%q: a pod already terminal is read from its status alone",
					obs.Kind, obs.EndReading, obs.Detail, tc.reading)
			}
			if strings.Contains(obs.Detail, deletedByAnother) {
				t.Errorf("the observation's detail is %q, want nothing of a deletion no act of the daemon's accounts for", obs.Detail)
			}
		})
	}
}

// Of the evictions whose message names no container over its request (overRequest, which
// readEndReading reads first, and which is the pod's own use whatever condition it carries), one is
// the pod's own only where the DisruptionTarget condition is not on it, and selfEvicted reads that
// condition itself, so it never agrees with disruption about a pod: an eviction under node
// pressure carries the same `Evicted` reason a pod over its own limit gets, and among those the
// condition is all that tells them apart. Read from the reason alone, selfEvicted would
// call a neighbour's pressure the pod's own, and any order of readEndReading's cases that asked it
// before disruption, or any other caller of it, would have every role of that pod charged.
func TestAnEvictionIsThePodsOwnOnlyWithNoDisruptionTargetWhateverAsksFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		reason     string
		conditions []corev1.PodCondition
		want       bool
	}{
		{name: "Evicted with no condition", reason: evictedReason, want: true},
		{name: "Evicted with the condition not set true", reason: evictedReason,
			conditions: []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionFalse}}, want: true},
		{name: "Evicted under node pressure, a disruption target", reason: evictedReason,
			conditions: []corev1.PodCondition{disruptionTarget(corev1.PodReasonTerminationByKubelet)}},
		{name: "a disruption target with no reason", conditions: []corev1.PodCondition{disruptionTarget("EvictionByEvictionAPI")}},
		{name: "a reason that is none of the cluster's own", reason: "UnexpectedAdmissionError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: endedPodStatus(tc.reason, tc.conditions...)}
			if got := selfEvicted(pod); got != tc.want {
				t.Errorf("selfEvicted = %v, want %v", got, tc.want)
			}
			if _, _, disrupted := disruption(pod); disrupted && selfEvicted(pod) {
				t.Errorf("the pod is read both as a disruption and as evicted for its own use; the two must never both hold")
			}
		})
	}
}

// A record of an end the daemon meant to make but never made — the Sandbox patched, the patch not
// taking, the pod running on — must not answer for the pod's next genuine end: it would make that
// end read as the daemon's own for ever after. The record stands for endReadingRecordLife and no
// longer, and past it the pod's end is read neither way, which is the end the supervisor charges a
// death for (TestAnEvictionWithNoDisruptionTargetIsChargedADeathAndFailsTheClaimAtTheThird). The
// expired record is dropped where the next one is written, so the map cannot grow without bound.
func TestAnIntendedEndThatNeverCameExpiresSoTheNextDeathIsCharged(t *testing.T) {
	t.Parallel()
	name := SandboxName(workerToken)
	act := "the daemon suspended the pod's sandbox to close or release its issue"
	crash := func(g *rig) {
		g.t.Helper()
		g.editPod(name, "the pod store to see the crashed pod", func(p *corev1.Pod) { p.Status = endedPodStatus("") })
	}

	t.Run("while the record stands", func(t *testing.T) {
		g := runningIssuePod(t)
		g.r.meanToEndPod(recorded, act)

		crash(g)

		if end := g.awaitEndReading(recorded); end.reading != runtime.PodEndOurs || end.words != act {
			t.Fatalf("read %+v of the pod, want %s naming the act the daemon recorded", end, runtime.PodEndOurs)
		}
	})
	t.Run("once it has expired", func(t *testing.T) {
		g := runningIssuePod(t)
		g.r.meanToEndPod(recorded, act)
		g.advance(endReadingRecordLife)

		crash(g)

		obs := g.observed(rolesLocator(workerToken, recorded))
		if obs.Kind != runtime.Gone || obs.EndReading != "" {
			t.Fatalf("observed %s EndReading=%q (%s), want Gone read neither way: a stale record waives no death",
				obs.Kind, obs.EndReading, obs.Detail)
		}
		g.r.meanToEndPod("uid-another-pod", act)
		if held := g.intendedEnds(); held != 1 {
			t.Errorf("the runtime holds %d records of an intended end, want only the one just written: an expired record is pruned where one is written", held)
		}
	})
}

// One line per pod end, not one per role: it names the pod, which of the two readings was made,
// the reason the cluster gave, and, as ended, every role of that pod the runtime watches, each of
// which the end ended. The field names what ended and nothing of what any role got for it, which
// each claim's own supervise line says: named roles, it read as the roles continued. Both roles of
// this issue pod run in the one container, so both are named by the one line their shared pod's
// end gets.
func TestOneLineNamesThePodTheReadingTheReasonAndEachRoleObserved(t *testing.T) {
	t.Parallel()
	logs := &lockedLog{}
	g := newRig(t, nil, withLog(logs))
	tester := g.spawn(workerSpec(t))
	reviewer := g.spawn(testSpec(t, otherToken, claim.RoleReviewer, testTree))
	if reviewer.Sandbox.PodUID != tester.Sandbox.PodUID {
		t.Fatalf("the two roles run in pods %s and %s; this case is about the one pod they share",
			tester.Sandbox.PodUID, reviewer.Sandbox.PodUID)
	}
	g.update(g.pod(tester.Sandbox.Name), func(p *corev1.Pod) {
		p.Status = endedPodStatus(evictedReason, disruptionTarget(corev1.PodReasonTerminationByKubelet))
	})

	g.awaitEndReading(tester.Sandbox.PodUID)

	lines := linesHolding(logs.String(), endReadingLine)
	if len(lines) != 1 {
		t.Fatalf("the pod's end was logged %d times, want one line for the pod and not one per role:\n%s", len(lines), logs.String())
	}
	for _, want := range []string{
		"pod=" + tester.Sandbox.Name, "uid=" + tester.Sandbox.PodUID, "reading=" + string(runtime.PodEndNotOurs),
		"reason=TerminationByKubelet",
	} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("the pod's end line is %q, want %q in it", lines[0], want)
		}
	}
	_, ended, ok := strings.Cut(lines[0], " ended=")
	if !ok {
		t.Fatalf("the pod's end line is %q, want the roles the end ended under ended", lines[0])
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleReviewer} {
		if !strings.Contains(ended, string(role)) {
			t.Errorf("the pod's end line names ended=%s, want the %s it ended among them", ended, role)
		}
	}
	if strings.Contains(lines[0], " roles=") {
		t.Errorf("the pod's end line is %q, want no roles field, which reads as the roles continued", lines[0])
	}
}

// One pod's end is read by the rule (captureEndReading) whichever of two facts the informer
// delivers first, a terminal status or a deletion timestamp, and the first event that shows the end
// decides it. The two cases carry the same two facts and differ in their order alone. A pod the
// kubelet failed with a reason outside the cluster's own — UnexpectedAdmissionError, a node
// refusing the pod before it ran, which the roles of the next pod would meet again — is read
// neither way, and the deletion that follows every end, the controller's, pod garbage collection's
// or an operator's, leaves it so: read as an end from outside the pod, that failure would be
// continued uncharged and relaunched for ever. A running pod deleted with no act of the daemon's
// behind it is read as an end from outside it, and the terminal status the kubelet then gives it
// leaves it so: read from that status, an eviction's delete would be charged a death.
func TestAPodsEndIsObservedByTheRuleWhicheverOfItsTerminalStatusAndItsDeletionComesFirst(t *testing.T) {
	t.Parallel()
	const refused = "UnexpectedAdmissionError"
	deleted := metav1.NewTime(rigNow)
	fail := func(p *corev1.Pod) { p.Status = endedPodStatus(refused) }
	markDeleted := func(p *corev1.Pod) { p.DeletionTimestamp = &deleted }
	for _, tc := range []struct {
		name          string
		first, second func(*corev1.Pod)
		reading       runtime.PodEndReading
		words, reason string
	}{
		{name: "the terminal status first", first: fail, second: markDeleted,
			words: terminalWithNoClusterEnd, reason: refused},
		{name: "the deletion of the running pod first", first: markDeleted, second: fail,
			reading: runtime.PodEndNotOurs, words: deletedByAnother},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := runningIssuePod(t)
			name := SandboxName(workerToken)
			wantRead := func(when string) {
				t.Helper()
				if end := g.awaitEndReadingRecord(recorded); end.reading != tc.reading || end.words != tc.words || end.reason != tc.reason {
					t.Fatalf("%s the pod's end reads %+v, want the reading %q from the words %q and the reason %q",
						when, end, tc.reading, tc.words, tc.reason)
				}
				if _, answered := g.r.endReadingOf(recorded); answered != (tc.reading != "") {
					t.Fatalf("%s the end was answered to a caller: %v, want %v; an end read neither way must read as nothing was read",
						when, answered, tc.reading != "")
				}
			}

			g.update(g.pod(name), tc.first)
			wantRead("after the first event,")

			g.editPod(name, "the pod store to see both facts", tc.second)
			g.leaveTheStore()
			wantRead("after the second event and the informer's delete,")

			obs := g.observed(rolesLocator(workerToken, recorded))
			if obs.Kind != runtime.Gone || obs.EndReading != tc.reading {
				t.Fatalf("observed %s EndReading=%q (%s), want Gone EndReading=%q", obs.Kind, obs.EndReading, obs.Detail, tc.reading)
			}
			if named := strings.Contains(obs.Detail, deletedByAnother); named != (tc.words == deletedByAnother) {
				t.Errorf("the observation's detail is %q; it names a deletion no act of the daemon's accounts for: %v, want %v",
					obs.Detail, named, tc.words == deletedByAnother)
			}
		})
	}
}

// An end read neither way is read again at each later event of the pod (captureEndReading), and
// only a reading that decides the end stops that: the kubelet's `Evicted` status can reach the
// informer in an event of its own before the DisruptionTarget condition that says the node's
// pressure caused it, and read from that first event alone, a neighbour's pressure would be charged
// a death. The later event reads the end as one from outside the pod. That reading decides the end,
// and it is the one the pod's line names: the empty reading before it decided nothing and wrote no
// line, so the end writes one line, the not-ours one.
func TestAnEndReadNeitherWayIsReadAgainAtTheEventThatSaysTheClusterEndedIt(t *testing.T) {
	t.Parallel()
	const disrupted = "TerminationByKubelet"
	logs := &lockedLog{}
	g := newRig(t, nil, withLog(logs))
	loc := g.spawn(workerSpec(t))
	name, uid := loc.Sandbox.Name, loc.Sandbox.PodUID

	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = endedPodStatus(evictedReason) })
	if end := g.awaitEndReadingRecord(uid); end.reading != "" || end.words != evictedForItsOwnUse {
		t.Fatalf("read %+v of the eviction with no condition, want it read neither way in the words %q", end, evictedForItsOwnUse)
	}
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status.Conditions = append(p.Status.Conditions, disruptionTarget(disrupted))
	})

	if end := g.awaitEndReading(uid); end.reading != runtime.PodEndNotOurs || end.reason != disrupted {
		t.Fatalf("read %+v of the eviction once its condition came, want it read again as %s with the reason %s",
			end, runtime.PodEndNotOurs, disrupted)
	}
	lines := linesHolding(logs.String(), endReadingLine)
	if len(lines) != 1 || !strings.Contains(lines[0], "uid="+uid) || !strings.Contains(lines[0], "reading="+string(runtime.PodEndNotOurs)) {
		t.Fatalf("the pod's end wrote the lines %q, want one naming the pod and the reading %s that decided the end:\n%s",
			lines, runtime.PodEndNotOurs, logs.String())
	}
}

// A re-read takes no record of an intended end (captureEndReading). The first reading of the end
// took any record that stood, so one the daemon writes once the pod has shown its end was written
// by an act that did not make that end: a launch that suspends the Sandbox of a pod that already
// failed with no reason records the daemon's end under daemonSuspendedAct (suspendRecordingEnd)
// before the controller's delete of that pod reaches the informer. Taken by the re-read that delete
// makes, the record would turn the pod's own failure into the daemon's end, in the words of an act
// that did not make it. The end stays read neither way, and the record is left to its expiry.
func TestAReReadOfAnEndTakesNoRecordTheDaemonWroteAfterThePodShowedIt(t *testing.T) {
	t.Parallel()
	g := runningIssuePod(t)
	name := SandboxName(workerToken)
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = endedPodStatus("") })
	if end := g.awaitEndReadingRecord(recorded); end.reading != "" || end.words != terminalWithNoClusterEnd {
		t.Fatalf("read %+v of the failed pod, want it read neither way in the words %q", end, terminalWithNoClusterEnd)
	}
	g.r.meanToEndPod(recorded, daemonSuspendedAct)

	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
		t.Fatal(err)
	}
	g.awaitEndReadingGone(recorded)

	if end := g.awaitEndReadingRecord(recorded); end.reading != "" || end.words != terminalWithNoClusterEnd {
		t.Fatalf("read %+v of the failed pod at its delete, want it still read neither way in the words %q, not as the act the daemon recorded once the pod had ended",
			end, terminalWithNoClusterEnd)
	}
	if held := g.intendedEnds(); held != 1 {
		t.Errorf("the runtime holds %d records of an intended end, want the one written after the pod showed its end left standing: a re-read takes none", held)
	}
	obs := g.observed(rolesLocator(workerToken, recorded))
	if obs.Kind != runtime.Gone || obs.EndReading != "" {
		t.Fatalf("observed %s EndReading=%q (%s), want Gone read neither way", obs.Kind, obs.EndReading, obs.Detail)
	}
}

// The line is the record of the watched roles an end ended, so a pod this runtime watches no claim
// of gets none. The pods informer carries the image probe's pod and each repository cache's pod
// beside the issue and controller pods, and a probe pod ends at every boot: with no such rule
// every boot would write, at warning level, a line for an end from outside the pod that names
// roles=[]. The reading itself is made all the same, since a claim of the pod may be watched by
// the time the end is evaluated.
func TestNoLineIsWrittenForTheEvictedPodNoWatchedClaimRunsIn(t *testing.T) {
	t.Parallel()
	logs := &lockedLog{}
	g := runningIssuePod(t, withLog(logs))
	name := SandboxName(workerToken)

	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status = endedPodStatus(evictedReason, disruptionTarget("TerminationByKubelet"))
	})

	if end := g.awaitEndReading(recorded); end.reading != runtime.PodEndNotOurs {
		t.Fatalf("read %+v of the evicted pod, want %s: the reading is made whether or not a line is written", end, runtime.PodEndNotOurs)
	}
	if strings.Contains(logs.String(), endReadingLine) {
		t.Errorf("the end of a pod no watched claim runs in was logged, with no role of it watched:\n%s", logs.String())
	}
}

// A re-read that reads the end neither way again, in other words than the reading standing, keeps
// the new reading and writes no line (captureEndReading): the end is still undecided, and the
// record later events are read against is the one made from the newest state. Here the kubelet's
// terminal status first names no cause the rule reads, and a later event of the same pod carries
// the `Evicted` reason with no DisruptionTarget condition, the pod's own use. The pod runs a
// watched claim, so a line would be written if the re-read decided anything.
//
// The control is a re-read that keeps the standing record whenever the reading is empty both
// times: the record then still holds the first words, and the test fails at the second reading.
func TestAReReadThatReadsTheEndNeitherWayInOtherWordsKeepsThemAndWritesNoLine(t *testing.T) {
	t.Parallel()
	const refused = "UnexpectedAdmissionError"
	logs := &lockedLog{}
	g := newRig(t, nil, withLog(logs))
	loc := g.spawn(workerSpec(t))
	name, uid := loc.Sandbox.Name, loc.Sandbox.PodUID

	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = endedPodStatus(refused) })
	if end := g.awaitEndReadingRecord(uid); end.reading != "" || end.words != terminalWithNoClusterEnd || end.reason != refused {
		t.Fatalf("read %+v of the failed pod, want it read neither way in the words %q with the reason %s", end, terminalWithNoClusterEnd, refused)
	}
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status.Reason = evictedReason })

	var end endReading
	g.eventually("the re-read of the pod's end to keep the words of its new state", func() bool {
		g.r.mu.Lock()
		defer g.r.mu.Unlock()
		end = g.r.endReadings[uid]
		return end.words == evictedForItsOwnUse
	})
	if end.reading != "" || end.reason != evictedReason {
		t.Fatalf("read %+v of the evicted pod, want it read neither way with the reason %s", end, evictedReason)
	}
	if lines := linesHolding(logs.String(), endReadingLine); len(lines) != 0 {
		t.Fatalf("the pod's end read neither way twice wrote the lines %q, want none:\n%s", lines, logs.String())
	}
}

package sandbox

import (
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// A pod whose workspace-init ended non-zero past its restart grace (initRestartGrace) is Gone for
// every role, and says so: no launcher could have started in it. The mapping classifies it a failed
// init, whatever the exit code, which is what keeps it out of ends() and so out of the displaced
// reading. An end inside the grace is the pod still provisioning
// (TestAWorkspaceInitsFirstThreeRestartsAreStartingAndItsFourthIsAFailure).
func TestRestartingInitFailureIsGoneForEveryRole(t *testing.T) {
	t.Parallel()
	for _, waiting := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminated", true: "backoff"}[waiting], func(t *testing.T) {
			g := newRig(t, nil)
			loc := g.spawn(workerSpec(t))
			g.update(g.pod(loc.Sandbox.Name), func(p *corev1.Pod) {
				p.Status.Phase = corev1.PodPending
				failed := &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}
				status := corev1.ContainerStatus{Name: initContainer, RestartCount: initRestartGrace + 1}
				if waiting {
					status.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
					status.LastTerminationState.Terminated = failed
				} else {
					status.State.Terminated = failed
				}
				p.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
			})
			g.eventually("the informer to see the restarting init", func() bool {
				p := g.r.storedPod(loc.Sandbox.Name)
				return p != nil && p.Status.Phase == corev1.PodPending
			})
			observation, err := g.r.Probe(g.ctx, loc)
			if err != nil || observation.Kind != runtime.Gone || !strings.Contains(observation.Detail, initContainer) {
				t.Fatalf("restarting init = %+v, %v; want Gone with the failed init and its detail", observation, err)
			}
			if !g.initFailed(loc) {
				t.Fatalf("restarting init = %+v; want it classified a failed init", observation)
			}
		})
	}
}

// A relaunch replaces a pod whose workspace-init keeps failing past its restart grace
// (initRestartGrace): no launcher can start in it.
func TestRelaunchReplacesAPodWhoseInitKeepsFailing(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	old := g.spawn(workerSpec(t))
	if err := g.r.Suspend(g.ctx, old); err != nil {
		t.Fatal(err)
	}
	g.update(g.pod(old.Sandbox.Name), func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: initContainer, RestartCount: initRestartGrace + 1,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}
	})
	g.eventually("the informer to see the failed init", func() bool { return g.r.storedPod(old.Sandbox.Name).Status.Phase == corev1.PodPending })
	spec := workerSpec(t)
	spec.Generation = 2
	loc := g.spawn(spec)
	if loc.Sandbox.PodUID == old.Sandbox.PodUID {
		t.Fatal("relaunch reused a pod whose init cannot start a launcher")
	}
}

// An issue whose microVM pod keeps its volume (keptVolume) keeps one claim across its pods. So a
// fault on that volume that fails workspace-init fails every later pod of the issue too. The issue's
// next launch therefore resets the volume when workspace-init failed, as failedInit reads it, for a
// cause local to the volume (localInitFailure). The launch deletes the Sandbox and the claim that it
// owns. It creates a Sandbox with a new uid in its place before it writes the Secrets and sets the
// Sandbox Running. It logs one line that names the issue, the claim and workspace-init. When the
// update round's ReplacePod or the release took that pod down first, the next launch reads no pod.
// So each of them writes the reset mark (volumeResetAnnotation) on the Sandbox in the patch that
// suspends it, and the launch resets on the mark. The Sandbox that the reset creates carries no
// mark. Every other failure replaces the pod and keeps the Sandbox and its volume, as at main: an
// exit for a cause outside the volume (workspace.InitExitOutsideVolume), a usage error
// (workspace.InitExitUsage), an OOMKilled end, an end in a pod that is being deleted or that the
// kubelet ended itself, and workspace-init while its grace still restarts. A Sandbox with an
// ephemeral volume has its pod replaced and no Sandbox created, as at main.
func TestAKeptVolumeWhoseInitContainerFailedIsReplaced(t *testing.T) {
	t.Parallel()
	name, claimName := SandboxName(workerToken), IssueClaimName(workerToken)
	kept := keptVolumes(SandboxMicroVM)
	// launched is a rig of config that logs to log, with the tester's first pod launched and its
	// process then stopped, as TestRelaunchReplacesAPodWhoseInitKeepsFailing starts. It returns the
	// first launch's locator and the Sandbox that the launch made.
	launched := func(t *testing.T, config func(*Options), log *logBuffer) (*rig, runtime.Locator, *sandbox) {
		t.Helper()
		g := newRig(t, nil, withOptions(config), withLog(log))
		old := g.spawn(workerSpec(t))
		if err := g.r.Suspend(g.ctx, old); err != nil {
			t.Fatal(err)
		}
		return g, old, g.sandbox(name)
	}
	// fail edits the issue's pod as edit says, and waits until the pod store holds the edit.
	fail := func(t *testing.T, g *rig, edit func(*corev1.Pod)) {
		t.Helper()
		g.editPod(name, "the pod store to see the failed init container", edit)
	}
	// goesOnSuspension makes the cluster finish the end of the issue's pod, which the test marked as
	// being deleted, when a patch suspends its Sandbox. The rig's controller ends only the pods that
	// it began to end itself.
	goesOnSuspension := func(t *testing.T, g *rig) {
		g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			patch := a.(k8stesting.PatchAction)
			if patch.GetName() == name && modePatched(t, string(patch.GetPatch()), modeSuspended) {
				_ = g.kube.Tracker().Delete(podsGVR, testNamespace, name)
			}
			return false, nil, nil
		})
	}
	// next is the issue's next launch of the tester, at its next generation, with the actions
	// before it cleared.
	next := func(t *testing.T, g *rig) runtime.Locator {
		t.Helper()
		g.clearActions()
		spec := workerSpec(t)
		spec.Generation = 2
		return g.spawn(spec)
	}
	// replacements are the lines of log that a replacement of the issue's Sandbox wrote.
	replacements := func(log *logBuffer) []string { return linesHolding(log.String(), "replacing the issue's Sandbox") }
	// patches are the bodies of g's patches of the issue's Sandbox.
	patches := func(g *rig) []string {
		var bodies []string
		for _, w := range g.writes() {
			if w.resource == "sandboxes" && w.verb == "patch" && w.name == name {
				bodies = append(bodies, w.patch)
			}
		}
		return bodies
	}
	// mark is the reset mark on the issue's Sandbox as the API holds it, and whether it has one.
	mark := func(g *rig) (string, bool) {
		value, marked := g.sandbox(name).Annotations[volumeResetAnnotation]
		return value, marked
	}
	// reset fails the test unless the launch that answered loc reset the kept volume of before,
	// the Sandbox as the launch found it, whose pod oldPod failed in container. Its writes are, in
	// order: the Suspended patch where before is Running, the delete of the Sandbox, the create of
	// one with a new uid that owns the kept template of before, the Secrets, and the Running patch.
	// The new Sandbox carries no mark and owns the tester's Secret. Its new pod mounts the claim.
	// The log holds one replacement line, which names the issue, the old uid, the claim and
	// container.
	reset := func(t *testing.T, g *rig, log *logBuffer, loc runtime.Locator, before *sandbox, oldPod, container string) {
		t.Helper()
		want := []string{"delete sandbox", "create sandbox", "secrets", "run", "record addresses"}
		if before.mode() == modeRunning {
			want = append([]string{"suspend"}, want...)
		}
		var got []string
		for _, step := range steps(t, g.writes(), name) {
			// The rig has no garbage collector, so the provisioning Secret of the deleted Sandbox
			// can still be there for the launch to replace: each write of it is one step.
			if strings.HasSuffix(step, " secret") {
				step = "secrets"
			}
			if len(got) == 0 || got[len(got)-1] != step {
				got = append(got, step)
			}
		}
		expectSteps(t, got, want...)
		writes := g.writes()
		created := slices.IndexFunc(writes, func(a action) bool {
			return a.resource == "sandboxes" && a.verb == "create" && a.name == name
		})
		role := slices.IndexFunc(writes, func(a action) bool {
			return a.resource == "secrets" && a.verb == "create" && a.name == roleSecretName(name, claim.RoleTester)
		})
		running := slices.IndexFunc(writes, func(a action) bool {
			return a.resource == "sandboxes" && a.verb == "patch" && a.name == name && modePatched(t, a.patch, modeRunning)
		})
		if created < 0 || role < created || running < role {
			t.Fatalf("writes %v: the create is write %d, the tester's Secret %d, the Running patch %d; want the create, then the Secrets, then the Running patch",
				writes, created, role, running)
		}
		s := g.sandbox(name)
		if s == nil || s.UID == before.UID || len(s.Spec.VolumeClaimTemplates) != 1 || !ownsKeptVolume(s) ||
			!sameClaimTemplate(s.Spec.VolumeClaimTemplates[0], before.Spec.VolumeClaimTemplates[0]) {
			t.Fatalf("the issue's Sandbox = %+v, want one with a new uid in place of %s that owns its kept template", s, before.UID)
		}
		if value, marked := s.Annotations[volumeResetAnnotation]; marked {
			t.Fatalf("the Sandbox that the reset created carries the mark %q, want none", value)
		}
		if secret := g.secret(roleSecretName(name, claim.RoleTester)); secret == nil || len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].UID != s.UID {
			t.Fatalf("the tester's Secret is %+v, want it owned by the new Sandbox %s", secret, s.UID)
		}
		pod := g.pod(name)
		if pod == nil || string(pod.UID) != loc.Sandbox.PodUID || loc.Sandbox.PodUID == oldPod {
			t.Fatalf("pod %+v, locator %+v; want the new pod of the new Sandbox, not %s", pod, loc, oldPod)
		}
		if disk := podDiskSource(t, pod.Spec).PersistentVolumeClaim; disk == nil || disk.ClaimName != claimName {
			t.Fatalf("the new pod takes its disk from %+v, want the claim %s", disk, claimName)
		}
		lines := replacements(log)
		if len(lines) != 1 {
			t.Fatalf("the launch logged %d replacements of the issue's Sandbox, want one: %q", len(lines), log.String())
		}
		for _, want := range []string{"issue=" + testTree, "uid=" + string(before.UID), "claim=" + claimName, container} {
			if !strings.Contains(lines[0], want) {
				t.Errorf("the reset logged %q, want it to name %s", lines[0], want)
			}
		}
	}
	// keeps fails the test unless the launch that answered loc kept before, the Sandbox as the
	// launch found it, with its uid, deleted and created no Sandbox, and logged no replacement.
	// Where podReplaced is set, the launch runs in a new pod in place of oldPod.
	keeps := func(t *testing.T, g *rig, log *logBuffer, loc runtime.Locator, before *sandbox, oldPod string, podReplaced bool) {
		t.Helper()
		if made := sandboxRemakes(g, name); len(made) > 0 {
			t.Fatalf("the launch wrote %v of the issue's Sandbox, want it kept", made)
		}
		if s := g.sandbox(name); s == nil || s.UID != before.UID {
			t.Fatalf("the issue's Sandbox = %+v, want %s kept", s, before.UID)
		}
		if lines := replacements(log); len(lines) > 0 {
			t.Fatalf("the launch logged a replacement of the issue's Sandbox: %q", lines)
		}
		if podReplaced && loc.Sandbox.PodUID == oldPod {
			t.Fatalf("the launch kept the pod %s, whose init container failed", oldPod)
		}
	}
	// takeDowns are the two acts that take a kept pod down and leave the issue in the workflow: the
	// update round's ReplacePod, and the release. later names the annotation that each patch after
	// the suspension writes: the release records, once it has deleted the Secrets, that it deleted
	// them (secretsReleasedAnnotation).
	takeDowns := []struct {
		name  string
		act   func(*testing.T, *rig, runtime.Locator)
		later []string
	}{
		{"ReplacePod", func(t *testing.T, g *rig, loc runtime.Locator) {
			t.Helper()
			if err := g.r.ReplacePod(g.ctx, runtime.Known{Claim: workerToken, Locator: &loc}, stillDue); err != nil {
				t.Fatalf("ReplacePod: %v", err)
			}
		}, nil},
		{"ReleaseIssue", func(t *testing.T, g *rig, _ runtime.Locator) {
			t.Helper()
			if _, err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized, nil); err != nil {
				t.Fatalf("ReleaseIssue: %v", err)
			}
		}, []string{secretsReleasedAnnotation}},
	}
	// ended is an init container that ended with code and reason after restarts restarts.
	ended := func(container string, restarts, code int32, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: container, RestartCount: restarts, State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: reason},
		}}
	}
	// restarting is an init container that the kubelet restarts after restarts restarts, the last
	// of which ended with code.
	restarting := func(container string, restarts, code int32) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: container, RestartCount: restarts,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: code, Reason: "Error"}},
		}
	}
	// initFailed is workspace-init ended 1 past its restart grace, which is the pod's death: an end
	// inside the grace, in a Pending pod, is the pod still provisioning (startingAgain).
	initFailed := ended(initContainer, initRestartGrace+1, 1, "Error")
	// status sets the pod's phase and its init containers' statuses.
	status := func(phase corev1.PodPhase, statuses ...corev1.ContainerStatus) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: phase, InitContainerStatuses: statuses}
		}
	}
	// deleting is edit on a pod that is being deleted.
	deleting := func(edit func(*corev1.Pod)) func(*corev1.Pod) {
		return func(p *corev1.Pod) {
			edit(p)
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}
	}

	for _, tc := range []struct {
		name, container string
		edit            func(*corev1.Pod)
	}{
		{"workspace-init ended 1", initContainer, status(corev1.PodPending, initFailed)},
		{"workspace-init failed with 1 past its restart grace", initContainer,
			status(corev1.PodPending, restarting(initContainer, initRestartGrace+1, 1))},
		// The kubelet's end of a container it lost is exit 137 with the reason ContainerStatusUnknown,
		// which keeps the volume (TestAKeptVolumeOutlivesAnInitContainerTheKubeletLost). The rule
		// reads the reason, so an exit 137 with the reason Error resets the volume.
		{"workspace-init ended 137 with the reason Error", initContainer, status(corev1.PodPending, ended(initContainer, initRestartGrace+1, 137, "Error"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &logBuffer{}
			g, old, _ := launched(t, kept, log)
			fail(t, g, tc.edit)
			before := g.sandbox(name)
			reset(t, g, log, next(t, g), before, old.Sandbox.PodUID, tc.container)
		})
	}

	t.Run("workspace-init ended 3 past its restart grace", func(t *testing.T) {
		log := &logBuffer{}
		g, old, before := launched(t, kept, log)
		fail(t, g, status(corev1.PodPending, restarting(initContainer, initRestartGrace+1, workspace.InitExitOutsideVolume)))
		keeps(t, g, log, next(t, g), before, old.Sandbox.PodUID, true)
	})

	for _, down := range takeDowns {
		t.Run("workspace-init ended 1, and "+down.name+" took the pod down", func(t *testing.T) {
			log := &logBuffer{}
			g, old, _ := launched(t, kept, log)
			fail(t, g, status(corev1.PodPending, initFailed))
			g.clearActions()
			down.act(t, g, old)
			bodies := patches(g)
			if len(bodies) != 1+len(down.later) || !isMergePatch(bodies[0]) || !modePatched(t, bodies[0], modeSuspended) ||
				!strings.Contains(bodies[0], volumeResetAnnotation) {
				t.Fatalf("%s patched the Sandbox with %q, want one merge patch that suspends it and marks it, then one patch for each of %v", down.name, bodies, down.later)
			}
			for i, annotation := range down.later {
				if body := bodies[1+i]; !isMergePatch(body) || !strings.Contains(body, annotation) || strings.Contains(body, volumeResetAnnotation) || strings.Contains(body, "operatingMode") {
					t.Fatalf("%s's patch %d after the suspension is %q, want a merge patch of %s alone", down.name, 1+i, body, annotation)
				}
			}
			want := initContainer + " exited 1 in pod " + old.Sandbox.PodUID
			if value, _ := mark(g); value != want {
				t.Fatalf("%s left the mark %q on the Sandbox, want %q", down.name, value, want)
			}
			before := g.sandbox(name)
			reset(t, g, log, next(t, g), before, old.Sandbox.PodUID, initContainer)
		})
	}

	// A launch that resets the kept volume of a Sandbox already Suspended has no suspension patch
	// to carry the mark. So it writes the mark in a patch of its own before it waits the pod out
	// and deletes the Sandbox. The second rig runs no controller, so the failed pod never goes and
	// the launch stops in its wait, before its delete. The mark is then on the Sandbox, and the next
	// launch, which reads no pod, resets the volume on it.
	t.Run("workspace-init ended 1 under a Sandbox already Suspended, and the launch stopped before its delete", func(t *testing.T) {
		first, old, _ := launched(t, kept, &logBuffer{})
		fail(t, first, status(corev1.PodPending, initFailed))
		g, _ := first.suspendedCopy(name, kept)
		before := g.sandbox(name)
		g.eventually("the pod store to hold the failed pod", func() bool { return g.r.storedPod(name) != nil })
		spec := workerSpec(t)
		spec.Generation = 2
		if _, err := g.r.Spawn(g.ctx, spec); err == nil || !strings.Contains(err.Error(), "wait out its previous pod") {
			t.Fatalf("the launch = %v, want it to fail waiting out the failed pod", err)
		}
		if made := sandboxRemakes(g, name); len(made) > 0 {
			t.Fatalf("the launch that stopped in its wait wrote %v of the Sandbox, want no delete yet", made)
		}
		bodies := patches(g)
		want := initContainer + " exited 1 in pod " + old.Sandbox.PodUID
		if len(bodies) != 1 || !isMergePatch(bodies[0]) || !strings.Contains(bodies[0], volumeResetAnnotation) || strings.Contains(bodies[0], "operatingMode") {
			t.Fatalf("the launch patched the Sandbox with %q, want one merge patch of the reset mark alone", bodies)
		}
		if value, _ := mark(g); value != want {
			t.Fatalf("the launch left the mark %q on the Sandbox, want %q", value, want)
		}
		if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
			t.Fatal(err)
		}
		g.eventually("the pod store to lose the failed pod", func() bool { return g.r.storedPod(name) == nil })
		g.clearActions()
		spec.Generation = 3
		// The rig runs no controller, so this launch fails after its reset, in a wait for an
		// object that the controller makes. Its writes are what this case reads.
		_, _ = g.r.Spawn(g.ctx, spec)
		if made := sandboxRemakes(g, name); !slices.Equal(made, []string{"delete", "create"}) {
			t.Fatalf("the next launch wrote %v of the Sandbox, want the reset's delete and create", made)
		}
		if s := g.sandbox(name); s == nil || s.UID == before.UID {
			t.Fatalf("after the next launch the issue's Sandbox is %+v, want one with a new uid in place of %s", s, before.UID)
		}
	})

	// A launch that fails once its Running patch was sent suspends the Sandbox again
	// (suspendFailedLaunch), and the pod that the patch made goes with that suspension. When the
	// init container of that pod failed on the kept volume first, the next launch reads no pod. So
	// the suspension writes the reset mark from the pod, and the next launch resets the volume on
	// the mark. A failed launch whose pod shows no such failure writes no mark, and the next launch
	// keeps the volume. Each rig suspends the Sandbox of its first launch, so that the next launch
	// sends a Running patch. The API server applies that patch, and the launch loses its answer once
	// the pod store holds the pod that the patch made, with edit applied to it.
	for _, tc := range []struct {
		name   string
		edit   func(*corev1.Pod)
		resets bool
	}{
		{"workspace-init ended 1 in the pod of a launch that failed after its Running patch", status(corev1.PodPending, initFailed), true},
		{"the init containers passed in the pod of a launch that failed after its Running patch", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &logBuffer{}
			g, _, _ := launched(t, kept, log)
			if err := g.r.setMode(g.ctx, g.sandbox(name), modeSuspended); err != nil {
				t.Fatal(err)
			}
			g.eventually("the pod of the Suspended Sandbox to go", func() bool { return g.pod(name) == nil && g.r.storedPod(name) == nil })
			apply := k8stesting.ObjectReaction(generationTracker{g.dyn.Tracker()})
			var lost atomic.Bool
			made := ""
			g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
				patch := a.(k8stesting.PatchAction)
				if patch.GetName() != name || !modePatched(t, string(patch.GetPatch()), modeRunning) || !lost.CompareAndSwap(false, true) {
					return false, nil, nil
				}
				if _, _, err := apply(a); err != nil {
					return true, nil, err
				}
				g.eventually("the pod that the Running patch made", func() bool { return g.pod(name) != nil })
				made = string(g.pod(name).UID)
				if tc.edit != nil {
					g.editPod(name, "the pod store to see the failed init container", tc.edit)
				}
				g.eventually("the pod store to hold the pod that the Running patch made", func() bool {
					p := g.r.storedPod(name)
					return p != nil && string(p.UID) == made
				})
				return true, nil, errors.New("the answer to the Running patch was lost")
			})
			spec := workerSpec(t)
			spec.Generation = 2
			g.launcher(spec.Claim)
			if err := failedLaunch(t, g, spec); err == nil || !strings.Contains(err.Error(), "set its sandbox running") {
				t.Fatalf("the launch = %v, want it to fail at its Running patch", err)
			}
			value, marked := mark(g)
			if want := initContainer + " exited 1 in pod " + made; tc.resets && value != want {
				t.Fatalf("the failed launch left the mark %q on the Sandbox, want %q", value, want)
			}
			if !tc.resets && marked {
				t.Fatalf("the failed launch marked the Sandbox with %q for a pod whose init containers passed", value)
			}
			g.eventually("the pod of the failed launch to go", func() bool { return g.pod(name) == nil && g.r.storedPod(name) == nil })
			before := g.sandbox(name)
			g.clearActions()
			spec.Generation = 3
			loc := g.spawn(spec)
			if tc.resets {
				reset(t, g, log, loc, before, made, initContainer)
			} else {
				keeps(t, g, log, loc, before, made, true)
			}
		})
	}

	t.Run("workspace-init ended 2", func(t *testing.T) {
		log := &logBuffer{}
		g, old, before := launched(t, kept, log)
		fail(t, g, status(corev1.PodPending, ended(initContainer, initRestartGrace+1, workspace.InitExitUsage, "Error")))
		keeps(t, g, log, next(t, g), before, old.Sandbox.PodUID, true)
	})

	// A pod that is being deleted, or that the kubelet ended itself, keeps its volume: the init
	// container ended because the pod ended, and that move is what the volume is kept for. The
	// kubelet's eviction for a storage limit sets status.reason Evicted and writes no condition. A
	// graceful node shutdown sets status.reason Terminated and writes the condition DisruptionTarget
	// with reason TerminationByKubelet (kubernetes v1.33.0, pkg/kubelet).
	for _, tc := range []struct {
		name     string
		deleting bool
		edit     func(*corev1.Pod)
	}{
		{"being deleted, workspace-init ended 1", true, deleting(status(corev1.PodPending, initFailed))},
		{"being deleted and Failed, workspace-init ended 137", true, deleting(status(corev1.PodFailed, ended(initContainer, 1, 137, "Error")))},
		{"evicted by the kubelet for its storage, workspace-init ended 1", false, func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				Reason:                "Evicted",
				Message:               "Pod ephemeral local storage usage exceeds the total limit of containers 50Gi. ",
				InitContainerStatuses: []corev1.ContainerStatus{initFailed},
			}
		}},
		{"terminated by the kubelet for a node shutdown, workspace-init ended 1", false, func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				Reason:                "Terminated",
				Message:               "Pod was terminated in response to imminent node shutdown.",
				Conditions:            []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "TerminationByKubelet"}},
				InitContainerStatuses: []corev1.ContainerStatus{initFailed},
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// failed is a kept rig whose pod failed as tc says.
			failed := func(t *testing.T, log *logBuffer) (*rig, runtime.Locator, *sandbox) {
				t.Helper()
				g, old, before := launched(t, kept, log)
				if tc.deleting {
					goesOnSuspension(t, g)
				}
				fail(t, g, tc.edit)
				return g, old, before
			}
			t.Run("the next launch", func(t *testing.T) {
				log := &logBuffer{}
				g, old, before := failed(t, log)
				keeps(t, g, log, next(t, g), before, old.Sandbox.PodUID, true)
			})
			for _, down := range takeDowns {
				t.Run(down.name, func(t *testing.T) {
					log := &logBuffer{}
					g, old, before := failed(t, log)
					down.act(t, g, old)
					if value, marked := mark(g); marked {
						t.Fatalf("%s marked the Sandbox with %q for a pod whose volume is kept", down.name, value)
					}
					keeps(t, g, log, next(t, g), before, old.Sandbox.PodUID, true)
				})
			}
		})
	}

	// The taint manager marks a pod with the condition DisruptionTarget and its own reason before
	// it deletes the pod. That condition is no end of the kubelet's own, so a pod that carries it
	// and is not being deleted resets the volume.
	t.Run("a DisruptionTarget of another reason, workspace-init ended 1", func(t *testing.T) {
		log := &logBuffer{}
		g, old, _ := launched(t, kept, log)
		fail(t, g, func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				Conditions:            []corev1.PodCondition{{Type: corev1.DisruptionTarget, Status: corev1.ConditionTrue, Reason: "DeletionByTaintManager"}},
				InitContainerStatuses: []corev1.ContainerStatus{initFailed},
			}
		})
		before := g.sandbox(name)
		reset(t, g, log, next(t, g), before, old.Sandbox.PodUID, initContainer)
	})

	t.Run("workspace-init ended OOMKilled", func(t *testing.T) {
		log := &logBuffer{}
		g, old, before := launched(t, kept, log)
		fail(t, g, status(corev1.PodPending, ended(initContainer, initRestartGrace+1, 137, "OOMKilled")))
		keeps(t, g, log, next(t, g), before, old.Sandbox.PodUID, true)
	})

	t.Run("workspace-init failed inside its restart grace while the pod is Pending", func(t *testing.T) {
		log := &logBuffer{}
		g, old, before := launched(t, kept, log)
		fail(t, g, status(corev1.PodPending, restarting(initContainer, 1, 1)))
		loc := next(t, g)
		keeps(t, g, log, loc, before, old.Sandbox.PodUID, false)
		if loc.Sandbox.PodUID != old.Sandbox.PodUID {
			t.Fatalf("the launch replaced the pod %s with %s, want it kept: workspace-init is still provisioning", old.Sandbox.PodUID, loc.Sandbox.PodUID)
		}
	})

	t.Run("an ephemeral volume, workspace-init ended 1", func(t *testing.T) {
		log := &logBuffer{}
		g, old, before := launched(t, sandboxBlock(SandboxMicroVM), log)
		if len(before.Spec.VolumeClaimTemplates) != 0 {
			t.Fatalf("the Sandbox built with keep_volume off owns %+v, or this case proves nothing", before.Spec.VolumeClaimTemplates)
		}
		fail(t, g, status(corev1.PodPending, initFailed))
		keeps(t, g, log, next(t, g), before, old.Sandbox.PodUID, true)
	})
}

// A launch whose new pod's workspace-init fails past its restart grace (initRestartGrace) returns at
// once, without waiting out the launcher's bound, and the supervisor reads the failed init.
func TestColdInitFailureReachesSupervisionWithoutWaitingForLauncher(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.autoStart.Store(false)
	type launchResult struct {
		loc runtime.Locator
		err error
	}
	result := make(chan launchResult, 1)
	spec := workerSpec(t)
	go func() { loc, err := g.r.Spawn(g.ctx, spec); result <- launchResult{loc, err} }()
	name := SandboxName(workerToken)
	g.eventually("the new Pending pod", func() bool { return g.pod(name) != nil })
	g.update(g.pod(name), func(p *corev1.Pod) {
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: initContainer, RestartCount: initRestartGrace + 1,
			State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		}}
	})
	launched := <-result
	if launched.err != nil {
		t.Fatalf("init failure was hidden as a launcher timeout: %v", launched.err)
	}
	obs, err := g.r.Probe(g.ctx, launched.loc)
	if err != nil || obs.Kind != runtime.Gone || !g.initFailed(launched.loc) {
		t.Fatalf("supervisor cannot observe the confirmed init failure: %+v, %v", obs, err)
	}
}

// The kubelet restarts workspace-init by itself under restartPolicy Always. Its first three
// restarts keep a clone already renamed into place and retry the transfer delta, so each is the pod
// still provisioning — to failedInit, which an observation reads a pod's death out of, and to
// provisioning, which a launch's wait reads a pod coming up by, alike — and the fourth restart is
// the pod's death. An always-failing refusal pays up to four attempts under the kubelet's back-off;
// the retry cost is worth avoiding a new clone after a transient transfer failure. A restart in a
// pod that has already failed is not provisioning.
func TestAWorkspaceInitsFirstThreeRestartsAreStartingAndItsFourthIsAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		restarts   int32
		terminated bool
		phase      corev1.PodPhase
		dead       bool
	}{
		{name: "workspace-init just ended, before the kubelet restarts it", terminated: true},
		{name: "workspace-init's first restart", restarts: 1},
		{name: "workspace-init's third restart", restarts: 3},
		{name: "workspace-init's fourth restart", restarts: 4, dead: true},
		{name: "workspace-init restarting in a pod that has failed", restarts: 1, phase: corev1.PodFailed, dead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phase := corev1.PodPending
			if tc.phase != "" {
				phase = tc.phase
			}
			ended := &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}
			status := corev1.ContainerStatus{Name: initContainer, RestartCount: tc.restarts}
			if tc.terminated {
				status.State.Terminated = ended
			} else {
				status.State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
				status.LastTerminationState.Terminated = ended
			}
			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: phase, InitContainerStatuses: []corev1.ContainerStatus{status}}}

			named, failed := failedInit(pod)
			if (failed != nil) != tc.dead {
				t.Errorf("failedInit = %q, %+v; want the pod dead %t", named, failed, tc.dead)
			}
			if tc.dead && named != initContainer {
				t.Errorf("failedInit named %q, want the pod's %s", named, initContainer)
			}
			if got := provisioning(pod); got == tc.dead {
				t.Errorf("provisioning = %t while failedInit reads the pod dead %t: the two readings of %s disagree", got, tc.dead, initContainer)
			}
		})
	}
}

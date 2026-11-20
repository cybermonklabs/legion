package sandbox

import (
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// A launch that resets a kept volume first suspends the Sandbox, then waits its pod out, and only
// then deletes the Sandbox and creates it again (replaceSupersededSandbox). A launch can fail
// between those two steps: its wait for the pod to go runs out, the daemon stops, or the delete is
// refused. That leaves a Suspended Sandbox and a pod that is on its way out. The next launch reads
// no pod, or one that is being deleted, so the reset mark (volumeResetAnnotation) is all that it
// can go on, as after ReplacePod or the release. R6: a fault on a kept volume that fails an init
// container does not reach a second pod. So the next launch resets the volume.
func TestAKeptVolumeIsResetAfterALaunchThatStoppedBeforeTheReset(t *testing.T) {
	name := SandboxName(workerToken)
	g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
	old := g.spawn(workerSpec(t))
	if err := g.r.Suspend(g.ctx, old); err != nil {
		t.Fatal(err)
	}
	edit := func(p *corev1.Pod) {
		p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
			{Name: fetchContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}}},
			{Name: initContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
		}}
	}
	want := g.pod(name).DeepCopy()
	edit(want)
	g.update(g.pod(name), edit)
	g.eventually("the pod store to see the failed workspace-init", func() bool {
		p := g.r.storedPod(name)
		return p != nil && reflect.DeepEqual(p.Status, want.Status)
	})
	before := g.sandbox(name)
	if before.mode() != modeRunning {
		t.Fatalf("the issue's Sandbox is %s before the launch, want Running", before.mode())
	}

	// The failed pod stays terminating past the boot timeout, so the first launch's wait for it to
	// go runs out after the launch suspended the Sandbox and before it replaced it.
	g.suspendDelay.Store(int64(time.Hour))
	spec := workerSpec(t)
	spec.Generation = 2
	g.launcher(spec.Claim)
	if _, err := g.r.Spawn(g.ctx, spec); err == nil || !strings.Contains(err.Error(), "wait out its previous pod") {
		t.Fatalf("the first launch = %v, want it to fail waiting out the failed pod", err)
	}
	if s := g.sandbox(name); s == nil || s.UID != before.UID || s.mode() != modeSuspended {
		t.Fatalf("after the first launch the issue's Sandbox is %+v, want %s Suspended and not yet replaced", s, before.UID)
	}
	g.suspendDelay.Store(0)
	g.eventually("the failed pod to go", func() bool { return g.pod(name) == nil && g.r.storedPod(name) == nil })

	g.clearActions()
	spec.Generation = 3
	g.spawn(spec)
	if s := g.sandbox(name); s == nil || s.UID == before.UID {
		t.Fatalf("the next launch kept the Sandbox %s, whose kept volume failed workspace-init in pod %s: the fault reaches a second pod (writes %v)",
			before.UID, old.Sandbox.PodUID, sandboxRemakes(g, name))
	}
}

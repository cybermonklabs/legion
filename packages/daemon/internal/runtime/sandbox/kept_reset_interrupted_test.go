package sandbox

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
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

	// The launch stops at its first write after the patch that suspends the Running Sandbox,
	// whatever that write is: the API refuses it, as a daemon stop or a refused request leaves a
	// launch. Only a mark that the suspension patch itself carried is then on the Sandbox. The
	// next launch, which reads the failed pod gone, resets the volume on that mark. A mark written
	// in a separate patch after the suspension is that first write, so this case fails it.
	t.Run("the launch stopped at its first write after its suspension", func(t *testing.T) {
		g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
		old := g.spawn(workerSpec(t))
		if err := g.r.Suspend(g.ctx, old); err != nil {
			t.Fatal(err)
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
		var suspended, stopped atomic.Bool
		stop := func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			if !slices.Contains([]string{"create", "update", "patch", "delete"}, a.GetVerb()) || stopped.Load() {
				return false, nil, nil
			}
			if !suspended.Load() {
				if patch, ok := a.(k8stesting.PatchAction); ok && a.GetResource().Resource == "sandboxes" && patch.GetName() == name &&
					modePatched(t, string(patch.GetPatch()), modeSuspended) {
					suspended.Store(true)
				}
				return false, nil, nil
			}
			stopped.Store(true)
			return true, nil, errors.New("the launch stops at its first write after its suspension")
		}
		g.dyn.PrependReactor("*", "*", stop)
		g.kube.PrependReactor("*", "*", stop)
		spec := workerSpec(t)
		spec.Generation = 2
		g.launcher(spec.Claim)
		if _, err := g.r.Spawn(g.ctx, spec); err == nil || !stopped.Load() {
			t.Fatalf("the first launch = %v (stopped %t), want it stopped at its first write after its suspension", err, stopped.Load())
		}
		if s := g.sandbox(name); s == nil || s.UID != before.UID || s.mode() != modeSuspended {
			t.Fatalf("after the first launch the issue's Sandbox is %+v, want %s Suspended and not yet replaced", s, before.UID)
		}
		g.eventually("the failed pod to go", func() bool { return g.pod(name) == nil && g.r.storedPod(name) == nil })

		g.clearActions()
		spec.Generation = 3
		g.spawn(spec)
		if s := g.sandbox(name); s == nil || s.UID == before.UID {
			t.Fatalf("the next launch kept the Sandbox %s, whose kept volume failed workspace-init in pod %s: the fault reaches a second pod (writes %v)",
				before.UID, old.Sandbox.PodUID, sandboxRemakes(g, name))
		}
	})

	// A launch that resets the kept volume of a Sandbox already Suspended sends no suspension
	// patch, so it writes the mark in a patch of its own before its delete. The rig runs no
	// controller: the copies of the Sandbox, Suspended, and of its failed pod stay as written, and
	// the pod goes when the launch first patches the Sandbox. The API refuses the delete of the
	// Sandbox, so the launch stops there. The next launch, which reads no pod, resets the volume on
	// the mark. A launch that writes no mark of its own for such a Sandbox fails this case.
	t.Run("the launch into a Sandbox already Suspended stopped at its delete", func(t *testing.T) {
		first := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
		old := first.spawn(workerSpec(t))
		if err := first.r.Suspend(first.ctx, old); err != nil {
			t.Fatal(err)
		}
		want := first.pod(name).DeepCopy()
		edit(want)
		first.update(first.pod(name), edit)
		first.eventually("the pod store to see the failed workspace-init", func() bool {
			p := first.r.storedPod(name)
			return p != nil && reflect.DeepEqual(p.Status, want.Status)
		})
		object, err := first.dyn.Tracker().Get(sandboxGVR, testNamespace, name)
		if err != nil {
			t.Fatal(err)
		}
		// The second rig's API stamps uids from its own count, so the copies carry a uid that no
		// Sandbox that rig creates holds.
		copied := object.(*unstructured.Unstructured).DeepCopy()
		copied.SetResourceVersion("")
		copied.SetUID("uid-sandbox-suspended")
		if err := unstructured.SetNestedField(copied.Object, modeSuspended, "spec", "operatingMode"); err != nil {
			t.Fatal(err)
		}
		pod := first.pod(name).DeepCopy()
		pod.ResourceVersion = ""
		pod.OwnerReferences[0].UID = copied.GetUID()
		first.stop()

		g := newRig(t, []k8sruntime.Object{copied, pod}, withoutController(), withOptions(keptVolumes(SandboxMicroVM)))
		g.eventually("the pod store to hold the failed pod", func() bool { return g.r.storedPod(name) != nil })
		var deletes atomic.Int32
		g.dyn.PrependReactor("patch", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			if a.(k8stesting.PatchAction).GetName() == name {
				_ = g.kube.Tracker().Delete(podsGVR, testNamespace, name)
			}
			return false, nil, nil
		})
		g.dyn.PrependReactor("delete", "sandboxes", func(a k8stesting.Action) (bool, k8sruntime.Object, error) {
			if a.(k8stesting.DeleteAction).GetName() != name || deletes.Add(1) > 1 {
				return false, nil, nil
			}
			return true, nil, errors.New("the launch stops at its delete of the Sandbox")
		})
		spec := workerSpec(t)
		spec.Generation = 2
		if _, err := g.r.Spawn(g.ctx, spec); err == nil || !strings.Contains(err.Error(), "the launch stops at its delete") {
			t.Fatalf("the first launch = %v, want it stopped at its delete of the Sandbox", err)
		}
		if s := g.sandbox(name); s == nil || s.UID != copied.GetUID() {
			t.Fatalf("after the first launch the issue's Sandbox is %+v, want %s not yet replaced", s, copied.GetUID())
		}
		g.eventually("the pod store to lose the failed pod", func() bool { return g.r.storedPod(name) == nil })

		g.clearActions()
		spec.Generation = 3
		// The rig runs no controller, so this launch fails after its reset, in a wait for an
		// object that the controller makes. Its writes are what this case reads.
		_, _ = g.r.Spawn(g.ctx, spec)
		if s := g.sandbox(name); s == nil || s.UID == copied.GetUID() {
			t.Fatalf("the next launch kept the Sandbox %s, whose kept volume failed workspace-init in pod %s: the fault reaches a second pod (writes %v)",
				copied.GetUID(), old.Sandbox.PodUID, sandboxRemakes(g, name))
		}
	})
}

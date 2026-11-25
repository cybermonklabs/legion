package sandbox

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// A release of an issue whose pod failed on its kept volume ends that pod as the daemon's own end.
// The release suspends the Sandbox, and the reset mark the pod's failed init container calls for
// (volumeFault) makes that suspension one merge patch of the mode and the mark rather than the JSON
// patch setMode sends. The record of the daemon's end comes from the act the release passes
// (daemonSuspendedAct, suspendOver), not from the patch's shape, so the end reads as the daemon's
// own here as it does for a release of a pod whose volume is sound. With no record the end would
// read as one from outside the pod (deletedByAnother), logged as a warning that something outside
// the pod ended it, which nothing did.
func TestAReleaseEndsAPodWhoseInitFailedOnItsKeptVolumeAsTheDaemonsOwn(t *testing.T) {
	name := SandboxName(workerToken)
	g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
	old := g.spawn(workerSpec(t))
	if err := g.r.Suspend(g.ctx, old); err != nil {
		t.Fatal(err)
	}
	g.editPod(name, "the pod store to see the failed init container", func(p *corev1.Pod) {
		p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
			terminated(fetchContainer, 0, "Completed"), terminated(initContainer, 1, "Error"),
		}}
	})

	if _, err := g.r.ReleaseIssue(g.ctx, testTree, testTree, authorized, nil); err != nil {
		t.Fatalf("ReleaseIssue: %v", err)
	}

	want := initContainer + " exited 1 in pod " + old.Sandbox.PodUID
	if s := g.sandbox(name); s == nil || s.Annotations[volumeResetAnnotation] != want {
		t.Fatalf("the release left the Sandbox %+v, want it kept with the reset mark %q: this case is about the merge patch the mark makes of the suspension", s, want)
	}
	if end := g.awaitEndReadingRecord(old.Sandbox.PodUID); end.reading != runtime.PodEndOurs || end.words != daemonSuspendedAct {
		t.Fatalf("read %+v of the released pod, want %s under the act %q: the release suspended it", end, runtime.PodEndOurs, daemonSuspendedAct)
	}
}

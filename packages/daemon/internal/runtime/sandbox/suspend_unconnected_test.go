package sandbox

import (
	"context"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// inInit is the tester's issue pod still in its init containers, its spec naming both of them:
// workspace-fetch ended 0 and workspace-init's status is init.
func inInit(init corev1.ContainerStatus) *corev1.Pod {
	pod := runningPod(workerToken, recorded, claim.RoleTester)
	pod.Spec.InitContainers = []corev1.Container{{Name: fetchContainer}, {Name: initContainer}}
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{terminated(fetchContainer, 0, "Completed"), init}}
	return pod
}

// failedInitStatuses are the two shapes workspace-init has after it ended with an error: its own
// termination, and the kubelet restarting it under the pod's restartPolicy Always, which waits out
// its backoff carrying that termination as its last.
var failedInitStatuses = map[string]corev1.ContainerStatus{
	"workspace-init terminated with an error": terminated(initContainer, 1, "Error"),
	"workspace-init restarting after an error": {Name: initContainer,
		State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}}},
}

// A pod still in, or failed in, an init container has not started the container its roles'
// launchers run in since those init containers last ran, so no launcher of it connected and no
// role process runs there. A stop of a role recorded in such a pod has nothing to stop: Suspend
// answers nil at once, sends nothing and takes the claim out of the watch, so the claim is
// suspended and the issue's suspension row deletes the Sandbox on its next pass. It used to wait
// the boot timeout and the termination grace for a launcher that could not come and then fail
// "did not connect": on 2026-11-24 a tester whose pods died of a full disk in workspace-init was
// held so from 08:46:47Z to 08:56:57Z.
func TestSuspendOfAPodWhoseLauncherNeverConnectedReturnsAtOnce(t *testing.T) {
	t.Parallel()
	for name, status := range failedInitStatuses {
		t.Run(name, func(t *testing.T) {
			// BootTimeout is this row's measured bound: Suspend's init-container branch makes no
			// wait at all, while the former wrong path waited a boot timeout. A minute leaves room
			// a loaded runner cannot eat, and Bounded keeps this test's failure before its binary
			// deadline on an explicitly short invocation.
			g := newRig(t, []k8sruntime.Object{runningSandbox(t, workerToken, claim.RoleTester), inInit(status)}, withoutController(),
				withMeasuredBootTimeout(testwait.Bounded(t, time.Minute, time.Second)))
			loc := sandboxLocator(workerToken, recorded)
			if err := g.r.ReconcileOrphans(g.ctx, []runtime.Known{{Claim: workerToken, Locator: &loc}}, time.Hour); err != nil {
				t.Fatal(err)
			}
			if _, ok := g.r.recorded(workerToken); !ok {
				t.Fatal("the tester is not in the watch before its Suspend")
			}
			began := time.Now()
			err := g.r.Suspend(g.ctx, loc)
			took := time.Since(began)
			if err != nil {
				t.Fatalf("Suspend of a role whose launcher never connected = %v after %s, want nil at once", err, took)
			}
			if took >= g.opts.BootTimeout {
				t.Fatalf("Suspend of a role whose launcher never connected took %s, want an answer before the boot timeout %s", took, g.opts.BootTimeout)
			}
			if _, ok := g.r.recorded(workerToken); ok {
				t.Fatal("Suspend returned with the tester still in the watch")
			}
			if frames := g.sent(workerToken); len(frames) != 0 {
				t.Fatalf("Suspend sent %v, want nothing: no launcher connected", frames)
			}
		})
	}
}

// A stop of a role whose launcher connected keeps its wait: Suspend sends the launcher one stop of
// the recorded generation and returns only once the launcher has answered it, here held until the
// test lets it go. That holds in a running pod, and in one whose status reads back in
// workspace-init while its launcher's connection still stands, as the kubelet's restart of the
// pod's sandbox leaves it until that connection drops: a connected launcher is the answer to wait
// for, whatever the pod's status says.
func TestSuspendOfAConnectedLauncherStillWaitsForTheStop(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*corev1.Pod){
		"its pod running": nil,
		"its pod back in workspace-init": func(p *corev1.Pod) {
			p.Status = inInit(terminated(initContainer, 1, "Error")).Status
		},
	} {
		t.Run(name, func(t *testing.T) {
			held := make(chan struct{})
			release := sync.OnceFunc(func() { close(held) })
			t.Cleanup(release)
			g := newRig(t, nil, withHeldStops(held))
			loc := g.spawn(workerSpec(t))
			if edit != nil {
				g.editPod(loc.Sandbox.Name, "the pod store to see the pod back in workspace-init", edit)
			}
			answered := make(chan error, 1)
			go func() { answered <- g.r.Suspend(context.WithoutCancel(g.ctx), loc) }()
			g.eventually("the stop to reach the launcher", func() bool { return stopsSent(g, workerToken) == 1 })
			// The launcher holds its answer; Suspend must hold too, for as long as it does.
			if testwait.Holds(testwait.Bounded(t, 300*time.Millisecond, time.Second), testwait.Poll, func() bool { return len(answered) > 0 }) {
				t.Fatalf("Suspend returned %v before the launcher answered its stop", <-answered)
			}
			release()
			if err := testwait.Receive(t, "Suspend's answer once the launcher answered its stop", answered); err != nil {
				t.Fatalf("Suspend after the launcher answered = %v", err)
			}
			if got := stopsSent(g, workerToken); got != 1 {
				t.Fatalf("%d stops sent, want the one", got)
			}
		})
	}
}

// stopsSent is how many stops token's fake launcher received.
func stopsSent(g *rig, token claim.Token) int {
	n := 0
	for _, frame := range g.sent(token) {
		if _, ok := frame.(shimwire.LauncherStop); ok {
			n++
		}
	}
	return n
}

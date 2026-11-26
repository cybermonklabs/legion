package sandbox

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// A container that the kubelet lost — the node or its container runtime restarted while the
// container ran — gets a status that the kubelet writes itself: an end with exit 137 and the reason
// ContainerStatusUnknown. Where the container runtime answers the container's state unknown, that
// end is State.Terminated. Where the runtime no longer knows the container, it is
// LastTerminationState under a Waiting state, and the restart count goes up by one
// (kubernetes v1.36.4, pkg/kubelet/kubelet_pods.go, convertToAPIContainerStatuses). The pod has no
// deletionTimestamp, no DisruptionTarget condition and no status.reason. Such an end is a node
// loss, which keeps the volume (R3), and not a fault on the volume (R6). So the next launch replaces
// the pod and keeps the Sandbox and the claim that it owns.
func TestAKeptVolumeOutlivesAnInitContainerTheKubeletLost(t *testing.T) {
	t.Parallel()
	name := SandboxName(workerToken)
	// lost is container as the kubelet writes it after it lost the container, with restarts
	// restarts: under a Waiting state where waiting is set, and as the container's own state where
	// it is not. Each form carries the kubelet's own message for it.
	lost := func(container string, restarts int32, waiting bool) corev1.ContainerStatus {
		end := &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "ContainerStatusUnknown",
			Message: "The container could not be located when the pod was terminated"}
		status := corev1.ContainerStatus{Name: container, RestartCount: restarts}
		if waiting {
			end.Message = "The container could not be located when the pod was deleted.  The container used to be Running"
			status.State.Waiting = &corev1.ContainerStateWaiting{Reason: "PodInitializing"}
			status.LastTerminationState.Terminated = end
		} else {
			status.State.Terminated = end
		}
		return status
	}
	for _, tc := range []struct {
		name     string
		statuses []corev1.ContainerStatus
	}{
		{"workspace-init lost, the runtime no longer knows it", []corev1.ContainerStatus{lost(initContainer, 1, true)}},
		{"workspace-init lost, the runtime answers its state unknown", []corev1.ContainerStatus{lost(initContainer, 1, false)}},
		{"workspace-init lost past its restart grace", []corev1.ContainerStatus{lost(initContainer, initRestartGrace+1, true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
			old := g.spawn(workerSpec(t))
			if err := g.r.Suspend(g.ctx, old); err != nil {
				t.Fatal(err)
			}
			before := g.sandbox(name)
			edit := func(p *corev1.Pod) {
				p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: tc.statuses}
			}
			g.editPod(name, "the pod store to see the lost init container", edit)
			g.clearActions()
			spec := workerSpec(t)
			spec.Generation = 2
			loc := g.spawn(spec)
			if made := sandboxRemakes(g, name); len(made) > 0 {
				t.Fatalf("the launch wrote %v of the issue's Sandbox after the kubelet lost an init container, want the Sandbox and its kept volume kept", made)
			}
			if s := g.sandbox(name); s == nil || s.UID != before.UID {
				t.Fatalf("the issue's Sandbox = %+v, want %s kept with its volume", s, before.UID)
			}
			if loc.Sandbox.PodUID == old.Sandbox.PodUID {
				t.Fatalf("the launch kept the pod %s, whose init container the kubelet lost", old.Sandbox.PodUID)
			}
		})
	}

	// The rule reads the reason and not the exit code: an init container that ended 137 with the
	// reason Error is a fault on the volume, so the next launch resets it and the Sandbox gets a
	// new uid.
	t.Run("workspace-init ended 137 with the reason Error", func(t *testing.T) {
		g := newRig(t, nil, withOptions(keptVolumes(SandboxMicroVM)))
		old := g.spawn(workerSpec(t))
		if err := g.r.Suspend(g.ctx, old); err != nil {
			t.Fatal(err)
		}
		before := g.sandbox(name)
		edit := func(p *corev1.Pod) {
			p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{{
				Name: initContainer, RestartCount: initRestartGrace + 1,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"}},
			}}}
		}
		g.editPod(name, "the pod store to see the failed init container", edit)
		g.clearActions()
		spec := workerSpec(t)
		spec.Generation = 2
		g.spawn(spec)
		if made := sandboxRemakes(g, name); !slices.Equal(made, []string{"delete", "create"}) {
			t.Fatalf("the launch wrote %v of the issue's Sandbox after workspace-init ended 137 with the reason Error, want the reset's delete and create", made)
		}
		if s := g.sandbox(name); s == nil || s.UID == before.UID {
			t.Fatalf("the issue's Sandbox = %+v, want one with a new uid in place of %s", s, before.UID)
		}
	})
}

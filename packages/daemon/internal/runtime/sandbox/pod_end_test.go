package sandbox

// What the pod's own reason for its end becomes on the observation the supervisor reads
// (runtime.Observation.PodEnd): an eviction for ephemeral storage is written on the pod and
// nowhere else, so it is read on both branches of Runtime.ended — the one where the resolved
// container has a terminated state and the one where it has none (INFRA-2009).

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// evictedMessage is what the kubelet writes on a pod it evicts for the node's disk, as a real
// eviction carries it.
const evictedMessage = "The node was low on resource: ephemeral-storage. Threshold quantity: 10Gi, available: 4Gi. Container roles was using 48Gi, which exceeds its request of 0."

// A pod the kubelet evicts carries its reason and its message on the pod, never on the container:
// the kubelet writes `Evicted` on the pod and then ends the container, so a pod whose container
// also terminated carried that eviction in neither the notice nor the detail before this. Both
// branches of Runtime.ended read it now, and the observation carries it apart from the detail,
// which ends with a tail of the role's own log.
//
// The negative control is the code at the merge base, where only the branch with no terminated
// container state writes the pod's reason: the first case below passes there and the second — an
// evicted pod whose container also ended — fails, with an empty PodEnd and a detail naming the
// container's end alone.
func TestAnEvictedPodsReasonReachesTheObservationOnBothBranches(t *testing.T) {
	labels := claimLabels(claim.RoleTester)
	name := SandboxName(workerToken)
	g := newRig(t, []k8sruntime.Object{sandboxObject(t, name, sandboxUID, modeRunning, labels),
		podObject(name, recorded, sandboxUID, labels, corev1.PodStatus{Phase: corev1.PodRunning})}, withoutController())

	for _, tc := range []struct {
		name   string
		status corev1.PodStatus
	}{
		{
			// The branch that already read the pod: nothing of the container terminated, so the
			// end the detail has is the pod's own.
			name:   "the pod was evicted and its container reports no terminated state",
			status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: evictedMessage},
		},
		{
			// The branch that did not: the kubelet ended the container as well, and the
			// eviction was lost behind it.
			name: "the pod was evicted and its container terminated too",
			status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted", Message: evictedMessage,
				ContainerStatuses: []corev1.ContainerStatus{endedContainer(runtime.IssuePodContainer,
					corev1.ContainerStateTerminated{ExitCode: 137, Reason: "Error"})}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g.update(g.pod(name), func(p *corev1.Pod) { p.Status = tc.status })
			g.eventually("the pod store to see "+tc.name, func() bool {
				pod := g.r.storedPod(name)
				return pod != nil && pod.Status.Phase == corev1.PodFailed && pod.Status.Reason == "Evicted" &&
					len(pod.Status.ContainerStatuses) == len(tc.status.ContainerStatuses)
			})
			obs, err := g.r.Probe(g.ctx, rolesLocator(workerToken, recorded))
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if obs.Kind != runtime.Gone {
				t.Fatalf("the evicted pod observes %s, want Gone: %q", obs.Kind, obs.Detail)
			}
			if want := runtime.PodEnd("Evicted", evictedMessage); obs.PodEnd != want {
				t.Fatalf("the observation carries pod end %q, want %q", obs.PodEnd, want)
			}
			if !strings.Contains(obs.Detail, "Evicted") || !strings.Contains(obs.Detail, "ephemeral-storage") {
				t.Fatalf("the detail does not name the eviction: %q", obs.Detail)
			}
		})
	}
}

// The field is bounded, because it is stored on a claim and published to a role topic, where the
// detail it comes from ends with an unbounded tail of the role's log. A message longer than the
// bound is cut at it; a pod with no reason at all carries nothing, however long its message.
func TestThePodEndIsBoundedAndEmptyWithoutAReason(t *testing.T) {
	if got := runtime.PodEnd("", strings.Repeat("x", 10)); got != "" {
		t.Fatalf("a pod with no reason carries %q, want nothing: the reason is what says the pod ended it", got)
	}
	if got := runtime.PodEnd("Evicted", ""); got != "Evicted" {
		t.Fatalf("a reason with no message carries %q, want the reason alone", got)
	}
	whole := runtime.PodEnd("Evicted", evictedMessage)
	if len([]rune(whole)) > runtime.PodEndRunes || !strings.HasPrefix(whole, "Evicted: ") {
		t.Fatalf("a real eviction carries %q, want it whole and inside the bound", whole)
	}
	long := runtime.PodEnd("Evicted", strings.Repeat("x", 2*runtime.PodEndRunes))
	if runes := []rune(long); len(runes) != runtime.PodEndRunes || runes[len(runes)-1] != '…' {
		t.Fatalf("a message past the bound carries %d runes ending %q, want %d ending in an ellipsis",
			len(runes), string(runes[len(runes)-1]), runtime.PodEndRunes)
	}
}

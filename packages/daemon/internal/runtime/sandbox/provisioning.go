package sandbox

import (
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// An issue pod provisions itself in two init containers, workspace-fetch and then workspace-init,
// before the launcher its roles run in starts. The two readings of those containers' statuses live
// here together: failedInit, which an observation reads a pod's death out of (verdict), and
// provisioning, which a relaunch reads a pod still coming up by (awaitRoleLauncher). They are one
// judgement seen from either side, so a pod cannot be dead to the one and starting to the other.

// fetchRestartGrace is how many times the kubelet may restart a pod's workspace-fetch before a
// non-zero end of it is the pod's death rather than one more start: three, so the fourth restart
// is the failure. The kubelet restarts a failed init container by itself under the pod's
// restartPolicy Always (manifest), and the fetch that comes back finds the feed its own pod's disk
// already holds and brings only what is missing, while a replacement pod downloads the repository
// again onto a fresh disk — what a fetch killed mid-download used to cost: the download thrown
// away and the issue launched again into the same wait (INFRA-495). The grace is counted because a
// fetch that cannot get through at all must still reach the claim's launch budget rather than
// restart for ever.
const fetchRestartGrace = 3

// initEnd is how an init container ended, when it ended non-zero: its own termination, or the one
// it is coming back from, which a container the kubelet is restarting carries in
// LastTerminationState while it waits out its backoff. A later successful termination wins over
// that one.
func initEnd(status corev1.ContainerStatus) *corev1.ContainerStateTerminated {
	ended := status.State.Terminated
	if ended == nil {
		ended = status.LastTerminationState.Terminated
	}
	if ended == nil || ended.ExitCode == 0 {
		return nil
	}
	return ended
}

// startingAgain is whether a non-zero end of pod's init container status is the kubelet bringing
// that container back, the pod still coming up, rather than the pod's death: a workspace-fetch
// inside the grace, in a pod that is still Pending. No init container of a pod that has already
// succeeded or failed is started again, whatever its own status says it did.
func startingAgain(pod *corev1.Pod, status corev1.ContainerStatus) bool {
	return phaseOf(pod) == corev1.PodPending && status.Name == fetchContainer && status.RestartCount <= fetchRestartGrace
}

// failedInit is the init container whose non-zero end is pod's death, and how it ended: no
// launcher can start in such a pod. It includes a failed attempt the kubelet is restarting, since
// nothing ends those on their own, except the workspace-fetch restarts the grace covers, which are
// the pod provisioning (startingAgain).
func failedInit(pod *corev1.Pod) (string, *corev1.ContainerStateTerminated) {
	if pod == nil {
		return "", nil
	}
	for _, status := range pod.Status.InitContainerStatuses {
		if ended := initEnd(status); ended != nil && !startingAgain(pod, status) {
			return status.Name, ended
		}
	}
	return "", nil
}

// provisioning is whether pod is still coming up through its init containers, whichever they are
// (workspace-fetch, then skills-fetch where the deployment names a skills repository, then
// workspace-init): one of them running, or a workspace-fetch between the restarts the grace covers,
// which is the same pod failedInit leaves alive. It is the rule the liveness mapping reads such a
// pod alive by (evaluate), and the rule by which a launch's wait for the pod's launcher keeps
// giving it longer (awaitRoleLauncher).
func provisioning(pod *corev1.Pod) bool {
	if pod == nil || phaseOf(pod) != corev1.PodPending {
		return false
	}
	return slices.ContainsFunc(pod.Status.InitContainerStatuses, func(status corev1.ContainerStatus) bool {
		return status.State.Running != nil || (initEnd(status) != nil && startingAgain(pod, status))
	})
}

// neverStarted is how long pod has been scheduled onto a node that has not made it ready to start
// its containers, and whether that is longer than bound at now. The kubelet reports
// PodReadyToStartContainers False until it has mounted the pod's volumes and made its sandbox, and
// no container of the pod runs before then, an init container or the launcher its roles run in: a
// node that cannot surface the new persistent disk an issue pod owns (FailedMount) holds the pod
// there for as long as the pod lives, and a launch that keeps such a pod waits for its launcher in
// vain at every try (relaunch). Such a pod is Pending, PodScheduled True and
// PodReadyToStartContainers False, counted from that condition's last transition, or from
// PodScheduled's where the condition carries none. Nothing else is judged: a pod whose kubelet has
// not reported the condition, whose wait for its image pull is the launcher wait's own
// (awaitRoleLauncher); a pod that is not scheduled, whose wait is the scheduler's (verdict); a pod
// that has left Pending, which has started; and a pod whose moment is unknown.
func neverStarted(pod *corev1.Pod, now time.Time, bound time.Duration) (time.Duration, bool) {
	if pod == nil || phaseOf(pod) != corev1.PodPending {
		return 0, false
	}
	scheduled, ready := podCondition(pod, corev1.PodScheduled), podCondition(pod, corev1.PodReadyToStartContainers)
	if scheduled == nil || scheduled.Status != corev1.ConditionTrue || ready == nil || ready.Status != corev1.ConditionFalse {
		return 0, false
	}
	since := ready.LastTransitionTime.Time
	if since.IsZero() {
		since = scheduled.LastTransitionTime.Time
	}
	if since.IsZero() {
		return 0, false
	}
	unstarted := now.Sub(since)
	return unstarted, unstarted > bound
}

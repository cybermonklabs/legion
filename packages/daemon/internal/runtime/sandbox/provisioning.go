package sandbox

import (
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// An issue pod provisions itself in workspace-init before the launcher its roles run in starts.
// The readings of that container's status live here together: failedInit, which an observation
// reads a pod's death out of (verdict), and provisioning, which a relaunch reads a pod still coming
// up by (awaitRoleLauncher). They are one judgement seen from either side, so a pod cannot be dead
// to the one and starting to the other. localInitFailure lives here too. It reads failedInit's
// failure again, for a kept volume (keptVolume): whether the failure is the volume's own fault,
// which a new volume cures. And so does beforeLaunchers, the widest of these readings: a pod whose
// launchers cannot have started, which a stop reads as having nothing to stop (Suspend).
// neverStarted lives here too, though it reads a pod's conditions rather than its init containers,
// and judges every pod the runtime launches, the project controller's as well as an issue's: a pod
// scheduled onto a node that never made it ready to start, which a relaunch replaces rather than
// waits on once the pod is older than its start bound (startBound).

// initRestartGrace is how many times the kubelet may restart workspace-init before a non-zero end
// is the pod's death rather than one more start: three, so the fourth restart is the failure. The
// kubelet restarts a failed init container by itself under the pod's restartPolicy Always
// (manifest). A restart keeps a clone already renamed into place and fetches only its delta, where
// a replacement pod starts on a fresh disk. The cost is that a refusal that always fails runs up to
// four times under the kubelet's back-off before the pod counts as dead; that cost is worth the
// retry of the transfers that now run in this container.
const initRestartGrace = 3

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
// that container back, the pod still coming up, rather than the pod's death: workspace-init inside
// the grace, in a pod that is still Pending. No init container of a pod that has already succeeded
// or failed is started again, whatever its own status says it did. A container the kubelet lost
// (the reason ContainerStatusUnknown, which the kubelet writes when the node or its container
// runtime restarted) is a node loss and never inside the grace: the next launch replaces the pod
// and keeps its volume.
func startingAgain(pod *corev1.Pod, status corev1.ContainerStatus) bool {
	if ended := initEnd(status); ended != nil && ended.Reason == "ContainerStatusUnknown" {
		return false
	}
	return phaseOf(pod) == corev1.PodPending && status.Name == initContainer && status.RestartCount <= initRestartGrace
}

// failedInit is the init container whose non-zero end is pod's death, and how it ended: no
// launcher can start in such a pod. It includes a failed attempt the kubelet is restarting, since
// nothing ends those on their own, except the workspace-init restarts the grace covers, which are
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

// localInitFailure is the init container whose end failedInit reads as pod's death, and how it
// ended, when that end is a fault local to the issue's volume. It is the one rule by which a kept
// volume (keptVolume) is reset: the launch reads it (volumeReset), and ReplacePod and the release
// read it to mark the Sandbox (volumeFault). Workspace-init writes the shared clone and the issue's
// workspace. A fault on a kept volume fails it in each pod that mounts the volume, and only a new
// volume cures it.
//
// These ends are not such a fault, and for them it returns "" and nil:
//
//   - An end in a pod that is being deleted (a deletionTimestamp, as verdict reads initFailed). An
//     eviction through the API, a drain or a preemption ends the container with SIGTERM or SIGKILL.
//     That move is what the volume is kept for.
//   - An end in a pod that the kubelet ended itself and did not delete (endedByKubelet).
//   - The exit workspace.InitExitUsage, a usage error, and the exit workspace.InitExitOutsideVolume,
//     a cause outside the volume. A new volume cures neither.
//   - An end with the reason OOMKilled. The container's memory limit caused it, not the volume.
//     outOfMemory answers for the roles' container alone, so this rule reads the reason itself.
//   - An end with the reason ContainerStatusUnknown, exit 137. The kubelet writes this end itself
//     when the node or its container runtime restarted while the container ran, and it lost the
//     container (kubernetes v1.36.4, pkg/kubelet/kubelet_pods.go, convertToAPIContainerStatuses).
//     The end is the container's own state, or the end under its Waiting state where the runtime
//     no longer knows it. The pod gets no deletionTimestamp and no DisruptionTarget. Such an end
//     is a node loss, which keeps the volume, as an end in a pod that is being deleted does.
//
// A workspace-init in a Pending pod that its grace still restarts is no failure (failedInit).
func localInitFailure(pod *corev1.Pod) (string, *corev1.ContainerStateTerminated) {
	name, ended := failedInit(pod)
	switch {
	case ended == nil, pod.DeletionTimestamp != nil, endedByKubelet(pod):
		return "", nil
	case ended.ExitCode == workspace.InitExitUsage, ended.ExitCode == workspace.InitExitOutsideVolume,
		ended.Reason == "OOMKilled", ended.Reason == "ContainerStatusUnknown":
		return "", nil
	}
	return name, ended
}

// endedByKubelet is whether the kubelet ended pod itself and left it in the API, as the kubelet's
// own code writes such a pod (kubernetes v1.33.0, pkg/kubelet). Each eviction sets status.reason
// Evicted. The eviction for node pressure writes the condition DisruptionTarget too, and the
// evictions for a storage limit write no condition. A graceful node shutdown (status.reason
// Terminated) and the kubelet's preemption for a critical pod (Preempting) write the condition
// DisruptionTarget with the reason TerminationByKubelet. This reads the condition's type and
// reason alone. A DisruptionTarget of another reason, such as the taint manager's
// DeletionByTaintManager, is not an end by the kubelet. kubeletFailure, which reads each
// DisruptionTarget of a Failed pod as the kubelet's, answers another question.
func endedByKubelet(pod *corev1.Pod) bool {
	return pod.Status.Reason == "Evicted" || slices.ContainsFunc(pod.Status.Conditions, func(condition corev1.PodCondition) bool {
		return condition.Type == corev1.DisruptionTarget && condition.Reason == corev1.PodReasonTerminationByKubelet
	})
}

// provisioning is whether pod is still coming up through its init containers (an issue pod's
// workspace-init; the controller's pod has none): it is running, or between the restarts the grace
// covers, which is the same pod failedInit leaves alive. It is the rule the liveness mapping reads
// such a pod alive by (evaluate), and the rule by which a launch's wait for the pod's launcher
// keeps giving it longer (awaitRoleLauncher).
func provisioning(pod *corev1.Pod) bool {
	if pod == nil || phaseOf(pod) != corev1.PodPending {
		return false
	}
	return slices.ContainsFunc(pod.Status.InitContainerStatuses, func(status corev1.ContainerStatus) bool {
		return status.State.Running != nil || (initEnd(status) != nil && startingAgain(pod, status))
	})
}

// beforeLaunchers is whether pod is in, or failed in, one of its init containers: Pending, with an
// init container its spec names that has no status yet, or one that has not ended 0 — waiting,
// running, or ended non-zero, restarting or not. The kubelet starts the container a pod's role
// launchers run in only once every init container has ended 0, holds the pod Pending until then,
// and runs them all again before it starts that container again after the pod's sandbox is made
// anew, so no launcher of such a pod has run since its init containers last began, and no role's
// process runs in it. It is wider than failedInit and provisioning together: a pod still pulling
// its images, and a workspace-init the grace restarts, are in it too, since neither has started a
// launcher either. A pod past Pending is never this, whatever its statuses list, and neither is the
// controller's pod, which runs no init container.
func beforeLaunchers(pod *corev1.Pod) bool {
	if pod == nil || phaseOf(pod) != corev1.PodPending {
		return false
	}
	return slices.ContainsFunc(pod.Spec.InitContainers, func(c corev1.Container) bool {
		status := initContainerStatus(pod, c.Name)
		return status == nil || status.State.Terminated == nil || status.State.Terminated.ExitCode != 0
	})
}

// startingGrace is the longest neverStarted leaves a pod just scheduled onto a node to start,
// whatever the bound it judges against: a minute, several times the 9 to 15 s an issue pod's node
// takes from the pod's scheduling to reporting it ready to start its containers.
const startingGrace = time.Minute

// keptVolumeAttachWait is how long a launch gives a pod with a kept volume (keptVolume) to start,
// where the boot timeout is shorter (startBound, Runtime.keptVolumeBound). A ReadWriteOnce volume
// attaches to one node. When the node that held it stops answering, the volume stays attached
// there. Once the old pod is gone from the API, the upstream attach-detach controller
// force-detaches the volume after 6 minutes (ReconcilerMaxWaitForUnmountDuration). The attach to
// the new pod's node takes up to one minute more. Until then the new pod is Pending and its node
// has not made it ready to start its containers, which neverStarted reads as a pod that never
// started once it is older than its bound.
const keptVolumeAttachWait = 7 * time.Minute

// neverStarted is how long ago pod was created and how long ago it was scheduled onto a node that
// has not made it ready to start its containers, and whether it has never started at now: created
// longer than bound ago, and scheduled longer than its margin ago, half of bound or startingGrace,
// whichever is less. The kubelet reports PodReadyToStartContainers False until it has mounted the
// pod's volumes and made its sandbox, and no container of the pod runs before then, an init
// container or the launcher its roles run in: a node that cannot surface the new persistent disk an
// issue pod owns (FailedMount) holds the pod there for as long as the pod lives, and a launch that
// keeps such a pod waits for its launcher in vain at every try (relaunch). Such a pod is Pending,
// PodScheduled True and PodReadyToStartContainers False. A pod with a kept volume is in that
// state too while its volume moves from the node that held it (keptVolumeAttachWait), so the
// relaunch judges each pod against its start bound (startBound).
//
// Two clocks judge it, since neither alone tells such a pod from one about to start. Its creation
// is what the launch's wait for its launcher runs against: that wait starts once the store holds
// the pod, so it gives up about the pod's start bound after the pod's creation, and the next try
// follows it by a backoff of at least a second, or at once for a sibling role's try queued on the
// pod's launch turn (lockPod). Both conditions come later than the creation: PodScheduled's time
// is stamped when the pod is bound, and the kubelet keeps it whenever it rewrites the status, while
// PodReadyToStartContainers' is the kubelet's own, written once it first syncs the pod. Counted
// from either, that next try could find the pod younger than the bound, keep it, and wait on it
// once more. Its scheduling, PodScheduled's time, is what tells it from a pod that waited
// unscheduled for room in the pool for longer than the bound: such a pod, once a node gets it,
// reads the same in the seconds until that node has mounted its volumes and made its sandbox,
// created longer than the bound ago, and a try in that window would delete a pod about to start and
// send its replacement to the back of the scheduling queue. PodScheduled's time is the one to read,
// not the kubelet's, since it says how long the named node has had the pod. A pod scheduled less
// than the margin ago is so left to start. A stuck pod scheduled late, within the margin of the try
// that follows its timed-out wait, as one that waited for the autoscaler can be, is kept by that
// try once more, which waits out the pod's start bound and is charged; the try after it replaces
// the pod, so a pod scheduled late costs at most one try.
//
// Nothing else is judged: a pod whose kubelet has not reported the condition, whose wait for its
// image pull is the launcher wait's own (awaitRoleLauncher); a pod that is not scheduled, whose
// wait is the scheduler's (verdict); a pod that has left Pending, which has started; and a pod
// whose creation or scheduling time is unknown. Each of those is 0, 0 and false.
func neverStarted(pod *corev1.Pod, now time.Time, bound time.Duration) (unstarted, scheduledFor time.Duration, stuck bool) {
	if pod == nil || phaseOf(pod) != corev1.PodPending || pod.CreationTimestamp.IsZero() {
		return 0, 0, false
	}
	scheduled, ready := podCondition(pod, corev1.PodScheduled), podCondition(pod, corev1.PodReadyToStartContainers)
	if scheduled == nil || scheduled.Status != corev1.ConditionTrue || scheduled.LastTransitionTime.IsZero() ||
		ready == nil || ready.Status != corev1.ConditionFalse {
		return 0, 0, false
	}
	unstarted, scheduledFor = now.Sub(pod.CreationTimestamp.Time), now.Sub(scheduled.LastTransitionTime.Time)
	return unstarted, scheduledFor, unstarted > bound && scheduledFor > min(bound/2, startingGrace)
}

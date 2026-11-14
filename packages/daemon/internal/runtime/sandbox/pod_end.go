package sandbox

import (
	"maps"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// podEndRecordLife is how long each of the two records a pod's end needs stands: the daemon's own
// record that it means to end a pod (intendedEnds), written before the act and read when the end
// arrives, and the reading the end was captured as (podEnds), written where the informer delivers
// the end and read again by each claim of that pod as it is evaluated.
//
// It is long enough that every claim of the pod reaches its verdict while the reading stands: the
// six roles of an issue pod share one pod and are evaluated one at a time, each marked for the
// running Observe by the same informer event that captured the reading, and the slowest of them
// waits a probe interval (30 seconds in the deployment's configuration) and the API reads one
// evaluation makes. Ten minutes is twenty such intervals.
//
// It is short enough that a pod which keeps running outlives it, which is what the record of an
// intended end needs: the daemon patches the Sandbox, the patch does not take, the pod runs on,
// and the pod's next genuine end must be read for what it is rather than taken for the end that
// patch meant to make. An issue pod runs for hours, so a record ten minutes old belongs to an act
// whose end never came, and prunePodEnds drops it rather than letting it answer for an end that
// is not the one it was written for.
const podEndRecordLife = 10 * time.Minute

// deletedByAnother is the reading of a pod the daemon found being deleted with no act of its own
// to account for it. The daemon deletes no pod itself — it patches or deletes the pod's Sandbox,
// and the Agent Sandbox controller deletes the pod — so every end the daemon makes carries a
// deletion timestamp the daemon did not write, and this reading is made only where no record of
// an intended end answers for the pod (capturePodEnd).
const deletedByAnother = "the pod was being deleted with no act of the daemon's to account for it"

// clusterEndedReasons are the reasons a pod's own status carries when the cluster ended it, and
// the whole of that set: a pod the kubelet ended because its node is going down (Terminated,
// NodeShutdown), and one the scheduler preempted for a pod of higher priority (Preempted). A reason
// outside the set is not one of them, however it reads: UnexpectedAdmissionError is a pod a node
// refused before it ran, which the roles of the next pod would meet again, and an end the daemon
// waived would be relaunched for ever.
//
// Evicted (evictedReason) is not in the set, because the reason alone does not say who caused the
// eviction. The kubelet writes it for a node under pressure, and then it also sets the
// DisruptionTarget condition, which disruption reads first. It writes the same reason, with no
// condition, for a pod that went over a limit of its own: the eviction manager's
// podEphemeralStorageLimitEviction (the pod's ephemeral-storage limit),
// containerEphemeralStorageLimitEviction (a container's) and emptyDirLimitEviction (an emptyDir's
// sizeLimit) each call evictPod with a nil condition (kubernetes pkg/kubelet/eviction/
// eviction_manager.go). Only the node-pressure path in synchronize passes one. Those three pods
// are ended by what they do themselves: a task that fills its own disk gets a new pod, the same
// task and the same eviction, and read as an end from outside the pod that would be a relaunch
// for ever. The kubelet's admission check writes `Evicted` with no condition as well, for a pod a
// node under pressure refuses before it runs, which is the refusal UnexpectedAdmissionError is.
// So `Evicted` with no DisruptionTarget condition reads as neither reading and is charged a
// death (capturePodEnd).
var clusterEndedReasons = []string{"Terminated", "NodeShutdown", "Preempted"}

// evictedReason is the status reason the kubelet gives every pod it evicts. It is an end from
// outside the pod only when the DisruptionTarget condition comes with it (clusterEndedReasons says
// why).
const evictedReason = "Evicted"

// evictedForItsOwnUse is the reading of a pod the kubelet evicted with no DisruptionTarget
// condition: it went over its own ephemeral-storage limit, a container's limit or an emptyDir's
// sizeLimit, or a node under pressure refused it at admission (clusterEndedReasons). Such an end
// is read neither way and charged.
const evictedForItsOwnUse = "the kubelet evicted the pod with no DisruptionTarget condition: over a limit of its own, or refused at admission"

// disruption is whether the cluster says it ended pod, with the words the daemon read that from
// and the reason the cluster gave: the DisruptionTarget condition the control plane and the
// kubelet set on a pod they are ending — an eviction, a preemption, a node shutting down or
// draining — and otherwise one of clusterEndedReasons on the pod's own status. Both readings live
// here, read in this one place. A pod whose status reason is evictedReason with no condition is no
// disruption (clusterEndedReasons); selfEvicted names that end for capturePodEnd.
//
// kubeletFailure asks a different question of the same two facts, so neither reads the other: it
// answers any non-empty reason, which this excludes, and it reads the image probe's pod, which
// runs under `restartPolicy: Never` where an issue pod is RestartPolicyAlways.
func disruption(pod *corev1.Pod) (cause, reason string, ok bool) {
	if c := podCondition(pod, corev1.DisruptionTarget); c != nil && c.Status == corev1.ConditionTrue {
		return "the cluster marked the pod a disruption target", c.Reason, true
	}
	if slices.Contains(clusterEndedReasons, pod.Status.Reason) {
		return "the cluster ended the pod", pod.Status.Reason, true
	}
	return "", "", false
}

// selfEvicted is whether the kubelet evicted pod with no DisruptionTarget condition, the end
// evictedForItsOwnUse names: the reason is evictedReason and the condition is not set true. It
// reads the condition itself rather than leaning on disruption having answered no first, so no
// order of capturePodEnd's cases, and no other caller, can read an eviction under node pressure,
// which carries the condition, as the pod's own and have it charged.
func selfEvicted(pod *corev1.Pod) bool {
	if pod.Status.Reason != evictedReason {
		return false
	}
	c := podCondition(pod, corev1.DisruptionTarget)
	return c == nil || c.Status != corev1.ConditionTrue
}

// intendedEnd is one act of the daemon's own that is about to end a pod: the act, in the words the
// pod's end is logged and detailed by, and the moment it was recorded, which its expiry is read
// from (podEndRecordLife).
type intendedEnd struct {
	act string
	at  time.Time
}

// podEnd is what the daemon read about the cause of one pod's end: the reading the supervisor
// decides on (runtime.PodEndReading), the words that reading was made from, the reason the cluster
// gave where it gave one, and the moment it was read.
type podEnd struct {
	reading runtime.PodEndReading
	cause   string
	reason  string
	at      time.Time
}

// detail is what a claim's observation of this end says of it, after the mapping's own detail: the
// words the reading was made from, with the cluster's reason where there was one.
func (e podEnd) detail() string {
	if e.reason == "" {
		return e.cause
	}
	return e.cause + " (" + e.reason + ")"
}

// meanToEndPod records, before an act of the daemon's own that will end the pod uid, that the
// daemon means to end it: the end is then read as the daemon's own (runtime.PodEndOurs) and
// charged as it always was, rather than as something outside the pod ending it. Every such act
// writes the pod's Sandbox rather than the pod — the idle release's teardown and an issue's close,
// a tree's cleanup and the orphan sweep, an update round's replacement and a relaunch's — so every
// one of the daemon's own ends reaches the pod as a deletion timestamp the daemon did not write,
// and this record is the whole of what tells those ends from an eviction's.
//
// It is written before the act and read after it, so it is dropped by its consumer the moment the
// end it was written for is read (takeIntendedEnd) and by its expiry when no end comes
// (podEndRecordLife): a record left standing would have the pod's next genuine end read as an end
// of the daemon's own. Every write prunes what has expired, so the map cannot grow without bound.
func (r *Runtime) meanToEndPod(uid, act string) {
	if uid == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prunePodEnds()
	r.intendedEnds[uid] = intendedEnd{act: act, at: r.now()}
}

// meanToEndPodOf is meanToEndPod for the pod the Sandbox s owns in the store now, which is the pod
// the caller's act is about to end. A Sandbox the store holds no pod of records nothing: there is
// no pod for the act to end, and a same-named pod it does not own is no process of its claims'.
func (r *Runtime) meanToEndPodOf(s *sandbox, act string) {
	if s == nil {
		return
	}
	if pod := r.viewOf(s.Name, s).pod; pod != nil {
		r.meanToEndPod(string(pod.UID), act)
	}
}

// takeIntendedEnd is the consumer of the records meanToEndPod writes: it answers the act the
// daemon recorded for the pod uid and drops that record, the end it was written for having now
// been read and nothing being left that could read it. A record older than podEndRecordLife is
// dropped and answers nothing, so an act whose end never came waives no later death. Called with
// r.mu held.
func (r *Runtime) takeIntendedEnd(uid string) (string, bool) {
	intended, ok := r.intendedEnds[uid]
	delete(r.intendedEnds, uid)
	if !ok || r.now().Sub(intended.at) >= podEndRecordLife {
		return "", false
	}
	return intended.act, true
}

// prunePodEnds drops every record of an intended end and every captured reading that has outlived
// podEndRecordLife. It runs wherever one of them is written, so neither map grows without bound on
// a daemon that has watched pods come and go for weeks. Called with r.mu held.
func (r *Runtime) prunePodEnds() {
	now := r.now()
	maps.DeleteFunc(r.intendedEnds, func(_ string, e intendedEnd) bool { return now.Sub(e.at) >= podEndRecordLife })
	maps.DeleteFunc(r.podEnds, func(_ string, e podEnd) bool { return now.Sub(e.at) >= podEndRecordLife })
}

// capturePodEnd reads what ended pod and keeps that reading by the pod's uid, once, the first time
// the pod shows an end; gone says the informer delivered the pod as deleted, which is an end
// whatever the copy that event carries says.
//
// The reading is made here, where the informer delivers the object, because neither fact it rests
// on can be read once the end is final: a pod being deleted carries its deletion timestamp while
// it still runs, which the mapping reads as a live pod (verdict), and by the time a claim of that
// pod is evaluated for the end the object has left the store. One reading answers every claim of
// the pod — the six roles of an issue pod share one — so it is kept by the pod's uid, which no
// other pod ever holds, beside the Sandbox-name record that stands in for an annotation
// (stranded).
//
// An end the daemon's own record names is the daemon's own (takeIntendedEnd), whatever the cluster
// made of it; otherwise what the cluster says ended the pod (disruption), and then a deletion
// timestamp no act of the daemon's accounts for (deletedByAnother), are each an end from outside
// the pod. A pod the kubelet evicted with no DisruptionTarget condition (selfEvicted) is read
// neither way, before the deletion timestamp is read: the controller's delete of that failed pod
// can reach the informer in the same event as its eviction, and read as deletedByAnother it would
// continue a pod that fills its own disk for ever. Its words and reason are kept for the line
// (logPodEnd), and no caller is answered with them (podEndOf).
//
// An end of neither reading is charged as it always was, and is recorded all the same, with an
// empty reading: the record is what makes the first end event the one that decides. A pod whose
// first end carries a reason outside the cluster's own — UnexpectedAdmissionError, a node
// refusing the pod before it ran, which is exactly the end that must be charged — is followed by
// another event for the same uid, the controller's delete, pod garbage collection or an
// operator's own `kubectl delete pod`; with nothing recorded, that event's deletion timestamp
// would read as deletedByAnother and every role of the pod would be continued uncharged. The
// empty reading is answered to no caller (podEndOf), so it decides nothing it did not read.
// Called with r.mu held.
func (r *Runtime) capturePodEnd(pod *corev1.Pod, gone bool) {
	uid := string(pod.UID)
	if uid == "" || !podEnding(pod, gone) {
		return
	}
	if _, read := r.podEnds[uid]; read {
		return
	}
	end := podEnd{at: r.now()}
	act, intended := r.takeIntendedEnd(uid)
	cause, reason, disrupted := disruption(pod)
	switch {
	case intended:
		end.reading, end.cause, end.reason = runtime.PodEndOurs, act, pod.Status.Reason
	case disrupted:
		end.reading, end.cause, end.reason = runtime.PodEndNotOurs, cause, reason
	case selfEvicted(pod):
		end.cause, end.reason = evictedForItsOwnUse, pod.Status.Reason
	case pod.DeletionTimestamp != nil:
		end.reading, end.cause = runtime.PodEndNotOurs, deletedByAnother
	}
	r.prunePodEnds()
	r.podEnds[uid] = end
	r.logPodEnd(pod, end)
}

// podEnding is whether pod's event is one an end can be read from: the informer delivered it as
// deleted (gone), it carries a deletion timestamp, or the kubelet has it in a terminal phase. A
// pod that is merely running is no end, and nothing is read or consumed for it.
func podEnding(pod *corev1.Pod, gone bool) bool {
	return gone || pod.DeletionTimestamp != nil || terminal(pod)
}

// podEndLine is the one line a pod's end gets, written where the end is first read and naming the
// pod, which reading was made, the words it was made from, the reason the cluster gave where it
// gave one, and every role of that pod the runtime watches, each of which the end ended. It says
// what the end was read as and nothing of what any role is charged for it: the supervisor decides
// that for each claim, continuing a pod_end_not_ours end uncharged until such ends reach
// launch_failure_limit in a row, a count this runtime does not keep. One line per pod end and not
// one per role: a pod's six roles end together, and the runtime knows every watched claim whose
// locator names the pod. A reader of the line tells a restart an eviction caused from one an
// out-of-memory kill caused.
const podEndLine = "sandbox runtime: read what ended a watched pod"

// logPodEnd writes that line for end, the reading just made of pod's end: at warning level for an
// end from outside the pod, which cost every role in it its turn through nothing of its own, and
// at info level for one the daemon itself made, which is the daemon reporting its own act, or for
// an end read neither way, which is charged as it always was.
//
// A pod the runtime watches no claim of gets no line. The line is the record of the watched roles
// an end ended, and a pod no claim of this runtime runs in ends none: the pods informer carries
// the image probe's pod and each repository cache's pod beside the issue and controller pods, and
// every one of those ends — a probe pod ends at every boot — would otherwise write, at warning
// level, a line for an end from outside the pod that names roles=[]. The reading itself is kept
// whether or not the line is written: a claim of the pod may be watched by the time the end is
// evaluated. Called with r.mu held.
func (r *Runtime) logPodEnd(pod *corev1.Pod, end podEnd) {
	roles := make([]string, 0, len(r.watch))
	for token, loc := range r.watch {
		if loc.Sandbox == nil || loc.Sandbox.PodUID != string(pod.UID) {
			continue
		}
		if role, ok := token.Role(); ok {
			roles = append(roles, string(role))
		}
	}
	if len(roles) == 0 {
		return
	}
	slices.Sort(roles)
	write := r.log.Info
	if end.reading == runtime.PodEndNotOurs {
		write = r.log.Warn
	}
	write(podEndLine, "pod", pod.Name, "uid", string(pod.UID), "reading", string(end.reading),
		"cause", end.cause, "reason", end.reason, "roles", roles)
}

// podEndOf is what the daemon read about the end of the pod uid, and whether it read anything at
// all: the reading capturePodEnd kept, unless it has outlived podEndRecordLife, which is answered
// to no one.
//
// An end read neither way is answered to no one either. Its record exists so that the first end
// event of a pod is the one that decides (capturePodEnd), and it holds no reading, so the claims
// of that pod are evaluated exactly as they were before the reading existed: no value on the
// observation and nothing added to its detail.
func (r *Runtime) podEndOf(uid string) (podEnd, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end, ok := r.podEnds[uid]
	if !ok || end.reading == "" || r.now().Sub(end.at) >= podEndRecordLife {
		return podEnd{}, false
	}
	return end, true
}

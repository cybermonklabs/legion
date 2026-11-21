package sandbox

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// podEndRecordLife is how long each of the two records a pod's end needs stands past the moment
// it is counted from: the daemon's own record that it means to end a pod (intendedEnds), written
// before the act and read when the end arrives, counted from the act; and the reading the end was
// captured as (podEnds), written where the informer delivers the end and read again by each claim
// of that pod as it is evaluated, counted from the moment the pod left the pods informer's store
// (podEnd.goneAt). A reading stands for as long as its pod is in the store, however long that is.
//
// The reading is counted from the pod's leaving and not from its making because an end from
// outside the pod can take longer than this to finish, and no claim of the pod is judged for it
// until it has: the mapping reads a pod that still runs under a deletion timestamp as alive, so a
// claim's first ending verdict comes once the pod has left the store (Gone) or its Sandbox holds a
// pod of a new uid (NotRecordedProcess). A node that fails holds its pods in the store under their
// deletion timestamps through the taint manager's evictions and the node's own grace, which can
// run well past ten minutes before the pods are deleted and the Sandbox controller makes new ones.
// A reading timed from its making would have expired by then, and that end would be charged as one
// the daemon read neither way.
//
// It is long enough that every claim of the pod reaches its verdict while the reading stands once
// the pod has left the store: the six roles of an issue pod share one pod and are evaluated one at
// a time, each marked for the running Observe by the informer event that delivered the pod's
// delete or its successor, and the slowest of them waits a probe interval (30 seconds in the
// deployment's configuration) and the API reads one evaluation makes. Ten minutes is twenty such
// intervals.
//
// The record of an intended end keeps counting from the act, because it must be short enough that
// a pod which keeps running outlives it: the daemon patches the Sandbox, the patch does not take,
// the pod runs on, and the pod's next genuine end must be read for what it is rather than taken for
// the end that patch meant to make. An issue pod runs for hours, so a record ten minutes old belongs
// to an act whose end never came, and prunePodEnds drops it rather than letting it answer for an
// end that is not the one it was written for.
const podEndRecordLife = 10 * time.Minute

// deletedByAnother is the reading of a pod the daemon found being deleted with no act of its own
// to account for it. The daemon deletes no pod itself — it patches or deletes the pod's Sandbox,
// and the Agent Sandbox controller deletes the pod — so every end the daemon makes carries a
// deletion timestamp the daemon did not write, and this reading is made only where no record of
// an intended end answers for the pod. It is made only of a pod not yet terminal: a terminal pod
// is read from its status alone, whatever deletion timestamp it carries (readPodEnd).
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
// death (readPodEnd). So does an eviction under node memory or disk pressure whose message
// names a container of the pod using more memory, or more ephemeral storage, than it requested
// (overRequest), though it carries the condition: that pod's own use is what the kubelet evicted
// first, and a new pod running the same work would use as much.
var clusterEndedReasons = []string{"Terminated", "NodeShutdown", "Preempted"}

// evictedReason is the status reason the kubelet gives every pod it evicts. It is an end from
// outside the pod only when the DisruptionTarget condition comes with it (clusterEndedReasons says
// why).
const evictedReason = "Evicted"

// evictedForItsOwnUse is the reading of a pod the kubelet evicted with no DisruptionTarget
// condition whose message names no container of the pod over its request: it went over its own
// ephemeral-storage limit, a container's limit or an emptyDir's sizeLimit, a node under pressure
// refused it at admission (clusterEndedReasons), or a kubelet that wrote no condition evicted it
// for node memory or disk pressure. Such an end is read neither way and charged. The same end
// whose message names a container of the pod over its memory or its ephemeral-storage request is
// read in evictedOverRequest's words instead (selfEvictedCause, readPodEnd), since the kubelet
// writes that sentence only for node pressure and these words would name a cause it was not.
const evictedForItsOwnUse = "the kubelet evicted the pod with no DisruptionTarget condition: over a limit of its own, refused at admission, or for node pressure, its message naming no container of the pod over its request"

// evictedOverRequest is the reading of a pod the kubelet evicted under node memory or disk
// pressure whose message names a container of the pod using more of the resource the node was
// short of than it requested (overRequest where the DisruptionTarget condition is set,
// selfEvictedCause where it is not), in the words of the cause it was read as. The kubelet evicts
// the pods using more than their request before any other, so the pressure the end answered was
// the pod's own use and not a neighbour's: such an end is read neither way and charged, as an
// eviction over a limit of the pod's own is, whatever DisruptionTarget condition it carries.
var evictedOverRequest = map[runtime.DeathCauseKind]string{
	runtime.DeathMemoryRequest: "the kubelet evicted the pod for node memory pressure, its message naming a container of the pod over its memory request",
	runtime.DeathDiskRequest:   "the kubelet evicted the pod for node disk pressure, its message naming a container of the pod over its ephemeral-storage request",
}

// containerOverRequest matches the sentence the kubelet's eviction manager adds to the message of
// a pod it evicts for node memory or disk pressure for each container of the pod using more of the
// resource the node was short of than it requested. It is upstream's containerMessageFmt,
// "Container %s was using %s, request is %s, has larger consumption of %v. " (kubernetes
// pkg/kubelet/eviction/helpers.go, evictionMessage), with the container's name, its use and its
// request, each quantity a resource.Quantity's String, and that resource: memory, or
// ephemeral-storage, where the use is the container's writable layer and its logs and never an
// emptyDir volume's, which that message names no container for. The sentence ends in a space,
// which the match lets the message's end stand in for.
var containerOverRequest = regexp.MustCompile(`Container ([^ ]+) was using ([^ ,]+), request is ([^ ,]+), has larger consumption of (memory|ephemeral-storage)\.(?: |$)`)

// overRequest is the cause of the end of pod where the kubelet evicted it for node memory or disk
// pressure — its reason is evictedReason and its DisruptionTarget condition is true — and its
// message names a container of the pod's own spec over its request (namedOverRequest). Any other
// pod answers the zero cause: an eviction whose message names no container of this pod over its
// request is a neighbour's pressure, so the end is read as one from outside the pod (disruption).
func overRequest(pod *corev1.Pod) runtime.DeathCause {
	if c := podCondition(pod, corev1.DisruptionTarget); pod.Status.Reason != evictedReason || c == nil || c.Status != corev1.ConditionTrue {
		return runtime.DeathCause{}
	}
	return namedOverRequest(pod)
}

// namedOverRequest is the cause pod's message names where it names, in containerOverRequest's
// words, a container of the pod's own spec whose use and request both parse and whose use is the
// larger: the first such container, its use and its request, as runtime.DeathMemoryRequest for
// memory and runtime.DeathDiskRequest for ephemeral storage. A message naming no such container,
// none over its request or only a name the pod's spec does not hold, answers the zero cause, and so
// does one whose disk use was in an emptyDir volume, which the message names no container for. Each
// number is the quantity the message gives, parsed, and the name is one the pod's spec holds, so no
// word of the message's own reaches a sentence built from the cause. It reads the message alone, so
// an eviction that carries the DisruptionTarget condition (overRequest) and one that carries none
// (selfEvictedCause) are read by the one rule.
func namedOverRequest(pod *corev1.Pod) runtime.DeathCause {
	for _, named := range containerOverRequest.FindAllStringSubmatch(pod.Status.Message, -1) {
		if !specContainer(pod, named[1]) {
			continue
		}
		use, useErr := resource.ParseQuantity(named[2])
		request, requestErr := resource.ParseQuantity(named[3])
		if useErr != nil || requestErr != nil || use.Cmp(request) <= 0 {
			continue
		}
		kind := runtime.DeathMemoryRequest
		if named[4] == string(corev1.ResourceEphemeralStorage) {
			kind = runtime.DeathDiskRequest
		}
		return runtime.DeathCause{Kind: kind, Container: named[1], Use: use.Value(), Request: request.Value()}
	}
	return runtime.DeathCause{}
}

// specContainer is whether pod's spec holds a container named name, an init container among them,
// since the kubelet's eviction message reads both kinds.
func specContainer(pod *corev1.Pod, name string) bool {
	named := func(c corev1.Container) bool { return c.Name == name }
	return slices.ContainsFunc(pod.Spec.Containers, named) || slices.ContainsFunc(pod.Spec.InitContainers, named)
}

// selfEvictedCause is the cause of the end of pod, a pod selfEvicted names, which the kubelet
// evicted with no DisruptionTarget condition. Where its message names a container of the pod's own
// spec over its memory or its ephemeral-storage request (namedOverRequest), the cause is that
// container, its use and its request, as overRequest reads them where the condition is set: the
// kubelet writes that sentence only on an eviction for node memory or disk pressure, and it evicts
// the pods using more than their request before any other, so the pressure was the pod's own use
// whether or not the kubelet also wrote the condition. The role is then told the resource, its use
// and its request as the message gives them (dispatch://INFRA-1117 R9), and not a limit it never
// reached. Any other message is read for a disk limit of the pod's own (diskEviction). The end is
// read neither way and charged whatever this answers (readPodEnd), so the cause decides only
// what the role is told of its death.
func selfEvictedCause(pod *corev1.Pod) runtime.DeathCause {
	if cause := namedOverRequest(pod); cause.Kind != "" {
		return cause
	}
	return diskEviction(pod)
}

// diskLimitSentences match the sentences the kubelet's eviction manager writes as the message of
// a pod it evicts for passing a disk limit of its own, each with that limit (kubernetes
// pkg/kubelet/eviction/helpers.go): podEphemeralStorageMessageFmt, "Pod ephemeral local storage
// usage exceeds the total limit of containers %s. ", the pod's own limit as a quantity's String;
// containerEphemeralStorageMessageFmt, "Container %s exceeded its local ephemeral storage limit
// %q. ", a container's, quoted; and emptyDirMessageFmt, "Usage of EmptyDir volume %q exceeds the
// limit %q. ", an emptyDir's sizeLimit, quoted. Each ends in a space, which the match lets the
// message's end stand in for.
var diskLimitSentences = []*regexp.Regexp{
	regexp.MustCompile(`Pod ephemeral local storage usage exceeds the total limit of containers ([^ ]+)\.(?: |$)`),
	regexp.MustCompile(`Container [^ ]+ exceeded its local ephemeral storage limit "([^"]+)"\.(?: |$)`),
	regexp.MustCompile(`Usage of EmptyDir volume "[^"]*" exceeds the limit "([^"]+)"\.(?: |$)`),
}

// diskEviction is the cause of the end of pod, a pod selfEvicted names, where the kubelet evicted
// it for passing a disk limit of its own (runtime.DeathDiskLimit): the limit its message gives in
// one of diskLimitSentences, parsed, or, where the message holds one of those sentences and its
// limit does not parse, the limit the pod's spec holds it to as a whole (specDiskLimit), with
// whether that limit counted the pod's test database (testDatabaseCounted). Only those
// sentences show an end of the pod's own disk. The word ephemeral-storage in a message is no such
// sign: the kubelet writes it in its node-pressure sentences ("The node was low on resource:
// ephemeral-storage. "), never in a limit sentence, which says "ephemeral local storage", and a
// node disk-pressure eviction told as the spec's limit would tell the role it passed a limit it
// never reached. A message that holds none of those sentences answers the zero cause: a
// node-pressure eviction whose message names no container of the pod over its request
// (selfEvictedCause reads one that does), and a pod a node under pressure refused at admission,
// which never ran and passed no disk limit of its own (upstream's nodeConditionMessageFmt, "The
// node had condition: %v. ", names a node condition and neither a limit nor the resource). So does
// a limit sentence whose limit does not parse, in a pod whose spec sets no limit either.
func diskEviction(pod *corev1.Pod) runtime.DeathCause {
	diskEnd := false
	for _, sentence := range diskLimitSentences {
		named := sentence.FindStringSubmatch(pod.Status.Message)
		if named == nil {
			continue
		}
		if limit, err := resource.ParseQuantity(named[1]); err == nil && limit.Sign() > 0 {
			return runtime.DeathCause{Kind: runtime.DeathDiskLimit, Limit: limit.Value(), TestDatabaseCounted: testDatabaseCounted(pod)}
		}
		diskEnd = true
	}
	if !diskEnd {
		return runtime.DeathCause{}
	}
	if limit := specDiskLimit(pod); limit > 0 {
		return runtime.DeathCause{Kind: runtime.DeathDiskLimit, Limit: limit, TestDatabaseCounted: testDatabaseCounted(pod)}
	}
	return runtime.DeathCause{}
}

// testDatabaseCounted is whether pod kept its own test database where the kubelet's
// ephemeral-storage limit counts it: its spec holds the testPGVolume volume as an emptyDir, which
// the limit counts as it counts the role homes. A testPGVolume a generic ephemeral volume provides
// (runtime.kubernetes.test_postgres_disk) is a disk of the pod's own, which the limit does not
// count, and a pod that runs no test database holds no such volume. It reads the ended pod's own
// spec rather than the runtime's setting: a pod made before the setting changed keeps the volume
// it was made with, and the role is told what the limit of the pod that ended counted.
func testDatabaseCounted(pod *corev1.Pod) bool {
	return slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
		return v.Name == testPGVolume && v.EmptyDir != nil
	})
}

// specDiskLimit is the ephemeral-storage limit pod's spec holds the pod to as a whole, which is the
// limit the kubelet evicts it for passing: the sum of its containers' limits, or one init
// container's where that is larger, since the init containers run one at a time before the others
// start (the kubelet's PodLimits, for a pod with no sidecar). It is 0 where no container sets one.
func specDiskLimit(pod *corev1.Pod) int64 {
	var total int64
	for _, c := range pod.Spec.Containers {
		total += c.Resources.Limits.StorageEphemeral().Value()
	}
	for _, c := range pod.Spec.InitContainers {
		total = max(total, c.Resources.Limits.StorageEphemeral().Value())
	}
	return total
}

// terminalWithNoClusterEnd is the reading of a pod already terminal whose status names neither an
// end the cluster made (disruption) nor an eviction (selfEvicted): a reason outside
// clusterEndedReasons, UnexpectedAdmissionError among them, or no reason at all. Such an end is
// read neither way and charged, whatever deletion timestamp the pod carries (readPodEnd).
const terminalWithNoClusterEnd = "the pod was terminal and its status names no end the cluster made"

// disruption is whether the cluster says it ended pod, with the words the daemon read that from
// and the reason the cluster gave: the DisruptionTarget condition the control plane and the
// kubelet set on a pod they are ending — an eviction, a preemption, a node shutting down or
// draining — and otherwise one of clusterEndedReasons on the pod's own status. Both readings live
// here, read in this one place. A pod whose status reason is evictedReason with no condition is no
// disruption (clusterEndedReasons); selfEvicted names that end for readPodEnd. A disruption the
// kubelet made for node memory or disk pressure whose message names a container of the pod over
// its request is still one here, and readPodEnd asks overRequest before it asks this.
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
// evictedForItsOwnUse names, or evictedOverRequest where its message names a container of the pod
// over its request: the reason is evictedReason and the condition is not set true. It reads the
// condition itself rather than leaning on disruption having answered no first, so no order of
// readPodEnd's cases, and no other caller, can read an eviction under node pressure, which
// carries the condition, as the pod's own and have it charged.
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
// gave where it gave one, and the cause the daemon tells in plain words where the end is one of the
// pod's own it names (runtime.DeathCause: overRequest, selfEvictedCause). The cause is kept here
// because the kubelet's message it is read from leaves with the pod.
//
// Beside the reading it keeps the pod's name, which the pods informer's store holds the pod by,
// and goneAt, the moment the daemon heard the pod leave that store: the informer's delete of it
// (capturePodEnd), or the first prune that found the store holding no pod of its uid under that
// name (prunePodEnds). goneAt is zero while the store holds the pod, and the reading's
// podEndRecordLife is counted from it (expired).
type podEnd struct {
	reading runtime.PodEndReading
	cause   string
	reason  string
	death   runtime.DeathCause
	name    string
	goneAt  time.Time
}

// expired is whether the reading has outlived podEndRecordLife at now: its pod left the informer's
// store that long ago. A reading whose pod the store still holds has not, however long ago it was
// made, since a node's failure can hold a pod there under its deletion timestamp for longer than
// that before any claim of it is judged for its end (podEndRecordLife).
func (e podEnd) expired(now time.Time) bool {
	return !e.goneAt.IsZero() && now.Sub(e.goneAt) >= podEndRecordLife
}

// sameReading is whether e reads a pod's end as o does: the same reading, made from the same words,
// with the same cause told in plain words. A later event of the pod read that way changes nothing
// a claim is answered and writes no line (capturePodEnd).
func (e podEnd) sameReading(o podEnd) bool {
	return e.reading == o.reading && e.cause == o.cause && e.death == o.death
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
//
// The record is held in memory alone. A daemon that restarts between the act and the pod's end
// has lost it, so an end the daemon itself made is then read as an end from outside the pod and
// its roles are relaunched uncharged, as for every end from outside a pod (supervise's
// endedFromOutside).
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

// prunePodEnds drops every record of an intended end that has outlived podEndRecordLife since its
// act, and every captured reading whose pod left the informer's store that long ago
// (podEnd.expired). It runs wherever one of them is written, so neither map grows without bound on
// a daemon that has watched pods come and go for weeks: the readings it keeps are of the pods in
// the store and of those that left it within the last podEndRecordLife.
//
// A reading is timed from the informer's delete of its pod (capturePodEnd), and here, where that
// delete never comes, from the first prune that finds the store holding no pod of its uid under its
// name. A pod can leave the store with no delete of its own delivered: where the watch missed both
// the pod's delete and its successor's add, the relist finds the new pod under the same name, and
// client-go hands the handlers an update from the old pod to the new one (processDeltas), so
// notify never hears the old uid go. Without this, the reading of such a pod would stand for ever.
// Called with r.mu held.
func (r *Runtime) prunePodEnds() {
	now := r.now()
	maps.DeleteFunc(r.intendedEnds, func(_ string, e intendedEnd) bool { return now.Sub(e.at) >= podEndRecordLife })
	for uid, e := range r.podEnds {
		switch {
		case e.expired(now):
			delete(r.podEnds, uid)
		case e.goneAt.IsZero():
			if pod := r.storedPod(e.name); pod == nil || string(pod.UID) != uid {
				e.goneAt = now
				r.podEnds[uid] = e
			}
		}
	}
}

// capturePodEnd reads what ended pod and keeps that reading by the pod's uid, the first time the
// pod shows an end; gone says the informer delivered the pod as deleted, which is an end whatever
// the copy that event carries says. The reading stands for as long as the pod stays in the
// informer's store and for podEndRecordLife after the informer delivers it deleted: the first
// event that does sets the moment that life is counted from (podEnd.goneAt), whatever reading
// already stands. A node's failure can hold a pod in the store under its deletion timestamp for
// longer than podEndRecordLife, through a second eviction and the node's own grace, and no claim of
// the pod is judged for its end before the pod has gone, so a reading timed from its making would
// have expired by the time any claim asked for it.
//
// The reading is made here, where the informer delivers the object, because neither fact it rests
// on can be read once the end is final: a pod being deleted carries its deletion timestamp while
// it still runs, which the mapping reads as a live pod (verdict), and by the time a claim of that
// pod is evaluated for the end the object has left the store. One reading answers every claim of
// the pod — the six roles of an issue pod share one — so it is kept by the pod's uid, which no
// other pod ever holds, beside the Sandbox-name record that stands in for an annotation
// (stranded).
//
// The reading itself is readPodEnd's, made from the state this event carries; what is kept, and
// when, is this function's: it takes the record of an intended end at the first reading of the end
// alone (takeIntendedEnd), keeps the reading, and writes its line (logPodEnd). Every reading is
// kept, an end of neither reading too, which is kept with an empty reading and charged as it always
// was. A reading that is not empty decides the end at the event that made it: a deletion read from
// outside stays so when the kubelet's terminal status follows it, and no later event of the same
// end — a second eviction, the controller's delete, pod garbage collection, an operator's own
// `kubectl delete pod`, the informer's delete — is read again or gets a line of its own. An empty
// reading is read again at each later event of the pod, by the same rule and from the state that
// event carries, so an end is read alike whether its facts came in one event or in several; a
// re-read that reads the end as the reading standing does (podEnd.sameReading) keeps that reading,
// and only a changed reading is kept. A re-read takes no record of an intended end: the first
// reading took any that stood (takeIntendedEnd), so a record standing now was written after the pod
// had shown its end, by an act that did not make it.
//
// Only a reading that decides the end, one that is not empty, writes the pod's line (logPodEnd),
// whether the first event made it or a re-read did. An empty reading decides nothing and writes
// none, so the one line a pod's end gets names the reading that decided it, and an end no event
// reads either way gets none. A reading that has expired is pruned before the event is read, so
// that event reads the end afresh. The empty reading is answered to no caller as a reading
// (podEndOf), so it decides nothing it did not read; the cause it carries, where it carries one, is
// answered. Called with r.mu held.
func (r *Runtime) capturePodEnd(pod *corev1.Pod, gone bool) {
	uid := string(pod.UID)
	if uid == "" || !podEnding(pod, gone) {
		return
	}
	r.prunePodEnds()
	held, read := r.podEnds[uid]
	if gone && held.goneAt.IsZero() {
		held.goneAt = r.now()
	}
	if read && held.reading != "" {
		r.podEnds[uid] = held
		return
	}
	var act string
	intended := false
	if !read {
		act, intended = r.takeIntendedEnd(uid)
	}
	end := readPodEnd(pod, act, intended)
	end.name, end.goneAt = pod.Name, held.goneAt
	if read && end.sameReading(held) {
		r.podEnds[uid] = held
		return
	}
	r.podEnds[uid] = end
	r.logPodEnd(pod, end)
}

// readPodEnd is the reading of pod's end by the one rule every event of the pod is read by, from
// the state this event carries; act is the act a record of the daemon's own names for the end,
// where intended says such a record stood (takeIntendedEnd, which capturePodEnd asks). It reads
// pod, act and intended and nothing else — no record, no clock, no runtime state — so it sets the
// reading, its words, the cluster's reason and the cause alone: the pod's name and the moment the
// pod left the informer's store are the record's, which capturePodEnd sets. A pod none of the
// rule's cases names, one the informer delivers deleted whose last state shows no end, reads as an
// empty end with no words.
//
// An end the daemon's own record names is the daemon's own, whatever the cluster made of it, so a
// pod the daemon is ending that the kubelet then evicts continues no role. An end the cluster says
// it made (disruption) is an end from outside the pod, except an eviction under node memory or disk
// pressure whose message names a container of the pod over its request of that resource
// (overRequest): that is the pod's own use, read neither way, with its cause kept for the claims of
// the pod (podEnd.death). Past those, a pod already terminal — in the phase the kubelet sets on a
// pod it has ended (terminal) — is read from its status alone, and a deletion timestamp counts only
// on a pod not yet terminal. A terminal pod is read neither way, whatever its reason and whatever
// deletion timestamp it carries: `Evicted` with no DisruptionTarget condition (selfEvicted), a
// reason outside the cluster's own such as UnexpectedAdmissionError, or no reason at all. An
// `Evicted` end with no condition keeps the cause its message names (selfEvictedCause): a container
// of the pod over its memory or its ephemeral-storage request, as the eviction above that carries
// the condition does and in the same words (evictedOverRequest), since the kubelet evicted that pod
// for its own use whatever condition it wrote, or else a disk limit of the pod's own that the
// kubelet evicted it for passing. A message naming neither, a node's refusal at admission among
// them, keeps no cause, so no role is told it passed a limit it never reached, and the end is
// charged all the same. The words of an end read neither way (evictedOverRequest,
// evictedForItsOwnUse, terminalWithNoClusterEnd) and its reason are kept on its record, which a
// later event's reading is compared with (podEnd.sameReading); no line names them (logPodEnd), and
// no caller is answered with them (podEndOf). A pod not yet terminal that carries a deletion
// timestamp no act of the daemon's accounts for is an end from outside it (deletedByAnother). A pod
// the informer delivers as deleted is read by the same rule from the last state it carries.
//
// The rule reads a pod's failure the same however the informer delivers its facts. The controller
// deletes a pod the kubelet has failed, and the informer can deliver the terminal status and the
// deletion timestamp in one event or in two. Read from the deletion timestamp wherever there is
// one, the same failure would be continued uncharged when both came in one event and charged when
// the status came first, and a pod a node keeps refusing, or a task that fills its own disk, would
// be relaunched for ever whenever the two came together.
//
// The rule has one cost: an outside delete the daemon first sees when the pod is already terminal
// is charged a death. That is rare. The informer hands notify each watch event in order, and a
// graceful delete sets the deletion timestamp on the running pod before the kubelet makes it
// terminal, so the daemon sees the running pod's deletion first and reads it as an end from
// outside. It sees the terminal pod first only when its watch missed that update and a relist
// delivered only the later state: that is inferred from how client-go's watch and relist behave,
// and no test here proves the order the informer delivers in.
func readPodEnd(pod *corev1.Pod, act string, intended bool) podEnd {
	var end podEnd
	cause, reason, disrupted := disruption(pod)
	ownUse := overRequest(pod)
	switch {
	case intended:
		end.reading, end.cause, end.reason = runtime.PodEndOurs, act, pod.Status.Reason
	case ownUse.Kind != "":
		end.cause, end.reason, end.death = evictedOverRequest[ownUse.Kind], reason, ownUse
	case disrupted:
		end.reading, end.cause, end.reason = runtime.PodEndNotOurs, cause, reason
	case terminal(pod) && selfEvicted(pod):
		end.reason, end.death = pod.Status.Reason, selfEvictedCause(pod)
		end.cause = cmp.Or(evictedOverRequest[end.death.Kind], evictedForItsOwnUse)
	case terminal(pod):
		end.cause, end.reason = terminalWithNoClusterEnd, pod.Status.Reason
	case pod.DeletionTimestamp != nil:
		end.reading, end.cause = runtime.PodEndNotOurs, deletedByAnother
	}
	return end
}

// podEnding is whether pod's event is one an end can be read from: the informer delivered it as
// deleted (gone), it carries a deletion timestamp, or the kubelet has it in a terminal phase. A
// pod that is merely running is no end, and nothing is read or consumed for it.
func podEnding(pod *corev1.Pod, gone bool) bool {
	return gone || pod.DeletionTimestamp != nil || terminal(pod)
}

// podEndLine is the one line a pod's end gets, written by the reading that decides the end — the
// daemon's own end or one from outside the pod — whether the first event of the end made that
// reading or a later event read again an end the events before it read neither way
// (capturePodEnd). A reading of neither kind decides nothing and gets no line. The line names the
// pod, which reading was made, the words it was made from, the reason the cluster gave where it
// gave one, and, as ended, the roles whose processes the end ended: every role of that pod the
// runtime watches. The line names what ended and nothing of what any claim got for it. It is
// written before any claim is decided on the reading it names, and the supervisor decides each
// claim on its own — a pod_end_not_ours end continued uncharged however often it comes, an end
// read neither way charged as the claim's own death wherever that claim had work outstanding — so
// each claim's own supervise line says what that claim got (supervise's endedFromOutside, died).
// One line per pod end and not one per role: a pod's six roles end together, and the runtime knows
// every watched claim whose locator names the pod. A restart an eviction from outside the pod
// caused has this line, and one an out-of-memory kill caused has none, which is how a reader tells
// the two apart. An eviction for the pod's own use — `Evicted` with no DisruptionTarget condition,
// or one for node pressure whose message names a container of the pod over its request
// (evictedForItsOwnUse, evictedOverRequest) — is read neither way and has no line either, as such a
// kill has none, so the line tells no such eviction from that kill.
const podEndLine = "sandbox runtime: read what ended a watched pod"

// logPodEnd writes that line for end, the reading just made of pod's end, where that reading
// decides the end: at warning level for an end from outside the pod, which cost every role in it
// its turn through nothing of its own, and at info level for one the daemon itself made, which is
// the daemon reporting its own act. A reading of neither kind writes nothing: it decides nothing,
// a later event of the pod reads the end again, and the reading that then decides it, where one
// does, is the one the line names. Such an end is charged as it always was, and each claim's own
// supervise line says what that claim got.
//
// A pod the runtime watches no claim of gets no line. The line is the record of the watched roles
// an end ended, and a pod no claim of this runtime runs in ends none: the pods informer carries
// the image probe's pod and each repository cache's pod beside the issue and controller pods, and
// every one of those ends — a probe pod ends at every boot — would otherwise write, at warning
// level, a line for an end from outside the pod that names ended=[]. The reading itself is kept
// whether or not the line is written: a claim of the pod may be watched by the time the end is
// evaluated. Called with r.mu held.
func (r *Runtime) logPodEnd(pod *corev1.Pod, end podEnd) {
	if end.reading == "" {
		return
	}
	ended := make([]string, 0, len(r.watch))
	for token, loc := range r.watch {
		if loc.Sandbox == nil || loc.Sandbox.PodUID != string(pod.UID) {
			continue
		}
		if role, ok := token.Role(); ok {
			ended = append(ended, string(role))
		}
	}
	if len(ended) == 0 {
		return
	}
	slices.Sort(ended)
	write := r.log.Info
	if end.reading == runtime.PodEndNotOurs {
		write = r.log.Warn
	}
	write(podEndLine, "pod", pod.Name, "uid", string(pod.UID), "reading", string(end.reading),
		"cause", end.cause, "reason", end.reason, "ended", ended)
}

// podEndOf is what the daemon read about the end of the pod uid, and whether it read anything at
// all: the reading capturePodEnd kept, unless its pod left the informer's store podEndRecordLife
// ago or longer (podEnd.expired), which is answered to no one whether or not a prune has dropped
// it yet. A reading whose pod the store still holds is answered however long ago it was made: a
// node's failure can hold a pod there under its deletion timestamp for longer than
// podEndRecordLife, and the claims of that pod are judged for its end only once it has gone.
//
// An end read neither way is answered to no one either, unless it carries a cause the daemon tells
// in plain words (podEnd.death), which is then answered with its empty reading. Its record exists
// so that a later event of the pod is read as a re-read, which takes no record of an intended end
// and changes nothing where it reads the end alike (capturePodEnd), and it holds no reading, so the
// claims of that pod are evaluated as they were before the reading existed: no reading on the
// observation and nothing added to its detail, and only the cause where there is one.
func (r *Runtime) podEndOf(uid string) (podEnd, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	end, ok := r.podEnds[uid]
	if !ok || (end.reading == "" && end.death.Kind == "") || end.expired(r.now()) {
		return podEnd{}, false
	}
	return end, true
}

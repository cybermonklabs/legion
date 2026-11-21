package writerhold

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
)

// PodRestartLimit is how long one hub pod's restart may take, from its eviction until it has
// passed step 4, $JS.API.INFO answers and each present protected stream's group is led with every
// copy current. A pod that has not stops the hold as UNKNOWN, with the row still held: the hold is
// never released early, and the operator decides.
const PodRestartLimit = 10 * time.Minute

// evictRetry is the pause between two evictions the disruption budget refused.
const evictRetry = 5 * time.Second

// restartPoll is the pause between two readings of a condition the restart waits for: the new
// pod Ready, the proof passed, $JS.API.INFO answered, a stream's group led.
const restartPoll = 250 * time.Millisecond

// streamRead is the limit of one read of a protected stream's info while the restart waits for its
// group (awaitStreamLed). nats-server answers a clustered stream's info from the group's leader
// alone, and a member whose JetStream started under 10 s ago answers nothing while its group has no
// leader (isGroupLeaderless, jetstream_cluster.go), so a read during the group's election after a
// restart draws no answer at all. A read with no answer within streamRead is sent again, rather
// than waited on for jsRequest's 10 s, which an election under load outlasts.
const streamRead = 2 * time.Second

// Restart is step 3, the restart of every declared server, one at a time, each proved by step 4
// before the next is stopped.
type Restart struct {
	Hub      KubeHub
	Declared Declared
	// Probe dials url, one server's own client address (KubeHub.ClientURL), with the probe's
	// credential (natsgrant.HoldProbe's BROKER_NATS_SERVICES entry), closes what it opened, and
	// returns the dial's error: nil when the server admitted the probe, an error wrapping
	// nats.ErrAuthorization when it refused it, and any other error when the dial reached no
	// decision.
	Probe func(ctx context.Context, url string) error
	// Bind is step 4's binding half, the checked binary and configuration (the render's checks):
	// the pod's image digest and revision, its claim's volume, and the server's version and
	// config_digest against step1, its /varz at step 1, and now, its /varz after the restart. It
	// returns a *Refusal or an *Unknown; an *Unknown is read again until the pod's limit.
	Bind func(ctx context.Context, pod string, step1, now Varz) error
	// Limit is each pod's limit, PodRestartLimit when zero.
	Limit time.Duration
	// Retry is the pause between two evictions the disruption budget refused, 5 s when zero.
	Retry time.Duration

	// keptIDs leaves out step 4's refusal of a server that kept its step-1 id
	// (TestHoldAServerThatKeepsItsId's control).
	keptIDs bool
}

// Restarted is one declared server step 3 restarted and step 4 proved: its pod, how its old
// container stopped, and its /varz once it passed the proof.
type Restarted struct {
	Pod  string
	Exit Exit
	Varz Varz
}

// Exit is how a restarted pod's old nats container stopped, from the last status the hold read of
// the old pod before it was gone: its terminated state, or its lastState.terminated when that is
// not the one it showed before the eviction.
type Exit struct {
	// Seen is false when the old pod was gone before the hold read its container terminated.
	Seen   bool
	Code   int32
	Signal int32
	Reason string
}

// Forced reports a container killed, by exit 137 or signal 9: nats-server had not exited within
// its grace period, and a write it had not flushed is lost. It is never a graceful flush.
func (e Exit) Forced() bool { return e.Seen && (e.Code == 137 || e.Signal == 9) }

// Graceful reports an exit 0 with no signal: nats-server's own shutdown, its file store flushed.
func (e Exit) Graceful() bool { return e.Seen && e.Code == 0 && e.Signal == 0 }

func (e Exit) String() string {
	switch {
	case !e.Seen:
		return "exit not seen: the old pod was gone before its container's terminated state was read"
	case e.Forced():
		return fmt.Sprintf("exit %d (%s), forced: the container was killed, so a write nats-server had not flushed is lost and never lands", e.Code, e.Reason)
	case e.Graceful():
		return fmt.Sprintf("exit 0 (%s), graceful", e.Reason)
	}
	return fmt.Sprintf("exit %d (%s, signal %d), not graceful", e.Code, e.Reason, e.Signal)
}

// Run restarts every declared server in turn, as Restart says, on nc, the operator's connection,
// which asks $JS.API.INFO after each and reads each protected stream's info. Before the first
// eviction it reads which protected streams are present, each once led with every copy current.
// Each pod is evicted through the Eviction API (policy/v1, never a delete, which ignores the
// disruption budget), with its UID as the precondition; an eviction the budget refuses (429) is
// sent again every Retry until the pod's limit. Then the old pod's exit is recorded as it goes,
// the new pod is awaited Ready, step 4 proves the server, $JS.API.INFO must answer, and each
// present protected stream's group must name a leader with every copy current
// (awaitStreamLed), all within the pod's limit, before the next pod is evicted: so no eviction
// takes a group's quorum, and the held command's first read after the last restart is answered. A
// pod past its limit is UNKNOWN, with the row still held. It returns the servers restarted so far,
// with the error that stopped it.
func (r Restart) Run(ctx context.Context, nc *nats.Conn) ([]Restarted, error) {
	switch {
	case len(r.Declared) == 0:
		return nil, unknownf(StepRestart, nil, "the restart names no declared server")
	case r.Probe == nil:
		return nil, unknownf(StepRestart, nil, "the restart has no probe, so step 4 cannot show a server refuses it")
	case r.Bind == nil:
		return nil, unknownf(StepRestart, nil, "the restart has no binding check, so step 4 cannot show a server runs the checked binary and configuration")
	case nc == nil:
		return nil, unknownf(StepRestart, nil, "the restart has no operator connection to ask $JS.API.INFO on")
	}
	js, err := jetstream.New(nc)
	if err != nil {
		return nil, unknownf(StepRestart, err, "the operator's JetStream context")
	}
	present, err := r.presentStreams(ctx, nc)
	if err != nil {
		return nil, err
	}
	before := map[string]string{}
	var restarted []Restarted
	for _, server := range r.Declared {
		before[server.Varz.ServerID] = server.Pod
		one, err := r.restart(ctx, nc, js, server, before, present)
		if err != nil {
			return restarted, err
		}
		restarted = append(restarted, one)
	}
	return restarted, nil
}

// limit is each pod's limit: Limit, or PodRestartLimit when zero.
func (r Restart) limit() time.Duration {
	if r.Limit == 0 {
		return PodRestartLimit
	}
	return r.Limit
}

// presentStreams are the protected streams present before the first eviction, in
// ProtectedStreams' order, each read until its group is led with every copy current, within one
// pod's limit; an absent one is left out.
func (r Restart) presentStreams(ctx context.Context, nc *nats.Conn) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, r.limit())
	defer cancel()
	var present []string
	for _, name := range ProtectedStreams() {
		found, err := awaitStreamLed(ctx, nc, "before the first eviction", name, streamRead, true)
		if err != nil {
			return nil, err
		}
		if found {
			present = append(present, name)
		}
	}
	return present, nil
}

// awaitStreamLed reads stream name's info on nc until its group names a leader and every copy is
// current and online (unled), each read limited to read and the next sent restartPoll later, and
// reports whether the stream is present. A read with no answer, or with an error, is read again:
// the group may be electing. With absentOK, `stream not found` is the stream absent (before the
// first eviction); otherwise it too is read again, since the stream was present before the
// restart. The end of ctx is UNKNOWN, naming the stream, when (after) and its last reading.
func awaitStreamLed(ctx context.Context, nc *nats.Conn, after, name string, read time.Duration, absentOK bool) (bool, error) {
	behind := "no read of it answered"
	for {
		attempt, cancel := context.WithTimeout(ctx, read)
		info, found, err := streamInfo(attempt, nc, name)
		cancel()
		switch {
		case err != nil:
			behind = "its last read: " + err.Error()
		case !found && absentOK:
			return false, nil
		case !found:
			behind = "it reads stream not found"
		default:
			if behind = unled(info); behind == "" {
				return true, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, unknownf(StepRestart, ctx.Err(), "%s's group was not led with every copy current %s within its limit: %s; the row stays held", name, after, behind)
		case <-time.After(restartPoll):
		}
	}
}

// unled is what keeps info's group from serving and from keeping its quorum through the next
// eviction: no leader, or a copy offline or not current, named; empty when the group is led with
// every copy current. A stream of one copy needs no election.
func unled(info *jetstream.StreamInfo) string {
	if info.Config.Replicas <= 1 {
		return ""
	}
	if info.Cluster == nil || info.Cluster.Leader == "" {
		return "it has no leader yet"
	}
	for _, peer := range info.Cluster.Replicas {
		switch {
		case peer == nil:
		case peer.Offline:
			return fmt.Sprintf("its copy on %s is offline", peer.Name)
		case !peer.Current:
			return fmt.Sprintf("its copy on %s is not current, %d behind", peer.Name, peer.Lag)
		}
	}
	return ""
}

// restart is one pod's eviction, exit, return, proof, $JS.API.INFO and present streams' groups,
// within its limit. before maps the step-1 id of every server restarted so far, this one's
// included, to its pod.
func (r Restart) restart(ctx context.Context, nc *nats.Conn, js jetstream.JetStream, server DeclaredServer, before map[string]string, present []string) (Restarted, error) {
	limit := r.limit()
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	pod := server.Pod
	pods := r.Hub.Client.CoreV1().Pods(r.Hub.Namespace)
	old, err := pods.Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return Restarted{}, unknownf(StepRestart, err, "read pod %s before its eviction", pod)
	}
	// The watch starts before the eviction, so the old pod's last status is seen as it goes.
	watcher, err := pods.Watch(ctx, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", pod).String(),
		ResourceVersion: old.ResourceVersion,
	})
	if err != nil {
		return Restarted{}, unknownf(StepRestart, err, "watch pod %s", pod)
	}
	defer watcher.Stop()
	if err := r.evict(ctx, pod, old.UID, limit); err != nil {
		return Restarted{}, err
	}
	exit, err := awaitGone(ctx, watcher, pods, old, limit)
	if err != nil {
		return Restarted{}, err
	}
	if err := awaitReady(ctx, pods, pod, old.UID, limit); err != nil {
		return Restarted{}, err
	}
	varz, err := r.prove(ctx, server, before, limit)
	if err != nil {
		return Restarted{}, err
	}
	if err := awaitJetStream(ctx, js, pod, limit); err != nil {
		return Restarted{}, err
	}
	for _, name := range present {
		if _, err := awaitStreamLed(ctx, nc, "after pod "+pod+"'s restart", name, streamRead, false); err != nil {
			return Restarted{}, err
		}
	}
	return Restarted{Pod: pod, Exit: exit, Varz: varz}, nil
}

// evict sends pod's eviction until it is accepted, again every Retry while the disruption budget
// refuses it, and UNKNOWN once ctx, the pod's limit, ends.
func (r Restart) evict(ctx context.Context, pod string, uid types.UID, limit time.Duration) error {
	retry := r.Retry
	if retry == 0 {
		retry = evictRetry
	}
	evictions := r.Hub.Client.PolicyV1().Evictions(r.Hub.Namespace)
	for {
		err := evictions.Evict(ctx, &policyv1.Eviction{
			ObjectMeta:    metav1.ObjectMeta{Name: pod, Namespace: r.Hub.Namespace},
			DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}},
		})
		switch {
		case err == nil:
			return nil
		case apierrors.IsTooManyRequests(err):
			select {
			case <-ctx.Done():
				return unknownf(StepRestart, err, "the disruption budget refused pod %s's eviction until its limit, %s; the row stays held", pod, limit)
			case <-time.After(retry):
			}
		case ctx.Err() != nil:
			return unknownf(StepRestart, err, "pod %s was not evicted within its limit, %s; the row stays held", pod, limit)
		default:
			return unknownf(StepRestart, err, "evict pod %s", pod)
		}
	}
}

// awaitGone waits for the old pod to be gone, from the watch, or from a read of the pod when the
// watch misses it, and returns its exit from the last status seen.
func awaitGone(ctx context.Context, watcher watch.Interface, pods podReader, old *corev1.Pod, limit time.Duration) (Exit, error) {
	last := old
	events := watcher.ResultChan()
	tick := time.NewTicker(4 * restartPoll)
	defer tick.Stop()
	take := func(event watch.Event) bool {
		seen, ok := event.Object.(*corev1.Pod)
		if !ok || seen.UID != old.UID {
			return false
		}
		last = seen
		return event.Type == watch.Deleted
	}
	for {
		select {
		case <-ctx.Done():
			return Exit{}, unknownf(StepRestart, ctx.Err(), "pod %s (UID %s) was not gone within its limit, %s; the row stays held", old.Name, old.UID, limit)
		case event, open := <-events:
			if !open {
				events = nil
				continue
			}
			if take(event) {
				return exitOf(old, last), nil
			}
		case <-tick.C:
			current, err := pods.Get(ctx, old.Name, metav1.GetOptions{})
			if err == nil && current.UID == old.UID {
				continue
			}
			if err != nil && !apierrors.IsNotFound(err) {
				continue
			}
			// Gone: the events the watch already holds may carry the old pod's last status.
			for drained := false; !drained && events != nil; {
				select {
				case event, open := <-events:
					if !open {
						drained = true
					} else if take(event) {
						drained = true
					}
				default:
					drained = true
				}
			}
			return exitOf(old, last), nil
		}
	}
}

// podReader is the pod read the restart needs, of one namespace.
type podReader interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Pod, error)
}

// exitOf is the nats container's exit as last shows it: its terminated state, or a
// lastState.terminated other than the one old showed before the eviction.
func exitOf(old, last *corev1.Pod) Exit {
	var before *corev1.ContainerStateTerminated
	for _, status := range old.Status.ContainerStatuses {
		if status.Name == natsContainer {
			before = status.LastTerminationState.Terminated
		}
	}
	for _, status := range last.Status.ContainerStatuses {
		if status.Name != natsContainer {
			continue
		}
		terminated := status.State.Terminated
		if terminated == nil && status.LastTerminationState.Terminated != nil &&
			(before == nil || !status.LastTerminationState.Terminated.FinishedAt.Equal(&before.FinishedAt)) {
			terminated = status.LastTerminationState.Terminated
		}
		if terminated != nil {
			return Exit{Seen: true, Code: terminated.ExitCode, Signal: terminated.Signal, Reason: terminated.Reason}
		}
	}
	return Exit{}
}

// awaitReady waits for pod to be back as a new pod, another UID than old, Ready.
func awaitReady(ctx context.Context, pods podReader, pod string, old types.UID, limit time.Duration) error {
	var last error
	for {
		current, err := pods.Get(ctx, pod, metav1.GetOptions{})
		switch {
		case err != nil:
			last = err
		case current.UID == old:
			last = fmt.Errorf("pod %s is still its old UID %s", pod, old)
		case !podReady(current):
			last = fmt.Errorf("pod %s (UID %s) is not Ready", pod, current.UID)
		default:
			return nil
		}
		select {
		case <-ctx.Done():
			return unknownf(StepRestart, last, "pod %s did not come back Ready within its limit, %s; the row stays held", pod, limit)
		case <-time.After(restartPoll):
		}
	}
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// prove runs step 4 on the restarted server until it passes, a check refuses, or the pod's limit
// ends, which is UNKNOWN naming the last reading that did not pass.
func (r Restart) prove(ctx context.Context, server DeclaredServer, before map[string]string, limit time.Duration) (Varz, error) {
	for {
		varz, err := r.proveOnce(ctx, server, before)
		if err == nil {
			return varz, nil
		}
		var refusal *Refusal
		if errors.As(err, &refusal) {
			return Varz{}, err
		}
		select {
		case <-ctx.Done():
			return Varz{}, unknownf(StepProof, err, "pod %s did not pass step 4 within its limit, %s; the row stays held", server.Pod, limit)
		case <-time.After(restartPoll):
		}
	}
}

// proveOnce is one reading of step 4's NATS half, then the binding half (Bind):
//
//   - the server's /varz server_id differs from its step-1 id (a server's id is new at each start),
//     so a server that keeps its id stops the hold;
//   - every declared server's routes, leafnodes and gateways pass step 1's checks again, against
//     the servers' ids now: a route still listed to a restarted server's old id is UNKNOWN until
//     it is gone;
//   - the probe, dialled to the server's own client address alone, is refused.
func (r Restart) proveOnce(ctx context.Context, server DeclaredServer, before map[string]string) (Varz, error) {
	reading, err := readServers(ctx, StepProof, r.Hub.Monitor, r.Declared.pods())
	if err != nil {
		return Varz{}, err
	}
	defer reading.close()
	now := reading.varz[server.Pod]
	if now.ServerID == server.Varz.ServerID && !r.keptIDs {
		return Varz{}, refusef(StepProof, "%s kept its step-1 id across its pod's restart: its nats-server did not restart", describeServer(server.Pod, now))
	}
	if err := reading.checkTopology(ctx, StepProof, r.Declared.names(), before); err != nil {
		return Varz{}, err
	}
	url, stop, err := r.Hub.ClientURL(ctx, server.Pod)
	if err != nil {
		return Varz{}, unknownf(StepProof, err, "%s's own client address", describeServer(server.Pod, now))
	}
	err = r.Probe(ctx, url)
	stop()
	switch {
	case err == nil:
		return Varz{}, refusef(StepProof, "%s admitted the probe on its own client address", describeServer(server.Pod, now))
	case !errors.Is(err, nats.ErrAuthorization):
		return Varz{}, unknownf(StepProof, err, "the probe on %s's own client address reached no decision", describeServer(server.Pod, now))
	}
	if err := r.Bind(ctx, server.Pod, server.Varz, now); err != nil {
		return Varz{}, err
	}
	return now, nil
}

// awaitJetStream waits for $JS.API.INFO to answer on the operator's connection after pod's restart.
func awaitJetStream(ctx context.Context, js jetstream.JetStream, pod string, limit time.Duration) error {
	for {
		attempt, cancel := context.WithTimeout(ctx, jsRequest)
		_, err := js.AccountInfo(attempt)
		cancel()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return unknownf(StepRestart, err, "$JS.API.INFO did not answer after pod %s's restart within its limit, %s; the row stays held", pod, limit)
		case <-time.After(restartPoll):
		}
	}
}

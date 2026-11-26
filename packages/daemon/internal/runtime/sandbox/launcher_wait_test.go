package sandbox

import (
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testwait"
	"github.com/sjawhar/legion/daemon/internal/workspace"
)

// fetching is a pod's status while workspace-fetch runs: Pending, workspace-init not started.
func fetching(p *corev1.Pod) {
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
		{Name: fetchContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}},
		{Name: initContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	}}
}

// pulling is a pod's status before any init container has started: an image pull.
func pulling(p *corev1.Pod) {
	p.Status = corev1.PodStatus{Phase: corev1.PodPending, InitContainerStatuses: []corev1.ContainerStatus{
		{Name: fetchContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
		{Name: initContainer, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}}},
	}}
}

// unstartedNode is the node a pod that never started is scheduled on (notReadyToStart).
const unstartedNode = "ip-192-0-2-9"

// notReadyToStart is a pod's status on a node that has not made it ready to start its containers,
// as a node that cannot surface the pod's new disk leaves it (FailedMount): scheduled there,
// PodReadyToStartContainers False, and its init containers waiting, each condition written at
// reported.
func notReadyToStart(reported time.Time) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		pulling(p)
		p.Spec.NodeName = unstartedNode
		at := metav1.NewTime(reported)
		p.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: at},
			{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionFalse, LastTransitionTime: at},
			{Type: corev1.PodInitialized, Status: corev1.ConditionFalse, Reason: "ContainersNotInitialized", LastTransitionTime: at},
		}
	}
}

// pendingPod waits, as long as a loaded machine needs, for the pod named name that the launch
// under test makes, sets its status with status, and returns when it first saw the pod: the wait
// for the pod's launcher begins once the runtime's store holds it, so no earlier.
func (g *rig) pendingPod(name string, status func(*corev1.Pod)) time.Time {
	g.t.Helper()
	testwait.Eventually(g.t, "the new Pending pod", func() bool { return g.pod(name) != nil })
	appeared := time.Now()
	g.update(g.pod(name), status)
	return appeared
}

// A pod's role launchers start only once its init containers have finished, and workspace-fetch's
// clone, with its retries of a transient failure, may run past the boot timeout. A launch whose pod
// is still provisioning then keeps waiting for its launcher, and starts the role in that same pod
// once the launcher connects: the claim is charged no launch failure.
func TestALaunchWaitsForAPodProvisioningPastTheBootTimeout(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.autoStart.Store(false)
	spec := workerSpec(t)
	machine, _, _ := supervisedSandboxMachine(t, g, spec)
	launched := make(chan error, 1)
	go func() { launched <- machine.Handle(g.ctx, supervise.RequestSpawn{Claim: spec.Claim}) }()
	name := SandboxName(spec.Claim)
	g.pendingPod(name, fetching)
	uid := g.pod(name).UID

	select {
	case err := <-launched:
		t.Fatalf("the launch ended while its pod was still in workspace-fetch: %v", err)
	case <-time.After(g.opts.BootTimeout + time.Second):
	}
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = runningStatusOf(p.Spec) })
	g.launcher(spec.Claim)
	select {
	case err := <-launched:
		if err != nil {
			t.Fatalf("the launch failed once its pod had provisioned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launch did not start the role once its launcher connected")
	}
	got := machine.Claim()
	if got.Budgets.LaunchFailures != 0 || got.Locator == nil || got.Locator.Sandbox.PodUID != string(uid) {
		t.Fatalf("claim after the launch = %+v, want it launched in pod %s with no launch failure", got, uid)
	}
	if !slices.ContainsFunc(g.sent(spec.Claim), func(frame shimwire.Frame) bool {
		start, ok := frame.(shimwire.LauncherStart)
		return ok && start.Generation == got.Generation
	}) {
		t.Fatalf("the launcher was sent %v, want the generation %d start", g.sent(spec.Claim), got.Generation)
	}
}

// The wait provisioning extends is still bounded once provisioning ends: the launcher then has the
// boot timeout to connect, and a launch whose launcher never does fails then, not at the end of
// ProvisionBound.
func TestALaunchGivesUpTheBootTimeoutAfterItsPodStopsProvisioning(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.autoStart.Store(false)
	result := make(chan error, 1)
	go func() { _, err := g.r.Spawn(g.ctx, workerSpec(t)); result <- err }()
	name := SandboxName(workerToken)
	appeared := g.pendingPod(name, fetching)
	time.Sleep(g.opts.BootTimeout + time.Second)
	provisioned := time.Now()
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = runningStatusOf(p.Spec) })

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "wait for its role launcher: timed out after") {
			t.Fatalf("Spawn = %v, want it timed out waiting for the role launcher", err)
		}
		if waited := time.Since(provisioned); waited < g.opts.BootTimeout-2*recheckInterval {
			t.Fatalf("the launch gave up %s after its pod provisioned, want about the boot timeout %s", waited, g.opts.BootTimeout)
		}
		if waited := time.Since(appeared); waited < 2*g.opts.BootTimeout {
			t.Fatalf("the launch gave up %s after its pod appeared, want the boot timeout counted from the end of provisioning", waited)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("hang guard: the launch never ended after its pod provisioned")
	}
}

// A pod that never runs an init container, as one whose image pull never finishes, is not
// provisioning: its launch gives up at the boot timeout.
func TestALaunchWhosePodNeverProvisionsGivesUpAtTheBootTimeout(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.autoStart.Store(false)
	result := make(chan error, 1)
	go func() { _, err := g.r.Spawn(g.ctx, workerSpec(t)); result <- err }()
	name := SandboxName(workerToken)
	g.pendingPod(name, pulling)

	select {
	case err := <-result:
		want := "wait for its role launcher: timed out after " + g.opts.BootTimeout.String() + " waiting for role launcher " + string(workerToken)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Spawn = %v, want %q", err, want)
		}
	case <-time.After(90 * time.Second):
		t.Fatal("hang guard: the launch never ended for a pod that never provisioned")
	}
}

// A pod has never started only while it is scheduled, still Pending, and its node has not made it
// ready to start its containers, once it was created longer than the bound ago and scheduled longer
// than its margin ago: half the bound, or a minute where that is less. Its age counts from its
// creation, not from the conditions, which come seconds after it (the row 30 s later); and a pod
// that waited unscheduled for room for longer than the bound reads the same in the seconds after
// its node gets it, so its scheduling must be old too (the rows of a pod created 15 min ago,
// against the minute at the 10 min bound and against half of a 90 s bound). The scheduling clock is
// PodScheduled's, not PodReadyToStartContainers', which the kubelet writes later (the rows where
// the two differ). A pod whose node reports no readiness to start is the launcher wait's, as an
// image pull is; a pod not scheduled is the scheduler's; a pod ready to start whose init container
// still waits is starting; a pod whose creation or scheduling time is unknown is not judged; and a
// pod that has left Pending has started, whatever its node reports of it since.
func TestAPodNotReadyToStartHasNeverStartedOnceCreatedPastTheBoundAndScheduledPastItsMargin(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 17, 15, 8, 0, 0, time.UTC)
	bound, short := 10*time.Minute, 90*time.Second
	waited := 15 * time.Minute
	// undated stands for a time the pod does not carry: its creation or a condition's, unknown.
	const undated time.Duration = -1
	for _, tc := range []struct {
		name  string
		phase corev1.PodPhase
		// bound is the bound the row is judged against, the 10 min one where it is 0.
		bound time.Duration
		// created is how long ago the pod was created, scheduledAgo how long ago PodScheduled took
		// its status, and readyAgo how long ago PodReadyToStartContainers did, each undated where
		// that time is unknown. scheduled and ready are the two conditions' statuses, "" for a
		// condition the pod does not carry.
		created, scheduledAgo, readyAgo time.Duration
		scheduled, ready                corev1.ConditionStatus
		// unstarted is the pod's age as neverStarted counts it, 0 for a pod it does not judge. The
		// scheduling age it counts is scheduledAgo for every pod it judges, and 0 for the others.
		unstarted time.Duration
		never     bool
	}{
		{name: "a pod with no conditions", created: time.Hour},
		{name: "a pod not scheduled", created: time.Hour, scheduledAgo: time.Hour, readyAgo: time.Hour,
			scheduled: corev1.ConditionFalse, ready: corev1.ConditionFalse},
		{name: "a pod scheduled whose node reports no readiness to start", created: time.Hour, scheduledAgo: time.Hour,
			scheduled: corev1.ConditionTrue},
		{name: "a pod ready to start whose init container waits", created: time.Hour, scheduledAgo: time.Hour, readyAgo: time.Hour,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionTrue},
		{name: "a pod not ready to start created less than the bound ago", created: bound - time.Second, scheduledAgo: bound - time.Second, readyAgo: bound - time.Second,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: bound - time.Second},
		{name: "a pod not ready to start created exactly the bound ago", created: bound, scheduledAgo: bound, readyAgo: bound,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: bound},
		{name: "a pod not ready to start created longer than the bound ago", created: bound + time.Second, scheduledAgo: bound + time.Second, readyAgo: bound + time.Second,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: bound + time.Second, never: true},
		{name: "a pod created longer than the bound ago whose node reported it not ready to start 30 s later", created: bound + 2*time.Second,
			scheduledAgo: bound - 28*time.Second, readyAgo: bound - 28*time.Second,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: bound + 2*time.Second, never: true},
		{name: "a pod created 15 min ago that waited unscheduled until its node got it 5 s ago", created: waited, scheduledAgo: 5 * time.Second, readyAgo: 5 * time.Second,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: waited},
		{name: "a pod created 15 min ago that its node got exactly a minute ago", created: waited, scheduledAgo: time.Minute, readyAgo: time.Minute,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: waited},
		{name: "a pod created 15 min ago that its node got a minute and 1 s ago", created: waited, scheduledAgo: time.Minute + time.Second, readyAgo: time.Minute + time.Second,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: waited, never: true},
		{name: "against a 90 s bound, a pod created 15 min ago that its node got exactly half the bound ago", bound: short, created: waited,
			scheduledAgo: short / 2, readyAgo: short / 2, scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: waited},
		{name: "against a 90 s bound, a pod created 15 min ago that its node got half the bound and 1 s ago", bound: short, created: waited,
			scheduledAgo: short/2 + time.Second, readyAgo: short/2 + time.Second, scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: waited, never: true},
		{name: "a pod created 15 min ago that its node got 6 min ago and reported not ready to start 30 s ago", created: waited,
			scheduledAgo: 6 * time.Minute, readyAgo: 30 * time.Second, scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: waited, never: true},
		{name: "a pod created 15 min ago that its node got 30 s ago and reported not ready to start with no time", created: waited,
			scheduledAgo: 30 * time.Second, readyAgo: undated, scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse, unstarted: waited},
		{name: "a pod not ready to start whose creation is unknown", created: undated, scheduledAgo: time.Hour, readyAgo: time.Hour,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse},
		{name: "a pod not ready to start whose scheduling time is unknown", created: time.Hour, scheduledAgo: undated, readyAgo: time.Hour,
			scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse},
		{name: "a pod Running whose node has since stopped making it ready", phase: corev1.PodRunning, created: time.Hour,
			scheduledAgo: time.Hour, readyAgo: time.Hour, scheduled: corev1.ConditionTrue, ready: corev1.ConditionFalse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{}
			pulling(pod)
			if tc.phase != "" {
				pod.Status.Phase = tc.phase
			}
			at := func(ago time.Duration) metav1.Time {
				if ago == undated {
					return metav1.Time{}
				}
				return metav1.NewTime(now.Add(-ago))
			}
			pod.CreationTimestamp = at(tc.created)
			for _, condition := range []corev1.PodCondition{
				{Type: corev1.PodScheduled, Status: tc.scheduled, LastTransitionTime: at(tc.scheduledAgo)},
				{Type: corev1.PodReadyToStartContainers, Status: tc.ready, LastTransitionTime: at(tc.readyAgo)},
			} {
				if condition.Status != "" {
					pod.Status.Conditions = append(pod.Status.Conditions, condition)
				}
			}
			judged, scheduledFor := bound, time.Duration(0)
			if tc.bound != 0 {
				judged = tc.bound
			}
			if tc.unstarted != 0 {
				scheduledFor = tc.scheduledAgo
			}
			if unstarted, scheduled, never := neverStarted(pod, now, judged); unstarted != tc.unstarted || scheduled != scheduledFor || never != tc.never {
				t.Fatalf("neverStarted = %s, %s, %t; want %s, %s, %t", unstarted, scheduled, never, tc.unstarted, scheduledFor, tc.never)
			}
		})
	}
}

// A node that cannot surface a pod's new disk leaves the pod scheduled and never ready to start its
// containers, so its launcher never connects and the launch gives up at the boot timeout, with the
// error the supervisor charges as a launch failure. The next try finds the pod created longer than
// the boot timeout ago and still not ready to start, and replaces it — a new pod, which for this
// issue pod is a new disk — and says so, naming the pod, its node, how long it has not started and
// how long its node has had it, rather than waiting on the same pod again at every try until the
// claim's launch budget runs out. The replaced pod ran no role, so nothing is named displaced from
// it. A pod created less than the boot timeout ago is kept, as a try keeps every pod of its own
// still coming up.
// The launch's wait starts once the store holds the pod, so it gives up about a boot timeout after
// the pod's creation, and the next try follows it by a backoff of a second or two, while the pod's
// conditions come seconds after its creation: so the pod's age counts from its creation, since
// from the conditions that try would find it younger than the boot timeout, keep it and wait on it
// once more. The second row is that case at the rig's 2 s boot timeout: the conditions written a
// second after the pod appeared, the next try half a second past the boot timeout.
// A pod that waited unscheduled for room through the first try, and that its node got just before
// the next one, reads the same in the seconds until that node has made it ready to start, created
// longer than the boot timeout ago. The next try keeps it, since its node got it less than its
// margin ago, half the rig's boot timeout: it is about to start. The last row is that case, the
// pod placed half a second before the next try, three boot timeouts after it appeared.
func TestALaunchReplacesAPodItsNodeNeverStartedOncePastTheBootTimeout(t *testing.T) {
	t.Parallel()
	const said = neverStartedWarning
	boot := testOptions().BootTimeout
	for _, tc := range []struct {
		name string
		// reported is how long after the pod appeared its node wrote its conditions, and aged how
		// far the clock has moved since the pod appeared when the next try starts.
		reported, aged time.Duration
		// scheduledBefore, for a pod that waited unscheduled through the first try, is how long
		// before the next try its node got it and wrote its conditions; 0 for a pod its node got
		// at once.
		scheduledBefore time.Duration
		replaces        bool
	}{
		{name: "not started for longer than the boot timeout", aged: boot + time.Second, replaces: true},
		{name: "reported a while after the pod appeared, the next try just past the boot timeout", reported: boot / 2, aged: boot + boot/4, replaces: true},
		{name: "not started for less than the boot timeout", aged: boot - time.Second},
		{name: "unscheduled past the boot timeout, its node got it just before the next try", aged: 3 * boot, scheduledBefore: boot / 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedLog{}
			g := newRig(t, nil, withOptions(func(o *Options) {
				o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			}))
			g.autoStart.Store(false)
			spec := workerSpec(t)
			name := SandboxName(spec.Claim)
			result := make(chan error, 1)
			go func() { _, err := g.r.Spawn(g.ctx, spec); result <- err }()
			status := notReadyToStart(g.now.Load().Add(tc.reported))
			if tc.scheduledBefore > 0 {
				status = func(p *corev1.Pod) { p.Status = unscheduledFor(0) }
			}
			g.pendingPod(name, status)
			stuck := g.pod(name).UID
			select {
			case err := <-result:
				want := "wait for its role launcher: timed out after " + g.opts.BootTimeout.String()
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("Spawn = %v, want %q, the failure the supervisor charges", err, want)
				}
			case <-time.After(90 * time.Second):
				t.Fatal("hang guard: the launch never ended for a pod that never started")
			}

			g.advance(tc.aged)
			if tc.scheduledBefore > 0 {
				g.update(g.pod(name), notReadyToStart(g.now.Load().Add(-tc.scheduledBefore)))
				g.eventually("the store to see the pod on its node", func() bool {
					pod := g.r.storedPod(name)
					return pod != nil && pod.Spec.NodeName == unstartedNode
				})
			}
			g.autoStart.Store(true)
			spec.Generation = 2
			loc := g.spawn(spec)
			if replaced := loc.Sandbox.PodUID != string(stuck); replaced != tc.replaces {
				t.Fatalf("the next try launched in pod %s, the pod that never started being %s: replaced %t, want %t",
					loc.Sandbox.PodUID, stuck, replaced, tc.replaces)
			}
			logged := logs.String()
			if !tc.replaces {
				if strings.Contains(logged, said) {
					t.Fatalf("the try that kept the pod logged %q", logged)
				}
				return
			}
			for _, want := range []string{said, "pod=" + name, "uid=" + string(stuck), "node=" + unstartedNode,
				"notStartedFor=" + tc.aged.Round(time.Second).String(), "scheduledFor=" + (tc.aged - tc.reported).Round(time.Second).String()} {
				if !strings.Contains(logged, want) {
					t.Errorf("the try that replaced the pod logged %q, want it to say %q", logged, want)
				}
			}
			if displaced := g.sandbox(name).Annotations[displacedAnnotation]; displaced == string(stuck) {
				t.Errorf("the Sandbox names the pod that never started, %s, displaced", stuck)
			}
		})
	}
}

// The launcher's deadline is the boot timeout after the pod was last seen provisioning, and never
// past the boot timeout plus ProvisionBound from the first time it was. The last row is the edge a
// cap counted from the wait's start got wrong: a 60 s image pull, a fetch that ran the whole bound
// this case gives it, then a 90 s workspace-init, whose launcher connects 32 min 30 s after the
// wait began.
func TestTheLauncherDeadlineFollowsProvisioningUpToItsBound(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 10, 13, 17, 49, 23, 0, time.UTC)
	boot, bound := 2*time.Minute, 30*time.Minute
	for _, tc := range []struct {
		name        string
		first, last time.Duration
		want        time.Duration
	}{
		{"provisioning at the start", 0, 0, boot},
		{"provisioning ten minutes in", 0, 10 * time.Minute, 10*time.Minute + boot},
		{"provisioning right up to the bound", 0, bound, bound + boot},
		{"provisioning past the bound", 0, bound + time.Minute, bound + boot},
		{"a pull, a whole fetch, then workspace-init", time.Minute, 32*time.Minute + 30*time.Second, time.Minute + bound + boot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := launcherDeadline(start.Add(tc.first), start.Add(tc.last), boot, bound).Sub(start); got != tc.want {
				t.Fatalf("launcherDeadline = start + %s, want start + %s", got, tc.want)
			}
		})
	}
}

// The launch's wait for its pod's launcher outlasts the fetch's whole clocked budget, which the
// fetch's waits and clocked stages keep to, so a fetch still within it is never cut short by the
// launch, and one that runs out of one of those bounds fails, naming why, before the launch would
// give up. The stream from the repository cache is the one part outside them, on the low-speed
// bound rather than a clock (workspace.CacheStreamLowSpeedBytes), and it runs on into the slack
// this wait holds over what the fetch actually waited. With the default 2-minute boot timeout
// and the runtime's own bound: a pod pulled in a minute whose fetch waits 35 minutes for its
// repository's cache is still waited on, and one whose fetch runs its whole FetchBudget leaves
// workspace-init the boot timeout.
func TestTheLauncherWaitOutlastsTheFetchsWholeBudget(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	start := time.Date(2026, 10, 16, 23, 0, 0, 0, time.UTC)
	first, boot := start.Add(time.Minute), 2*time.Minute
	for _, tc := range []struct {
		name string
		last time.Duration
		want time.Duration
	}{
		{"a fetch waiting 35 minutes for its repository's cache", 35 * time.Minute, 35*time.Minute + boot},
		{"a fetch running its whole budget", workspace.FetchBudget, workspace.FetchBudget + boot},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := launcherDeadline(first, first.Add(tc.last), boot, g.r.ProvisionBound()).Sub(first); got != tc.want {
				t.Fatalf("launcherDeadline = first provisioning + %s, want + %s: the fetch's own bound ends first", got, tc.want)
			}
		})
	}
}

// A launch that gives up on a pod whose workspace-fetch is still running says so, with the state
// of the repository cache that fetch fills a fresh feed from: the fetch's own failure, which would
// name it, has not come.
func TestALaunchThatGivesUpOnAFetchNamesTheRepositorysCache(t *testing.T) {
	t.Parallel()
	g := newRig(t, nil)
	g.autoStart.Store(false)
	g.r.provisionBound = time.Second
	spec := workerSpec(t)
	result := make(chan error, 1)
	go func() { _, err := g.r.Spawn(g.ctx, spec); result <- err }()
	g.pendingPod(SandboxName(spec.Claim), fetching)

	select {
	case err := <-result:
		want := "; its workspace-fetch is still running; the repository cache of " + spec.Repository.String() + ", which a fresh feed comes from alone, is "
		if err == nil || !strings.Contains(err.Error(), "wait for its role launcher: timed out after") || !strings.Contains(err.Error(), want) {
			t.Fatalf("Spawn = %v, want the launcher wait's timeout naming the running fetch and %q", err, want)
		}
	case <-time.After(2*g.opts.BootTimeout + 5*time.Second):
		t.Fatal("the launch still waits past its bound on a pod that is still fetching")
	}
}

// The cap is counted from the first time the wait sees the pod provisioning, not from the wait's
// start: an image pull before workspace-fetch does not eat into the fetch's bound. With a 2 s boot
// timeout and a 3 s bound, the pod pulls for 1.5 s and provisions until 5.5 s after it appeared,
// past the 5 s a cap from the wait's start would allow; its launcher connects then, and the launch
// starts the role.
func TestALaunchCountsItsProvisioningBoundFromTheFirstInitContainer(t *testing.T) {
	// Not parallel: it moves its pod into provisioning by the wall clock 500 ms before the 2 s boot
	// timeout runs out, a window other tests' load could make the runtime miss.
	g := newRig(t, nil)
	g.autoStart.Store(false)
	g.r.provisionBound = 3 * time.Second
	spec := workerSpec(t)
	result := make(chan error, 1)
	go func() { _, err := g.r.Spawn(g.ctx, spec); result <- err }()
	name := SandboxName(spec.Claim)
	appeared := g.pendingPod(name, pulling)
	time.Sleep(time.Until(appeared.Add(1500 * time.Millisecond)))
	g.update(g.pod(name), fetching)
	time.Sleep(time.Until(appeared.Add(5500 * time.Millisecond)))
	g.update(g.pod(name), func(p *corev1.Pod) { p.Status = runningStatusOf(p.Spec) })
	g.launcher(spec.Claim)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Spawn = %v %s after its pod appeared, want the role started", err, time.Since(appeared).Round(time.Millisecond))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the launch did not start the role once its launcher connected")
	}
}

// neverStartedWarning is what a launch logs when it replaces a pod that its node never started
// (neverStarted).
const neverStartedWarning = "sandbox runtime: replacing a pod older than its start bound that its node has not made ready to start its containers"

// A pod with a kept volume (keptVolume) can stand Pending on its new node, scheduled and not ready
// to start its containers, while its volume moves to that node. The upstream attach-detach
// controller force-detaches the volume 6 minutes after the old pod is gone from the API, and the
// attach takes up to one minute more. So a launch gives such a pod its start bound (startBound) in
// place of the boot timeout: the kept-volume bound of 7 minutes (keptVolumeAttachWait), or the
// boot timeout where that is longer. Every other pod, the controller's too, keeps the boot
// timeout. neverStarted counts the bound from the pod's creation, with its scheduling margin.
// awaitRoleLauncher counts it from the start of its wait until it first sees the pod
// provisioning.
//
// On the rig's clock, at a boot timeout of 120 s, a kept pod that was created and scheduled at the
// same moment has not yet failed to start 5 minutes after its creation. It has failed 8 minutes
// after it. A pod with an ephemeral volume has failed at 3 minutes. At INFRA's boot timeout of
// 600 s, the kept pod has not failed at 9 minutes and has failed at 11: its bound is then the boot
// timeout, not a fixed 7 minutes. A relaunch keeps a pod that has not failed. It replaces a pod
// that has failed, names the start bound, and keeps the Sandbox and its uid, so a kept volume's
// next pod mounts the same claim.
//
// On the wall clock, the rig sets the boot timeout and the kept-volume bound short. A launch whose
// launcher never connects then gives up at its pod's start bound and names it: the kept-volume
// bound for a kept pod where that bound is longer, the boot timeout for a pod with an ephemeral
// volume in the same rig, and the boot timeout for a kept pod where the timeout is longer. When
// the wait first sees a kept pod provisioning just before its start bound, the wait runs on to the
// deadline that launcherDeadline sets, and the wait's backstop does not end it first. With
// keep_volume on, the launch try's own bound (LaunchBound) counts a start of 7 minutes.
func TestAKeptVolumePodWaitsOutADetach(t *testing.T) {
	// Not parallel: a subtest moves its pod into provisioning by the wall clock 1 s before an 8 s
	// start bound runs out, a window other tests' load could make the runtime miss.
	const boot, infraBoot = 120 * time.Second, 600 * time.Second
	for _, tc := range []struct {
		name string
		// boot is the rig's boot timeout, and aged is how long after the pod's creation and
		// scheduling the next launch reads it.
		boot, aged time.Duration
		// kept is whether the pod keeps its volume: LEGION-208's pod does, and the pod of the
		// canary LEGION-209 runs in gVisor on a volume of its own.
		kept bool
		// bound is the pod's start bound, and stuck is whether the pod has never started at aged.
		bound time.Duration
		stuck bool
	}{
		{name: "a kept pod 5 minutes after its creation", boot: boot, aged: 5 * time.Minute, kept: true, bound: 7 * time.Minute},
		{name: "a kept pod 8 minutes after its creation", boot: boot, aged: 8 * time.Minute, kept: true, bound: 7 * time.Minute, stuck: true},
		{name: "a pod with an ephemeral volume 3 minutes after its creation", boot: boot, aged: 3 * time.Minute, bound: boot, stuck: true},
		{name: "a kept pod at a boot timeout of 600 s, 9 minutes after its creation", boot: infraBoot, aged: 9 * time.Minute, kept: true, bound: infraBoot},
		{name: "a kept pod at a boot timeout of 600 s, 11 minutes after its creation", boot: infraBoot, aged: 11 * time.Minute, kept: true, bound: infraBoot, stuck: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedLog{}
			g := newRig(t, nil, withOptions(func(o *Options) {
				keptVolumes(SandboxMicroVM, childIssue)(o)
				o.BootTimeout = tc.boot
				o.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
			}))
			g.autoStart.Store(false)
			spec := childSpec(t)
			if tc.kept {
				spec = workerSpec(t)
			}
			name := SandboxName(spec.Claim)
			awaitingItsRoleLauncher(t, g, spec)
			unstarted := g.pod(name)
			// The pod's node got it at its creation and has not made it ready to start since.
			g.update(unstarted, notReadyToStart(unstarted.CreationTimestamp.Time))
			g.eventually("the store to see the pod on its node", func() bool {
				pod := g.r.storedPod(name)
				return pod != nil && pod.Spec.NodeName == unstartedNode
			})
			s := g.sandbox(name)
			if kept := ownsKeptVolume(s); kept != tc.kept {
				t.Fatalf("the issue's Sandbox owns a kept volume: %t, want %t", kept, tc.kept)
			}
			g.advance(tc.aged)
			if got := g.r.startBound(s); got != tc.bound {
				t.Fatalf("startBound = %s, want %s", got, tc.bound)
			}
			if _, _, never := neverStarted(g.r.storedPod(name), g.r.now(), g.r.startBound(s)); never != tc.stuck {
				t.Fatalf("neverStarted %s after the pod's creation = %t, want %t", tc.aged, never, tc.stuck)
			}

			g.autoStart.Store(true)
			spec.Generation = 2
			loc := g.spawn(spec)
			if replaced := loc.Sandbox.PodUID != string(unstarted.UID); replaced != tc.stuck {
				t.Fatalf("the next launch ran in pod %s, the pod that never started being %s: replaced %t, want %t",
					loc.Sandbox.PodUID, unstarted.UID, replaced, tc.stuck)
			}
			if after := g.sandbox(name); after == nil || after.UID != s.UID || ownsKeptVolume(after) != tc.kept {
				t.Fatalf("the issue's Sandbox after the launch = %+v, want %s, the same Sandbox", after, s.UID)
			}
			logged := logs.String()
			if !tc.stuck {
				if strings.Contains(logged, neverStartedWarning) {
					t.Fatalf("the launch that kept the pod logged %q", logged)
				}
				return
			}
			for _, want := range []string{neverStartedWarning, "uid=" + string(unstarted.UID),
				"notStartedFor=" + tc.aged.String(), "startBound=" + tc.bound.String()} {
				if !strings.Contains(logged, want) {
					t.Errorf("the launch that replaced the pod logged %q, want it to say %q", logged, want)
				}
			}
		})
	}

	t.Run("the start bound of each Sandbox", func(t *testing.T) {
		if keptVolumeAttachWait != 7*time.Minute {
			t.Fatalf("keptVolumeAttachWait = %s, want 7m0s: 6 minutes to the force-detach and one minute for the attach", keptVolumeAttachWait)
		}
		stored := func(template func(*testing.T, *unstructured.Unstructured) *unstructured.Unstructured) func(*testing.T, *Runtime) *sandbox {
			return func(t *testing.T, _ *Runtime) *sandbox {
				t.Helper()
				s, err := decodeSandbox(template(t, sandboxObject(t, SandboxName(workerToken), "uid-sandbox", modeRunning, claimLabels(claim.RoleTester))))
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
		}
		kept := stored(func(t *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured {
			return withKeptTemplate(t, u, true)
		})
		for _, tc := range []struct {
			name    string
			boot    time.Duration
			sandbox func(*testing.T, *Runtime) *sandbox
			want    time.Duration
		}{
			{"a kept volume's, at the default boot timeout", boot, kept, 7 * time.Minute},
			{"an ephemeral volume's", boot, stored(func(_ *testing.T, u *unstructured.Unstructured) *unstructured.Unstructured { return u }), boot},
			{"the unlabelled issue template's of the release before the pod-owned disk", boot, stored(withSupersededDisk), boot},
			{"the controller's", boot, func(t *testing.T, r *Runtime) *sandbox {
				s := r.sandboxManifest(launchOf(t, r, controllerSpec(t)))
				return &s
			}, boot},
			{"a kept volume's, at a boot timeout of 10 minutes", 10 * time.Minute, kept, 10 * time.Minute},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: keptMicroVM()}, func(o *Options) { o.BootTimeout = tc.boot })
				if r.keptVolumeBound != keptVolumeAttachWait {
					t.Fatalf("the runtime's kept-volume bound = %s, want %s", r.keptVolumeBound, keptVolumeAttachWait)
				}
				if got := r.startBound(tc.sandbox(t, r)); got != tc.want {
					t.Fatalf("startBound = %s, want %s", got, tc.want)
				}
			})
		}
	})

	for _, tc := range []struct {
		name string
		// boot is the rig's boot timeout and kept its kept-volume bound.
		boot, kept time.Duration
		// keeps is whether the launch's pod keeps its volume, and want is the start bound it waits.
		keeps bool
		want  time.Duration
	}{
		{"a kept pod, the kept-volume bound longer than the boot timeout", time.Second, 3 * time.Second, true, 3 * time.Second},
		{"a pod with an ephemeral volume, at the same bounds", time.Second, 3 * time.Second, false, time.Second},
		{"a kept pod, the kept-volume bound shorter than the boot timeout", 2 * time.Second, time.Second, true, 2 * time.Second},
	} {
		t.Run("a launch whose launcher never connects: "+tc.name, func(t *testing.T) {
			g := newRig(t, nil, withOptions(func(o *Options) {
				keptVolumes(SandboxMicroVM, childIssue)(o)
				o.BootTimeout = tc.boot
			}))
			g.r.keptVolumeBound = tc.kept
			g.autoStart.Store(false)
			spec := childSpec(t)
			if tc.keeps {
				spec = workerSpec(t)
			}
			name := SandboxName(spec.Claim)
			result := make(chan error, 1)
			go func() { _, err := g.r.Spawn(g.ctx, spec); result <- err }()
			appeared := g.pendingPod(name, pulling)

			select {
			case err := <-result:
				waited := time.Since(appeared)
				want := "wait for its role launcher: timed out after " + tc.want.String() + " waiting for role launcher " + string(spec.Claim)
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("Spawn = %v, want %q", err, want)
				}
				if waited < tc.want-recheckInterval {
					t.Fatalf("the launch gave up %s after its pod appeared, want its start bound %s", waited, tc.want)
				}
			case <-time.After(90 * time.Second):
				t.Fatal("hang guard: the launch never ended for a pod whose launcher never connects")
			}
			if kept := ownsKeptVolume(g.sandbox(name)); kept != tc.keeps {
				t.Fatalf("the issue's Sandbox owns a kept volume: %t, want %t", kept, tc.keeps)
			}
		})
	}

	// The start bound is 8 s and ProvisionBound 3 s. The wait first sees the pod provisioning 7 s
	// after the pod appeared, and from then on its deadline is the boot timeout after the last time
	// it sees the pod provisioning, up to the boot timeout plus ProvisionBound after the first
	// (launcherDeadline): 12 s. The launcher connects at 10 s, past the 8 s backstop that twice
	// the boot timeout would set (2*boot + ProvisionBound + 2*recheckInterval). Each step has a
	// margin of 1 s or more, so a loaded runner does not move one past the next.
	t.Run("a kept pod first seen provisioning just before its start bound", func(t *testing.T) {
		const boot, kept, provision = 2 * time.Second, 8 * time.Second, 3 * time.Second
		g := newRig(t, nil, withOptions(func(o *Options) {
			keptVolumes(SandboxMicroVM)(o)
			o.BootTimeout = boot
		}))
		g.r.keptVolumeBound, g.r.provisionBound = kept, provision
		g.autoStart.Store(false)
		spec := workerSpec(t)
		name := SandboxName(spec.Claim)
		result := make(chan error, 1)
		go func() { _, err := g.r.Spawn(g.ctx, spec); result <- err }()
		appeared := g.pendingPod(name, pulling)
		time.Sleep(time.Until(appeared.Add(kept - time.Second)))
		g.update(g.pod(name), fetching)
		time.Sleep(time.Until(appeared.Add(2*boot + provision + 2*recheckInterval + 2*time.Second)))
		g.update(g.pod(name), func(p *corev1.Pod) { p.Status = runningStatusOf(p.Spec) })
		g.launcher(spec.Claim)
		// The launcher's state wakes no wait: this update does, so the wait reads it at once.
		g.update(g.pod(name), func(*corev1.Pod) {})

		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("Spawn = %v %s after its pod appeared, want the role started", err, time.Since(appeared).Round(time.Millisecond))
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the launch did not start the role once its launcher connected")
		}
		if !ownsKeptVolume(g.sandbox(name)) {
			t.Fatal("the issue's Sandbox owns no kept volume")
		}
	})

	t.Run("LaunchBound counts the kept-volume bound once keep_volume is on", func(t *testing.T) {
		const grace = 10 * time.Second
		at := func(block *MicroVM) *Runtime {
			return sandboxRuntime(t, SandboxConfig{Type: SandboxMicroVM, MicroVM: block}, func(o *Options) { o.TerminationGrace = grace })
		}
		on, off := at(keptMicroVM()), at(testMicroVM())
		// 6 h 4 min 41 s: launchBound at the default boot timeout and grace over ProvisionBound's 295
		// minutes, 15 more than before the fetch's attempt on the cache's bucket joined its budget.
		if got, want := off.LaunchBound(), launchBound(boot, boot, off.ProvisionBound(), grace); got != want || want != 6*time.Hour+4*time.Minute+41*time.Second {
			t.Fatalf("LaunchBound with keep_volume off = %s, want %s (6h4m41s)", got, want)
		}
		if got, want := on.LaunchBound(), launchBound(boot, keptVolumeAttachWait, on.ProvisionBound(), grace); got != want || got != off.LaunchBound()+keptVolumeAttachWait-boot {
			t.Fatalf("LaunchBound with keep_volume on = %s, want %s: %s more than with it off", got, want, keptVolumeAttachWait-boot)
		}
	})
}

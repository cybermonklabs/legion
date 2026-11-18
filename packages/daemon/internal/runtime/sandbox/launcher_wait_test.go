package sandbox

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	const said = "sandbox runtime: replacing a pod older than the boot timeout that its node has not made ready to start its containers"
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

// replacement judges a pod its node never started by one reading of the clock (runsNoRole): the
// reading that replaces such a pod is the one that says no role ran in it, so its answer never
// names that pod's roles displaced. The pod here is the one a launch timed out on, its launcher
// Secrets bound to it and nothing about it drifted, so never having started is the one reason
// replacement could replace it. Past the boot timeout it is replaced, displacing nothing. At the
// boot timeout's edge, with the clock past the edge from the second reading on, its one reading
// keeps it — or, where something else read the clock first, replaces it displacing nothing — and a
// replacement that read the clock twice would judge the pod live by the first reading and replace
// it by the second, naming roles that never ran in it displaced.
func TestReplacementJudgesAPodItsNodeNeverStartedByOneReadingOfTheClock(t *testing.T) {
	boot := testOptions().BootTimeout
	for _, tc := range []struct {
		name string
		// past is how far past the boot timeout after the pod's creation each reading of the
		// clock is, in turn; the last one answers every reading after it.
		past []time.Duration
		// replaced is the pod replaced as never started whatever reading replacement takes; a row
		// at the edge may see it kept.
		replaced bool
	}{
		{name: "past the boot timeout", past: []time.Duration{time.Second}, replaced: true},
		{name: "at the boot timeout's edge, the clock moving past it between readings", past: []time.Duration{0, time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var readings []time.Time
			g := newRig(t, nil, func(g *rig, o *Options) {
				o.Now = func() time.Time {
					mu.Lock()
					defer mu.Unlock()
					if len(readings) == 0 {
						return *g.now.Load()
					}
					at := readings[0]
					if len(readings) > 1 {
						readings = readings[1:]
					}
					return at
				}
			})
			g.autoStart.Store(false)
			spec := workerSpec(t)
			name := SandboxName(spec.Claim)
			result := make(chan error, 1)
			go func() { _, err := g.r.Spawn(g.ctx, spec); result <- err }()
			g.pendingPod(name, notReadyToStart(*g.now.Load()))
			select {
			case err := <-result:
				want := "wait for its role launcher: timed out after " + boot.String()
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("Spawn = %v, want %q, the failure the supervisor charges", err, want)
				}
			case <-time.After(90 * time.Second):
				t.Fatal("hang guard: the launch never ended for a pod that never started")
			}
			s, err := g.r.storedSandbox(name)
			if err != nil || s == nil {
				t.Fatalf("the stored Sandbox %s = %v, %v", name, s, err)
			}
			pod := g.r.storedPod(name)
			spec.Generation = 2
			l, err := g.r.prepare(spec)
			if err != nil {
				t.Fatal(err)
			}
			created := pod.CreationTimestamp.Time
			mu.Lock()
			for _, past := range tc.past {
				readings = append(readings, created.Add(boot+past))
			}
			mu.Unlock()

			decided, err := g.r.replacement(g.ctx, s, pod, &l)
			mu.Lock()
			readings = nil
			mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}

			never := strings.Contains(decided.why, "has not started its containers")
			switch {
			case decided.displacesRoles:
				t.Fatalf("replacement = %+v, want it to name nothing displaced: no role ran in a pod its node never started", decided)
			case decided.why != "" && !never, tc.replaced && !never:
				t.Fatalf("replacement = %+v, want the pod replaced as one its node never started, and for nothing else", decided)
			case never && decided.unstarted <= boot:
				t.Fatalf("replacement = %+v, want the pod replaced for an age past the boot timeout %s", decided, boot)
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

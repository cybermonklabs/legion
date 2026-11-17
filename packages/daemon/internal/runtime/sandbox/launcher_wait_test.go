package sandbox

import (
	"log/slog"
	"slices"
	"strings"
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

// unstartedNode is the node a pod that never started is scheduled on (unstartedSince).
const unstartedNode = "ip-192-0-2-9"

// unstartedSince is a pod's status on a node that has not made it ready to start its containers
// since at, as a node that cannot surface the pod's new disk leaves it (FailedMount): scheduled
// there, PodReadyToStartContainers False, and its init containers waiting.
func unstartedSince(at time.Time) func(*corev1.Pod) {
	return func(p *corev1.Pod) {
		pulling(p)
		p.Spec.NodeName = unstartedNode
		since := metav1.NewTime(at)
		p.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: since},
			{Type: corev1.PodReadyToStartContainers, Status: corev1.ConditionFalse, LastTransitionTime: since},
			{Type: corev1.PodInitialized, Status: corev1.ConditionFalse, Reason: "ContainersNotInitialized", LastTransitionTime: since},
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
// ready to start its containers for longer than the bound: counted from that condition's last
// transition, or from PodScheduled's where the condition carries none. A pod whose node reports no
// such condition is the launcher wait's, as an image pull is; a pod not scheduled is the
// scheduler's; a pod ready to start whose init container still waits is starting; and a pod that
// has left Pending has started, whatever its node reports of it since.
func TestAPodHasNeverStartedOnlyOnceItsNodeLeavesItUnreadyToStartPastTheBound(t *testing.T) {
	now := time.Date(2026, 11, 17, 15, 8, 0, 0, time.UTC)
	bound := 10 * time.Minute
	condition := func(kind corev1.PodConditionType, status corev1.ConditionStatus, age time.Duration) corev1.PodCondition {
		var at metav1.Time
		if age > 0 {
			at = metav1.NewTime(now.Add(-age))
		}
		return corev1.PodCondition{Type: kind, Status: status, LastTransitionTime: at}
	}
	scheduled := condition(corev1.PodScheduled, corev1.ConditionTrue, time.Hour)
	unready := func(age time.Duration) corev1.PodCondition {
		return condition(corev1.PodReadyToStartContainers, corev1.ConditionFalse, age)
	}
	for _, tc := range []struct {
		name       string
		phase      corev1.PodPhase
		conditions []corev1.PodCondition
		// unstarted is how long the pod has not started, as neverStarted counts it: 0 for a pod
		// it does not judge.
		unstarted time.Duration
		never     bool
	}{
		{name: "a pod with no conditions"},
		{name: "a pod not scheduled", conditions: []corev1.PodCondition{condition(corev1.PodScheduled, corev1.ConditionFalse, time.Hour), unready(time.Hour)}},
		{name: "a pod scheduled whose node reports no readiness to start", conditions: []corev1.PodCondition{scheduled}},
		{name: "a pod ready to start whose init container waits", conditions: []corev1.PodCondition{scheduled, condition(corev1.PodReadyToStartContainers, corev1.ConditionTrue, time.Hour)}},
		{name: "a pod not ready to start for less than the bound", conditions: []corev1.PodCondition{scheduled, unready(bound - time.Second)}, unstarted: bound - time.Second},
		{name: "a pod not ready to start for exactly the bound", conditions: []corev1.PodCondition{scheduled, unready(bound)}, unstarted: bound},
		{name: "a pod not ready to start for longer than the bound", conditions: []corev1.PodCondition{scheduled, unready(bound + time.Second)}, unstarted: bound + time.Second, never: true},
		{name: "a pod not ready to start since no moment, scheduled longer than the bound ago", conditions: []corev1.PodCondition{scheduled, unready(0)}, unstarted: time.Hour, never: true},
		{name: "a pod Running whose node has since stopped making it ready", phase: corev1.PodRunning, conditions: []corev1.PodCondition{scheduled, unready(time.Hour)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod := &corev1.Pod{}
			pulling(pod)
			if tc.phase != "" {
				pod.Status.Phase = tc.phase
			}
			pod.Status.Conditions = tc.conditions
			if unstarted, never := neverStarted(pod, now, bound); unstarted != tc.unstarted || never != tc.never {
				t.Fatalf("neverStarted = %s, %t; want %s, %t", unstarted, never, tc.unstarted, tc.never)
			}
		})
	}
}

// A node that cannot surface a pod's new disk leaves the pod scheduled and never ready to start its
// containers, so its launcher never connects and the launch gives up at the boot timeout, with the
// error the supervisor charges as a launch failure. The next try finds the pod not started past
// the boot timeout and replaces it — a new pod, so a new disk — and says so, naming the pod, its
// node and how long it has not started, rather than waiting on the same pod again at every try
// until the claim's launch budget runs out. The replaced pod ran no role, so nothing is named
// displaced from it. A pod not ready to start for less than the boot timeout is kept, as a try
// keeps every pod of its own still coming up.
func TestALaunchReplacesAPodItsNodeNeverStartedOncePastTheBootTimeout(t *testing.T) {
	const said = "sandbox runtime: replacing a pod whose node has not made it ready to start its containers for longer than the boot timeout"
	for _, tc := range []struct {
		name     string
		aged     time.Duration
		replaces bool
	}{
		{name: "not started for longer than the boot timeout", aged: testOptions().BootTimeout + time.Second, replaces: true},
		{name: "not started for less than the boot timeout", aged: testOptions().BootTimeout - time.Second},
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
			g.pendingPod(name, unstartedSince(*g.now.Load()))
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
			for _, want := range []string{said, "pod=" + name, "uid=" + string(stuck), "node=" + unstartedNode, "notStartedFor=" + tc.aged.String()} {
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

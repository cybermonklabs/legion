package sandbox

import (
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// recreatePod has the controller stand-in make the Sandbox name's pod again outside any launch, as
// Agent Sandbox does after an eviction or a node loss: the pod a launch bound goes, and the
// controller creates one from the template the Sandbox stores, under the same name, a new uid and
// the same owner. It returns the new pod's uid once the pod store holds it.
func recreatePod(g *rig, name, bound string) string {
	g.t.Helper()
	if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
		g.t.Fatal(err)
	}
	var recreated string
	g.eventually("the pod store to hold the pod the controller recreated", func() bool {
		pod := g.r.storedPod(name)
		if pod == nil || string(pod.UID) == bound {
			return false
		}
		recreated = string(pod.UID)
		return true
	})
	return recreated
}

// A pod the Agent Sandbox controller recreated outside a launch projects the role Secrets the
// launch bound to the pod before it, so its launchers say hello with each role's current token
// and a pod uid no Secret binds. The first such hello binds every role Secret of the pod to it,
// each once however many of its launchers say hello, and the hello is accepted without waiting
// for a launch. The supervisor's next launch of the role then reads the pod as bound and keeps it,
// rather than replacing a pod whose launchers were refused until that launch came.
func TestAPodTheControllerRecreatedIsBoundAtItsLaunchersHello(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	first := g.spawn(spec)
	name := first.Sandbox.Name
	recreated := recreatePod(g, name, first.Sandbox.PodUID)
	g.clearActions()

	accept := g.r.LauncherResolver()
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleArchitect, claim.RoleTester} {
		if handler, _, reason := accept(rigLauncherHello(g, name, recreated, role)); handler == nil {
			t.Fatalf("the %s launcher of the recreated pod %s was refused: %s", role, recreated, reason)
		}
	}

	var want []string
	for _, role := range claim.Roles {
		secret := roleSecretName(name, role)
		want = append(want, secret)
		if bound := g.secret(secret).Annotations[launcherPodUIDAnnotation]; bound != recreated {
			t.Errorf("%s binds pod %q, want the recreated pod %q", secret, bound, recreated)
		}
	}
	if updated := secretUpdates(g.writes()); !slices.Equal(slices.Sorted(slices.Values(updated)), slices.Sorted(slices.Values(want))) {
		t.Fatalf("the hellos updated %v, want each role Secret of the pod once: %v", updated, want)
	}

	// The rig's fake launcher stands in for the tester's accepted connection, which a launch
	// waits on.
	g.connect(spec.Claim)
	spec.Generation++
	spec.BootToken = fmt.Sprintf("boot-%s-g%d", spec.Claim, spec.Generation)
	resumed := spec
	resumed.ResumeSessionFile = resumeSession
	next, err := g.r.Resume(g.ctx, &first, resumed)
	if err != nil {
		t.Fatalf("the tester's next launch: %v", err)
	}
	if next.Sandbox.PodUID != recreated {
		t.Fatalf("the tester's next launch ran in pod %s, want the recreated pod %s kept", next.Sandbox.PodUID, recreated)
	}
}

// A hello binds only the Sandbox's current pod, and only for a launcher holding its role's current
// token: a pod the Sandbox does not own, a pod being deleted, a pod that ran to its end, and a
// hello whose token is not current are each refused, and no role Secret is written or read for
// them. Each pod stands where the controller stand-in, held, does not replace it.
func TestAHelloBindsNoPodButTheSandboxsCurrentLiveOne(t *testing.T) {
	for label, tc := range map[string]struct {
		owned bool
		edit  func(*corev1.Pod, *string)
	}{
		"a pod the Sandbox does not own": {},
		"a pod being deleted": {owned: true, edit: func(p *corev1.Pod, _ *string) {
			now := metav1.Now()
			p.DeletionTimestamp = &now
		}},
		"a pod that ran to its end":   {owned: true, edit: func(p *corev1.Pod, _ *string) { p.Status.Phase = corev1.PodFailed }},
		"a token that is not current": {owned: true, edit: func(_ *corev1.Pod, token *string) { *token = "not-the-token" }},
	} {
		t.Run(label, func(t *testing.T) {
			g := newRig(t, nil)
			first := g.spawn(workerSpec(t))
			name := first.Sandbox.Name
			g.hold.Store(true)
			if err := g.kube.Tracker().Delete(podsGVR, testNamespace, name); err != nil {
				t.Fatal(err)
			}
			g.eventually("the pod store to lose the bound pod", func() bool { return g.r.storedPod(name) == nil })
			owner := ""
			if tc.owned {
				owner = string(g.sandbox(name).UID)
			}
			const stranger = "uid-pod-unbound"
			pod := podObject(name, stranger, owner, claimLabels(claim.RoleTester), runningStatus())
			hello := rigLauncherHello(g, name, stranger, claim.RoleTester)
			if tc.edit != nil {
				tc.edit(pod, &hello.Token)
			}
			if err := g.kube.Tracker().Add(pod); err != nil {
				t.Fatal(err)
			}
			g.eventually("the pod store to hold the unbound pod", func() bool {
				held := g.r.storedPod(name)
				return held != nil && string(held.UID) == stranger
			})
			g.clearActions()

			if handler, _, reason := g.r.LauncherResolver()(hello); handler != nil || reason == "" {
				t.Fatalf("the hello from %s was accepted (reason %q)", stranger, reason)
			}
			for _, request := range g.requests() {
				if request.resource == "secrets" {
					t.Errorf("the refused hello sent %s", request)
				}
			}
			for _, role := range claim.Roles {
				secret := roleSecretName(name, role)
				if bound := g.secret(secret).Annotations[launcherPodUIDAnnotation]; bound != first.Sandbox.PodUID {
					t.Errorf("%s binds pod %q, want the launch's binding of %q left as it was", secret, bound, first.Sandbox.PodUID)
				}
			}
		})
	}
}

// A launch, a suspension or a release that holds a pod's launch turn decides the pod itself, so a
// hello that would bind the pod meanwhile is refused without waiting for the turn and writes
// nothing; once the turn is given back, the launcher's next hello binds the pod. A hello that
// waited for the turn would wait out its apiTimeout, three times the bound this test gives it.
func TestAHelloBindsNoPodWhoseLaunchTurnIsHeld(t *testing.T) {
	g := newRig(t, nil)
	first := g.spawn(workerSpec(t))
	name := first.Sandbox.Name
	recreated := recreatePod(g, name, first.Sandbox.PodUID)
	release, err := g.r.lockPod(g.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	g.clearActions()
	accept := g.r.LauncherResolver()

	refused := make(chan string, 1)
	go func() {
		handler, _, reason := accept(rigLauncherHello(g, name, recreated, claim.RoleTester))
		if handler != nil {
			reason = ""
		}
		refused <- reason
	}()
	select {
	case reason := <-refused:
		if reason == "" {
			t.Fatal("the hello was accepted while its pod's launch turn was held")
		}
	case <-time.After(apiTimeout / 3):
		t.Fatal("the hello waited for its pod's held launch turn")
	}
	for _, request := range g.requests() {
		if request.resource == "secrets" {
			t.Errorf("the hello refused under the held turn sent %s", request)
		}
	}

	release()
	if handler, _, reason := accept(rigLauncherHello(g, name, recreated, claim.RoleTester)); handler == nil {
		t.Fatalf("the hello after the turn was given back was refused: %s", reason)
	}
	if bound := g.secret(roleSecretName(name, claim.RoleTester)).Annotations[launcherPodUIDAnnotation]; bound != recreated {
		t.Fatalf("the tester's Secret binds pod %q, want the recreated pod %q", bound, recreated)
	}
}

// A binding at a hello that the API server refused is not tried again for that Sandbox's pod
// until credentialRetry has passed, as a failed read of a launcher Secret is not: each launcher of
// the pod says hello again a second after its refusal, and every hello meanwhile is refused
// without a request, so the launchers of a pod whose binding fails cannot turn into a stream of
// Kubernetes requests. Once the retry interval has passed, the next hello binds the pod.
func TestABindingAtAHelloThatFailedWaitsOutTheRetryInterval(t *testing.T) {
	g := newRig(t, nil)
	first := g.spawn(workerSpec(t))
	name := first.Sandbox.Name
	recreated := recreatePod(g, name, first.Sandbox.PodUID)
	var refuse atomic.Bool
	refuse.Store(true)
	g.kube.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		if refuse.Load() {
			return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), name, errors.New("the binding is refused"))
		}
		return false, nil, nil
	})
	secretRequests := func() int {
		n := 0
		for _, request := range g.requests() {
			if request.resource == "secrets" {
				n++
			}
		}
		return n
	}
	g.clearActions()
	accept := g.r.LauncherResolver()

	if handler, _, reason := accept(rigLauncherHello(g, name, recreated, claim.RoleTester)); handler != nil {
		t.Fatalf("the tester's hello was accepted while its pod's binding was refused (reason %q)", reason)
	}
	tried := secretRequests()
	if tried == 0 {
		t.Fatal("the first hello sent no Secret request, so the binding was never tried and this test proves nothing")
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleArchitect} {
		if handler, _, reason := accept(rigLauncherHello(g, name, recreated, role)); handler != nil {
			t.Fatalf("the %s hello was accepted inside the retry interval (reason %q)", role, reason)
		}
	}
	if again := secretRequests(); again != tried {
		t.Fatalf("the hellos inside the retry interval sent %d Secret requests, want none after the failed binding's %d", again-tried, tried)
	}

	refuse.Store(false)
	g.advance(credentialRetry)
	if handler, _, reason := accept(rigLauncherHello(g, name, recreated, claim.RoleTester)); handler == nil {
		t.Fatalf("the tester's hello after the retry interval was refused: %s", reason)
	}
	for _, role := range claim.Roles {
		secret := roleSecretName(name, role)
		if bound := g.secret(secret).Annotations[launcherPodUIDAnnotation]; bound != recreated {
			t.Errorf("%s binds pod %q, want the recreated pod %q", secret, bound, recreated)
		}
	}
}

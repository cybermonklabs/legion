package sandbox

import (
	"context"
	"crypto/sha256"
	"errors"
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
	"github.com/sjawhar/legion/daemon/internal/shimwire"
)

// secretRequests are the requests of g's runtime on Secrets, reads included, in order.
func secretRequests(g *rig) []action {
	var secrets []action
	for _, request := range g.requests() {
		if request.resource == "secrets" {
			secrets = append(secrets, request)
		}
	}
	return secrets
}

// A pod the Agent Sandbox controller recreated outside a launch projects the role Secrets the
// launch bound to the pod before it, so its launchers say hello with each role's current token
// and a pod uid no Secret binds. The first such hello binds every role Secret of the pod to it,
// each once however many of its launchers say hello, and the hello is accepted without waiting
// for a launch. The supervisor's next launch of the role then reads the pod as bound and keeps it,
// starting the role on the launcher the binding accepted, rather than replacing a pod whose
// launchers were refused until that launch came.
func TestAPodTheControllerRecreatedIsBoundAtItsLaunchersHello(t *testing.T) {
	g := newRig(t, nil)
	spec := workerSpec(t)
	first := g.spawn(spec)
	name := first.Sandbox.Name
	recreated := g.recreatePod(name, first.Sandbox.PodUID)
	g.clearActions()

	accept := g.r.LauncherResolver()
	for _, role := range []claim.Role{claim.RoleArchitect, claim.RoleReviewer, claim.RoleArchitect} {
		if handler, _, reason := accept(rigLauncherHello(g, name, recreated, role)); handler == nil {
			t.Fatalf("the %s launcher of the recreated pod %s was refused: %s", role, recreated, reason)
		}
	}
	tester := redial(t, g, spec.Claim, 0, false)

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

	next := g.resumeNext(first, &spec)
	if next.Sandbox.PodUID != recreated {
		t.Fatalf("the tester's next launch ran in pod %s, want the recreated pod %s kept", next.Sandbox.PodUID, recreated)
	}
	if got := tester.commands(); got != "start 2" {
		t.Fatalf("the tester's launcher the binding accepted was sent %q, want generation 2 started", got)
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
			if requests := secretRequests(g); len(requests) > 0 {
				t.Errorf("the refused hello sent %v", requests)
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

// A binding at a hello checks the hello's token against its role's Secret as the API server holds
// it, not against the credential the runtime holds, which a launch that rewrote the Secret's token
// and then failed to bind its pod leaves behind (writeLauncherSecrets, bindLauncherSecrets). A
// hello with the token the runtime still holds, which the Secret no longer does, is refused and
// writes no Secret; the read it made replaces the held credential, so its next hello is refused
// from memory, and the hello with the token the Secret holds binds the pod.
func TestABindingAtAHelloChecksTheTokenItsSecretHoldsNow(t *testing.T) {
	g := newRig(t, nil)
	first := g.spawn(workerSpec(t))
	name := first.Sandbox.Name
	recreated := g.recreatePod(name, first.Sandbox.PodUID)
	secret := roleSecretName(name, claim.RoleTester)
	stale := rigLauncherHello(g, name, recreated, claim.RoleTester)
	editSecret(g, secret, func(s *corev1.Secret) { s.Data[LauncherTokenFile] = []byte("launcher-token-written-since") })
	if held, ok := g.r.launcherAuth.lookup(secret, g.sandbox(name).UID); !ok || held.hash != sha256.Sum256([]byte(stale.Token)) {
		t.Fatal("the runtime does not hold the token the Secret held before, so this test proves nothing")
	}
	g.clearActions()
	accept := g.r.LauncherResolver()

	if handler, _, reason := accept(stale); handler != nil {
		t.Fatalf("the hello with the token the Secret no longer holds was accepted (reason %q)", reason)
	}
	if updated := secretUpdates(g.writes()); len(updated) > 0 {
		t.Fatalf("the refused hello updated %v, want no Secret written", updated)
	}
	for _, role := range claim.Roles {
		if bound := g.secret(roleSecretName(name, role)).Annotations[launcherPodUIDAnnotation]; bound != first.Sandbox.PodUID {
			t.Errorf("%s binds pod %q, want the launch's binding of %q left as it was", roleSecretName(name, role), bound, first.Sandbox.PodUID)
		}
	}

	g.clearActions()
	if handler, _, reason := accept(stale); handler != nil {
		t.Fatalf("the stale token's next hello was accepted (reason %q)", reason)
	}
	if requests := g.requests(); len(requests) > 0 {
		t.Fatalf("the stale token's next hello sent %v, want it refused from memory", requests)
	}
	if handler, _, reason := accept(rigLauncherHello(g, name, recreated, claim.RoleTester)); handler == nil {
		t.Fatalf("the hello with the token the Secret holds was refused: %s", reason)
	}
	if bound := g.secret(secret).Annotations[launcherPodUIDAnnotation]; bound != recreated {
		t.Fatalf("%s binds pod %q, want the recreated pod %q", secret, bound, recreated)
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
	recreated := g.recreatePod(name, first.Sandbox.PodUID)
	release, err := g.r.lockPod(g.ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	g.clearActions()
	accept := g.r.LauncherResolver()

	refused := helloAsync(g, rigLauncherHello(g, name, recreated, claim.RoleTester))
	select {
	case reason := <-refused:
		if reason == "" {
			t.Fatal("the hello was accepted while its pod's launch turn was held")
		}
	case <-time.After(apiTimeout / 3):
		t.Fatal("the hello waited for its pod's held launch turn")
	}
	if requests := secretRequests(g); len(requests) > 0 {
		t.Errorf("the hello refused under the held turn sent %v", requests)
	}

	release()
	if handler, _, reason := accept(rigLauncherHello(g, name, recreated, claim.RoleTester)); handler == nil {
		t.Fatalf("the hello after the turn was given back was refused: %s", reason)
	}
	if bound := g.secret(roleSecretName(name, claim.RoleTester)).Annotations[launcherPodUIDAnnotation]; bound != recreated {
		t.Fatalf("the tester's Secret binds pod %q, want the recreated pod %q", bound, recreated)
	}
}

// blockingLimiter is a client-side rate limiter that lets every request go until it is blocked,
// and then holds each request until the request's context ends or the limiter is released, as a
// limiter whose queue outlasts any caller's deadline does. waiting is told of the first request it
// holds.
type blockingLimiter struct {
	blocked  atomic.Bool
	waiting  chan struct{}
	released chan struct{}
}

func newBlockingLimiter() *blockingLimiter {
	return &blockingLimiter{waiting: make(chan struct{}, 1), released: make(chan struct{})}
}

// release lets every request it holds go, and every later one.
func (l *blockingLimiter) release() {
	l.blocked.Store(false)
	close(l.released)
}

func (l *blockingLimiter) Wait(ctx context.Context) error {
	if !l.blocked.Load() {
		return nil
	}
	select {
	case l.waiting <- struct{}{}:
	default:
	}
	select {
	case <-l.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *blockingLimiter) Accept()         {}
func (l *blockingLimiter) TryAccept() bool { return !l.blocked.Load() }
func (l *blockingLimiter) Stop()           {}
func (l *blockingLimiter) QPS() float32    { return 50 }

// helloAsync says hello through g's resolver in a goroutine and answers the channel its refusal's
// reason arrives on, "" for a hello accepted.
func helloAsync(g *rig, hello shimwire.LauncherHello) <-chan string {
	refused := make(chan string, 1)
	go func() {
		handler, _, reason := g.r.LauncherResolver()(hello)
		if handler != nil {
			reason = ""
		}
		refused <- reason
	}()
	return refused
}

// A binding at a hello checks the token of the hello that began it, so a hello of another role or
// with another token never joins it: it begins a binding of its own, which finds the pod's launch
// turn held by the one in flight and is refused at once rather than waiting on a binding that
// checked another token. That launcher's next hello finds the pod the first binding bound. The
// first binding is held in flight on the client's rate limiter.
func TestAHelloNeverJoinsABindingAnotherTokenBegan(t *testing.T) {
	const guard = 10 * time.Second
	limiter := newBlockingLimiter()
	g := newRig(t, nil, withClientLimiter(limiter))
	first := g.spawn(workerSpec(t))
	name := first.Sandbox.Name
	recreated := g.recreatePod(name, first.Sandbox.PodUID)
	limiter.blocked.Store(true)

	tester := helloAsync(g, rigLauncherHello(g, name, recreated, claim.RoleTester))
	select {
	case <-limiter.waiting:
	case <-time.After(guard):
		t.Fatal("the tester's binding sent no request the limiter held")
	}
	select {
	case reason := <-helloAsync(g, rigLauncherHello(g, name, recreated, claim.RoleArchitect)):
		if reason == "" {
			t.Fatal("the architect's hello was accepted while the tester's binding was in flight")
		}
	case <-time.After(guard):
		t.Fatal("the architect's hello waited on the binding the tester's hello began, which checked the tester's token alone")
	}

	limiter.release()
	select {
	case reason := <-tester:
		if reason != "" {
			t.Fatalf("the tester's hello was refused once its binding went on: %s", reason)
		}
	case <-time.After(guard):
		t.Fatal("the tester's hello was not answered once its binding went on")
	}
	if handler, _, reason := g.r.LauncherResolver()(rigLauncherHello(g, name, recreated, claim.RoleArchitect)); handler == nil {
		t.Fatalf("the architect's next hello was refused after the tester's binding bound the pod: %s", reason)
	}
}

// A binding at a hello holds its pod's launch turn no longer than its deadline (helloBindBound):
// its requests waiting on a client rate limiter that would hold them past it are cancelled there,
// the turn is given back, and the hello is refused, so a launch waiting for the turn waits at most
// that long. The rig's bound is a fraction of the hang guard here; the deployment's is apiTimeout.
func TestABindingAtAHelloGivesItsPodsTurnBackAtItsDeadline(t *testing.T) {
	limiter := newBlockingLimiter()
	g := newRig(t, nil, withClientLimiter(limiter))
	first := g.spawn(workerSpec(t))
	name := first.Sandbox.Name
	recreated := g.recreatePod(name, first.Sandbox.PodUID)
	const bound, guard = 200 * time.Millisecond, 10 * time.Second
	g.r.helloBindBound = bound
	limiter.blocked.Store(true)

	refused := helloAsync(g, rigLauncherHello(g, name, recreated, claim.RoleTester))
	select {
	case <-limiter.waiting:
	case <-time.After(guard):
		t.Fatal("the hello's binding sent no request the limiter held")
	}
	if release, free := g.r.tryLockPod(name); free {
		release()
		t.Fatal("the pod's launch turn was free while its binding's request waited, so this test proves nothing")
	}

	taking, cancel := context.WithTimeout(g.ctx, guard)
	defer cancel()
	release, err := g.r.lockPod(taking, name)
	if err != nil {
		t.Fatalf("the pod's launch turn was still held %s into its binding, whose bound is %s: %v", guard, bound, err)
	}
	release()
	select {
	case reason := <-refused:
		if reason == "" {
			t.Fatal("the hello whose binding ran out of its bound was accepted")
		}
	case <-time.After(guard):
		t.Fatal("the hello whose binding ran out of its bound was not answered")
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
	recreated := g.recreatePod(name, first.Sandbox.PodUID)
	var refuse atomic.Bool
	refuse.Store(true)
	g.kube.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		if refuse.Load() {
			return true, nil, apierrors.NewForbidden(corev1.Resource("secrets"), name, errors.New("the binding is refused"))
		}
		return false, nil, nil
	})
	g.clearActions()
	accept := g.r.LauncherResolver()

	if handler, _, reason := accept(rigLauncherHello(g, name, recreated, claim.RoleTester)); handler != nil {
		t.Fatalf("the tester's hello was accepted while its pod's binding was refused (reason %q)", reason)
	}
	tried := len(secretRequests(g))
	if tried == 0 {
		t.Fatal("the first hello sent no Secret request, so the binding was never tried and this test proves nothing")
	}
	for _, role := range []claim.Role{claim.RoleTester, claim.RoleArchitect} {
		if handler, _, reason := accept(rigLauncherHello(g, name, recreated, role)); handler != nil {
			t.Fatalf("the %s hello was accepted inside the retry interval (reason %q)", role, reason)
		}
	}
	if again := len(secretRequests(g)); again != tried {
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

package sandbox

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/stream"
)

type launcherCredential struct {
	podUID, issue string
	hash          [sha256.Size]byte
}

// launcherCredentials are the launcher Secrets the runtime holds, by name and then by the UID of
// the Sandbox each was read or bound for, so a Sandbox's entry is never another's of the same
// name; the reads of them, one flight per Secret of a Sandbox (launcherCredential); and the
// bindings of a pod's at a hello, one flight per pod, role and token, serialized by the pod's
// launch turn (bindAtHello).
type launcherCredentials struct {
	mu      sync.Mutex
	secrets map[string]map[types.UID]launcherCredential
	reads   heldOffCalls
	binds   heldOffCalls
}

// credentialRetry is how long after a launcher Secret's read, or a binding of a pod's launcher
// Secrets at a hello, failed a hello needing it is refused without another (heldOffCalls). A
// launcher says hello again a second after a refusal, so a Secret the API does not serve costs one
// read in ten hellos.
const credentialRetry = 10 * time.Second

// heldOffCalls are calls a hello needs, single-flight, each outliving the hello that began it: the
// reads of a Sandbox's launcher Secrets (launcherCredential) and the bindings of its pods at a hello
// (bindAtHello). A hello waits for the call it began or joined until its context ends and is then
// refused, so the launcher's next hello, a second later, joins the same call or finds what it
// left, never starting another. Each call is held under a key, and a key whose last call failed is
// refused without a call until credentialRetry has passed, so launchers whose calls fail cost one
// call each credentialRetry however often they say hello. A call that declined to act
// (errDeclined) failed at nothing, and changes no hold-off.
type heldOffCalls struct {
	mu     sync.Mutex
	failed map[string]time.Time
	calls  singleflight.Group
}

// do is the result of the call flight names, held under key: the one in flight joined, or call
// begun, unless key's last call failed less than credentialRetry before now(), which is refused at
// once. Calls under one key that flight names apart run side by side, as their own work decides.
// It waits until ctx ends.
func (c *heldOffCalls) do(ctx context.Context, now func() time.Time, key, flight string, call func() (any, error)) (any, error) {
	if from := c.heldOffUntil(key); now().Before(from) {
		return nil, fmt.Errorf("its last try failed; it is tried again from %s", from.UTC().Format(time.RFC3339))
	}
	result := c.calls.DoChan(flight, func() (any, error) {
		value, err := call()
		c.settle(key, now(), err)
		return value, err
	})
	select {
	case done := <-result:
		return done.Val, done.Err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// heldOffUntil is when key's next call may begin: credentialRetry after its last call failed, the
// zero time when none did.
func (c *heldOffCalls) heldOffUntil(key string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	failed, ok := c.failed[key]
	if !ok {
		return time.Time{}
	}
	return failed.Add(credentialRetry)
}

// settle records how a call held under key ended at at: a failure holds key off, a call that did
// what it was for lifts what an earlier one held, and one that declined changes nothing. A failure
// drops every other failure whose hold-off is over, so the keys of Sandboxes since deleted are not
// kept.
func (c *heldOffCalls) settle(key string, at time.Time, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case errors.Is(err, errDeclined):
		return
	case err == nil:
		delete(c.failed, key)
		return
	}
	for held, failed := range c.failed {
		if !at.Before(failed.Add(credentialRetry)) {
			delete(c.failed, held)
		}
	}
	if c.failed == nil {
		c.failed = make(map[string]time.Time)
	}
	c.failed[key] = at
}

// errDeclined is a call of heldOffCalls that decided not to act rather than failed. A binding at a
// hello declines a pod that is not bindable, a token its role's Secret does not hold, and a pod whose
// launch turn something else holds; none of them holds the pod's next binding off.
var errDeclined = errors.New("declined")

var (
	errNotBindable     = fmt.Errorf("the pod is not its Sandbox's current live pod: %w", errDeclined)
	errTokenNotCurrent = fmt.Errorf("the token is not the one its role's Secret holds: %w", errDeclined)
	errTurnHeld        = fmt.Errorf("its pod's launch turn is held: %w", errDeclined)
)

// LauncherResolver rejects unknown pods from informer state before consulting credentials. An
// issue pod's launcher must name a configured role or one in the pod's template. A role removed
// from the deployment can still authenticate to let a pending stop reach its child. The project
// controller pod accepts only the controller. The pod kind derives the claim (podKind.claimToken).
// Tokens minted here are cached as hashes. After a restart, a known role's Secret is read once
// however often its launcher says hello (launcherCredential), so unauthenticated hellos cannot
// turn into an unbounded stream of Kubernetes requests. A hello waits for that read up to
// apiTimeout.
//
// A launcher holding its role's current token from the Sandbox's current pod, which no Secret
// binds, is of a pod no launch made: the Agent Sandbox controller recreated it after an eviction or
// a node loss. Its pod is bound at that hello (bindAtHello) once the role's Secret, read afresh,
// holds the hello's token, which the hello waits for up to the same apiTimeout, and the launcher
// is accepted once the binding names its pod, so its roles need not wait for the supervisor's next
// launch, nor that launch replace the pod once more.
//
// An accepted launcher is answered with the acknowledgement launcherHelloAck builds for its pod's
// kind, which the listener writes whole; every refusal answers the zero acknowledgement, which
// the listener never writes.
func (r *Runtime) LauncherResolver() stream.LauncherResolver {
	return func(hello shimwire.LauncherHello) (stream.LauncherHandler, shimwire.LauncherHelloAck, string) {
		role := claim.Role(hello.Role)
		view, err := r.view(hello.Sandbox)
		if err != nil || view.sandbox == nil || view.pod == nil || string(view.pod.UID) != hello.PodUID {
			return nil, shimwire.LauncherHelloAck{}, "launcher pod is not the current controller-owned pod"
		}
		kind, err := podKindOf(view.sandbox.Labels)
		if err != nil {
			return nil, shimwire.LauncherHelloAck{}, "launcher role is not one of its pod's roles"
		}
		if _, issue := kind.(issuePod); !slices.Contains(kind.roles(r), role) &&
			(!issue || !slices.Contains(templateRoles(view.sandbox), role)) {
			return nil, shimwire.LauncherHelloAck{}, "launcher role is not one of its pod's roles"
		}
		ctx, cancel := context.WithTimeout(context.Background(), apiTimeout)
		defer cancel()
		credential, err := r.launcherCredential(ctx, view.sandbox, role)
		if err != nil {
			return nil, shimwire.LauncherHelloAck{}, "launcher credential is unavailable"
		}
		hash := sha256.Sum256([]byte(hello.Token))
		if subtle.ConstantTimeCompare(credential.hash[:], hash[:]) != 1 {
			return nil, shimwire.LauncherHelloAck{}, "launcher token is not current"
		}
		if credential.podUID != hello.PodUID {
			switch err := r.bindAtHello(ctx, view.sandbox, view.pod, role, hash); {
			case errors.Is(err, errTokenNotCurrent):
				return nil, shimwire.LauncherHelloAck{}, "launcher token is not current"
			case errors.Is(err, errDeclined):
				return nil, shimwire.LauncherHelloAck{}, "launcher pod UID is not bound to its Secret epoch"
			case err != nil:
				return nil, shimwire.LauncherHelloAck{}, "launcher pod could not be bound to its Secret epoch"
			}
			// The binding holds the credentials it read and wrote, so this is answered from
			// memory, and the hello is checked again whole against them: its token and its pod.
			credential, err = r.launcherCredential(ctx, view.sandbox, role)
			if err != nil || subtle.ConstantTimeCompare(credential.hash[:], hash[:]) != 1 || credential.podUID != hello.PodUID {
				return nil, shimwire.LauncherHelloAck{}, "launcher pod UID is not bound to its Secret epoch"
			}
		}
		// Options.Project is the legion.dev/project label's value (its own doc comment), not
		// necessarily already a claim.ProjectToken: a deployment's own project name may carry
		// characters ProjectToken drops (its doc: "cybermonklabs/legion" names the same role topics
		// under either daemon), and go through it before naming a claim.
		project, err := claim.ProjectToken(r.project)
		if err != nil {
			return nil, shimwire.LauncherHelloAck{}, "launcher credential does not name this pod's role"
		}
		token, err := kind.claimToken(project, credential.issue, claim.Agent(role))
		if err != nil || SandboxName(token) != hello.Sandbox {
			return nil, shimwire.LauncherHelloAck{}, "launcher credential does not name this pod's role"
		}
		// The pod the connection is accepted for carries the instant the API server created its
		// Sandbox, where that Sandbox owns the issue's kept volume, which each role's start in the
		// pod records with the pod (launchers.recordKeptPod). It comes from the informer's Sandbox,
		// so it costs no request.
		return r.launchers.accept(token, credential.issue, keptVolumeSince(view.sandbox), hello), r.launcherHelloAck(kind), ""
	}
}

// keptVolumeSince is when the API server created s, where s owns an issue's kept volume
// (ownsKeptVolume), and the zero time otherwise: a Sandbox whose pod's volume is the pod's own,
// and the project controller's Sandbox. A kept volume is as old as the Sandbox that owns it,
// since Agent Sandbox makes the claim from the Sandbox's template, and the claim goes only with
// the Sandbox.
func keptVolumeSince(s *sandbox) time.Time {
	if !ownsKeptVolume(s) {
		return time.Time{}
	}
	return s.CreationTimestamp.Time
}

// launcherHelloAck is the acknowledgement an accepted launcher on a pod of kind is answered with,
// the one place each of its members is decided. Each pod kind's launcher is handed the memory
// guard's settings of its own kind and never the other's: an issue pod's launcher those of
// runtime.kubernetes.memory_guard.issue_pods (Options.MemoryGuard), the controller pod's launcher
// those of runtime.kubernetes.memory_guard.controller_pod (Options.ControllerMemoryGuard), in
// either of its forms. A kind whose block turned the guard off is handed none. An issue pod's
// launcher is also handed what its cache trimmer runs by (Options.VolumeTrim), none when the
// deployment configured none; the controller's pod is handed none of it, the volume it works on
// being its Sandbox's own and no issue's cache. Its launcher runs no trimmer in its single-role
// form. Where the session database names a writer audience, its launcher runs the roles-dir form
// and builds a trimmer, which waits for settings that never come; the git lock sweep there finds
// no clone, since the pod's working directory is no jj workspace and no start of the controller
// names one. An absent member is what a launcher reads as "run none". Every launcher of either
// kind is also told that this daemon knows the pod's kernel's kill (KernelKills), whatever the
// guard's settings, since a microVM pod's launcher sends those reports only where it is told so
// and its roles are owed the note however the guard is set. The controller's launcher is answered
// both, its guard's settings and KernelKills, whatever the project's sandbox is now, since its pod
// may be one an earlier configuration built, and the launcher, not this answer, knows which
// sandbox it runs in: a gVisor controller pod's launcher reads the guard's settings and ignores
// KernelKills, and a microVM controller pod's launcher ignores the guard's settings and sends its
// reports where KernelKills came, as a microVM issue pod's does (the launcher's runsGuest). So a
// gVisor controller pod re-adopted after a change of runtime.kubernetes.sandbox.type, which runs
// on until its next launch replaces it, keeps its guard.
func (r *Runtime) launcherHelloAck(kind podKind) shimwire.LauncherHelloAck {
	ack := shimwire.LauncherHelloAck{KernelKills: true}
	switch kind.(type) {
	case issuePod:
		ack.MemoryGuard = r.memoryGuard
		ack.VolumeTrim = r.volumeTrim
	case controllerPod:
		ack.MemoryGuard = r.controllerMemoryGuard
	}
	return ack
}

// launcherCredential is the credential of role's launcher in s: the one held, or one read of the
// role's Secret, single-flight per Secret of a Sandbox and held off for credentialRetry after it
// failed (heldOffCalls). The read outlives the hello that began it: it waits its turn on the
// client's rate limiter for as long as that takes (readLauncherCredential), while each hello waits
// for it until ctx ends and is then refused.
func (r *Runtime) launcherCredential(ctx context.Context, s *sandbox, role claim.Role) (launcherCredential, error) {
	name := roleSecretName(s.Name, role)
	if credential, held := r.launcherAuth.lookup(name, s.UID); held {
		return credential, nil
	}
	key := credentialKey(name, s.UID)
	read, err := r.launcherAuth.reads.do(ctx, r.now, key, key, func() (any, error) { return r.readLauncherCredential(s, name) })
	if err != nil {
		return launcherCredential{}, fmt.Errorf("launcher Secret %s: %w", name, err)
	}
	return read.(launcherCredential), nil
}

// credentialKey is what the read of the launcher Secret name of the Sandbox with uid is held under
// (heldOffCalls): the name alone would have a deleted Sandbox's failed read hold off its same-name
// successor's.
func credentialKey(name string, uid types.UID) string { return name + "/" + string(uid) }

// readLauncherCredential reads the launcher Secret name of s and stores its credential, or logs
// that the read failed. It runs for the runtime's lifetime rather than any one hello's, so its wait
// on the client's rate limiter is bounded by nothing but its turn; the request itself is bounded by
// apiTimeout (requestConfig).
func (r *Runtime) readLauncherCredential(s *sandbox, name string) (launcherCredential, error) {
	if credential, held := r.launcherAuth.lookup(name, s.UID); held {
		return credential, nil
	}
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(r.lifetime, name, metav1.GetOptions{})
	credential := launcherCredential{}
	if err == nil {
		credential, err = credentialOf(secret, s)
	}
	if err != nil {
		r.log.Warn("sandbox runtime: a launcher Secret could not be read; its launcher's hellos are refused until it is read again",
			"secret", name, "sandbox", s.Name, "readAgainAfter", credentialRetry, "err", err)
		return launcherCredential{}, err
	}
	return r.launcherAuth.store(name, s.UID, credential), nil
}

// credentialOf is the credential secret holds as a launcher Secret of s: one s owns, holding a
// launcher token.
func credentialOf(secret *corev1.Secret, s *sandbox) (launcherCredential, error) {
	if !ownedBySandbox(secret.OwnerReferences, s.UID) || len(secret.Data[LauncherTokenFile]) == 0 {
		return launcherCredential{}, fmt.Errorf("launcher Secret %s is not a credential of Sandbox %s", secret.Name, s.Name)
	}
	return credentialFromSecret(secret), nil
}

func credentialFromSecret(secret *corev1.Secret) launcherCredential {
	return launcherCredential{podUID: secret.Annotations[launcherPodUIDAnnotation],
		issue: secret.Annotations[annotationIssue], hash: sha256.Sum256(secret.Data[LauncherTokenFile])}
}

// bindable reports whether pod, the pod s owns in the pod store (view), is one a hello may bind its
// launchers to: s is not Suspended, and pod is neither being deleted nor ended. A pod of a
// Suspended Sandbox is on its way out, as such a pod is.
func bindable(s *sandbox, pod *corev1.Pod) bool {
	return s.mode() != modeSuspended && pod.DeletionTimestamp == nil && !terminal(pod)
}

// bindAtHello binds the launcher Secrets of pod, the pod s owns in the pod store, to pod where no
// launch did, for a hello of role's launcher whose token hashes to hash. After an eviction or a
// node loss the Agent Sandbox controller makes a Running Sandbox's pod again from the template the
// Sandbox stores, under a new uid, and that pod projects the Secrets a launch bound to the pod
// before it: its launchers hold each role's current token and name a pod no Secret binds. A pod
// that is not bindable is declined without a request (errNotBindable).
//
// The binding is a heldOffCalls call, one flight per pod, role and token, serialized by the pod's
// launch turn: a launcher's hellos join the flight its first began, and each flight checks its own
// hello's token against the role's Secret before it writes anything (bindCurrentPod), so no hello
// joins a flight another role's or token's check began. Flights of one pod run one at a time under
// its launch turn: the first writes every Secret of the pod not yet bound to it, one that overlaps
// it finds the turn held and is declined, and that launcher's next hello, a second later, finds
// the pod bound. A binding that failed holds the pod off for credentialRetry, whatever role or
// token says hello next.
func (r *Runtime) bindAtHello(ctx context.Context, s *sandbox, pod *corev1.Pod, role claim.Role, hash [sha256.Size]byte) error {
	if !bindable(s, pod) {
		return errNotBindable
	}
	key := string(s.UID) + "/" + string(pod.UID)
	_, err := r.launcherAuth.binds.do(ctx, r.now, key, fmt.Sprintf("%s/%s/%x", key, role, hash), func() (any, error) {
		return nil, r.bindCurrentPod(s.Name, s.UID, pod.UID, role, hash)
	})
	return err
}

// bindCurrentPod binds the launcher Secrets of the pod with uid podUID to it, for the hello of
// role's launcher whose token hashes to hash, where that pod is still the bindable pod of the
// Sandbox named name with uid sandboxUID once the pod's launch turn is held.
//
// The turn is taken only where nothing holds it: a launch, a suspension or a release that holds it
// decides the pod itself, so the binding is declined at once rather than waiting the turn out
// (errTurnHeld), and the launcher's next hello finds what that left. Under the turn, role's Secret
// is read afresh, and the binding is declined, having written nothing, unless it holds the hello's
// token (errTokenNotCurrent): the credential the runtime held may be one a launch left behind when
// it rewrote the Secret's token and then failed to bind its pod (writeLauncherSecrets,
// bindLauncherSecrets). The read is held in its place, so that token's next hello is refused from
// memory. The binding then writes the Secret of each role the pod was built with (templateRoles)
// that no held credential binds to it yet, and holds what it wrote (bindLauncherSecrets), so the
// next hello is accepted without a request.
//
// Its requests share one deadline, helloBindBound after it took the turn: apiTimeout, the longest
// a hello waits for a binding (LauncherResolver). A binding still running past it would keep the
// pod's launch turn, and every launch waiting for that turn, for a hello that has already been
// refused, and its requests, each bounded by apiTimeout once the client's rate limiter lets it go
// (requestConfig), could keep the turn for minutes between them. At the deadline its requests are
// cancelled and the turn is given back; each Secret it bound by then is held, so the pod's next
// binding writes only the rest.
func (r *Runtime) bindCurrentPod(name string, sandboxUID, podUID types.UID, role claim.Role, hash [sha256.Size]byte) error {
	release, free := r.tryLockPod(name)
	if !free {
		return errTurnHeld
	}
	defer release()
	ctx, cancel := context.WithTimeout(r.lifetime, r.helloBindBound)
	defer cancel()
	failed := func(err error) error {
		r.log.Warn("sandbox runtime: binding the role launchers of a pod made outside a launch failed; its launchers' hellos are refused until a binding lands",
			"sandbox", name, "pod", podUID, "bindAgainAfter", credentialRetry, "err", err)
		return err
	}
	v, err := r.view(name)
	if err != nil {
		return failed(fmt.Errorf("read sandbox %s and its pod from the stores: %w", name, err))
	}
	if v.sandbox == nil || v.sandbox.UID != sandboxUID || v.pod == nil || v.pod.UID != podUID || !bindable(v.sandbox, v.pod) {
		return errNotBindable
	}
	if _, err := podKindOf(v.sandbox.Labels); err != nil {
		return failed(fmt.Errorf("sandbox %s's pod: %w", name, err))
	}
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(ctx, roleSecretName(name, role), metav1.GetOptions{})
	if err != nil {
		return failed(fmt.Errorf("read launcher secret %s: %w", roleSecretName(name, role), err))
	}
	current, err := credentialOf(secret, v.sandbox)
	if err != nil {
		return failed(err)
	}
	r.cacheLauncherCredential(v.sandbox, secret)
	if subtle.ConstantTimeCompare(current.hash[:], hash[:]) != 1 {
		return errTokenNotCurrent
	}
	// The roles the pod was built with (templateRoles), the roles launcherBound holds a pod to: a
	// pod recreated from a template built before a role of the deployment has no Secret of that
	// role, and a binding that asked for one would fail at every hello of the pod.
	var unbound []claim.Role
	for _, each := range templateRoles(v.sandbox) {
		if credential, held := r.launcherAuth.lookup(roleSecretName(name, each), sandboxUID); !held || credential.podUID != string(podUID) {
			unbound = append(unbound, each)
		}
	}
	if len(unbound) == 0 {
		return nil
	}
	if err := r.bindLauncherSecrets(ctx, v.sandbox, unbound, string(podUID)); err != nil {
		return failed(err)
	}
	r.log.Info("sandbox runtime: bound the role launchers of a pod made outside a launch to that pod",
		"sandbox", name, "pod", podUID, "roles", unbound)
	return nil
}

// lookup is the credential held as name's for the Sandbox with uid, if one is (held).
func (c *launcherCredentials) lookup(name string, uid types.UID) (credential launcherCredential, held bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	credential, held = c.secrets[name][uid]
	return credential, held
}

// bind holds credential, a binding write's or a read made under the pod's launch turn, as name's
// for the Sandbox with uid, in place of whatever was held. Both are made under that Sandbox's pod's
// launch turn — a launch's binding or its binding check (launcherBound), or a hello's binding of
// its current pod (bindAtHello) — which proves it holds the name now, so every other Sandbox's
// entry of the name, a deleted one's, goes.
func (c *launcherCredentials) bind(name string, uid types.UID, credential launcherCredential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		c.secrets = make(map[string]map[types.UID]launcherCredential)
	}
	c.secrets[name] = map[types.UID]launcherCredential{uid: credential}
}

// store holds credential, a read's, as name's for the Sandbox with uid, unless that Sandbox holds
// one already: a binding that completed during the read is newer than it. It is the credential the
// Sandbox holds.
func (c *launcherCredentials) store(name string, uid types.UID, credential launcherCredential) launcherCredential {
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.secrets[name][uid]; ok {
		return held
	}
	if c.secrets == nil {
		c.secrets = make(map[string]map[types.UID]launcherCredential)
	}
	if c.secrets[name] == nil {
		c.secrets[name] = make(map[types.UID]launcherCredential)
	}
	c.secrets[name][uid] = credential
	return credential
}

// forget drops whatever is held of name, for every Sandbox. A read that lands afterwards for a
// Sandbox since deleted writes only that Sandbox's entry, which nothing looks up again, until the
// name's next bind or forget drops it.
func (c *launcherCredentials) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.secrets, name)
}

// cacheLauncherCredential holds secret's credential, read under its pod's launch turn or written as
// bound to its pod, for the Sandbox s (launcherCredentials.bind).
func (r *Runtime) cacheLauncherCredential(s *sandbox, secret *corev1.Secret) {
	r.launcherAuth.bind(secret.Name, s.UID, credentialFromSecret(secret))
}

// forgetLauncherCredentials drops what is held of every launcher role's Secret of the Sandbox named
// name (launcherRoles: a role the deployment defines among them), once that Sandbox is deleted:
// whichever roles its pod ran, no other Sandbox's Secret has a name of its.
func (r *Runtime) forgetLauncherCredentials(name string) {
	for _, role := range r.launcherRoles() {
		r.launcherAuth.forget(roleSecretName(name, role))
	}
}

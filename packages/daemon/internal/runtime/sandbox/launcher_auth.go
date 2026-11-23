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

// launcherSecret is what the runtime holds of one role's launcher Secret for one Sandbox: the
// credential a read or a binding of the Secret gave it (held), or else when its latest read failed;
// and when a binding of it at a hello (bindAtHello) last failed, which a binding that lands clears.
type launcherSecret struct {
	credential   launcherCredential
	held         bool
	failedAt     time.Time
	bindFailedAt time.Time
}

// launcherCredentials are the launcher Secrets the runtime holds, by name and then by the UID of
// the Sandbox each was read or bound for, so a Sandbox's entry is never another's of the same
// name; the reads of them in flight, one per Secret of a Sandbox (launcherCredential); and the
// bindings at a hello in flight, one per Sandbox (bindAtHello).
type launcherCredentials struct {
	mu      sync.Mutex
	secrets map[string]map[types.UID]launcherSecret
	reads   singleflight.Group
	binds   singleflight.Group
}

// credentialRetry is how long after a launcher Secret's read failed a hello needing it is refused
// without another read. A launcher says hello again a second after a refusal, so a Secret the API
// does not serve costs one read in ten hellos.
const credentialRetry = 10 * time.Second

// LauncherResolver rejects unknown pods from informer state before consulting credentials: a
// launcher's role must be one of the roles of the kind its pod's labels name (podKindOf), a
// workflow role of an issue pod or the controller of the project controller's, and its claim is the
// one that kind derives (podKind.claimToken). Tokens minted here are cached as hashes; after a
// restart a known role's Secret is read once however often its launcher says hello
// (launcherCredential), so a restart's reconnecting launchers cost one read each and
// unauthenticated hellos cannot turn into an unbounded stream of Kubernetes requests. A hello waits
// for that read up to apiTimeout.
//
// A launcher holding its role's current token from the Sandbox's current pod, which no Secret
// binds, is of a pod no launch made: the Agent Sandbox controller recreated it after an eviction or
// a node loss. Its pod is bound at that hello (bindAtHello), which the hello waits for up to the
// same apiTimeout, and the launcher is accepted once the binding names its pod, so its roles need
// not wait for the supervisor's next launch, nor that launch replace the pod once more.
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
		if err != nil || !slices.Contains(kind.roles(), role) {
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
			switch err := r.bindAtHello(ctx, view.sandbox, view.pod, role); {
			case errors.Is(err, errNotBindable):
				return nil, shimwire.LauncherHelloAck{}, "launcher pod UID is not bound to its Secret epoch"
			case err != nil:
				return nil, shimwire.LauncherHelloAck{}, "launcher pod could not be bound to its Secret epoch"
			}
			// The binding holds what it wrote, so this is answered from memory, and it is checked
			// again as a whole: the binding read each Secret afresh.
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
		token, err := kind.claimToken(project, credential.issue, role)
		if err != nil || SandboxName(token) != hello.Sandbox {
			return nil, shimwire.LauncherHelloAck{}, "launcher credential does not name this pod's role"
		}
		// The pod the connection is accepted for carries the instant the API server created it,
		// and the instant the API server created its Sandbox, where that Sandbox owns the issue's
		// kept volume. The two decide whether a release's saved work is this pod's to restore, or
		// is on its volume already (launchers.restoreWorkspace). Both come from the informer's
		// objects, so neither costs a request.
		return r.launchers.accept(token, credential.issue, view.pod.CreationTimestamp.Time, keptVolumeSince(view.sandbox), hello), r.launcherHelloAck(kind), ""
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
// role's Secret, single-flight per Secret of a Sandbox. The read outlives the hello that began it:
// it waits its turn on the client's rate limiter for as long as that takes
// (readLauncherCredential), while each hello waits for it until ctx ends and is then refused, so
// the launcher's next hello joins the same read or finds what it stored, never starting another. A
// Secret whose read failed is not read again for that Sandbox for credentialRetry.
func (r *Runtime) launcherCredential(ctx context.Context, s *sandbox, role claim.Role) (launcherCredential, error) {
	name := roleSecretName(s.Name, role)
	credential, held, readFrom := r.launcherAuth.lookup(name, s.UID)
	if held {
		return credential, nil
	}
	if r.now().Before(readFrom) {
		return launcherCredential{}, fmt.Errorf("launcher Secret %s: its last read failed; it is read again from %s", name, readFrom.UTC().Format(time.RFC3339))
	}
	read := r.launcherAuth.reads.DoChan(name+"/"+string(s.UID), func() (any, error) { return r.readLauncherCredential(s, name) })
	select {
	case result := <-read:
		if result.Err != nil {
			return launcherCredential{}, result.Err
		}
		return result.Val.(launcherCredential), nil
	case <-ctx.Done():
		return launcherCredential{}, ctx.Err()
	}
}

// readLauncherCredential reads the launcher Secret name of s and stores its credential, or records
// and logs that the read failed. It runs for the runtime's lifetime rather than any one hello's, so
// its wait on the client's rate limiter is bounded by nothing but its turn; the request itself is
// bounded by apiTimeout (requestConfig).
func (r *Runtime) readLauncherCredential(s *sandbox, name string) (launcherCredential, error) {
	if credential, held, _ := r.launcherAuth.lookup(name, s.UID); held {
		return credential, nil
	}
	secret, err := r.kube.CoreV1().Secrets(r.namespace).Get(r.lifetime, name, metav1.GetOptions{})
	if err == nil && (!ownedBySandbox(secret.OwnerReferences, s.UID) || len(secret.Data[LauncherTokenFile]) == 0) {
		err = fmt.Errorf("launcher Secret %s is not a credential of Sandbox %s", name, s.Name)
	}
	if err != nil {
		r.launcherAuth.fail(name, s.UID, r.now())
		r.log.Warn("sandbox runtime: a launcher Secret could not be read; its launcher's hellos are refused until it is read again",
			"secret", name, "sandbox", s.Name, "readAgainAfter", credentialRetry, "err", err)
		return launcherCredential{}, err
	}
	return r.launcherAuth.store(name, s.UID, credentialFromSecret(secret)), nil
}

func credentialFromSecret(secret *corev1.Secret) launcherCredential {
	return launcherCredential{podUID: secret.Annotations[launcherPodUIDAnnotation],
		issue: secret.Annotations[annotationIssue], hash: sha256.Sum256(secret.Data[LauncherTokenFile])}
}

// errNotBindable is a hello's pod that bindAtHello does not bind (bindable).
var errNotBindable = errors.New("the pod is not its Sandbox's current live pod")

// bindable reports whether pod, the pod s owns in the pod store (view), is one a hello may bind its
// launchers to: s is not Suspended, and pod is neither being deleted nor ended. A pod of a
// Suspended Sandbox is on its way out, as such a pod is.
func bindable(s *sandbox, pod *corev1.Pod) bool {
	return s.mode() != modeSuspended && pod.DeletionTimestamp == nil && !terminal(pod)
}

// bindAtHello binds the launcher Secrets of pod, the pod s owns in the pod store, to pod where no
// launch did, for a hello of role's launcher. After an eviction or a node loss the Agent Sandbox
// controller makes a Running Sandbox's pod again from the template the Sandbox stores, under a new
// uid, and that pod projects the Secrets a launch bound to the pod before it: its launchers hold
// each role's current token and name a pod no Secret binds. The caller has checked the hello's
// token against role's credential, so a hello without the current token writes nothing, and nor
// does one from a pod that is not bindable. The binding is single-flight per Sandbox and outlives
// the hello that began it, as a credential's read does (launcherCredential): the hello waits for it
// until ctx ends and is then refused, and the launcher's next hello joins the same binding or
// finds what it held. A binding whose writes failed is not begun again for role's Secret for
// credentialRetry, and a hello meanwhile is refused without a request, so the launchers of a pod
// the API server will not bind, each saying hello again a second after its refusal, ask for one
// binding each credentialRetry.
func (r *Runtime) bindAtHello(ctx context.Context, s *sandbox, pod *corev1.Pod, role claim.Role) error {
	if !bindable(s, pod) {
		return errNotBindable
	}
	if from := r.launcherAuth.bindFrom(roleSecretName(s.Name, role), s.UID); r.now().Before(from) {
		return fmt.Errorf("sandbox %s: its pod's last binding failed; it is bound again from %s", s.Name, from.UTC().Format(time.RFC3339))
	}
	bind := r.launcherAuth.binds.DoChan(string(s.UID), func() (any, error) { return nil, r.bindCurrentPod(s.Name, s.UID, pod.UID) })
	select {
	case result := <-bind:
		return result.Err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// bindCurrentPod binds the launcher Secrets of the pod with uid podUID to it, where that pod is
// still the bindable pod of the Sandbox named name with uid sandboxUID once the pod's launch turn
// is held. The turn is taken only where nothing holds it: a launch, a suspension or a release that
// holds it decides the pod itself, so the binding is refused at once rather than waiting the turn
// out, and the launcher's next hello finds what that left. It writes the Secret of each role of the
// pod that no held credential binds to it yet, so the launchers of one pod write each Secret once
// however many of them say hello, and it holds what it wrote (bindLauncherSecrets), so the next
// hello is accepted without a request. It runs for the runtime's lifetime, as
// readLauncherCredential does; each request is bounded by apiTimeout (requestConfig).
func (r *Runtime) bindCurrentPod(name string, sandboxUID, podUID types.UID) error {
	release, free := r.tryLockPod(name)
	if !free {
		return fmt.Errorf("sandbox %s: its pod's launch turn is held", name)
	}
	defer release()
	v, err := r.view(name)
	if err != nil {
		return err
	}
	if v.sandbox == nil || v.sandbox.UID != sandboxUID || v.pod == nil || v.pod.UID != podUID || !bindable(v.sandbox, v.pod) {
		return errNotBindable
	}
	kind, err := podKindOf(v.sandbox.Labels)
	if err != nil {
		return err
	}
	var unbound []claim.Role
	for _, role := range kind.roles() {
		if credential, held, _ := r.launcherAuth.lookup(roleSecretName(name, role), sandboxUID); !held || credential.podUID != string(podUID) {
			unbound = append(unbound, role)
		}
	}
	if len(unbound) == 0 {
		return nil
	}
	if err := r.bindLauncherSecrets(r.lifetime, v.sandbox, unbound, string(podUID)); err != nil {
		for _, role := range unbound {
			r.launcherAuth.failBind(roleSecretName(name, role), sandboxUID, r.now())
		}
		r.log.Warn("sandbox runtime: binding the role launchers of a pod made outside a launch failed; its launchers' hellos are refused until a binding lands",
			"sandbox", name, "pod", podUID, "bindAgainAfter", credentialRetry, "err", err)
		return err
	}
	r.log.Info("sandbox runtime: bound the role launchers of a pod made outside a launch to that pod",
		"sandbox", name, "pod", podUID, "roles", unbound)
	return nil
}

// lookup is the credential held as name's for the Sandbox with uid, if one is (held), and otherwise
// when a read of it may next begin: credentialRetry after that Sandbox's latest failed read, the
// zero time when none failed.
func (c *launcherCredentials) lookup(name string, uid types.UID) (credential launcherCredential, held bool, readFrom time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	secret := c.secrets[name][uid]
	switch {
	case secret.held:
		return secret.credential, true, time.Time{}
	case secret.failedAt.IsZero():
		return launcherCredential{}, false, time.Time{}
	}
	return launcherCredential{}, false, secret.failedAt.Add(credentialRetry)
}

// bind holds credential, a binding write's or a binding check's, as name's for the Sandbox with
// uid, in place of whatever was held. A binding is made under that Sandbox's pod's launch turn, by
// a launch or by a hello's binding of its current pod (bindAtHello), which proves it holds the name
// now, so every other Sandbox's entry of the name, a deleted one's, goes.
func (c *launcherCredentials) bind(name string, uid types.UID, credential launcherCredential) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.secrets == nil {
		c.secrets = make(map[string]map[types.UID]launcherSecret)
	}
	c.secrets[name] = map[types.UID]launcherSecret{uid: {credential: credential, held: true}}
}

// store holds credential, a read's, as name's for the Sandbox with uid, unless that Sandbox holds
// one already: a binding that completed during the read is newer than it. It is the credential the
// Sandbox holds. A failed binding at a hello stays recorded (failBind).
func (c *launcherCredentials) store(name string, uid types.UID, credential launcherCredential) launcherCredential {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.secrets[name][uid]
	if held.held {
		return held.credential
	}
	c.put(name, uid, launcherSecret{credential: credential, held: true, bindFailedAt: held.bindFailedAt})
	return credential
}

// fail records that a read of name for the Sandbox with uid failed at at, unless that Sandbox
// holds a credential. A failed binding at a hello stays recorded (failBind).
func (c *launcherCredentials) fail(name string, uid types.UID, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.secrets[name][uid]
	if held.held {
		return
	}
	c.put(name, uid, launcherSecret{failedAt: at, bindFailedAt: held.bindFailedAt})
}

// failBind records that a binding of name at a hello for the Sandbox with uid failed at at,
// keeping whatever else is held of it.
func (c *launcherCredentials) failBind(name string, uid types.UID, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	secret := c.secrets[name][uid]
	secret.bindFailedAt = at
	c.put(name, uid, secret)
}

// bindFrom is when a binding of name at a hello for the Sandbox with uid may next begin:
// credentialRetry after its latest failed one, the zero time when none failed since the Secret was
// last bound.
func (c *launcherCredentials) bindFrom(name string, uid types.UID) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	failed := c.secrets[name][uid].bindFailedAt
	if failed.IsZero() {
		return time.Time{}
	}
	return failed.Add(credentialRetry)
}

// forget drops whatever is held of name, for every Sandbox. A read that lands afterwards for a
// Sandbox since deleted writes only that Sandbox's entry, which nothing looks up again, until the
// name's next bind or forget drops it.
func (c *launcherCredentials) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.secrets, name)
}

// put holds secret as name's for the Sandbox with uid; its caller holds c.mu.
func (c *launcherCredentials) put(name string, uid types.UID, secret launcherSecret) {
	if c.secrets == nil {
		c.secrets = make(map[string]map[types.UID]launcherSecret)
	}
	if c.secrets[name] == nil {
		c.secrets[name] = make(map[types.UID]launcherSecret)
	}
	c.secrets[name][uid] = secret
}

// cacheLauncherCredential holds secret's credential, read or written as bound to its pod, for the
// Sandbox s.
func (r *Runtime) cacheLauncherCredential(s *sandbox, secret *corev1.Secret) {
	r.launcherAuth.bind(secret.Name, s.UID, credentialFromSecret(secret))
}

// forgetLauncherCredentials drops what is held of every launcher role's Secret of the Sandbox named
// name, once that Sandbox is deleted: whichever roles its pod ran, no other Sandbox's Secret has a
// name of its.
func (r *Runtime) forgetLauncherCredentials(name string) {
	for _, role := range launcherRoles {
		r.launcherAuth.forget(roleSecretName(name, role))
	}
}

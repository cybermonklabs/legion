package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/sjawhar/legion/daemon/internal/appauth"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghconfig"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// Spawn starts a fresh agent for spec's claim, over whatever the claim's Sandbox already holds
// (decision 3a): a relaunch before the agent's first registration — the registration deadline, a
// death before registration, a retried launch, boot finishing a launch a crash interrupted — is a
// Spawn over an existing Sandbox. A spec with a ResumeSessionFile is refused: continuing a
// recorded agent is Resume's. spec.Plugins travels with a claim that has registered a session
// (specs.SpawnSpec), which a Spawn's claim never has yet, and is cleared here anyway, so Spawn
// always installs nothing and a fresh pod always loads the image's copies.
func (r *Runtime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile != "" {
		return runtime.Locator{}, fmt.Errorf("spawn %s: a ResumeSessionFile is Resume's to set", spec.Claim)
	}
	spec.Plugins = nil
	return r.relaunch(ctx, nil, spec)
}

// Resume starts the agent spec's claim recorded, again, from spec.ResumeSessionFile. prev is a
// hint: the issue's Sandbox is found by name. A suspended Sandbox waits out its pod before
// restarting; an active issue pod starts only this role. The launcher checks the session file
// before starting the child, so a missing retained session never becomes a fresh agent.
func (r *Runtime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	if spec.ResumeSessionFile == "" {
		return runtime.Locator{}, fmt.Errorf("resume %s: no session file to resume from", spec.Claim)
	}
	if err := (runtime.Known{Claim: spec.Claim, Locator: prev}).Validate(); err != nil {
		return runtime.Locator{}, fmt.Errorf("sandbox runtime: resume: %w", err)
	}
	return r.relaunch(ctx, prev, spec)
}

// relaunch ensures the claim's pod once, then starts only this role's worker-shim through its
// authenticated launcher. An existing healthy pod is never suspended or reinitialized when another
// role starts or one role recovers. A pod is not healthy when it cannot run the role as a pod
// created now would: it holds, for one of its roles, something fixed for its life that a pod created
// now is not handed and whose worker this daemon cannot ask to push its work first (movedInPod,
// the rule evaluate reads such a role StaleAddress by): it runs the layout before one container,
// one launcher container per role (movedLayout), whose workers predate the update round's
// contract, or its launchers dial a stream other than the one a pod created now is handed, and
// they never redial, so they never reach this daemon at all. The first role relaunched after one
// of these replaces it, and its siblings resume into the new pod.
//
// A live pod a round covers (roundCovers) that has merely drifted from the configuration — a
// container on another image, the kind's reservation, an operator volume or mount, the disk size,
// the ServiceAccount it runs as, the role's overlays, the secrets broker's enrollment, the session
// store or the GitHub owners its gh volume projects a directory for (PodDrift) — is kept,
// and this role is started in it.
// Such a pod is replaced by an update round, which first asks every role in it to push its work
// and answer, and then ends the pod (ReplacePod): replacing it at a launch would end every role's
// turn in it and delete the issue's disk with everything unpushed on it (INFRA-484). A launch into
// a drifted pod is therefore an ordinary launch, the install of a stale-plugin pair (spec.Plugins,
// LEGION-164) included, which brings this launch's agent onto the current plugin without a fresh
// pod. Its role is started as the kept pod was built, from the volumes and mounts of that pod's
// own stored template (keptBy, keptPod) and not from the configuration as it now reads: two of
// those drift facets, the broker's enrollment and the role's overlays, are named in the role's own
// start command, and a process started against what its pod cannot carry refuses to run, never
// registers, and so never answers the round that is to replace that pod. A pod whose template says
// nothing this daemon can read is replaced at the launch instead, as every drifted pod was before
// the round.
//
// A live pod no round covers is replaced here for every kind of drift instead, as every launch did
// before the round existed: no round is ever opened over such a pod, so a launch that kept it
// would leave its role in a pod the configuration has left behind for ever (movedInPod,
// roundCovers). Every facet of the pod itself but the account reaches this launch as a moved
// address — the two whose worker cannot be reached at all and the four driftedInPod reads, which
// in a covered pod are a round's — and the shape its template was built from is read here
// (shapeDrift, the one reading PodDrift takes of a covered pod's shape). A launch installing a
// stale-plugin pair into such a pod keeps it, as it always did
// (spec.Plugins, LEGION-164): the install, not a fresh pod on the configured image, is what brings
// that agent onto the current plugin, and a pod replaced for any other reason in the same sweep
// still is.
//
// A ServiceAccount other than the configured one (accountDrift) is answered by whoever answers
// the pod's other drift. A launch into a pod a round covers keeps it for the account as for every
// other drift, and leaves it to that pod's round, which has every worker in it push its work
// before the pod is ended. A launch into a pod no round covers replaces it for the account
// whatever it is installing, since no round will ever come for such a pod: the account is fixed
// for the pod's life, and until the pod is created afresh its agents go on holding the identity
// the old account grants, so an old account can be deleted only once every pod of it has gone.
//
// Replacing a live pod — its Sandbox Running, the pod its own, not ended, its init containers
// passed, and not a pod its node never started (neverStarted) — ends the process of every role
// still running in it, wherever its turn stood, so the Suspended patch that replaces it names it on
// the Sandbox in the same write (suspendDisplacing): evaluate then reports each of those roles
// ended by the daemon's replacement (Displaced), which the supervisor resumes uncharged
// (LEGION-729). A pod already ended, or never the Sandbox's, is named nothing displaced: its roles
// ended with it. Nor is a pod that never started, where no role ran: the launch that waited the
// pod's start bound on it (startBound) was charged, and the next one replaces it rather than
// waiting on it again. Where the Sandbox still runs and its own pod ended with a reason
// (runtime.PodEnd), the Suspended patch that replaces that pod records the end on the Sandbox
// instead, keyed by the pod's uid (suspendRecordingEnd): an eviction ends every role of the pod,
// and a role whose next check comes after this launch has replaced the pod reads the eviction
// from there (evaluate's endedPod), since the pod's own reason goes with the pod. Before each
// start the Sandbox records the addresses the generation is handed (recordAddresses). A new pod is
// provisioned by its kind (podKind.provision), under nothing but the pod's own launch turn: an
// issue pod has its workspace's provisioning token minted, the controller's pod needs nothing. A
// workflow claim passed its tree's lifecycle check before the call (supervise's checkLaunch), so
// the tree's cleanup, which waits for every claim of the tree to retire, lists whatever Sandbox
// this creates. Every Kubernetes request of the launch is sent again while the API server answers
// it overloaded, up to the launch's own deadline for it (launching, resend), so a server under load
// slows the launch rather than failing it.
// The try is bounded on the runtime's own account from the moment it holds the pod's launch turn
// (launchBound, LaunchBound): a try that never answers ends at the bound with an error naming it,
// which the supervisor charges as a launch failure and tries again; one the caller's own context
// ends is not the bound's, and the supervisor charges it nothing. The cleanups of a failed try
// (suspendFailedLaunch, stopIssuedChild) run on the caller's context with bounds of their own,
// since the try's bound may be what ended it.
// On an error the caller owns stopping its previous process (runtime.Runtime's Spawn and Resume);
// relaunch stops only the child it may itself have started: a Start the launcher received but did
// not answer in time (stopIssuedChild). A launch installs its plugin pair (launcherCommand) only
// when spec.Plugins is set and the pod was kept rather than replaced: a pod replaced for any other
// reason — a suspended Sandbox, a lost launcher binding, a failed init, a pod that never started, a
// moved address — comes up on the configured image, which the boot gate holds to the daemon's
// contract, so its copies are already right; carrying the install onto a fresh pod would be
// installing over a pair already current. l.spec.Plugins is cleared in that case before
// launcherCommand builds its argv.
//
// An issue Sandbox whose disk shape is not the one this launch renders (supersededDisk) is
// replaced with the pod. It is one of the release before the pod-owned disk, which owns an
// `issue` claim template with no kept label. Or it owns no template where the launch keeps its
// volume (keptVolume). Or it owns a kept volume where the launch keeps none, or a kept volume of
// another storage class, size or access modes. volumeClaimTemplates is immutable in the CRD, so
// the only way to the launch's shape is to delete the Sandbox and create it again
// (replaceSupersededSandbox). That replacement carries the name the suspension wrote onto the
// Sandbox it creates, and keeps it in the runtime's hands while the issue has no Sandbox at all
// (stranded), so the roles that the replacement ended are read as displaced rather than dead, in
// the gap between the delete and the create too.
//
// The other disk shape alone ends no live pod whose shape record drifts (shapeDrift). Each change
// that moves an issue's disk shape moves that record too: keep_volume, the issue's sandbox, the
// microVM block's storage class and the disk's size. So the pod's update round asks every role in
// it to push its work and then ends it (ReplacePod), and the launch after the round, which finds
// no live pod, replaces the Sandbox. A live pod whose record does not drift is replaced here, with
// its Sandbox: that is the cutover from the release before the pod-owned disk. Every launch that
// replaces the pod, for any reason, replaces a Sandbox of the other shape with it, so the Running
// patch never writes a pod that mounts a claim its Sandbox does not own.
//
// A launch resets a kept volume in the same way (volumeReset): it replaces the Sandbox, and with it
// the claim, before it provisions. It does so where the stored pod shows that an init container
// failed on the volume for a cause local to it (localInitFailure). It does so too where the Sandbox
// carries the reset mark (volumeResetAnnotation), which ReplacePod or the release wrote when they
// took such a pod down. A fault on a kept volume stays there for each later pod of the issue, and
// only a new volume cures it. Such a pod is not live, so the launch replaces it in any case. The
// Sandbox that the reset creates carries no mark (createSandbox).
func (r *Runtime) relaunch(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	ctx = launching(ctx)
	l, err := r.prepare(spec)
	if err != nil {
		return runtime.Locator{}, err
	}
	r.forget(spec.Claim)
	release, err := r.lockPod(ctx, l.name)
	if err != nil {
		return runtime.Locator{}, fmt.Errorf("launch %s: take its pod's launch turn: %w", spec.Claim, err)
	}
	defer release()
	bounded, cancel := context.WithTimeout(ctx, r.launchBound)
	defer cancel()
	fail := func(step string, err error) (runtime.Locator, error) {
		return runtime.Locator{}, fmt.Errorf("launch %s: %s: %w", spec.Claim, step, boundRanOut(ctx, bounded, r.launchBound, err))
	}
	s, err := r.ensureSandbox(bounded, l)
	if err != nil {
		return fail("ensure its sandbox", err)
	}
	pod := r.storedPod(s.Name)
	_, initExit := failedInit(pod)
	live := s.mode() != modeSuspended && ownedBy(pod, s.UID) && !terminal(pod) && initExit == nil
	// A pod its node never started (neverStarted) is not live either: no launcher connects from it,
	// so a launch that kept it would wait out its start bound on it, be charged a launch failure,
	// and leave it for the next try to find again. It is replaced as a pod whose init failed is,
	// and names nothing displaced, since no role ever ran in it. The warning gives the pod's age,
	// how long its node has had it and the bound it was judged by, which is what says whether that
	// node is the one to look at. A pod with a kept volume gets the longer bound a move of its
	// volume needs (startBound).
	bound := r.startBound(s)
	if unstarted, scheduledFor, stuck := neverStarted(pod, r.now(), bound); live && stuck {
		r.log.Warn("sandbox runtime: replacing a pod older than its start bound that its node has not made ready to start its containers",
			"pod", pod.Name, "uid", pod.UID, "node", pod.Spec.NodeName, "notStartedFor", unstarted.Round(time.Second),
			"scheduledFor", scheduledFor.Round(time.Second), "startBound", bound, "claim", spec.Claim)
		live = false
	}
	_, accountDrifted := accountDrift(pod, r.pod.ServiceAccount)
	installing := l.spec.Plugins != nil
	// A kept volume on which an init container failed for a cause local to it is reset: the launch
	// replaces its Sandbox as it replaces one of the other disk shape (volumeReset). A change of
	// the disk shape replaces the Sandbox too, so its reason comes first.
	migrate := r.supersededDisk(s, l)
	if migrate == "" {
		migrate = volumeReset(s, pod)
	}
	covered := roundCovers(l.kind)
	// A live pod no round covers is this launch's to replace for every kind of drift: every facet
	// of the pod itself but the account comes through movedInPod, the account is read here as
	// accountDrifted, and the shape its template was built from is read here too, unless this
	// launch is installing a stale-plugin pair into it (roundCovers, shapeDrift).
	//
	// A pod running as a ServiceAccount other than the configured one is this launch's to
	// replace only where no round covers that pod (accountDrift, roundCovers), and then whatever
	// the launch is installing: no round will ever come for such a pod, the account is fixed for
	// the pod's life, and until the pod is created afresh its agents go on holding the identity
	// the old account grants. A pod a round covers is kept for the account as for its other
	// drift, and that round, which has every worker in it push its work first, is what replaces
	// it.
	//
	// A Sandbox of the other disk shape (migrate) ends a live pod only where the pod's shape record
	// does not drift, the cutover from the release before the pod-owned disk. A drifted record
	// opens the pod's update round, which ends the pod once every role in it has pushed its work
	// (ReplacePod). A pod that this launch replaces for any other reason takes such a Sandbox with
	// it below. A reset comes with a pod that is not live: a pod whose init container failed, or
	// no pod after ReplacePod or the release.
	replace := !live || (accountDrifted && !covered) ||
		(migrate != "" && len(r.shapeDrift(s, l.shapeKey())) == 0) ||
		slices.ContainsFunc(l.roles, func(role claim.Role) bool { return len(r.movedInPod(pod, role)) > 0 }) ||
		(!installing && !covered && len(r.shapeDrift(s, l.shapeKey())) > 0)
	if !replace {
		bound, err := r.launcherBound(bounded, s, pod, &l)
		if err != nil {
			return fail("read its pod's launcher bindings", err)
		}
		replace = !bound
	}
	if !replace {
		// The pod is kept, so this role is started as that pod was built rather than as the
		// configuration now reads (keptBy): a drifted pod the update round has not replaced yet
		// carries neither an enrollment nor an overlay the configuration gained after it was
		// created, and a start naming either would refuse to run, never register, and so never
		// answer the round that is to replace it (INFRA-484). Where the stored template says
		// nothing the daemon can read, it cannot tell what the pod carries, and the launch
		// replaces the pod where it stands, which is what every launch into a drifted pod did
		// before the round existed.
		kept, readable := r.keptBy(s, l)
		if !readable {
			replace = true
		} else {
			l.kept = &kept
		}
	}
	if installing && replace {
		// Replaced for some other reason: the fresh pod's image is already current, so nothing is
		// installed into it.
		l.spec.Plugins = nil
	}
	if replace {
		switch {
		case live:
			suspended, err := r.suspendDisplacing(bounded, s, pod.UID, launchReplacement)
			if err != nil {
				return fail("suspend its sandbox", err)
			}
			// The patch's own copy, which carries the annotation naming the replaced pod: a
			// migrating launch reads it from here onto the Sandbox it creates
			// (replaceSupersededSandbox).
			s = suspended
		case s.mode() != modeSuspended:
			if err := r.suspendRecordingEnd(bounded, s, pod); err != nil {
				return fail("suspend its sandbox", err)
			}
		}
		old, err := r.awaitPodGone(bounded, s)
		if err != nil {
			return fail("wait out its previous pod", err)
		}
		if migrate != "" {
			if s, err = r.replaceSupersededSandbox(bounded, l, s, migrate); err != nil {
				return fail("replace its sandbox ("+string(migrate)+")", err)
			}
		}
		if err := l.kind.provision(bounded, r, &l, s); err != nil {
			return runtime.Locator{}, fmt.Errorf("launch %s: %w", spec.Claim, boundRanOut(ctx, bounded, r.launchBound, err))
		}
		if err := r.writeLauncherSecrets(bounded, s, &l); err != nil {
			return fail("write its launcher secrets", err)
		}
		template := r.podTemplate(l)
		running, err := r.patch(bounded, s,
			jsonPatchOp{Op: "add", Path: "/spec/podTemplate", Value: template},
			jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: modeRunning},
		)
		if err != nil {
			r.suspendFailedLaunch(ctx, spec.Claim, s)
			return fail("set its sandbox running", err)
		}
		pod, err = r.awaitNewPod(bounded, running, old)
		if err != nil {
			r.suspendFailedLaunch(ctx, spec.Claim, s)
			return fail("wait for its new pod", err)
		}
		s = running
		if err := r.bindLauncherSecrets(bounded, s, l, string(pod.UID)); err != nil {
			return fail("bind its role launchers to the new pod", err)
		}
	}
	loc := r.locatorFor(spec.Claim, spec.Role, pod.UID, spec.Generation)
	// The start bound is read from the Sandbox the launch ends on: after a replacement of another
	// disk shape (replaceSupersededSandbox), the new Sandbox whose volume the pod mounts.
	ended, deadline, err := r.awaitRoleLauncher(bounded, loc, r.startBound(s))
	if err != nil {
		return fail("wait for its role launcher", err)
	}
	starting, cancel := context.WithDeadline(bounded, deadline.Add(r.terminationGrace))
	defer cancel()
	if ended {
		// The pod this launch waited on has ended — gone, replaced, terminal, or its init container
		// failed — so no role could start in it. Return its recorded pod: the supervisor's next
		// probe reads that pod's own end, which says what happened, rather than this launch being
		// charged a launcher timeout that end explains.
		r.join(loc)
		return loc, nil
	}
	// A launcher still running an earlier generation of this role runs a process the supervisor has
	// already given up on: it is stopped before the new generation starts, so two generations of a
	// role never run side by side.
	if state, connected := r.launchers.state(spec.Claim, string(pod.UID)); connected && state.Child != nil && state.Child.Generation < spec.Generation {
		stale := state.Child.Generation
		if err := r.launchers.stop(starting, spec.Claim, string(pod.UID), r.stopFrame(stale)); err != nil {
			return fail(fmt.Sprintf("stop its earlier generation %d", stale), err)
		}
	}
	if err := r.recordAddresses(starting, s, spec.Role, spec.Generation, l.enrolls(r)); err != nil {
		return fail("record the addresses its generation is handed", err)
	}
	// The start sent carries what this launch learned from the cluster since prepare measured it,
	// so it is measured again here: a frame no launcher reads is refused as the prompt it is, which
	// supervise charges nothing, rather than failing in the writer as a charged launch failure.
	command := launcherCommand(l, r)
	if err := l.checkStartFrame(command); err != nil {
		return fail("measure its start frame", err)
	}
	if err := r.launchers.start(starting, spec.Claim, string(pod.UID), command); err != nil {
		return fail("start through its authenticated launcher", errors.Join(err, r.stopIssuedChild(ctx, spec.Claim, string(pod.UID), spec.Generation, err)))
	}
	r.nameSession(loc, l)
	r.join(loc)
	return loc, nil
}

// keptBy is what the live pod of the Sandbox read as s was built with, for the role l starts in
// it: the enrollment decision for that role in that pod (enrollsIn, which reads the broker's
// volumes the pod carries), which of the role's own overlays and of every declared variant's
// overlays
// its container holds (heldRoleOverlays), and the session store it was built under
// (heldSessionStore, the URL file its generation is pointed at). A launch that keeps such a pod
// builds its start command from these rather than from the configuration as it now reads, so the
// role is started as its pod was built (launcherCommand, roleOperatorEnv, mainEnvironment) and
// runs degraded until the update round replaces the pod — able to push its work and answer that
// round, where a start naming what its pod cannot carry would refuse to run at all (keptPod,
// INFRA-484).
// The same enrollment decision writes the generation's address record (recordAddresses), so the
// record says what the start said.
//
// It reads the Sandbox's stored pod template, the build of the pod the controller made from it,
// and not the pod-shape annotation (podShapeAnnotation), which records the configuration the pod
// was built from rather than what its container holds, and which a pod created before the
// annotation existed does not carry. readable is false when the template
// holds no container for the role — a template of another shape, or one this daemon cannot read:
// the daemon then cannot tell what the pod carries, and relaunch replaces the pod.
func (r *Runtime) keptBy(s *sandbox, l launch) (keptPod, bool) {
	spec := s.Spec.PodTemplate.Spec
	overlays, variantFiles, readable := r.heldRoleOverlays(spec, l.spec.Role)
	if !readable {
		return keptPod{}, false
	}
	kind, _, _ := heldSessionStore(spec)
	return keptPod{enrolled: r.enrollsIn(&spec, l.spec.Role), trades: r.tradesIn(&spec, l.spec.Role), overlays: overlays, variantFiles: variantFiles, store: kind}, true
}

// stopIssuedChild stops generation of token's launcher in podUID after its Start returned startErr,
// unless the launcher answered that Start with a refusal (startRefusal), which started nothing, or
// the start was refused before its Start was sent because the pod's shared public leak check could
// not be installed (runtime.LeakCheckRefusal, launchers.serveLeakCheck).
// Any other error leaves the Start's outcome unknown: the launcher may have received it and begun
// the generation while its answer was still on its way. The Stop travels after the Start on the
// same connection, so the launcher handles it once it has handled the Start. It runs with its own
// bound, since the start's own deadline may be what ended it.
func (r *Runtime) stopIssuedChild(ctx context.Context, token claim.Token, podUID string, generation uint64, startErr error) error {
	var refused startRefusal
	if errors.As(startErr, &refused) || runtime.LeakCheckCause(startErr) != "" {
		return nil
	}
	stopping, cancel := context.WithTimeout(context.WithoutCancel(ctx), r.bootTimeout+r.terminationGrace)
	defer cancel()
	if err := r.launchers.stop(stopping, token, podUID, r.stopFrame(generation)); err != nil {
		return fmt.Errorf("stop generation %d, which its unanswered Start may have begun: %w", generation, err)
	}
	return nil
}

// boundRanOut is err with a note that the launch try's bound ran out, when it did: the bounded
// context has ended while the caller's own, parent, is alive, and err is a deadline exceeded. Any
// other err is returned as it is — a step's own deadline (apiTimeout's call, the start frame's
// grace), or the caller's context ending, which the supervisor tells apart by that context's own
// error and charges nothing.
func boundRanOut(parent, bounded context.Context, bound time.Duration, err error) error {
	if bounded.Err() != nil && parent.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w (the launch try's bound of %s ran out)", err, bound)
	}
	return err
}

// relaunchCalls is the most bounded calls one replacing launch try makes between its waits —
// client requests, each bounded by apiTimeout once the limiter lets it go (requestConfig), the
// mints bounded by call, and the store reads and launcher frames bounded by their context alone,
// the try's — counted generously, every retry and rare branch included, since the bound they size
// (launchBound) must never be shorter than a legitimate try. Per try (relaunchPodCalls):
// ensureSandbox's Get, its Create or relabel patch and that relabel's store read, and for a Sandbox
// being deleted the wait's last Get, past its deadline, and the Get and Create after it (6); the
// suspension patch (1); awaitPodGone's confirming Get, which may begin just before its deadline
// (1); provision's token mint and its Secret upsert's Get, Delete and Create (4), and its
// repository cache's ensure (cacheEnsureCalls); the Running patch (1); the stop of an earlier
// generation (1); the address record patch (1); the Start frame (1) — 16, the cache's ensure apart.
// A try that replaces its Sandbox for another disk shape or a reset of its kept volume
// (replaceSupersededSandbox) still spends one call more, and so sets the count at 17: its
// deleteStoredSandbox Get, the fenced Delete, awaitSandboxDeleted's confirming Get and the Create
// (4) against the 3 of the 6 above that ensureSandbox's wait for a Sandbox being deleted spends.
// That try never takes that wait: a Sandbox that ensureSandbox waited out is gone, and the one it
// creates in its place already has the disk shape that the launch renders (sandboxManifest),
// carries no reset mark and has no pod, so the launch does not replace it.
// Per launcher role of the pod (relaunchRoleCalls, claim.Roles for an issue pod, the most any pod
// has): launcherBound's Secret Get (1); writeLauncherSecrets' credential mint and its upsert's Get,
// Delete and Create (4); bindLauncherSecrets' Get and Update, and both again on a conflict (4).
var relaunchCalls = relaunchPodCalls + relaunchRoleCalls*len(claim.Roles)

const (
	relaunchPodCalls  = 17 + cacheEnsureCalls
	relaunchRoleCalls = 9
	// cacheEnsureCalls is one ensure of a repository's cache (ensureRepositoryCache) at its most:
	// ensureCacheSandbox's Get and Create, the Get after a Create that found the name taken, and
	// for a Sandbox being deleted the wait's last Get and the Get and Create after it (6);
	// writeCacheSecret's token mint, its Secret's Get, and its upsert's Get, Delete and Create (5);
	// reconcileCachePod's suspension patch, awaitPodGone's confirming Get and the Running patch (3).
	cacheEnsureCalls = 14
)

// launchBound is the longest one launch try may run once it holds its pod's launch turn, from the
// runtime's boot timeout, the longest start bound any of its pods gets (startBound),
// ProvisionBound and termination grace (LaunchBound): the sum of a replacing try's waits —
// ensureSandbox's wait for a Sandbox being deleted, awaitPodGone and awaitNewPod, the boot timeout
// each; provision's waits on its repository's cache (cacheWaits); awaitRoleLauncher's backstop;
// the termination grace the start frame has past the launcher deadline — and of its relaunchCalls
// calls at apiTimeout each.
func launchBound(bootTimeout, startBound, provisionBound, terminationGrace time.Duration) time.Duration {
	waits := 3*bootTimeout + cacheWaits(bootTimeout) + launcherBackstop(startBound, bootTimeout, provisionBound) + terminationGrace
	return waits + time.Duration(relaunchCalls)*apiTimeout
}

// cacheWaits is the most a launch try's provision waits on its repository's cache: the cache's
// turn (lockPod on its Sandbox), which at most one other ensure holds ahead of it in the worst case
// — a launch of another of the repository's issues, or the sweep — with that ensure's two waits and
// its cacheEnsureCalls calls, since an ensure after it finds the cache made; then this try's own
// ensure's two waits, ensureCacheSandbox's for a cache Sandbox being deleted and
// reconcileCachePod's awaitPodGone; then awaitCacheService. Each wait is the boot timeout.
func cacheWaits(bootTimeout time.Duration) time.Duration {
	return 2*bootTimeout + time.Duration(cacheEnsureCalls)*apiTimeout + 2*bootTimeout + bootTimeout
}

// launcherBackstop is await's own budget for a role launcher's wait (awaitRoleLauncher), given the
// pod's start bound, the boot timeout and ProvisionBound: the wait first sees the pod provisioning
// within the start bound of its start or never, no launcher deadline lies past the boot timeout
// plus ProvisionBound from that first sight (launcherDeadline), and two recheck intervals more let
// the wait's own check end it first.
func launcherBackstop(start, boot, bound time.Duration) time.Duration {
	return start + boot + bound + 2*recheckInterval
}

// startBound is how long a launch gives s's pod to start, where it counts the boot timeout for
// every other pod: neverStarted counts it from the pod's creation, and awaitRoleLauncher from the
// start of its wait until it first sees the pod provisioning. A Sandbox that owns a kept volume
// (ownsKeptVolume) gets the kept-volume bound (keptVolumeBound, keptVolumeAttachWait), or the boot
// timeout where that is longer: its pod may wait on its node until the volume detaches from the
// node that held it and attaches to the pod's. Every other Sandbox, the controller's included,
// gets the boot timeout.
func (r *Runtime) startBound(s *sandbox) time.Duration {
	if ownsKeptVolume(s) {
		return max(r.bootTimeout, r.keptVolumeBound)
	}
	return r.bootTimeout
}

// awaitRoleLauncher waits until loc's role launcher has connected, reporting false, or loc's pod
// has ended, reporting true, and returns the deadline the wait stood at when it stopped. The
// launcher starts only once every init container of the pod has finished (an issue pod's two; the
// controller's pod has none), and the three steps whose duration follows the repository's size —
// workspace-fetch's whole fetch — its stream from the repository cache, which no clock ends while
// it keeps receiving (workspace.CacheStreamLowSpeedBytes), its transfers with their retries, and
// the restarts the grace covers (fetchRestartGrace) — then workspace-init's shared clone of the
// feed and the issue's own checkout of it — may take up to ProvisionBound from the fetch's own
// start, a sum that counts no term for the commands which keep CommandTimeout (ProvisionBound,
// manifest.go). So until the wait first sees the pod provisioning (an init container running, or a
// fetch between those restarts: provisioning) it gives the pod start, the pod's start bound
// (startBound), which covers the image pull and, for a kept volume, the volume's move; from then
// on, the boot timeout from the last moment it saw the pod provisioning, never past the boot
// timeout plus ProvisionBound from the first (launcherDeadline). A pod that never provisions waits
// its start bound alone, and the failure names that bound.
func (r *Runtime) awaitRoleLauncher(ctx context.Context, loc runtime.Locator, start time.Duration) (bool, time.Time, error) {
	what := "role launcher " + string(loc.Claim)
	began := time.Now()
	deadline := began.Add(start)
	var first time.Time
	ended := false
	// await's own budget is only a backstop (launcherBackstop). The check below runs at every
	// informer event and at least every recheckInterval, and it ends the wait before it extends the
	// deadline, so the pod is first seen provisioning within the start bound of the wait's start or
	// never, and no deadline lies past the start bound, the boot timeout and ProvisionBound from
	// the wait's start.
	backstop := launcherBackstop(start, r.bootTimeout, r.ProvisionBound())
	err := r.await(ctx, backstop, what, func(context.Context) (bool, error) {
		view, err := r.view(loc.Sandbox.Name)
		if serverBusy(err) && time.Now().Before(deadline) {
			// A store whose list or watch the API server answers overloaded is waited out within the
			// wait's deadline, as the launch's own requests are (resend): the informer lists and
			// watches again by itself.
			return false, nil
		}
		if err != nil {
			return false, err
		}
		_, initExit := failedInit(view.pod)
		if view.pod == nil || string(view.pod.UID) != loc.Sandbox.PodUID || terminal(view.pod) || initExit != nil {
			ended = true
			return true, nil
		}
		if _, ready := r.launchers.state(loc.Claim, loc.Sandbox.PodUID); ready {
			return true, nil
		}
		now := time.Now()
		if !now.Before(deadline) {
			return false, fmt.Errorf("timed out after %s waiting for %s%s", deadline.Sub(began), what, r.stillFetching(view.pod))
		}
		if provisioning(view.pod) {
			if first.IsZero() {
				first = now
			}
			deadline = launcherDeadline(first, now, r.bootTimeout, r.ProvisionBound())
		}
		return false, nil
	})
	return ended, deadline, err
}

// launcherDeadline is when a wait for a role launcher gives up, having first seen its pod
// provisioning at first and last at last: the boot timeout after last, but no later than the boot
// timeout plus bound after first.
func launcherDeadline(first, last time.Time, boot, bound time.Duration) time.Time {
	deadline := last.Add(boot)
	if limit := first.Add(boot + bound); deadline.After(limit) {
		return limit
	}
	return deadline
}

// stillFetching is what a launch that gives up on pod says of it while its workspace-fetch is
// still running, or coming back from one of the restarts the grace covers (fetchRestartGrace):
// so, with the state of the repository cache that fetch fills a fresh feed from (fetchCacheState),
// since the fetch's own failure, which names why it ran so long, has not come. Nothing for a pod
// past its fetch.
func (r *Runtime) stillFetching(pod *corev1.Pod) string {
	status := initContainerStatus(pod, fetchContainer)
	if status == nil {
		return ""
	}
	doing := ""
	switch {
	case status.State.Running != nil:
		doing = " is still running"
	case initEnd(*status) != nil && startingAgain(pod, *status):
		doing = fmt.Sprintf(" is starting again, restarted %d times", status.RestartCount)
	default:
		return ""
	}
	return "; its " + fetchContainer + doing + r.fetchCacheState(pod)
}

// suspendFailedLaunch sets the Sandbox of a launch that failed once its Running patch was sent
// Suspended again. The patch may have been applied even when its answer was an error, and a pod the
// controller made later would run a valid token for a claim the supervisor counts as failed. It is
// not cancelled with the caller, and it is bounded by apiTimeout: it is part of the launch, so it
// waits out an API server answering it overloaded (resend), but no longer than that, so a daemon
// stop never waits a boot timeout on it. A suspension that still fails is logged; the next
// relaunch suspends the Sandbox in any case.
func (r *Runtime) suspendFailedLaunch(ctx context.Context, token claim.Token, s *sandbox) {
	suspending, cancel := call(context.WithoutCancel(ctx))
	defer cancel()
	if err := r.setMode(suspending, s, modeSuspended); err != nil {
		r.log.Error("sandbox runtime: could not suspend the sandbox of a failed launch", "claim", token, "err", err)
	}
}

// ensureSandbox is the claim's Sandbox read from the API, created Suspended when absent, with a
// template the Running patch replaces before any pod exists. One being deleted is waited out
// first, bounded by the boot timeout; one by the claim's name that is not this project's is a
// refusal.
//
// A Sandbox fits a launch when it carries the tree label the launch's pod carries, none for the
// controller's pod. It is made for an issue, whose key, so its Sandbox's name, outlives a move to
// another tree: a child of a closed tree re-admitted as a root of its own finds the Sandbox its old
// tree left, labelled with that tree. Such a Sandbox is relabelled for this launch's tree and
// kept, once no stored claim of the issue in another tree may still run a process there
// (Store.IssueRunsElsewhere); the Running patch rewrites its pod template, labels included, before
// any pod runs. Of the old tree's work, only a kept volume comes with it. An issue pod's own
// ephemeral volume (issuePod.disk) went with that pod, so the relabelled Sandbox's first pod
// brings a disk of its own and provisions the issue's workspace afresh. A kept volume (keptVolume)
// is the Sandbox's own claim, whose template carries no tree label, so it stays with the Sandbox
// and its next pod mounts it. One its old tree left Running — the old tree's close suspended every
// role of it, but the issue's suspension never ran, since the issue had moved — is suspended in the
// same patch, so the relaunch waits its pod out. One a claim of the old tree may still run in is a
// refusal, since replacing it would end that claim. An issue Sandbox of another disk shape than
// the launch renders (supersededDisk) reaches here and is returned as it is: one of the release
// before the pod-owned disk, or one that a change of keep_volume, the issue's sandbox or the
// volume's class, size or access modes left behind. The boot census lets it through and names it
// rather than refusing the daemon (treeVolumeLayout), and the relaunch replaces it, Sandbox and
// claim together, with or after its pod (replaceSupersededSandbox). Nothing here deletes a
// Sandbox. One it creates carries the name of the pod a replacement whose own Create failed left
// stranded (createSandbox), so the roles that replacement ended are read as displaced from it.
func (r *Runtime) ensureSandbox(ctx context.Context, l launch) (*sandbox, error) {
	deadline := time.Now().Add(r.bootTimeout)
	// refused is the relabel the API server refused as invalid, and refusedAt the resourceVersion its
	// fence tested: a Sandbox read again still at that resourceVersion has not changed since, so the
	// refusal was the patch's own, and it is the answer.
	var refused error
	var refusedAt string
	tree := r.labels(l)[labelTree]
	for {
		u, err := r.sandboxClient().Get(ctx, l.name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			created, err := r.createSandbox(ctx, l)
			if apierrors.IsAlreadyExists(err) && time.Now().Before(deadline) {
				continue
			}
			if err != nil {
				return nil, err
			}
			return created, nil
		}
		if err != nil {
			return nil, err
		}
		s, err := decodeSandbox(u)
		if err != nil {
			return nil, err
		}
		if s.Labels[labelProject] != r.project {
			return nil, fmt.Errorf("sandbox %s exists but is not project %s's (%s=%q)", l.name, r.project, labelProject, s.Labels[labelProject])
		}
		if s.DeletionTimestamp == nil {
			if s.Labels[labelTree] == tree {
				return s, nil
			}
			if refused != nil && s.ResourceVersion == refusedAt {
				return nil, fmt.Errorf("relabel sandbox %s for tree %s: %w", l.name, tree, refused)
			}
			// Fenced to the resourceVersion read, beside the uid patch tests: a relabel, a mode change,
			// or a delete begun since that read fails the test (422), and this launch reads the Sandbox
			// again within its deadline; a delete already completed answers 404 and fails the try.
			ops := []jsonPatchOp{
				{Op: "test", Path: "/metadata/resourceVersion", Value: s.ResourceVersion},
				{Op: "add", Path: labelPatchPath(labelTree), Value: tree},
			}
			if s.mode() != modeSuspended {
				running, err := r.store.IssueRunsElsewhere(ctx, r.project, l.spec.Issue, l.spec.Tree)
				if err != nil {
					return nil, fmt.Errorf("sandbox %s, made for tree %s: %w", l.name, s.Labels[labelTree], err)
				}
				if running != "" {
					return nil, fmt.Errorf("sandbox %s, made for tree %s, does not fit this launch of tree %s and still runs roles (%s); it is relabelled once they stop",
						l.name, s.Labels[labelTree], tree, running)
				}
				ops = append(ops, jsonPatchOp{Op: "add", Path: "/spec/operatingMode", Value: modeSuspended})
			}
			r.log.Info("sandbox runtime: relabelling a sandbox for its issue's new tree, keeping the Sandbox",
				"sandbox", l.name, "uid", s.UID, "tree", s.Labels[labelTree], "for", tree, "operatingMode", s.mode())
			relabelled, err := r.patch(ctx, s, ops...)
			if apierrors.IsInvalid(err) && time.Now().Before(deadline) {
				// The fence's test failed: the Sandbox changed since the read, or this patch, sent
				// again after an overloaded answer (resend), was already applied. Read it again and
				// decide from what it holds now; one still at the tested resourceVersion refused the
				// patch itself.
				refused, refusedAt = err, s.ResourceVersion
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("relabel sandbox %s for tree %s: %w", l.name, tree, err)
			}
			return relabelled, nil
		}
		r.log.Info("sandbox runtime: waiting out a sandbox being deleted", "sandbox", l.name, "uid", s.UID)
		gone := func(ctx context.Context) (bool, error) {
			current, err := r.sandboxClient().Get(ctx, l.name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return err == nil && current.GetUID() != s.UID, err
		}
		if err := r.await(ctx, time.Until(deadline), fmt.Sprintf("sandbox %s (uid %s) to be deleted", l.name, s.UID), gone); err != nil {
			return nil, err
		}
	}
}

// sandboxReplacement is why a launch deletes its issue's Sandbox and creates it again
// (replaceSupersededSandbox), as the line that the replacement logs names it. The empty reason is
// no replacement. Each reason is its own text, so a reason that names a part of the pod, such as
// an init container, is a value built from that name.
type sandboxReplacement string

// The reasons that a Sandbox's disk shape gives (diskShapeChange).
const (
	// replacedFromPreviousRelease is an issue Sandbox of the release before the pod-owned disk: it
	// owns an `issue` claim template that carries no kept label (keptTemplate).
	replacedFromPreviousRelease sandboxReplacement = "a Sandbox of the release before the pod-owned disk"
	// replacedToKeepVolume is a Sandbox that owns no claim template, where the launch keeps its
	// volume (keptVolume).
	replacedToKeepVolume sandboxReplacement = "the launch now keeps a volume"
	// replacedToKeepNone is a Sandbox that owns a kept volume, where the launch keeps none.
	replacedToKeepNone sandboxReplacement = "the launch now keeps no volume"
	// replacedForVolumeChange is a Sandbox that owns a kept volume of another storage class, size
	// or access modes than the launch renders.
	replacedForVolumeChange sandboxReplacement = "the volume's class, size or access modes changed"
)

// volumeReset is why a launch resets the kept volume of the issue Sandbox s, whose stored pod is
// pod, or "" when it does not. It resets a Sandbox that owns a kept volume (ownsKeptVolume) when
// pod shows that an init container failed on that volume for a cause local to it (volumeFault), or
// when s carries the reset mark (volumeResetAnnotation). ReplacePod and the release write that mark
// when they take such a pod down, because the launch after them reads no pod. The reason names the
// init container, its exit code and its pod, in the same words as the mark.
func volumeReset(s *sandbox, pod *corev1.Pod) sandboxReplacement {
	fault := volumeFault(s, pod)
	if fault == "" && ownsKeptVolume(s) {
		fault = s.Annotations[volumeResetAnnotation]
	}
	if fault == "" {
		return ""
	}
	return sandboxReplacement("an init container failed on the kept volume: " + fault)
}

// volumeFault is the reset mark (volumeResetAnnotation) for pod on the issue Sandbox s: the init
// container that failed on the kept volume of s for a cause local to that volume
// (localInitFailure), its exit code and the pod's uid. It is "" for a Sandbox that owns no kept
// volume (ownsKeptVolume), for a pod that is not the Sandbox's own, and for a pod that shows no
// such failure.
func volumeFault(s *sandbox, pod *corev1.Pod) string {
	if !ownsKeptVolume(s) || !ownedBy(pod, s.UID) {
		return ""
	}
	container, ended := localInitFailure(pod)
	if ended == nil {
		return ""
	}
	return fmt.Sprintf("%s exited %d in pod %s", container, ended.ExitCode, pod.UID)
}

// supersededDisk is why the issue Sandbox s does not have the disk shape that the launch l
// renders (podKind.disk), by diskShapeChange, or "" when it has that shape. The launch's kind is
// this Sandbox's: ensureSandbox finds it by the claim's own name, and podKindOf reads an issue
// pod's kind from an issue's labels. The project controller's Sandbox owns its `issue` template by
// design: the working root that its launcher runs in outlives its pods, so the claim that holds it
// must outlive them too. So it is never replaced for this reason.
func (r *Runtime) supersededDisk(s *sandbox, l launch) sandboxReplacement {
	if _, issue := l.kind.(issuePod); !issue {
		return ""
	}
	want, _ := l.kind.disk(r, l)
	return diskShapeChange(s.Spec.VolumeClaimTemplates, want)
}

// diskShapeChange is why an issue Sandbox that owns the claim templates held does not have the
// disk shape of want, the templates that its issue's launch renders (issuePod.disk), or "" when it
// has that shape. It is the one comparison of the two: a launch asks it (supersededDisk), and the
// boot census asks it for each issue Sandbox that it accepts (treeVolumeLayout).
//
// A launch that keeps no volume renders no template, so every template held differs. A launch that
// keeps a volume (keptVolume) renders one `issue` template that carries the kept label
// (keptTemplate), the launch's storage class, its size and its access modes. The comparison reads
// those and nothing else (sameClaimTemplate). It reads no other label: the project and issue labels
// of a template are those of its Sandbox. The vendored CRD sets no default under
// volumeClaimTemplates, so a stored template reads back as it was written.
func diskShapeChange(held, want []volumeClaimTemplate) sandboxReplacement {
	if slices.EqualFunc(held, want, sameClaimTemplate) {
		return ""
	}
	kept := slices.ContainsFunc(held, func(t volumeClaimTemplate) bool {
		return keptTemplate(t.Metadata.Name, t.Metadata.Labels[labelVolume])
	})
	switch {
	case len(held) == 0:
		return replacedToKeepVolume
	case !kept:
		return replacedFromPreviousRelease
	case len(want) == 0:
		return replacedToKeepNone
	default:
		return replacedForVolumeChange
	}
}

// sameClaimTemplate is whether the claim templates a and b declare the same disk: the same name,
// the same kept label (labelVolume), the same storage class as text, the same requested size by
// value (resource.Quantity.Cmp, so 20480Mi is 20Gi), and the same access modes as a set.
func sameClaimTemplate(a, b volumeClaimTemplate) bool {
	return a.Metadata.Name == b.Metadata.Name &&
		a.Metadata.Labels[labelVolume] == b.Metadata.Labels[labelVolume] &&
		storageClassOf(a.Spec) == storageClassOf(b.Spec) &&
		a.Spec.Resources.Requests.Storage().Cmp(*b.Spec.Resources.Requests.Storage()) == 0 &&
		sameAccessModes(a.Spec.AccessModes, b.Spec.AccessModes)
}

// storageClassOf is the storage class a claim names, "" for none.
func storageClassOf(spec corev1.PersistentVolumeClaimSpec) string {
	if spec.StorageClassName == nil {
		return ""
	}
	return *spec.StorageClassName
}

// sameAccessModes is whether a and b hold the same access modes, in any order and any number of
// times.
func sameAccessModes(a, b []corev1.PersistentVolumeAccessMode) bool {
	for _, mode := range a {
		if !slices.Contains(b, mode) {
			return false
		}
	}
	for _, mode := range b {
		if !slices.Contains(a, mode) {
			return false
		}
	}
	return true
}

// replaceSupersededSandbox deletes the issue Sandbox s, for reason, and creates in its place one
// of the disk shape that the launch renders (sandboxManifest: Suspended, owning the kept `issue`
// template where the launch keeps its volume and no template otherwise), which it returns for the
// rest of the launch to run against. reason is a change of the disk shape (supersededDisk) or the
// reset of a kept volume (volumeReset). spec.volumeClaimTemplates is immutable in the CRD, so no
// patch changes the templates, and delete-and-create is the only way to another shape or to a new,
// empty volume. The launch holds the pod's launch turn and has already waited the Sandbox's pod
// out, so no process of the issue runs while its Sandbox is away. The delete is the foreground one
// that every Sandbox delete is (deleteSandbox), so garbage collection deletes the claim the old
// Sandbox owned, if any, and the role Secrets it owned, before the Sandbox is gone. That claim
// holds the issue's whole workspace, which the next pod provisions again. That is why this runs
// before provision and writeLauncherSecrets and not after: the provisioning Secret and every role
// Secret of this launch are written against the new Sandbox and owned by it, where ones written
// first would be deleted with the old one. It logs one line that names the issue, the Sandbox, the
// old uid, the claim of the issue's volume (IssueClaimName) and reason.
//
// The one piece of the predecessor's metadata that travels is the pod it names as displaced
// (displacedAnnotation): the five roles of the pod this launch just replaced are read as ended by
// the daemon rather than dead only while their Sandbox names that pod (observe's names), and at
// the cutover from the release before the pod-owned disk every running issue's launch comes
// through here. Once the foreground delete's garbage collection is done and the object is gone,
// no Sandbox of the name exists to name it until the create lands, and the observer, which takes
// no launch turn, reads a sibling role's end in that gap as readily as after
// it — a gap of a confirming read and a create when all goes well, and one a failed Create holds
// open until the issue's next launch makes the Sandbox again. So the name is put in the runtime's
// own keeping (stranded) before the delete is sent — the Sandbox carries it until it is gone, so no
// instant is uncovered — and createSandbox sets it on the Sandbox it creates and drops it once the
// API server holds that one. It is taken from s, not from this launch's suspension alone: a
// launch that suspended the pod and failed before the delete left the name on the Sandbox, and the
// next launch, finding the pod already gone, replaces that Sandbox without suspending anything. A
// round's suspension (ReplacePod) leaves the name on the Sandbox in the same way, and the launch
// after the round finds no pod to suspend.
// A daemon that restarts inside the gap has neither, and reads those roles' ends as deaths. No
// other annotation travels, the reset mark (volumeResetAnnotation) among them, so the Sandbox that
// a reset creates is not reset again for the pod that failed on the volume it replaced.
func (r *Runtime) replaceSupersededSandbox(ctx context.Context, l launch, s *sandbox, reason sandboxReplacement) (*sandbox, error) {
	r.log.Info("sandbox runtime: replacing the issue's Sandbox, with any volume claim it owns",
		"issue", l.spec.Issue, "sandbox", l.name, "uid", s.UID, "claim", IssueClaimName(l.spec.Claim), "reason", string(reason))
	if named := s.Annotations[displacedAnnotation]; named != "" {
		r.strand(l.name, named)
	}
	if err := r.deleteStoredSandbox(ctx, l.name); err != nil {
		return nil, err
	}
	created, err := r.createSandbox(ctx, l)
	if err != nil {
		return nil, fmt.Errorf("create sandbox %s: %w", l.name, err)
	}
	return created, nil
}

// createSandbox creates l's Sandbox as sandboxManifest makes it, naming on it the pod held for its
// name as displaced (stranded), and drops that record once the API server holds a Sandbox of the
// name: from then the object answers, where a restarted daemon reads it too, and the record would
// only outlive the gap it was kept for. A Create answered AlreadyExists drops it as well — a resend
// of one the server applied, under the launch turn nothing else of this daemon creates the name —
// since evaluate never reads the record while a Sandbox exists, and one kept past that would be set
// on whatever Sandbox the name is next created as, after an issue's close and re-admission, naming
// a pod that Sandbox never ran. Any other failure keeps it: the name may still hold no Sandbox, and
// the roles the replacement ended are read as displaced until one exists. sandboxManifest carries
// no annotations of its own, so this sets the one key rather than the manifest threading it. So a
// Sandbox that this creates carries no reset mark (volumeResetAnnotation).
func (r *Runtime) createSandbox(ctx context.Context, l launch) (*sandbox, error) {
	manifest := r.sandboxManifest(l)
	held, _ := r.strandedPod(l.name)
	if held != "" {
		manifest.Annotations = map[string]string{displacedAnnotation: held}
	}
	object, err := encodeSandbox(manifest)
	if err != nil {
		return nil, err
	}
	created, err := r.sandboxClient().Create(ctx, object, metav1.CreateOptions{})
	if held != "" && (err == nil || apierrors.IsAlreadyExists(err)) {
		r.unstrand(l.name)
	}
	if err != nil {
		return nil, err
	}
	return decodeSandbox(created)
}

// strand keeps uid as the pod the Sandbox name names as displaced while that Sandbox is away
// (stranded), moving the record's generation (strandedGen).
func (r *Runtime) strand(name, uid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stranded[name] = uid
	r.strandedGen++
}

// strandedPod is the pod kept for the Sandbox name as displaced (stranded), "" when none is,
// beside the record's generation as of that read (strandedGen).
func (r *Runtime) strandedPod(name string) (string, uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stranded[name], r.strandedGen
}

// unstrand drops what is kept for the Sandbox name (stranded), moving the record's generation
// (strandedGen).
func (r *Runtime) unstrand(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stranded, name)
	r.strandedGen++
}

// awaitPodGone waits, bounded by the boot timeout, until no pod the Sandbox owns holds its name —
// the store first, then one GET to confirm, since a store that has not yet seen a pod proves
// nothing about it (decision 2). It returns every uid it saw holding the name, none of which is
// the incarnation the relaunch will return.
func (r *Runtime) awaitPodGone(ctx context.Context, s *sandbox) (map[types.UID]bool, error) {
	old := map[types.UID]bool{}
	gone := func(ctx context.Context) (bool, error) {
		if pod := r.storedPod(s.Name); ownedBy(pod, s.UID) {
			old[pod.UID] = true
			return false, nil
		}
		pod, err := r.kube.CoreV1().Pods(r.namespace).Get(ctx, s.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if ownedBy(pod, s.UID) {
			old[pod.UID] = true
			return false, nil
		}
		return true, nil
	}
	err := r.await(ctx, r.bootTimeout, fmt.Sprintf("the pod of sandbox %s to be gone", s.Name), gone)
	return old, err
}

// awaitNewPod waits, bounded by the boot timeout, for a pod the Sandbox owns that is none of old
// and is not being deleted: the pod the Running patch made. s is the Sandbox as that patch left
// it, and the wait also holds until the Sandbox store has reached its generation (the API server
// bumps it on every spec write) and shows it Running, so nothing that reads the stores right
// after the launch — a Probe, a Suspend — sees its pod and not its Sandbox, or any copy from before
// the patch: the Suspended one it was created as, or, for a relaunch over a Running Sandbox, the
// Running one from before the relaunch.
func (r *Runtime) awaitNewPod(ctx context.Context, s *sandbox, old map[types.UID]bool) (*corev1.Pod, error) {
	var found *corev1.Pod
	what := fmt.Sprintf("a new pod of sandbox %s and its Running patch in the Sandbox store", s.Name)
	err := r.await(ctx, r.bootTimeout, what, func(context.Context) (bool, error) {
		pod := r.storedPod(s.Name)
		if !ownedBy(pod, s.UID) || old[pod.UID] || pod.DeletionTimestamp != nil {
			return false, nil
		}
		stored, err := r.storedSandbox(s.Name)
		if err != nil || stored == nil || stored.UID != s.UID {
			return false, err
		}
		if stored.Generation < s.Generation || stored.mode() != modeRunning {
			return false, nil
		}
		found = pod
		return true, nil
	})
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w; %s", err, r.waitedOut(ctx, s))
	}
	return found, err
}

// waitedOut is what the daemon's own stores say about a launch that ran out of time: the Sandbox's
// Ready condition, where the controller reports why it made no pod, and the pod's phase where one
// exists and the wait was for something else. A pod still Pending carries its events too, as an
// unschedulable pod's verdict does (observe's evaluate): a pod whose disk the kubelet cannot
// provision — one waiting behind a predecessor's claim the garbage collector has not reaped —
// reports nothing but Pending, and the FailedBinding event is the whole of the reason. A timeout
// that says only how long it waited sends an operator to the cluster for a condition the daemon
// was already holding.
func (r *Runtime) waitedOut(ctx context.Context, s *sandbox) string {
	stored, err := r.storedSandbox(s.Name)
	switch {
	case err != nil:
		return fmt.Sprintf("the Sandbox store could not be read for %s: %v", s.Name, err)
	case stored == nil:
		return fmt.Sprintf("the Sandbox store holds no %s", s.Name)
	}
	detail := fmt.Sprintf("sandbox %s is %s at generation %d", s.Name, stored.mode(), stored.Generation)
	if ready := stored.condition(conditionReady); ready != nil {
		detail += fmt.Sprintf("; Ready=%s %s: %s", ready.Status, ready.Reason, ready.Message)
	}
	pod := r.storedPod(s.Name)
	if !ownedBy(pod, s.UID) {
		return detail + "; it has no pod"
	}
	detail += fmt.Sprintf("; its pod %s is %s", pod.UID, phaseOf(pod))
	if pod.Status.Phase == corev1.PodPending {
		detail += fmt.Sprintf("; events: %s", r.events(ctx, pod))
	}
	return detail
}

// writeLauncherSecrets makes, for each launcher role of the pod (l.roles), a role-private Secret
// holding a fresh launcher token. It runs only before a new pod starts, so every pod's launchers
// authenticate with tokens no earlier pod held; a role's launch credentials travel in its
// launcher's start command instead (launcherCommand). The Secret also carries the role's GitHub
// credential, its gh bundle (gitHubBundles): each directory of that role's own bundle as its
// hosts.yml and config.yml (GitHubHostsKey, GitHubConfigKey), the bundle's configured renames
// (GitHubPairsKey, empty where the project has none that resolve) and the default owner
// (GitHubDefaultKey), which its container projects at GHConfigDir.
//
// The Secret holds the role's own directories, where the pod projects only the ones every role
// holds (l.gitHubDirs, gitHubBundles): a key outside that projection is written and never read,
// and the refresher keeps it current with the rest (refreshGitHubCredential), so the role's next
// pod projects it the moment every role's bundle holds it.
func (r *Runtime) writeLauncherSecrets(ctx context.Context, s *sandbox, l *launch) error {
	bundles, err := r.gitHubBundles(ctx, l)
	if err != nil {
		return err
	}
	for _, role := range l.roles {
		token, err := launcherToken()
		if err != nil {
			return err
		}
		bundle := bundles[role]
		data := map[string][]byte{
			LauncherTokenFile: []byte(token),
			GitHubDefaultKey:  []byte(bundle.Default),
			GitHubPairsKey:    []byte(ghconfig.RenderPairs(bundle.Pairs)),
		}
		for _, dir := range bundle.DirNames() {
			rendered := bundle.Dirs[dir]
			data[GitHubHostsKey(dir)], data[GitHubConfigKey(dir)] = []byte(rendered.Hosts), []byte(rendered.Config)
		}
		if err := r.upsertSecret(ctx, corev1.Secret{
			ObjectMeta: r.secretMeta(s, *l, roleSecretName(s.Name, role)),
			Type:       corev1.SecretTypeOpaque,
			Data:       data,
		}); err != nil {
			return err
		}
		if bundle.Default == "" {
			r.log.Info("sandbox runtime: no GitHub owner to act for; the role's gh acts as nobody", "sandbox", s.Name, "role", role)
			continue
		}
		written := bundle.Dirs[bundle.Default]
		r.log.Info("sandbox runtime: github credential written", "sandbox", s.Name, "role", role, "app", written.App,
			"expiresAt", written.ExpiresAt, "dirs", l.gitHubDirs)
	}
	return nil
}

// gitHubBundles mints each launcher role's gh bundle for the issue's repository
// (Options.GitHubCredential) and records on l the directories the pod projects and the bundles'
// default, which the pod's gh volumes and every agent's GH_CONFIG_DIR are built from. A mint
// that fails, a bundle Validate refuses, and a role whose bundle names another default fail the
// launch, naming the role, since a pod whose gh holds no token would start every role unable to
// reach GitHub. The one exception is the controller of a project with no GitHub owner to mint
// for (runtime.ErrNoGitHubOwner, every mapped repository without an App): it runs with gh acting
// as nobody, as `legion controller start` does there, its bundle empty — no directory, no
// default — and its gh directory an empty one (volumes).
//
// The pod projects the directories every role's bundle holds, and no more (ghDirsShared): a
// projection item naming a key the role's Secret lacks stops the pod from starting, and the six
// roles of an issue pod run as two Apps whose installations answer for themselves, so one App's
// installation not reaching `acme/x` leaves acme out of that App's bundles alone (appauth's
// mintBundle). A role whose bundle holds a directory outside that set simply does not project
// it; its Secret keeps the key, unused (writeLauncherSecrets), and the launch says once which
// role lacked which directory. The launch fails where the set the roles share does not hold the
// bundles' default, an empty set included — GH_CONFIG_DIR would then name a directory the pod
// does not carry — and where a role's own default is not the pod's, which would leave that
// role's Secret recording a directory its container never reads from (refreshGitHubCredential).
// A lease GitHub refuses costs that owner's directory alone (appauth's mintBundle). The bundle
// fails only where the directory left out is the one it was asked for, the default that
// GH_CONFIG_DIR names (appauth's bundleDefault). Every role that reaches here therefore holds
// the directory it works its issue's repository in.
//
// The controller's bundle is asked for by owner (Options.ControllerCredential), the one function
// a controller's bundle is made from here, by this launch and by the refresh of the pod it makes
// (refreshGitHubCredential). The launch asks for no owner, which takes the first DIRECTORY of the
// bundle, sorted (appauth's bundleDefault), and records it as the pod's default. That is usually
// an owner the review App is installed on that the mint answered for, but a configured rename
// also puts the pair's `from` owner in the bundle as an alias directory (bundleDirs), and such a
// name can sort first and be taken; a controller works no repository, so its every directory is
// read-only.
//
// The launch's roles run as two Apps and work the one repository, and a bundle is that pair and
// nothing else, so the six roles of an issue pod are two bundles built once each (appauth.Bundles):
// the launch opens with two live reads of the Apps' installations rather than six serial round
// trips to GitHub before its first role starts.
func (r *Runtime) gitHubBundles(ctx context.Context, l *launch) (map[claim.Role]ghconfig.Bundle, error) {
	l.gitHubDirs, l.gitHubDefault = nil, ""
	bundles := make(map[claim.Role]ghconfig.Bundle, len(l.roles))
	held := make(map[claim.Role][]string, len(l.roles))
	memo := appauth.NewBundles(r.gitHubCredential)
	for i, role := range l.roles {
		minting, cancel := call(ctx)
		var bundle ghconfig.Bundle
		var err error
		if role == claim.RoleController {
			bundle, err = r.controllerCredential(minting, "")
		} else {
			bundle, err = memo.Bundle(minting, role, l.spec.Repository)
		}
		cancel()
		switch {
		case role == claim.RoleController && errors.Is(err, runtime.ErrNoGitHubOwner):
			bundle, err = ghconfig.Bundle{}, nil
		case err == nil:
			err = bundle.Validate()
		}
		if err != nil {
			return nil, fmt.Errorf("write the github credential for %s: %w", role, err)
		}
		if i == 0 {
			l.gitHubDefault = bundle.Default
		}
		bundles[role], held[role] = bundle, bundle.DirNames()
	}
	shared, lacked := ghDirsShared(held)
	if len(lacked) > 0 {
		r.log.Warn("sandbox runtime: the roles' gh bundles hold different directories; the pod projects the ones they share",
			"sandbox", l.name, "lacked", lacked, "projecting", shared)
	}
	if l.gitHubDefault != "" && !slices.Contains(shared, l.gitHubDefault) {
		return nil, fmt.Errorf("write the github credential: the roles' gh bundles share the directories %v, which do not hold the pod's default owner %s",
			shared, l.gitHubDefault)
	}
	for _, role := range l.roles {
		if bundles[role].Default != l.gitHubDefault {
			return nil, fmt.Errorf("write the github credential for %s: its default owner %s is not the pod's %s",
				role, bundles[role].Default, l.gitHubDefault)
		}
	}
	l.gitHubDirs = shared
	return bundles, nil
}

// ghDirsShared is the directories every role's bundle holds, sorted — what a pod of those roles
// may project (volumes) — and lacked, one phrase for each directory some role's bundle does not
// hold, naming the directory and the roles that lack it, for the launch to say once.
func ghDirsShared(held map[claim.Role][]string) (shared, lacked []string) {
	holders := map[string]int{}
	for _, dirs := range held {
		for _, dir := range dirs {
			holders[dir]++
		}
	}
	roles := slices.Sorted(maps.Keys(held))
	for _, dir := range slices.Sorted(maps.Keys(holders)) {
		if holders[dir] == len(held) {
			shared = append(shared, dir)
			continue
		}
		without := make([]string, 0, len(roles))
		for _, role := range roles {
			if !slices.Contains(held[role], dir) {
				without = append(without, string(role))
			}
		}
		lacked = append(lacked, dir+" ("+strings.Join(without, ", ")+")")
	}
	return shared, lacked
}

// secretMeta is a pod's Secret's metadata: the pod's labels, the Sandbox as owner, and the
// annotations its kind gives every Secret of the pod (podKind.secretAnnotations).
func (r *Runtime) secretMeta(s *sandbox, l launch, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name: name, Namespace: r.namespace, Labels: r.labels(l), Annotations: l.kind.secretAnnotations(l),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: sandboxGVR.GroupVersion().String(), Kind: "Sandbox", Name: s.Name, UID: s.UID,
		}},
	}
}

// upsertSecret writes want, creating it, updating the Sandbox's own Secret by its name, or
// replacing one that is not the Sandbox's (writeSecret). A create refused as already done, or an
// update refused as a conflict, is read and written once more: a launch's write the server applied
// and answered overloaded is sent again (resend) and refused that way, so what it left is read
// before the launch goes on from it, as bindLauncherSecrets reads again after a conflict.
func (r *Runtime) upsertSecret(ctx context.Context, want corev1.Secret) error {
	err := r.writeSecret(ctx, want)
	if apierrors.IsAlreadyExists(err) || apierrors.IsConflict(err) {
		err = r.writeSecret(ctx, want)
	}
	return err
}

func (r *Runtime) writeSecret(ctx context.Context, want corev1.Secret) error {
	secrets := r.kube.CoreV1().Secrets(r.namespace)
	existing, err := secrets.Get(ctx, want.Name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = secrets.Create(ctx, &want, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	case ownedBySandbox(existing.OwnerReferences, want.OwnerReferences[0].UID):
		existing.Labels, existing.OwnerReferences, existing.Data, existing.StringData = want.Labels, want.OwnerReferences, want.Data, nil
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		for key, value := range want.Annotations {
			existing.Annotations[key] = value
		}
		_, err = secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	default:
		uid := existing.UID
		if err := secrets.Delete(ctx, want.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil &&
			!apierrors.IsNotFound(err) {
			return err
		}
		_, err = secrets.Create(ctx, &want, metav1.CreateOptions{})
		return err
	}
}

func launcherToken() (string, error) {
	var bytes [32]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("mint launcher token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}

func ownedBySandbox(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, ref := range refs {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// lockPod takes a pod's launch turn, keyed by its Sandbox's name, an issue pod's and the project
// controller's alike: one relaunch, issue suspension, release or orphan deletion of a pod at a time
// holds it. Role starts in a healthy pod share it only while they select a launcher command; the
// pods of one tree's issues take turns of their own, never one another's.
func (r *Runtime) lockPod(ctx context.Context, sandbox string) (func(), error) {
	return r.takeTurn(ctx, r.podTurns, sandbox)
}

// takeTurn waits for key's turn in turns, a map r.mu guards, until ctx ends; the returned func
// gives the turn back.
func (r *Runtime) takeTurn(ctx context.Context, turns map[string]chan struct{}, key string) (func(), error) {
	r.mu.Lock()
	turn, ok := turns[key]
	if !ok {
		turn = make(chan struct{}, 1)
		turns[key] = turn
	}
	r.mu.Unlock()
	select {
	case turn <- struct{}{}:
		return func() { <-turn }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// jsonPatchOp is one RFC 6902 operation.
type jsonPatchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// labelPatchEscape escapes a label key as an RFC 6901 JSON pointer token: `~` as `~0`, `/` as `~1`.
var labelPatchEscape = strings.NewReplacer("~", "~0", "/", "~1")

// labelPatchPath is the JSON pointer of one of a Sandbox's labels, its key escaped as RFC 6901
// requires (`~` as `~0`, then `/` as `~1`): an `add` there sets the label, replacing its value.
func labelPatchPath(key string) string {
	return "/metadata/labels/" + labelPatchEscape.Replace(key)
}

// displacedAnnotation names, on a Sandbox, the uid of the last live pod of it the daemon replaced
// (relaunch). A process recorded in that pod ended by the daemon's own act once the pod is going
// (evaluate's displaced). It lives with the Sandbox, so a restarted daemon reads it as well. Both
// acts that replace a live pod write it: the roles of a pod a launch replaced are the daemon's own
// ends as much as the roles of one an update round replaced.
//
// roundDisplacedAnnotation names the pod a Sandbox was suspended to replace for its update round,
// and only ReplacePod writes it. It is what tells the round's own replacement, in flight between
// its suspension and the pod's end, from a launch's replacement of the same pod, which takes the
// pod's disk with whatever its workers have not pushed: the two are the same suspension naming the
// same pod under displacedAnnotation, and only the round's may be read as a pod that is there and
// has not drifted (PodDrift).
const (
	displacedAnnotation      = "legion.dev/displaced-pod"
	roundDisplacedAnnotation = "legion.dev/round-displaced-pod"
)

// endedPodAnnotation names, on a Sandbox, the uid of the last pod of it a launch replaced after
// that pod had ended with a reason of its own (relaunch's suspendRecordingEnd), and
// endedPodEndAnnotation holds that end as runtime.PodEnd builds it: the pod's reason and the
// kubelet's message, bounded. The two are written in one patch and read together (endedPod). An
// eviction ends every role of the pod, and the roles' checks run one after another, so a sibling's
// launch can replace the evicted pod before a role's check has read it; the pod's own reason goes
// with the pod, and this is where every role recorded in it reads that end afterwards. Keyed by
// uid, a record of an earlier pod says nothing of a role recorded in a later one. It lives with the
// Sandbox, so a restarted daemon reads it as well, as it reads displacedAnnotation. A Sandbox a
// migrating launch deletes and creates again (replaceSupersededSandbox) does not carry it over.
const (
	endedPodAnnotation    = "legion.dev/ended-pod"
	endedPodEndAnnotation = "legion.dev/ended-pod-end"
)

// replacementKind is which of the two acts that replace a live pod a suspension is made for: a
// launch that cannot use the pod it found, or the update round of that pod, once it is over.
type replacementKind bool

const (
	launchReplacement replacementKind = false
	roundReplacement  replacementKind = true
)

// suspendDisplacing sets the Sandbox read as s Suspended and names uid on it as the live pod that
// suspension replaces (displacedAnnotation), in one JSON merge patch: the API server applies both
// or neither, so a refused suspension names no pod, and a merge patch sets the one annotation key
// whatever other annotations the Sandbox carries, or none. A round's replacement names uid under
// the round's own mark in that same write (roundDisplacedAnnotation, roundReplacement), which a
// launch's replacement never writes. It carries s's uid, which the API server refuses to change
// on a live object, so a Sandbox deleted and recreated in between is never written: the fence
// recordAddresses keeps. It returns the Sandbox as the patch left it, the one copy that carries
// the name: a migrating launch reads it from there onto the Sandbox it creates in its place
// (replaceSupersededSandbox).
func (r *Runtime) suspendDisplacing(ctx context.Context, s *sandbox, uid types.UID, kind replacementKind) (*sandbox, error) {
	return r.suspendAnnotating(ctx, s, displacing(uid, kind))
}

// displacing is the annotations of a suspension that replaces the live pod uid for kind
// (suspendDisplacing): uid as the displaced pod (displacedAnnotation), and for a round's
// replacement as the round's too (roundDisplacedAnnotation). ReplacePod adds the reset mark to
// them where the pod failed on a kept volume (volumeResetAnnotation, volumeFault), so that the one
// merge patch carries the mark as well.
func displacing(uid types.UID, kind replacementKind) map[string]string {
	annotations := map[string]string{displacedAnnotation: string(uid)}
	if kind == roundReplacement {
		annotations[roundDisplacedAnnotation] = string(uid)
	}
	return annotations
}

// suspendRecordingEnd sets the Sandbox read as s Suspended to replace pod, which is not live: it
// ended, it is not the Sandbox's, or there is none. A pod of the Sandbox's own whose end carries a
// reason (runtime.PodEnd) has that end recorded in the same JSON merge patch (endedPodAnnotation,
// endedPodEndAnnotation), so the API server applies the suspension and the record together or
// neither; any other pod is suspended over as setMode does, recording nothing.
func (r *Runtime) suspendRecordingEnd(ctx context.Context, s *sandbox, pod *corev1.Pod) error {
	end := ""
	if ownedBy(pod, s.UID) {
		end = runtime.PodEnd(pod.Status.Reason, pod.Status.Message)
	}
	if end == "" {
		return r.setMode(ctx, s, modeSuspended)
	}
	_, err := r.suspendAnnotating(ctx, s, map[string]string{endedPodAnnotation: string(pod.UID), endedPodEndAnnotation: end})
	return err
}

// suspendAnnotating sets the Sandbox read as s Suspended and sets each of annotations on it, in
// one JSON merge patch: the API server applies both or neither, and a merge patch sets those keys
// whatever other annotations the Sandbox carries, or none. It carries s's uid, which the API
// server refuses to change on a live object, so a Sandbox deleted and recreated in between is
// never written: the fence recordAddresses keeps. It returns the Sandbox as the patch left it.
func (r *Runtime) suspendAnnotating(ctx context.Context, s *sandbox, annotations map[string]string) (*sandbox, error) {
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"uid": string(s.UID), "annotations": annotations},
		"spec":     map[string]any{"operatingMode": modeSuspended},
	})
	if err != nil {
		return nil, err
	}
	patched, err := r.sandboxClient().Patch(ctx, s.Name, types.MergePatchType, body, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return decodeSandbox(patched)
}

// patch applies ops to the Sandbox read as s, as one JSON patch whose first operation tests s's
// uid, so a Sandbox deleted and recreated in between is never written. A JSON patch, not a merge
// patch: `add` replaces the pod template whole, where a merge patch would keep every key of the
// previous template the new one leaves out.
func (r *Runtime) patch(ctx context.Context, s *sandbox, ops ...jsonPatchOp) (*sandbox, error) {
	body, err := json.Marshal(append([]jsonPatchOp{{Op: "test", Path: "/metadata/uid", Value: string(s.UID)}}, ops...))
	if err != nil {
		return nil, err
	}
	u, err := r.sandboxClient().Patch(ctx, s.Name, types.JSONPatchType, body, metav1.PatchOptions{})
	if err != nil {
		return nil, err
	}
	return decodeSandbox(u)
}

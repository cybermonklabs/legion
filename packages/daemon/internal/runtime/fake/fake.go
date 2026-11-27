// Package fake is the runtime boundary as a test double: it records every call, answers from a
// script, and plays observations into the sweep on demand.
//
// It exists because the things the supervisor does are mostly invisible in their return values —
// a suspend, a release, an orphan reconciliation all return only an error — and because the
// interesting orderings (an observation that arrives during a resume, a stale incarnation, a
// duplicated verdict) are not reachable by driving a real tmux server.
//
// A fake ignores its context except where the contract is about the context: only the sweep,
// whose channel closes when the context ends, reads one.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

var (
	_ runtime.Runtime             = (*Runtime)(nil)
	_ runtime.IssuePodReader      = (*Runtime)(nil)
	_ runtime.IssuePodReplacer    = (*Runtime)(nil)
	_ runtime.LaunchSessionNamer  = (*Runtime)(nil)
	_ runtime.WorkspaceWorkReader = (*Runtime)(nil)
	_ runtime.PodWorkspaceReader  = (*Runtime)(nil)
	_ runtime.IssueVolumeReader   = (*Runtime)(nil)
	_ runtime.ActReader           = (*Runtime)(nil)
)

// Call is one method the caller called, with the arguments it called it with. A method that does
// not take a given argument leaves it zero.
type Call struct {
	Method  string
	Spec    runtime.SpawnSpec
	Locator runtime.Locator
	// Previous is Resume's previous incarnation; nil when the caller recorded none.
	Previous *runtime.Locator
	// Released is Release's claim; its Locator is nil for a claim released with no process.
	Released runtime.Known
	// Pod is the claim PodDrift and ReplacePod were called about; its Locator is nil for a claim
	// with no process.
	Pod      runtime.Known
	Known    []runtime.Known
	Grace    time.Duration
	Identity runtime.GitIdentity
	// Tree is CleanupTree's tree.
	Tree string
	// Issue is IssuePod's, WorkspaceWork's and ReplaceIssuePod's issue.
	Issue string
	// PodUID is ReplaceIssuePod's pod, and the pod a launch replaced as its round's own
	// replacement (ReplaceRoundPodAtLaunch, whose Spec is the launch's).
	PodUID string
	// Token is WorkspaceWork's claim.
	Token claim.Token
}

// SpawnResult is one scripted answer to Spawn or Resume.
type SpawnResult struct {
	Locator runtime.Locator
	Err     error
}

// ProbeResult is one scripted answer to Probe: a verdict about the locator probed, or the
// runtime failing to answer at all. The fake fills the observation's locator and time in.
// OutOfMemory is the verdict's own (runtime.Observation.OutOfMemory), for a Gone whose pod ran
// out of memory.
type ProbeResult struct {
	Kind        runtime.ObservationKind
	Detail      string
	OutOfMemory bool
	Err         error
}

// Runtime is the fake. The zero value is not usable; call NewRuntime.
type Runtime struct {
	// InPod is what ProvisionsWorkspaces answers: false, the tmux answer, unless a test sets it.
	InPod bool
	// ProvisionGrace is what ProvisionBound answers: zero, the tmux answer, unless a test sets it.
	ProvisionGrace time.Duration
	// TryBound is what LaunchBound answers: zero, no bound stated, the tmux answer, unless a test
	// sets it. The fake itself never ends a Spawn or Resume at it: a test that wants a try to run
	// out scripts the error.
	TryBound time.Duration
	// Now stamps the observations the fake mints. NewRuntime sets it to time.Now; a test with a
	// clock of its own replaces it before use.
	Now func() time.Time
	// OnResume, when set, runs at the start of every Resume, after the call is recorded, with that
	// call's own context: a test that must act while a launch is still in the runtime blocks here
	// and watches that context for the caller's cancellation. A non-nil answer is the Resume's,
	// and nothing is started.
	OnResume func(ctx context.Context) error
	// OnSpawn, when set, runs at the start of every Spawn, after the call is recorded, with that
	// call's own context: a test that must act while a launch is still in the runtime blocks here
	// and watches that context for the caller's cancellation. A non-nil answer is the Spawn's,
	// nothing is started and no scripted answer is spent.
	OnSpawn func(ctx context.Context) error
	// RefuseLookupByID, when set, stands for a pod whose launcher has not said it can look a
	// session up by its id, on every try: Resume records its call and then answers an error that
	// wraps runtime.ErrSessionLookupUnavailable to a spec that has ResumeSession and no
	// ResumeByAgent, before OnResume and the scripted answers. Neither of those can stand for it,
	// since OnResume does not see the spec and a scripted answer is spent by one call whatever it
	// asked. A test clears it to stand for a pod replaced by one with a newer launcher.
	RefuseLookupByID bool
	// RoundPodEnds, when set, is what Spawn and Resume ask of the live pod a scripted Sandbox
	// locator puts the launch in, as the Sandbox runtime's relaunch asks the daemon's reading of
	// the update round over a live pod it would keep (sandbox.Options.RoundPodEnds). A yes is the
	// round's own replacement, made by the launch: the call is recorded as
	// ReplaceRoundPodAtLaunch, the pod is replaced, and the launch answers a locator in a new pod
	// (launchInto). An error is the launch's. A test binds it to the daemon's own reading, as the
	// daemon binds the Sandbox runtime's. nil keeps every pod.
	RoundPodEnds runtime.RoundPodEnds

	mu           sync.Mutex
	pending      *sync.Cond
	calls        []Call
	spawns       []SpawnResult
	resumes      []SpawnResult
	probes       []ProbeResult
	failures     map[string]error
	drift        map[claim.Token][]string
	gone         map[claim.Token]bool
	issuePods    map[string]string
	keptVolumes  map[string]time.Time
	sessions     map[claim.Token]string
	work         map[string]runtime.WorkspaceWork
	holdReplace  func()
	replaceNoPod bool
	holdIssue    func()
	// suspendedOver is every pod whose Sandbox a replacement's suspension landed on, as the
	// Sandbox runtime leaves it whether or not the pod is still there to see: a ReplacePod or a
	// ReplaceIssuePod of such a pod replaces nothing and answers nil, as that runtime answers a
	// suspended Sandbox (FailReplacePodAfterItsSuspension, ScriptSuspendedOver).
	suspendedOver map[string]bool
	// onRead runs at the start of every WorkspaceWork and PodWorkspaceWork, with no lock of the
	// fake held (OnWorkspaceWork).
	onRead func()
	// took is every pod a ReplacePod replaced, which ReadAct answers taken from then on, as the
	// Sandbox runtime answers a pod its replacement's suspension took. ReplacePod and
	// ReplaceIssuePod read nothing of it, so a test that replaces the same pod again is answered as
	// before.
	took map[string]bool
	// roundTook is every pod a round's own replacement suspended: a ReplacePod that replaced it or
	// whose suspension landed before its error, a ReplaceIssuePod that replaced it, and a launch
	// whose RoundPodEnds answered yes (launchInto). PodDrift answers every claim recorded in such a
	// pod drifted from nothing, whichever claim or issue the replacement was made through, as the
	// Sandbox runtime reads the drift of every claim of a pod off its one Sandbox, which the round's
	// replacement marked as its own (drift.go, PodDrift; relaunch.go, roundReplacement).
	roundTook map[string]bool
	// replaced is every pod a replacement by issue (ReplaceIssuePod) or a launch (RoundPodEnds)
	// replaced, by uid, with the uid of the pod a launch built in its place, "" while none has:
	// the issue's Sandbox no longer holds it, as IssuePod then answers, and a launch scripted into
	// it comes up in the pod in its place, as the Sandbox runtime starts a launch into a suspended
	// Sandbox in a new pod (podNow, launchInto).
	replaced map[string]string
	// built counts the pods launches built in the place of replaced ones (buildIn).
	built int
	// sandboxGone is every Sandbox uid whose delete the API server has confirmed
	// (ScriptSandboxGone): an act that takes the volume such a Sandbox owned is made.
	sandboxGone map[string]bool
	// turn is the pods' launch turn, which the Sandbox runtime takes per pod (lockPod): a launch
	// reads and replaces the pod it goes into under it (launchInto), and a replacement by issue
	// asks its last word and replaces under it (ReplaceIssuePod), so no two of them interleave.
	// The fake takes one turn for every pod, and never while it holds mu.
	turn       sync.Mutex
	holdResume func()
	holdProbe  func(runtime.Locator)
	queue      []runtime.Observation
	minted     int
}

// NewRuntime is a fake ready to record: no script, controller launched by the daemon, real time.
func NewRuntime() *Runtime {
	fake := &Runtime{
		Now:         time.Now,
		failures:    map[string]error{},
		drift:       map[claim.Token][]string{},
		gone:        map[claim.Token]bool{},
		issuePods:   map[string]string{},
		keptVolumes: map[string]time.Time{},
		sessions:    map[claim.Token]string{},
		work:        map[string]runtime.WorkspaceWork{},
		replaced:    map[string]string{},
		// suspendedOver, took and roundTook start empty: no replacement has landed on any pod.
		suspendedOver: map[string]bool{},
		took:          map[string]bool{},
		roundTook:     map[string]bool{},
		sandboxGone:   map[string]bool{},
	}
	fake.pending = sync.NewCond(&fake.mu)
	return fake
}

// ScriptSpawn queues answers to Spawn, one per call, in order. Spawn mints its own locator once
// the script runs out.
func (r *Runtime) ScriptSpawn(results ...SpawnResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spawns = append(r.spawns, results...)
}

// ScriptResume queues answers to Resume, one per call, in order.
func (r *Runtime) ScriptResume(results ...SpawnResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resumes = append(r.resumes, results...)
}

// ScriptProbe queues answers to Probe, one per call, in order. An unscripted probe answers
// `Alive`: the uninteresting case stays out of the test.
func (r *Runtime) ScriptProbe(results ...ProbeResult) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probes = append(r.probes, results...)
}

// Emit hands the sweep observations. They are queued, so a test may emit before `Observe` is
// ever called; one sweep consumes them, in the order they were emitted.
func (r *Runtime) Emit(observations ...runtime.Observation) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, observation := range observations {
		if observation.At.IsZero() {
			observation.At = r.Now()
		}
		r.queue = append(r.queue, observation)
	}
	r.pending.Broadcast()
}

// FailSuspend makes every later Suspend return err.
func (r *Runtime) FailSuspend(err error) { r.fail("Suspend", err) }

// FailRelease makes every later Release return err.
func (r *Runtime) FailRelease(err error) { r.fail("Release", err) }

// FailReleaseOf makes every later Release of token's claim return err. nil clears this claim's
// failure; a FailRelease still applies to it.
func (r *Runtime) FailReleaseOf(token claim.Token, err error) { r.fail("Release:"+string(token), err) }

// FailObserve makes every later Observe return err instead of a sweep.
func (r *Runtime) FailObserve(err error) { r.fail("Observe", err) }

// FailReconcileOrphans makes every later ReconcileOrphans return err.
func (r *Runtime) FailReconcileOrphans(err error) { r.fail("ReconcileOrphans", err) }

// FailAdoptWorkingCopy makes every later AdoptWorkingCopy return err.
func (r *Runtime) FailAdoptWorkingCopy(err error) { r.fail("AdoptWorkingCopy", err) }

// FailCleanupTree makes every later CleanupTree return err; nil clears it.
func (r *Runtime) FailCleanupTree(err error) { r.fail("CleanupTree", err) }

// ScriptPodDrift makes every later PodDrift of token's claim answer reasons, as the Sandbox
// runtime answers a pod built from a configuration the runtime has since moved on from, until a
// round's own replacement takes the claim's recorded pod (roundTook, PodDrift). A claim no call
// named has drifted from nothing, which is the uninteresting case; nil clears this claim's answer.
func (r *Runtime) ScriptPodDrift(token claim.Token, reasons ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if reasons == nil {
		delete(r.drift, token)
		return
	}
	r.drift[token] = reasons
}

// ScriptPodGone makes every later PodDrift of token's claim answer runtime.ErrPodGone, as a
// runtime answers a claim whose recorded pod is not there at all: the pod was replaced where it
// stood for a failure of its own and its disk went with it, or the round's own replacement has
// already ended it. gone false puts the claim back to the drift it was scripted, which is what a
// claim running in a pod the runtime can read again answers.
func (r *Runtime) ScriptPodGone(token claim.Token, gone bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !gone {
		delete(r.gone, token)
		return
	}
	r.gone[token] = true
}

// ScriptIssuePod makes every later IssuePod of issue answer pod, as the Sandbox runtime answers
// the uid of the pod the issue's Sandbox holds now: "" is a Sandbox that holds no pod, or no
// Sandbox at all. An issue no call named is one the fake cannot read, which is the uninteresting
// case for every round whose claims record a pod and the conservative one for a round whose
// claims record none: such a round waits, as it does where a runtime cannot say. A pod the fake
// replaced (ReplaceIssuePod, RoundPodEnds) is answered as no pod whatever this scripted, as the
// Sandbox holds no pod once that replacement is made, until a launch builds a new one, which is
// answered from then on (podNow).
func (r *Runtime) ScriptIssuePod(issue, pod string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issuePods[issue] = pod
}

// ScriptIssuePodUnread makes every later IssuePod of issue answer read false, as the Sandbox
// runtime answers an issue whose Sandbox it cannot read, until a ScriptIssuePod names it again.
func (r *Runtime) ScriptIssuePodUnread(issue string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.issuePods, issue)
}

// ScriptKeptVolume makes every later IssueKeepsVolume of issue answer as the Sandbox runtime
// answers an issue Sandbox that owns a kept volume, carries no reset mark, has the disk shape that
// the configuration renders and was made at created: true for a since whose second is after
// created, false for any other. A zero created clears it, which is also how a test scripts a
// Sandbox that the next launch replaces. An issue no call named keeps no volume, which is every
// pod's case but a microVM pod's under keep_volume.
func (r *Runtime) ScriptKeptVolume(issue string, created time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if created.IsZero() {
		delete(r.keptVolumes, issue)
		return
	}
	r.keptVolumes[issue] = created
}

// ScriptLaunchSession makes every later LaunchSession of token's claim answer session, as the
// Sandbox runtime answers the session it chose for a Claude Code role's launch, and for an Oh My
// Pi role's launch whose pod trades an exchange identity. A claim no call named has no session the
// runtime chose, which is the case of an Oh My Pi launch that named no id (its agent mints one and
// reports it at its registration); "" clears it.
func (r *Runtime) ScriptLaunchSession(token claim.Token, session string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if session == "" {
		delete(r.sessions, token)
		return
	}
	r.sessions[token] = session
}

// LaunchSession answers what ScriptLaunchSession set for loc's claim, whatever incarnation loc
// records.
func (r *Runtime) LaunchSession(loc runtime.Locator) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	session, named := r.sessions[loc.Claim]
	return session, named
}

// ScriptWorkspaceWork makes every later WorkspaceWork and PodWorkspaceWork of issue answer work, as
// the Sandbox runtime answers what the shim of a claim in the issue's pod, or the pod's launcher,
// read: the claims and the launcher of a pod read the one workspace. An issue no call named holds
// no work, which is the uninteresting case.
func (r *Runtime) ScriptWorkspaceWork(issue string, work runtime.WorkspaceWork) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.work[issue] = work
}

// FailWorkspaceWork makes every later WorkspaceWork and PodWorkspaceWork of issue return err, as
// the Sandbox runtime answers a read that no process of the pod could make or that the shim or the
// launcher refused. nil clears it, and the issue's scripted reading answers again.
func (r *Runtime) FailWorkspaceWork(issue string, err error) { r.fail("WorkspaceWork:"+issue, err) }

// WorkspaceWork records its call and answers what FailWorkspaceWork set for the issue, else what
// ScriptWorkspaceWork set, else no work.
func (r *Runtime) WorkspaceWork(_ context.Context, issue string, token claim.Token) (runtime.WorkspaceWork, error) {
	r.reading()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "WorkspaceWork", Issue: issue, Token: token})
	return r.workspaceWork(issue)
}

// OnWorkspaceWork makes every later WorkspaceWork and PodWorkspaceWork run during first, before it
// answers, with no lock of the fake held, so during may drive the daemon and the fake again: a test
// starts a turn of a claim there, as a turn can start while the read runs in the pod. nil clears
// it.
func (r *Runtime) OnWorkspaceWork(during func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRead = during
}

// reading runs what OnWorkspaceWork set, if anything, with no lock of the fake held.
func (r *Runtime) reading() {
	r.mu.Lock()
	during := r.onRead
	r.mu.Unlock()
	if during != nil {
		during()
	}
}

// PodWorkspaceWork is the read the Sandbox runtime has the issue pod's launcher make: it records
// its call, with no claim, and answers as WorkspaceWork does, naming the pod the issue's Sandbox
// holds at the call (ScriptIssuePod, followed through the replacements the fake made), as the
// Sandbox runtime names the pod its launcher read, error or not. An issue no ScriptIssuePod named
// names no pod.
func (r *Runtime) PodWorkspaceWork(_ context.Context, issue string) (runtime.WorkspaceWork, error) {
	r.reading()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "PodWorkspaceWork", Issue: issue})
	pod := ""
	if scripted, ok := r.issuePods[issue]; ok && scripted != "" {
		if now, live := r.podNow(scripted); live {
			pod = now
		}
	}
	work, err := r.workspaceWork(issue)
	work.PodUID = pod
	return work, err
}

// workspaceWork is what a read of issue answers, the caller holding mu.
func (r *Runtime) workspaceWork(issue string) (runtime.WorkspaceWork, error) {
	if err := r.failures["WorkspaceWork:"+issue]; err != nil {
		return runtime.WorkspaceWork{}, err
	}
	work := r.work[issue]
	work.Commits = append([]string(nil), work.Commits...)
	return work, nil
}

// HoldResume makes every later Resume run hold before it answers, which is where the Sandbox
// runtime waits out a pod it is building: the claim's machine holds its own lock for the whole
// call, so a reader that waits on Machine.Claim waits with it. A test holds there to read what
// another part of the daemon sees of a claim mid-relaunch, and then lets the call go on. nil
// clears it; the hook runs with none of the fake's own locks held, so it may drive the fake
// again.
func (r *Runtime) HoldResume(hold func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holdResume = hold
}

// HoldProbe makes every later Probe run hold before it answers, with the locator it probes: the
// claim's machine asks Probe inside a decision (supervise's Machine.probe), so the machine is in
// that decision (Machine.Deciding) for as long as hold runs, and its published view shows the
// claim as the decision found it. A test holds there to read what another part of the daemon sees
// of a claim whose machine is deciding, and then lets the call go on. nil clears it; the hook runs
// with none of the fake's own locks held, so it may drive the fake again.
func (r *Runtime) HoldProbe(hold func(runtime.Locator)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holdProbe = hold
}

func (r *Runtime) fail(method string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failures[method] = err
}

// Calls is every call made so far, in order.
func (r *Runtime) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// CallsOf is every call of one method, in order.
func (r *Runtime) CallsOf(method string) []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	var of []Call
	for _, call := range r.calls {
		if call.Method == method {
			of = append(of, call)
		}
	}
	return of
}

// Methods is the order the calls came in, which is usually the whole assertion.
func (r *Runtime) Methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	methods := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		methods = append(methods, call.Method)
	}
	return methods
}

// Spawn records its call, then runs OnSpawn when a test set one, then answers its script, a
// Sandbox locator in the pod the launch comes up in (launchInto), or a locator of its own once the
// script runs out.
func (r *Runtime) Spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.mu.Lock()
	r.calls = append(r.calls, Call{Method: "Spawn", Spec: spec})
	hook := r.OnSpawn
	r.mu.Unlock()
	if hook != nil {
		if err := hook(ctx); err != nil {
			return runtime.Locator{}, err
		}
	}
	r.mu.Lock()
	next := SpawnResult{}
	if len(r.spawns) > 0 {
		next = r.spawns[0]
		r.spawns = r.spawns[1:]
	} else {
		next.Locator = r.mint(spec.Claim)
	}
	r.mu.Unlock()
	return r.launched(ctx, spec, next)
}

// Resume records its call, then answers runtime.ErrSessionLookupUnavailable to a resume by id
// while RefuseLookupByID is set, then runs OnResume when a test set one, then refuses a previous
// incarnation of another claim, as every runtime does, and answers as Spawn does.
func (r *Runtime) Resume(ctx context.Context, prev *runtime.Locator, spec runtime.SpawnSpec) (runtime.Locator, error) {
	r.mu.Lock()
	hold := r.holdResume
	r.mu.Unlock()
	if hold != nil {
		hold()
	}
	r.mu.Lock()
	call := Call{Method: "Resume", Spec: spec}
	if prev != nil {
		recorded := *prev
		call.Previous = &recorded
	}
	r.calls = append(r.calls, call)
	hook := r.OnResume
	refused := r.RefuseLookupByID && spec.ResumeSession != "" && !spec.ResumeByAgent
	r.mu.Unlock()
	if refused {
		return runtime.Locator{}, fmt.Errorf("resume %s by session %s: the pod's launcher has not said it can look a session up by its id: %w",
			spec.Claim, spec.ResumeSession, runtime.ErrSessionLookupUnavailable)
	}
	if hook != nil {
		if err := hook(ctx); err != nil {
			return runtime.Locator{}, err
		}
	}
	r.mu.Lock()
	if err := (runtime.Known{Claim: spec.Claim, Locator: prev}).Validate(); err != nil {
		r.mu.Unlock()
		return runtime.Locator{}, err
	}
	next := SpawnResult{}
	if len(r.resumes) > 0 {
		next = r.resumes[0]
		r.resumes = r.resumes[1:]
	} else {
		next.Locator = r.mint(spec.Claim)
	}
	r.mu.Unlock()
	return r.launched(ctx, spec, next)
}

// launched is what a launch the script answered with next comes to: next's error as scripted, a
// locator of no pod as it stands, and a Sandbox locator moved to the pod the launch comes up in
// (launchInto).
func (r *Runtime) launched(ctx context.Context, spec runtime.SpawnSpec, next SpawnResult) (runtime.Locator, error) {
	if next.Err != nil || next.Locator.Sandbox == nil {
		return next.Locator, next.Err
	}
	return r.launchInto(ctx, spec, next.Locator)
}

// launchInto is loc moved to the pod a launch into loc's pod comes up in, as the Sandbox runtime's
// relaunch decides it under the pod's launch turn (turn). A pod that was replaced with nothing
// built in its place is a suspended Sandbox, and the launch builds a new pod without asking
// anything. A live pod — the one scripted, or the one a launch built in its place — is asked of
// RoundPodEnds, as the Sandbox runtime asks of a live pod it would keep: a yes records a
// ReplaceRoundPodAtLaunch call and replaces the pod with a new one, as the round's own replacement
// (roundTook: every claim recorded in the old pod reads drifted from nothing), and an error is
// the launch's. A launch that comes up in another pod than the scripted one forgets its claim's
// drift, as ReplacePod does: every pod the fake builds is built from the configuration the
// runtime has now.
// RoundPodEnds runs with mu free, so it may drive the fake again.
func (r *Runtime) launchInto(ctx context.Context, spec runtime.SpawnSpec, loc runtime.Locator) (runtime.Locator, error) {
	r.turn.Lock()
	defer r.turn.Unlock()
	r.mu.Lock()
	pod, live := r.podNow(loc.Sandbox.PodUID)
	if !live {
		pod = r.buildIn(pod)
	}
	ends := r.RoundPodEnds
	r.mu.Unlock()
	if live && ends != nil {
		over, err := ends(ctx, spec.Claim, pod)
		if err != nil {
			return runtime.Locator{}, err
		}
		if over {
			r.mu.Lock()
			r.calls = append(r.calls, Call{Method: "ReplaceRoundPodAtLaunch", Spec: spec, PodUID: pod})
			r.replaced[pod] = ""
			r.roundTook[pod] = true
			pod = r.buildIn(pod)
			r.mu.Unlock()
		}
	}
	if pod == loc.Sandbox.PodUID {
		return loc, nil
	}
	r.mu.Lock()
	delete(r.drift, spec.Claim)
	r.mu.Unlock()
	in := *loc.Sandbox
	in.PodUID = pod
	loc.Sandbox, loc.Incarnation = &in, runtime.SandboxIncarnation(pod, in.Generation)
	return loc, nil
}

// podNow follows pod through the replacements the fake made to the pod the issue's Sandbox holds
// in its place: pod itself where nothing replaced it, else the pod a launch built in its place,
// and so on. live is false where the last pod of that walk was replaced with nothing built in its
// place yet, which is a Sandbox that holds no pod; that last pod is answered then. Called with mu
// held.
func (r *Runtime) podNow(pod string) (string, bool) {
	for {
		next, gone := r.replaced[pod]
		switch {
		case !gone:
			return pod, true
		case next == "":
			return pod, false
		}
		pod = next
	}
}

// buildIn is a new pod a launch builds in the place of replaced, a pod the fake replaced: its uid,
// which podNow answers for replaced from then on. Called with mu held.
func (r *Runtime) buildIn(replaced string) string {
	r.built++
	pod := fmt.Sprintf("fake-pod-%d", r.built)
	r.replaced[replaced] = pod
	return pod
}

func (r *Runtime) Suspend(_ context.Context, loc runtime.Locator) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "Suspend", Locator: loc})
	return r.failures["Suspend"]
}

// Release records its claim, then refuses one whose locator disagrees with it, as every runtime
// does.
func (r *Runtime) Release(_ context.Context, k runtime.Known) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "Release", Released: copyKnown(k)})
	if err := k.Validate(); err != nil {
		return err
	}
	if err := r.failures["Release:"+string(k.Claim)]; err != nil {
		return err
	}
	return r.failures["Release"]
}

// Probe runs the hook HoldProbe set, if any, then records its call and answers the next scripted
// verdict, or Alive once the script runs out.
func (r *Runtime) Probe(_ context.Context, loc runtime.Locator) (runtime.Observation, error) {
	r.mu.Lock()
	hold := r.holdProbe
	r.mu.Unlock()
	if hold != nil {
		hold(loc)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "Probe", Locator: loc})
	kind, detail, outOfMemory := runtime.Alive, "", false
	if len(r.probes) > 0 {
		next := r.probes[0]
		r.probes = r.probes[1:]
		if next.Err != nil {
			return runtime.Observation{}, next.Err
		}
		kind, detail, outOfMemory = next.Kind, next.Detail, next.OutOfMemory
	}
	return runtime.Observation{Locator: loc, Kind: kind, At: r.Now(), Detail: detail, OutOfMemory: outOfMemory}, nil
}

// Observe is the sweep. Every observation emitted, before or after this call, reaches the
// channel in order; the channel closes when ctx ends. One sweep at a time: two live sweeps would
// split the emissions between them, which no test wants.
func (r *Runtime) Observe(ctx context.Context) (<-chan runtime.Observation, error) {
	r.mu.Lock()
	r.calls = append(r.calls, Call{Method: "Observe"})
	failure := r.failures["Observe"]
	r.mu.Unlock()
	if failure != nil {
		return nil, failure
	}

	sweep := make(chan runtime.Observation)
	// The pump waits on the condition; the context ending has to wake it, or a sweep with
	// nothing queued would never notice that it is over.
	go func() {
		<-ctx.Done()
		r.mu.Lock()
		r.pending.Broadcast()
		r.mu.Unlock()
	}()
	go func() {
		defer close(sweep)
		for {
			r.mu.Lock()
			for len(r.queue) == 0 && ctx.Err() == nil {
				r.pending.Wait()
			}
			if ctx.Err() != nil {
				r.mu.Unlock()
				return
			}
			observation := r.queue[0]
			r.queue = r.queue[1:]
			r.mu.Unlock()
			select {
			case sweep <- observation:
			case <-ctx.Done():
				return
			}
		}
	}()
	return sweep, nil
}

// ReconcileOrphans records the known set, then refuses an entry whose locator disagrees with its
// claim, as every runtime does.
func (r *Runtime) ReconcileOrphans(_ context.Context, known []runtime.Known, grace time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	recorded := make([]runtime.Known, len(known))
	for i, entry := range known {
		recorded[i] = copyKnown(entry)
	}
	r.calls = append(r.calls, Call{Method: "ReconcileOrphans", Known: recorded, Grace: grace})
	for _, entry := range known {
		if err := entry.Validate(); err != nil {
			return err
		}
	}
	return r.failures["ReconcileOrphans"]
}

// copyKnown is k with a locator of its own, so a caller that reuses its locator later does not
// rewrite what was recorded.
func copyKnown(k runtime.Known) runtime.Known {
	if k.Locator != nil {
		loc := *k.Locator
		k.Locator = &loc
	}
	return k
}

func (r *Runtime) AdoptWorkingCopy(_ context.Context, loc runtime.Locator, id runtime.GitIdentity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "AdoptWorkingCopy", Locator: loc, Identity: id})
	return r.failures["AdoptWorkingCopy"]
}

func (r *Runtime) ProvisionsWorkspaces() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.InPod
}

func (r *Runtime) ProvisionBound() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ProvisionGrace
}

func (r *Runtime) LaunchBound() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.TryBound
}

// CleanupTree records its call and answers what FailCleanupTree set, nil by default.
func (r *Runtime) CleanupTree(_ context.Context, tree string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, Call{Method: "CleanupTree", Tree: tree})
	return r.failures["CleanupTree"]
}

// mint is the locator an unscripted spawn or resume hands back: tmux-shaped, because that is a
// shape the store round-trips and `Validate` accepts, and a fresh incarnation every time,
// because that is what starting a process gives you. Called with the lock held.
func (r *Runtime) mint(token claim.Token) runtime.Locator {
	r.minted++
	return runtime.Locator{
		Runtime:     runtime.RuntimeTmux,
		Claim:       token,
		Incarnation: fmt.Sprintf("%d:%d", 31000+r.minted, 900000+r.minted),
		Tmux: &runtime.TmuxLocator{
			Window: fmt.Sprintf("@%d", r.minted),
			Pane:   fmt.Sprintf("%%%d", r.minted),
		},
	}
}

package supervise

// The pod's start gate: one session load at a time in an issue pod (dispatch://INFRA-5297). Under
// Kubernetes every role of one issue runs in the issue's one pod, under one memory limit, and an
// Oh My Pi process that resumes a long session peaks at about 4 GiB for some tens of seconds
// while it reads the transcript. Each claim has a machine of its own, and nothing made one
// machine's start wait for another's: two wakes that had passed the wake pacing while the first
// launch was still bringing the pod up started their roles 0.25 s apart, the two loads met, and
// the kernel killed the pod (LINUX-1301). The wake pacing (reserveWake) spaces wakes over the
// whole project, and the pod's launch turn (the Sandbox runtime's lockPod) is given back as the
// role's process starts, before its load; neither sees a load.
//
// The gate is Kubernetes' OrderedReady rule inside one pod, with the end of a session load as the
// readiness signal: a role's process starts only when no role of its pod is starting or loading
// and no role ahead of it in the order waits. A machine asks right before it asks the runtime for
// the process (launchNow, waitForPod), holds the pod from the grant until its process starts
// (podStartFailed ends a hold whose start failed), and from that start until its load ends,
// which is its agent's registration (register moves the claim out of booting) or an end of the
// process before it: a death, a relaunch at a new generation, a failure, a suspension or a
// retirement. Every decision of a machine ends by reading which (Machine.unlock, endPodLoad),
// so a path added later keeps the rule by ending its decision, as the refresh lane's credit is
// kept (endRefreshLane).
//
// A load that does not end is bounded: podLoadBound after its process started, the next role
// starts all the same. A waiting start leaves the queue when its own wait ends: a suspension
// cancels the launch it belongs to (stopStart), a retirement cancels the wait alone (Handle,
// stopPodWait), and the daemon's stop ends both. Neither waits for a load.
//
// The order among the starts waiting in one pod is the spec's (dispatch://INFRA-5297/spec R3,
// podQueue.next): the role whose task or message caused the pod's start goes first, and the others
// follow in the order their starts were asked. A start a request raised — a task, a message, an
// operator's spawn, resume or retry, which is every launch revive makes — is such a cause, and
// the earliest asked of those waiting goes first; one goes first, not every one, so a later
// request's start waits behind a recovery asked before it. With no such start waiting, no single
// role caused the start — deaths, a roles container's restart, a pod an update round replaced —
// and the role working the issue's current phase goes first (Deps.PhaseRole), then the others in
// the order of claim.Roles, each in the order asked where the two say nothing. The order is read
// at each grant, over the starts waiting then, and the pod remembers that a request has led its
// run of starts until it holds nothing and nothing waits in it.
//
// It is one per daemon, shared by every machine and safe for concurrent use, as the refresh gate
// is (RefreshGate): a machine waits inside its decision, holding its own lock, so the gate never
// takes a machine's lock and no machine takes another's. A pod is keyed by its issue, which is
// what one Sandbox serves; the project controller runs in a pod of its own and asks nothing
// (Machine.gated). A daemon whose roles run in no pod builds none (Deps.PodStarts), and the
// daemon's own state holds no part of it: a restart builds an empty gate and gives each pod the
// load of a claim it restores booting (NewMachine, adoptPodLoad), and the log lines are the
// record (podStartedLine, podLoadEndedLine).

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// podLoadBound is how long a role's session load holds its pod's start before the next start
// goes all the same, counted from the moment its process started: the spec's decision, from the
// measured peak of a long resume, which lasts some tens of seconds, so a hung load holds the roles
// behind it a short time only. It is well below the 600 s boot timeout every deployment
// configures (worker_boot_timeout_seconds), so the bound releases a start long before that
// timeout probes the hung process. It is a constant: no setting names it.
const podLoadBound = 2 * time.Minute

// The two lines the gate's record is read from (dispatch://INFRA-5297 R4 and R6): every process
// start into an issue pod (podStarted) and every end of a load (endLoad), each at Info through the
// machine's logger, which carries the claim.
const (
	podStartedLine   = "supervise: started a role's process in its issue pod"
	podLoadEndedLine = "supervise: a role's session load in its issue pod ended"
)

// podHoldEnd is why a hold of a pod's start ended, which is what ended the wait of the start
// granted next: the start line of that start names it (waitEndedBy).
type podHoldEnd string

const (
	// podLoadEnded is the holder's agent registering (register): its load is over.
	podLoadEnded podHoldEnd = "load end"
	// podFailedLaunch is the holder's start failing before its process started (podStartFailed).
	podFailedLaunch podHoldEnd = "failed launch"
	// podProcessEnded is the holder's process ending before its registration: it died, it was
	// relaunched at a new generation, or its claim failed.
	podProcessEnded podHoldEnd = "process end"
	// podSuspension is the holder's claim suspended, its start or its load with it.
	podSuspension podHoldEnd = "suspension"
	// podRetirement is the holder's claim retired.
	podRetirement podHoldEnd = "retirement"
	// podBound is podLoadBound passing over a load that had not ended.
	podBound podHoldEnd = "bound"
)

// loadEnd is the word the load-end line names this end by (endedBy): a load ended by its
// registration, or by what ended its process. A failed launch and the bound end no load and are
// never written there.
func (e podHoldEnd) loadEnd() string {
	if e == podLoadEnded {
		return "registration"
	}
	return string(e)
}

// PodStarts is the gate itself (the gate's own file says what it holds to, and why): for each
// issue pod, the one start it holds, and the starts waiting for it.
type PodStarts struct {
	// clock is the machines' clock (Deps.Clock): it stamps each ask and each load's start, and it
	// arms each load's bound.
	clock Clock

	// mu guards pods, and every field of every hold a queue holds: the machines of every pod
	// reach the gate, and the bound fires on a goroutine of the clock's own.
	mu sync.Mutex
	// pods is each pod with a hold or a wait in it, keyed by the issue; a pod left with neither
	// is dropped (tidy), so the map is as large as the pods starting now.
	pods map[string]*podQueue
}

// podQueue is one pod's start: the pod, the one hold, the starts waiting for it, and the end of
// the last hold, which the next start granted reads as what it waited for.
type podQueue struct {
	pod string
	// holder is the start granted the pod, nil for none: starting from its grant until its process
	// starts, loading from that start until its load ends.
	holder *podHold
	// waiting is every start asked and not yet granted, in the order asked.
	waiting []*podHold
	// lastEnd is the claim of the last hold that left and why it left, read by the grant it
	// makes (grant).
	lastEnd podEnd
	// requestLed is whether a start a request raised has gone first in this run of the pod's
	// starts (next): every later grant of the run follows the order asked. The run ends with the
	// queue, when the pod holds nothing and nothing waits in it (tidy).
	requestLed bool
}

// podEnd is a hold that left: its claim, and why it left.
type podEnd struct {
	token claim.Token
	why   podHoldEnd
}

// podHold is one start's place in its pod: waiting from its ask, then holding the pod from its
// grant. Its fields are the gate's, under PodStarts.mu, but for the ask's own, which are set
// before the hold is queued and never change, and the grant's, which the machine reads once its
// wait returns, after the grant set them.
type podHold struct {
	pod   string
	token claim.Token
	role  claim.Role
	// requested and phaseWorker are what the order reads (podQueue.next): whether a request
	// raised this start, and whether its role works the issue's current phase.
	requested, phaseWorker bool
	// asked is when the start asked; its place in the order asked is its place in its queue's
	// waiting.
	asked time.Time

	// granted is closed at the grant, which is what a wait waits on.
	granted chan struct{}
	// grantedAt is the grant's moment; waited is whether the start waited for it, and waitedFor
	// what ended that wait: the hold that left last when it was granted. A start granted where
	// it asked waited for nothing.
	grantedAt time.Time
	waited    bool
	waitedFor podEnd

	// loading is whether the hold's process has started (started), from loadStart; bound is the
	// timer that releases it podLoadBound later, nil before. releasedByBound is whether that
	// timer released it, for the line its machine writes at its load's end.
	loading         bool
	loadStart       time.Time
	bound           Cancel
	releasedByBound bool
}

// next is the index of the start waiting first in the pod's order, and whether a request put it
// there. Where no request has led the run yet (requestLed), the earliest-asked start a request
// raised goes first; once one has, the order asked. With no start a request raised waiting and
// none having led, the phase worker goes first, then claim.Roles, then the order asked. waiting
// is in the order asked, so that order's first is its first. Called with PodStarts.mu held, over
// a queue with a start waiting.
func (q *podQueue) next() (int, bool) {
	if q.requestLed {
		return 0, false
	}
	if i := slices.IndexFunc(q.waiting, func(h *podHold) bool { return h.requested }); i >= 0 {
		return i, true
	}
	first := 0
	for i, h := range q.waiting {
		o := q.waiting[first]
		if h.phaseWorker != o.phaseWorker {
			if h.phaseWorker {
				first = i
			}
			continue
		}
		if roleRank(h.role) < roleRank(o.role) {
			first = i
		}
	}
	return first, false
}

// roleRank is role's place in claim.Roles; a role outside them goes after every one of them.
func roleRank(role claim.Role) int {
	if i := slices.Index(claim.Roles, role); i >= 0 {
		return i
	}
	return len(claim.Roles)
}

// podAsk is what a start asks the gate with: its pod, its claim and role, and what the order reads
// of it.
type podAsk struct {
	pod                    string
	token                  claim.Token
	role                   claim.Role
	requested, phaseWorker bool
}

// NewPodStarts builds the gate of a daemon whose roles run in issue pods, on the machines' clock
// (internal/daemon's openSupervision, under the Kubernetes runtime alone).
func NewPodStarts(clock Clock) *PodStarts {
	return &PodStarts{clock: clock, pods: map[string]*podQueue{}}
}

// wait asks for the pod's start and waits for it: the hold granted, or ctx's error once ctx ends
// first, with the start out of the queue. A start whose own claim still holds the pod — a hold no
// machine ended, as a machine restarted over the same gate leaves one — gives that hold up first,
// as a process that was let go (podProcessEnded): a claim runs one process at a time.
//
// A grant that lands as ctx ends is given back as a failed launch, so no pod is held by a start
// that will never ask the runtime for its process.
func (g *PodStarts) wait(ctx context.Context, ask podAsk) (*podHold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if held := g.pods[ask.pod]; held != nil && held.holder != nil && held.holder.token == ask.token {
		g.release(held, podProcessEnded)
	}
	q := g.queue(ask.pod)
	h := &podHold{pod: ask.pod, token: ask.token, role: ask.role, requested: ask.requested, phaseWorker: ask.phaseWorker,
		asked: g.clock.Now(), granted: make(chan struct{})}
	q.waiting = append(q.waiting, h)
	g.grant(q, false)
	g.mu.Unlock()
	select {
	case <-h.granted:
		return h, nil
	case <-ctx.Done():
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if q.holder == h {
		g.release(q, podFailedLaunch)
	} else {
		q.waiting = slices.DeleteFunc(q.waiting, func(w *podHold) bool { return w == h })
		g.tidy(q)
	}
	return nil, ctx.Err()
}

// started is h's process started: the hold moves from starting to loading, and the bound is armed
// from now. It answers the load's start.
func (g *PodStarts) started(h *podHold) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	h.loading, h.loadStart = true, g.clock.Now()
	if g.pods[h.pod] != nil && g.pods[h.pod].holder == h {
		h.bound = g.clock.AfterFunc(podLoadBound, func() { g.boundReached(h) })
	}
	return h.loadStart
}

// end is h's hold ended, for why, and the next start granted where h still held the pod. It
// answers whether the bound had released h before, which the load-end line says.
func (g *PodStarts) end(h *podHold, why podHoldEnd) (releasedByBound bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if q := g.pods[h.pod]; q != nil && q.holder == h {
		g.release(q, why)
	}
	return h.releasedByBound
}

// adopt gives a pod with no hold the load of a claim a restart found booting (adoptPodLoad), as
// loading from now, so a start after the restart waits for it. A pod held by that same claim's
// hold — a machine rebuilt over the same gate — is given the new hold in its place. A pod another
// claim holds is left as it is, and the answer is false.
func (g *PodStarts) adopt(pod string, token claim.Token, role claim.Role) (*podHold, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.queue(pod)
	if q.holder != nil && q.holder.token != token {
		return nil, false
	}
	if q.holder != nil && q.holder.bound != nil {
		q.holder.bound.Stop()
	}
	now := g.clock.Now()
	h := &podHold{pod: pod, token: token, role: role, asked: now, granted: make(chan struct{}), grantedAt: now,
		loading: true, loadStart: now}
	close(h.granted)
	h.bound = g.clock.AfterFunc(podLoadBound, func() { g.boundReached(h) })
	q.holder = h
	return h, true
}

// boundReached is the bound passing over h: a hold still loading is released, and the next start
// granted. Its machine writes the load's end when that end comes, and says the bound released it.
func (g *PodStarts) boundReached(h *podHold) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if q := g.pods[h.pod]; q != nil && q.holder == h && h.loading {
		h.releasedByBound = true
		g.release(q, podBound)
	}
}

// release ends q's hold for why and grants the next start. Called with mu held.
func (g *PodStarts) release(q *podQueue, why podHoldEnd) {
	h := q.holder
	if h.bound != nil {
		h.bound.Stop()
	}
	q.holder, q.lastEnd = nil, podEnd{token: h.token, why: why}
	g.grant(q, true)
}

// grant hands q's start to the first start waiting in the order (podQueue.next), where no start
// holds it. after is whether a hold has just left, which is the one way a start waits: the first
// ask into a pod that holds nothing is granted where it asks, chosen over no other start, so it
// leads no run of the pod's starts either. A pod left with nothing is dropped. Called with mu
// held.
func (g *PodStarts) grant(q *podQueue, after bool) {
	if q.holder != nil {
		return
	}
	if len(q.waiting) == 0 {
		g.tidy(q)
		return
	}
	first, byRequest := q.next()
	q.requestLed = q.requestLed || byRequest && after
	h := q.waiting[first]
	q.waiting = slices.Delete(q.waiting, first, first+1)
	q.holder = h
	h.grantedAt, h.waited = g.clock.Now(), after
	if after {
		h.waitedFor = q.lastEnd
	}
	close(h.granted)
}

// queue is pod's queue, made where the pod has none. Called with mu held.
func (g *PodStarts) queue(pod string) *podQueue {
	q := g.pods[pod]
	if q == nil {
		q = &podQueue{pod: pod}
		g.pods[pod] = q
	}
	return q
}

// tidy drops q's pod once nothing holds it and nothing waits in it: the next start into it waits
// for nothing, so nothing of q is left to read. Called with mu held.
func (g *PodStarts) tidy(q *podQueue) {
	if q.holder != nil || len(q.waiting) > 0 {
		return
	}
	if g.pods[q.pod] == q {
		delete(g.pods, q.pod)
	}
}

// podLoad is the session load of this machine's process in its issue pod, from the start of that
// process until its load ends (endPodLoad): the hold it keeps in the gate, the pod's name as the
// lines write it, the generation of the process, and when the process started. It is memory only,
// as the gate is: a restart gives the pod the load of a claim it restores booting (adoptPodLoad).
type podLoad struct {
	hold       *podHold
	pod        string
	generation uint64
	start      time.Time
}

// gated is whether this claim's starts go through the gate: a daemon whose roles run in issue
// pods (Deps.PodStarts), and a claim of an issue's, which the project controller's is not — it
// runs in a pod of its own, and it is the recovery path no wait may hold (reserveWake).
func (m *Machine) gated() bool {
	return m.deps.PodStarts != nil && !claim.IsController(m.claim.Role, m.claim.Issue, m.claim.Tree)
}

// podName is the pod a line names for a claim of issue whose process loc is: the Sandbox's name,
// or the issue where the locator names no Sandbox.
func podName(issue string, loc runtime.Locator) string {
	if loc.Sandbox != nil && loc.Sandbox.Name != "" {
		return loc.Sandbox.Name
	}
	return issue
}

// adoptPodLoad is a restored claim's load: one found booting with a process (NewMachine) is
// loading as far as anyone can tell, so it holds its pod from now, where the pod holds nothing
// yet, and a start after the restart waits for it, or for the bound.
func (m *Machine) adoptPodLoad() {
	c := &m.claim
	if !m.gated() || !slices.Contains(booting, c.State) || c.Locator == nil {
		return
	}
	if hold, ok := m.deps.PodStarts.adopt(c.Issue, c.Token, c.Role); ok {
		m.podLoad = &podLoad{hold: hold, pod: podName(c.Issue, *c.Locator), generation: c.Generation, start: hold.loadStart}
	}
}

// podWaitable is the context a launch's waits at the gate run on: starting's, which a suspension
// and the daemon's stop end, and one of its own that a retirement ends alone (stopPodWait), kept
// under startMu as the start's own is (startable). The returned function clears it and cancels
// it. A claim the gate does not hold gets starting and nothing to clear.
func (m *Machine) podWaitable(starting context.Context) (context.Context, func()) {
	if !m.gated() {
		return starting, func() {}
	}
	waiting, cancel := context.WithCancel(starting)
	m.startMu.Lock()
	m.cancelPodWait = cancel
	m.startMu.Unlock()
	return waiting, func() {
		m.startMu.Lock()
		m.cancelPodWait = nil
		m.startMu.Unlock()
		cancel()
	}
}

// stopPodWait ends the wait at the gate of the launch in flight, if one is in flight, and nothing
// else: a retirement does not wait for a load, and a start already in the runtime is not cut
// short by it (Handle). It takes startMu alone, never mu, as stopStart does.
func (m *Machine) stopPodWait() {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.cancelPodWait == nil {
		return
	}
	m.log.Info("supervise: a retirement of the claim ends its launch's wait for its issue pod's start")
	m.cancelPodWait()
}

// waitForPod is the gate's wait of one try of a launch, right before that try asks the runtime
// for its process (launchNow): the hold granted, nil for a claim the gate does not hold, or the
// error of the wait's context. A load this machine still records is its earlier process's, which
// this launch has let go, so it ends first, as that process's end (podProcessEnded).
func (m *Machine) waitForPod(waiting context.Context, requested bool) (*podHold, error) {
	if !m.gated() {
		return nil, nil
	}
	if m.podLoad != nil {
		m.endLoad(podProcessEnded)
	}
	return m.deps.PodStarts.wait(waiting, podAsk{pod: m.claim.Issue, token: m.claim.Token, role: m.claim.Role,
		requested: requested, phaseWorker: m.phaseWorker(waiting)})
}

// phaseWorker is whether this claim's role works its issue's current phase (Deps.PhaseRole), read
// under the RPC timeout. A read that fails is logged and answers false: the start waits in the
// order of claim.Roles rather than not at all.
func (m *Machine) phaseWorker(ctx context.Context) bool {
	if m.deps.PhaseRole == nil {
		return false
	}
	reading, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	role, err := m.deps.PhaseRole(reading, m.claim.Issue)
	if err != nil {
		m.log.Warn("supervise: could not read the role of the issue's current phase; this start waits as no phase's worker", "error", err)
		return false
	}
	return role == m.claim.Role
}

// podStarted is hold's process started at loc: the gate counts the load from now, the machine
// records it, and the start line is written, naming what the start waited for where it waited.
func (m *Machine) podStarted(hold *podHold, loc runtime.Locator) {
	if hold == nil {
		return
	}
	start := m.deps.PodStarts.started(hold)
	m.podLoad = &podLoad{hold: hold, pod: podName(m.claim.Issue, loc), generation: m.claim.Generation, start: start}
	fields := []any{"pod", m.podLoad.pod, "generation", m.claim.Generation}
	if hold.waited {
		fields = append(fields, "waitedFor", string(hold.waitedFor.token), "waited", hold.grantedAt.Sub(hold.asked),
			"waitEndedBy", string(hold.waitedFor.why))
	}
	m.log.Info(podStartedLine, fields...)
}

// podStartFailed is hold's start failing before its process started: the pod's start goes to the
// next start at once, and a retry of this launch asks again. A start a suspension cut short
// (stopStart) is that suspension's end of the hold.
func (m *Machine) podStartFailed(starting context.Context, hold *podHold) {
	if hold == nil {
		return
	}
	why := podFailedLaunch
	if starting.Err() != nil && m.startWasStopped() {
		why = podSuspension
	}
	m.deps.PodStarts.end(hold, why)
}

// startWasStopped is whether a suspension cancelled the start in flight (stopStart), read without
// taking the mark, which that suspension's own row reads (stoppedAStart).
func (m *Machine) startWasStopped() bool {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	return m.startStopped
}

// endPodLoad ends the load this machine records once its decision has left the claim anywhere but
// booting with that load's process: registered or later (the load is over), suspended, retired,
// or anything else (the process ended — it died, its claim failed, or a relaunch moved the claim
// past it). A process the runtime has reported gone ended its load whatever state the claim still
// reads (processGone): a relaunch the wake pacing parks keeps the dead process on the claim until
// its turn. It is the end of every decision (Machine.unlock), so no path that ends a load has to
// name it.
func (m *Machine) endPodLoad() {
	load := m.podLoad
	if load == nil {
		return
	}
	c := &m.claim
	if slices.Contains(booting, c.State) && c.Locator != nil && c.Generation == load.generation && !m.processGone {
		return
	}
	why := podProcessEnded
	switch {
	case HoldsCapability(c.State):
		why = podLoadEnded
	case c.State == StateSuspended:
		why = podSuspension
	case c.State == StateRetired:
		why = podRetirement
	}
	m.endLoad(why)
}

// endLoad ends the load this machine records, for why: the gate is told, which grants the next
// start where this load still held the pod, and the load-end line is written.
func (m *Machine) endLoad(why podHoldEnd) {
	load := m.podLoad
	m.podLoad = nil
	fields := []any{"pod", load.pod, "generation", load.generation, "loaded", m.deps.Clock.Now().Sub(load.start),
		"endedBy", why.loadEnd()}
	if m.deps.PodStarts.end(load.hold, why) {
		fields = append(fields, "releasedByBound", true)
	}
	m.log.Info(podLoadEndedLine, fields...)
}

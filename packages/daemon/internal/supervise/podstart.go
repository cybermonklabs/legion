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
// which is podLoadSettle after its agent's registration (register moves the claim out of
// booting, and a timer of the gate's own ends the load: PodStarts.settle), or an end of the
// process before that: a death, a relaunch at a new generation, a failure, a suspension or a
// retirement. Every decision of a machine ends by reading which (Machine.unlock, endPodLoad),
// so a path added later keeps the rule by ending its decision, as the refresh lane's credit is
// kept (endRefreshLane).
//
// A load that does not end is bounded: podLoadBound after its process started, the next role
// starts all the same. A waiting start leaves the queue when its own wait ends: a suspension
// cancels the launch it belongs to (stopStart), a retirement cancels that launch's waits at the
// gate and nothing else of it, the one waiting now or one a later try would come to (Handle,
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
	"log/slog"
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

// podLoadSettle is how long after its agent's registration a role's session load ends: the end
// of a load the daemon keeps itself (dispatch://INFRA-5297/spec R2), because the registration
// comes before the load's peak. The plugin registers inside Oh My Pi's session_start (pi-legion's
// claim-session.ts), and in two resumes read every 0.5 s in an issue pod, of a 4.7 MB and a 75 MB
// transcript, the process's memory kept rising after it and peaked 6.7 s and 16.4 s later. So a
// load ends podLoadSettle after the registration rather than at it, nearly twice the later peak.
// The bound still counts from the process start (podLoadBound), so a settle that would pass it is
// cut short there for the next start, and the load's own line is written when its settle ends.
const podLoadSettle = 30 * time.Second

// The two lines the gate's record is read from (dispatch://INFRA-5297 R4 and R6): every process
// start into an issue pod (podStarted) and every end of a load (PodStarts.finish), each at Info
// through the machine's logger, which carries the claim.
const (
	podStartedLine   = "supervise: started a role's process in its issue pod"
	podLoadEndedLine = "supervise: a role's session load in its issue pod ended"
)

// podHoldEnd is why a hold of a pod's start ended, which is what ended the wait of the start
// granted next: the start line of that start names it (waitEndedBy).
type podHoldEnd string

const (
	// podLoadEnded is the holder's load settled: podLoadSettle passed after its agent registered
	// (register, PodStarts.settle), and its load is over.
	podLoadEnded podHoldEnd = "load end"
	// podFailedLaunch is the holder's start failing before its process started (podStartFailed).
	podFailedLaunch podHoldEnd = "failed launch"
	// podProcessEnded is the holder's process ending before its load did: it died, it was
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
// registration, once settled, or by what ended its process. A failed launch and the bound end no
// load and are never written there.
func (e podHoldEnd) loadEnd() string {
	if e == podLoadEnded {
		return "registration"
	}
	return string(e)
}

// PodStarts is the gate itself (the gate's own file says what it holds to, and why): for each
// issue pod, the one start it holds, and the starts waiting for it.
type PodStarts struct {
	// clock is the machines' clock (Deps.Clock): it stamps each ask, each load's start and its
	// registration, and it arms each load's bound and settle.
	clock Clock

	// mu guards pods, and every field of every hold a queue holds: the machines of every pod
	// reach the gate, and the bound and the settle fire on a goroutine of the clock's own.
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
// grant. Its fields are the gate's, under PodStarts.mu, but for three kinds: the ask's own
// (podAsk, asked), which are set before the hold is queued and never change; the grant's, which
// the machine reads once its wait returns, after the grant set them; and loadStart and line,
// which the machine reads once started or adopt set them, and which never change after.
type podHold struct {
	podAsk
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

	// loadStart is when the hold's process started, from which its load counts, and line what the
	// load's end line names beside the gate's own record (started, adopt). bound is the timer that
	// releases the hold podLoadBound later, nil before; releasedByBound is whether it did, which
	// that line says.
	loadStart       time.Time
	line            podLoadLine
	bound           Cancel
	releasedByBound bool
	// registeredAt is when the hold's agent registered, zero before (settle); settle is the timer
	// that ends the load podLoadSettle after it, nil before. ended is whether the load has ended,
	// by its settle or by its machine (finish), so a load ends once and writes one line.
	registeredAt time.Time
	settle       Cancel
	ended        bool
}

// podLoadLine is what a load's end line names beside what the gate keeps of its hold: the
// machine's logger, which carries the claim and is safe to use from any goroutine, the pod's name
// as the lines write it, and the generation of the process. The gate writes the line with it
// (finish), so the settle can write it on the clock's goroutine, which holds no machine's lock.
type podLoadLine struct {
	log        *slog.Logger
	pod        string
	generation uint64
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
// of it (podQueue.next): whether a request raised the start, and whether its role works the
// issue's current phase, which is read only of a start no request raised.
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
// machine ended, as a machine restarted over the same gate leaves one — ends that hold's load
// first, as a process that was let go (podProcessEnded): a claim runs one process at a time.
//
// A grant that lands as ctx ends is given back as a failed launch, so no pod is held by a start
// that will never ask the runtime for its process.
func (g *PodStarts) wait(ctx context.Context, ask podAsk) (*podHold, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	if held := g.pods[ask.pod]; held != nil && held.holder != nil && held.holder.token == ask.token {
		g.finish(held.holder, podProcessEnded)
	}
	q := g.queue(ask.pod)
	h := &podHold{podAsk: ask, asked: g.clock.Now(), granted: make(chan struct{})}
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

// started is h's process started: the hold moves from starting to loading, its load counts from
// now (loadStart), the bound is armed from now, and line is what the load's end line names
// (finish).
func (g *PodStarts) started(h *podHold, line podLoadLine) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h.loadStart, h.line = g.clock.Now(), line
	if q := g.pods[h.pod]; q != nil && q.holder == h {
		h.bound = g.clock.AfterFunc(podLoadBound, func() { g.boundReached(h) })
	}
}

// settle is h's agent registered: its load ends podLoadSettle from now (settled), unless an end
// of its process, its suspension or its retirement comes first (end), and a settle already under
// way is left as it is. It answers whether the load has ended already, which the machine reads
// as its record of the load to drop.
func (g *PodStarts) settle(h *podHold) (ended bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h.ended {
		return true
	}
	if h.settle == nil {
		h.registeredAt = g.clock.Now()
		h.settle = g.clock.AfterFunc(podLoadSettle, func() { g.settled(h) })
	}
	return false
}

// settled is the settle passing over h: its load ends as a load end, where nothing ended it
// first (finish).
func (g *PodStarts) settled(h *podHold) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finish(h, podLoadEnded)
}

// end is h's hold ended by its machine, for why: a start that failed (podStartFailed), or a load
// an end of its process, its suspension or its retirement ended (endLoad). A load its settle
// ended already is left as it is (finish).
func (g *PodStarts) end(h *podHold, why podHoldEnd) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.finish(h, why)
}

// finish ends h's load for why, once: the settle is stopped, the pod released where h still holds
// it (release), and the load-end line written where h's process had started (line). A load that
// has ended already, by its settle or by its machine, is left as it is, so each load writes one
// line. The line is written under mu, so it comes before the start line of the start it lets
// through, which reads the gate (started) before it writes its own.
//
// The line names the claim and pod, how long the load ran from its process's start (loaded), and
// what ended it (endedBy); a load whose agent registered names how long after its process's start
// that was (registered), and one the bound released before its end says so (releasedByBound).
// Called with mu held.
func (g *PodStarts) finish(h *podHold, why podHoldEnd) {
	if h.ended {
		return
	}
	h.ended = true
	if h.settle != nil {
		h.settle.Stop()
	}
	if q := g.pods[h.pod]; q != nil && q.holder == h {
		g.release(q, why)
	}
	if h.line.log == nil {
		return
	}
	fields := []any{"pod", h.line.pod, "generation", h.line.generation, "loaded", g.clock.Now().Sub(h.loadStart),
		"endedBy", why.loadEnd()}
	if !h.registeredAt.IsZero() {
		fields = append(fields, "registered", h.registeredAt.Sub(h.loadStart))
	}
	if h.releasedByBound {
		fields = append(fields, "releasedByBound", true)
	}
	h.line.log.Info(podLoadEndedLine, fields...)
}

// adopt gives a pod with no hold the load of a claim a restart found booting (adoptPodLoad), as
// loading from now, so a start after the restart waits for it; line is what its end line names.
// A pod held by that same claim's hold — a machine rebuilt over the same gate — is given the new
// hold in its place, the old one's timers stopped and its load taken over by the new one, so no
// line is written for it. A pod another claim holds is left as it is, and the answer is false.
// The hold carries only what is read of a holder: its pod and claim, its load's start and line,
// and its bound.
func (g *PodStarts) adopt(pod string, token claim.Token, line podLoadLine) (*podHold, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	q := g.queue(pod)
	if old := q.holder; old != nil {
		if old.token != token {
			return nil, false
		}
		old.ended = true
		for _, timer := range []Cancel{old.bound, old.settle} {
			if timer != nil {
				timer.Stop()
			}
		}
	}
	h := &podHold{podAsk: podAsk{pod: pod, token: token}, loadStart: g.clock.Now(), line: line}
	h.bound = g.clock.AfterFunc(podLoadBound, func() { g.boundReached(h) })
	q.holder = h
	return h, true
}

// boundReached is the bound passing over h: a hold that still holds its pod is released, and the
// next start granted. The bound is armed only once the hold's process started (started, adopt), so
// such a hold is loading, or settling after its registration. Its load ends when that end comes
// (finish), and its line says the bound released it.
func (g *PodStarts) boundReached(h *podHold) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if q := g.pods[h.pod]; q != nil && q.holder == h {
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
	line := podLoadLine{log: m.log, pod: podName(c.Issue, *c.Locator), generation: c.Generation}
	if hold, ok := m.deps.PodStarts.adopt(c.Issue, c.Token, line); ok {
		m.podLoad = hold
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

// stopPodWait cancels every wait at the gate of the launch in flight, if one is in flight — the
// one waiting there now, or one a later try of that launch would come to — and nothing else: a
// retirement does not wait for a load, and a start already in the runtime is not cut short by it
// (Handle). It takes startMu alone, never mu, as stopStart does.
func (m *Machine) stopPodWait() {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.cancelPodWait == nil {
		return
	}
	m.log.Info("supervise: a retirement of the claim cancels every wait for its issue pod's start that its launch in flight makes")
	m.cancelPodWait()
}

// waitForPod is the gate's wait of one try of a launch, right before that try asks the runtime
// for its process (launchNow): the hold granted, nil for a claim the gate does not hold, or the
// error of the wait's context. A load this machine still records is its earlier process's, which
// this launch has let go, so it ends first, as that process's end (podProcessEnded). The phase is
// read only for a start no request raised, since the order reads it of no other (podQueue.next).
func (m *Machine) waitForPod(waiting context.Context, requested bool) (*podHold, error) {
	if !m.gated() {
		return nil, nil
	}
	if m.podLoad != nil {
		m.endLoad(podProcessEnded)
	}
	return m.deps.PodStarts.wait(waiting, podAsk{pod: m.claim.Issue, token: m.claim.Token, role: m.claim.Role,
		requested: requested, phaseWorker: !requested && m.phaseWorker(waiting)})
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
	m.deps.PodStarts.started(hold, podLoadLine{log: m.log, pod: podName(m.claim.Issue, loc), generation: m.claim.Generation})
	m.podLoad = hold
	fields := []any{"pod", hold.line.pod, "generation", m.claim.Generation}
	if hold.waited {
		fields = append(fields, "waitedFor", string(hold.waitedFor.token), "waited", hold.grantedAt.Sub(hold.asked),
			"waitEndedBy", string(hold.waitedFor.why))
	}
	m.log.Info(podStartedLine, fields...)
}

// podStartFailed is hold's start failing before its process started: the pod's start goes to the
// next start at once, and a retry of this launch asks again. A start a suspension cut short
// (stopStart, startCutShort) is that suspension's end of the hold.
func (m *Machine) podStartFailed(hold *podHold) {
	if hold == nil {
		return
	}
	why := podFailedLaunch
	if m.startCutShort() {
		why = podSuspension
	}
	m.deps.PodStarts.end(hold, why)
}

// endPodLoad reads, once its decision has left the claim, what became of the load this machine
// records. A claim still booting with that load's process is loading still. One registered or
// later with it settles: its load ends podLoadSettle after the registration, at a timer of the
// gate's own (PodStarts.settle), and the record is dropped once the gate says that settle ended
// it, which writes nothing here. Anything else ends the load now: a claim suspended or retired,
// or one whose process ended — it died, its claim failed, or a relaunch moved the claim past it —
// during its load or its settle alike. A process the runtime has reported gone ended its load
// whatever state the claim still reads (processGone): a relaunch the wake pacing parks keeps the
// dead process on the claim until its turn. It is the end of every decision (Machine.unlock), so
// no path that settles or ends a load has to name it.
func (m *Machine) endPodLoad() {
	hold := m.podLoad
	if hold == nil {
		return
	}
	c := &m.claim
	if c.Locator != nil && c.Generation == hold.line.generation && !m.processGone {
		switch {
		case slices.Contains(booting, c.State):
			return
		case HoldsCapability(c.State):
			if m.deps.PodStarts.settle(hold) {
				m.podLoad = nil
			}
			return
		}
	}
	why := podProcessEnded
	switch c.State {
	case StateSuspended:
		why = podSuspension
	case StateRetired:
		why = podRetirement
	}
	m.endLoad(why)
}

// endLoad ends the load this machine records, for why, and drops the record: the gate grants the
// next start where this load still held the pod and writes the load-end line, and a load its
// settle ended already is left as it is (PodStarts.finish).
func (m *Machine) endLoad(why podHoldEnd) {
	hold := m.podLoad
	m.podLoad = nil
	m.deps.PodStarts.end(hold, why)
}

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
// the process (launchTries, askPod), holds the pod from the grant until its process starts
// (podStartFailed ends a hold whose start failed), and from that start until its load ends,
// which is podLoadSettle after its agent's registration (register moves the claim out of
// booting, and a timer of the gate's own ends the load: PodStarts.settle), or an end of the
// process before that: a death, a relaunch at a new generation, a failure, a suspension or a
// retirement. Every decision of a machine ends by reading which (Machine.unlock, endPodLoad),
// so a path added later keeps the rule by ending its decision, as the refresh lane's credit is
// kept (endRefreshLane).
//
// No start waits under its machine's lock. A start the gate cannot grant where it asks parks on
// the machine, as the wake pacing parks a launch (askPod, wake.park): the claim stays written
// launching with no process and the decision ends, so a suspension, a release or a close of the
// claim, and every reader of it, finds the machine free, and the outbox row that raised the
// launch finishes and frees its issue's lane. The grant reaches the machine as a timer's firing
// does, an event of its own (TimerPodStart) posted from a goroutine outside the gate's lock, and
// that event's decision asks the runtime for the process under the hold it was granted
// (podStartDue).
//
// A load whose agent has not registered podLoadBound after its process started holds the next
// start no longer: the next role starts all the same. A registration before that stops the bound
// (settle), and the load then ends at its settle, however long after its process's start.
//
// A parked start is a wake that carries its podHold (askPod, wake.park), and the machine keeps its
// own record of that hold (Machine.podPark). The start leaves the gate when anything takes the
// wake off the machine but its grant: every decision ends by reading whether the wake still
// carries the hold the machine recorded (Machine.unlock, endPodPark), as it reads the end of a
// load, so a path added later that drops or replaces the wake takes the start out by ending its
// decision. The cut names its end in the line of a launch cut short, read from what the decision
// left of the claim: a suspension of any source, the pause's and the idle release's among them, as
// a suspension; a retirement — a close of the issue or of the tree, an operator's close — as a
// retirement; a failure of the claim as an end of its process; and a claim still live — a park
// that replaced this one, the pause's gate holding the launch at its grant, a relaunch, the
// daemon's stop — as a failed launch. A start still waiting leaves the queue; one granted and not
// yet run gives the pod on, and the start granted next at once names that end as what ended its
// wait (waitEndedBy). None of them waits for a load. The daemon's stop takes no park out where it
// arrives, and nothing watches a machine's context for it: the gate and every machine's context
// end together, and a grant (TimerPodStart) that reaches a machine whose context has ended starts
// nothing, and its start leaves the gate as a failed launch (podStartDue).
//
// A grant that reaches a machine while its project is paused starts nothing either: the pause's
// gate holds the launch in the park's place and stops the claim for the pause, a launch owed, as
// it holds every launch raised during a pause (podStartDue, pauseGate; decision 7 of
// dispatch://INFRA-3759). A park can outlast the pause check its launch made before it parked by
// minutes, so the grant checks again.
//
// A launch stops the process it let go before it parks (askPod, stopPrevious), so a parked launch
// holds no running process, and what that process held is free while the other role loads.
//
// The order among the starts waiting in one pod is the spec's (dispatch://INFRA-5297/spec R3,
// podQueue.next): the role whose task or message caused the pod's start goes first, and the others
// follow in the order their starts were asked. A start a request raised — a task, a message, an
// operator's spawn, resume or retry, which is every launch revive makes — is such a cause, and
// the earliest asked of those waiting goes first; one goes first, not every one, so a later
// request's start waits behind a recovery asked before it. Where no single role caused the start
// — deaths, a roles container's restart, a pod an update round replaced — the first start to reach
// the gate of a pod that holds nothing goes first, whatever its role, since nothing is there for
// it to wait behind (grant), and the starts that wait behind it go with the role working the
// issue's current phase first (Deps.PhaseRole), then the others in the order of claim.Roles, each
// in the order asked where the two say nothing. The order is read at each grant, over the starts
// waiting then, and the pod remembers that a request has led its run of starts until it holds
// nothing and nothing waits in it.
//
// It is one per daemon, shared by every machine and safe for concurrent use, as the refresh gate
// is (RefreshGate): a machine asks it inside its decision, holding its own lock, and the gate never
// takes a machine's lock, so no machine takes another's. A pod is keyed by its issue, which is
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

// podLoadBound is how long a role's session load holds its pod's start while its agent has not
// registered, counted from the moment its process started: past it the next start goes all the
// same. A registration inside it stops it (settle), and the load holds the pod until its settle
// ends (podLoadSettle): a load that registers late is the one whose peak is late. In the
// measured resume of #5153 the process started at 00:21:34.85Z, its agent registered at +90.7 s
// and the pod's memory peaked at +119.9 s: a bound that ran on past the registration, counted
// from the process's start, would have released the next role at +120.0 s, 0.1 s after the peak,
// where the settle ends that load at +120.7 s. It is the spec's decision, from the measured peak
// of a long resume, which lasts some tens of seconds, so a hung load holds the roles behind it a
// short time only. It is well below the 600 s boot timeout every deployment configures
// (worker_boot_timeout_seconds), so the bound releases a start long before that timeout probes
// the hung process. It is a constant: no setting names it.
const podLoadBound = 2 * time.Minute

// podLoadSettle is how long after its agent's registration a role's session load ends: the end
// of a load the daemon keeps itself (dispatch://INFRA-5297/spec R2), because the registration
// comes before the load's peak. The plugin registers inside Oh My Pi's session_start (pi-legion's
// claim-session.ts), and in two resumes read every 0.5 s in an issue pod, of a 4.7 MB and a 75 MB
// transcript, the process's memory kept rising after it and peaked 6.7 s and 16.4 s later. So a
// load ends podLoadSettle after the registration rather than at it, nearly twice the later peak.
// The registration stops the bound (podLoadBound), so the settle runs whole whenever the agent
// registered; one that registers after the bound released the pod ends its load at its settle
// all the same, and the load's own line is written then.
const podLoadSettle = 30 * time.Second

// The two lines the gate's record is read from (dispatch://INFRA-5297 R4 and R6): every process
// start into an issue pod (podStarted) and every end of a load (PodStarts.finish), each at Info
// through the machine's logger, which carries the claim.
const (
	podStartedLine   = "supervise: started a role's process in its issue pod"
	podLoadEndedLine = "supervise: a role's session load in its issue pod ended"
)

// podLaunchCutShortLine is the line of a parked start that something took out of the gate before
// it ran, naming what did by the end its cut passes (endPodPark): a suspension, a retirement, a
// failure of its claim, or anything that leaves the claim live — a park that replaced it, the
// pause's gate at its grant, a relaunch, the daemon's stop.
const podLaunchCutShortLine = "supervise: launch cut short while it waited for its issue pod's start; its start left the gate"

// podHoldEnd is why a hold of a pod's start ended, which is what ended the wait of the start
// granted next: the start line of that start names it (waitEndedBy).
type podHoldEnd string

const (
	// podLoadEnded is the holder's load settled: podLoadSettle passed after its agent registered
	// (register, PodStarts.settle), and its load is over.
	podLoadEnded podHoldEnd = "load end"
	// podFailedLaunch is the holder's start failing before its process started (podStartFailed),
	// or a start granted and taken out of the gate before its machine ran it while its claim
	// stays live: a park that replaced it, the pause's gate holding the launch at its grant, a
	// relaunch, or the daemon's stop (endPodPark).
	podFailedLaunch podHoldEnd = "failed launch"
	// podProcessEnded is the holder's process ending before its load did: it died, it was
	// relaunched at a new generation, or its claim failed; and a start granted that a failure of
	// its claim took out of the gate before it ran (endPodPark).
	podProcessEnded podHoldEnd = "process end"
	// podSuspension is the holder's claim suspended, its start or its load with it.
	podSuspension podHoldEnd = "suspension"
	// podRetirement is the holder's claim retired, its start or its load with it.
	podRetirement podHoldEnd = "retirement"
	// podBound is podLoadBound passing over a load whose agent had not registered.
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
	// post runs a grant's notify on a goroutine of its own, outside the gate's lock (grant):
	// `go notify()`, which NewPodStarts sets. The package's test harness replaces it (podHarness).
	post func(notify func())

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
// (podAsk, asked, notify), which are set before another goroutine can reach the hold and never
// change; the grant's, which the machine reads once it runs the start granted — in the decision
// that asked, or in the one the grant's event starts, which a goroutine the grant posts begins —
// after the grant set them; and loadStart and line, which the machine reads once started or adopt
// set them, and which never change after.
type podHold struct {
	podAsk
	// asked is when the start asked; its place in the order asked is its place in its queue's
	// waiting.
	asked time.Time
	// notify tells the start's machine of its grant (askPod, Machine.fire), nil for a start
	// granted where it asked. The gate posts it at the grant (grant, post), on a goroutine of its
	// own, so it runs outside the gate's lock and holding no machine's.
	notify func()

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
	// that ends the load podLoadSettle after it, nil before. ended is whether the hold has ended:
	// its load, by its settle or by its machine (finish), or the start itself, taken out of the
	// gate before it ran (cancel). So a load ends once and writes one line.
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
// there. It orders the starts that wait behind a hold alone: the first start to reach a pod that
// holds nothing is granted where it asks, whatever its role (grant), so next never orders it.
// Where no request has led the run yet (requestLed), the earliest-asked start a request raised
// goes first; once one has, the order asked. With no start a request raised waiting and none
// having led, the phase worker goes first, then claim.Roles, then the order asked. waiting is in
// the order asked, so that order's first is its first. Called with PodStarts.mu held, over a
// queue with a start waiting.
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

// podAsk is what a start asks the gate with: its pod, its claim and role, the generation of the try
// that asks, which the line of a start cut short names (endPodPark), and what the order reads of
// it (podQueue.next): whether a request raised the start, and whether its role works the issue's
// current phase, which is read only of a start no request raised.
type podAsk struct {
	pod                    string
	token                  claim.Token
	role                   claim.Role
	generation             uint64
	requested, phaseWorker bool
}

// NewPodStarts builds the gate of a daemon whose roles run in issue pods, on the machines' clock
// (internal/daemon's openSupervision, under the Kubernetes runtime alone).
func NewPodStarts(clock Clock) *PodStarts {
	return &PodStarts{clock: clock, post: func(notify func()) { go notify() }, pods: map[string]*podQueue{}}
}

// ask asks for the pod's start of one try of a launch, and never waits: it answers the hold, and
// whether the gate granted it where it asked. A start that is not granted waits in the pod's
// queue, and notify is how its grant reaches its machine (podHold.notify); it leaves the queue by
// that grant or by cancel. A start whose own claim still holds the pod — a hold no machine ended,
// as a machine restarted over the same gate leaves one — ends that hold's load first, as a
// process that was let go (podProcessEnded): a claim runs one process at a time.
func (g *PodStarts) ask(ask podAsk, notify func()) (*podHold, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if held := g.pods[ask.pod]; held != nil && held.holder != nil && held.holder.token == ask.token {
		g.finish(held.holder, podProcessEnded)
	}
	q := g.queue(ask.pod)
	h := &podHold{podAsk: ask, asked: g.clock.Now()}
	q.waiting = append(q.waiting, h)
	g.grant(q, false)
	if q.holder == h {
		return h, true
	}
	h.notify = notify
	return h, false
}

// cancel takes h out of its pod's start, for why, where no process of it has started: a start
// still waiting leaves the queue, and one granted that its machine has not run yet gives the
// pod's start to the next start, which is granted at once and names why as what ended its wait
// (finish). It answers whether it took h out: false for a start whose process started, and for
// one already taken out or ended.
func (g *PodStarts) cancel(h *podHold, why podHoldEnd) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h.ended || !h.loadStart.IsZero() {
		return false
	}
	q := g.pods[h.pod]
	if q == nil {
		return false
	}
	if q.holder == h {
		g.finish(h, why)
		return true
	}
	i := slices.Index(q.waiting, h)
	if i < 0 {
		return false
	}
	h.ended = true
	q.waiting = slices.Delete(q.waiting, i, i+1)
	g.tidy(q)
	return true
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
// way is left as it is. The registration stops the bound, which holds a load only until its agent
// registers (podLoadBound): the pod is held until the settle ends. It answers whether the load
// has ended already, which the machine reads as its record of the load to drop.
func (g *PodStarts) settle(h *podHold) (ended bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if h.ended {
		return true
	}
	if h.settle == nil {
		h.registeredAt = g.clock.Now()
		h.settle = g.clock.AfterFunc(podLoadSettle, func() { g.settled(h) })
		if h.bound != nil {
			h.bound.Stop()
		}
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

// boundReached is the bound passing over h: a hold that still holds its pod, and whose agent has
// not registered, is released, and the next start granted. The bound is armed only once the
// hold's process started (started, adopt), and a registration stops it (settle), so such a hold
// is loading; a registration the timer's own goroutine raced is read here, and keeps the pod. Its
// load ends when that end comes (finish), and its line says the bound released it.
func (g *PodStarts) boundReached(h *podHold) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if q := g.pods[h.pod]; q != nil && q.holder == h && h.registeredAt.IsZero() {
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
// holds it, and tells that start's machine (podHold.notify) where it waited. after is whether a
// hold has just left, which is the one way a start waits: the first ask into a pod that holds
// nothing is granted where it asks, whatever its role and whoever asks after it, since nothing is
// there for it to wait behind, so it is chosen over no other start and leads no run of the pod's
// starts either. That is the spec's first start to reach the pod's gate where no single role
// caused the pod's start. A pod left with nothing is dropped. Called with mu held.
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
	if h.notify != nil {
		g.post(h.notify)
	}
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

// askPod asks the pod's gate for the start of one try of a launch: launchTries asks it once the
// try is written (writeTry), right before it asks the runtime for the try's process. It answers
// the hold the gate granted where it asked, or nil for a claim the gate does not hold, with false;
// or true where the try waits — another role of the pod starts or loads, or one ahead of this
// start in the order waits — and is parked on the machine until its grant, and launchTries
// returns. A load this machine still records is its earlier process's, which this launch has let
// go, so it ends first, as that process's end (podProcessEnded); and an earlier park this machine
// still records, whose wake this launch dropped as it let the claim's process go (letGo), leaves
// the gate first (endPodPark), so a claim asks for one start at a time. The phase is read only for
// a start no request raised, since the order reads it of no other (podQueue.next).
//
// A try that waits stops the process its launch let go as it parks (stopLetGo), as a try whose
// start failed stops it: that process is let go and still running (letGo, previous), and what
// replaces it in the runtime is the grant's start, so a park that something ends before its grant
// — a suspension, a retirement, a relaunch — would leave it running with no claim recording it,
// and while the park lasts it would hold what the other role's load needs. A stop the runtime
// refuses, or that the try's own context cuts short, leaves the try parked all the same, as every
// caller of stopPrevious goes on: the refusal is recorded (stopUnconfirmed) and logged at Warn,
// and launchTries returns no error at the park. The launch has not failed — it waits at the gate
// to start as asked — and an error would fail the request that raised it: an operator's resume or
// spawn answered as a failure (internal/api's operatorFailure), and the decision logged at Error
// as an event that failed (internal/daemon's supervisor). stopUnconfirmed is what carries the
// refusal to whoever must hear that the process may be running still: the owner of a stalled
// worker.
//
// A parked try is a wake whose turn is the grant rather than a moment of the pacing's (wake.park):
// it waits on the machine as a paced wake over no process does (waitOnMachine), holding the
// rest of the launch from that grant on (launchTries with the grant), and the decision ends where
// launchTries returns at the park, so nothing waits under the machine's lock. The claim is written
// launching at this try's generation with no process already: the try wrote it before its backoff
// (writeTry), and the launch let its process go before its first try (launchNow). The grant
// reaches the machine as a timer's firing does: the gate posts notify at the grant on a goroutine
// of its own (PodStarts.post), outside the gate's lock and holding no machine's, and notify hands
// the machine a Timer of TimerPodStart (fire), whose decision runs the launch on
// (podStartDue). The timer's entry is the arming that event is fenced by. The machine records the
// hold (Machine.podPark), so whatever drops the wake takes the start out of the gate at the end of
// its decision (endPodPark). The daemon's stop leaves the start where it is, since the gate ends
// with the daemon, and a grant that reaches the machine after that stop starts nothing
// (podStartDue).
//
// The wake is a held try's (heldTryWake) with the park's hold: its run is the launch from that
// grant on while the wake carries the hold, and a fresh try of the same launch, asked of the gate
// anew, once it carries none: the pause's gate holds the wake in the park's place where the
// project is paused at the grant (podStartDue), and its let-out after the pause runs it
// (letOutHeldLaunch, runWake), without a second place in the pacing or a second wake line, since
// the launch passed the wake gate before it parked (wake.paid, wake.ran). Its state is the claim's
// as the park leaves it, launching, which a request that does not wait restores once the pause's
// gate holds the wake (dropWakeFor).
//
// A restart finds a claim whose launch was parked here stored launching with no process, as it
// finds one the wake pacing parked over no process: internal/daemon's supervisor.restore writes
// it launch_uncertain, and relaunchUnfinished, once the boot's reconciliation has proved that no
// process of it runs, releases it to queued and spawns it again (launchUnfinished), which
// asks the new daemon's empty gate.
func (m *Machine) askPod(ctx context.Context, l launchTry, token string) (*podHold, bool) {
	if !m.gated() {
		return nil, false
	}
	if m.podLoad != nil {
		m.endLoad(podProcessEnded)
	}
	m.endPodPark()
	ask := podAsk{pod: m.claim.Issue, token: m.claim.Token, role: m.claim.Role, generation: m.claim.Generation,
		requested: l.requested, phaseWorker: !l.requested && m.phaseWorker(ctx)}
	m.seq++
	granted := Timer{Claim: m.claim.Token, Kind: TimerPodStart, Generation: m.claim.Generation, Seq: m.seq}
	hold, now := m.deps.PodStarts.ask(ask, func() { m.fire(granted) })
	if now {
		return hold, false
	}
	m.timers[TimerPodStart] = armed{seq: granted.Seq}
	parked := m.heldTryWake(l, token)
	parked.park = hold
	m.wake, m.podPark = parked, hold
	m.log.Info("supervise: the launch waits for its issue pod's start while another role of the pod starts or loads its session",
		"generation", m.claim.Generation, "pod", m.claim.Issue)
	m.stopLetGo(ctx, "its issue pod's start")
	return hold, true
}

// endPodPark takes the start this machine parked at its issue pod's gate (Machine.podPark) out of
// the gate once the wake that carried it is off the machine: dropped or replaced by the decision
// that is ending, whatever path did so — a suspension, a retirement, a let-go of the claim's
// process, a park of the wake pacing's or of the pause's gate, the daemon's stop (podStartDue) —
// or by an earlier launch's let-go that a new try now replaces (askPod). A wake that still
// carries the hold keeps it, and the grant clears the record as it runs the start (podStartDue).
// It is the end of every decision (Machine.unlock), so no path that drops the wake has to name it.
//
// The end the cut passes is read from what the decision left of the claim: suspended is a
// suspension, retired a retirement, failed an end of the process the launch was to start, and a
// claim still live a failed launch. A start still waiting leaves the queue, and one granted that
// its machine has not run yet gives the pod to the next start, whose start line names that end as
// what ended its wait (PodStarts.cancel). The grant's arming goes with the record (TimerPodStart),
// so a grant already posted is dropped as stale. The line of a launch cut short names the end and
// the generation of the try that parked (podAsk.generation), once: a start the gate had ended
// already writes nothing.
func (m *Machine) endPodPark() {
	park := m.podPark
	if park == nil || m.wake != nil && m.wake.park == park {
		return
	}
	m.podPark = nil
	m.disarm(TimerPodStart)
	why := podEndFor(m.claim.State, podFailedLaunch)
	if m.deps.PodStarts.cancel(park, why) {
		m.log.Info(podLaunchCutShortLine, "generation", park.generation, "by", string(why))
	}
}

// podEndFor is the end a hold of a claim in state passes when its machine ends it (endPodPark,
// endPodLoad): a suspension, a retirement, or an end of its process for a claim that failed; and
// live, the caller's own end, for a claim still live — a failed launch for a parked start, an end
// of its process for a load.
func podEndFor(state ClaimState, live podHoldEnd) podHoldEnd {
	switch state {
	case StateSuspended:
		return podSuspension
	case StateRetired:
		return podRetirement
	case StateFailed:
		return podProcessEnded
	}
	return live
}

// podStartDue is the grant of the pod's start to the launch parked at its gate (TimerPodStart,
// askPod): the wake is taken off the machine, the record of its hold with it, and the launch runs
// on from that grant, asking the runtime for its process under the hold it was granted
// (launchTries). Nothing else of a wake's turn is taken here (runWake): the launch made its
// lifecycle check, passed its wake gate and wrote the one line a wake writes before it reached the
// pod's gate (launchAfter, reserveWake), let its process go there (launchNow) and stopped it at the
// park (askPod), so no turn of a process is left to take back.
//
// A grant that reaches a machine whose context has ended — the daemon's stop — starts nothing: the
// wake is dropped, and its start leaves the gate as a failed launch at the end of this decision
// (endPodPark), as a start that stop cut short in the runtime ends (launchTries, podStartFailed).
//
// A grant that reaches a machine whose project is paused starts nothing either (decision 7 of
// dispatch://INFRA-3759): the park can hold a launch for minutes after the pause check its try
// made (launchTries), so the grant checks again, as a wake's turn does (wakeNow). The wake gives
// up its hold, which leaves the gate as a failed launch at the end of this decision (endPodPark),
// and the pause's gate holds it in the park's place and stops the claim for the pause in its next
// decision, a launch owed (pauseGate, pauseStopDue).
func podStartDue(m *Machine, ctx context.Context, _ Event) error {
	waiting := m.wake
	if waiting == nil || waiting.park == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		m.wake = nil
		return err
	}
	if pause, holds := m.pauseHolds(); holds {
		waiting.park = nil
		return m.pauseGate(ctx, waiting, pause)
	}
	m.wake, m.podPark = nil, nil
	return waiting.run(ctx)
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
	m.endLoad(podEndFor(c.State, podProcessEnded))
}

// endLoad ends the load this machine records, for why, and drops the record: the gate grants the
// next start where this load still held the pod and writes the load-end line, and a load its
// settle ended already is left as it is (PodStarts.finish).
func (m *Machine) endLoad(why podHoldEnd) {
	hold := m.podLoad
	m.podLoad = nil
	m.deps.PodStarts.end(hold, why)
}

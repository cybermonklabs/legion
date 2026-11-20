package daemon

// The rig the sweep's contract tests (podupdate_sweep_contract_test.go) drive the sweep's whole
// path with, from the daemon's tick to the publish of each ask, and the substitutes its machines
// are built over.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/runtime/fake"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// sweepPath drives the sweep's whole path over claims a test builds, from the daemon's tick to the
// publish of each ask. The tick is supervision.reconcilePodUpdates, which the orphan sweep calls
// with every claim it read: it groups the claims by issue (reconcileIssuePodUpdate), reads the
// issue's pod and the round open over it, reads the pod's drift, reads the group live from the
// claims' machines (liveGroup), decides whether the round is over (completePodUpdate), and hands
// the live group to openPodUpdate. That hands it to askRound, which reads the sweep's list
// (unaskedClaims), keeps each claim of it whose agent said it is ready and that a machine of this
// daemon holds, reads the round's asks still queued and enqueues one ask for each claim it keeps;
// and openPodUpdate posts PodUpdateOpen to each claim with a session (blockingClaims). The outbox
// then runs each ask's row (the outbox's updateAsk), which publishes it under the claim's own
// machine (supervise's Machine.PublishUpdateAsk). So a statement of the sweep's own placed anywhere
// on that path changes what a test reads here: in reconcilePodUpdates or reconcileIssuePodUpdate,
// in the live read, in openPodUpdate, in askRound, in the list, or in the outbox's publish.
//
// Each claim a test builds is put where every read of it on that path finds it. A machine built
// from the claim itself takes the place of the rig's machine of its token (hold), so the live
// read, askRound's question whether a machine holds the claim, and the publish under the claim's
// lock each read the claim as built. The claims the tick is handed are the same claims, each with
// the issue and the round set and, where a machine holds it, a process in the pod (sweep): those
// are what the tick reads to group the claims and to find the group's pod and the round open over
// it, and the sweep ends a round over any other pod, and opens rather than asks where no claim is
// in one, before openPodUpdate. Every other fact of a claim the tick reads as built, and the
// machine holds the claim whole. A claim of a token no machine holds (the controller's) is read
// live as the tick was handed it (roundGroup), so it is handed with no process: the round is then
// replaced through no claim but one the test built. A group in which no claim waits for the round
// (waitingClaims) is decided before openPodUpdate too (completePodUpdate), and the rule owes no
// claim of such a group an ask, so the path is held to the rule there as well.
//
// The machines a test builds write nothing to the store and answer every name of a claim's session
// as that claim's (sweepClaims), and arm no timer (sweepClock): a test puts hundreds of thousands
// of claims to the path, each a machine of its own for one sweep. The runtime reads every pod there
// and drifted, and records nothing (steadyPod), and the tick reads no hold of a project's pause
// (noPauseHolds).
// The rig's machines are fed nothing once the path is built: each one's inbox is swapped for one
// no goroutine takes from, and the inbox it had is closed, which ends the goroutine that fed it. So
// each post openPodUpdate makes is kept there for posts to read rather than decided, and no
// machine changes under a test.
type sweepPath struct {
	rig    *podUpdateRig
	posted map[claim.Token]*inbox
	deps   supervise.Deps
	read   []supervise.Claim
	// view, where it is set, is the state of the machine's view outside the claim that hold puts
	// every machine of a group in (sweepViewStates), held is each machine that took it, and taken
	// counts the machines that took it.
	view  *sweepViewState
	held  map[claim.Token]*sweepHeld
	taken int
}

func newSweepPath(rig *podUpdateRig) *sweepPath {
	p := &sweepPath{rig: rig, posted: map[claim.Token]*inbox{}, deps: rig.sup.deps, held: map[claim.Token]*sweepHeld{}}
	p.deps.Store, p.deps.Clock, p.deps.Log = sweepClaims{rig.sup.deps.Store}, sweepClock{rig.clock}, slog.New(slog.DiscardHandler)
	rig.s.runtime, rig.s.pauseHoldsSince = steadyPod{rig.rt}, noPauseHolds
	rig.sup.mu.Lock()
	defer rig.sup.mu.Unlock()
	for token, m := range rig.sup.machines {
		fed := m.inbox
		m.inbox = newInbox()
		fed.close()
		p.posted[token] = m.inbox
	}
	return p
}

// hold puts each claim of group into a machine of its own, in the place of the rig's machine of
// the claim's token. A claim of a token no machine of the rig holds stays unheld, and holds none of
// a machine's own state (sweepMachineStates, sweepViewStates). A claim that sets a field of that
// state is put to the path as its machine holds it once the machine was driven into that state:
// group's claim becomes the machine's view of it, so where the machine does not take the state
// the claim is tried without the field, as a running daemon would hold it. Where the path names a
// state of the view outside the claim (view), each machine of the group is put in it the same
// way, through the machine's own request, and group's claim becomes its view; a machine that does
// not take it holds the claim as the request left it.
func (p *sweepPath) hold(group []supervise.Claim) {
	p.rig.t.Helper()
	p.rig.sup.mu.Lock()
	defer p.rig.sup.mu.Unlock()
	for i := range group {
		c := &group[i]
		member, ok := p.rig.sup.machines[c.Token]
		var states []sweepMachineState
		for _, s := range sweepMachineStates {
			if !s.holds(*c) {
				continue
			}
			if !ok {
				s.clear(c)
				continue
			}
			if s.prepare != nil {
				s.prepare(c)
			}
			states = append(states, s)
		}
		if !ok {
			continue
		}
		if p.view != nil {
			if len(states) > 0 {
				p.rig.t.Fatalf("%s holds the machine state %s and is put in the view's %s too: a test tries one at a time", describeSwept(*c), states[0].field, p.view.field)
			}
			m, held := p.view.take(p, *c)
			if held != nil {
				p.held[c.Token] = held
				p.taken++
			}
			*c = m.View().Claim
			member.machine = m
			continue
		}
		m := p.machine(p.deps, *c)
		for _, s := range states {
			s.drive(p, m, *c)
		}
		// A state's drive can leave the machine's own follow-up work in flight — the revoke of a
		// let-go process's enrollment (supervise's Machine.revoke), which takes the machine's lock
		// when it ends — and a row run under that lock answers ErrMachineBusy, so the claim is put
		// to the path only once that work has ended.
		m.Wait()
		if len(states) > 0 {
			*c = m.View().Claim
		}
		member.machine = m
	}
}

// machine is a machine of its own over c, built on deps.
func (p *sweepPath) machine(deps supervise.Deps, c supervise.Claim) *supervise.Machine {
	p.rig.t.Helper()
	m, err := supervise.NewMachine(p.rig.sup.ctx, deps, c)
	if err != nil {
		p.rig.t.Fatalf("build the machine of %s: %v", describeSwept(c), err)
	}
	return m
}

// sweepMachineState is one field of the claim that the machine's view puts over the claim's record
// from the machine's own memory (supervise's Machine.View), with how a test puts a machine in the
// state it reads: set on the claim alone, the field would read false in the live read whatever the
// claim says.
type sweepMachineState struct {
	field string
	holds func(supervise.Claim) bool
	clear func(*supervise.Claim)
	// prepare makes the claim one a machine can hold the state over, before its machine is built.
	prepare func(*supervise.Claim)
	// drive puts the machine in the state through the machine's own request or read. Where the
	// machine does not take it, its view reads the field false.
	drive func(p *sweepPath, m *supervise.Machine, c supervise.Claim)
}

// sweepMachineStates is every field of the claim the machine's view reads from the machine's own
// state (supervise's Machine.current): a compaction in flight (Claim.Compacting), which the
// machine's own request takes the claim for as the first step of Machine.Compact, so the claim is
// working until the compaction ends, and none ends here; and the runtime's last report of the
// process the claim records (Claim.ProcessGone), which a read of that process the runtime answers
// gone records (Machine.ReadProcess), on a claim given a process in the pod where it records none.
// A field the view reads from the machine that this list does not name fails the field test
// (sweepPath.machineStatesRead).
var sweepMachineStates = []sweepMachineState{
	{
		field: "Compacting",
		holds: func(c supervise.Claim) bool { return c.Compacting },
		clear: func(c *supervise.Claim) { c.Compacting = false },
		drive: func(p *sweepPath, m *supervise.Machine, c supervise.Claim) {
			// A claim in a state that takes no compaction is refused or ignored, and its view
			// reads no compaction.
			_ = m.Handle(p.rig.ctx, supervise.RequestCompact{Claim: c.Token})
		},
	},
	{
		field: "ProcessGone",
		holds: func(c supervise.Claim) bool { return c.ProcessGone },
		clear: func(c *supervise.Claim) { c.ProcessGone = false },
		prepare: func(c *supervise.Claim) {
			if c.Locator == nil {
				c.Locator = podUpdateLocator(c.Token, podUpdateIssue, podUpdatePod)
			}
		},
		drive: func(p *sweepPath, m *supervise.Machine, c supervise.Claim) {
			p.rig.rt.ScriptProbe(fake.ProbeResult{Kind: runtime.Gone})
			if read := m.ReadProcess(p.rig.ctx); read != supervise.ProcessGone {
				p.rig.t.Fatalf("the read of the process of %s answered %q, want %q: the runtime was scripted to answer it gone", describeSwept(c), read, supervise.ProcessGone)
			}
		},
	},
}

// machineStatesRead reports each field the machine's view puts over the claim's record from the
// machine's own memory that sweepMachineStates does not drive, and each one it drives that the
// machine of base does not take: base with each leaf it leaves empty set, put into a machine of
// its own (hold), is read back through the machine's view.
func (p *sweepPath) machineStatesRead(base supervise.Claim, leaves []claimLeaf, at time.Time) (wrong []string) {
	p.rig.t.Helper()
	for _, leaf := range leaves {
		if !leaf.empty(base) {
			continue
		}
		c := base
		leaf.set(&c, at)
		p.hold([]supervise.Claim{c})
		m, ok := p.rig.sup.Machine(c.Token)
		if !ok {
			p.rig.t.Fatalf("no machine holds %s", describeSwept(c))
		}
		if !leaf.empty(m.View().Claim) {
			continue
		}
		if slices.ContainsFunc(sweepMachineStates, func(s sweepMachineState) bool { return s.field == leaf.top }) {
			wrong = append(wrong, fmt.Sprintf("the machine of %s did not take the state its view reads %s from", describeSwept(c), leaf.name))
			continue
		}
		wrong = append(wrong, fmt.Sprintf("the machine's view reads %s empty on a claim whose record sets it, so it is the machine's own state: name it in sweepMachineStates with how a machine is put in that state", leaf.name))
	}
	return wrong
}

// sweepViewState is one field the machine's view holds beside the claim rather than in it
// (supervise's ClaimView), from the machine's own memory, with how a test puts a machine in the
// state it reads. take builds the machine of c over dependencies that hold that state where the
// test can let it go, and puts the machine in it through the machine's own request; it answers
// the hold, or nil where the machine did not take the state.
type sweepViewState struct {
	field string
	// what names the state in a test's failure text.
	what string
	take func(p *sweepPath, c supervise.Claim) (*supervise.Machine, *sweepHeld)
}

// sweepHeld is a machine held in a view state. deciding is whether the machine's decision is in
// flight while the tick reads it — a stop is, and holds the machine's lock until it is let go, so
// the tick reads that claim as one that has not answered (roundGroup's mask) — and letGo lets such
// a decision end once the tick has read the group, before the outbox runs its rows, which publish
// under that lock. decided is the machine's claim as the decision left it, read once letGo has
// returned and before any row ran (sweepPath.letGo), so the delivery mark a row writes is not its
// own expectation. end lets the rest go and waits for the machine's own work, once the rows ran.
type sweepHeld struct {
	machine  *supervise.Machine
	deciding bool
	letGo    func()
	decided  *supervise.Claim
	end      func()
}

// sweepViewStates is every field of the machine's view outside the claim (supervise's ClaimView,
// published by Machine.publish), each with how a machine is put in it: a prompt on its way to the
// agent (SendInFlight), which an operator's task starts on a ready or idle claim over a connection
// whose prompt waits until the test lets it go; a stop in flight (Stopping), which an operator's
// suspension of a claim whose process runs holds while the runtime's suspension waits; and a wake
// the pacing holds (WakeWaiting), which the stale-plugin lane's relaunch of a ready or idle claim
// on another plugin contract parks where the pacing gives its launch a later moment. A field of the
// view outside the claim this list does not name fails the field test (sweepPath.viewStatesRead).
//
// The prompt's machine runs on a daemon that speaks the claim's own plugin contract, and a claim
// that records none is given the daemon's: a claim in no round whose contract is not the daemon's
// is handed no task (supervise.StaleRefusesCalls), and a claim at the tick that opens the round is
// in none yet. A claim that has answered its open round is handed no task either (sendPending), so
// for such a claim the task is sent first and the answer reaches the machine while the prompt is
// on its way, as a worker's answer from a turn an Envoy message started can.
var sweepViewStates = []sweepViewState{
	{
		field: "SendInFlight", what: "with a prompt on its way to the agent",
		take: func(p *sweepPath, c supervise.Claim) (*supervise.Machine, *sweepHeld) {
			// The package's held prompt (supervisor_test.go): the one send this machine starts
			// waits at its door until end lets it through, and the entry it reports is not read.
			conn := newGatedPrompt()
			deps := p.deps
			deps.Conns = sweepConns{Conns: p.deps.Conns, token: c.Token, conn: conn}
			if c.PluginContract == 0 {
				c.PluginContract = p.deps.PluginContract
			}
			deps.PluginContract = c.PluginContract
			answers, head := c.UpdateRound != "" && c.UpdateSignalledAt != nil, c.UpdateHead
			if answers {
				c.UpdateSignalledAt, c.UpdateHead = nil, ""
			}
			m := p.machine(deps, c)
			// An operator's own task names no phase, so no phase check drops it, and it is sent at
			// once to a ready or idle claim with no task in flight.
			_ = m.Handle(p.rig.ctx, supervise.RequestDeliver{Claim: c.Token, Task: "the operator's task"})
			end := func() {
				close(conn.release)
				m.Wait()
			}
			taken := m.View().SendInFlight
			if !taken {
				end()
			}
			if answers {
				_ = m.Handle(p.rig.ctx, supervise.PodUpdateSignalled{
					Claim: c.Token, Generation: c.Generation, Session: c.Session, Round: c.UpdateRound, Head: head,
				})
			}
			if !taken {
				return m, nil
			}
			return m, &sweepHeld{machine: m, end: end}
		},
	},
	{
		field: "Stopping", what: "with its process being stopped",
		take: func(p *sweepPath, c supervise.Claim) (*supervise.Machine, *sweepHeld) {
			if c.Locator == nil {
				c.Locator = podUpdateLocator(c.Token, podUpdateIssue, podUpdatePod)
			}
			rt := &heldStop{Runtime: p.deps.Runtime, entered: make(chan struct{}, 1), release: make(chan struct{})}
			deps := p.deps
			deps.Runtime = rt
			m := p.machine(deps, c)
			decided := make(chan struct{})
			go func() {
				defer close(decided)
				_ = m.Handle(p.rig.ctx, supervise.RequestSuspend{Claim: c.Token, By: supervise.SuspendedByOperator})
			}()
			// letGo ends the decision and waits for the work it leaves behind (Machine.Wait): the
			// stop of a claim with an enrollment revokes it on a goroutine of the machine's, which
			// takes the machine's lock when it ends, and a row run under that lock answers
			// ErrMachineBusy.
			letGo := func() {
				close(rt.release)
				<-decided
				m.Wait()
			}
			select {
			case <-rt.entered:
			case <-decided:
				close(rt.release)
				m.Wait()
				return m, nil
			}
			if !m.View().Stopping {
				letGo()
				return m, nil
			}
			return m, &sweepHeld{machine: m, deciding: true, letGo: letGo, end: m.Wait}
		},
	},
	{
		field: "WakeWaiting", what: "with a wake the pacing holds",
		take: func(p *sweepPath, c supervise.Claim) (*supervise.Machine, *sweepHeld) {
			// The machine's daemon speaks a later plugin contract than the claim's process, and its
			// pacing gives every wake a moment an hour on. The relaunch replaces a process, so a
			// claim that records none is given one in the pod, and its launch check, which asks the
			// store about the cleanup of the claim's tree (store.CheckLaunch), a lifecycle the test
			// claims record no tree of, passes.
			if c.Locator == nil {
				c.Locator = podUpdateLocator(c.Token, podUpdateIssue, podUpdatePod)
			}
			deps := p.deps
			deps.Store = launchChecked{p.deps.Store}
			deps.PluginContract = c.PluginContract + 1
			deps.WakePace = supervise.WakePace{Reserve: func(now time.Time) time.Time { return now.Add(time.Hour) }, Rate: 1, Burst: 1}
			m := p.machine(deps, c)
			_ = m.Handle(p.rig.ctx, supervise.StalePlugin{Claim: c.Token, Trigger: "sweep", Admitted: true})
			if m.View().WakeWaiting == nil {
				m.Wait()
				return m, nil
			}
			return m, &sweepHeld{machine: m, end: m.Wait}
		},
	},
}

// sweepViewField is a state of the machine's view (sweepViewState.field), a field of the claim
// (claimLeaf.name) and where the field test tries the two together: "in an open round" or "at the
// tick that opens the round".
type sweepViewField struct{ state, field, where string }

// sweepViewFieldsUntaken is each pair of a view state and a field of the claim that no machine of
// the field test holds at once, with why. A try of such a pair puts the field to the path on a
// claim whose machine took no state, so it tries the field alone. The field test fails for a pair
// that no machine held and this does not name, and for a pair this names that a machine held.
var sweepViewFieldsUntaken = map[sweepViewField]string{
	{"SendInFlight", "Pending.ConfirmedAt", "in an open round"}:                 untakenConfirmedSend,
	{"SendInFlight", "Pending.ConfirmedAt", "at the tick that opens the round"}: untakenConfirmedSend,
	{"Stopping", "Pending.ConfirmedAt", "in an open round"}:                     untakenConfirmedStop,
	{"Stopping", "Pending.ConfirmedAt", "at the tick that opens the round"}:     untakenConfirmedStop,
	{"WakeWaiting", "Pending.ConfirmedAt", "in an open round"}:                  untakenConfirmedWake,
	{"WakeWaiting", "Pending.ConfirmedAt", "at the tick that opens the round"}:  untakenConfirmedWake,
}

const (
	untakenConfirmedSend = "a prompt starts only for a task the agent has not taken up (sendPending), and the rig sends no turn's start while a prompt is on its way"
	untakenConfirmedStop = "a task the agent took up is a turn, and an operator's suspension of a claim in a turn is held until the turn ends (supervise's holdSuspension), so no stop is in flight"
	untakenConfirmedWake = "a task the agent took up is a turn, and the stale-plugin lane relaunches no claim in a turn before the turn ends (holdStaleRelaunch), so it paces no wake"
)

// viewStatesRead reports each field of the machine's view outside the claim that sweepViewStates
// does not name, and each one it names that the machine of base, put into a machine of its own
// (hold), does not take.
func (p *sweepPath) viewStatesRead(base supervise.Claim) (wrong []string) {
	p.rig.t.Helper()
	view := reflect.TypeOf(supervise.ClaimView{})
	for i := range view.NumField() {
		f := view.Field(i)
		if f.Anonymous || !f.IsExported() {
			continue
		}
		j := slices.IndexFunc(sweepViewStates, func(s sweepViewState) bool { return s.field == f.Name })
		if j < 0 {
			wrong = append(wrong, fmt.Sprintf("the machine's view holds %s beside the claim, from the machine's own memory: name it in sweepViewStates with how a machine is put in that state", f.Name))
			continue
		}
		p.view = &sweepViewStates[j]
		p.hold([]supervise.Claim{base})
		_, took := p.held[base.Token]
		p.letGo()
		p.end()
		p.view = nil
		if !took {
			wrong = append(wrong, fmt.Sprintf("the machine of %s did not take the state its view reads %s from", describeSwept(base), f.Name))
		}
	}
	return wrong
}

// letGo lets each machine held deciding end its decision (sweepHeld.letGo), and keeps the claim
// each decision left (sweepHeld.decided).
func (p *sweepPath) letGo() {
	for _, h := range p.held {
		if h.letGo != nil {
			h.letGo()
			h.letGo = nil
			decided := h.machine.View().Claim
			h.decided = &decided
		}
	}
}

// deciding is what the tick and the publish do with c where its machine is held deciding
// (sweepHeld), and whether it is. The tick reads such a claim through the deciding mask as one
// that has not answered the round (roundGroup), so it asks c where asks holds for c with its answer
// taken off; the ask's row, run once the decision ended, reaches the role only where the rule
// still owes the claim an ask as the decision left it (supervise's Machine.PublishUpdateAsk, which
// reads Claim.OwesUpdateAsk under the claim's lock). It is read after letGo and before any row
// runs. asks is the test's own statement of whom the tick asks.
func (p *sweepPath) deciding(c supervise.Claim, asks func(supervise.Claim) bool) (asked, heard, deciding bool) {
	p.rig.t.Helper()
	h := p.held[c.Token]
	if h == nil || !h.deciding {
		return false, false, false
	}
	if h.decided == nil {
		p.rig.t.Fatalf("what the publish does with %s was read before its machine's decision was let go (sweepPath.letGo)", describeSwept(c))
	}
	read := c
	read.UpdateSignalledAt, read.UpdateHead = nil, ""
	asked = asks(read)
	owed, _ := h.decided.OwesUpdateAsk(podUpdateRound)
	return asked, asked && owed, true
}

// end lets every held machine go and waits for its own work (sweepHeld.end).
func (p *sweepPath) end() {
	p.letGo()
	for token, h := range p.held {
		if h.end != nil {
			h.end()
		}
		delete(p.held, token)
	}
}

// sweepConns is the rig's connections with token's in conn's place.
type sweepConns struct {
	runtime.Conns
	token claim.Token
	conn  runtime.Conn
}

func (s sweepConns) Conn(token claim.Token) (runtime.Conn, bool) {
	if token == s.token {
		return s.conn, true
	}
	return s.Conns.Conn(token)
}

// heldStop is the runtime whose suspension of a process waits until the test lets it go, so the
// decision that stops the process stays in flight (supervise's ClaimView.Stopping).
type heldStop struct {
	runtime.Runtime
	entered chan struct{}
	release chan struct{}
}

func (h *heldStop) Suspend(ctx context.Context, loc runtime.Locator) error {
	select {
	case h.entered <- struct{}{}:
	default:
	}
	<-h.release
	return h.Runtime.Suspend(ctx, loc)
}

// launchChecked is a machine's store whose launch check passes every launch (sweepViewStates'
// wake).
type launchChecked struct{ supervise.Store }

func (launchChecked) CheckLaunch(context.Context, supervise.Claim) error { return nil }

// sweep runs the daemon's tick over group, each claim of it read by the tick in the issue and in a
// process in pod where a machine holds it, and, where round is not empty, in round with the rig's
// drift as the reasons it records. An empty round is the tick that opens the round: no claim
// records a round yet, so the tick reads the group live and posts the round's opening.
func (p *sweepPath) sweep(group []supervise.Claim, pod, round string) {
	p.read = append(p.read[:0], group...)
	for i := range p.read {
		c := &p.read[i]
		c.Issue, c.UpdateRound, c.UpdateReasons, c.Locator = podUpdateIssue, round, "", nil
		if round != "" {
			c.UpdateReasons = podUpdateDrift
		}
		if _, held := p.posted[c.Token]; held {
			c.Locator = podUpdateLocator(c.Token, podUpdateIssue, pod)
		}
	}
	p.rig.s.reconcilePodUpdates(p.rig.ctx, p.read)
}

// posts reports what is wrong with the posts the last sweep made to the claims of group, of the
// tokens a machine of this daemon holds, and clears them. The tick posts PodUpdateOpen of round,
// with the rig's drift as its reasons, once to each claim of group with a session that has not
// retired, and nothing to any other claim; a retired claim is weighed by no round
// (reconcilePodUpdates, roundGroup). At a tick that opens the round (opening) it always does. At a
// tick that finds the round open it may post no opening at all instead, where it decided the round
// before openPodUpdate because no claim of the group waits for it (completePodUpdate), which it may
// do only where the rule owes no claim of the group with a session an ask; a round so decided may
// be replaced at that sweep, which posts the round's end to its members (endPodUpdates). So it never
// posts the opening to some of those claims and not to others. A claim of a token no machine holds
// has no inbox to read.
func (p *sweepPath) posts(group []supervise.Claim, round string, opening bool) (wrong []string) {
	got := map[claim.Token]int{}
	posted, ended := false, false
	for token, box := range p.posted {
		box.mu.Lock()
		items := box.items
		box.items = nil
		box.mu.Unlock()
		for _, item := range items {
			ev := item.ev
			if ev == (supervise.PodUpdateOpen{Claim: token, Round: round, Reasons: podUpdateDrift}) {
				got[token]++
				posted = true
				continue
			}
			if ev == (supervise.PodUpdateEnded{Claim: token, Round: round}) {
				ended = true
				continue
			}
			wrong = append(wrong, fmt.Sprintf("the sweep posted a %T to %s, want only PodUpdateOpen or PodUpdateEnded of round %s", ev, sweptToken(token), round))
		}
	}
	for token := range p.posted {
		i := slices.IndexFunc(group, func(c supervise.Claim) bool { return c.Token == token })
		want := (posted || opening) && i >= 0 && group[i].Session != "" && group[i].State != supervise.StateRetired
		if n := got[token]; (n == 1) != want || n > 1 {
			where := "where it posts to any claim"
			if opening {
				where = "at the round's opening"
			}
			wrong = append(wrong, fmt.Sprintf("the sweep posted %s the round %d times, want it posted once exactly where the group holds its claim with a session that has not retired, %s (in the group: %t)",
				sweptToken(token), n, where, i >= 0))
		}
	}
	if posted && ended {
		wrong = append(wrong, fmt.Sprintf("the sweep both posted round %s and ended it at one sweep", round))
	}
	if !posted {
		for _, c := range group {
			_, held := p.posted[c.Token]
			if owed, _ := c.OwesUpdateAsk(round); held && owed && c.Session != "" && c.State != supervise.StateRetired {
				wrong = append(wrong, fmt.Sprintf("the sweep posted the round to no claim of the group, and the rule owes %s an ask", describeSwept(c)))
			}
		}
	}
	return wrong
}

// sweepClaims is the store the machines a sweepPath builds write through: it takes every claim
// write and keeps nothing, so the ask mark a publish writes stays on the machine, and a claim a
// test built with a value the claims table would refuse is not written there. It answers every
// name of a claim's session as held by that claim, as a running daemon's table does for each
// claim's own session, and keeps nothing: the claims a test builds of different roles record the
// same session, and the outbox publishes to a claim only while it holds its session (the outbox's
// heldBack, supervise's Machine.HoldsItsSession), which is no fact of the round's rule.
type sweepClaims struct{ supervise.Store }

func (sweepClaims) PutClaim(context.Context, supervise.Claim) error { return nil }

func (sweepClaims) HoldSessions(_ context.Context, _ string, token claim.Token, holds []supervise.Hold) ([]claim.Token, error) {
	holders := make([]claim.Token, len(holds))
	for i := range holders {
		holders[i] = token
	}
	return holders, nil
}

// sweepClock is the rig's held clock as the machines a sweepPath builds read it, arming nothing: a
// machine built from a launching or registered claim arms its boot watch at once, and a timer per
// claim tried would be kept by the held clock for the whole test.
type sweepClock struct{ *heldClock }

func (sweepClock) AfterFunc(time.Duration, func()) supervise.Cancel { return sweepTimer{} }

type sweepTimer struct{}

func (sweepTimer) Stop() bool { return true }

// steadyPod is the runtime as a sweepPath's tick reads it: every pod is there and has drifted by
// the rig's drift, and nothing of the reads is recorded, since a test reads the pod hundreds of
// thousands of times. Everything else is the rig's runtime.
type steadyPod struct{ runtime.Runtime }

func (steadyPod) PodDrift(context.Context, runtime.Known) ([]string, error) {
	return []string{podUpdateDrift}, nil
}

// noPauseHolds is the holds of a project's pause as a sweepPath's tick reads them
// (supervision.pauseHoldsSince): none, which is what the store answers over the rig's database,
// where no pause is ever taken. It answers from memory because a test puts groups to the sweep
// hundreds of thousands of times, and a read of the store at each sweep is a database round trip.
func noPauseHolds(context.Context, string, time.Time) ([]store.PausePodHold, error) {
	return nil, nil
}

// sweepRig is the pod-update rig with a ready worker of every workflow role, so a machine holds
// the claim of each role's token, and the controller's token, like any token of no issue, is held
// by none, driven from the daemon's tick to the publish (sweepPath). The round's enqueue is
// recorded rather than written: a test puts groups to the sweep hundreds of thousands of times, and
// the outbox's own rows are the ask-path test's (podupdate_sweep_contract_test.go). Each recorded
// ask is then run as the outbox runs its row (outbox.execute), through the rig's supervisor, so it
// is published under the machine of the claim it names; what reaches each role's topic is read from
// the listener (heard).
// With nothing written, no ask of the round is ever queued, so each group is asked afresh.
type sweepRig struct {
	rig   *podUpdateRig
	path  *sweepPath
	asked []record.UpdateAsk
	lane  *outbox
	heard *holderPublisher
	row   int64
}

func newSweepRig(t *testing.T) *sweepRig {
	t.Helper()
	workers := make([]podUpdateRole, 0, len(claim.Roles))
	for _, role := range claim.Roles {
		workers = append(workers, readyWorker(role))
	}
	r := &sweepRig{rig: newPodUpdateRig(t, workers...), heard: &holderPublisher{}}
	r.rig.s.log = slog.New(slog.DiscardHandler)
	r.rig.s.enqueue = func(_ context.Context, issue string, rows ...record.OutboxPayload) (bool, error) {
		for _, row := range rows {
			ask, ok := row.(record.UpdateAsk)
			if !ok || issue != podUpdateIssue {
				t.Fatalf("the sweep enqueued %T on the issue numbered %s, want only update asks of issue number %s", row, issueNumber(issue), issueNumber(podUpdateIssue))
			}
			r.asked = append(r.asked, ask)
		}
		return true, nil
	}
	r.lane = &outbox{project: podUpdateProject, notices: r.heard, supervisor: r.rig.sup, log: slog.New(slog.DiscardHandler)}
	r.path = newSweepPath(r.rig)
	return r
}

// token is the token of role's claim: the rig's for a workflow role, and for the controller one no
// machine of the rig holds.
func (r *sweepRig) token(role claim.Role) claim.Token {
	if token, ok := r.rig.tokens[role]; ok {
		return token
	}
	return claim.Token("legion-legion-" + string(role))
}

// hold puts group, which holds one claim of a role at most, to the sweep's whole path from the
// daemon's tick to the publish (sweepPath), at a tick that finds the round open over the group's
// pod, and reports each claim of it the sweep treats otherwise than the rule. The sweep's list
// (unaskedClaims) must hold a claim exactly where it has a session and its own record owes it an
// ask (supervise's Claim.OwesUpdateAsk); the sweep must enqueue one ask of the round for a claim,
// and that ask's row must reach the claim's role topic once, exactly where the list holds the
// claim, its agent said it is ready (supervise.SaidReady) and a machine of this daemon holds it
// (asks); every row must run without an error; and the tick must post the round to each claim with
// a session that has not retired (sweepPath.posts). Each claim is read as its machine holds it
// (sweepPath.hold). asked is how many claims of the group are owed an ask that way.
func (r *sweepRig) hold(group []supervise.Claim) (asked int, wrong []string) {
	return r.drive(group, podUpdateRound)
}

// open is hold at the tick that opens the round: no claim of group records a round yet, so the tick
// reads the group live and posts the round's opening to each claim of it with a session that has
// not retired, and the rule owes no claim an ask until its own record holds the round.
func (r *sweepRig) open(group []supervise.Claim) (asked int, wrong []string) {
	return r.drive(group, "")
}

// asks is whether the sweep asks c: the rule owes c an ask of the round and c has a session
// (sweepOwed), its agent said it is ready (supervise.SaidReady), and a machine of this daemon holds
// it.
func (r *sweepRig) asks(c supervise.Claim) bool {
	_, held := r.rig.sup.Machine(c.Token)
	return sweepOwed(c) && supervise.SaidReady(c.State) && held
}

// drive is hold and open: the tick reads group in round, or at the round's opening where round is
// empty. A claim whose machine the path holds deciding (sweepHeld) is read by the tick as one that
// has not answered the round (roundGroup's mask), so the tick asks it as such; its decision ends
// before the outbox runs its rows, and the ask's row publishes under the claim's lock only where
// the rule still owes the claim an ask as the decision left it.
//
// The tick a group is put to must agree with the claims: at the tick that opens the round no claim
// records a round, and at a tick that finds it open each claim records one. The tick reads each
// machine's claim live, so the two ticks decide such a group alike, and a test that put a group to
// the other tick than its claims say would pass while it reached the other half of the sweep's
// path; such a group is reported here.
func (r *sweepRig) drive(group []supervise.Claim, round string) (asked int, wrong []string) {
	for _, c := range group {
		if (c.UpdateRound == "") != (round == "") {
			wrong = append(wrong, fmt.Sprintf("%s records the round %q and was put to the tick of round %q; want a group put to the tick that opens the round to record none, and one put to a tick that finds it open to record one",
				describeSwept(c), c.UpdateRound, round))
		}
	}
	r.asked = r.asked[:0]
	r.path.hold(group)
	defer r.path.end()
	listed := unaskedClaims(group, podUpdateRound)
	r.path.sweep(group, podUpdatePod, round)
	wrong = append(wrong, r.path.posts(group, podUpdateRound, round == "")...)
	r.path.letGo()
	r.heard.mu.Lock()
	r.heard.delivered = r.heard.delivered[:0]
	r.heard.mu.Unlock()
	for _, ask := range r.asked {
		r.row++
		payload, err := json.Marshal(ask)
		if err != nil {
			r.rig.t.Fatalf("encode the ask %+v: %v", ask, err)
		}
		row := record.OutboxRow{ID: r.row, Kind: record.OutboxKindUpdateAsk, Issue: podUpdateIssue, Payload: payload}
		if err := r.lane.execute(r.rig.ctx, row); err != nil {
			wrong = append(wrong, fmt.Sprintf("the row of the ask to the %s of round %s failed: %v", ask.Role, ask.Round, err))
		}
	}
	heard := map[string]int{}
	for _, published := range r.heard.delivered {
		heard[published.topic]++
	}
	for _, c := range group {
		owed, why := c.OwesUpdateAsk(podUpdateRound)
		wantListed := c.Session != "" && owed
		wantAsked := r.asks(c)
		wantHeard := wantAsked
		state := ""
		if h := r.path.held[c.Token]; h != nil {
			state = ", its machine " + r.path.view.what
		}
		if asks, reaches, ok := r.path.deciding(c, r.asks); ok {
			wantAsked, wantHeard = asks, reaches
			state += " and deciding as the tick read it"
		}
		gotListed, gotAsked := 0, 0
		for _, l := range listed {
			if l.Token == c.Token {
				gotListed++
			}
		}
		for _, a := range r.asked {
			if a.Role == c.Role && a.Round == podUpdateRound {
				gotAsked++
			}
		}
		topic := notify.RoleTopicPrefix + string(c.Token)
		gotHeard := heard[topic]
		delete(heard, topic)
		if wantHeard {
			asked++
		}
		if (gotListed == 1) != wantListed || gotListed > 1 || (gotAsked == 1) != wantAsked || gotAsked > 1 || (gotHeard == 1) != wantHeard || gotHeard > 1 {
			wrong = append(wrong, fmt.Sprintf("%s%s: the sweep's list holds it %d times, the sweep asks it %d times and %d asks reached its role, where the rule says owed=%t (%v), so want it listed %t, asked once %t and reached once %t",
				describeSwept(c), state, gotListed, gotAsked, gotHeard, owed, why, wantListed, wantAsked, wantHeard))
		}
	}
	for _, a := range r.asked {
		if a.Round != podUpdateRound || !slices.ContainsFunc(group, func(c supervise.Claim) bool { return c.Role == a.Role }) {
			wrong = append(wrong, fmt.Sprintf("the sweep asked %s in round %q, which is no claim of the group's round", a.Role, a.Round))
		}
	}
	for topic, n := range heard {
		wrong = append(wrong, fmt.Sprintf("%d asks reached the topic of %s, which is no claim of the group", n, sweptToken(claim.Token(strings.TrimPrefix(topic, notify.RoleTopicPrefix)))))
	}
	return asked, wrong
}

// sweptToken names the claim of token by its role and its issue's number, never by the token
// itself, which holds the issue's key: a failure text a report quotes carries no key form.
func sweptToken(token claim.Token) string {
	role, ok := token.Role()
	if !ok {
		return "a claim of no role"
	}
	return fmt.Sprintf("the %s claim of issue number %s", role, issueNumber(podUpdateIssue))
}

// issueNumber is the number of an issue's key, the part after its last '-': what a failure text
// names an issue by, so a text a report quotes carries no key form.
func issueNumber(key string) string {
	return key[strings.LastIndexByte(key, '-')+1:]
}

func describeSwept(c supervise.Claim) string {
	return fmt.Sprintf("a %s %s claim, session %q, locator %t, asked %v, answered %v, left %q, in round %q, pending task %t, told %t",
		c.State, c.Role, c.Session, c.Locator != nil, c.UpdateTaskedAt != nil, c.UpdateSignalledAt != nil, c.UpdateLeftRound,
		c.UpdateRound, c.Pending != nil, c.UpdateToldAt != nil)
}

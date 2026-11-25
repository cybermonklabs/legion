package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/treelifecycle"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// The tree lifecycle (internal/treelifecycle) is the store's barrier between a tree's runnable
// work and its cleanup, under every runtime. A claim is bound to its tree's open epoch when it is
// admitted (Store.AdmitClaim) and every launch rechecks that binding (Store.CheckLaunch) before it
// persists StateLaunching or calls the runtime. A reserved cleanup answers either with
// treelifecycle.ErrCleanupReserved, a wait that charges no budget and changes nothing but a claim
// whose process a retirement already stopped (relaunchRefused); the start that met it retries once
// the cleanup confirmed and a fresh admission opened the next epoch.

// StartWork is the work a workflow start brings its claim back for: the phase and the run its
// outbox row was queued for (record.SuperviseRequest's Phase and Generation), which the spawn, the
// rule's resume and the retry it asks for carry (RequestSpawn, RequestRuleResume, RequestRetry).
// The start's runner judges it on its own read of the issue, which the workflow's move of the
// issue does not wait on: a move that commits after that read and before the claim is brought back
// would leave a process started for work the issue has left, which nothing then stops, since no
// move stops a worker. So the machine judges it again under its own lock, with the one answer a
// queued task is judged by (Deps.PhaseHolds), where it brings the claim back (startHolds) and, for
// a launch the wake pacing parks, at that launch's turn (launchAfter). The zero StartWork — an
// operator's request, the boot's, a start of no phase — is judged by nothing.
//
// Agent is the agent of the step the start is a run of (record.SuperviseRequest.Agent,
// plan.Agent.ID), "" for a start of no step's agent. The admission that brings the claim back
// leaves the claim's session behind first where it is another agent's, and records Agent as the
// agent of the session it begins or takes (Claim.SessionAgent, revive).
//
// LeaveSession is the session the start's claim must not resume (record.SuperviseRequest's
// LeaveSession): the session the stored step of the start names (record.PlanStep.LeaveSession),
// which no writer sets now, since record.NameAgents gives no agent a name a key held. The claim
// leaves it where it still records it, so the step's agent starts in a session of its own
// (revive); a claim that records another session has left it already, and resumes that one.
type StartWork struct {
	Phase        phase.Phase
	Generation   uint64
	Agent        plan.AgentID
	LeaveSession string
}

// Leaves is whether start asks the claim to leave the session it records now (StartWork's
// LeaveSession): the one test of it, which the machine's own resume and retry ask before they
// refuse a session the claim does not hold (resumeByRule, retry) and the outbox's start asks
// before it suspends a claim that still runs that session (internal/daemon's suspendToLeave).
func (m *Machine) Leaves(start StartWork) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.leaves(start)
}

// leaves is Leaves under the machine's lock, which every caller inside the machine already holds.
func (m *Machine) leaves(start StartWork) bool {
	return start.LeaveSession != "" && m.claim.Session == start.LeaveSession
}

// ErrStartLeft is a start the machine judged no longer the work of its claim's issue
// (startHolds): the issue has left the phase, the step or the run the start was queued for. The
// claim is left exactly as it was, with no process started, and the start's runner finishes its
// row without acting, as its own read would have had it come later (internal/daemon's outbox).
var ErrStartLeft = errors.New("the issue has left the phase or the step this start was queued for")

// startHolds is ErrStartLeft where start's work is not the work of the claim's issue now, nil where
// it is or names none, and the read's error where the record could not be read. It runs under the
// machine's lock, before anything of the claim changes.
func (m *Machine) startHolds(ctx context.Context, start StartWork) error {
	holds, err := m.phaseHolds(ctx, Delivery{Phase: start.Phase, Generation: start.Generation})
	if err != nil {
		return err
	}
	if !holds {
		return fmt.Errorf("start of %s for phase %s of run %d: %w", m.claim.Token, start.Phase, start.Generation, ErrStartLeft)
	}
	return nil
}

// revive binds an explicitly spawned, resumed or retried claim to the open tree epoch before
// launching. In-lifetime recovery only rechecks the bound epoch (checkLaunch), so an old launch
// cannot cross a cleanup confirmation into a later admission. request names the decision that
// brings the claim back, for a refusal and the line. exempt is whether the request that asked
// for it is one the wake pacing never makes wait (exemptWake); every caller reads it from its own
// event, so a request that carries no such field is paced. turn is the moment the request
// already reserved for this launch in the pacing (RequestSpawn.Turn), the zero time for every
// request that reserved none (launchAfter). start is the work a workflow start brings the claim
// back for, which its caller has judged already (startHolds) and the launch judges again where a
// park it waits in ends (startLeftWhileParked).
//
// A start that leaves the session the claim records (leaves, StartWork's LeaveSession) forgets it
// once the admission has bound the claim, so the launch spawns a fresh agent, as retry's fresh
// session does: no agent but the key whose session it is resumes it. Whether the claim may clear
// it is asked first (mayForget), and a refusal there, as a refused admission, leaves the claim
// exactly as it was. Every caller reaches it with a claim that runs nothing — queued, suspended,
// failed or retired — since the outbox suspends a running claim first (internal/daemon's
// suspendToLeave), so the forget writes no record of a process that still runs.
//
// admitted, where a caller gives one, runs once the admission has bound the claim and before
// anything else of the start: it is where a caller puts what it may change only for a claim
// that is being run again, since an admission the lifecycle refuses leaves the claim exactly as
// it was (retry, whose failure record, budgets and new session go there).
//
// The admission is also where the claim's session passes to start's agent, and it is decided here
// alone, for every decision that brings the claim back: a session that is another agent's
// (SessionOfAnotherAgent) is left behind, so the launch spawns start's agent in a session of its
// own, and start's agent is recorded as the agent of the session the launch begins or takes
// (Claim.SessionAgent). A session the claim does not hold is left only once its role no longer
// leads to the process under it (mayForget), which is judged before the admission; until then the
// decision is refused and the claim left as it was. A refused admission leaves the session and the
// record of its agent as they were, as it leaves the rest of the claim, so the agent that session
// is of still resumes it at its own next start.
//
// The claims it starts — spawned, resumed, retried, a suspended claim given a task — record no
// process, so its launch lets none go and records nothing of one (noEnd). Each was raised by a
// request, which the pod's start gate puts first among the starts waiting in the claim's pod
// (launchAfter's requested, podQueue.next).
func (m *Machine) revive(ctx context.Context, request string, exempt bool, turn time.Time, start StartWork, admitted func()) error {
	another := m.claim.SessionOfAnotherAgent(start.Agent)
	leave := !another && m.leaves(start)
	if another || leave {
		if err := m.mayForget(ctx, request); err != nil {
			return err
		}
	}
	bound, err := m.deps.Store.AdmitClaim(ctx, m.claim)
	if err != nil {
		return err
	}
	m.claim.TreeEpoch = bound.TreeEpoch
	if another {
		held := m.claim.SessionAgent
		m.log.Info("supervise: the step this start is for is another agent's than the one whose session the claim holds, so it starts in a new session",
			"request", request, "agent", start.Agent, "sessionAgent", held, "session", m.claim.Session)
		// The session it drops is recorded as the earlier agent's (AbandonedSession.Agent), so the
		// new agent's first task tells it as another agent's (abandonedTask), as it tells one that
		// agent lost before this start, where the claim holds no session to drop.
		m.forgetSession(fmt.Sprintf("the step it was started for is another agent's (%s) than the one whose session it held (%s)",
			start.Agent, held))
		// The cause of a forget the machine keeps is the earlier agent's too, where its fresh
		// session never registered (Machine.lostCause): the new agent's launch is built with none.
		m.lostCause = ""
	}
	if start.Agent != "" {
		m.claim.SessionAgent = start.Agent
	}
	if admitted != nil {
		admitted()
	}
	if leave {
		m.log.Info("supervise: the start leaves the session a key's agent ran in, so it spawns a fresh agent", "session", m.claim.Session)
		m.forgetSession("its step's agent took a name a key's agent held in an earlier generation, so it starts in a session of its own: this session was that key's")
	}
	return m.launchAfter(ctx, false, exempt, true, turn, noEnd, start)
}

// SessionOfAnotherAgent is whether the claim's session is the session of another agent than agent,
// the agent of the step a start is a run of (StartWork.Agent): both are recorded and they differ.
// A start that names no agent, and a claim that records none, take the session the claim holds.
func (c Claim) SessionOfAnotherAgent(agent plan.AgentID) bool {
	return otherAgent(c.SessionAgent, agent)
}

// otherAgent is whether held, an agent a claim recorded, and agent are two agents: both are
// recorded, since "" names none (plan.AgentID), and they differ.
func otherAgent(held, agent plan.AgentID) bool {
	return agent != "" && held != "" && held != agent
}

// TakeSessionFor records agent, the agent of the step a start is a run of (StartWork.Agent), as
// the agent of the session of a claim that runs a process and records no agent (SessionAgent ""):
// a claim a release before the record started, or one an operator's spawn started for no step.
// The start's task goes to that process, so the session is that agent's from here, and a later
// start of another agent finds it so (SessionOfAnotherAgent) and starts its own agent in a new
// session. Without it every agent whose start reached the running claim would share its session.
// A claim that runs nothing is left alone: the decision that brings it back records the agent
// once the claim is admitted (revive), and a refused admission leaves the record as it was. So is
// a claim that records an agent, and a start that names none. Its one caller is the outbox's start,
// under the machine's lock taken here, after the start's own record (StartedBy).
//
// The start read its step from the record before this lock, so it asks with the step's standing
// (Standing), judged here under the lock before anything is recorded where no earlier act of the
// start has passed it: a step its issue has left since, by a move or a later entry, is
// ErrStepLeft, and the claim records no agent. Recorded for a stale start, the agent would hold
// the session though its task is never delivered, and the next start, of the agent whose session
// it is, would find another agent's and start a new session.
func (m *Machine) TakeSessionFor(ctx context.Context, agent plan.AgentID) error {
	m.mu.Lock()
	defer m.unlock()
	if agent == "" || m.claim.SessionAgent != "" || m.claim.RunsNothing() {
		return nil
	}
	if err := judgeStanding(ctx, m.claim.Token, true); err != nil {
		return fmt.Errorf("record the agent %s of the session of claim %s: %w", agent, m.claim.Token, err)
	}
	m.log.Info("supervise: a running claim that recorded no agent of its session takes the agent of the start that reached it",
		"agent", agent, "session", m.claim.Session, "state", m.claim.State)
	m.claim.SessionAgent = agent
	return m.persist(ctx)
}

// checkLaunch is the lifecycle recheck every launch makes first: a claim whose tree cleanup is
// reserved, or whose bound epoch is no longer the open one, starts nothing and changes nothing, so
// a relaunch refused here charges no budget.
func (m *Machine) checkLaunch(ctx context.Context) error {
	return m.deps.Store.CheckLaunch(ctx, m.claim)
}

// relaunchRefused is a relaunch checkLaunch refused, relaunch the call that made it
// (relaunchAfterFailure, launchAfter, runWake) and so the one to try again. A claim that records
// its process, or runs none — a spawn, a resume, a retry — is left exactly as it was: the error
// goes back to whoever asked for it, and this writes nothing, arms nothing and starts nothing.
//
// A claim that records its process is left stored working over it where a death or a
// displacement raised the relaunch, and nothing of the claim is written to say so: this
// refusal is one of the windows Claim.ProcessGone is read for, and what that field is and
// what reads it is written there. It is no record this refusal makes, and none it clears —
// the report that raised the relaunch made it, and the next report of the same process, the
// one that raises the relaunch again, is what changes it.
//
// The credit of the refresh gate a machine may hold goes back at the end of the decision this
// refusal belongs to, which is where every credit of this lane goes back (endRefreshLane,
// dispatch://INFRA-2712 R1). This is the refusal of every launch there is, so a refused launch
// of the stale-plugin lane loses its credit wherever the refusal came from — the launch path's
// own pre-check, the recheck the wake's turn makes and the relaunch a failure raises
// (launchAfter, runWake, relaunchAfterFailure) — and the one shape the reader has to hold is
// that a refused launch raises no wake, so nothing of that refresh waits in the pacing past
// this line.
//
// No refresh wake can be waiting on the machine when this runs, and the refusal parks none, so
// the credit of a refused launch of this lane is one nothing is left waiting for: a credit is
// taken over a claim that holds no wake at all (refreshPlugin's own guard), the turn of a parked
// wake takes it off the machine before anything is rechecked (wakeNow, runParkedLaunchNow), and
// no decision of this package raises a launch over one still waiting (parkWake lists them).
//
// Why the retry such a refusal may arm carries the intent, and what it costs the gate, is where
// retryHolding and the gate state it (retryHolding, RefreshGate, Correct): the launch that retry
// makes then waits under the lane that raised it rather than under the launch kind every other
// launch takes (reserveWake, dispatch://INFRA-2712 R4). A retry armed here with the bare
// relaunch would park under `launch`, where the reader this kind exists for counts no refresh of
// a claim the lane is still working through, and where the correction the sweep makes from the
// lane's own parked wakes never counts it at all (api.State.ClaimsToRefresh,
// refreshStalePlugins in internal/daemon).
//
// Two roads reach this line with an intent on the machine, and each is a retry of this lane
// refused a second time: the retry a failed write of StateLaunching armed (launchTries), and the
// retry the refused turn of this lane's own parked wake armed (runWake). Both put the intent
// back as they run and raise the same launch anew from the top, whose own lifecycle check is
// refused here (launchAfter, retryHolding).
//
// The other two callers hold no intent where they refuse, so the wrapping is inert for them.
// The wake's turn took the intent off the wake it was parked with and puts it back only past
// both steps, which is past this refusal, so that caller wraps the intent its own wake carried
// (runWake, holdPluginRelaunch). A relaunch after a launch failure is no relaunch of this lane
// at all: the charged road a stale registration takes raises none, and nothing else of that
// path does either (relaunchAfterFailure, staleRegistration).
//
// What one refused relaunch leaves behind is put right by the caller that left it, never here.
// The one such thing is the watch a park took off the process the relaunch was to replace, and
// it belongs to the launch the wake pacing held: that turn is where it is armed again (runWake,
// watchAgain). This is the refusal of every launch there is — launchAfter's own pre-check, a
// relaunch after a failure, a refused retry or resume over a claim that runs no process — and
// those launches park nothing, so arming a timer for one would arm what the fence has just
// fired.
//
// No plugin mark is touched here, by this refusal or by any other. A stale-plugin relaunch
// writes PluginRelaunchFor where its launch starts the process that replaces the stale one
// (launchNow, commitPluginRelaunch), so a refused relaunch has written none, and the mark the
// claim carries names the relaunch that did run — which is what the charged second stale
// registration in a row rests on (staleRegistration).
//
// A live claim that records none has already let its process go: a retirement's relaunch whose
// stop already ended the process (the registration deadline and a prompt retirement check before
// they stop, so the check failed while the stop waited out its grace), or a launch whose write
// failed, waiting (relaunchLater). A claim left there would have nothing watching or relaunching
// it. A reserved cleanup suspends it for the daemon, its session kept, for the tree's close to
// retire. Any other failure is not the tree's — a store that did not answer — and ends nothing:
// the relaunch waits (relaunchLater), as a stop that fails does.
func (m *Machine) relaunchRefused(ctx context.Context, relaunch func(context.Context) error, err error) error {
	if m.claim.Locator != nil || !slices.Contains(live, m.claim.State) {
		return err
	}
	if errors.Is(err, treelifecycle.ErrCleanupReserved) {
		m.log.Warn("supervise: the tree's cleanup is reserved and the retired process is stopped; suspending the claim for the tree's close", "error", err)
		return errors.Join(err, m.suspended(ctx, SuspendedByDaemon, nil))
	}
	m.log.Warn("supervise: the lifecycle check of the retired process's relaunch failed; trying it again at the probe interval", "error", err)
	m.relaunchLater(m.deps.Timeouts.Probe, m.retryHolding(m.pluginRelaunch, relaunch))
	return errors.Join(err, m.persist(ctx))
}

// watchAgain arms again what watches the process the claim records, where that is not armed: the
// timers the claim's state calls for, which are the ones a restart reading this claim back from
// the store would arm for it (NewMachine), and the bound an acknowledged prompt waits for its
// turn on. Its one caller is the refusal a parked launch's turn meets (runWake), over a claim
// that keeps its process with the launch that was to replace it gone: the park disarmed the boot
// interval, the registration deadline, the probe interval and the turn bound of that process
// (parkWake), the claim keeps it and the wake is spent, so a booting process whose agent never
// registers would meet no deadline and a prompt the agent acknowledged would wait for its turn
// on nothing.
//
// A claim still coming up is watched by its boot interval and its registration deadline, a
// registered one by the deadline alone, and a claim whose agent is ready by neither: a live
// session is watched by the runtime's sweep (observe) and by its own connection, and the probe
// interval is an uncertain answer's retry rather than a watch of the claim's — the sweep asks
// again at its own pass. The turn bound goes with them for a prompt Oh My Pi acknowledged and no
// turn has started on (acked, Machine.outstanding), for the same RPC wait that prompt was given,
// so one the agent read and never turned on still fails rather than waiting for ever (noTurn).
//
// A timer already armed keeps the moment it already holds: the park takes only the four timers
// above, so whatever the claim still holds is a watch nothing of this launch disturbed, and
// arming again over it would hand the process a fresh boot interval and a fresh deadline at
// every refusal.
func (m *Machine) watchAgain() {
	if m.claim.Locator == nil {
		return
	}
	armed := func(kind TimerKind) bool {
		_, ok := m.timers[kind]
		return ok
	}
	switch m.claim.State {
	case StateLaunching, StateShimConnected:
		if !armed(TimerBoot) {
			m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
		}
		if !armed(TimerRegistration) {
			m.armRegistration()
		}
	case StateRegistered:
		if !armed(TimerRegistration) {
			m.armRegistration()
		}
	}
	if p := m.claim.Pending; p != nil && m.outstandingPrompt(p.ID) && p.ConfirmedAt.IsZero() && !armed(TimerTurn) {
		m.arm(TimerTurn, m.deps.Timeouts.RPC, p.ID)
	}
}

// relaunchLater leaves a claim whose process is already let go waiting launching with no process,
// its task kept, and tries relaunch again after wait (TimerRelaunch, relaunchAgain): a relaunch
// whose lifecycle check the store did not answer (relaunchRefused), or a launch whose write failed
// (launchAfter), each at the probe interval, and a start the session store could not answer, at
// its own minute (startWaitsForStore). Nothing else would relaunch such a claim before a restart,
// which takes it up from what the store last holds.
func (m *Machine) relaunchLater(wait time.Duration, relaunch func(context.Context) error) {
	m.claim.State = StateLaunching
	m.relaunch = relaunch
	m.arm(TimerRelaunch, wait, "")
}

// relaunchAgain is the relaunch relaunchLater left waiting (TimerRelaunch). A suspension held
// meanwhile ends here, with no process left to stop, rather than in a relaunch — a stop that meets
// the relaunch it ends, which a project pause's stop owes the role (suspendedFor); otherwise the
// relaunch is tried again as it was first made, charged as that call charges it.
func relaunchAgain(m *Machine, ctx context.Context, _ Event) error {
	relaunch := m.relaunch
	m.relaunch = nil
	if m.held != nil {
		return m.suspendedFor(ctx, *m.held, true)
	}
	return relaunch(ctx)
}

// RunsNothing is whether the claim is in a state with no process of its own (RunsNothingStates):
// only such a claim may move to another tree.
func (c Claim) RunsNothing() bool { return slices.Contains(runsNothing, c.State) }

// Gone is whether the claim is suspended, failed or retired (GoneStates): its process has been
// let go, so a project's pause has nothing of it to stop.
func (c Claim) Gone() bool { return IsGone(c.State) }

// Retree re-points a claim to tree, the tree its issue belongs to now: a child of a closed tree
// re-admitted as a root of its own keeps its roles' claims, which still name the tree it left. The
// claim drops its old tree's epoch, so its next start binds the new tree's lifecycle and launches
// in the new tree's resources, and keeps its session: the session is the issue's, on the host under
// tmux, and under a sandbox in the runtime's session database (runtime.kubernetes.session_store
// postgres), which every tree's pods read, so it resumes in the new tree while the old tree
// lingers, whatever the old tree's cleanup takes. Only a claim that runs nothing is re-pointed; one
// whose process still runs in the old tree is a wait (wait.ErrWaiting): the new tree's start
// suspends it first, keeping its session, and waits for that suspension to land (the daemon's
// outbox, stopElsewhere).
func (m *Machine) Retree(ctx context.Context, tree string) error {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.Tree == tree {
		return nil
	}
	if !m.claim.RunsNothing() {
		return wait.Errorf("re-point claim %s from tree %s to %s: it is %s there", m.claim.Token, m.claim.Tree, tree, m.claim.State)
	}
	m.log.Info("supervise: the claim's issue moved to another tree", "from", m.claim.Tree, "to", tree)
	m.claim.Tree, m.claim.TreeEpoch = tree, 0
	return m.persist(ctx)
}

// ErrRunsAProcess is Rerole's refusal of a claim that still runs a process: that process was
// started with the claim's role's prompt and App, so the role it holds is the one it runs.
var ErrRunsAProcess = errors.New("the claim still runs a process")

// ErrStepLeft is a machine's refusal of a request whose reading of the record no longer holds
// under the machine's lock (Standing): the claim it read as the worker on a role's step works it no
// more, or the step a start is for is one its issue has left. Nothing of the claim changed.
var ErrStepLeft = errors.New("the record no longer asks this claim for the step the request was made for")

// standingKey is the context key Standing puts a request's standing under.
type standingKey struct{}

// standing is a request's reading of the record (Standing), and whether an act of the request has
// passed it under a machine's lock.
type standing struct {
	stands func(context.Context) (bool, error)
	mu     sync.Mutex
	passed bool
}

// Standing is ctx carrying stands, the reading of the record a request on a claim was made on: the
// claim a route read as the worker on a role's step (api's workerToken), the step a start is for
// (the daemon's outbox). The request reads the record before any lock its acts take, so each act a
// machine decides for it under its lock — Handle, and so Compact, and Rerole — judges that reading
// again there, before anything acts, and refuses with ErrStepLeft where it no longer holds.
//
// A request is the record's from the first act that passes: stands is asked until it first answers
// true under a machine's lock, and every later act of the same request reads nothing again. Its
// acts are several decisions under several takings of the lock, so a move that commits after the
// first finds the request under way, as it would had the whole request run before it, and no
// request is left half done by a move after it began. A request whose first act is refused did
// nothing at all. The judgment a suspension makes before it cancels a start in flight (Handle) is
// a filter, and passes nothing: the one that counts is under the lock.
func Standing(ctx context.Context, stands func(context.Context) (bool, error)) context.Context {
	return context.WithValue(ctx, standingKey{}, &standing{stands: stands})
}

// judgeStanding judges the standing ctx carries for an act on the claim token (Standing), and
// answers nil for a ctx that carries none. pass is whether this judgment is one under a machine's
// lock, which commits the request once it holds.
func judgeStanding(ctx context.Context, token claim.Token, pass bool) error {
	s, ok := ctx.Value(standingKey{}).(*standing)
	if !ok {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.passed {
		return nil
	}
	holds, err := s.stands(ctx)
	if err != nil {
		return fmt.Errorf("supervise: read again what the request on %s was made on: %w", token, err)
	}
	if !holds {
		return fmt.Errorf("supervise: claim %s: %w", token, ErrStepLeft)
	}
	s.passed = pass
	return nil
}

// Rerole gives a claim the role of its agent's next step, which a step of another role than the
// claim holds asks for (the daemon's outbox, the start of a phase entry): one key is one claim
// across roles, and its next generation registers, is prompted and acts as the App of the role it
// holds then. Only a claim that runs nothing takes a new role, so a process keeps the role it was
// started with; one that still runs is ErrRunsAProcess, and the start suspends it first, keeping
// its session, and waits for its turn to end. The claim keeps its session, so its next generation
// resumes it, and that session can hold a GitHub token of the earlier role that the agent printed:
// the claim records the change (RoleChange), unless one it has not told yet stands, and the first
// prompt its agent takes says such a token is no longer its to use (roleChangedTask).
//
// The start read the step from the record before this lock, so it asks with the step's standing
// (Standing), and the first act of the start to pass that standing under a machine's lock commits
// the start. Where Rerole is that first act, a step its issue has left since, by a move back or a
// later entry, is ErrStepLeft, and the claim keeps the role it holds. Where a suspension the start
// asked for landed at once, that suspension committed the start and Rerole judges nothing: a move
// that committed in between leaves the claim with the step's role all the same, StartWork then
// refuses to bring it back for the step the issue left (startHolds, through Deps.PhaseHolds), and
// the next start of the claim's agent gives the claim the role of its own step.
func (m *Machine) Rerole(ctx context.Context, role claim.Role) error {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.Role == role {
		return nil
	}
	if !m.claim.RunsNothing() {
		return fmt.Errorf("give claim %s the role %s for its agent's next step: it is %s as %s: %w",
			m.claim.Token, role, m.claim.State, m.claim.Role, ErrRunsAProcess)
	}
	if err := judgeStanding(ctx, m.claim.Token, true); err != nil {
		return fmt.Errorf("give claim %s the role %s for its agent's next step: %w", m.claim.Token, role, err)
	}
	m.log.Info("supervise: the claim takes the role of its agent's next step", "claim", m.claim.Token, "from", m.claim.Role, "to", role)
	if !m.claim.RoleChange.Tells() {
		m.claim.RoleChange = RoleChange{From: m.claim.Role}
	}
	m.claim.Role = role
	return m.persist(ctx)
}

// Package supervise is process supervision: one state machine per role claim, and the only place
// the runtime's process facts, the worker stream's agent facts, the machine's own timers, and the
// API's requests meet.
//
// A machine is the claim's one serialised decision owner. Every event enters through Handle,
// which holds the machine's lock for the whole decision — including the runtime call a decision
// makes (a spawn, a stop, a probe). That is what makes the fences work: an event that arrives
// while a relaunch is in the runtime waits, and is then judged against the claim the relaunch
// left, not the one it replaced. The one thing a decision never waits on is a prompt: a send runs
// on its own goroutine and posts its outcome back as PromptAcked or PromptRefused, so a slow agent
// cannot hold the claim. (The only agent round trip a decision does wait on is the get_state a
// restored claim asks once, bounded by the RPC timeout.)
//
// The package reads no configuration and knows neither the worker stream nor any runtime's
// implementation: its limits and timeouts are constructor parameters, the connection directory is
// runtime.Conns, and the runtime is runtime.Runtime.
package supervise

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backoff"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// ClaimState is where a claim is in its life.
type ClaimState string

const (
	// StateQueued: the claim exists and nothing has been launched for it.
	StateQueued ClaimState = "queued"
	// StateLaunchUncertain: the previous daemon started a pane but crashed before recording its
	// locator, and this daemon cannot yet reconcile that pane. It survives a restart and refuses a
	// new spawn until reconciliation proves the old pane absent or reaped.
	StateLaunchUncertain ClaimState = "launch_uncertain"
	// StateLaunching: a process was started (or is being started) and its shim has not said hello.
	StateLaunching ClaimState = "launching"
	// StateShimConnected: the shim said hello with this launch's boot token. Under always-dial the
	// shim connects before it starts Oh My Pi, so this says nothing about the agent yet.
	StateShimConnected ClaimState = "shim_connected"
	// StateRegistered: the agent registered — the session is recorded and its secret issued.
	StateRegistered ClaimState = "registered"
	// StateReady: the agent finished booting and can be prompted.
	StateReady ClaimState = "ready"
	// StateWorking: a turn is in flight or a compaction is in progress (compact.go's compactStart).
	StateWorking ClaimState = "working"
	// StateIdle: the agent finished a turn and can be prompted.
	StateIdle ClaimState = "idle"
	// StateSuspended: the process was stopped and the session kept; the claim resumes on demand.
	StateSuspended ClaimState = "suspended"
	// StateFailed: a budget ran out, or a launch was refused in a way no try changes (a system
	// prompt past the start frame, promptRefused). Nothing relaunches the claim on its own.
	StateFailed ClaimState = "failed"
	// StateRetired: the claim is over — stopped by the operator, or ended by its agent.
	StateRetired ClaimState = "retired"
)

// allStates is every ClaimState, in the order the constants above declare them.
// TestStatesIsEveryDeclaredStateOnce holds it to the constants the compiler sees in this package,
// so a state added above and left out here fails that test.
var allStates = []ClaimState{
	StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered, StateReady,
	StateWorking, StateIdle, StateSuspended, StateFailed, StateRetired,
}

// States is every state a claim can be in, in the order they are declared: the one list a test
// outside this package walks to cover each state. It is a copy, so no caller changes the
// package's own list.
func States() []ClaimState {
	return slices.Clone(allStates)
}

// SuspendSource is who suspended a claim. The daemon resumes a suspended claim by rule on the
// triggers main had — its phase's start, a workflow delivery, the controller keeper
// (RequestRuleResume) — unless an operator suspended it: that suspension holds until the operator
// resumes the claim. The boot's resume by rule, which main did not have, acts only on a source
// ByRule names, so it never overrides a drain recorded before the source existed, and it leaves a
// released claim to the wake that brings its pod back.
type SuspendSource string

const (
	// SuspendedByUnknown is the source migration 0038 assigns every claim that was already
	// suspended before the source existed: most such rows are the daemon's own (a lingering tree, a
	// parked child, a re-entry's interrupted worker), and main's triggers resume them as before.
	SuspendedByUnknown SuspendSource = "unknown"
	// SuspendedByOperator: the operator's suspend route.
	SuspendedByOperator SuspendSource = "operator"
	// SuspendedByDaemon: the workflow's own suspend — the issue left the workflow, or its tree
	// lingers.
	SuspendedByDaemon SuspendSource = "daemon"
	// SuspendedByExit: the tree root's agent reported its own exit, which suspends a root.
	SuspendedByExit SuspendSource = "exit"
	// SuspendedByReleased: the daemon released the claim's idle issue pod, whose disk and Sandbox
	// went with it; a release takes no pod whose workspace holds a commit on a working copy or a
	// bookmark that origin does not hold, or a working-copy change. The
	// claim keeps its phase and its pending task, and the next wake for its role resumes the same
	// session; the boot's resume by rule resumes no such claim, since only that wake brings the pod
	// back (workflow's booted).
	// Two other steps can start such a claim: admission's pass sends the work of an entry to a
	// released claim behind the entry its phases row names (admit's owedEntryRoles), and, once per
	// project at a boot, the cutover repair owes a start to each merger the `awaiting_merge`
	// cutover left released with no wake to come (workflow's cutoverMergerRepair). It is no source
	// ByRule names: the rules that act on a released claim name it themselves — the wake that brings
	// its pod back, the stranded report once a wake has run out with no turn, and that cutover
	// repair.
	SuspendedByReleased SuspendSource = "released"
)

// allSuspendSources is every SuspendSource, in the order the constants above declare them.
// TestSuspendSourcesIsEveryDeclaredSourceOnce holds it to the constants the compiler sees in this
// package, so a source added above and left out here fails that test.
var allSuspendSources = []SuspendSource{
	SuspendedByUnknown, SuspendedByOperator, SuspendedByDaemon, SuspendedByExit, SuspendedByReleased,
}

// SuspendSources is every source a suspended claim can record, in the order they are declared:
// the one list a test outside this package walks to cover each source. They are the values the
// store's check constraint allows beside the empty source of a claim that is not suspended
// (internal/store/migrations/20261101201850_claim_suspended_by_released.up.sql). It is a copy, so
// no caller changes the package's own list.
func SuspendSources() []SuspendSource {
	return slices.Clone(allSuspendSources)
}

// ByRule is whether the source is one the daemon itself recorded as a stop of its own — its own
// suspend, or a root's exit: the sources a daemon rule resumes with no operator, and the only
// suspended phase workers the stranded report leaves out. A release is not one of them. It is a
// stop the daemon recorded too, but no rule here acts on it: the boot's resume by rule leaves it to
// its role's wake (workflow's booted), and the stranded report holds it to R6's one fact
// (workflow's stranded).
func (s SuspendSource) ByRule() bool {
	return s == SuspendedByDaemon || s == SuspendedByExit
}

// RetiresPhaseTask is whether a suspension by this source ends the claim's phase, so a task queued
// for that phase goes with it (settle). Every source does but two: an operator's, and a release.
// Each of those resumes this same claim in the same phase — the operator's by hand, a released
// claim's at its role's next wake — so the pending task waits through the suspension and is sent
// on that resume.
func (s SuspendSource) RetiresPhaseTask() bool {
	return s != SuspendedByOperator && s != SuspendedByReleased
}

// SessionLoss is why a launch forgot the session its claim recorded and started a fresh agent
// (startFresh), and "" for a launch that lost nothing (Claim.SessionLost).
type SessionLoss string

const (
	// SessionLossGone: no session store holds the session any more. The runtime looked for it and
	// did not find it (runtime.ErrSessionFileMissing).
	SessionLossGone SessionLoss = "gone"
	// SessionLossNotResumedByID: the pod's launcher could not look the session up by its id, and
	// Oh My Pi did not resume it by that id: the start that had Oh My Pi resolve the id itself did
	// not register as the session (dispatch://INFRA-4108 R3). Nothing of the session was deleted.
	SessionLossNotResumedByID SessionLoss = "not-resumed-by-id"
	// SessionLossClaudeNoFile: a Claude Code claim records a session with no transcript file.
	SessionLossClaudeNoFile SessionLoss = "claude-no-file"
)

// Claim is one role claim as the supervisor holds and persists it.
type Claim struct {
	Token      claim.Token
	Project    string
	Tree       string
	Issue      string
	Role       claim.Role
	Generation uint64
	// TreeEpoch is the durable lifecycle epoch this claim bound to before its first persistence.
	// A cleanup reservation closes that epoch before a later claim/launch can write or run.
	TreeEpoch uint64
	// Session and SessionFile are the agent's reported session id and absolute transcript path.
	Session     string
	SessionFile string
	// Abandoned is the session this claim stopped resuming when it last forgot one
	// (forgetSession), and why. The session itself stays in its store — nothing of it is deleted
	// — so this is the one record of where it went, for the operator who may want to read it and
	// for the agent that replaces it, which is told of it in the first task it is given
	// (abandonedTask). It is persisted, since that first task can be a launch and a daemon
	// restart away, and it is replaced rather than cleared by the next abandonment: the session
	// before this one is gone whatever happens next. A claim that has abandoned none records a
	// session of "".
	Abandoned AbandonedSession
	// SessionRun is what the record says of the recorded session since it last finished a turn:
	// how many of its launches ended in a way the machine charges, how the last of them ended,
	// and whether the session has finished a turn at all (SessionRun, dispatch://INFRA-2045 R1).
	// It is persisted, so a daemon restart reads the count the daemon before it kept, and it goes
	// with the session it is of (forgetSession).
	SessionRun SessionRun
	// EndedWork is what the last let-go of the claim's process ended of the work its agent had
	// running, for the next process to be told (EndedWork, recordEnd). It is persisted, since the
	// prompt that tells it can be a relaunch, a wake and a daemon restart away, and a newer
	// let-go replaces it — as the union of the two while it is still untold (joined). A claim
	// whose let-gos ended nothing worth telling records none.
	EndedWork EndedWork
	// ServingGeneration is the issue generation of the last confirmed delivery this claim retired:
	// the run it goes on working until another delivery of a run replaces it. It is the second of
	// the three sources ServingRun reads (serving): the pending delivery whose turn is running,
	// then this, then — for a claim serving no run — a pending task the agent may have read. The
	// completion route adds a fourth of its own (api/handoff.go): where those three leave the
	// claim serving no run, it reads the held task directly and takes that task's run, but only
	// when the task is of the issue's own current generation and phase. It is what answers for a
	// worker told to wait, whose turn's end retired its delivery and whose next turn an Envoy
	// notice starts with no delivery behind it. Zero is a claim that has retired no confirmed
	// task of a run.
	ServingGeneration uint64
	// GivenEntry is the phase entry of the last task this claim was queued: the daemon's own
	// record of what this role was given, beside ServedEntry, which is what its worker took. It
	// is what lets the difference between a role's work and the work its claim holds be read
	// off the record rather than off the outbox row that carried the task: a claim whose number
	// is below the entry its role's phases row names (record.PhaseRow.Entry) is owed that
	// entry's own task, and one level with it is already holding it. Zero is a claim that has
	// been queued no task carrying an entry, which every claim of a daemon upgraded in place is
	// until its role's next start. Three decisions write it: queue, in the transaction that
	// writes the delivery carrying the entry; retirePending, which puts it back to ServedEntry
	// when it retires that entry's task unconfirmed, so the entry is owed again; and EntryGiven,
	// for the start whose task the outbox holds back because the claim holds that entry's task
	// already.
	GivenEntry int64
	// ServedEntry is the phase entry of the last confirmed delivery this claim retired, the
	// entry side of ServingGeneration: what ServingEntry answers with once no delivery backs the
	// worker's turn. Zero is a claim that has retired no confirmed task of an entry, which every
	// claim of a daemon upgraded in place is until its role's next start.
	ServedEntry int64
	// Handshake is the handshake between the daemon and this claim's worker over the last phase
	// entry the claim was queued a task of: that entry, the issue generation the task was of, when
	// a task of that entry was first queued to the claim and when a turn of its worker first
	// confirmed one (EntryHandshake). It outlasts the task it was read from, which the end of the
	// confirming turn retires, so it is what the state reads an issue's two times from
	// (api.Issue.EntryQueuedAt and EntryConfirmedAt), and for a tree's own root it is its
	// architect's in every phase the root stands in. The zero value names no entry, which every
	// claim of a daemon upgraded in place is until its role's next start: the two times read as
	// absent there. Four decisions write it, each beside GivenEntry or the delivery it records:
	// queue and EntryGiven, where the claim is given an entry's work; confirmCarrying, where a
	// turn confirms a task of that entry; and the take-back of that confirmation where it was never
	// shown to be the worker's — a refusal or a busy answer (takeBackPending, refused), or a
	// let-go that cut off a turn that confirmed the task while its prompt's send was in flight or
	// a prompt of it still waited on its result (interrupted, confirmationInDoubt) — which clears
	// the handshake's own. A queue of another entry replaces it, and nothing else clears it: a
	// retirement, a turn's end, a hold and a let-go's take-back of a confirmation a turn of the
	// task did take, its prompt's result come or no send or prompt of it outstanding, each leave it.
	Handshake EntryHandshake
	// ServingCheckLook is the production check look of the last confirmed delivery this claim
	// retired, the look side of ServingGeneration: what ServingCheck answers with once no delivery
	// backs the worker's turn. The zero value is a claim that has retired no confirmed task of a
	// look, which is every claim whose last task was a phase's.
	ServingCheckLook CheckLook
	// LastStartRow is the outbox row id of the newest start run against this claim, and 0 for one
	// no start has run for. A stop from an older row is one the newer start superseded: it was
	// written to end a run that is over, and acting on it would stop the run that start began.
	LastStartRow int64
	// DeliveredRows is every outbox start row whose task this claim queued (RequestDeliver.Row),
	// in the order the rows gave them, and empty for a claim no row has given a task. Each row is
	// written with its delivery, in the same transaction, so a row's task is known queued no later
	// than it is: a retry of that row is never queued again, whether its delivery is still pending
	// or the claim has since retired it. That is what the delivery of a start of no entry is
	// held to (record.SuperviseRequest.Entry, which lists those starts), where one carrying an
	// entry is held to GivenEntry instead (queue).
	// The rows of one issue do not run in row order (record's outboxInOrder), so the set is kept
	// rather than a high-water mark: an older row still to deliver after a newer one has is given
	// its task, and only a row that gave this claim a task is read as given. It is also what says
	// whether a start's task reached the claim, which is what settles the decisions that start
	// named (the outbox's settleKept).
	DeliveredRows []int64
	Locator       *runtime.Locator
	// Enrollment is the secrets broker's record of the claim's running process, when the runtime
	// enrolls pods: the enrollment id, and the incarnation it was made for. It is
	// revoked and cleared wherever the process is let go (letGo), so a record for another
	// incarnation is never acted on.
	Enrollment *Enrollment
	State      ClaimState
	Budgets    Budgets
	// Failure is the reason a terminally failed claim could not continue. It is cleared before a
	// retry and persisted so a daemon restart can report the same failure to the workflow.
	Failure string
	// PodEnd is the last pod end this claim's process was observed to have died of, as the
	// runtime read it off the pod itself (runtime.Observation.PodEnd): `Evicted: The node was low
	// on resource: ephemeral-storage. ...` and nothing of the role's own log. It is what lets the
	// failure the workflow is told name the cause a role never chose, where Failure alone says
	// only that the relaunch budget ran out.
	//
	// It is written where died records a death — a suspension over a dead process records one
	// without it (suspendOverADeadProcess) — and cleared with Failure at each launch (launchTries),
	// so a claim that comes back and later fails for something else carries nothing of the
	// earlier end. The retry that admits a failed claim again clears Failure and not this
	// (retry), so a retried claim whose launch has not run yet still carries the pod end of its
	// last death. Since a launch clears it, a failed claim that holds one failed with no launch
	// since the death that wrote it.
	//
	// It is durable for the reason Failure is: the failed claim is what a restarted daemon
	// reports the failure from. A failure whose fact no daemon applied — the live apply failed, or
	// the daemon's stop cut it off after the claim's state was written — is applied at the next
	// boot from the claim read back from the store (the daemon's replayTerminal), and that fact
	// names the pod end only where the store kept it.
	PodEnd string
	// FailureCause is the one word for what failed the claim, written beside that sentence by the
	// site that failed it (fail), and FailedAt is when it did. Both are cleared where Failure is,
	// by the retry that gives the claim another run and by the launch of that run (retry,
	// launchTries), so a claim carries them from its failure until it is run again — a claim an
	// operator retired after it failed still carries them — and they are persisted with it, so
	// an agent reading the claim after a daemon restart reads the cause the daemon that failed
	// it chose.
	FailureCause FailureCause
	FailedAt     time.Time
	// LeakCheckCause is the sentence a launch try's start was refused with because the shared
	// public leak check could not be put in place in the role's pod (runtime.LeakCheckCause), and
	// "" when the latest try started its process, failed for another reason, or was cut short by
	// its own context, which refuses nothing. launchTries writes it from the outcome of every try
	// and does not clear it at the start of one, so it reads the same across the backoff between
	// tries; the retry that revives a failed claim clears it with the failure record (retry). It
	// is kept beside Failure rather than in it: a claim that fails on it carries Failure `launch
	// failures ran out: <the error>` and the cause word launchTries chooses, and this field says
	// which check refused it.
	LeakCheckCause string
	// PendingCause is what failed the process whose relaunch is now under way
	// (relaunchAfterFailure), and "" for a claim relaunching after no failure. It is the cause a
	// launch the runtime then refuses fails under, so a claim that died and whose relaunch the
	// runtime refused reads died rather than launch_refused, which is what the architect must act
	// on (launchTries).
	//
	// It is durable for the reason Claim.DeathCharged is: the relaunch that carries it lives in
	// memory — a parked wake's turn, and the retry relaunchLater holds in m.relaunch — while a
	// restored claim is taken up from what the store holds, so a daemon replaced between the
	// charge and the budget running out would write launch_refused for a claim that died. It is
	// cleared where Budgets.LaunchFailures is put back to zero, by the agent's ready and by the
	// whole-budget reset of a retry, because the pending cause and the launch-failure count
	// measure one episode, and by the decision that charges a death (died), whose relaunch
	// charges that death its launch failure and records died here as it does: so died here, on a
	// claim whose death is charged, says this death's launch failure is in the count beside it,
	// which is what keeps a raise of that relaunch after a restart from charging it again
	// (deathLaunchFailureCharged).
	PendingCause FailureCause
	Pending      *Delivery
	// LastTurnStart and LastTurnEnd are when the agent last began a turn and when it last ended
	// one, each zero until that event happens. This is the one list of who dates a turn's start,
	// which the other places point to. A turn's start is when the daemon first learned of it: its
	// agent_start (beginTurn); the busy answer that said the agent is in a turn whose start the
	// daemon never read (recordBusyTurn), which that turn's own agent_start, read later, moves to
	// its own moment (busyTurnBegun); the agent's answer that it is streaming, which puts a ready
	// or idle claim a restart restored in working (sendPending's askFirst branch,
	// answerAskFirstAfterDrop); or an agent_start in a claim already working, which is a turn Oh
	// My Pi began under a compaction, or one it began under a compaction a restart forgot
	// (turnStartedWhileBusy). Each is dated where its start line is written (startTurn,
	// busyTurnBegun), so no path that starts a turn leaves it timed from the turn before, and
	// each writes the claim with its date. A turn's end is dated where the agent ends it
	// (turnEnded), and where the daemon ends it: a charged death (chargeDeath), the controller
	// keeper's end of a turn past its bound (endTurnPastBound), and a compaction's start, which
	// closes a turn whose end was never dated (compactStart). The end is written with the claim at
	// each, except the end of a turn that ran under a compaction: that is written with the
	// compaction's end (compactEnded), or with the death's own write where the process was lost
	// under the compaction and no compaction end runs (compactionLostProcess). Every other path
	// that ends a turn — a suspension that stops a process mid-turn, a displacement, a stale
	// address, the stale-plugin relaunch, the stall backstop, a death charged nothing — dates no
	// end, and leaves the times showing the turn open until a compaction or the next turn moves
	// them. They are persisted, so a restart keeps what was written: they are what a reader has to
	// tell a worker still making progress from one wedged in a turn or idle since yesterday, and a
	// claim rebuilt without them would look as if it had never worked.
	LastTurnStart time.Time
	LastTurnEnd   time.Time
	// TurnOutcomes are the newest turns the claim's agent ended that its backend's producer
	// reported, in (generation, sequence) order and bounded by the launch failure limit
	// (recordTurnOutcome). They are the window RefusedRun derives the run of refused turns from,
	// and they are persisted, so a daemon restart reads the run the reports before it left rather
	// than starting the count again. They go with the session whose turns they are (freshRun).
	TurnOutcomes []TurnOutcome
	// LastDeathCause is why the claim's process was last found gone — the runtime's own account of
	// the death (runtime.Observation.Detail: an OOM kill, a node that went away, a pod the cluster
	// evicted) — and LastStartFailure why its last start did not produce a running agent — a
	// launcher that refused the start, an init container that failed, a pod that never became
	// ready (launchAfter's error). They are two answers to two questions, kept apart because a
	// claim that cannot start and a claim that keeps dying are different faults, and neither
	// overwrites the other. The death's cause is kept until another death replaces it. The start
	// failure is kept until another failed start replaces it or the agent says it is ready, which
	// ends the episode of launch failures it names (table.go's ready). So a start failure this
	// release records is one from after LastReady. A claim an earlier release left in any state
	// but ready, working or idle can hold one from before its last ready until its agent is next
	// ready; migration 20261123212127 cleared the ones it left ready, working or idle, and the
	// boot clears such a row a daemon of that release writes later (DropStaleStartFailure). Both
	// are a far side's text, kept as such: LastStartFailure as any far side's error text is
	// (storetext.FarSideError), and LastDeathCause as a far side's text (storetext.FarSideText)
	// under the runtime's own bound (deathCause).
	//
	// LastDeathAt and LastStartFailureAt are when the daemon recorded the cause beside each, by
	// its own clock, and zero where that cause is empty (#5349). The rule: each is set and
	// cleared only together with its cause, so a cause recorded again with the same text is given
	// its new time, and whatever drops a cause drops its time. A reader tells an old cause from a
	// current one by it, which the text alone does not say: a claim keeps its death's cause
	// through every later ready. The store writes each time with a digest of the cause it dates
	// and reads it back only beside that cause (the claim store's scanClaim); a cause an earlier
	// release recorded has no time, except after a rollback (the migration's header).
	//
	// LastReady is when the agent last said it could be prompted. LastDeathAt dates the death: a
	// LastDeathAt before LastReady is a death the running agent survived. All five are persisted:
	// the operator reading a claim after a restart is the one who has to act on them, and the
	// death's cause is what the record of that death names (endDied, EndedWork), which the
	// restarted worker is told (RelaunchSentence, interruptedTask).
	LastDeathCause     string
	LastDeathAt        time.Time
	LastStartFailure   string
	LastStartFailureAt time.Time
	LastReady          time.Time
	// BootTokenHash is the hash of the current launch's boot token, which the shim's hello and the
	// agent's registration are resolved by. CapabilityHash is the hash of the secret the agent's
	// registration was issued.
	BootTokenHash   []byte
	CapabilityHash  []byte
	UncertainStreak int
	// SuspendedBy is who suspended the claim while it is suspended, and "" otherwise.
	SuspendedBy SuspendSource
	// SuspensionHeld is whether a suspension is held for the agent's turn (holdSuspension), and
	// SuspensionHeldBy who asked for it. Both are memory only: Machine.Claim reports them, and the
	// store never writes them.
	SuspensionHeld   bool
	SuspensionHeldBy SuspendSource
	// SessionLost is the cause of the forget that the launch this claim is being built a spec for
	// follows (startFresh): the launch is one the machine keeps that cause for (Machine.lostCause,
	// which says until when), and it is "" for every other launch. It is an argument of that one
	// build and of nothing else: the machine sets it on the copy of the claim it hands the spec
	// builder (spawnSpec), never on the claim it keeps or persists, so the new session is told it
	// does not continue its predecessor's transcript and nothing afterwards reads it.
	SessionLost SessionLoss
	// Compacting is whether the claim is working because its session is being compacted rather
	// than because a turn is (Machine.Compact). It is memory only, as SuspensionHeld is:
	// Machine.Claim reports it, so a caller can tell a claim in a turn from one being compacted,
	// and the store never writes it.
	Compacting bool
	// ProcessGone is whether the runtime's last report of the process this claim still records
	// found it gone (judge, reportedProcess): false for a claim whose last report found that
	// process running, for one the runtime has reported nothing of yet, and for a claim that
	// records no process at all. It is memory only, as Compacting is: Machine.Claim reports it,
	// and the store never writes it.
	//
	// This is the one statement of what it is for, which every other reader of it points to. A
	// claim stored working is in a turn, or it is one the launch that was to replace the
	// process it records never started, and this is what the machine's own side tells the two
	// apart by: a suspension that reaches such a claim is decided where it arrives rather than
	// held for a turn that will never end (holdSuspension). It is memory, so it is no answer
	// for a caller outside the machine: a restart forgets it and leaves the same claim reading
	// plain working, which is why both recovery doors read the process itself at the moment of
	// the request instead, and that read records here as the sweep's own report does
	// (ReadProcess, reportedProcess, api's refuseWorkingClaim). Two windows leave such a claim,
	// each of them a verdict of gone and then a step that wrote nothing of the claim:
	//
	//   - the take-back of the turn the dead process was running, which the store would not
	//     write: after a death (died) and after a displacement (displaced, replaceProcess),
	//     each of which takes the task back before it relaunches anything;
	//   - the tree lifecycle's refusal of the relaunch that verdict raised, which keeps the
	//     claim exactly as it stands (relaunchRefused). One refusal serves every launch, so it
	//     is met before the wake pacing (relaunchAfterFailure, launchAfter) and at a parked
	//     relaunch's own turn, where the wake is spent with it (runWake).
	//
	// A launch that does run clears it with the process it lets go (launchNow, letGo), and
	// while one waits its paced turn both doors read that launch first, so a claim whose
	// process died under a park is answered by the launch rather than by any read of that
	// process (relaunchWaits, ProcessReplacedByLaunch).
	//
	// What the next report of that same process is charged, and what it can end in, is died's
	// (DeathCharged).
	ProcessGone bool
	// DeathCharged is whether the death of the process this claim records has been charged, or
	// found to have nothing to charge: the decision died makes for one dead process, and the
	// record that keeps a second report of that same process from being charged a second death.
	// The rule it keeps is written where it is applied (died); what leaves a claim to be
	// reported twice is ProcessGone's two windows above.
	//
	// It is durable, and it is the one of these marks that is: a restart restores the claim a
	// refused or parked relaunch left standing over its dead process, and the boot reads this
	// record to raise that relaunch again at once (RecordedDeath, relaunchRecordedDeath), rather
	// than wait for the sweep's next report of the process, which is the second report of a
	// death this record says was charged. Charged a second time, the one death walks the claim
	// to the deaths limit and fails the worker for one dead process, in the very window the claim
	// is waiting to be relaunched out of — the window both recovery doors reach it in, the
	// fresh session by stopping it and starting it again (api's refuseWorkingClaim,
	// freshSession). Every write of it is made where the decision it records is made, because
	// the windows that leave such a claim are the windows where nothing else writes the claim at
	// all: died writes the charge with the count it charged, and the report that finds the
	// process running again writes the clear (judge, reportedProcess). The read a recovery makes
	// of the process records what it finds as such a report does, and writes that clear in the
	// same write (ReadProcess).
	//
	// It is of the process the claim records, as ProcessGone is, and goes with it: letGo clears
	// it with the process it is about, and the claim written there records no process for a
	// later report to be about.
	DeathCharged bool
	// AwaitsPod is the live pod this claim's launch waits on, and nil for none: the claim's last
	// launch try was refused because that pod waits for its update round
	// (runtime.AwaitingPodRound), and the claim records no process (Locator is nil) until a later
	// try starts one. It is memory only, as ProcessGone is: Machine.Claim and View report it, the
	// daemon's pod-update sweep reads it off the machine of each claim that records no process
	// (Machine.AwaitsPod), and the store never writes or reads it. A daemon restart forgets it.
	// The claim's next refusal records it again (startWaitsForPodRound), but only when the
	// restarted daemon starts that launch again: after the boot's orphan reconciliation, and at the
	// turn the wake pacing gives it (internal/daemon's relaunchUnfinished). In a restart of many
	// pods that can be more than a minute later, and a sweep before then does not see the pod.
	//
	// It is what the daemon's update round has of such a claim, which may be the issue's only
	// one: the round opens over this pod as over a pod a process runs in, waits on no answer and
	// no turn of the claim, and replaces the pod through this locator where no claim records a
	// process there. Three functions of internal/daemon read it: podLocator, awaitsRoundPod and
	// podProcess. The doc of awaitsRoundPod says what the round does with such a claim.
	AwaitsPod *runtime.Locator
	// RegisteredPods is the set of pods, by uid, in which an agent of this claim registered a
	// session, as its machine knows it: each registration adds the pod of the process that
	// registered (register), and a process in no pod (a tmux pane, a locator with no pod) adds
	// nothing. Nothing takes a pod out of it, not a session lost or forgotten, a let-go, or a
	// relaunch into another pod, since an agent that registered in a pod had a worker there, and
	// what that worker did not push was on that pod's disk. The daemon's update round reads the
	// set to name a member that lost work (internal/daemon's workOnRoundPod).
	//
	// A machine built over a stored claim does not know where the claim registered before it was
	// built, so it reads the set off the stored claim (bootRegisteredPods). It is memory only, as
	// AwaitsPod is: Machine.Claim and View report it, the daemon's pod-update sweep reads it off
	// the machine of each claim it groups (Machine.RegisteredPods), and the store never writes or
	// reads it (StoredClaim).
	RegisteredPods RegisteredPods
	// PluginContract is the contract the current registration declared: 0 means no contract has
	// been recorded, which every claim has at the first boot on this release. It is set whenever a
	// registration records Session/SessionFile/CapabilityHash (register, reregister).
	PluginContract int
	// PluginRelaunchFor is the daemon contract the last stale-plugin relaunch was made for, 0 when
	// none. It is written where that relaunch starts the process the stale one is replaced with
	// (launchNow, commitPluginRelaunch), by that launch's own write, and cleared when a
	// registration records the daemon's own contract, so a registration that still differs while
	// it equals the daemon's contract is the one that is charged rather than relaunched again
	// uncharged. A relaunch that was decided but never launched — refused, abandoned at its turn,
	// or ended in a suspension — writes nothing here, so the mark always names a relaunch whose
	// launch was recorded (relaunchMarked, staleRegistration); that write comes before the runtime
	// starts the process (commitPluginRelaunch).
	PluginRelaunchFor int
	// AgentResumeGeneration is the generation of the launch that had Oh My Pi resume the claim's
	// session by its id itself (`--resume=<session id>`, runtime.SpawnSpec.ResumeByAgent), and 0
	// where no such launch is outstanding. start sets it, and only while it is 0, with the launch
	// it names; register clears it in the write that records the session and its file, and
	// forgetSession clears it with the session. No other code writes it. The store keeps it in
	// claims.agent_resume_generation.
	AgentResumeGeneration uint64
	// UpdateRound is the update round this claim is in, and "" when none is open: the round's
	// identity and its kind in one string. PodRoundPrefix and the pod's uid is the drift round of
	// the pod the claim runs on, which every claim of that pod shares, and PluginRoundPrefix and
	// the daemon's contract the per-claim round of a claim whose plugin is behind this daemon. A
	// round ends when the process it waits for is replaced. The daemon asks each role of the
	// round and replaces the pod once every asked role has answered, no turn runs and no claim's
	// activity report holds it (internal/daemon's roundEnd.holding), or at the round's bound
	// (pod_update_grace_minutes) whatever the roles answered and whatever runs, an operator's
	// suspension excepted. Work that a role did not push goes with the disk the replacement
	// deletes.
	UpdateRound string
	// UpdateReasons is what the round was opened for, as the daemon found the drift: the task's
	// text and the operator's reading of why this pod is being replaced.
	UpdateReasons string
	// UpdateOpenedAt is the round's own clock — when the round was opened on this claim — and
	// UpdateTaskedAt is when the round's ask reached a session of this claim's role
	// (record.UpdateAsk, Machine.PublishUpdateAsk), nil until one has: the mark that makes the
	// sweep ask each claim of a round until one ask reaches it, and never after.
	// UpdateSignalledAt is when the worker answered ready_for_update, nil until it has.
	UpdateOpenedAt    *time.Time
	UpdateTaskedAt    *time.Time
	UpdateSignalledAt *time.Time
	// UpdateHead is the commit the worker reported, when it signalled, as the head of its issue
	// branch at origin, and "" when it reported none (a planner step that runs before the branch's
	// first push). It is the worker's word, which nothing verifies.
	UpdateHead string
	// UpdateToldAt is when this round was last reported to the operator's side, so one round is
	// not reported again at every sweep that finds it open.
	UpdateToldAt *time.Time
	// UpdateLeftRound is the pod round this claim left, and "" when it has left none: the daemon
	// started the claim's process again in another pod than the one that round was opened over
	// (launchAfter).
	//
	// It is the round's own record of the claim's part in it, and every reader of that part
	// reads it rather than the locator: the locator moves under a relaunch and goes with a
	// suspension (letGo), so a part derived from it thaws the moment a claim is suspended, and a
	// late answer from the process in the new pod counts for a round it took no part in.
	// UpdateRound is left as it was when a claim leaves, so the claim is still a member of the
	// round it left and the report of what that round's pod took with it still weighs the claim.
	// Whether that claim had answered when it left is UpdateSignalledAt, which nothing writes
	// once the claim has left (podUpdateSignalled refuses the answer). Only the round's end
	// clears it (clearUpdate).
	UpdateLeftRound string
	// ProviderErrorRun is how many turns of this claim ended in a provider error in a row, and 0
	// for a claim whose last turn ended with none. A refused turn a prompt ran is counted from that
	// prompt's result (promptResulted), and a refused turn that ran no prompt of the daemon's from
	// its report (unpromptedTurnEnded), in the one run; a clean prompt result and a clean report,
	// prompted or not, each end it (turnOutcome). At providerErrorRunTold the project's controller
	// is told once, and not again until a turn with no error puts it back to zero. It bounds
	// nothing: R2 of dispatch://INFRA-956 forbids an attempt limit here, since a limit would fail
	// every claim of a project at once in a provider outage.
	ProviderErrorRun int
	// Step is what the daemon last saw this claim's agent do, as the tool frames of its stream
	// report it. It is written by the store's PutClaimStep alone and read back with the claim;
	// no write a machine makes carries it or erases it, a launch's included: the store's putClaim
	// names none of its eight columns. The activity report below is held the same way but for
	// that one write, which clears the report and leaves the step as it stood.
	Step StepRecord
	// ActivityAt, ActivityBackground, ActivityKinds and ActivityNextReminderAt are what the
	// process this claim runs now last reported it was doing (POST /legion/v1/claims/activity):
	// when it read what it was doing, the zero time for a session that has reported nothing;
	// whether that reading found background work of the session's own running, and the kinds it
	// named; and when the phase reminder armed in that session is due, the zero time for none.
	// They are written by the store's PutClaimActivity alone and read back with the claim, as
	// Step is: no write a machine makes carries them, and a launch's write clears them.
	//
	// The store's putClaim sets all four to their zero values on a write that moves Generation
	// (launchTries), because the report describes the process that made it and the launch replaces
	// that process; and a report is written only onto the launch whose secret the route
	// authenticated it with, so one a dead process sent around its relaunch is never written over
	// that clear, whatever the route answers it (store's PutClaimActivity). So a worker displaced
	// into a replacement pod while a background task of its own ran is read as a session that has
	// reported nothing, not as one still waiting on that task.
	//
	// The daemon's minute sweep reads them off the live claim, which is the only reading it has
	// of what a quiet worker says it is waiting on (internal/daemon's tellSilentPhaseWorkers,
	// config.Config.WorkerBackground). The rules over a run read the same report from the record
	// instead (record.RoleClaim).
	ActivityAt             time.Time
	ActivityBackground     bool
	ActivityKinds          []string
	ActivityNextReminderAt time.Time
}

// EntryHandshake is the handshake between the daemon and a claim's worker over one phase entry:
// the entry, the issue generation the entry's task was of (Delivery.Generation), when a task of
// that entry was first queued to the claim (the first such Delivery.QueuedAt) and when a turn of
// the claim's worker first confirmed a task of it (the first such Delivery.ConfirmedAt). The zero
// value names no entry, and a zero ConfirmedAt is an entry no turn has confirmed a task of.
type EntryHandshake struct {
	Entry       int64
	Generation  uint64
	QueuedAt    time.Time
	ConfirmedAt time.Time
}

// queued is h once a task of entry, of generation's run, was queued to the claim at the moment at.
// Where h already names that entry of that run it stands as it is: a second task of one entry is
// a sweep's re-send after an unconfirmed retirement put the entry back (retirePending), or a held
// phase's retry (RequestDeliver.ResendEntry), and the entry was still first queued, and perhaps
// confirmed, when h says. Any other entry replaces h with that entry, queued at and confirmed by
// no turn yet.
func (h EntryHandshake) queued(entry int64, generation uint64, at time.Time) EntryHandshake {
	if h.Entry == entry && h.Generation == generation {
		return h
	}
	return EntryHandshake{Entry: entry, Generation: generation, QueuedAt: at}
}

// confirmed is h once a turn confirmed the task d: the first confirmation of a task of h's entry
// and run is recorded at d.ConfirmedAt. Every other confirmation leaves h as it stands: a later
// one of the same entry, which a task a death took back and sent again makes, and one of a task of
// another entry or of no entry, a wake's or an operator's, which is no part of this handshake.
func (h EntryHandshake) confirmed(d Delivery) EntryHandshake {
	if d.Entry == 0 || d.Entry != h.Entry || d.Generation != h.Generation ||
		d.ConfirmedAt.IsZero() || !h.ConfirmedAt.IsZero() {
		return h
	}
	h.ConfirmedAt = d.ConfirmedAt
	return h
}

// unconfirmed is h once the daemon took back the confirmation of the task d, and whether that
// changed h: a refusal or a busy answer said the turn that confirmed it was not d's, so the worker
// never took it, or a let-go cut that turn off before anything said whose it was (interrupted,
// confirmationInDoubt), so nothing says the worker took it. Where that confirmation is the one h
// recorded — d is of h's entry and run, and its ConfirmedAt is h's — h is unconfirmed again, the
// task's next real confirmation is recorded in its place, and the answer is true. Any other
// take-back leaves h as it stands and answers false: one of a task of another entry or of no
// entry, and one of a later confirmation of h's entry, after the first one stood. The bool is
// what its callers read to know whether the claim's write must carry the handshake, rather than
// comparing two handshakes that hold times.
func (h EntryHandshake) unconfirmed(d Delivery) (EntryHandshake, bool) {
	if d.Entry == 0 || d.Entry != h.Entry || d.Generation != h.Generation ||
		d.ConfirmedAt.IsZero() || !d.ConfirmedAt.Equal(h.ConfirmedAt) {
		return h, false
	}
	h.ConfirmedAt = time.Time{}
	return h, true
}

// StepRecord is the claim's record of its agent's last step: the last call the agent made, when
// the daemon saw it, and when the turn that call belongs to began (INFRA-1274). The backstop that
// ends a turn whose agent has stopped stepping reads this and guesses nothing. The call it names
// is replaced by the next call and cleared by nothing, so after a recovery — the hung process
// ended, the claim relaunched, a turn begun on the replacement — it still names the call that
// was open when the agent stopped.
//
// It is no part of a machine's state. The frames it is built from arrive between a machine's
// writes, and a machine's in-memory claim does not carry them, so one write of the whole claim
// would erase every step seen since that machine read it. The store writes these columns through
// a narrow call of their own for that reason.
type StepRecord struct {
	// At is when the daemon received the agent's last step, and the zero time for a claim whose
	// agent has stepped none. On a write the zero time records the call without moving the
	// claim's last-step time: the fence on a frame of a connection that is no longer the claim's
	// (staleStep), which is a frame of a process already replaced, and says what that agent was
	// doing and nothing about what the claim is doing now.
	At time.Time
	// TurnStartedAt is when the agent's current turn began, which is what a claim whose agent has
	// sent no tool frame at all is judged from. On a write the zero time leaves the stored turn
	// start where it is, so an ordinary step does not erase the turn it belongs to.
	TurnStartedAt time.Time
	// Tool and CallID name the call the agent is in: the tool, and the call id the start and end
	// frames pair by, which is the only thing that tells two calls of one tool with
	// byte-identical arguments apart. On a write an empty CallID is a write that carries no call
	// at all — a turn's start — and leaves the call the record names where it is; a write that
	// carries one carries every field of it.
	Tool   string
	CallID string
	// ArgsBytes is the length of that call's arguments and ArgsHash a hash of them. Their text is
	// never kept: a shell command routinely holds a credential, and this record reaches every
	// surface an operator reads.
	ArgsBytes int
	ArgsHash  string
	// StartedAt and EndedAt are when that call's start and end were received. A call still open
	// has a start and no end, which is the shape a stalled turn leaves behind.
	StartedAt time.Time
	EndedAt   time.Time
}

// The two kinds of round Claim.UpdateRound spells, each with what identifies it: a pod's, which
// every role claim of one issue shares, and one claim's own plugin round. They are here, beside
// the field, because a round is matched in this package and written in internal/daemon's sweep
// (openPodUpdate), which recognises the rounds it must carry by them.
//
// The round's id is the one record of its kind: PodRound is how a pod's is written, the prefix
// above all that is left of how a plugin round was written, since nothing of this release writes
// one, InPodRound and InPluginRound how a claim's round is read, and IsPodRound and IsPluginRound
// how a round id is read on its own. Nothing else carries the kind beside the id, so nothing can
// disagree with it.
//
// No daemon on this release writes a plugin round: the stale-plugin lane relaunches a stale
// process and asks its worker nothing (plugin.go's refreshPlugin). The plugin half of this is
// how a claim a daemon of an earlier release left in such a round is found and taken out of it
// (endCurrentPluginRound, the end the sweep posts), and it stays until no daemon that can write
// one is left to restart from.
const (
	PodRoundPrefix    = "pod:"
	PluginRoundPrefix = "plugin:"
)

// PodRound is the round id of the drift round of the pod with this uid, which every claim of that
// pod shares.
func PodRound(pod string) string { return PodRoundPrefix + pod }

// InPodRound and InPluginRound say which kind of round this claim is in; both are false for a
// claim in no round at all. IsPodRound and IsPluginRound ask the same of a round id on its own,
// so the words and the claim's own reading can never part company. IsPodRound is the one with
// callers that have a round before any claim holds it (LeftPodRound, asked about a round the
// claim may not record and answering for a pod round alone, so the empty round nobody is in is
// no round it left; the workflow's idle rule, reading a round it was handed); IsPluginRound
// is reached through InPluginRound alone, since the deletion of the plugin round left no caller
// holding such an id on its own.
//
// The number a plugin round's id carries after `plugin:` is the daemon's contract number and not
// the worker's: the round was opened by a daemon over a claim whose plugin spoke something else,
// and the id records the contract that daemon spoke, which is what the claim's process had to
// reach rather than what it had. So `plugin:59` in a claims row or a log line names a daemon at
// 59 and says nothing of the worker's own contract, and a reader who takes it for the worker's
// reads it backwards. No case asserts this wording: a test that pins prose goes red at every
// rewording and proves nothing of behaviour (dispatch://INFRA-2694).
func (c Claim) InPodRound() bool { return IsPodRound(c.UpdateRound) }

func (c Claim) InPluginRound() bool { return IsPluginRound(c.UpdateRound) }

func IsPodRound(round string) bool { return strings.HasPrefix(round, PodRoundPrefix) }

func IsPluginRound(round string) bool { return strings.HasPrefix(round, PluginRoundPrefix) }

// LeftPodRound is whether this claim left the pod round named: that round's pod died and the
// claim was started again in another one while the round was still recorded on it. The work the
// round asks about went with the dead pod's disk, so the process now running takes no part in
// that round at all: it cannot answer for that work. The claim's part stands as the dead pod
// left it, answered or not, until the round ends.
//
// It takes the round rather than reading the claim's own, because the two are not always the
// same question and the difference decides whether a disk is deleted. A claim that left round X
// and that round Y's own open has not reached yet still records X, and it is no claim of Y that
// has left: Y has asked it nothing, and it holds Y's pod until Y does. Read of the claim's own
// round instead, such a claim would stop holding Y's pod, and Y could delete a disk the claim
// still had unpushed work on. UpdateLeftRound is written for a pod round alone
// (leftPodRoundAt), so the two equalities here are the whole question.
//
// It is read from the round's own record and from nothing else (UpdateLeftRound, written by
// leftPodRoundAt where the launch records the pod its process came up in). Derived from the
// locator, it would be a fact about where the claim's process is now rather than about the
// round: a suspension clears the locator (letGo), the claim would read as one that had never
// left, and the answer its new pod's process gives late would be taken for the round.
//
// It is exported because internal/daemon's sweep asks the same question of the same record when
// it decides whom a replacement still waits for, and one predicate is what keeps the two from
// drifting apart.
func (c Claim) LeftPodRound(round string) bool {
	return IsPodRound(round) && c.UpdateRound == round && c.UpdateLeftRound == round
}

// HoldsSession is whether session is the one the claim holds. The claim holds its session as the
// store keeps it: register records the session of a registration Handle gave through the store's
// rule, and a restart reads it back so. A session is compared as the store would keep it
// (storetext.Text), because internal/api and internal/daemon ask this with a session of their own
// — an API request's, a notice's recipient — which no Handle has given through the rule, and an
// agent whose session id holds a NUL is the claim's own agent before a restart and after one.
func (c Claim) HoldsSession(session string) bool { return c.Session == storetext.Text(session) }

// RuleMayResume says whether the rule may resume this suspended claim: every suspension but the
// operator's, which holds until an operator resumes the claim.
func (c Claim) RuleMayResume() bool {
	return c.State == StateSuspended && c.SuspendedBy != SuspendedByOperator
}

// PassedOverByRound is whether a pod update round passes over this claim by R1's rule, read of
// the claim alone. The round does not ask such a claim, does not wait for it, and does not wake or
// resume it (dispatch://INFRA-4109 R1). It is every suspension the rule may resume
// (RuleMayResume): a suspension by the daemon, by a root's exit, by an idle release, or by an
// unknown source. Such a claim runs no process and no turn in the pod, so an ask reaches no
// session. A wake or a resume for the ask would override the reason the daemon stopped the claim.
// For a released claim that reason is the approved decision of dispatch://INFRA-2981: an
// idle-released pod is not woken to be asked, because it holds no turn to interrupt.
//
// The round makes one exception that the claim does not carry: a claim a project's pause stopped
// is suspended released too, and the round asks it once its agent is ready and waits for it while
// the daemon's podHolds.owes in internal/daemon holds (internal/daemon's roundClock.passesOver,
// which reads this method and that rule). PublishUpdateAsk still refuses an ask to every claim this
// method names, a claim the pause stopped included: a publish to a role with no holder raises a
// role wake, and the sweep asks a claim only once its agent is ready, when the claim is no longer
// suspended.
//
// An operator's suspension is not passed over. A person's suspend route, the boot's
// session-conflict suspension and fresh_session record it, and it holds the round until the
// operator acts (dispatch://INFRA-2981 R5, RuleHolds). internal/daemon's roundAsks (through
// roundClock.passesOver) and PublishUpdateAsk both read this method, so the ask, the wait and the
// publish state R1's rule once.
func (c Claim) PassedOverByRound() bool {
	return c.RuleMayResume()
}

// Released is whether the claim is the one suspension a wake brings back: the daemon released its
// idle issue pod, so the claim keeps its session, its phase and its pending task, and the next
// wake for its role resumes it (SuspendedByReleased). Every rule that acts on a released claim —
// the wakes, the notice routing and a reader in a transaction (both through record.HoldsRole), the
// stranded report, a release the store cancelled — reads it here rather than spelling the pair out
// again.
func (c Claim) Released() bool {
	return c.State == StateSuspended && c.SuspendedBy == SuspendedByReleased
}

// RuleHolds is whether an operator's suspension blocks a daemon rule: the claim is suspended by the
// operator, or the operator's suspension is held for its agent's turn.
func (c Claim) RuleHolds() bool {
	if c.SuspensionHeld {
		return c.SuspensionHeldBy == SuspendedByOperator
	}
	return c.State == StateSuspended && c.SuspendedBy == SuspendedByOperator
}

// RuleHoldError says why a start or outbox delivery waits: the operator's suspension, which only
// the operator's resume ends.
func (c Claim) RuleHoldError() error {
	return fmt.Errorf("the claim is suspended by %s; it waits for the operator's resume: %w", SuspendedByOperator, ErrHeldByOperator)
}

// ClaimView is a claim as its machine last published it (Machine.View), with what only the decision
// in flight knows.
type ClaimView struct {
	Claim
	// Stopping is whether the decision in flight is stopping the claim's process for good — a
	// suspension, a failure, a release, a root's exit (endProcess, release) — while the claim still
	// shows the state it is leaving. A process retired and relaunched in one decision
	// (chargePrompt, the registration deadline) is not stopping: its claim keeps its role.
	Stopping bool
	// SendInFlight is whether a prompt of the claim's pending task is on its way to the agent:
	// the send's own goroutine is running, between startSend and the outcome that ends it
	// (fenceOutcome), so its adoption of the working copy or its prompt frame may be running
	// right now. The claim still reads ready or idle meanwhile, because the turn that prompt asks
	// for has not started — but the agent is about to be in one, and the adoption the prompt goes
	// behind is already rewriting the working copy the issue's roles share. So a reader deciding
	// who may adopt that working copy counts a send in flight as a turn (the daemon's
	// claimInTurn, supervise's Deps.IssueInTurn): two roles of one issue that each read the other
	// as idle would otherwise adopt it at the same moment (dispatch://INFRA-314).
	SendInFlight bool
	// WakeWaiting is the wake the pacing holds over this claim until its turn (reserveWake), nil
	// while none waits. It travels on the published view rather than behind the machine's lock
	// because none of its readers may wait on the decision in flight, which is the whole reason
	// View exists: the state route, a reader of every claim, takes it through
	// Machine.WaitingWake, which copies no claim (the daemon's supervision,
	// api.State.WakesWaiting); the operator API's recoveries read it to refuse a claim whose
	// launch waits (api's relaunchWaits); and the daemon's delivery wake reads it to hold a row
	// until the turn of a launch that waits over no process (internal/daemon's wakeForDelivery).
	WakeWaiting *WakeWaiting
}

// treeRoot is whether the claim is its tree's root claim (claim.IsTreeArchitect), which ends only
// when its tree closes.
func (c Claim) treeRoot() bool { return claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) }

// Event is everything that reaches a machine: RuntimeObservation, StreamHello, StreamTurnStart,
// StreamTurnEnd, IssueTurnEnded, StreamPromptResult, StreamClosed, StreamLateRefusal,
// PromptAcked, PromptRefused, Timer, StalePlugin, PodUpdateOpen, PodUpdateEnded,
// PodUpdateSignalled, ResumePause, and the requests. The transition table has a row or a named
// ignore for every one in every state.
type Event interface{ isEvent() }

// Store is the persistence a machine writes through. A claim and its pending delivery are
// written separately: the delivery changes on every send, the claim far less often.
type Store interface {
	// AdmitClaim binds a new or reactivated claim to an open durable tree lifecycle and writes it
	// in that same short transaction. A cleanup reservation refuses before runnable work exists.
	AdmitClaim(ctx context.Context, c Claim) (Claim, error)
	// CheckLaunch rechecks the bound lifecycle before a claim persists StateLaunching or calls the
	// runtime; it returns the named cleanup wait without charging a launch failure.
	CheckLaunch(ctx context.Context, c Claim) error
	PutClaim(ctx context.Context, c Claim) error
	PutDelivery(ctx context.Context, token claim.Token, d Delivery) error
	// PutClaimAndDelivery writes both in one transaction: a confirmation records the claim and
	// the delivery its turn started, and half of that is a task nothing sends again.
	PutClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error
	// RetireDelivery ends the claim's delivery and writes the claim it is given in the same
	// transaction: the run a claim serves is set by the retiring of the task that ran. A delete
	// that removes no row is refused two ways, because the table is keyed on the claim and not
	// on the delivery: ErrNoSuchDelivery where the row holds no delivery at all, and
	// ErrOtherDelivery where it holds one under another id.
	RetireDelivery(ctx context.Context, c Claim, deliveryID string) error
	// PutClaimStep writes the claim's step columns alone: what the daemon last saw its agent do
	// (toolStepped). It is apart from PutClaim because the record is built from frames that
	// arrive between a machine's writes, and a machine's claim does not carry it, so a claim
	// write would erase every step recorded since that machine read its claim. It answers
	// whether the store holds the claim at all.
	PutClaimStep(ctx context.Context, token claim.Token, step StepRecord) (bool, error)
	// HoldSessions records every one of holds as token's claim's within project when no claim of
	// the project has recorded it, and answers, in holds' order, the claim that holds each: token
	// itself when it recorded the name before or none had, another claim when that one recorded it
	// first, and the empty token for a name held for no claim. It records all of holds or none: a
	// name another claim holds leaves every other name unrecorded. A hold ends only by an
	// operator's release (ReleaseHolds), whatever becomes of the claim holding it (holdRecorded).
	HoldSessions(ctx context.Context, project string, token claim.Token, holds []Hold) ([]claim.Token, error)
	// ClaimHolds is every name token's claim holds within project, in kind and then name order.
	ClaimHolds(ctx context.Context, project string, token claim.Token) ([]Hold, error)
	// ReleaseHolds deletes every one of holds the table holds for c's claim within c's project and
	// writes c, in one transaction, and answers the names it deleted, in kind and then name order:
	// the operator's release (Machine.ReleaseNames), the one way a hold ends. A refused claim write
	// frees nothing, so the claim's record of which abandoned session a release freed never
	// disagrees with the names the store still holds for it.
	ReleaseHolds(ctx context.Context, c Claim, holds []Hold) ([]Hold, error)
}

// Specs builds the part of a launch the claim does not carry — the pane's environment, its
// secrets, its prompt, its workspace. The machine fills in who the launch is for: the claim's
// identity, the generation, the boot token, and the session file a resume continues.
type Specs interface {
	SpawnSpec(ctx context.Context, c Claim) (runtime.SpawnSpec, error)
}

// Clock is time as a machine sees it.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f once d has passed from the clock's time at the call.
	AfterFunc(d time.Duration, f func()) Cancel
	// At calls f once the clock reaches at, at once when it already has. A timer due at a moment
	// is armed here, never for the wait from an earlier read of Now: time that passes between that
	// read and the arm would put the timer past the moment, and a test's clock, which another
	// goroutine moves forward without waiting, can move by any amount there. The envoy package has
	// the same shape in dispatch/api's hubClock (delivery_live_hub.go) and dispatch/delivery's
	// jobClock (jobs.go).
	At(at time.Time, f func()) Cancel
	// Sleep waits d, or returns ctx's error once ctx ends first.
	Sleep(ctx context.Context, d time.Duration) error
}

// Cancel stops a scheduled callback.
type Cancel interface{ Stop() bool }

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

func (RealClock) AfterFunc(d time.Duration, f func()) Cancel { return time.AfterFunc(d, f) }

func (RealClock) At(at time.Time, f func()) Cancel { return time.AfterFunc(time.Until(at), f) }

func (RealClock) Sleep(ctx context.Context, d time.Duration) error { return backoff.Sleep(ctx, d) }

// Timeouts are every wait a machine times.
type Timeouts struct {
	// Boot is the boot observation interval (worker_boot_timeout_seconds): each interval an
	// unregistered process is probed, and a live one is left alone.
	Boot time.Duration
	// RegistrationIntervals is how many boot intervals a live process gets to register, and again
	// from its registration to say it is ready, before it is retired
	// (worker_boot_registration_deadline_intervals).
	RegistrationIntervals int
	// RPC bounds a prompt's acknowledgement and, after it, the wait for the turn it should start
	// (worker_rpc_timeout_seconds).
	RPC time.Duration
	// Probe is how soon an uncertain process is probed again (probe_interval_seconds).
	Probe time.Duration
	// Stop is how long a suspension that arrives while the agent is in a turn waits for that turn
	// to end before it stops the process anyway (worker_stop_timeout_seconds).
	Stop time.Duration
	// LaunchBackoff is the first wait before a launch is tried again after a charged launch
	// failure, doubled for each failure charged since the claim was last ready, and jittered
	// (backoff.Wait): a launch the runtime refused, and a process relaunched after its charge
	// (relaunchAfterFailure) — one that died, or was retired at its registration deadline. The
	// daemon sets it to its launchBackoffFirst.
	LaunchBackoff time.Duration
	// ClaimWrite bounds each claim and delivery write a decision makes while it holds the machine's
	// lock, so a database that has stopped answering cannot hold that lock for as long as the
	// connection's own patience: every other event for the claim queues behind the decision in
	// flight, and a machine whose lock is held through an outage decides nothing at all. Through
	// the daemon's store (Deps.BoundWrite) it counts from the moment the pool hands the write a
	// connection, and a dial the write waits for runs under it too (store.BoundAfterAcquire); the
	// wait in the pool's queue before that is defaultClaimAcquire's. No setting names it, because
	// it answers a question about the write rather than about a deployment, and only tests set it.
	// Zero takes defaultClaimWrite.
	ClaimWrite time.Duration
}

// Deps is what a machine is built from.
type Deps struct {
	Runtime  runtime.Runtime
	Conns    runtime.Conns
	Store    Store
	Specs    Specs
	Clock    Clock
	Log      *slog.Logger
	Limits   Limits
	Timeouts Timeouts
	// WakePace is how fast the daemon lets this machine's launches out (reserveWake); its zero
	// value is the pacing off.
	WakePace WakePace
	// RefreshGate bounds how much of that pacing the stale-plugin refresh lane may hold at once:
	// a credit per refresh the lane has waiting, K of them, K the same rate WakePace carries
	// (RefreshGate, NewRefreshGate, and the gate's own file for why the lane is bounded where no
	// other is). The daemon builds one for all its machines and holds the same reference for its
	// own refresh pass, which corrects it from the wakes those machines publish
	// (internal/daemon's run, openSupervision, refreshStalePlugins).
	//
	// Every Deps carries one, never nil: a daemon or a harness that paces nothing builds
	// NewRefreshGate(0), which grants every request, so "the pacing is off" is read at that one
	// construction rather than at each asker of this field (admitRefresh, releaseRefresh,
	// refreshPlugin).
	RefreshGate RefreshAdmitter
	// PodStarts is the daemon's one gate over the starts into its issue pods: a claim of an issue
	// starts its process only when no other role of that issue's pod is starting or loading its
	// session, the order among the starts waiting is the spec's, and a load whose agent has not
	// registered podLoadBound after its process started holds the next start no longer
	// (PodStarts, and the gate's own file for why, dispatch://INFRA-5297). A start the gate cannot
	// grant at once parks on the machine until its grant, so no decision waits on it (askPod). The
	// daemon builds one for all its machines under the Kubernetes runtime, where every role of an
	// issue runs in the issue's one pod (internal/daemon's openSupervision). nil is a daemon whose
	// roles run in no pod — tmux — or a harness that builds none, and nothing waits: every start
	// goes to the runtime as it is asked. The project controller's claim never waits on it
	// (Machine.gated).
	PodStarts *PodStarts
	// PhaseRole names the role working the current phase of issue, which the gate's order puts
	// first among the starts no request raised that wait behind another (podQueue.next): the first
	// start to reach a pod that holds nothing goes at once whatever its role (PodStarts.grant). It
	// is the workflow's own reading of the issue record (internal/workflow's OpenPhaseRole), "" for
	// an issue it does not record, for a phase no role works, and for a phase the architect works
	// (admitted, accepting), which has no phase worker, so the order falls back to claim.Roles
	// there. It is read at each start into a gated pod that no request raised, before the gate
	// answers, since the order reads it of no other, bounded by Timeouts.RPC; an error is logged
	// and read as no phase worker, so the start waits in the order of claim.Roles. nil is a daemon
	// with no workflow, where no role is the phase worker.
	PhaseRole func(ctx context.Context, issue string) (claim.Role, error)
	// Secrets is the secrets broker's enrollment routes for the runtime's pods. nil
	// deploys with no broker: no identity is kept, and nothing is enrolled or revoked.
	Secrets Enroller
	// Registrar keeps each process's pod registered at the secrets broker to trade under the
	// claim's session (ensureRegistered). nil — every deployment that names no
	// `runtime.kubernetes.agent_secrets.exchange_audience` — registers nothing, and pins and logs
	// no hello's key (helloKey). It reads the pod token the shim's hello carried, so it does
	// nothing without Secrets.
	Registrar Registrar
	// Identity is the git identity a role's commits carry on issue: its App's bot, minted for the
	// owner of the repository the issue works. A phase task hands it to the agent's working copy
	// before the task (adopts). nil, for a daemon with no workflow or no GitHub Apps, adopts
	// nothing.
	Identity func(ctx context.Context, role claim.Role, issue string) (runtime.GitIdentity, error)
	// IssueInTurn names a claim of issue other than except whose agent is in a turn now, or is
	// about to be, and reports whether there is one. The roles of an issue work one jj workspace,
	// so the adoption a phase task makes of its working copy is not a private act: one run while
	// another role is in a turn moves that role's worker onto a commit of this role's
	// (dispatch://INFRA-714), and a task that would adopt waits instead (sendPending). The daemon
	// answers it from the state its machines publish — a claim is StateWorking from its turn's
	// start to its turn's end, and carries a send in flight from the moment its own prompt and
	// that prompt's adoption go out (ClaimView.SendInFlight) — which is read without waiting on
	// any decision in flight (Machine.View). nil holds no delivery back.
	IssueInTurn func(issue string, except claim.Token) (claim.Token, bool)
	// PhaseHolds says whether issue is still in phase p on run generation: the workflow enqueues a
	// task for the phase and run its issue was in, and the outbox refuses to start a role for a
	// phase the issue has left (daemon/outbox.go), so a delivery queued before the phase or the run
	// moved on is that same staleness one step later and is dropped rather than sent. It is asked
	// only about a delivery that names a phase; a task an operator delivered by hand names none and
	// is never put to it, which the machine decides rather than this answer. generation is 0 for a
	// task of no run. nil holds every delivery.
	PhaseHolds func(ctx context.Context, issue string, p phase.Phase, generation uint64) (bool, error)
	// RootHears says whether the root of c's tree has an architect to hear d now: it runs, it does
	// not linger and it does not wait in `todo` for an admission slot. A root architect's task of
	// a run names no phase, so no phase ever drops it, and it waits on the claim until that claim
	// next runs — which can be after a park, a hand back to `todo` or a linger, generations later.
	// Such a task names a person's decisions, which its own confirmation tells (daemon's
	// settleKept, the claim store's putDelivery), so it is held rather than dropped: false keeps it
	// pending, unsent and uncharged, and the start of the root's next run sends it (StartedBy,
	// queue) — unless that start's own task supersedes it (supersedes), which retires it unsent
	// and leaves the decisions it names for the catch-up that follows. It is
	// asked only about a root architect's task of a run — never a phase worker's, never a task of
	// no run such as an operator's or a production check's. nil sends every delivery.
	RootHears func(ctx context.Context, c Claim, d Delivery) (bool, error)
	// RootTask is what a root architect's held task of a run says now: its text, what it names of
	// the decisions a person made while nobody in the root's tree could hear them (Delivery.Kept),
	// and what it carries of news of the approved design (Delivery.News), re-read from the record at
	// the moment the task is about to be sent. An issue the task names can have left this claim's
	// tree while the task waited, and the tree it stands in now is the one owed that answer: the
	// answer drops it from the task, releases this delivery's hold on its row and writes that tree's
	// architect a catch-up naming it, all in one write of its own. A task that carries news is
	// narrowed in its own text, which the news follows (record.DeliveryNews.Task), and its text and
	// its news are rendered again by the one render every send of news goes through, so the news
	// the answer carries is exactly what its text names. An empty text is a task that names nothing
	// now, which is retired rather than sent. It is asked only about a root architect's task of a
	// run (asksRoot), once that root has an architect to hear it, and nil leaves every task as it
	// was queued.
	RootTask func(ctx context.Context, c Claim, d Delivery) (task string, kept, news []byte, err error)
	// DesignNews is what a task that carries news of the approved design (Delivery.News) says now:
	// its text, and what it still carries of that news, re-read from the record at the moment the
	// task is about to be sent, by the one render every task of news goes through (the daemon's
	// renderNews), so the news it answers is exactly what the text it answers names. The gate it
	// names can have closed again, and an item it tells can have stopped standing while the task
	// waited — a later approval replaced it, a changes request cleared it, or another delivery's
	// credit told it — so the answer drops each such item from the text and from the news, re-keys
	// each item that stands to the version its audience holds now, and says the gate as it stands.
	// An empty text is a task that queued for its news alone and has nothing left to say, which is
	// retired rather than sent. It is asked only about a delivery that carries news, and nil leaves
	// every task as it was queued.
	DesignNews func(ctx context.Context, c Claim, d Delivery) (task string, news []byte, err error)
	// TreeClosable answers whether the operator may close this claim's tree: false refuses the
	// close, and an error is the read itself failing, which the caller sees as a failure rather
	// than a refusal. It is asked for the operator's own close — the workflow's linger close is its
	// own decision — and here rather than by the caller, so the answer and the stop it decides are
	// one decision rather than two round trips apart; and by a refused stop of the tree's root,
	// whose refusal then names that close. It is not a lock on what it reads: an issue recorded for
	// the tree commits in its own transaction and can still land between this answer and the
	// retire. nil closes every tree.
	TreeClosable func(ctx context.Context, c Claim) (bool, error)
	// ClaimActivity reads the activity report the store holds for launch generation of the claim
	// token (the store's PutClaimActivity columns), and whether it holds one: false for a launch
	// whose process has reported nothing, and for any launch but the one the row records. It is
	// what a let-go reads the background work the process it lets go reported from (recordEnd,
	// launchActivity), because the machine's claim holds only what it read at its own boot, and
	// it is read before the launch's first write, which clears the report as it moves the
	// generation. It is a read of the record, as RootTask is, bounded by the claim-write deadline
	// where it is read; nil reads no report, so every let-go of a process that ran a turn
	// records that the daemon has none.
	ClaimActivity func(ctx context.Context, token claim.Token, generation uint64) (ActivityReport, bool, error)
	// PluginContract is the daemon's own plugin contract number (api.DaemonAPIVersion; supervise
	// cannot import api), compared against each claim's recorded PluginContract to decide whether a
	// registration or a restored claim is stale.
	PluginContract int
	// PluginRelaunchable is whether this daemon can relaunch a stale process onto a plugin at all:
	// true under tmux always (the pane resumes on the host's own plugin, which the boot gate holds
	// to this daemon's contract), and under Kubernetes iff a plugin pair to install is known. False
	// means a stale claim is recorded and reported, never relaunched or refused — relaunching
	// nothing it cannot build, and refusing nothing a relaunch is not already under way to fix.
	PluginRelaunchable bool
	// NoPluginInstall is whether a launch of this claim installs no plugin at all: it runs in a
	// pod whose image carries its plugin, which no install reaches (a Claude Code role's
	// claude-envoy under Kubernetes). Only the daemon knows a role's backend and the runtime it
	// launches into — this package cannot read the configuration a role is launched from — so it
	// states the fact here, as it states its plugin contract and whether it can relaunch at all.
	// nil is a daemon whose every launch installs a plugin, which is what every caller that
	// states nothing means.
	//
	// A claim it answers true for, at any contract, is never relaunched by the stale-plugin lane
	// and never charged for its stale registration (staleRegistration): the relaunch would keep
	// the pod, bring the same plugin back and charge the claim toward failed, and a failed claim
	// cannot push its work. Such a claim is left running, and the update round over its pod is
	// what carries it: its role is asked the steps of UpdateTask like any other, on its own Envoy
	// topic (internal/daemon's openPodUpdate), and the replacement brings it up on a new image
	// (internal/daemon's refreshStalePlugins).
	NoPluginInstall func(c Claim) bool
	// TaskConfirmed is told of each claim whose pending delivery a turn has just confirmed: the
	// agent took the work the daemon gave it. The workflow decides what a root still owes its
	// architect from that (admission's ReconcileAwaiting), so without it a root whose wait was
	// held by an untaken task would wait for the next tick. Nil tells no one. It is called inside
	// the machine's decision, so it returns at once.
	TaskConfirmed func(c Claim)
	// SessionLost is told of each claim whose recorded session the launch forgot, once, at the
	// forget and before the fresh agent that replaces it is spawned (startFresh), so a spawn that
	// fails leaves the issue, its architect and the controller told all the same: each has to know
	// that a worker is continuing work it cannot read, and nothing else would say so. c is the
	// claim at the forget's launch, whose generation names it. cause is why the session was
	// forgotten, and detail is why in words the cause chooses: the runtime's own account of the
	// resume for a session no store holds, and none of it for the other two. Nil tells no one.
	SessionLost func(c Claim, cause SessionLoss, detail string)
	// SessionStoreFault is told when a start cannot reach the session store at all, at the first
	// such start and hourly while it lasts (startWaitsForStore): the claim is not failing and
	// nothing of it is lost, but no worker of it runs until a person fixes the store. Nil tells no
	// one. Both are called inside the machine's decision, as TaskConfirmed is, so both return at
	// once.
	SessionStoreFault func(c Claim, detail string)
	// SessionGone is told the Envoy session a claim recorded once no process stands behind it,
	// once per process end (sessionGone says where and when, and why then). It is the daemon's
	// clean stop of that session's registration, which makes the listener read it `gone` at once
	// rather than at the end of its live window (dispatch://INFRA-392), so a message to a role
	// released or suspended raises its wake rather than being stored for a session nothing reads.
	// The claim's own session id is what it carries, read before any path that clears it
	// (forgetSession). Nil tells no one — a daemon with no Envoy listener — and a claim that
	// recorded no session tells nothing. It is called inside the machine's decision, so it returns
	// at once and does the call elsewhere.
	SessionGone func(session string)
	// RoleHolder reads the session the Envoy listener names as role's holder, with that holder's
	// state (`live`, `unresponsive`, `gone`, or `lapsed` from an older listener), or no holder for
	// a role no claim names (notify.HTTPPublisher.RoleHolder). A forget of a session this claim
	// records but does not hold asks it once and refuses while the listener names that session as
	// the claim's role holder, in any state (mayForget): the claim's role topic opens once it
	// records no session, and the role's messages would then reach the other claim's process under
	// that session. An error is an answer the daemon does not have, and the forget refuses on it.
	// Nil is a daemon with no Envoy listener, where no role carries anything, and asks nothing. It
	// is called inside the machine's decision, bounded by Timeouts.RPC.
	RoleHolder func(ctx context.Context, role string) (holder, state string, err error)
	// StallBackstopped is told of each turn the daemon's stall backstop did something to
	// (backstopStalledTurn), with the record it acted on and the end it made: the owner's notice
	// says what happened to that worker, and only the machine knows it. The sweep that posted the
	// request cannot: its claims were read before the decision, the machine's own fence can leave
	// the turn alone, a suspension held for that turn ends the process rather than replacing it,
	// the wake pacing can park the relaunch the take-back has already begun, and a replacement
	// the store or the runtime refused on the way still ended the turn (ProcessEnd). Every end
	// but the one that changed nothing is told here, once per decision (ProcessEnd.TellsOwner). Nil
	// tells no one — a daemon with no workflow has no owner to tell. It is called inside the
	// machine's decision, so it returns at once and writes its row elsewhere.
	StallBackstopped func(c Claim, stop StallBackstopStop)
	// ProviderErrorRunReached is told of a claim whose turns have ended in a provider error
	// providerErrorRunTold times in a row, once per run, with the reason the project's controller
	// is given: the model, and the HTTP status the provider answered where it answered one, in
	// words built from structured fields alone. The claim carries the issue and the role. nil
	// tells nobody, as it does for TaskConfirmed above. It is called inside the machine's
	// decision, so it returns at once.
	ProviderErrorRunReached func(c Claim, reason string)
	// Paused is the daemon's project pause flag (decision 18 of dispatch://INFRA-3759's plan):
	// the id of the open pause and true while the project is paused, 0 and false while it runs.
	// The daemon hands the pause service's method value (internal/pause's Service.Paused), which
	// is one atomic load: it is read inside decisions, under the machine's lock, so it answers at
	// once and never waits on anything. Every machine of the project reads it for four things:
	// the gate that holds every launch of a claim that is not the controller's while the project
	// is paused (reserveWake, wakeNow, pauseGate), the send that hands such a claim no task then
	// (sendPending), a start that keeps the hold of the open pause (StartedBy), and a pause's
	// hold that is dropped at the start of a decision once its pause is no longer the open one
	// (dropEndedPauseHold). The controller's claim (claim.IsController) is never held by it. nil
	// is a project never paused, which is today's behaviour: nothing is gated, and a hold that
	// names a pause is one whose pause has ended.
	Paused func() (int64, bool)
	// PauseStopped is the stop transaction (decision 6): the claim write a suspension a project
	// pause asked for (RequestSuspend.Pause) ends in, in place of the plain write, together with
	// the pause's records, in one store transaction under the claim-write deadline
	// (Timeouts.ClaimWrite, defaultClaimWrite), which the machine applies to the context it hands.
	//
	// c is the claim as the plain write would write it, normalized (NormalizeClaim). Where
	// c.Pending is nil the plain write is the claim alone (Store.PutClaim); where it is not, the
	// plain write is the claim with that pending delivery in the same transaction
	// (Store.PutClaimAndDelivery(ctx, StoredClaim(c), *c.Pending)), and the implementation
	// writes both. stop names the pause (store.Pause.ID) and whether the role is owed a launch at
	// the resume (PauseStop.LaunchOwed, which the machine decides).
	//
	// internal/daemon's projectPause.stopped is the implementation, and it owns what the
	// transaction writes beside the claim and in what order.
	//
	// Its error is read by the rule a plain write's is (writeOwed). A failure the rule owes
	// leaves the stop owed on the machine (owedClaimWrite.stops), and every later claim write
	// comes back here with that same stop until one lands: the repair at the head of each
	// decision, and every write that settles an owed write when it lands (putClaim,
	// putClaimAndPending, the stop-first write of retirePending, StartedBy's persist). So c can
	// be a claim that is no longer suspended by then — a later write of a claim whose stop is
	// owed — and the transaction records the stop all the same. It must therefore be safe to run
	// again for one pause and claim: a commit whose answer the deadline cut off is owed and made
	// again, and must not open a second stop row, a second held wake or a second pod hold. nil
	// writes the claim through the plain write and records nothing of a pause.
	PauseStopped func(ctx context.Context, c Claim, stop PauseStop) error
	// WakeLetOut is told the moment the wake pacing lets a launch of the claim token (of issue's
	// role) out, at each reservation the machine takes for one (decision 14): the gate's own
	// (reserveWake), the one that lets the launch out at once included, and the let-out of a
	// launch the pause's gate held once its pause has ended (letOutHeldLaunch). at is the moment
	// the reservation answered, never before the decision's now. The daemon sets the stuck clock
	// of the role's open resume to it (store.Store.SetResumeClock on pod_resumes.clock_at, which
	// never moves a clock back), so a launch that waits its turn in a long queue is not warned
	// stuck for the wait. It is called inside the machine's decision, so it returns at once and
	// does its write on a goroutine of its own, as the durable-state report's podResume does in
	// internal/daemon. nil stamps nothing.
	WakeLetOut func(token claim.Token, issue string, role claim.Role, at time.Time)
	// Worker is the context a goroutine the machine starts applies its decision on: a timer that
	// fires (arm) and a prompt's send (startSend) each call Handle from a goroutine of their own,
	// beside whatever goroutine feeds the machine its events, and the decision writes the claim
	// through the store. The daemon gives each such goroutine a hold mark of its own for its
	// connection pool (store.ForConcurrentWorker, which this package cannot import: the store
	// imports it). nil applies the decision on the machine's context unchanged.
	Worker func(context.Context) context.Context
	// BoundWrite is ctx for one claim or delivery write, or claim read, of a decision (bounded):
	// the wait for a connection of the store ends acquire after it begins, and the call itself
	// ends write after it holds one, so a write queued behind the pool's other callers is not cut
	// by a bound that is about the write (Timeouts.ClaimWrite). The daemon sets its store's
	// (store.BoundAfterAcquire). nil, for a store whose writes queue for no connection, bounds the
	// whole call by write alone.
	BoundWrite func(ctx context.Context, acquire, write time.Duration) (context.Context, context.CancelFunc)
}

func (d Deps) check() error {
	for _, bound := range []struct {
		name  string
		value int
	}{
		{"Limits.LaunchFailures", d.Limits.LaunchFailures},
		{"Limits.PromptFailures", d.Limits.PromptFailures},
		{"Limits.PromptRetires", d.Limits.PromptRetires},
		{"Timeouts.RegistrationIntervals", d.Timeouts.RegistrationIntervals},
		{"PluginContract", d.PluginContract},
	} {
		if bound.value < 1 {
			return fmt.Errorf("supervise: %s must be at least 1, got %d", bound.name, bound.value)
		}
	}
	for _, wait := range []struct {
		name  string
		value time.Duration
	}{
		{"Timeouts.Boot", d.Timeouts.Boot},
		{"Timeouts.RPC", d.Timeouts.RPC},
		{"Timeouts.Probe", d.Timeouts.Probe},
		{"Timeouts.Stop", d.Timeouts.Stop},
		{"Timeouts.LaunchBackoff", d.Timeouts.LaunchBackoff},
	} {
		if wait.value <= 0 {
			return fmt.Errorf("supervise: %s must be positive, got %s", wait.name, wait.value)
		}
	}
	return nil
}

// defaultClaimWrite is the claim-write bound a Deps that names none takes, which in production is
// every one of them. Five seconds is chosen for what such a write is: one claim row, and at most
// one delivery row beside it, written to a Postgres on this cluster, which a healthy database
// answers in single-digit milliseconds. Five seconds is therefore hundreds of times a healthy
// write — so no write that was going to land is ever cut short by it — and still a small fraction
// of any outage worth the name. A database that stops answering holds the machine's lock for up to
// about twice this per bounded call that holds a connection, and for at most defaultClaimAcquire,
// this and the commit's settle together (37 s) for one that queues first, the bounds
// store.BoundAfterAcquire states. A call this bound cuts after the database was handed it (its
// BEGIN answered) is made once more at once (write, claimWriteTries, cutByBound), and the second
// try may first wait in the pool's queue: pgxpool frees the slot of the first try's connection
// only once its destructor has closed it, which it gives up to 15 s on a server that answers
// nothing (pgxpool/pool.go:328-337 at v5.13.0; puddle/v2 pool.go:695-700). So such a write holds
// the lock for two tries' bounds before it is owed: about 10 s when the second try is handed a
// connection at once, 25.0 s measured on a pool of one connection whose server went silent after
// the BEGIN (5.0 s before this retry), and at most two tries of 37 s. A dial, a queue wait or a
// BEGIN the bound cut is owed at once.
// The write a second cut leaves is not lost: a bare context deadline is a write the server never
// answered, which writeOwed owes, so the claim write is marked and made again at the claim's next
// decision (owes, repair).
//
// It counts the write and not the wait in the pool's queue (Deps.BoundWrite). Counted from before
// that wait, it was the queue's bound too: a boot relaunches every unfinished claim at once, each
// on a goroutine of its own, through a pool of max(4, CPUs) connections, so the last of forty
// writes waited out thirty-odd writes before its own, and on a loaded node four of forty such
// writes passed five seconds with almost all of their time in that wait (internal/daemon's
// TestWithNoPacingTheBootWaveGoesOutInOneTick, and a measurement of its writes: acquire about 90%
// of each write's time). Each such launch was cut, owed, and tried again only at the next probe.
const defaultClaimWrite = 5 * time.Second

// defaultClaimAcquire bounds a bounded write's wait in the store's pool queue (Deps.BoundWrite),
// before defaultClaimWrite's count begins. It covers the queue alone: every connection the pool has
// handed out comes back within its holder's own bound, so the queue drains while the database
// answers nothing, and thirty seconds is a queue six writes deep per connection, against healthy
// writes of milliseconds, so no write that would land waits it out. It ends a wait for a pool
// whose every connection is held by work that has no such bound. A write it cuts is owed like any
// other and made again at the claim's next decision. It is no setting and no field: nothing needs
// another value of it.
const defaultClaimAcquire = 30 * time.Second

// claimWrite is Deps.Timeouts.ClaimWrite, or defaultClaimWrite when it names none. It is a
// fallback rather than a check, so a Deps built without it still writes under the deadline.
func (d Deps) claimWrite() time.Duration {
	if d.Timeouts.ClaimWrite <= 0 {
		return defaultClaimWrite
	}
	return d.Timeouts.ClaimWrite
}

// RefusedError is a request the claim does not allow — the answer an API route gives its caller
// instead of pretending the request happened. A refusal the transition table makes is the claim's
// state refusing the request, and says which state and why (Reason). One an action makes is about
// something else — a delivery already pending, the claim being its tree's root — and carries the
// sentinel a caller can act on (Err: ErrDeliveryPending, ErrRootStop), which is what it says.
type RefusedError struct {
	State   ClaimState
	Request string
	Reason  string
	Err     error
}

func (e *RefusedError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s refused: %v", e.Request, e.Err)
	}
	return fmt.Sprintf("%s refused: the claim is %s (%s)", e.Request, e.State, e.Reason)
}

func (e *RefusedError) Unwrap() error { return e.Err }

// ErrDeliveryPending is a delivery refused because the claim already holds one: a wait, not a
// fault — the claim takes the next task once its pending delivery's turn is over, so the caller
// asks again later.
var ErrDeliveryPending = wait.New("a delivery is already pending")

// ErrRootStop is a stop of the tree's root claim that is not its tree's close, refused in every
// state: the root ends only with its tree. The refusal adds what stops the root's process instead,
// where one runs.
var ErrRootStop = errors.New("the tree's root claim ends only when its tree closes")

// ErrForeignUpdateRound is a ready_for_update that names a round the claim is not in: the round
// it answers for was replaced while its worker worked, or the claim is in no round at all. The
// answer counts for the round whose task the worker read and for no other, so it is refused
// rather than recorded against a round nobody asked this worker about (podUpdateSignalled). The
// refusal adds both rounds and what the worker does next.
var ErrForeignUpdateRound = errors.New("the answer names an update round this claim is not in")

// ErrLeftPodRound is a ready_for_update from a claim the daemon started again in another pod
// than the one its round was opened over (Claim.LeftPodRound). The answer says the round's work
// is pushed, and the process that gives it never held that work: it came up in a pod the round
// asks nothing of. It is refused rather than counted for a pod nobody pushed for. The refusal
// adds the round the claim left and what the worker does next.
var ErrLeftPodRound = errors.New("the answer comes from a process in another pod than the update round's own")

// ErrRetryNotTheController is the operator's retry of the daemon's own controller claim, which the
// controller keeper retries instead (operatorRetry). The refusal adds who retries the claim.
var ErrRetryNotTheController = errors.New("the operator's retry does not relaunch the daemon's controller")

// ErrHeldByOperator is the daemon's start or task meeting an operator's suspension, which no rule
// overrides (Claim.RuleHolds): a wait, ended only by the operator's resume. RuleHoldError wraps it
// for the outbox log.
var ErrHeldByOperator = wait.New("the suspension holds until the operator resumes the claim")

// ErrNoSuchDelivery is the store's refusal of a retirement of a delivery the claim's row no
// longer holds. It is named here, rather than matched as a string where the refusal is read,
// because internal/store imports this package and so cannot be imported back: the store's
// refusals can only carry an identity this package owns. It is a sentinel so that a caller can
// tell a retirement the store has already made — a commit whose answer a broken connection lost,
// made again and finding nothing left to delete — apart from any other failure of that write,
// which is the difference between a claim that is settled and one whose write is still owed.
var ErrNoSuchDelivery = errors.New("the claim holds no such delivery")

// ErrOtherDelivery is the store's refusal of a retirement whose claim row holds a delivery under
// another id than the one being retired. Because pending_task_deliveries is keyed on the claim,
// a delete that removes no row says one of two things, and they call for opposite answers: the
// row holds nothing, which is the retirement this daemon already made (ErrNoSuchDelivery), or it
// holds another delivery, which is a write that rotated the id — an interruption, a take-back —
// whose answer the claim-write bound cut off and which committed all the same, so the store
// holds B where memory holds A.
//
// The two are told apart because reading the second as a retirement already made would drop A
// from memory and leave B stored: a delivery no machine holds, unconfirmed and never retired,
// whose kept decisions nothing releases. So it is a failure of the write like any other, and it
// is a sentinel of its own that does not wrap ErrNoSuchDelivery, so that a caller asking
// errors.Is for the retirement already made never catches this instead. It is named in this
// package for the reason ErrNoSuchDelivery is: internal/store imports this package and so
// cannot be imported back.
var ErrOtherDelivery = errors.New("the claim's row holds another delivery")

// Machine is one claim's decision owner.
type Machine struct {
	mu             sync.Mutex
	ctx            context.Context
	deps           Deps
	log            *slog.Logger
	claim          Claim
	onDurableState func(Claim, ClaimState)

	timers map[TimerKind]armed
	seq    uint64
	// send is the prompt in flight, from the moment its goroutine starts to the moment its
	// outcome is handled; helloDuringSend is a shim hello that arrived meanwhile — the trigger for
	// a re-send, should that send fail.
	send            *sending
	helloDuringSend bool
	// unsaved is a pending delivery the store's row does not hold as memory holds it — a write of
	// it the store did not take (saved), a row a rotation the daemon never heard of left under
	// another id (retirePending), or the copy of an unconfirmed task's news that letGo dropped in
	// memory alone (letGo): settle writes it, with the claim, before the next decision.
	unsaved bool
	// owedWrite is the claim write a decision made and the store did not take, nil for none: the
	// mark that the stored row is behind what memory holds, and that the write is to be made
	// again at the head of the claim's next decision (owes, repair). It is a mark of its own
	// rather than unsaved, which says the same of the pending delivery, because unsaved is
	// cleared by writes that say nothing about the claim write owed: queue and retirePending both
	// set it false when their own write lands, so a claim write marked with it would be dropped
	// without a word. It is memory only, as unsaved is: a daemon that stops with a write owed
	// loses the mark, and the next boot builds the machine from the stored row — the row the
	// failed write left unchanged — which is the same claim a crash between a decision and its
	// write has always left behind.
	owedWrite *owedClaimWrite
	// sentThrough is the registration sequence of the newest connection a prompt was sent through
	// (takesBackConfirmed). A send always goes through the connection registered last, so it only
	// rises, and a relaunch leaves it: the new process's connections are registered later still.
	sentThrough uint64
	// openCalls is every tool call of this claim whose start was read and whose end was not,
	// keyed by the call id the two frames pair by (openCall, holdOpenCall). It is what an end
	// frame is written with, since the end repeats neither the arguments nor the intent. It is
	// memory only: a call open across a restart is recorded by its end alone, with no arguments
	// measured, which is what the daemon then knows of it.
	openCalls map[string]openCall
	// steppedAt is the last step this machine itself read of its claim — a tool frame that moved
	// the record's time, or a turn's own start — which the claim it holds does not carry: the
	// record is written through the store's narrow call and never read back into a machine's
	// claim (putStep). The stall backstop reads it beside the claim's own record, which is the
	// record as the store held it when this machine was built, and acts on the later of the two
	// (backstopStalledTurn). It is memory only, and it is what makes the fence exact: a frame
	// that arrived after the sweep read the claims is already in this machine's inbox, ahead of
	// the backstop's own event, so a turn whose agent moved in that window is left alone.
	steppedAt time.Time
	// answer is the model that answered last and the session it answered in (answered,
	// LastAnswer), nil before any. It is apart from mu, so a read never waits on a decision, and it
	// is memory only, as steppedAt is.
	answer atomic.Pointer[sessionAnswer]
	// sessionFrom is the first launch of the claim's current session: the launch that spawned it
	// (spawn), which every later resume continues. The fence takes an answer of any launch from it
	// to the claim's generation as the session's own and drops an answer of an earlier launch, which
	// answered a session the claim has left. A machine read back at boot starts it at the claim's
	// generation: no answer of an earlier launch is in this daemon's stream. It is memory only, as
	// answer is.
	sessionFrom uint64
	// lostCause is the cause of the forget the launch made last (startFresh), and "" where none is
	// kept; the rule of how long it is kept is written here alone. It is kept from that forget
	// until the claim's first ready after it (ready), or until a later forget that drops a session
	// (forgetSession): an operator's forget-session, or a retry in a new session, of the fresh
	// agent's own session. A forget that drops nothing keeps it. Every launch until then is built
	// with it (spawnSpec reads it into Claim.SessionLost): the retry of a fresh spawn that failed,
	// a relaunch of the fresh agent before its ready, such as the stale-plugin relaunch that
	// resumes its new file, and the spawn after a retry in a new session that dropped nothing, so
	// the agent that comes up is told in its own prompt that it does not continue the session the
	// claim forgot. The session is not forgotten again: the claim records none by then, or the
	// fresh agent's own. A forget drops nothing where the fresh spawn failed until the launch
	// budget ran out, or the fresh agent never registered: the claim records no session, and a
	// retry in a new session then (the controller's fresh_session, `legion claims retry
	// --fresh-session`) leaves the session the cause is about as the one the claim abandoned
	// (Claim.Abandoned), which the agent it starts still replaces. A forget that drops the fresh
	// agent's own session is followed by a launch that replaces that session, not the one the cause
	// is about, and the cause kept past it would tell the agent that comes up something false about
	// the session it replaces. It is memory only: a restart loses it and the prompt's sentence with
	// it, and the reason the claim stores with the abandoned session (Claim.Abandoned) still tells
	// the fresh agent at its first task.
	lostCause SessionLoss
	// refusedEarlierRelease is the generation at which the fence refused the registration of a
	// process an earlier release started, for a claim that records a session and no file
	// (markRollEdge), and 0 where no such refusal is outstanding. Such a process is a fresh agent
	// that release spawned for the claim, registering as a session of its own on that release's
	// older plugin contract: this release starts no fresh agent for a claim that still records its
	// session, so the refusal stands, and the end of that process — its death, or its registration
	// deadline — is charged nothing (endUncharged), so the launch failures the earlier release's
	// loop charged do not fail the claim at the deploy, and its next launch resumes the session by
	// its id or forgets it once. The next launch clears it (launchNow). It is memory only: it must
	// hold across the one refusal and the end after it, in one daemon run, and a restart in that
	// window costs the claim the one launch failure that end is then charged.
	refusedEarlierRelease uint64
	// outstanding is every prompt of the pending task that Oh My Pi acknowledged and has not yet
	// reported the end of (shimwire.PromptResult), by delivery id. The pinned Oh My Pi answers each
	// prompt with exactly one prompt_result, owned by the first run after the prompt's admission
	// (rpc-prompt-results.ts), and refuses a prompt that arrives while another is admitted ("Agent
	// is already processing") rather than queue it; the Claude Code bridge takes one prompt at a
	// time too (internal/claudebridge's Busy and promptRun). So the session's next turn after an
	// acknowledgement is that prompt's, however long its pre-turn work runs past the wait for it
	// (noTurn): a turn that starts while an id is held here is the task's (turnStarted), and a
	// result naming one is the task's whichever id the task has moved on to since (fence,
	// promptResulted). While one is held, a turn that ends is not known to be the task's — with
	// background work on, a role's own background result wakes it and starts turns the daemon
	// never prompted — so the delivery's life ends at its result rather than at any agent_end
	// (turnEnded, resultPending).
	//
	// acked and ackedInTurn add an id, with the news of the approved design that prompt carried,
	// which a confirmation of that prompt's turn credits (turnAnswers, turnNews). A result or a
	// refusal naming one removes it, and the result of a prompt that ran a turn ends the wait for
	// them all (promptResulted); the wait for a turn that runs out (noTurn) takes the task back and
	// keeps its prompt here. The sweep's answer that the agent is in no prompt empties it
	// (askTurnOver), and so does a compaction, which aborts whatever run the session had
	// (compactStart). letGo, retirePending — and with it queue, when a newer task takes the pending
	// one's place — and interrupted empty it too, since what it waits on goes with the process or the
	// task. Each of those empties it through forgetOutstanding.
	//
	// It is memory only: a daemon restarted inside a late turn's wait restores the claim with no
	// prompt outstanding and asks the agent where it is instead (askFirst), so that window keeps
	// the behaviour it had before the set: a turn that starts is taken for the task by askFirst,
	// and credits the delivery's own copy (Delivery.Carried).
	outstanding awaitedPrompts
	// turnOutcomeFailed is whether the turn-outcome report this decision handles failed the
	// claim: turnOutcome clears it, unpromptedTurnEnded sets it when it fails the claim, and
	// HandleTurnOutcome reads it and clears it before the decision's lock is let go, so a report
	// the table ignores, which runs neither, answers that it failed nothing. It is memory only and
	// means nothing outside that one decision.
	turnOutcomeFailed bool
	// turnOutcomeDecided, when it is set, runs once HandleTurnOutcome's decision has let its lock
	// go and before it returns: the moment another event's decision can run between a report's
	// decision and a read of the claim made after it. Only a test sets it, to run such a decision
	// there (TestTheTurnOutcomeAnswerSaysWhetherThisReportFailedTheClaim); it is nil otherwise.
	turnOutcomeDecided func()
	// askFirst is a restored claim whose agent an earlier daemon was talking to: a pending
	// delivery it may already have sent, or a turn it saw start and may not have seen end. The
	// machine asks the agent (get_state) before it acts on either. On a working claim it is one of
	// the two marks that arm the sweep's question whether the turn is over (askTurnOver), busyTurn
	// being the other, and the one an answer of "streaming" disarms for good.
	askFirst bool
	// unansweredAsks counts the asks in a row the agent left unanswered while askFirst or busyTurn
	// stayed armed on a working claim (askTurnOver); from unansweredAsksWarn on, each one is
	// logged at Warn.
	unansweredAsks int
	// held is the suspension held for the agent's turn (holdSuspension), or for a launch's ready
	// (holdSuspendUntilReady), nil for none.
	held *RequestSuspend
	// heldLaunch is whether the held suspension waits over a launch — one holdSuspendUntilReady
	// keeps until the agent says ready — and false whenever held is nil: it is set with held and
	// cleared with it (dropHeld, letGo). A project pause's stop owes the role that launch at the
	// resume once the hold runs (PauseStop.LaunchOwed, suspendHeld, endHeld), and nothing else
	// says it by then: the claim reads ready, or its process is gone. It is a mark of its own
	// rather than a field of the request, because the request is the event the daemon posts and
	// carries nothing a caller may not set.
	heldLaunch bool
	// staleRelaunch is the stale-plugin relaunch held for the agent's turn to end, naming the pass
	// that found the claim ("boot", "sweep"); nil for none. Relaunching a stale claim where it
	// stands would end a turn whose worker the stale-plugin lane asked nothing of, so the relaunch
	// waits for the turn (refreshPlugin, turnEnded). It is memory only, as the held suspension's
	// deadline timer is: the daemon's boot pass and every orphan sweep post StalePlugin again for
	// a claim still stale (refreshStalePlugins in internal/daemon), so a restart that forgets this
	// mark finds the same claim again, and letGo drops it with the process it was made for.
	staleRelaunch *string
	// busyTurn is the claim recorded working by Oh My Pi's busy answer to a prompt of its pending
	// task (recordBusyTurn): the agent said its session is in a prompt, preparing or running a
	// turn, and no agent_start of that turn has been read yet. It arms the sweep's question
	// whether the turn is over (askTurnOver), as askFirst does for a restored claim, and an answer
	// of "streaming" leaves it armed: the record is the daemon's own reading of a refusal, and the
	// turn it stands for may send no frame this daemon ever reads, so only the agent's word that
	// it is in no prompt, or the turn's own frames, end it. The turn's agent_start clears it and is
	// decided then as any start is (turnStartedWhileBusy); turnEnded clears it with the turn, and
	// letGo with the process. It is memory only: a daemon restarted inside such a turn restores
	// the claim working, which arms askFirst instead (NewMachine), and that asks the same question.
	busyTurn bool
	// turnsDelivery is the delivery the turn now running is the turn of, as that turn's own start
	// line named it, and "" for a turn that is no task's. It is set where the machine learns a
	// turn started (startTurn, on each of the four paths into working), read where that turn ends
	// (endTurn), and cleared with the turn, so the end line names what the start line named rather
	// than what the claim is left holding. The one start that sets it without a start line of its
	// own is the agent_start of a turn a busy answer recorded, whose start line was written at the
	// record and named no delivery: that start names the task it confirms in a line of its own,
	// and the end names it too (busyTurnBegun). It is memory only, as askFirst is: a daemon
	// restarted mid-turn holds none, and the end line of that turn then names no delivery rather
	// than guessing one, which is the safe answer — the empty value says "this turn names
	// no delivery" for both a turn that is no task's and a turn this daemon never saw begin, and
	// both are lines a reader can trust.
	turnsDelivery string
	// cutTurnOf is the id of the pending task the decision that lets the claim's process go takes
	// back from that process (interrupted), and "" while no decision has taken one back from it.
	// interrupted sets it to the id it gives the task. The two decisions that record the end of a
	// process they found gone before they take its task back (died, displaced) set it first to the
	// id the confirmed task still holds, since the take-back follows in either whatever the claim's
	// state (recordGoneEnd), and the take-back then moves it to the new id. The take-back clears
	// the task's confirmation, and it takes back a task whose turn ended while its prompt's result
	// was still to come as it takes back one whose turn runs, so this, rather than the confirmation
	// or the claim's state, is what tells the let-go that follows — in the same decision, or at the
	// turn of a wake the replacement parked — that the task's turn was cut off (recordEnd,
	// EndedWork.TaskTurn). letGo clears it with the process. It is memory only: a daemon
	// restarted inside such a park records the end without it, from what the claim still holds.
	cutTurnOf string
	// carried is which of the claim's two untold records a prompt this machine sent to the claim's
	// current process carried the sentence of (sentences, Delivery.message): the session the claim
	// abandoned, and what its last let-go ended, each marked only where the prompt's text carries
	// that record's own sentence. It is what confirmCarrying reads to mark each record told, so a
	// turn whose prompt went out before a record existed — the takeover of a process another
	// runtime still runs, whose relaunch the pacing parks — tells nothing of it. It is set where
	// the send goes out (startSend), and goes with the process (letGo) and, for the record of the
	// let-go, with that record (recordEnd), since a newer record is words no earlier prompt
	// carried. It is memory only: a daemon restarted inside the turn of such a prompt reads none
	// (confirmCarrying).
	carried sentences
	// compacting is the compaction the claim is in (Machine.Compact), nil for none: the claim is
	// working for as long as one runs, and this is what tells that working apart from a turn's —
	// a turn that ends under a compaction leaves the claim where it is (turnEnded), and only
	// CompactEnded gives it back. It is memory only, as the held suspension is: a daemon that
	// restarts mid-compaction leaves a claim stored working, whose agent's own get_state answer
	// then ends it (askTurnOver).
	compacting *compacting
	// lostCompaction is the compaction a gone verdict dropped with the process it ran over
	// (compactionLostProcess), kept for the let-go of that process alone and cleared by letGo,
	// nil for none. The verdict drops the compaction before the death or the displacement
	// records what the process ended, and a relaunch the wake pacing parks lets the process go
	// a wake's turn later with the claim still in the working the compaction took it to, so
	// recordEnd reads this, as it reads compacting, to tell that working from a turn's
	// (endState). Nothing else reads it: for the rest of the daemon the compaction is gone. It
	// is memory only, as compacting is.
	lostCompaction *compacting
	// heldStopBack is the state the claim's held suspension finds it in when it runs at the end of
	// the turn, or of the compaction, the claim is still stored working for (suspendHeldAtEnd):
	// the state that end gives the claim back in. That is idle after a turn, and after a
	// compaction the state it gives back (compacting.back): the state it took the claim from
	// where no turn ran under it, and idle where a turn ran and ended under it (endState). It is
	// "" at every other moment. Nothing runs any more at that end, so the let-go the stop makes
	// reads the claim in this state rather than as a turn cut off (endState, recordEnd). The
	// claim's own state stays working through the stop, as it always has: the stop publishes the
	// claim while the runtime stops it (stoppingProcess), and the end gives the claim its state
	// back only once the hold's outcome is known (turnEnded, compactEnded).
	heldStopBack ClaimState
	// processGone is what the runtime's last report of the process the claim records found
	// (reportedProcess, Claim.ProcessGone), and false until a report has found that process
	// gone. Every report the machine judges writes it — a gone verdict sets it and a verdict
	// that found the process running clears it — and letGo clears it with the process it is
	// about, so it is never read of a process the claim no longer records. A report that is no
	// answer leaves what the last answer said (judge's uncertain verdict).
	//
	// It is memory only, as compacting is: a restart has had no report of any claim's process
	// and reads one stored working as a claim whose agent it asks where it is (askFirst,
	// askTurnOver), which ends such a turn without it. What was charged for that process's
	// death is the claim's own and durable, because a restart must not charge it again
	// (Claim.DeathCharged).
	processGone bool
	// goneOutOfMemory is whether the report that set processGone found that process's pod ended
	// for running out of memory (runtime.Observation.OutOfMemory), and false wherever processGone
	// is: written and cleared with it (reportedProcess, letGo). It is kept for the one path that
	// takes the dead turn's task back after the report rather than at it — a suspension that
	// reaches the claim over that process (suspendOverADeadProcess) — so the task goes back with
	// the note the death would have given it (interrupted, outOfMemoryTask). It is memory only,
	// as processGone is, and for the same reason.
	goneOutOfMemory bool
	// goneCause is the runtime's account of the death the report that set processGone found
	// (deathCause), and empty wherever processGone is false: written and cleared with it. It is
	// kept for the same suspension, which records the dead process's end as the death it is under
	// this account (recordEnd, endDied) once the dead turn's task is taken back, unless a death
	// died judged already recorded it, so the task is sent again saying why its process died
	// rather than that a suspension ended it: the sentence in front of it reads that record, its
	// own while the record is untold (relaunchSentence) and the record's cause once it is told
	// (interruptedTask, EndedWork.Cause). Before the take-back the suspension also puts it on the
	// claim as died would have (Claim.LastDeathCause), which the operator's listing and a failed
	// claim's notices read, and it puts back the cause the claim held when the take-back's write
	// fails (suspendOverADeadProcess).
	goneCause string
	// previous is the incarnation the claim last ran and no longer records — stopped by a
	// suspension, retired, failed on, or found dead — which every launch of the same session hands
	// the runtime to wait out until one starts. letGo is the one way a process gets here. It is
	// memory only: after a restart the boot orphan sweep has reaped every process no claim records.
	previous *runtime.Locator
	// stopUnconfirmed is whether the runtime left the process this replacement let go possibly
	// running: it refused the stop and the machine went on anyway (endHeld, stopPrevious),
	// or the daemon's own stop cut the launch short before the stop was made (launchTries). It is
	// the one thing a caller cannot read off the claim the replacement leaves — the claim records
	// no process either way — and a stalled worker's owner is told of it, since a process the
	// runtime would not stop goes on holding what it runs on (StallBackstopStop.StopUnconfirmed,
	// the daemon's stallBackstopRecord). replaceProcess clears it before the replacement it
	// reports on, so it names that replacement and no earlier one; a later stop of that same
	// process that the runtime does make clears it too, since the question is whether this one
	// process was seen to stop and the latest answer about it is the answer (stopPrevious); and
	// a start that succeeds clears it as well: the launch that started was handed the process it
	// replaces (start).
	stopUnconfirmed bool
	// relaunch is the relaunch relaunchLater left waiting, tried again when TimerRelaunch fires; it
	// lives as long as that timer, which letGo disarms with it. It is memory only, as the timer is: a
	// restart launches that claim again as an unfinished launch.
	relaunch func(context.Context) error
	// cancelStart cancels the start in flight, and is nil between starts; startStopped is whether
	// a suspension cancelled one, read once by that suspension's own row (stoppedAStart). startMu
	// is their own lock, held only to set, clear or call them, so a suspension can reach them
	// while the decision that started the launch holds mu for minutes (stopStart, startable).
	// storeFaultTold is when the issue and the controller were last told the session store could
	// not answer, so a start that retries every minute reports once an hour (startWaitsForStore).
	startMu        sync.Mutex
	cancelStart    context.CancelFunc
	startStopped   bool
	storeFaultTold time.Time
	// podRoundWaiting is whether the retry relaunchLater has armed is one of a start the runtime
	// refused for want of its pod's update round (startWaitsForPodRound): the wait is logged once,
	// at the refusal that begins it, and not at each minute's retry of it. letGo clears it, so
	// every other way out of that wait — a start that succeeds or fails otherwise, a suspension, a
	// retirement — makes the next such refusal a new wait that logs again. Memory only, as
	// relaunch is: a restarted daemon's first refusal logs.
	podRoundWaiting bool
	// awaitsPod is the live pod that wait is for, as the runtime's refusal named it
	// (runtime.AwaitingPodRound), and nil for none or for a refusal that named no pod. It is what
	// Claim.AwaitsPod reports (current), which the daemon's update round reads. It is set where
	// the wait begins and at each refusal of its retries (startWaitsForPodRound). letGo clears it
	// with podRoundWaiting, except that every launch keeps it through its own try, whatever raised
	// the launch, so a sweep that reads the claim while that try runs still finds the pod; that
	// try's outcome then settles it (launchTries). Memory only, as podRoundWaiting is.
	awaitsPod *runtime.Locator
	// registeredPods is the set of pods an agent of the claim registered a session in: what
	// Claim.RegisteredPods reports (current). NewMachine reads it off the claim it is built over
	// (bootRegisteredPods), and register adds to it; nothing else writes it, so neither letGo nor a
	// launch takes a pod out of it. Memory only, as awaitsPod is.
	registeredPods RegisteredPods
	// podLoad is the gate's hold of the session load of the claim's process in its issue pod, from
	// that process's start until its load ends, at its settle after the registration or at an end
	// of the process before it (podStarted, endPodLoad), nil for none and for every claim the pod
	// start gate does not hold (Deps.PodStarts). Its loadStart and line are this machine's to read
	// (podHold). It is memory only, as the gate is: a restart gives the pod the load of a claim it
	// restores booting (adoptPodLoad).
	podLoad *podHold
	// podPark is the gate's hold of the start of the launch parked at the claim's issue pod's gate
	// (askPod), from the park until the grant runs that start (podStartDue) or the start leaves
	// the gate (endPodPark), nil for none. The parked wake carries the same hold (wake.park); this
	// is the machine's own record of what it asked the gate, kept apart from the wake so that a
	// decision that drops or replaces the wake, by whatever path, takes the start out of the gate
	// as it ends (Machine.unlock). It is memory only, as the gate is.
	podPark *podHold
	// wake is the launch the wake pacing holds until its turn (reserveWake), run when TimerWake
	// fires (wakeNow); nil for none. It lives as long as that timer, which letGo disarms with it,
	// and it is memory only, as relaunch and staleRelaunch are: a restart loses no wake. A
	// process-less one parked the claim launching with no process, which the boot repairs to
	// launch_uncertain and relaunches; one over a process whose death the claim's record charges
	// is raised again by the boot's replay of that death (RecordedDeath). The boot reserves the
	// turns of both in one order, the order their claims' work was owed (RequestSpawn.Turn,
	// RecordedDeath.Turn), so the queue it rebuilds keeps the order this one drained. One over a
	// process still running is posted again by the boot's own stale-plugin pass, and one over a
	// death no record charges is raised again by the runtime's next observation of the dead
	// process. Each is paced again from an empty bucket, and no wake runs twice. A launch waiting
	// for its issue pod's start is held here too, until the gate's grant fires TimerPodStart
	// (askPod, wake.park, podStartDue), and a restart finds it as it finds a process-less wake.
	wake *wake
	// pluginRelaunch is the intent of the stale-plugin relaunch this machine is raising: the
	// contract that relaunch is made for and the wake kind its launch waits under
	// (pluginIntent, markPluginRelaunch), and nil whenever the launch being raised is not one.
	// It is memory only, and it belongs to one launch at a time: the gate reads the kind off it
	// and takes it onto the wake it parks, the turn hands it back (reserveWake,
	// wake.pluginMark, holdPluginRelaunch), and the launch consumes it as it starts (launchNow,
	// commitPluginRelaunch). It never outlives the decision that raised it, which the end of
	// every decision holds it to (endRefreshLane): a retry that still owes one carries it in its
	// own closure (retryHolding). The mark itself is the claim's and durable, written by the
	// launch that commits this intent.
	pluginRelaunch *pluginIntent
	// refreshCredit is the credit of the stale-plugin refresh gate this machine holds, nil for
	// none: one of the K the lane may have in the wake pacing at once, taken before the relaunch
	// it pays for was decided — by the daemon's pass, which hands it over on the post it makes
	// (StalePlugin.Credit), or inside the decision itself (refreshPlugin, admitRefresh) — and made
	// this machine's own in one place (holdCredit). It goes back at the end of the decision that
	// holds it unless a refresh wake of this machine's own is waiting (endRefreshLane), and a
	// sweep's correction takes it back from a machine that holds it for no such wake
	// (reclaimCredit). One is all a machine holds: a post that arrives carrying a credit over one
	// held gives its own straight back (refreshPlugin). It is memory only, as the wake it is
	// taken for is: a restart leaves the gate empty.
	refreshCredit *RefreshCredit
	// identity is the latest hello2 identity for the claim's current incarnation,
	// kept until its enrollment lands; enrollmentSent is whether that enrollment has been handed
	// to the shim over the claim's current connection.
	identity       *heldIdentity
	enrollmentSent bool
	// heldNames is every name of the session the claim records that the store answered as this
	// claim's (holdRecorded): such a name is not asked again while the claim records that session.
	// An operator's release keeps the names of the recorded file and id, may free one of an earlier
	// file of the session whose name contains no id, and takes each name it frees out of the cache
	// (ReleaseNames). forgetSession empties it. Memory only: a restarted machine asks once more.
	heldNames map[Hold]bool
	// unheldDelivery is the pending delivery sendPending last held back because the claim records a
	// session it does not hold, so its Error line is written once per delivery rather than at every
	// sweep that asks again. Memory only.
	unheldDelivery string
	// refusalLogged is the Warn line holdRecorded last wrote for a refused hold, so a refusal asked
	// again at every sweep (sendPending, for a claim that does not hold its session) is written
	// once rather than every probe interval; a hold admitted clears it (warnRefusal). Memory only.
	refusalLogged string
	// admitted is the session and file the store last admitted for this claim, which holdRecorded
	// and HoldsItsSession write, and HoldsItsSession and Admits (the daemon's step record) read
	// without the lock; forgetSession clears it with heldNames. Memory only.
	admitted atomic.Pointer[admittedSession]
	// registration is the pod-session registration this machine keeps written for the claim's
	// current process (ensureRegistered), nil while it keeps none. Memory only: a restored claim
	// writes it again at its shim's first hello to the new daemon.
	registration *podRegistration
	// registrationRefused is the incarnation and session ensureRegistered last wrote a hold's
	// refusal at Error for, so a refusal asked again at every observation is written once; a
	// registration the hold admits clears it. Memory only.
	registrationRefused registrationKey
	// helloKey is the thumbprint the first hello of the claim's current incarnation named, which
	// every registration of that incarnation is bound to (ensureRegistered); a later hello of the
	// same incarnation naming another key is logged and changes nothing. A machine with no
	// Registrar pins none and logs none, since it keeps no registration for a key to be bound to.
	// Memory only, like registration: a restored claim pins the key its shim's first hello to the
	// new daemon names.
	helloKey helloKey
	// stale is every stale event already logged, so a repeated one is dropped in silence.
	stale map[string]bool
	// goroutines counts sends whose outcome has not been handled yet; idle wakes Wait.
	goroutines int
	idle       *sync.Cond
	// view is the claim View reports: published under mu as every locked section that can change
	// the claim ends (unlock) and at every write of the claim (persist), and read without mu, so a
	// reader never waits on a decision in flight. stopping is whether that decision is stopping
	// the claim's process for good (ClaimView.Stopping); the send in flight is read from send
	// itself, which only a locked section changes (ClaimView.SendInFlight).
	view     atomic.Pointer[ClaimView]
	stopping bool
	// deciding is the event the decision in flight handles, nil between decisions (Deciding). It
	// has a lock of its own, held only to set, clear or read it, so a reader never waits on mu.
	decidingMu sync.Mutex
	deciding   Event
	// turnsEnded counts the turns whose end this machine has recorded (turnEnded), whichever event
	// ended them, for the daemon to read across a decision (TurnsEnded). It is atomic so that read
	// never waits on mu.
	turnsEnded atomic.Uint64
}

// sending is the send in flight (startSend): its delivery id and generation. What its prompt
// carries of news of the approved design is the pending delivery's own while the id is the
// pending one's (startSend says why), so the send keeps no copy of it.
type sending struct {
	id         string
	generation uint64
}

// awaitedPrompt is one prompt of the pending task that Oh My Pi acknowledged and has not yet
// reported the end of (Machine.outstanding): its delivery id, and the news of the approved design
// it carried, as its first acknowledgement recorded it (awaitResult).
type awaitedPrompt struct {
	id   string
	news []byte
}

// awaitedPrompts is every prompt the pending task waits on a result for, oldest first
// (Machine.outstanding).
type awaitedPrompts []awaitedPrompt

// ids is the delivery id of each prompt, oldest first.
func (p awaitedPrompts) ids() []string {
	ids := make([]string, len(p))
	for i, prompt := range p {
		ids[i] = prompt.id
	}
	return ids
}

// LogValue logs the prompts by their ids: what each carried of news is the delivery's to show.
func (p awaitedPrompts) LogValue() slog.Value { return slog.AnyValue(p.ids()) }

// armed is one scheduled timer: its arming's sequence and delivery, how to cancel it, and when it
// fires (due), so a later arming can tell whether it would fire sooner. TimerPodStart's is no
// timer of the clock's: it fires at the pod's gate's grant, so it has no due and nothing to
// cancel, and the start it waits for leaves the gate at the end of the decision that drops its
// wake, for what that decision left of the claim (askPod, endPodPark).
type armed struct {
	seq        uint64
	deliveryID string
	cancel     Cancel
	due        time.Time
}

// NewMachine builds the machine for one claim: a new one the spawn route just stored, or one the
// daemon read back at boot. A restored claim that was booting, or registered and not yet ready, is
// watched again from now, and a restored delivery that may already have been sent is sent again
// only after asking the agent — except the one a restored claim's open update round holds, which
// no daemon has sent. ctx bounds the machine's own work — its timers and its sends.
func NewMachine(ctx context.Context, deps Deps, c Claim) (*Machine, error) {
	if err := deps.check(); err != nil {
		return nil, err
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	m := &Machine{
		ctx:            ctx,
		deps:           deps,
		log:            deps.Log.With("claim", string(c.Token)),
		claim:          copyClaim(c),
		sessionFrom:    c.Generation,
		registeredPods: bootRegisteredPods(c),
		timers:         map[TimerKind]armed{},
		stale:          map[string]bool{},
	}
	m.idle = sync.NewCond(&m.mu)
	switch c.State {
	case StateLaunching, StateShimConnected:
		m.armBoot()
		m.adoptPodLoad()
	case StateRegistered:
		m.armRegistration()
	case StateReady, StateIdle:
		// A task the claim's open update round holds gets no such mark (openRoundHoldsTask).
		// askFirst is the statement that the agent may already hold the task, and of this one it
		// is false rather than merely needless: sendPending held it back from the moment the
		// worker answered the round, and holds it while the round is open, so no daemon ever sent
		// it. Marked, the first turn of any kind — an Envoy notice reaching the worker, a
		// person's steer — would be taken as this task's turn (turnStarted reads askFirst as a
		// delivery in flight), its run marked served, and the turn's end would retire it: a task
		// destroyed without ever reaching the worker. It stays pending and unconfirmed instead,
		// and goes when the hold lifts, as it would have had this daemon never restarted.
		m.askFirst = c.Pending != nil && c.Pending.ConfirmedAt.IsZero() && !c.openRoundHoldsTask()
	case StateWorking:
		m.askFirst = true
	}
	m.publish()
	return m, nil
}

// Handle is one event: fenced, then looked up in the transition table, then acted on. A request
// the claim's state does not allow is refused with a RefusedError; a fenced request is refused
// with the claim refusal it violates (claim.StaleGeneration, claim.SameAgentRefusal). Any other
// event that is stale is logged once and dropped. A suspension of a claim whose agent is in a turn
// answers ErrSuspendHeld (holdSuspension). A request the wake pacing never makes wait, reaching a
// claim a paced wake parked with no process, drops that wake before the lookup and is then
// decided against the state the gate found (dropWakeFor); an operator's retry no row of that state
// decides — queued, suspended, or launching for a try the pause held after its write or a start
// whose pod grant the pause met — runs the dropped launch itself (runDroppedLaunchForRetry); an
// operator's task the claim can take, over a launch parked above a process the claim still records,
// runs that launch first, so the row that queues the task is the row of the claim that launch
// leaves (runParkedLaunchNow).
//
// Handle returns when the decision is made, which includes the runtime call it makes: a stop
// waits out its grace, a resume waits out the previous incarnation. A caller feeding many claims
// from one source hands each machine its events on its own goroutine, so one claim's stop does
// not hold another's hello. Meanwhile Deciding names ev, whoever the caller.
//
// A suspension is the one event that acts before it takes the lock: it cancels the start in
// flight, if one is running (stopStart), and then waits for the decision that start belongs to as
// every event does. Without that, suspending a worker whose pod is coming up waited out the whole
// launch — minutes under Kubernetes, and 120 seconds was not enough for an operator's
// `legion claims suspend` to be answered at all (dispatch://INFRA-714 R10). The cancelled start
// has charged nothing, and the row this suspension then runs finds a claim launching with no
// process to stop (holdSuspendUntilReady). A project pause's stop whose pause has ended cancels
// nothing, since it stops nothing (pauseStopEnded, ErrPauseEnded): the start it would cut short
// may be the very launch the resume let out.
//
// No decision waits for another role's session load in the claim's issue pod: a launch the pod's
// start gate cannot grant at once parks on the machine as a wake that carries its podHold and its
// decision ends (askPod, wake.park), so a suspension, a retirement and every read of the claim
// find the lock free, and the row each of them runs drops that wake, and the end of its decision
// takes the start out of the gate and names that row's end: a suspension's or a retirement's
// (endPodPark, dispatch://INFRA-5297).
//
// Handle gives ev through the store's rule first; the comment that opens handle's body says why.
func (m *Machine) Handle(ctx context.Context, ev Event) error {
	return m.handle(ctx, ev, nil)
}

// TurnOutcomeAnswer is what one turn-outcome report's decision left, read under that decision's
// own lock (HandleTurnOutcome): the claim's run of refused turns (Claim.RefusedRun), and whether
// this report failed the claim, which only the report of a turn that ran no prompt of the
// daemon's, refused with a status that cannot clear, does (unpromptedTurnEnded).
type TurnOutcomeAnswer struct {
	RefusedRun int
	Failed     bool
}

// HandleTurnOutcome is Handle for one turn-outcome report, answering what that decision left. The
// answer is read before the lock is let go, so a decision of another event that runs next — a
// prompt result's 400, or a death that spends the launch budget — cannot make a clean or prompted
// report answer that it failed the claim, as a read of the claim after Handle returned could.
func (m *Machine) HandleTurnOutcome(ctx context.Context, r RequestTurnOutcome) (TurnOutcomeAnswer, error) {
	var answer TurnOutcomeAnswer
	err := m.handle(ctx, r, func() {
		answer = TurnOutcomeAnswer{RefusedRun: m.claim.RefusedRun(), Failed: m.turnOutcomeFailed}
		m.turnOutcomeFailed = false
	})
	if m.turnOutcomeDecided != nil {
		m.turnOutcomeDecided()
	}
	return answer, err
}

// handle is Handle, and calls read, when it is given, at the end of the decision with the lock
// still held, whatever the decision returned.
func (m *Machine) handle(ctx context.Context, ev Event, read func()) error {
	// An event is a far side's or an agent's words — a session id, a task, an update round's id
	// and reasons, a worker's head — and the claim keeps each text as the store keeps it, so the
	// claim in memory, the store and a restart agree. So the event is given through the store's
	// rule once, here, before anything reads it, as intake.ApplyFact gives every fact. Strings
	// returns ev itself when nothing in it changes, and leaves its bytes as they are.
	ev = storetext.Strings(ev)
	if request, suspending := ev.(RequestSuspend); suspending && !m.pauseStopEnded(request) {
		m.stopStart("a suspension of the claim is waiting for its start")
	}
	m.mu.Lock()
	defer m.unlock()
	if read != nil {
		defer read()
	}
	// A refresh post carries a credit of the gate, and this decision is the last place that credit
	// can come back if the machine does not take it: a state whose row ignores the post, a settle
	// or fence that returns first, or a post for another claim (RefreshGate, the rule of return).
	if stale, ok := ev.(StalePlugin); ok && stale.Credit != nil {
		defer m.settleUntakenCredit(stale.Credit)
	}
	m.decide(ev)
	if token := claimOf(ev); token != m.claim.Token {
		return fmt.Errorf("supervise: %T for claim %s reached the machine of %s", ev, token, m.claim.Token)
	}
	// The write a decision owes is made before anything reads or writes the claim again: settle
	// can retire a delivery, and the repair must write what the failed decision left.
	m.repair(ctx)
	// A project pause's hold whose pause has ended goes before anything of the decision reads the
	// hold, so the fence, the row, the send and every place a hold fires read only one that stands.
	m.dropEndedPauseHold()
	// Settle first, so the fence judges the task the row will find: a settle after the fence could
	// retire the very task it let the event through for.
	if err := m.settle(ctx); err != nil {
		return err
	}
	if pass, err := m.fence(ctx, ev); !pass {
		return err
	}
	if hello, ok := ev.(StreamHello); ok && hello.AgentSecrets != nil && m.claim.Locator != nil && m.deps.Secrets != nil {
		identity := *hello.AgentSecrets
		if e := m.claim.Enrollment; e != nil && e.Incarnation == m.claim.Locator.Incarnation {
			identity.PodToken = "" // already enrolled for this incarnation; the token is not needed again
		}
		m.identity = &heldIdentity{incarnation: m.claim.Locator.Incarnation, AgentSecretsIdentity: identity}
		m.enrollmentSent = false
		if m.deps.Registrar != nil {
			m.pinHelloKey(m.claim.Locator.Incarnation, identity.Thumbprint)
		}
	}
	dropped := m.dropWakeFor(ev)
	if err := m.runParkedLaunchNow(ctx, ev); err != nil {
		return err
	}
	k := key{m.claim.State, kindOf(ev)}
	r, ok := table[k]
	if !ok {
		return fmt.Errorf("supervise: no row for %s in %s", k.kind, k.state)
	}
	if r.act == nil {
		if ran, err := m.runDroppedLaunchForRetry(ctx, ev, dropped); ran {
			return err
		}
		// The refusal names the state that decided it, which is the state the gate found for a
		// claim whose wake was just dropped: the re-park moves the claim back to launching after.
		var refusal error
		if name, ok := requestName(ev); ok {
			refusal = &RefusedError{State: m.claim.State, Request: name, Reason: r.ignore}
		}
		m.reparkWake(ctx, dropped)
		return refusal
	}
	// A row ends only in a state it names, except that a held suspension may end in suspended from
	// whichever row the end comes through (holdSuspension); a claim that holds none may not.
	from, held := m.claim.State, m.held != nil
	err := errors.Join(r.act(m, ctx, ev), m.settle(ctx))
	if to := m.claim.State; to != from && !slices.Contains(r.to, to) && (!held || to != StateSuspended) {
		err = errors.Join(err, fmt.Errorf("supervise: row %q moved %s from %s to %s, outside %v",
			r.name, m.claim.Token, from, to, r.to))
	}
	m.reparkWake(ctx, dropped)
	return err
}

// ReleaseUncertainLaunch is the only path out of the persisted uncertain-launch state. The daemon
// calls it only after reconciliation proves the unrecorded predecessor absent or reaped; concurrent
// old-pane hellos move the state first, so the caller does not open a second pane. The release is
// written, queued at the generation the claim had, so the store and memory agree whatever the
// relaunch then meets: a refused admission leaves both queued. A release the store does not take
// is undone, the claim left launch_uncertain in both. A stop between the release and the launch's
// own write leaves the claim stored queued at that generation, which ReleasedLaunch tells apart.
// Because that undo puts the state back, the write goes through putClaim and leaves nothing owed:
// memory then holds launch_uncertain, which is what the store still holds, so there is no write
// left to make, and the release is made again only by this call's own caller retrying it.
func (m *Machine) ReleaseUncertainLaunch(ctx context.Context) (bool, error) {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.State != StateLaunchUncertain || m.claim.Locator != nil {
		return false, nil
	}
	m.claim.State = StateQueued
	if err := m.putClaim(ctx); err != nil {
		m.claim.State = StateLaunchUncertain
		return false, err
	}
	return true, nil
}

// PodUpdateTold marks this claim's open update round as reported to the operator's side, so the
// daemon's sweep (internal/daemon's reconcilePodUpdates) tells one round once however many ticks
// find it still open. It is no event: the sweep is what decides a round is stalled, and the mark
// is bookkeeping of that decision, as StartedBy is of the row running against the claim.
//
// It stamps nothing and reports false when the claim is no longer in round — the round ended, or
// another opened — or when this round was already told: both are decisions already taken, not
// faults, since the sweep posts over the same claims at every tick. A write the store refuses is
// returned with false, and the sweep tells the round again at the next tick rather than lose the
// notice. That is also why the write goes through putClaim and leaves nothing owed: the stamp is
// taken back here, so there is no mark to make again, and the next tick's call writes the stamp
// it makes then.
func (m *Machine) PodUpdateTold(ctx context.Context, round string) (bool, error) {
	return m.markRound(ctx, round, &m.claim.UpdateToldAt)
}

// ErrMachineBusy is PublishUpdateAsk finding the machine's lock held — a decision in flight, or
// another locked section — so it published nothing and marked nothing. The caller asks again
// later; nothing of the ask happened.
var ErrMachineBusy = errors.New("the claim's machine is busy")

// UpdateAskOutcome is what PublishUpdateAsk did with one ask of an update round.
type UpdateAskOutcome int

const (
	// UpdateAskDelivered is a publish that reached a session: the claim is marked asked.
	UpdateAskDelivered UpdateAskOutcome = iota + 1
	// UpdateAskUndelivered is a publish no live session took, the role's topic having no holder
	// (notify.ErrNoHolder): nothing is marked, so the sweep asks the claim again.
	UpdateAskUndelivered
	// UpdateAskAlreadyDelivered is an ask of a round an earlier ask of which reached a session:
	// nothing is published.
	UpdateAskAlreadyDelivered
	// UpdateAskOutOfRound is an ask of a round the claim is not in: nothing is published.
	UpdateAskOutOfRound
	// UpdateAskSuspended is an ask of a claim R1 passes over, one the daemon suspended
	// (Claim.PassedOverByRound): nothing is published and nothing is marked. The sweep asks no
	// suspended claim (internal/daemon's askRound asks a claim only once its agent is ready), so
	// the row was enqueued before the suspension.
	UpdateAskSuspended
	// UpdateAskAnswered is an ask of a round the claim has already answered (UpdateSignalledAt):
	// nothing is published and nothing is marked. The sweep reads the claims before the ask is
	// published, so a role that answers in between was read as unanswered; under the claim's own
	// lock its answer is read as it stands, and the agent is not asked what it has done.
	UpdateAskAnswered
)

// PublishUpdateAsk publishes one ask of the claim's update round through publish, and marks the
// claim asked (UpdateTaskedAt) when, and only when, that publish reached a session. publish
// answers true for a publish a session of the role took, false for a topic no live session holds
// (notify.ErrNoHolder), and an error for every other failure; it must not call into this machine,
// whose lock it runs under. The outbox's update-ask row is the one caller (internal/daemon's
// updateAsk). The sweep only enqueues the row and marks nothing (internal/daemon's askRound): an
// ask counts once it reached a session, and an ask that reached none is lost, not given, so the
// claim stays unasked and a later sweep asks it again (dispatch://INFRA-4109 R4,
// dispatch://INFRA-4131 R4).
//
// The order is what keeps an ask from being lost or given twice, and every step of it is inside
// one locked section of this machine:
//
//  1. The lock is taken without waiting. A decision in flight holds it — a Sandbox relaunch for
//     minutes while the runtime waits out the pod — and the caller runs on the outbox's
//     role-message lane of this claim's issue, one row at a time (record.LaneKey), so a caller
//     that waited would hold every role message of that issue behind that decision. Held, the
//     call answers ErrMachineBusy having published nothing, and the row's next attempt is still
//     the first publish.
//  2. The claim is read under the lock: a claim no longer in round is published nothing, and so
//     is one an earlier ask of round already reached. Two rows of one round — a row left by an
//     older daemon beside the sweep's own — therefore deliver one ask between them, whichever
//     order they run in and whichever of them finds a holder. A claim that holds an answer of
//     round (UpdateSignalledAt) is published nothing, and the call answers UpdateAskAnswered: the
//     sweep reads the claims before the row runs, so a role can answer in between, and its agent
//     is not asked what it has done. That check comes before the next one, so a claim that
//     answered and was then suspended answers UpdateAskAnswered too. A claim the daemon suspended
//     (PassedOverByRound) is published nothing either, and the call answers UpdateAskSuspended:
//     the round does not ask such a claim (dispatch://INFRA-4109 R1). The sweep can enqueue a
//     row before the suspension and the row can run after it, an idle release's included. Its
//     publish would then go to a role with no holder, and the Envoy listener raises a role wake
//     for every publish to a role with no holder (packages/envoy/cmd/listener/confirmed.go,
//     roleSend.noHolder). That wake would bring back the pod the release took.
//  3. The publish runs under the lock, so nothing else of this claim reads or writes the mark
//     between the publish and the mark. The section holds the lock for at most the owed write's
//     repair, the publish and the mark's write: each write up to two tries of the claim-write
//     deadline (write), notify's request timeout once, about 30 seconds, and longer when a
//     write's second try waits in the pool's queue (defaultClaimWrite); the claim's other
//     decisions wait that long.
//  4. The mark is set in memory and written before the section ends, and before the row can end.
//     The write runs under context.WithoutCancel, still bounded by the claim-write deadline
//     (write), so a daemon stop that cancels the lane during or just after a delivered publish
//     does not leave the mark unwritten; the outbox's own settle takes a fresh context for the
//     same reason (outboxSettleTimeout). A write the store does not take, for a reason that judged
//     no statement, is owed (persist) and the mark stays in memory, and the call answers
//     UpdateAskDelivered with the store's error: the row stays queued, its next attempt makes the
//     owed write at its head (repair), finds the mark and publishes nothing, and it answers an
//     error for as long as the write stays owed. A write the store refuses outright is not owed
//     (writeOwed): the mark stays in memory alone, the row's next attempt finds it and finishes,
//     and after a restart, which loads the claim without it, the sweep asks the role again under
//     a new row's dedupe key.
//
// What is left is a daemon that dies between a delivered publish and the write of its mark, a
// crash rather than a stop, which the write outlives. It boots with the row still queued and the
// claim unmarked, so the row publishes once more, under the row's own dedupe key
// (record.OutboxKey), which the listener answers as a repeat, forwarding nothing, to a holder it
// already forwarded that key to within its dedupe window (ten minutes, in the listener's memory).
// Past that window, or across a listener restart, the role reads the ask twice: one message more,
// never an ask lost (dispatch://INFRA-4109 R4 accepts it).
//
// It is no event, as PodUpdateTold is none: the outbox's publish is what reaches the role, and the
// mark is bookkeeping of it.
func (m *Machine) PublishUpdateAsk(ctx context.Context, round string, publish func(context.Context) (bool, error)) (UpdateAskOutcome, error) {
	if !m.mu.TryLock() {
		return 0, ErrMachineBusy
	}
	defer m.unlock()
	m.repair(ctx)
	switch {
	case m.claim.UpdateRound != round:
		return UpdateAskOutOfRound, nil
	case m.claim.UpdateTaskedAt != nil && m.owedWrite != nil:
		return UpdateAskAlreadyDelivered, fmt.Errorf("the mark of the ask of round %s that reached the role waits on a claim write the store has not taken", round)
	case m.claim.UpdateTaskedAt != nil:
		return UpdateAskAlreadyDelivered, nil
	case m.claim.UpdateSignalledAt != nil:
		return UpdateAskAnswered, nil
	case m.claim.PassedOverByRound():
		return UpdateAskSuspended, nil
	}
	delivered, err := publish(ctx)
	if err != nil {
		return 0, err
	}
	if !delivered {
		return UpdateAskUndelivered, nil
	}
	at := m.deps.Clock.Now()
	m.claim.UpdateTaskedAt = &at
	return UpdateAskDelivered, m.persist(context.WithoutCancel(ctx))
}

// markRound stamps mark, one of this claim's own update-round marks, at the clock, for round
// (PodUpdateTold). It stamps nothing and reports false when the claim is not in round or the mark
// is already set. A write the store refuses takes the stamp back and is returned with false.
func (m *Machine) markRound(ctx context.Context, round string, mark **time.Time) (bool, error) {
	m.mu.Lock()
	defer m.unlock()
	if m.claim.UpdateRound != round || *mark != nil {
		return false, nil
	}
	at := m.deps.Clock.Now()
	*mark = &at
	if err := m.putClaim(ctx); err != nil {
		*mark = nil
		return false, err
	}
	return true, nil
}

// ReleasedLaunch is whether a stored claim is an uncertain launch ReleaseUncertainLaunch released
// and no launch then recorded: queued at a generation above zero, with no process. A claim is
// created queued at generation zero, and only a release makes one queued again after a launch, so
// the daemon's next boot counts such a claim as a launch to finish.
func (c Claim) ReleasedLaunch() bool {
	return c.State == StateQueued && c.Generation > 0 && c.Locator == nil
}

// StartedBy records the outbox row of the start being run against this claim, so a stop written
// before it can be told apart from one written after. A stop is retried until the runtime takes
// it, and a retry that lands after this start would suspend the run this start began. That
// includes a held suspension (holdSuspension), which the start drops — unless it is the operator's:
// a claim an operator's suspension holds records no start and answers ErrHeldByOperator, so the
// start waits for the operator to resume the claim. A project pause's hold whose pause is the
// open pause (Deps.Paused) is kept too, and the start is recorded beside it (decision 5 of
// dispatch://INFRA-3759's plan): a start leased before the pause began must not let the role it
// starts run past the pause's stop, which the outbox's pause rule would otherwise have held back
// (record's outboxPauseFree). Its task waits as every task does while the project is paused
// (sendPending). A pause's hold whose pause has ended is gone before the start reads the hold, as
// it is at the start of every decision (dropEndedPauseHold): this is the one decision that does
// not go through Handle.
//
// It is also what releases a task held because its root had no architect to hear it (sendPending,
// Deps.RootHears): this start is the root running again, so the held task goes now, before the
// start's own work — including when that start carries no task of its own to deliver, where
// nothing else would ask.
//
// The one held task it does not send is one the start's own task supersedes (task, the request
// the start delivers once the claim is brought back, nil for a start that carries none;
// supersedes): a tree's root architect's task of an earlier run, or of a lower entry of the same
// run, that carries an entry of its own and that nothing of is in the agent's hands. Sent here,
// it would be the re-admitted architect's first prompt — a task saying what was true of a run
// that is over (dispatch://INFRA-2614 R3) — and the start's own task, the one the transition that
// raised the newer entry wrote (R4), would wait behind its turn. The start's delivery retires it
// unsent instead (queue), which releases the decisions it names for the catch-up that follows to
// tell. A start of no entry supersedes nothing, and a held task of no entry, a wake's
// (workflow's wakeRow) among them, is superseded by nothing, so either way a held task is sent
// at the start as before.
func (m *Machine) StartedBy(ctx context.Context, row int64, task *RequestDeliver) error {
	m.mu.Lock()
	defer m.unlock()
	m.dropEndedPauseHold()
	if m.current().RuleHolds() {
		return fmt.Errorf("start row %d of %s: %w", row, m.claim.Token, m.current().RuleHoldError())
	}
	if row <= m.claim.LastStartRow {
		return m.releaseHeld(ctx, row, task)
	}
	m.claim.LastStartRow = row
	switch {
	case m.held == nil:
	case m.held.Pause != 0:
		m.log.Info("supervise: a start keeps the held suspension of the open pause", "row", row,
			"pause", m.held.Pause, "reason", m.held.Reason)
	default:
		m.log.Info("supervise: a start drops the held suspension", "row", row, "reason", m.held.Reason)
		m.dropHeld()
	}
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.releaseHeld(ctx, row, task)
}

// releaseHeld is StartedBy's send of the pending task, which it skips for a task the start's own
// task supersedes (supersedes, the one rule queue applies when that task arrives).
func (m *Machine) releaseHeld(ctx context.Context, row int64, task *RequestDeliver) error {
	if p := m.claim.Pending; p != nil && task != nil && m.supersedes(*p, *task) {
		m.log.Info("supervise: the start's own task supersedes the held task, so the held task is not sent; the start's delivery retires it",
			"row", row, "held", p.ID, "heldGeneration", p.Generation, "heldEntry", p.Entry,
			"generation", task.Generation, "entry", task.Entry)
		return nil
	}
	return m.sendPending(ctx)
}

// EntryGiven records that this claim was given the work of phase entry entry (Claim.GivenEntry)
// where no delivery of that entry records it. Its one caller is the outbox, on the branch that
// holds a start's task back because the claim already holds its phase's task (internal/daemon's
// outbox): the task the claim holds is that entry's work, so the entry was given, and the
// difference the admission pass reads between the entry a role's phases row names
// (record.PhaseRow.Entry) and the entry its claim was given has to close here rather than
// through a second task of the same entry.
//
// The handshake is written with the number (Claim.Handshake), from the task the claim holds where
// that task carries entry: its queued time is when that entry's work was first queued, and its
// confirmed time, where a turn has confirmed it, when the worker took it. A claim holding no task
// of entry has no handshake of it to record and keeps the one it holds.
//
// A claim already at or above entry is left alone and written nothing: the number only rises, so
// a row run again after the claim was given a later entry's work cannot take it back.
//
// The raised number stands in memory whether or not the store takes the write, as the row
// StartedBy records does: the claim holds that entry's task either way, and a write the store
// refused is owed and made again at the claim's next decision (persist, owes).
func (m *Machine) EntryGiven(ctx context.Context, entry int64) error {
	m.mu.Lock()
	defer m.unlock()
	if entry <= m.claim.GivenEntry {
		return nil
	}
	m.claim.GivenEntry = entry
	if p := m.claim.Pending; p != nil && p.Entry == entry {
		m.claim.Handshake = m.claim.Handshake.queued(entry, p.Generation, p.QueuedAt).confirmed(*p)
	}
	return m.persist(ctx)
}

// ServingRun is the issue generation of the run whose task this claim's worker took, and 0 for a
// claim that has taken none. It is the first answer to which run a completion belongs to, and the
// only one outside the completion route's own fallback to a held task (api/handoff.go): a
// completion names no run, the pane cannot say it (LEGION_GENERATION is the claim's launch
// counter, and a live worker is handed the next run's task without being relaunched), and the
// three sources serving reads are read for it.
//
// An operator's task belongs to no run and answers for none of them: it carries generation 0.
// The older work an unread task's run could be mistaken for is a run this claim was left
// serving, and nothing else (serving's rule 3).
func (c Claim) ServingRun() uint64 {
	return serving(c.Pending, deliveredRun, c.ServingGeneration, c.ServingGeneration != 0, c.openRoundHoldsTask())
}

// ServingEntry is the phase entry of the task this claim's worker took, and 0 for a claim that
// has taken none that carried one. It is the finer of the two: every phase entry of one run
// carries that run, where each entry that hands this role a task carries a number of its own.
// A task of no entry answers for none of them: an operator's own, every start of no entry
// (record.SuperviseRequest.Entry, which lists them), and every task a daemon older than the
// entry delivered — so a worker resumed by one of those is never refused an in-flight
// completion for an entry it was never given. The older work an unread task's entry could be
// mistaken for is an entry this claim was left serving, and nothing else: a worker whose last
// task carried none — one of those starts, a pre-entry daemon's delivery — is on its first
// entry still, and the fallback is what lets it complete.
func (c Claim) ServingEntry() int64 {
	return serving(c.Pending, deliveredEntry, c.ServedEntry, c.ServedEntry != 0, c.openRoundHoldsTask())
}

// ServingCheck is the production check look of the task this claim's worker took, and the zero
// CheckLook for a claim that has taken none that carried one. It is what says which kind of
// completion a turn reports: a worker whose live assignment is a check look reports on that look
// (api/handoff.go, checkReport), and every other worker reports a phase completion. The look is
// the one the task was given, not whichever look the check has reached, so a report of a look the
// check has left is held to it and refused by the engine (CHECK_LOOK_NOT_OPEN).
//
// A task of a phase is the one task that takes a look back rather than leaving it: a worker
// handed phase work again — a root reopened inside its linger, whose check still stands — is back
// in the workflow, and the completion of that phase is no report on production. Every other task
// of no look (an operator's own, a resume) leaves the look the claim is serving alone, as it
// leaves the run and the entry.
//
// The look is the one field whose fallback (serving's rule 3) is held to more than its own
// retired value: a look an unread task carries is taken only by a claim that has served nothing
// at all. A claim records no look until a look's task is given to it, so a tester in the middle
// of its testing run reads the zero look while its run and its entry answer the testing
// task; held to the look alone, the fallback would hand that tester a look it has not begun to
// serve, and its testing verdict — pass or fail, which a check report also names — would close
// the check with nobody having looked at production. The look, the run and the entry are
// one serving, not three. A look whose own turn starts after the daemon's wait for it still
// reports: the prompt Oh My Pi acknowledged stays outstanding until its result, and the pinned
// Oh My Pi runs one admitted prompt at a time, so the session's next turn is that prompt's and
// confirms the look's task (Machine.outstanding, turnStarted). The cost left is a look turn no
// acknowledged prompt of the task is waiting on, such as one that starts after Oh My Pi answered
// the look's prompt without a turn: that turn counts as the agent's own, its report is refused,
// and the task is sent again once the turn ends, so the next turn confirms it and reports. The
// look's own bound (workflow.CheckLookBound, two hours) is the backstop under that, and a
// refusal the agent sees beats a silent false pass.
func (c Claim) ServingCheck() CheckLook {
	retired := c.ServingCheckLook
	if c.Pending != nil && c.Pending.Phase != "" {
		retired = CheckLook{}
	}
	return serving(c.Pending, deliveredCheckLook, retired, c.servedAnything(), c.openRoundHoldsTask())
}

// servedAnything is whether this claim's worker has taken any task at all — a run, an entry
// or a look — which is what older work means for the look alone (ServingCheck). Only that
// accessor reads it: the retire writes what a confirmed delivery leaves behind, and a confirmed
// delivery is taken by serving's first rule, before any question of older work.
func (c Claim) servedAnything() bool {
	return c.ServingGeneration != 0 || c.ServedEntry != 0 || c.ServingCheckLook != (CheckLook{})
}

// deliveredRun, deliveredEntry and deliveredCheckLook are what a delivery carries of each:
// the three values serving picks between, named so the three accessors read as one order of
// sources with a guard of their own rather than three rules.
func deliveredRun(d Delivery) uint64          { return d.Generation }
func deliveredEntry(d Delivery) int64         { return d.Entry }
func deliveredCheckLook(d Delivery) CheckLook { return d.CheckLook }

// serving picks what a claim's worker is on — the run for ServingRun, the phase entry for
// ServingEntry, the production check look for ServingCheck — from the three sources all
// three read, in this order. carried is what the pending delivery holds of it, and retired what
// the claim was left serving (ServingGeneration, ServedEntry, ServingCheckLook); the zero
// value of any of them is a task, or a claim, that answers for none of them. The order of the
// three sources lives here once, and each caller supplies what counts as older work for the
// field it is asking about (servedOlder, below).
//
//  1. The task whose turn is running. A completion is a tool call inside a turn, and one process
//     runs one turn at a time.
//
//  2. Otherwise what this claim is left serving: the turn's end retires its delivery, and the
//     worker goes on working that run, that entry and that look — a tester waiting on CI, a
//     merger waiting on a person, a checking agent between a look's start and its report — until
//     another delivery carrying one replaces it. A notice's turn has no delivery behind it.
//
//  3. Otherwise, for a claim that has served no older work this value could be mistaken for, the
//     task it holds, unless a prompt of that task has already come to nothing (Delivery.Prompted)
//     or an open update round is holding it back from the worker (held, Claim.openRoundHoldsTask).
//     Oh My Pi's own agent_start names no prompt, so a turn that starts with no prompt of the task
//     in flight or outstanding (Machine.outstanding) is indistinguishable from a turn of the
//     agent's own and confirms nothing; and a turn an Envoy message starts can run the whole
//     phase before the phase's task is sent at all, which is what INFRA-264's tester did — it
//     registered on a fresh claim, a message to its role topic started its first turn six
//     seconds later, and its completion was refused as belonging to no run. A worker on its
//     first task would otherwise have its completion refused. A task the daemon has not sent, or
//     one the agent acknowledged, is the only work such a claim can be on, and cannot be mistaken
//     for older work, because there is none this claim served. A task the agent refused is not
//     one it read, and carries no such mark, so the claim answers for nothing until one is. The
//     completion retires the task it is credited to, through the phase it leaves behind
//     (sendPending), so that task is never sent after the work it names is done.
//
//     held is sendPending's own hold, read through the one predicate that spells it
//     (openRoundHoldsTask): a worker that has answered its update round is given no task until the
//     round ends, so a task waiting behind that hold is work the worker has not been handed and
//     answers for nothing it does in the meantime. The hold reaches this rule alone. A task the
//     daemon had already sent when the round opened stays what the claim is on, since a round that
//     opens over a worker mid-turn takes nothing back from it.
//
// What counts as older work is servedOlder, which each caller answers for what it is guarding
// against, because the three fields are not zero on the same claims: for the run and the
// entry it is that field's own retired value, since a claim left serving neither has taken
// no task that carried one; for the look it is anything the claim has served at all
// (Claim.servedAnything), since a claim in the middle of a phase run records no look, and a look
// held to its own retired value alone would be handed to that worker while its run and its
// entry answer the phase task.
func serving[T comparable](pending *Delivery, carried func(Delivery) T, retired T, servedOlder, held bool) T {
	var none T
	if pending == nil {
		return retired
	}
	value := carried(*pending)
	if value == none {
		return retired
	}
	if !pending.ConfirmedAt.IsZero() {
		return value
	}
	if !servedOlder && (!pending.DeliveredAt.IsZero() || (!pending.Prompted && !held)) {
		return value
	}
	return retired
}

// Claim is a copy of the claim as the machine holds it now. It waits for the decision in flight,
// so what it reports is what that decision left: a check a decision must not race reads it here.
func (m *Machine) Claim() Claim {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current()
}

// View is a copy of the claim as the machine last published it: as its latest decision left it, or
// as the decision in flight last tried to write it to the store. It never waits, where Claim waits
// out a decision that holds the machine through its runtime call — a Sandbox relaunch waits minutes
// for its pods — so a reader of every claim, like the operator's listing, reads View.
func (m *Machine) View() ClaimView {
	v := *m.view.Load()
	v.Claim = copyClaim(v.Claim)
	return v
}

// Published is the claim as the machine last published it, as View reads it, shared rather than
// copied: publish stores a view built fresh and nothing writes a stored one again, so a reader
// that only reads the claim may share its slices and maps, as WaitingWake does. A reader that keeps
// the claim past its own read, or writes it, takes View. It never waits on the decision in flight.
// The daemon's refresh pass reads it for each claim it would post (internal/daemon's
// refreshStalePlugins), at a sweep of every claim, where a copy of each would be garbage.
func (m *Machine) Published() Claim {
	return m.view.Load().Claim
}

// WaitingWake is the wake the pacing holds over this claim, nil while none waits: what the
// published view names (ClaimView.WakeWaiting) with the claim's token, issue and role beside it,
// for the reader of every claim the state route is (the daemon's waitingWakes,
// api.WakesWaitingOf). It never waits on the decision in flight, as View does not, and it copies
// no claim: publish stores a view built fresh and mutates no stored one, so every field read here
// is one nothing writes again. A machine holding no wake allocates nothing, which is every machine
// outside a wave.
func (m *Machine) WaitingWake() *WaitingWake {
	v := m.view.Load()
	if v.WakeWaiting == nil {
		return nil
	}
	return &WaitingWake{Claim: v.Token, Issue: v.Issue, Role: v.Role, Kind: v.WakeWaiting.Kind, At: v.WakeWaiting.At}
}

// AwaitsPod is the live pod the claim's launch waits on as the machine last published it
// (Claim.AwaitsPod), nil for none. It never waits on the decision in flight, as View does not, and
// it copies the locator alone rather than the whole claim, since the daemon's pod-update sweep
// reads it of every claim it groups that records no process (internal/daemon's
// reconcilePodUpdates): publish stores a view built fresh and mutates no stored one.
func (m *Machine) AwaitsPod() *runtime.Locator {
	return cloneLocator(m.view.Load().AwaitsPod)
}

// RegisteredPods is the set of pods an agent of the claim registered a session in
// (Claim.RegisteredPods), as the machine last published it. The daemon's pod-update sweep reads it
// of every claim it groups (internal/daemon's reconcilePodUpdates), as it reads AwaitsPod, and for
// the same reasons it never waits on the decision in flight and copies the set alone.
func (m *Machine) RegisteredPods() RegisteredPods {
	return m.view.Load().RegisteredPods.clone()
}

// Deciding is the event the decision in flight handles (Handle), or nil when the machine decides
// nothing. It never waits on the decision.
func (m *Machine) Deciding() Event {
	m.decidingMu.Lock()
	defer m.decidingMu.Unlock()
	return m.deciding
}

// TurnsEnded is how many turns this machine has recorded the end of (turnEnded), whichever event
// ended them: the agent's own agent_end, or its answer to the sweep's question that it is no
// longer streaming (askTurnOver). The daemon reads it on both sides of a decision to tell whether
// that decision ended a turn, and posts the end to the issue's other claims (its wakeIssue,
// IssueTurnEnded): a phase task one of them holds for this turn may go now. It never waits on the
// decision in flight, and a count read after Handle returns is one the claim that decision
// published already carries, since the count moves inside the decision and the publish is that
// decision's last act (unlock).
func (m *Machine) TurnsEnded() uint64 { return m.turnsEnded.Load() }

// decide publishes ev as the event the decision now beginning handles (Deciding), and unlock
// clears it. The caller holds mu.
func (m *Machine) decide(ev Event) {
	m.decidingMu.Lock()
	defer m.decidingMu.Unlock()
	m.deciding = ev
}

// current is a copy of the claim as the machine holds it now, with what only memory holds. The
// caller holds mu. Each field it sets here is one StoredClaim clears, below.
func (m *Machine) current() Claim {
	c := copyClaim(m.claim)
	if m.held != nil {
		c.SuspensionHeld, c.SuspensionHeldBy = true, m.held.By
	}
	c.Compacting = m.compacting != nil
	c.ProcessGone = m.processGone
	c.AwaitsPod = cloneLocator(m.awaitsPod)
	c.RegisteredPods = m.registeredPods.clone()
	return c
}

// StoredClaim is c in the form a claim write hands the store. A pending delivery has a table of
// its own, and the store holds none of the fields current overlays on the claim the machine
// reports: a suspension held for its turn (SuspensionHeld, SuspensionHeldBy), a compaction in
// flight (Compacting), a process the runtime reported gone (ProcessGone), the pod a launch waits
// on (AwaitsPod) and the pods an agent of the claim registered in (RegisteredPods). Only the
// machine holds them, in memory, and internal/store writes none of them, so the one statement
// below clears every one, and no stored claim, nor a claim compared with one, carries any of
// them. A field that current comes to overlay belongs in that statement too.
//
// No caller needs one of them on the claim it hands the store: the machine's own writes hand it
// the claim the machine holds, which carries none of them (stored); the boot's repair hands it a
// claim the store read back (internal/daemon's supervisor.restore); and a project pause's stop
// hands it the claim one of those writes built (internal/daemon's projectPause.stopTx). Call
// NormalizeClaim before this when c will also construct a machine.
func StoredClaim(c Claim) Claim {
	c.Pending, c.SuspensionHeld, c.SuspensionHeldBy, c.Compacting, c.ProcessGone, c.AwaitsPod, c.RegisteredPods =
		nil, false, "", false, false, nil, RegisteredPods{}
	return c
}

// publish makes the claim as the machine holds it now the one View reports, with the wake the
// pacing holds over it, if any: a fresh value each time, since the view is read without mu. A
// launch the project pause's gate holds is no such wake: it has no moment to run and waits in no
// pacing (wake.pause), so it is published as none. Nor is a launch waiting for its issue pod's
// start (wake.park): it has no moment either, and the daemon's state does not show that wait
// (dispatch://INFRA-5297's decision; the start and load-end lines are its record). The caller
// holds mu.
func (m *Machine) publish() {
	view := &ClaimView{Claim: m.current(), Stopping: m.stopping, SendInFlight: m.send != nil}
	if m.wake != nil && m.wake.pause == 0 && m.wake.park == nil {
		view.WakeWaiting = &WakeWaiting{Kind: m.wake.kind, At: m.wake.at}
	}
	m.view.Store(view)
}

// stoppingProcess marks the claim's process as being stopped for good, for View, until the runtime
// call that stops it returns; the end of the decision publishes what it left. The caller holds mu.
func (m *Machine) stoppingProcess() func() {
	m.stopping = true
	m.publish()
	return func() { m.stopping = false }
}

// unlock ends a locked section that may have changed the claim: it holds the stale-plugin lane to
// its invariants — the credit this machine holds goes back unless a refresh wake of its own is
// waiting, and nothing of a relaunch's intent outlives the decision that raised it
// (endRefreshLane) — takes the start of a launch parked at its issue pod's gate out of the gate
// where the decision took that launch's wake off the machine (endPodPark), settles the session
// load of a process the decision left registered, or ends the load of one it left no longer
// booting, which gives its issue pod's start to the next role (endPodLoad), publishes the claim,
// says the machine decides nothing, then releases mu.
//
// Every decision of a machine ends here, which is what makes it the place those four are kept:
// every locked section that can change the wake, the credit, the intent or the claim's process
// ends in unlock, and a section that takes mu without it reads the claim or counts goroutines and
// changes none of them. The sections that defer it are Handle, which every event and every timer
// of this package comes through, ReleaseUncertainLaunch, markRound (PodUpdateTold),
// PublishUpdateAsk, StartedBy, ReadProcess, Retree and the refresh gate's reclaimCredit; the
// sections that take mu without it are Claim, OnDurableState, Wait, compaction and the two
// goroutine tails in delivery.go and secrets.go. It is that rule a section added later has to
// keep, rather than this list of the sections there are now.
func (m *Machine) unlock() {
	m.endRefreshLane()
	m.endPodPark()
	m.endPodLoad()
	m.publish()
	m.decide(nil)
	m.mu.Unlock()
}

// OnDurableState installs the daemon callback for the durable transitions it acts on: ready,
// working and failed. Two of them are terminal for the workflow and one, working, is not: the
// daemon reads a turn starting to stamp a woken pod's first turn. The callback runs after the
// transition's claim write has succeeded, and a machine keeps one, so a daemon that observes
// more than one of them observes them in that one callback.
func (m *Machine) OnDurableState(callback func(Claim, ClaimState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onDurableState = callback
}

// report hands the daemon one durable transition, with the claim copy that transition is of: the
// copy of the claim as the decision reporting now holds it (durableWrite), or the copy an owed
// report kept from the decision that owed it (makeOwedReports). Both go through here so that
// what a report is — a state and the claim it is true of — is spelled in one place, and so is
// the daemon that may have installed no callback at all.
func (m *Machine) report(c Claim, state ClaimState) {
	if m.onDurableState != nil {
		m.onDurableState(c, state)
	}
}

// durableWrite makes a decision's own write and reports the durable transition it wrote, in that
// one order: the report follows the write, and a write the store did not take reports nothing
// and owes the report instead, for whatever claim write lands next to make (oweDurableState,
// settleOwedWrite). Every decision that reports a durable transition goes through here — the
// turn that starts, the claim that is ready, the claim that failed — because the order is the
// point and the owing works only on the error of the write it belongs to: a fourth caller that
// wrote the three steps out itself and left the owing off would lose its report in silence,
// which is the fault the mark exists to end.
//
// write is the decision's own claim write: persist for most of them, and confirmCarrying for a
// turn start that confirms the delivery whose turn it is.
func (m *Machine) durableWrite(ctx context.Context, state ClaimState, write func(context.Context) error) error {
	if err := write(ctx); err != nil {
		m.oweDurableState(state)
		return err
	}
	m.report(copyClaim(m.claim), state)
	return nil
}

// turnStartedLine is the message a turn's start is logged under, at Info, on each of the four
// paths that put a claim in working. It is pinned as a constant because an operator and a
// production check read it whole: with turnEndedLine it is how what the daemon did between a
// turn's start and its end is read out of the log at all, and a message reworded or extended at
// a call site would answer that search with a part of the answer. What differs between the call
// sites belongs in the attributes (logTurn), never in the message.
const turnStartedLine = "supervise: a turn started"

// turnEndedLine is the close of that pair, at Info: the turn whose start the line above recorded
// is over. It is pinned for the same reason, and is not to be reworded or extended at its call
// site either — an operator counts the two against each other.
const turnEndedLine = "supervise: a turn ended"

// logTurn writes a turn line at Info — one of the two pinned above, or the start of a turn a busy
// answer recorded (busyTurnBegun) — with the claim's generation and, where the turn is a task's,
// that delivery. The claim is already an attribute of the machine's own logger, so no line
// repeats it, and a turn that is no task's carries no delivery key at all rather than an empty
// one.
func (m *Machine) logTurn(line, delivery string) {
	if delivery == "" {
		m.log.Info(line, "generation", m.claim.Generation)
		return
	}
	m.log.Info(line, "generation", m.claim.Generation, "delivery", delivery)
}

// startTurn records what this turn is the turn of and writes its start line, which is how the
// turn's end comes to name what its start named (Machine.turnsDelivery, endTurn), and dates the
// turn (Claim.LastTurnStart, whose doc names every path that starts one). Each of those paths
// goes through here, the three that name no delivery included, so a turn that is no task's
// overwrites the identity of the turn before it rather than leaving it standing for its end to
// read. The agent_start of a turn a busy answer recorded goes through busyTurnBegun instead:
// recordBusyTurn already wrote that turn's start line, and the start names the task it confirms
// in a line of its own, so one start line and one end line still bracket the turn.
//
// The identity is carried from the start because no reading of the claim can answer it at the
// end: a pending delivery that is confirmed is the running turn's in one case and a finished
// turn's leftover in the other. The second is the window turnEnded's own resultPending branch
// leaves — a turn of the task's ends before Oh My Pi says which turn the prompt's was, so settle
// retires nothing and the task stays confirmed in the worker's hands — and a turn of the agent's
// own starting in that window is no task's, so its start line names no delivery while a claim
// read at its end would name that task and a reader would count the task's turn twice.
//
// A turn under a compaction writes its end line at its end (turnEnded).
func (m *Machine) startTurn(delivery string) {
	m.claim.LastTurnStart = m.deps.Clock.Now()
	m.turnsDelivery = delivery
	m.logTurn(turnStartedLine, delivery)
}

// endTurn writes the turn's end line, naming what its own start line named, and forgets the turn
// with it, so nothing of a turn that is over is left for the next turn's end to read. A turn this
// daemon never saw begin — a restart mid-turn, or a turn a busy answer recorded whose own start
// was never read — holds no identity here, and its end line names no delivery rather than the
// claim's pending task, which is the safe answer for a turn nobody can say the task of.
func (m *Machine) endTurn() {
	m.logTurn(turnEndedLine, m.turnsDelivery)
	m.turnsDelivery = ""
}

// Wait blocks until every send the machine started has had its outcome handled — what a daemon
// waits for before closing the store the outcome is written to.
func (m *Machine) Wait() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for m.goroutines > 0 {
		m.idle.Wait()
	}
}

// fence applies the checks an event carries the fields for: the incarnation an observation is
// about, the generation of a hello or an answer or a prompt outcome or a request, the session a
// request comes from, the delivery a turn or a refusal or a timer names, and the arming a timer
// came from. It reports whether the event goes on to the table.
//
// A hello and a registration are the two events that move a launch on (helloed, register), and a
// live claim past launching records its process. So neither reaches the table while the claim is
// a launch that records none (launchRecordsNoProcess): a hello is dropped, and a registration is
// refused. The process that sent it is not the claim's: its launch was cut short by the daemon's
// stop, which the next boot launches again once its orphan reconciliation has reaped every process
// no claim records, or it is the process a relaunch waiting at the probe interval let go
// (relaunchLater) or a wake waiting its turn in the pacing let go (reserveWake), which that
// relaunch and that wake each wait out.
func (m *Machine) fence(ctx context.Context, ev Event) (bool, error) {
	pending := m.claim.Pending
	switch ev := ev.(type) {
	case RuntimeObservation:
		held := ""
		if m.claim.Locator != nil {
			held = m.claim.Locator.Incarnation
		}
		if got := ev.Observation.Locator.Incarnation; held == "" || got != held {
			m.dropStale("RuntimeObservation", "incarnation", got, held)
			return false, nil
		}
	case StreamHello:
		if ev.Generation != m.claim.Generation {
			m.dropStale("StreamHello", "generation", fmt.Sprint(ev.Generation), fmt.Sprint(m.claim.Generation))
			return false, nil
		}
		if m.launchRecordsNoProcess() {
			m.dropStale("StreamHello", "process", fmt.Sprint(ev.Generation), "none")
			return false, nil
		}
	case StreamAnswered:
		// A registration reaches the machine on its HTTP request, and the operator's suspend and
		// resume on theirs, not behind the stream's queue. So an answer of a launch whose session
		// the claim has left can arrive after a fresh session registered, and would name a model
		// that never served that session; and an answer of an earlier launch of the claim's own
		// session can arrive after a resume moved the generation on, and is that session's answer
		// all the same (sessionFrom).
		if ev.Generation < m.sessionFrom || ev.Generation > m.claim.Generation {
			m.dropStale("StreamAnswered", "generation", fmt.Sprint(ev.Generation),
				fmt.Sprintf("%d..%d", m.sessionFrom, m.claim.Generation))
			return false, nil
		}
	case StreamTurnStart:
		if ev.DeliveryID != "" && (pending == nil || pending.ID != ev.DeliveryID) {
			m.dropStale("StreamTurnStart", "delivery", ev.DeliveryID, pendingID(pending))
			return false, nil
		}
	case StreamPromptResult:
		// The result of a prompt for a delivery the claim no longer holds — retired, or taken back
		// under a new id — answers a question nothing is still asking. A prompt of the pending task
		// still outstanding is the one exception: the wait for its turn took the task back under a
		// new id, or a later send's busy answer did, and the turn the prompt ran is the task's all
		// the same, since a session's next turn after an admission is that prompt's
		// (Machine.outstanding).
		if pending == nil || (pending.ID != ev.DeliveryID && !m.outstandingPrompt(ev.DeliveryID)) {
			m.dropStale("StreamPromptResult", "delivery", ev.DeliveryID, pendingID(pending))
			return false, nil
		}
	case StreamLateRefusal:
		// A refusal naming the prompt that set the mark always reaches the table. One naming the
		// pending id, or a prompt of the pending task still outstanding, does when its connection
		// sent that prompt, or when it takes back a task a turn not its own confirmed
		// (takesBackConfirmed): a replayed refusal answers an earlier send, and the pending id may be
		// the one its connection re-sent.
		named := pending != nil && pending.MarkedBy == ev.DeliveryID
		pendingNamed := pending != nil && (pending.ID == ev.DeliveryID || m.outstandingPrompt(ev.DeliveryID)) && !ev.Replayed
		if pending == nil || !(named || pendingNamed || m.takesBackConfirmed(ev)) {
			m.dropStale("StreamLateRefusal", "delivery", ev.DeliveryID, pendingID(pending))
			return false, nil
		}
	case PromptAcked:
		return m.fenceOutcome(ctx, "PromptAcked", ev.Generation, ev.DeliveryID)
	case PromptRefused:
		return m.fenceOutcome(ctx, "PromptRefused", ev.Generation, ev.DeliveryID)
	case Timer:
		a, ok := m.timers[ev.Kind]
		if !ok || a.seq != ev.Seq || ev.Generation != m.claim.Generation || a.deliveryID != ev.DeliveryID ||
			(timesADelivery(ev.Kind) && pendingID(pending) != ev.DeliveryID) {
			m.dropStale("Timer", "timer", fmt.Sprintf("%s#%d", ev.Kind, ev.Seq), fmt.Sprintf("%s#%d", ev.Kind, a.seq))
			return false, nil
		}
		delete(m.timers, ev.Kind)
	case RequestRegister:
		// A registration as another session than the claim records is refused (fenceRequest),
		// and stays refused: its process is not the claim's agent. Where that process is one an
		// earlier release started for a claim that records a session and no file, the refusal
		// also marks the claim so the process's end is charged nothing (markRollEdge).
		if pass, err := m.fenceRequest("register", ev.Generation, ev.Session); !pass {
			if errors.Is(err, claim.SameAgentRefusal) {
				m.markRollEdge(ev)
			}
			return false, err
		}
		if m.launchRecordsNoProcess() {
			m.log.Warn("supervise: refused a registration: the launch at its generation records no process",
				"generation", ev.Generation, "session", ev.Session)
			return false, &RefusedError{State: m.claim.State, Request: "register", Reason: "the launch records no process"}
		}
		return true, nil
	case RequestReady:
		return m.fenceRequest("ready", ev.Generation, ev.Session)
	case RequestExit:
		return m.fenceRequest("exit", ev.Generation, ev.Session)
	case PodUpdateSignalled:
		// The worker's answer ends a round, and a round ending deletes a disk: a process of a
		// superseded generation, or of another session, must never be what says the work is
		// pushed.
		return m.fenceRequest("ready_for_update", ev.Generation, ev.Session)
	case CompactEnded:
		// The end of a compaction the claim no longer owns decides nothing, and it is answered
		// rather than dropped: Machine.Compact reads what became of the compaction from this one
		// answer, which is the only one made under the lock the claim moves behind.
		if !m.ownsCompaction(ev.Generation) {
			m.log.Warn("supervise: the compaction's end reached a claim that no longer holds it; the claim was not given back and sent nothing",
				"generation", ev.Generation, "claimGeneration", m.claim.Generation, "state", m.claim.State)
			return false, ErrCompactProcessGone
		}
	}
	return true, nil
}

// timesADelivery is whether a timer of kind waits on one delivery rather than on the claim or
// its process: the wait for an acknowledged prompt's turn, and the ladder's wait a provider
// error arms (providererror.go). Each is fenced on the claim's pending delivery as well as on
// the id the arming recorded, so one that fires after the task it was armed for was replaced
// decides nothing about the task that replaced it.
func timesADelivery(kind TimerKind) bool {
	switch kind {
	case TimerTurn, TimerPromptRetry:
		return true
	default:
		return false
	}
}

// fenceOutcome is the fence on a prompt's outcome. The send it belongs to is over either way; an
// outcome for a delivery or a generation the claim has moved past records nothing — and when it
// was the machine's own send that finished late, the delivery that replaced it may go now, if the
// claim is one sendPending sends to. The acknowledgement of a send whose busy answer arrived first
// is one of those: that answer took the task back under a new id (takeBackPending), so the
// acknowledgement adds no prompt to the ones the task waits on a result for (Machine.outstanding),
// and the same answer recorded the claim working (recordBusyTurn), so nothing goes from here into
// the turn it named: the task waits for that turn's end.
func (m *Machine) fenceOutcome(ctx context.Context, event string, generation uint64, deliveryID string) (bool, error) {
	own := m.send != nil && m.send.id == deliveryID && m.send.generation == generation
	if own {
		m.send = nil
	}
	if generation == m.claim.Generation && pendingID(m.claim.Pending) == deliveryID {
		return true, nil
	}
	if own {
		m.helloDuringSend = false
		return false, m.sendPending(ctx)
	}
	if generation != m.claim.Generation {
		m.dropStale(event, "generation", fmt.Sprint(generation), fmt.Sprint(m.claim.Generation))
	} else {
		m.dropStale(event, "delivery", deliveryID, pendingID(m.claim.Pending))
	}
	return false, nil
}

// fenceRequest is the fence on an agent's own request: it must come from the claim's current
// generation, and — once the claim has recorded a session — from that session (HoldsSession,
// claim.SameAgentRefusal, which the API answers 409). That holds for a claim that records a
// session and no file as well: every launch of it resumes the session by its id, or forgets the
// session before it spawns a fresh agent (start), so a process that registers as another session
// is never that session's agent. A refused registration's plugin exits on the 409, and the end of
// its process is charged as the end of any process that never registered is (died,
// registrationDeadline), except the two ends that are charged nothing (endUncharged).
func (m *Machine) fenceRequest(request string, generation uint64, session string) (bool, error) {
	if generation != m.claim.Generation {
		return false, fmt.Errorf("%s for %s at generation %d, the claim is at %d: %w",
			request, m.claim.Token, generation, m.claim.Generation, claim.StaleGeneration)
	}
	if m.claim.Session != "" && !m.claim.HoldsSession(session) {
		return false, fmt.Errorf("%s for %s as session %q, the claim resumes %q: %w",
			request, m.claim.Token, session, m.claim.Session, claim.SameAgentRefusal)
	}
	return true, nil
}

// markRollEdge is the roll edge of dispatch://INFRA-4108 R1, for a registration the fence refused
// as another session than the claim records (claim.SameAgentRefusal). Where the claim records a
// session and no file, and the registration's plugin speaks an older contract than the daemon's,
// the registering process is one an earlier release started: this release raised the contract,
// and starts no fresh agent for a claim that still records its session. At the deploy such a
// claim can run a fresh agent the earlier release spawned and carry the launch failures its loop
// charged, so the refusal marks the claim's generation (refusedEarlierRelease) and the end of
// that process is charged nothing (endUncharged); the 409 stands. A refusal at the generation of a
// start that had Oh My Pi resume the session by its id (Claim.AgentResumeGeneration) is no roll
// edge: that process is this release's own start, and its end is the uncharged end of that start.
// Every other refusal is marked nothing and keeps its charge: a process of this release adopted at
// a restart registers at the daemon's own contract, and so does this release's resume by the id
// through the pod's launcher; a daemon that knows no contract of its own (Deps.PluginContract 0)
// marks nothing. The mark writes the one line a production check reads for the roll edge, once
// for the generation: that check reads it because no launched line of this daemon run comes
// before such a refusal.
func (m *Machine) markRollEdge(ev RequestRegister) {
	if m.claim.SessionFile != "" || m.deps.PluginContract <= 0 || ev.PluginContract >= m.deps.PluginContract ||
		m.claim.Generation == m.claim.AgentResumeGeneration || m.refusedEarlierRelease == m.claim.Generation {
		return
	}
	m.refusedEarlierRelease = m.claim.Generation
	m.log.Warn("supervise: refused the registration of a process an earlier release started for a claim with a session and no file; its end is charged nothing",
		"session", m.claim.Session, "registeringSession", ev.Session, "pluginContract", ev.PluginContract,
		"daemonContract", m.deps.PluginContract, "generation", m.claim.Generation)
}

// launchRecordsNoProcess is whether the claim is launching with no process recorded: a launch
// written launching before it starts (launchAfter) and cut short, a relaunch waiting at the probe
// interval (relaunchLater), or a wake the pacing parked there until its turn (reserveWake). Every
// other live state records its process.
func (m *Machine) launchRecordsNoProcess() bool {
	return m.claim.State == StateLaunching && m.claim.Locator == nil
}

// dropStale logs a stale event the first time it is seen. The sweep observes every thirty seconds
// and a shim replays its backlog on every reconnect, so the same stale fact arrives again and
// again; the operator needs it once.
func (m *Machine) dropStale(event, fence, got, held string) {
	seen := event + "/" + fence + "/" + got
	if m.stale[seen] {
		return
	}
	m.stale[seen] = true
	m.log.Warn("supervise: dropped a stale event", "event", event, "fence", fence, "got", got, "held", held)
}

// launch starts a process for the claim at a new generation with a new boot token: the same
// agent resumed from its session file when the claim has one, or, for an Oh My Pi claim, from its
// session id when the claim records a session and no file (start, resumeByID) — after the process
// the claim last ran is gone — and a fresh spawn when it records no session. A Claude Code claim
// that records a session and no file forgets the session and spawns a fresh agent in the same try
// (start, startFresh). A process the claim still records is let go first, so it is the one waited
// out. The boot token's hash is persisted before the process starts, so the shim's first hello
// resolves. A launch the runtime or the spec refuses is a launch
// failure and is tried again after the backoff (backOff), waiting out the same process, until the
// budget runs out; only a start that succeeds forgets it. A system prompt past the start frame
// (runtime.PromptTooLarge, promptRefused) is no launch failure: the claim fails at once, charged
// nothing, and a start the session store could not answer is charged nothing and tried again
// (startWaitsForStore), as is one the runtime refused for want of its pod's update round
// (startWaitsForPodRound). A launch that fails stops the process it
// let go (stopPrevious), whichever step failed. A launch the tree lifecycle refuses (checkLaunch)
// starts nothing and is charged nothing (relaunchRefused), and so is one whose context ended under
// it — the daemon's stop. A runtime waits out a cluster that answers a launch's requests overloaded
// inside the launch's own deadline, so what reaches here failed the launch. This launch waits its
// turn in the wake pacing first (reserveWake), the project controller's claim excepted: it is the
// recovery path, which no request raises, so there is no request here to let one through. A
// launch a request raised that the pacing lets through goes by launchAfter (revive).
//
// cause is why the process the claim still records is let go, which the let-go records with
// what that process ended (letGo, recordEnd); noEnd for a launch that lets none go.
func (m *Machine) launch(ctx context.Context, cause string) error {
	return m.launchAfter(ctx, false, false, false, time.Time{}, cause)
}

// launchAfter is launch, whose first try waits the backoff when charged: the launch follows a
// launch failure charged just before it (relaunchAfterFailure). exempt is whether the request
// that raised it is one the wake pacing never makes wait (exemptWake); every other path into a
// launch is paced, and the gate sits here, between the lifecycle check and letGo, because a
// launch the tree lifecycle refuses charges no budget and so takes no token either, while a
// launch that waits its turn keeps the process it still runs until that turn comes.
//
// Which lane the wake it reserves waits under is no part of what this path says: it is every
// launch's, raised by a death, a resume, a retry, a repoint or the stale-plugin lane alike, so
// the gate reads the lane off the intent that lane leaves on the machine rather than being told
// one here (reserveWake, pluginIntent, markPluginRelaunch).
//
// Which retries pass this gate again, and which do not. The gate is taken once for the launch
// that gets as far as the runtime: the loop's own retry of a start the runtime refused re-enters
// launchTries, the grant of a start its issue pod's gate held runs the launch on there
// (podStartDue), and the turn the wake waited for re-enters launchNow, and none of them takes a
// second token. Two retries do re-enter here, and so reserve again: a launch whose write of
// StateLaunching failed (relaunchLater, from launchTries), and a launch the lifecycle check could
// not answer, from here or from the wake's own turn (relaunchRefused, wake.again). Both are kept
// paced because both are rare — the store answers every other launch, and one that does not has
// just failed a write this daemon makes before every process it starts, so a daemon meeting them
// often is a daemon whose store is down, where a token spent waiting is the least of it — and
// because each is a fresh launch from the top: it rechecks the tree lifecycle, as every launch of
// this package does, and a retry that skipped the gate would launch outside the pace the operator
// set while the queue behind it still waited.
//
// Every try runs on a context of the machine's own (startable), which a suspension cancels: a
// start under Kubernetes waits minutes on a pod, and an operator suspending the claim meanwhile
// waited all of that before its request was even looked at. A cancelled start is charged nothing,
// as a start the daemon's stop cut short is, and the cause of every start that did fail is kept on
// the claim (Claim.LastStartFailure) for whoever asks why it is not running.
//
// cause is launch's: why the process the claim still records is let go. It travels with the
// launch to wherever it runs — here, or at the turn of the wake the gate parks it on, which is
// when a process the park left running is let go and what it ended is read (launchNow, letGo).
//
// requested is whether a request raised this launch — a task, a message, an operator's spawn,
// resume or retry, every launch revive makes — which the pod's start gate puts first among the
// starts waiting in the pod (podQueue.next). It travels with the launch as cause does, captured
// in the work the wake gate may run at its turn, never on the machine, where another launch
// could read it. A retry raised anew from the top — the lifecycle check's (again) and every one
// relaunchLater arms — is a request's no longer, and passes false.
//
// turn is the moment the request already reserved for this launch in the wake pacing, the zero
// time for every launch that reserved none, which the gate decides by the rule RequestSpawn.Turn
// states. Only the gate reads it, and no field of the machine holds it; a retry raised anew from
// the top reserves afresh, passing the zero time.
func (m *Machine) launchAfter(ctx context.Context, charged, exempt, requested bool, turn time.Time, cause string) error {
	again := func(ctx context.Context) error { return m.launchAfter(ctx, charged, exempt, false, time.Time{}, cause) }
	if err := m.checkLaunch(ctx); err != nil {
		return m.relaunchRefused(ctx, again, err)
	}
	return m.reserveWake(ctx, exempt, turn, func(ctx context.Context) error {
		return m.launchNow(ctx, charged, exempt, requested, cause)
	}, again)
}

// launchNow is launchAfter past its lifecycle check and its wake gate: the launch itself, and the
// one entry the wake's own turn re-enters by (wakeNow), which is what keeps that turn from taking
// a second token. Each try (launchTries) is written launching, at its generation and with every
// charge so far, before its backoff waits. A try whose write fails has started nothing and is
// tried again at the probe interval (relaunchLater), the process it let go stopped: by then the
// claim records no process, so nothing else would launch it before a restart. One the daemon's
// stop cut short is left to the next boot, as a start it cut short is.
//
// This is where a stale-plugin relaunch marks its claim (commitPluginRelaunch): the process the
// relaunch replaces is let go here and the mark is written with the launch itself, so
// PluginRelaunchFor names a relaunch whose launch was recorded and reaches the store with the
// write that records it, before the runtime starts the process. A relaunch abandoned anywhere
// before this line — refused by the lifecycle recheck, cut off by a take-back the store refused,
// ended in a suspension, or dropped with the wake a second launch replaced — never reaches it
// and marks nothing (relaunchMarked, runWake). A try whose own write fails keeps the mark it
// has already written in memory, which the retry's write then stores (relaunchLater), and the
// intent that mark came from goes back on the machine where that retry runs, so the relaunch it
// raises waits under the lane's own kind rather than under the launch kind every other launch
// takes — the restoration a refused turn's retry takes, from the same wrapper (retryHolding,
// runWake, reserveWake, dispatch://INFRA-2712 R4).
//
// It is also where the process a relaunch replaces is let go, for cause, and what that process
// ended is recorded before the first try's write moves the generation and clears the process's
// activity report (letGo, recordEnd, the store's putClaim). The retries this launch arms — a
// failed write's here, a session store's that could not answer (startWaitsForStore), a pod
// round's (startWaitsForPodRound) — launch a claim that records no process any more, so they
// pass noEnd.
//
// Each try of a claim of an issue pod asks the pod's start gate right before it asks the runtime
// for its process, so no role's process starts while another role of the pod starts or loads its
// session (askPod, PodStarts, dispatch://INFRA-5297). requested is launchAfter's, and is what the
// gate's order reads of the try. A try the gate cannot grant at once stops the process this launch
// let go, parks on the machine, and this decision ends; the grant runs the rest of the launch from
// that try's start (askPod, podStartDue, launchTries).
func (m *Machine) launchNow(ctx context.Context, charged, exempt, requested bool, cause string) error {
	// The intent is read before it is committed rather than taken, because committing takes it
	// off the machine (commitPluginRelaunch, takePluginRelaunch) and the retry a failed write
	// arms must put that same one back where it runs. Reading it without taking it is what the
	// wake gate does for the kind it reads off it (reserveWake).
	intent := m.pluginRelaunch
	m.commitPluginRelaunch()
	// Read before letGo clears it: this launch may be the retry of a wait for a pod's round, whose
	// next refusal is the same wait and logs nothing more (startWaitsForPodRound). The pod that
	// wait is for stays the one the claim waits on through this launch's try, which is what the
	// update round reads of a claim that records no process (Claim.AwaitsPod): a sweep that reads
	// the claim while the try runs finds the pod, and the try's outcome settles it below.
	waiting, awaits := m.podRoundWaiting, m.awaitsPod
	m.letGo(ctx, cause)
	m.awaitsPod = awaits
	// The roll edge's mark names the process an earlier release started, which this launch has
	// let go; the process the launch starts is this release's own (refusedEarlierRelease).
	m.refusedEarlierRelease = 0
	// The launch that follows the controller session's second end in a row starts a fresh
	// session, once the process it replaces is let go and what that process ended recorded
	// (replaceEndedSession, dispatch://INFRA-2045 R1).
	m.replaceEndedSession(ctx)
	return m.launchTries(ctx, launchTry{intent: intent, charged: charged, exempt: exempt, requested: requested, waiting: waiting}, nil)
}

// launchTry is what every try of one launch reads, and what a try parked at its issue pod's gate
// carries to its grant (askPod): the intent of the stale-plugin relaunch the launch is, for the
// retries it may arm (relaunchAtProbe); whether its next try waits the backoff (charged); whether
// the request that raised it is one the wake pacing lets through (exempt); whether a request
// raised it at all, which the pod's gate reads (requested); and whether the launch retries a wait
// for its pod's update round that is logged already, which a charged failure ends
// (startWaitsForPodRound, waiting).
type launchTry struct {
	intent                              *pluginIntent
	charged, exempt, requested, waiting bool
}

// podGrant is a try the pod's gate granted after it parked (podStartDue): the boot token its
// claim was written with, and the hold of the pod it was granted.
type podGrant struct {
	token string
	hold  *podHold
}

// launchTries is the tries of one launch, from its first (launchNow), or from the start in the
// runtime of a try the pod's gate parked and then granted (granted): that try was written and
// waited its backoff before it parked, so it goes on where it stopped, at the generation and with
// the boot token it was written with. Every other try is written (writeTry), checks the project's
// pause, and then asks its issue pod's gate (askPod), which lets it start or parks it. A try whose
// start fails and that is tried again is written, checks the pause and asks the gate again, and may
// park again. A try the gate parks ends the launch's decision with no error, whatever the stop of
// the process the launch let go answered there; askPod says why.
//
// The pause is checked after the write because the wake gate's check came before it (reserveWake),
// and a pause can begin between the two: its catch-up reads the claim's published view
// (internal/daemon's Paused), which shows the claim as it stood before this launch until the write
// publishes it launching. A try that finds the project paused here starts nothing; the pause's
// gate holds it (pauseHeldTry). A pause that begins after this check finds the claim published
// launching, and its catch-up asks the claim's stop.
//
// Every try runs on a context of the machine's own (startable), which a suspension cancels; the
// decision a grant starts makes its own, so a suspension cuts short the start a grant began as it
// cuts short any other.
func (m *Machine) launchTries(ctx context.Context, l launchTry, granted *podGrant) error {
	starting, started := m.startable(ctx)
	defer started()
	for {
		var token string
		var hold *podHold
		if granted != nil {
			token, hold, granted = granted.token, granted.hold, nil
		} else {
			var err error
			if token, err = m.writeTry(starting, l); err != nil {
				return err
			}
			if pause, holds := m.pauseHolds(); holds {
				return m.pauseHeldTry(starting, l, pause)
			}
			var parked bool
			if hold, parked = m.askPod(starting, l, token); parked {
				return nil
			}
		}
		loc, way, err := m.start(starting, token)
		// The try settles the wait for a pod's round that this launch may retry: a refusal for
		// that round names the pod again (startWaitsForPodRound), and any other outcome ends the
		// wait, whether it started a process or failed otherwise.
		if !errors.Is(err, runtime.ErrAwaitingPodRound) {
			m.awaitsPod = nil
		}
		// A start cut short by its own context was refused by nothing, so it records no cause. The
		// cause is a launcher's sentence, which can come from the pod, so the claim keeps it as the
		// daemon keeps a far side's text (storetext.FarSideText, the rule leakguard.Cause applies):
		// the claim the operator's answers read holds the cause the store holds.
		m.claim.LeakCheckCause = ""
		if starting.Err() == nil {
			m.claim.LeakCheckCause = storetext.FarSideText(runtime.LeakCheckCause(err))
		}
		if err == nil {
			m.previous = nil
			// The process this launch let go is the one the start was handed and waits out
			// (start, Resume), so a stop an earlier try of this same launch could not make is no
			// longer what anyone must be told of.
			m.stopUnconfirmed = false
			// The mark of a start that had Oh My Pi resume the session by its id, which start set
			// (Claim.AgentResumeGeneration), reaches the store in this one write with the process
			// it names, so a claim that records that process always records the mark with it.
			m.claim.Locator = &loc
			m.leftPodRoundAt(loc)
			m.armBoot()
			m.log.Info("supervise: launched", "generation", m.claim.Generation, "incarnation", loc.Incarnation,
				"resumed", way != wayNone, "resumedBy", string(way))
			m.podStarted(hold, loc)
			return m.persist(starting)
		}
		m.podStartFailed(hold)
		if starting.Err() != nil {
			// The runtime refused nothing: the launch ended with its context — the daemon's stop,
			// or a suspension that cancelled the start. The claim stays launching with no process
			// recorded, which that suspension's own row then suspends (holdSuspendUntilReady), and
			// which the next boot launches again once its orphan reconciliation has reaped every
			// process no claim records, the one let go included. Until that boot the process this
			// launch let go was never stopped, which is what the owner of a stalled worker is told
			// (stopUnconfirmed).
			m.stopUnconfirmed = true
			m.log.Info("supervise: launch cut short; its context ended", "generation", m.claim.Generation, "error", err)
			return err
		}
		if errors.Is(err, errHoldUnanswered) {
			// The store did not answer the session's hold, which says nothing about the claim or
			// its session: nothing started, so nothing is charged, and the same launch is tried
			// again at the probe interval, as a launch whose write the store did not take is.
			err = errors.Join(err, m.stopPrevious(starting))
			m.log.Warn("supervise: the store did not answer the session's hold; trying the launch again at the probe interval, charging nothing",
				"generation", m.claim.Generation, "error", err)
			m.relaunchAtProbe(l)
			return errors.Join(err, m.persist(starting))
		}
		m.claim.LastStartFailure, m.claim.LastStartFailureAt = storetext.FarSideError(err.Error()), m.deps.Clock.Now()
		if errors.Is(err, runtime.ErrSessionStore) {
			return m.startWaitsForStore(starting, err, l.charged, l.exempt)
		}
		if errors.Is(err, runtime.ErrAwaitingPodRound) {
			return m.startWaitsForPodRound(starting, err, l.charged, l.exempt, l.waiting)
		}
		var tooLarge *runtime.PromptTooLarge
		if errors.As(err, &tooLarge) {
			return m.promptRefused(ctx, starting, err)
		}
		err = errors.Join(err, m.stopPrevious(starting))
		m.claim.Budgets.LaunchFailures++
		m.logFarSideError(ctx, slog.LevelWarn, "supervise: launch failed", err,
			"generation", m.claim.Generation, "launchFailures", m.claim.Budgets.LaunchFailures, "limit", m.deps.Limits.LaunchFailures)
		if m.claim.Budgets.LaunchFailures >= m.deps.Limits.LaunchFailures {
			return m.failLaunch(ctx, err, fmt.Sprintf("launch failures ran out: %v", err))
		}
		l.charged = true
		// A charged failure ends whatever wait this launch retried: a refusal on the next try is
		// a new one.
		l.waiting = false
	}
}

// heldTryWake is the wake that holds a try of launch l the wake gate let past, so it has had its
// turn from the pacing and written its line (wake.paid, wake.ran), in the claim's state as the try
// left it, launching: a try parked at its issue pod's gate (askPod), which sets park, and a try the
// project's pause met after its write (pauseHeldTry). Its run is the launch from the park's grant
// on while the wake carries the hold, the grant starting with token, the boot token the try was
// written with; and a fresh try of the same launch, asked of the gate anew, once it carries none,
// as a let-out after a pause runs it (letOutHeldLaunch, runWake). Its again is the launch raised
// anew from the top, for that let-out's lifecycle recheck (launchFromTop, relaunchRefused).
func (m *Machine) heldTryWake(l launchTry, token string) *wake {
	held := &wake{kind: WakeLaunch, state: m.claim.State, paid: true, ran: true, again: m.launchFromTop(l)}
	held.run = func(ctx context.Context) error {
		if held.park == nil {
			return m.launchTries(ctx, l, nil)
		}
		return m.launchTries(ctx, l, &podGrant{token: token, hold: held.park})
	}
	return held
}

// stopLetGo is the stop a try that is held rather than started makes of the process its launch let
// go (stopPrevious): a try parked at its issue pod's gate (askPod) and a try the project's pause
// met (pauseHeldTry). A stop the runtime refuses, or that the try's own context cuts short, leaves
// the try held all the same: the refusal is recorded (stopUnconfirmed) and logged at Warn, naming
// what the launch waits for, and no error is answered, for the reason askPod gives.
func (m *Machine) stopLetGo(ctx context.Context, waitsFor string) {
	if stopped := m.stopPrevious(ctx); stopped != nil {
		m.log.Warn("supervise: the runtime did not confirm the stop of the process this launch let go; the launch waits for "+waitsFor+" all the same",
			"generation", m.claim.Generation, "error", stopped)
	}
}

// writeTry is a fresh try of the launch (launchTries): the claim at the next generation with a
// new boot token, written launching with no failure, then the backoff of a charged try. It
// answers the boot token, or the error the launch ends with at this try, which launchTries
// returns: a write the store did not take, which is tried again at the probe interval unless the
// try's own context ended (relaunchAtProbe), or a backoff cut short.
func (m *Machine) writeTry(starting context.Context, l launchTry) (token string, err error) {
	m.claim.Generation++
	token = rand.Text()
	m.claim.BootTokenHash = HashBootToken(token)

	m.claim.State = StateLaunching
	// The pod end goes with the failure: a claim being launched again carries nothing of the end
	// that stopped the process before it (Claim.PodEnd).
	m.claim.Failure, m.claim.FailureCause, m.claim.FailedAt, m.claim.PodEnd = "", "", time.Time{}, ""
	if err = m.persist(starting); err != nil {
		err = errors.Join(err, m.stopPrevious(starting))
		if starting.Err() == nil {
			m.log.Warn("supervise: the launch could not be written; trying it again at the probe interval",
				"generation", m.claim.Generation, "error", err)
			m.relaunchAtProbe(l)
		}
		return "", err
	}
	if l.charged {
		if err = m.backOff(starting); err != nil {
			return "", err
		}
	}
	return token, nil
}

// relaunchAtProbe leaves the launch launchTries was making to be tried again at the probe
// interval (relaunchLater), raised anew from the top (launchFromTop). It is the retry of the two
// tries that started nothing and were refused by nothing about the claim: a write the store did
// not take, and a hold of the session the store did not answer (launchTries).
func (m *Machine) relaunchAtProbe(l launchTry) {
	m.relaunchLater(m.deps.Timeouts.Probe, m.launchFromTop(l))
}

// launchFromTop is the launch l's tries belong to raised anew from the top: as no request's,
// letting no process go (noEnd), and as the lane whose intent it carried (retryHolding). The
// retries of a try that started nothing take it (relaunchAtProbe), and so does every try the
// wake gate let past that a gate then holds, for the lifecycle recheck of its let-out after a
// pause (heldTryWake, wake.again).
func (m *Machine) launchFromTop(l launchTry) func(context.Context) error {
	return m.retryHolding(l.intent, func(ctx context.Context) error {
		return m.launchAfter(ctx, l.charged, l.exempt, false, time.Time{}, noEnd)
	})
}

// promptRefused is a start the runtime refused because the role's composed system prompt is larger
// than the one bound that still applies to it (runtime.PromptTooLarge): the start frame a pod's
// launcher reads, which the sandbox runtime measures before anything starts. Nothing was
// started, and every later try would compose the same text from the same files and be refused
// the same way until the deployment changes them, which takes a restart of the daemon. So it is
// charged no launch failure and not tried again: the claim fails at once, under the cause the
// charged path at its limit would name (launchTries), with the refusal's sentence — the role, the
// prompt's size and the limit — as its failure and as LastStartFailure, which read_state serves as
// the claim view's `failure.detail` and `lastStartFailure`. The process the launch let go is
// stopped first, as every refused start's is.
func (m *Machine) promptRefused(ctx, starting context.Context, refusal error) error {
	err := errors.Join(refusal, m.stopPrevious(starting))
	m.log.Error("supervise: the role's system prompt is too large to launch; failing the claim, charging no launch failure",
		"generation", m.claim.Generation, "error", err, "launchFailures", m.claim.Budgets.LaunchFailures)
	return m.failLaunch(ctx, err, fmt.Sprintf("launch refused, charged no launch failure: %v", refusal))
}

// failLaunch fails the claim over a launch the runtime refused, with err, the refusal and what
// stopping the process it let go answered, and detail, the claim's failure sentence. The refusal
// is this claim's own cause only where nothing else is already being relaunched after: a claim
// that died and whose relaunch the runtime then refused reads died (Claim.PendingCause).
func (m *Machine) failLaunch(ctx context.Context, err error, detail string) error {
	cause := m.claim.PendingCause
	if cause == "" {
		cause = CauseLaunchRefused
	}
	return errors.Join(err, m.fail(ctx, cause, detail))
}

// startable is the context the tries of one launch run on: the caller's, which a suspension can
// cancel through the machine (stopStart). The returned function clears it and cancels what it
// left, so nothing outlives the launch; a new launch also forgets that an older one was stopped,
// which only the suspension that stopped it reads (startStopped).
func (m *Machine) startable(ctx context.Context) (context.Context, func()) {
	starting, cancel := context.WithCancel(ctx)
	m.startMu.Lock()
	m.cancelStart, m.startStopped = cancel, false
	m.startMu.Unlock()
	return starting, func() {
		m.startMu.Lock()
		m.cancelStart = nil
		m.startMu.Unlock()
		cancel()
	}
}

// stopStart ends the start in flight, if one is running, and reports whether it cancelled one. It
// takes startMu alone, never mu, because the decision that is launching holds mu throughout: that
// is the whole point — a suspension that waited for mu would wait out the very launch it is
// cancelling (Handle). That it stopped one is remembered for the suspension's own row, which runs
// once the cancelled launch has let the lock go (stoppedAStart).
func (m *Machine) stopStart(why string) bool {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	if m.cancelStart == nil {
		return false
	}
	m.log.Info("supervise: cancelling the start in flight", "why", why)
	m.cancelStart()
	m.startStopped = true
	return true
}

// stoppedAStart reports, once, whether the event now being handled stopped a start of this claim:
// a claim left launching with nothing running because its own suspension cut the launch short,
// which that suspension completes rather than holds (holdSuspendUntilReady).
func (m *Machine) stoppedAStart() bool {
	m.startMu.Lock()
	defer m.startMu.Unlock()
	stopped := m.startStopped
	m.startStopped = false
	return stopped
}

// sessionStoreRetry is how long a start whose session store could not answer waits before trying
// again, and sessionStoreReport how often the issue and the controller are told while that lasts:
// a store outage is a deployment fault a person fixes, so the claim keeps its session and waits
// for it rather than spending a launch budget on it, and the people who can fix it hear of it at
// once and then once an hour rather than every minute (dispatch://INFRA-714 R8).
const (
	sessionStoreRetry  = time.Minute
	sessionStoreReport = time.Hour
)

// startWaitsForStore is a start the session store could not answer: the table is missing, or the
// store did not reply. That says nothing about the claim's session, so it is neither a lost
// session nor a launch failure — charging it would fail a claim whose agent is fine for a fault
// outside it — and the same session is started again every minute (relaunchLater) until the store
// answers. The issue and the controller are told at once and then hourly, since nothing else says
// why the worker is not running. The process the launch let go is let go already, so the retry
// lets none go (noEnd). The store's error is a far side's text: the line, the claim and the
// notices carry it as the claim keeps one (storetext.FarSideError, logFarSideError).
func (m *Machine) startWaitsForStore(ctx context.Context, cause error, charged, exempt bool) error {
	err := errors.Join(cause, m.stopPrevious(ctx))
	m.logFarSideError(ctx, slog.LevelError, "supervise: the session store could not answer this start; trying it again every minute, charging nothing",
		err, "generation", m.claim.Generation, "retry", sessionStoreRetry)
	m.tellStoreFault(cause)
	m.relaunchLater(sessionStoreRetry, func(ctx context.Context) error {
		return m.launchAfter(ctx, charged, exempt, false, time.Time{}, noEnd)
	})
	return errors.Join(err, m.persist(ctx))
}

// podRoundRetry is how long a start refused for want of its pod's update round waits before
// trying again (startWaitsForPodRound): the round replaces the pod once every role in it has
// answered, or at its bound (pod_update_grace_minutes), and the first try after that finds the
// pod the round brought.
const podRoundRetry = time.Minute

// podRoundWaitLine is the one line a claim's wait for its pod's update round logs, at the refusal
// that begins the wait (startWaitsForPodRound).
const podRoundWaitLine = "supervise: this start waits for its pod's update round; trying it again every minute, charging nothing"

// startWaitsForPodRound is a start the runtime refused because the live pod it would keep carries
// no exchange identity, under dispatch_token_broker, and the deployment names no transition token
// for such a pod (runtime.ErrAwaitingPodRound). That is the pod's state, not the claim's failing:
// charging it would fail a claim whose agent is fine for a pod that only its round replaces, so
// the same launch is started again every minute (relaunchLater) until the pod it finds carries the
// identity. A round can take hours, so the wait logs one line, with the runtime's refusal, which
// names the pod, at the refusal that begins it; waiting is whether this refusal is a retry of a
// wait already logged (podRoundWaiting). The process the launch let go is let go already, so the
// retry lets none go (noEnd).
//
// The claim records no process while it waits, and it may be its issue's only claim, so it keeps
// the pod the refusal names (awaitsPod, Claim.AwaitsPod): that is what the daemon's update round
// opens over and replaces the pod through, where no claim records a process in it. Without it no
// round would come, and this wait would never end. The write below publishes it, before the
// retry is due.
func (m *Machine) startWaitsForPodRound(ctx context.Context, cause error, charged, exempt, waiting bool) error {
	err := errors.Join(cause, m.stopPrevious(ctx))
	if !waiting {
		m.log.Warn(podRoundWaitLine, "generation", m.claim.Generation, "retry", podRoundRetry, "error", err)
	}
	m.relaunchLater(podRoundRetry, func(ctx context.Context) error {
		return m.launchAfter(ctx, charged, exempt, false, time.Time{}, noEnd)
	})
	m.podRoundWaiting = true
	m.awaitsPod = nil
	var refusal *runtime.AwaitingPodRound
	if errors.As(cause, &refusal) {
		pod := refusal.Pod
		m.awaitsPod = &pod
	}
	return errors.Join(err, m.persist(ctx))
}

// RegisteredPods is the set of pods in which an agent of a claim registered a session, as its
// machine knows it (Claim.RegisteredPods).
type RegisteredPods struct {
	// Pods is the uid of each pod an agent of the claim registered a session in, each once, in the
	// order each registered.
	Pods []string
	// Unknown is whether an agent of the claim may have registered in a pod that Pods does not
	// name: a machine built over a stored claim that shows a session it held cannot say where
	// each earlier agent of it registered (bootRegisteredPods).
	Unknown bool
}

// add puts pod in the set, unless it is "" (a process in no pod) or in the set already.
func (s *RegisteredPods) add(pod string) {
	if pod != "" && !slices.Contains(s.Pods, pod) {
		s.Pods = append(s.Pods, pod)
	}
}

// clone is s with Pods of its own, so a claim handed out of the machine shares no slice with the
// set the machine adds to (current, copyClaim).
func (s RegisteredPods) clone() RegisteredPods {
	s.Pods = slices.Clone(s.Pods)
	return s
}

// bootRegisteredPods is the set of a machine built over c (NewMachine). A claim the store read
// back may have registered agents in pods before this machine was built, which no memory of it
// records, so the set is read off the stored claim, and it holds no pod:
//
//   - A claim that shows a session it held, now (Session) or abandoned (Claim.Abandoned), may have
//     registered an agent in any pod, which nothing stored names, so the set is Unknown.
//     forgetSession writes the abandoned record whenever it drops a session, in the write that
//     drops it, and nothing clears it: the next abandonment replaces it. The daemon's update round
//     names every member that had not answered whose set is unknown, for any pod
//     (internal/daemon's workOnRoundPod): the rule errs towards naming a role that lost work.
//   - A claim that shows neither has registered no agent anywhere: the set is known and empty.
//
// The pod of the process that a stored claim whose state holds a registered agent
// (HoldsCapability) records is not put in the set, because it would decide nothing. Only register
// brings a claim to such a state, and it records the session in the same write, which only
// forgetSession clears and which forgetSession then records as abandoned. So such a stored claim
// always shows a session it held, its set is Unknown, and the update round names it for every pod.
func bootRegisteredPods(c Claim) RegisteredPods {
	return RegisteredPods{Unknown: c.Session != "" || c.Abandoned.Session != ""}
}

// locatorPod is the uid of the pod loc names, and "" for none and for a process in no pod (a tmux
// pane).
func locatorPod(loc *runtime.Locator) string {
	if loc == nil || loc.Sandbox == nil {
		return ""
	}
	return loc.Sandbox.PodUID
}

// tellStoreFault reports the session store fault to the issue and the controller, at the first
// start that met it and at most once an hour after that (Deps.SessionStoreFault). The hour is
// counted from the last report rather than from the first fault, so an outage that ends and
// returns is reported again.
func (m *Machine) tellStoreFault(cause error) {
	if m.deps.SessionStoreFault == nil {
		return
	}
	now := m.deps.Clock.Now()
	if !m.storeFaultTold.IsZero() && now.Sub(m.storeFaultTold) < sessionStoreReport {
		return
	}
	m.storeFaultTold = now
	m.deps.SessionStoreFault(m.current(), storetext.FarSideError(cause.Error()))
}

// launchWaits is whether a wake that starts a process waits over this claim: the one wake whose
// turn lets the claim's process go and starts the next, whichever lane parked it
// (StartsProcess, launchNow). It is what tells a claim that only waits its turn from one that
// is to be replaced, and two decisions read it: the delivery that would be handed to a process
// this launch is about to end (sendPending), and every observation of that same process,
// whichever path reports it (judge).
//
// It asks the predicate rather than naming a kind because what every reader of it means is "a
// process is about to be replaced here", which the launch and the stale-plugin refresh's
// relaunch answer alike (WakeLaunch, WakePluginRefresh): a lane added later that starts no
// process is one no reader of this holds back for.
func (m *Machine) launchWaits() bool {
	return m.wake != nil && StartsProcess(m.wake.kind)
}

// leftPodRoundAt records that the claim has left its pod round, in the one decision that records
// which pod the claim's process runs in (launchAfter). The round was opened over a pod this
// process did not come up in, so the work it asks about is on a disk that is gone: the claim's
// part in the round is frozen here, as the dead pod left it, and the round the claim left is the
// whole of what any reader of that part needs beside the answer it already records
// (Claim.LeftPodRound, the sweep's report of what the pod took with it).
//
// It is written once. A claim is asked nothing of a round it has left (internal/daemon's
// openPodUpdate publishes no ask to it), so it can answer nothing between one launch and the
// next, and the first launch out of the round's own pod is the one that says what was lost. Only
// the round's end clears it (clearUpdate), so a later relaunch, a suspension and a restart each
// leave it exactly as it is.
//
// A process in no pod leaves no pod round: a tmux pane runs outside one, and a claim in a plugin
// round or in no round at all has no pod round to leave.
func (m *Machine) leftPodRoundAt(loc runtime.Locator) {
	if !m.claim.InPodRound() || m.claim.LeftPodRound(m.claim.UpdateRound) {
		return
	}
	if loc.Sandbox == nil || PodRound(loc.Sandbox.PodUID) == m.claim.UpdateRound {
		return
	}
	m.claim.UpdateLeftRound = m.claim.UpdateRound
	m.log.Warn("supervise: this claim's process came up in another pod than its update round's; its part in that round stands as the round's own pod left it",
		"round", m.claim.UpdateRound, "pod", loc.Sandbox.PodUID, "answered", m.claim.UpdateSignalledAt != nil)
}

// backOff waits before a try that follows a charged launch failure: backoff.Wait from
// Timeouts.LaunchBackoff for the failures charged since the claim was last ready, so the tries a
// budget allows are spread over seconds rather than spent in the moment one fault lasts — a launch
// the runtime refuses would otherwise be tried again at once, and a process that dies as it starts
// be relaunched as soon as its death is seen. The wait holds the machine's decision, as a launch's
// runtime call does. The try it waits for is already written launching with its charge, so a wait
// the daemon's stop ends forgives nothing: the next boot launches that claim again, as it does a
// launch its context cut short.
func (m *Machine) backOff(ctx context.Context) error {
	wait := backoff.Wait(m.deps.Timeouts.LaunchBackoff, m.claim.Budgets.LaunchFailures)
	m.log.Info("supervise: waiting before the next launch", "after", wait, "launchFailures", m.claim.Budgets.LaunchFailures)
	if err := m.deps.Clock.Sleep(ctx, wait); err != nil {
		m.log.Info("supervise: the wait before the next launch was cut short; its context ended", "error", err)
		return err
	}
	return nil
}

// resumeWay is how a start began the claim's process (start), which the launched line names in
// its resumedBy field (launchTries) and a production check of dispatch://INFRA-4108 reads.
type resumeWay string

const (
	// wayFile is a Resume of the session file the claim records.
	wayFile resumeWay = "file"
	// wayID is a Resume of the session the claim records, by its id alone, which the pod's
	// launcher found in its session table or the tmux runtime in the pane's sessions directory.
	wayID resumeWay = "id"
	// wayAgentLookup is a Resume of that session through a pod launcher that cannot look a
	// session up by its id, whose agent's own command resolves the id (runtime.SpawnSpec's
	// ResumeByAgent): Oh My Pi's own lookup.
	wayAgentLookup resumeWay = "agent-lookup"
	// wayNone is a Spawn, which resumes nothing.
	wayNone resumeWay = "none"
)

// start launches the claim's process for this try, and answers how it began it (resumeWay):
//
//   - A claim that records a session file resumes that file.
//   - A claim that records a session and no file resumes the session by its id alone
//     (resumeByID), and spawns no fresh agent while it records the session: the fence would
//     refuse that agent's registration as another session (claim.SameAgentRefusal), and the
//     claim would be launched the same way again until its launch budget ran out
//     (dispatch://INFRA-4108 R1). A Claude Code claim resumes only a transcript file and asks for
//     no lookup by the id, so it forgets the session at once instead (startFresh,
//     SessionLossClaudeNoFile).
//   - A claim that records no session spawns a fresh agent.
//
// A Resume the runtime answers with runtime.ErrSessionFileMissing found the recorded session
// nowhere it could be resumed: no store holds it any more. Both runtimes answer that way for that
// one fact, of a file and of an id — the tmux runtime when no folder it searched holds the
// session, the Kubernetes launcher when the session table does not hold the row
// (internal/shimwire's LauncherStartResult) — and neither has started anything when it does.
// Resuming would fail the same way on every try and wedge the claim until its launch budget ran
// out, so the claim forgets the session and the same try spawns a fresh agent, which registers a
// session of its own (startFresh, SessionLossGone). It is charged nothing: a store that lost a
// session is not the claim failing.
//
// A store that could not answer at all is not that fact and never gets here: it wraps
// runtime.ErrSessionStore and the caller waits for the store (startWaitsForStore). Any other
// refusal of a Resume is a launch failure the caller charges, and the claim keeps its session: a
// fresh agent on a claim that had one is what the same-agent refusal exists to catch.
//
// A resume runs under every name its record gives the session, so each is held to the claim
// before anything starts (holdRecorded, plan E's t-e-54): an Oh My Pi launch continues the
// recorded file (`--resume=<file>`), or, in a pod that trades an exchange identity, the session
// by its id (`--resume <id>`: the claim's recorded session, which the resume's spec carries as
// ResumeSessionID, else the id the file's name gives), the tmux runtime one of the file's name or
// id where that path is missing (sessionFileOf), and a Claude Code launch the session the
// transcript's name gives (claudelaunch.ResumeSession), any of which a claim recorded before the
// hold could name for another claim's session. A resume by the id runs under the id and under the
// id the file it finds is named for (holdByID). One another claim holds is refused here, as a
// launch failure the caller charges, and nothing starts under it.
func (m *Machine) start(ctx context.Context, token string) (runtime.Locator, resumeWay, error) {
	spec, err := m.spawnSpec(ctx, token, false)
	if err != nil {
		return runtime.Locator{}, wayNone, err
	}
	switch {
	case m.claim.SessionFile == "" && m.claim.Session == "":
		loc, err := m.spawn(ctx, spec)
		return loc, wayNone, err
	case m.claim.SessionFile == "" && runtime.Of(spec.Backend) == runtime.BackendClaude:
		return m.startFresh(ctx, token, SessionLossClaudeNoFile, nil)
	case m.claim.SessionFile == "":
		return m.resumeByID(ctx, token, spec)
	}
	if err := m.holdRecorded(ctx, m.claim.Session, m.claim.SessionFile); err != nil {
		return runtime.Locator{}, wayFile, err
	}
	spec.ResumeSessionFile, spec.ResumeSessionID = m.claim.SessionFile, m.claim.Session
	loc, err := m.deps.Runtime.Resume(ctx, m.previous, spec)
	if !errors.Is(err, runtime.ErrSessionFileMissing) {
		return loc, wayFile, err
	}
	return m.startFresh(ctx, token, SessionLossGone, err)
}

// resumeByID is start's resume of the session a claim records with no file, by its id alone,
// once the names it runs under are held to the claim (holdByID). A Resume the runtime answers
// with runtime.ErrSessionFileMissing forgets the session, as a missing file does (start).
//
// One it answers with runtime.ErrSessionLookupUnavailable was never sent: the pod's launcher is
// older than the lookup and has not said in its hello that it can make it (dispatch://INFRA-4108
// R3). The same try then goes on by the claim's mark of a start that had Oh My Pi resume the
// session by its id itself (Claim.AgentResumeGeneration):
//
//   - With no mark, the spec is built again, on a copy of the claim marked with this generation
//     (spawnSpec's byAgent), which is how the spec builder knows to add to the agent's prompt
//     that the workspace may have been recreated since the session last ran: that launcher reads
//     no row of the session table, so its word that the workspace was not recreated was not
//     checked. The prompt is part of the spec, so this build follows the first Resume's answer.
//     The spec is resumed with ResumeByAgent, waiting out the same previous process: the agent's
//     own command carries `--resume=<session id>`, which Oh My Pi resolves, and the launcher is
//     named no session and no file. A start that succeeds marks the claim with its generation,
//     which launchTries writes in the write that records the process, so a restart reads the mark
//     back; one the runtime refuses marks nothing and is a launch failure the caller charges, as
//     any refused start is.
//   - With a mark, an earlier start of that kind did not register as the session, since that
//     registration clears the mark (register). Oh My Pi is not asked again: this try forgets the
//     session, charged nothing (startFresh, SessionLossNotResumedByID).
//
// The mark is set only while it is 0, so it names one generation. A try through a pod since
// replaced by one whose launcher can make the lookup resumes by the id through that launcher and
// leaves the mark as it is, for that start's registration or a forget to clear.
func (m *Machine) resumeByID(ctx context.Context, token string, spec runtime.SpawnSpec) (runtime.Locator, resumeWay, error) {
	if err := m.holdByID(ctx, m.claim.Session); err != nil {
		return runtime.Locator{}, wayID, err
	}
	spec.ResumeSession = m.claim.Session
	loc, err := m.deps.Runtime.Resume(ctx, m.previous, spec)
	switch {
	case errors.Is(err, runtime.ErrSessionFileMissing):
		return m.startFresh(ctx, token, SessionLossGone, err)
	case !errors.Is(err, runtime.ErrSessionLookupUnavailable):
		return loc, wayID, err
	case m.claim.AgentResumeGeneration != 0:
		return m.startFresh(ctx, token, SessionLossNotResumedByID, err)
	}
	spec, err = m.spawnSpec(ctx, token, true)
	if err != nil {
		return runtime.Locator{}, wayAgentLookup, err
	}
	spec.ResumeSession, spec.ResumeByAgent = m.claim.Session, true
	loc, err = m.deps.Runtime.Resume(ctx, m.previous, spec)
	if err != nil {
		return loc, wayAgentLookup, err
	}
	m.claim.AgentResumeGeneration = m.claim.Generation
	return loc, wayAgentLookup, nil
}

// spawnSpec is the launch's spec: what the daemon builds for this claim, with what only the
// machine knows — who the claim is, the generation and the boot token this try mints. A resume
// adds its session file and the session id recorded beside it, or its session id alone; a spawn
// carries neither.
//
// Two things about this launch are not the claim's own state, and each is set on the copy handed
// to the builder and never on the claim the machine keeps, which is the claim it persists.
// spawnSpec reads SessionLost from the machine (Machine.lostCause): the cause of the forget this
// launch follows (startFresh), which the spec builder turns into a sentence in the agent's own
// prompt, and "" for a launch that follows none. byAgent marks the copy with the claim's
// generation (Claim.AgentResumeGeneration): the launch has Oh My Pi resume the session by its id
// itself (resumeByID), and the builder adds the sentence that the workspace may have been
// recreated. The claim the machine keeps is marked only once that start has succeeded, after its
// build, and each later try moves the generation before it builds, so no other build finds the
// mark equal to the generation.
func (m *Machine) spawnSpec(ctx context.Context, token string, byAgent bool) (runtime.SpawnSpec, error) {
	building := m.claim
	building.SessionLost = m.lostCause
	if byAgent {
		building.AgentResumeGeneration = m.claim.Generation
	}
	spec, err := m.deps.Specs.SpawnSpec(ctx, building)
	if err != nil {
		return runtime.SpawnSpec{}, fmt.Errorf("build the launch of %s: %w", m.claim.Token, err)
	}
	spec.Claim, spec.Project, spec.Tree, spec.Issue, spec.Role = m.claim.Token, m.claim.Project, m.claim.Tree, m.claim.Issue, m.claim.Role
	spec.Generation, spec.BootToken = m.claim.Generation, token
	spec.ResumeSessionFile, spec.ResumeSessionID = "", ""
	spec.ResumeSession, spec.ResumeByAgent = "", false
	return spec, nil
}

// startFresh forgets the session the claim records, for cause, and spawns a fresh agent in its
// place in the same try, charged nothing (start, resumeByID). cause chooses the words of the
// forget's warning line and of its two texts (forgetWords): the reason the claim keeps with the
// session it abandons (Claim.Abandoned), which the fresh agent is told at its first task, and the
// detail the workflow tells the issue, its architect and the controller (Deps.SessionLost).
// answer is the runtime's answer that led to the forget, nil for a Claude Code claim's, which
// asked the runtime nothing. It is a far side's error text, so the line carries it as the claim
// keeps one (logFarSideError). Only the texts of a session no store holds (SessionLossGone) carry
// that kept text; the texts of the other two causes carry no answer (forgetWords).
//
// The workflow is told once, at the forget and before the spawn: a fresh spawn that fails is a
// launch failure the caller charges, and its retry finds no session to forget, so a report made
// only after a spawn that succeeded would never be made at all. The cause is kept on the machine
// for the launches that follow the forget, which Machine.lostCause names and says until when, so
// each of them is built for a claim that knows its session was forgotten, and the agent that comes
// up is told in its own prompt rather than left to infer it from an empty transcript.
func (m *Machine) startFresh(ctx context.Context, token string, cause SessionLoss, answer error) (runtime.Locator, resumeWay, error) {
	line, kept := forgetLine(cause), ""
	if answer != nil {
		kept = m.logFarSideError(ctx, slog.LevelWarn, line, answer, "session", m.claim.Session, "sessionFile", m.claim.SessionFile)
	} else {
		m.log.Warn(line, "session", m.claim.Session, "sessionFile", m.claim.SessionFile)
	}
	why, detail := forgetWords(cause, m.claim.Session, kept)
	m.forgetSession(why)
	m.lostCause = cause
	if m.deps.SessionLost != nil {
		m.deps.SessionLost(m.current(), cause, detail)
	}
	spec, err := m.spawnSpec(ctx, token, false)
	if err != nil {
		return runtime.Locator{}, wayNone, err
	}
	loc, err := m.spawn(ctx, spec)
	return loc, wayNone, err
}

// forgetLine is the warning line of a forget for cause (startFresh). For a session no store holds
// it keeps the words it had before there was a cause, with the prefix a production check reads
// ("supervise: the recorded session is found nowhere"). startFresh writes the runtime's answer
// beside it, which for a session the pod's launcher could not look up names the pod, and writes
// none for a Claude Code claim's session with no file (SessionLossClaudeNoFile): the runtime was
// asked nothing.
func forgetLine(cause SessionLoss) string {
	switch cause {
	case SessionLossNotResumedByID:
		return "supervise: the pod's launcher cannot look the recorded session up by its id, and Oh My Pi did not resume it by that id, so the claim forgets it and starts a fresh agent, charged nothing"
	case SessionLossClaudeNoFile:
		return "supervise: a Claude Code claim records a session and no transcript file, so the claim forgets it and starts a fresh agent, charged nothing"
	}
	return "supervise: the recorded session is found nowhere the runtime can resume it, so the claim starts a fresh agent, charged nothing"
}

// forgetWords is what a forget of session for cause says beside its line (startFresh, forgetLine):
// the reason the claim keeps with the abandoned session (AbandonedSession.Why), and the detail the
// workflow is told (Deps.SessionLost). answer is the runtime's answer as the claim keeps a far
// side's text (logFarSideError). For a session no store holds, both keep the words they had
// before there was a cause, with that answer in both. For a session the pod's launcher could not
// look up, neither says that the session was lost or that no store holds it, and neither carries
// the answer, which could: nothing of the session was deleted, and a store may still hold it. For
// a Claude Code claim's session with no file (SessionLossClaudeNoFile), both say that the claim
// recorded the session with no transcript file.
func forgetWords(cause SessionLoss, session, answer string) (why, detail string) {
	switch cause {
	case SessionLossNotResumedByID:
		return fmt.Sprintf("the pod's launcher, which is older than this release, could not look the session %s up by its id, and Oh My Pi did not resume it by that id", session),
			fmt.Sprintf("the pod's launcher, which is older than this release, could not look the session %s this claim recorded up by its id, and Oh My Pi did not resume it by that id, so it runs a fresh agent that has read none of its own earlier work", session)
	case SessionLossClaudeNoFile:
		return fmt.Sprintf("a Claude Code claim recorded the session %s with no transcript file, so the launch cannot resume it", session),
			fmt.Sprintf("this Claude Code claim recorded the session %s with no transcript file, so the launch cannot resume it, and it runs a fresh agent that has read none of its own earlier work", session)
	}
	return "no session store the runtime could resume it from holds it any more: " + answer,
		fmt.Sprintf("the session %s this claim recorded is in no session store the runtime could resume it from, so it runs a fresh agent that has read none of its own earlier work: %s", session, answer)
}

// spawn starts a fresh agent for this launch, and so a session of its own: from here an answer of
// an earlier launch answered a session the claim no longer has (sessionFrom), where a resume keeps
// the session, and the answers of its earlier launches with it.
func (m *Machine) spawn(ctx context.Context, spec runtime.SpawnSpec) (runtime.Locator, error) {
	m.sessionFrom = m.claim.Generation
	return m.deps.Runtime.Spawn(ctx, spec)
}

// stopPrevious stops the process letGo moved into previous when a launch fails, or when a try of it
// is held rather than started — parked at its issue pod's gate, or held by the project's pause
// (stopLetGo): the claim no longer records it, so nothing else ends it.
// Machine owns it whichever step failed (persisting the launch, building its spec, Spawn, Resume);
// a runtime stops only a process it started itself (runtime.Runtime's Spawn and Resume). Suspend
// stops that one process alone, and does nothing for one already gone or replaced; previous is
// kept, for the next launch to wait out.
//
// A refusal is recorded as well as returned, and a stop the runtime makes clears that record
// (stopUnconfirmed): every caller goes on — to the retry, to the next try, to the hold, or to the
// failed claim — joining a refusal to an error of its own or returning it, but for the hold, which
// logs it at Warn and answers no error, for the reason askPod gives (stopLetGo). So the process is
// left possibly running, and the owner of a stalled worker is told that rather than that it was
// stopped. The tries of one launch stop one process, the one letGo moved into previous (launchTries
// holds it until a start succeeds), so what the record answers is whether that process was seen
// to stop, and the latest answer about it is the answer: a stop refused and then made is a process
// somebody saw stop, and an owner told otherwise is sent after a pod that is not there.
func (m *Machine) stopPrevious(ctx context.Context) error {
	if m.previous == nil {
		return nil
	}
	if err := m.deps.Runtime.Suspend(ctx, *m.previous); err != nil {
		m.stopUnconfirmed = true
		return fmt.Errorf("stop the process %s let go before its relaunch: %w", m.previous.Incarnation, err)
	}
	m.stopUnconfirmed = false
	return nil
}

// died is the claim's process found gone — or found to be some other process — while it was
// live: one launch failure, and the same session relaunched after it, or failed when the budget
// is spent. The runtime's account of the death is kept on the claim (Claim.LastDeathCause) before
// anything else, so whoever asks afterwards — the operator's listing, the notices a failed claim
// sends, the relaunched agent's own first prompt — is told why this claim's process ended rather
// than left to read the daemon's log. A task whose turn the process was running goes back to
// waiting (interrupted), for the relaunch to send, and a death with work outstanding is counted
// as one (chargeDeath), failing the claim at the limit. A process that dies while a suspension is
// held leaves the claim suspended instead (endHeld). A process that ended with a pod the daemon
// itself replaced is not a death at all (displaced).
//
// Two deaths of a claim that records a session and no file are charged nothing (endUncharged):
// that of a process an earlier release started, whose registration the fence refused, and that of
// a start that had Oh My Pi resume the session by its id and did not register as it. Each is
// recorded, taken back from, stopped and written as any death is, and relaunched through launch,
// which charges no launch failure and waits no backoff, in place of relaunchAfterFailure. Neither
// counts a death: each is of a claim whose agent has not registered as its session, which
// chargeDeath counts none of.
//
// One dead process is one death, however often the runtime reports that process gone. The report
// that reaches the charge below makes the charge decision and writes it (Claim.DeathCharged), and a
// later report of that same process is charged nothing: it raises the relaunch the charged
// report's was refused at, or fails the claim where its launch budget runs out. What leaves a
// claim to be reported twice is a step that wrote nothing of it — the take-back above that the
// store would not write, or the lifecycle's refusal of the relaunch, which keeps the claim over
// its dead process (relaunchRefused) — and the report that reaches it is the daemon's own orphan
// sweep, since the runtimes stop watching a process once they have reported it gone (the
// daemon's reconcileOrphans). What a second charge would cost such a claim is
// Claim.DeathCharged's. The claim is relaunched for as long as one process's death leaves
// budget for it, which is what the count bounds; a death of the process that relaunch starts
// is a death of its own, since the launch clears the record with the process it lets go
// (letGo).
//
// The record is of the charge and not of the report, because a report can end before the charge:
// one whose take-back the store would not write returns above, and a death under a held
// suspension leaves the claim suspended instead (endHeld). Were the skip keyed on the report,
// such a death would be charged nothing at all — the report that does take the task back would
// read it as a death already had — and a claim whose take-back write fails every time would
// relaunch for ever, with the deaths limit that bounds a crash loop never reached.
//
// The record outlives the daemon that made it, which is why it is the claim's and written here,
// with the deaths it charged in the same statement: a claim restored from the store is charged
// exactly what the daemon before it charged (Claim.DeathCharged).
//
// What the death ended is recorded first, before the take-back changes the task it finds
// (recordGoneEnd, recordEnd): the process is gone whatever this decision goes on to do, every fact
// the record reads is still as the death left it, and the relaunch that lets the process go can be
// a wake's turn away. The take-back follows whatever the claim's state, an idle claim's task whose
// prompt's result is still to come included, so a confirmed task is marked as taken by this
// decision before the record is read (Machine.cutTurnOf), and the record says its turn was cut
// off. The let-go that follows — the launch, the held suspension's or the failure's — finds this
// launch's record standing and adds none.
//
// This is the judgement that the agent behind the claim's Envoy session has ended, so it is where
// the session's clean stop is sent (sessionGone): a connection closing is not — a daemon restart
// closes every stream while the agents run on — and the probe that reaches here is what tells the
// two apart. It is sent before the take-back, so a death whose take-back the store would not write
// still stops its session. A held suspension the death ends is told the stop went (endHeld,
// stopSent), so the one death is one stop. The other places a stop is sent are listed on
// sessionGone.
func (m *Machine) died(ctx context.Context, observation runtime.Observation) error {
	m.log.Warn("supervise: process died", "incarnation", m.claim.Locator.Incarnation, "observed", string(observation.Kind),
		"detail", observation.Detail, "podEnd", observation.PodEnd, "chargedBefore", m.claim.DeathCharged)
	m.claim.LastDeathCause, m.claim.LastDeathAt = deathCause(observation), m.deps.Clock.Now()
	// The pod's own reason for this end is kept on the claim, so the failure the workflow is told
	// of when this death fails it — here, or where the relaunch it raises finds the launch budget
	// spent (relaunchAfterFailure) — names the cause the role never chose, and so does the boot's
	// replay of that failure after a daemon restart (Claim.PodEnd). Each report writes its own, an
	// empty one included, so where died wrote them the pod end and the account above are one
	// report's; a suspension over a dead process writes the account alone
	// (suspendOverADeadProcess). It is written before any branch below returns: each of them
	// persists the claim or leaves it to the next decision's write, and every path out of here has
	// had the end recorded. The end is the kubelet's text, so the claim keeps it as the store keeps
	// a far side's text (storetext.FarSideText): the failure's fact, written from the claim in
	// memory, names the pod end the store holds and a restart's replay reads back. The rule never
	// adds a rune, so the runtime's bound (runtime.PodEndRunes) still holds.
	m.claim.PodEnd = storetext.FarSideText(observation.PodEnd)
	ended := endDied(m.claim.LastDeathCause)
	m.recordGoneEnd(ctx, ended)
	m.sessionGone(ctx)
	if err := m.interrupted(ctx, "the process died in the task's turn", observation.OutOfMemory); err != nil {
		return err
	}
	if m.held != nil {
		return m.endHeld(ctx, false, stopSent)
	}
	uncharged := m.endUncharged("died")
	if m.claim.DeathCharged {
		return m.relaunchDeath(ctx, ended, uncharged, time.Time{})
	}
	m.claim.DeathCharged = true
	// The death the machine charges is an end of the session's launch (SessionRun), counted in
	// the decision that charges it, once for the one process; an end charged nothing — the roll
	// edge's, a resume by Oh My Pi's own lookup that never registered — is not.
	if !uncharged {
		m.sessionEnded(diedEnding(slices.Contains(unready, m.claim.State), m.claim.LastDeathCause))
	}
	if m.chargeDeath() {
		return m.fail(ctx, CauseDied, "deaths with work outstanding ran out")
	}
	// This death's launch failure is charged by the first raise of its relaunch that passes the
	// lifecycle check, and that charge records died as the cause pending (relaunchAfterFailure,
	// deathLaunchFailureCharged). Cleared here, with the decision, the cause says no raise has
	// charged this death yet, so a later raise — the next report of the death, or the boot's
	// replay of it after a restart — charges it only where none did.
	if !uncharged {
		m.claim.PendingCause = ""
	}
	// The decision and the count it charged are written here, before the relaunch, because
	// every window that leaves this claim to be reported again writes nothing of it: the
	// lifecycle's refusal of the relaunch below keeps the claim exactly as it stands
	// (relaunchRefused), and a relaunch the wake pacing parks writes nothing until its turn
	// comes (launchNow). A write the store would not take is owed and logged where it fails,
	// and the claim's next decision makes it good (owes); the relaunch goes on under it, since
	// a death that left the claim standing for want of a write would be a worse answer to an
	// outage than a charge written late — and a launch that does run clears the charge with
	// the process it replaces anyway (letGo).
	return errors.Join(m.persist(ctx), m.relaunchDeath(ctx, ended, uncharged, time.Time{}))
}

// relaunchDeath is the relaunch a death of the claim's process raises, ended being how that
// process ended (endDied): the same session after the death's one launch failure
// (relaunchAfterFailure), or, for an end endUncharged charges nothing (uncharged), a launch
// that charges no launch failure and waits no backoff. died raises it at every report of the
// death, and relaunchRecordedDeath at the boot that finds the death recorded, so the two raise
// one relaunch the same way. turn is the moment the boot reserved for that relaunch
// (RecordedDeath.Turn), and the zero time for died, whose relaunch the gate reserves.
func (m *Machine) relaunchDeath(ctx context.Context, ended string, uncharged bool, turn time.Time) error {
	if uncharged {
		return m.launchAfter(ctx, false, false, false, turn, ended)
	}
	return m.relaunchAfterFailure(ctx, CauseDied, ended, turn)
}

// relaunchRecordedDeath is the RecordedDeath row: a boot finds the claim's record saying the
// process it records died (Claim.DeathCharged), and raises again the relaunch that death raised,
// which the restart lost with the memory it lived in (dispatch://INFRA-5472). It is the relaunch
// the sweep's next report of the death would raise (died's charged branch), raised without
// waiting for that report. What the death ended was recorded, its task taken back and its
// session stopped by the daemon that judged it, and its charge is written: so this charges no
// death, takes nothing back and stops nothing, and the report that comes later decides nothing
// while the relaunch waits (judge, launchWaits), or is fenced off once it has run (fence).
//
// It raises nothing over a claim whose record moved since the boot read it: a launch started the
// next generation, or a report found the process running, which clears the charge
// (reportedProcess). Nor over one that already holds a launch (launchMet), which is a report of
// the death decided first, nor over a suspension held for the claim, which the report of the
// death ends where it lands (died, endHeld): run here, a launch the burst lets straight out would
// let the process go with the hold (letGo), and the claim would run where it should be suspended.
// The turn such a replay carries is left spent, as a dropped wake leaves its token spent — but
// for a relaunch a report parked at a later moment than that turn, which moves to the turn
// (takeEarlierTurn): a report that came after the boot's pass reserved the turn parks its own
// moment behind every turn the pass handed out, and the relaunch would otherwise lose its owed
// place. Moving a wake that already waits launches nothing a hold would forbid that the wake
// would not launch at its own moment, so the move asks nothing of a hold.
//
// It writes one line for each claim whose relaunch it raises or moves, which is what the boot's
// count of recorded deaths is read against.
func relaunchRecordedDeath(m *Machine, ctx context.Context, ev Event) error {
	recorded := ev.(RecordedDeath)
	relaunching := func() {
		m.log.Warn("supervise: relaunching a process the previous daemon found dead; its relaunch did not survive the restart",
			"incarnation", m.claim.Locator.Incarnation, "generation", m.claim.Generation, "lastDeathCause", m.claim.LastDeathCause)
	}
	switch {
	case m.claim.Generation != recorded.Generation || !m.claim.DeathCharged || m.claim.Locator == nil:
		m.log.Info("supervise: the claim's record moved on from the death the boot read; nothing is relaunched for it",
			"generation", m.claim.Generation, "read", recorded.Generation, "deathCharged", m.claim.DeathCharged)
		return nil
	case m.takeEarlierTurn(recorded.Turn):
		relaunching()
		return nil
	case m.launchMet() || m.held != nil:
		m.log.Info("supervise: the recorded death already has its relaunch, or a held suspension that its report ends; the boot raises nothing",
			"launchWaiting", m.launchMet(), "heldSuspension", m.held != nil)
		return nil
	}
	relaunching()
	return m.relaunchDeath(ctx, endDied(m.claim.LastDeathCause), m.endUncharged("died"), recorded.Turn)
}

// endUncharged is whether the end of the claim's process is charged nothing, and it logs why
// where it is; ended is how the process ended, which the line names. died and
// registrationDeadline ask it once they have done what they do for any end, and relaunch an end
// it answers true for through launch, which charges no launch failure and waits no backoff, in
// place of relaunchAfterFailure. Both such ends are of a claim that records a session and no file,
// at the generation of a process that never registered as that session (dispatch://INFRA-4108):
//
//   - The roll edge (R1): at this generation the fence refused the registration of a process an
//     earlier release started (refusedEarlierRelease, markRollEdge).
//   - A start that had Oh My Pi resume the session by its id itself (R3): the claim's mark names
//     this generation (Claim.AgentResumeGeneration), and the start ended before it registered as
//     the session, or its registration deadline passed, a registration as another session that
//     the fence refused included. Through a launcher that cannot look the session up, nothing
//     else can resume it, so the next try forgets it, charged nothing either (resumeByID).
//
// A claim whose agent registered as the session records its file (register), so the end of an
// agent that registered and never said it was ready is charged as every such end is. Each mark
// names one generation, and the relaunch moves the claim past it.
func (m *Machine) endUncharged(ended string) bool {
	g := m.claim.Generation
	switch {
	case g == 0 || m.claim.SessionFile != "":
		return false
	case g == m.refusedEarlierRelease:
		m.log.Info("supervise: the process an earlier release started, whose registration was refused, ended; its end is charged nothing",
			"session", m.claim.Session, "generation", g, "ended", ended)
		return true
	case g == m.claim.AgentResumeGeneration:
		m.log.Warn("supervise: a start that had Oh My Pi resume the recorded session by its id did not register; its end is charged nothing, and the next try forgets the session",
			"session", m.claim.Session, "generation", g, "ended", ended)
		return true
	}
	return false
}

// deathCause is what the claim records of a death: the runtime's own account of it, behind the
// verdict it reached, so a cause reads as a sentence wherever it is repeated. A runtime that gave
// no detail leaves the verdict alone. The account is a pod's events and the last lines of its log,
// so it is kept as a far side's text (storetext.FarSideText) before the claim, the state, the
// store and ended work repeat it. It keeps the runtime's own bound rather than taking
// FarSideError's: that bound keeps the last lines, which are what name the death.
func deathCause(observation runtime.Observation) string {
	if observation.Detail == "" {
		return string(observation.Kind)
	}
	return storetext.FarSideText(fmt.Sprintf("%s: %s", observation.Kind, observation.Detail))
}

// AbandonedSession is a session a claim stopped resuming: the id and transcript path it recorded
// for it, why it was left behind, and when. Why is the sentence whoever abandoned it wrote — the
// failure of a retried claim whose provider refused the session's last turns, the reason an
// operator's retry gave, the runtime's account of a session no store holds — and it is what the
// agent that replaces the session is told, so it reads the daemon's own record of what happened
// rather than a sentence invented for the prompt.
type AbandonedSession struct {
	Session     string    `json:"session"`
	SessionFile string    `json:"sessionFile,omitempty"`
	Why         string    `json:"why"`
	At          time.Time `json:"at"`
	// Told is whether the agent that replaced the session has been told of it, so it is told in
	// the first task it actually takes and in no later one, and ToldBy is the delivery whose
	// confirmation told it, as confirmCarrying states. A sentence told twice is the harmless way
	// round, where losing it strands a worker reading an empty transcript as a fresh start
	// (dispatch://INFRA-714).
	Told   bool   `json:"told,omitempty"`
	ToldBy string `json:"toldBy,omitempty"`
	// Released is when an operator's release freed the names of this session (Machine.ReleaseNames),
	// zero while the table holds them: the boot holds no name of a released session again (store's
	// recordedNames), and the record of where the session went stays.
	Released time.Time `json:"released,omitzero"`
	// ReadyNamed is whether the line that names the project controller's fresh session beside
	// this one has been written, at that session's first ready (the table's ready,
	// dispatch://INFRA-2045 R4), so the line is written once for each replacement.
	ReadyNamed bool `json:"readyNamed,omitempty"`
}

// Tells is whether a prompt to this claim's agent must still name the session the claim
// abandoned: one was abandoned and it has not been told yet.
func (a AbandonedSession) Tells() bool { return a.Session != "" && !a.Told }

// sessionGone tells the daemon to stop the Envoy session this claim recorded, now that no process
// stands behind it. Three decisions tell it, each only once the process has ended: a death a probe
// judged (died), before the take-back; a suspension over a process a report found gone
// (suspendOverADeadProcess), once its take-back has landed; and a suspension whose stop ended a
// live process (stopForSuspension, for suspendNow, letProcessGoFor and exit), once the runtime's
// stop has returned, since both runtimes answer only when the process has ended. A decision that
// has already sent it tells letProcessGoFor so (stopSent), so the death or the found-gone process
// it ends is one stop. Nothing else tells it: a failure (fail), a worker's exit (release, retire)
// and the registration deadline's retirement end a process and send no stop. A death whose
// relaunch the wake pacing parked keeps the dead process's locator until the wake's turn
// (parkWake), so a suspension that arrives meanwhile stops that process again and sends a second
// stop. The first runs on a goroutine nothing in the decision waits for (Deps.SessionGone), so it
// may still be in flight or may have failed, and the second can find the dead process's
// registration still there. The second is told, as every stop is, once the process has ended,
// and before any relaunch has registered, since the relaunch was still parked when the
// suspension came. The delete's two conditions then guard a relaunch's registration as they do
// for any stop: cleanSessionStop (internal/daemon/daemon.go) says what they keep.
//
// The daemon dates the stop when it is told, and the listener keeps a registration written after
// that moment. Told after the process has ended, the stop comes after the process's last
// heartbeat, so the listener deletes the registration rather than reading the session live for
// the rest of its window, and a message to a released role finds the session gone and raises the
// wake that brings it back. It is sent whether or not the write that follows lands, because the
// process has ended either way. A stop the runtime refused, of a process no report found gone,
// sends nothing: that process may still run, and deleting its registration would leave a live
// agent unreachable until its next heartbeat wrote it again.
//
// It reads the session id from the claim before anything clears it (forgetSession), and a claim
// that recorded none — never registered, or already forgotten — has nothing to stop. A relaunch
// of the claim registers the same session again — in the same decision after a death, or at a
// later wake after a suspension — which is the state the listener should then read, so the stop
// says what is true when it is sent.
//
// A claim that records a session it does not hold sends no stop (holdRecorded, plan E's t-e-54).
// The stop deletes the registration the listener holds for the session id, whichever process
// wrote it last (cleanSessionStop reads its client instance id first), and where another claim
// holds the id its live process can be that writer, as after a rollback: the stop of this claim's
// ended process would delete the registration of a process it never ran, and that claim's role
// would read gone until the next heartbeat. The boot suspends such a claim (the merge of
// cybermonklabs/legion#4088 into #4220 made that suspension send a stop), so the stop is held as
// a publish to the claim's role is (outbox.publish). A store that does not answer sends nothing
// either, since whose registration the id names is then unknown; the registration lapses when its
// record expires, the wait the session had before the stop existed. A claim holding its session
// is stopped as before (R9): with names this machine has admitted already (Machine.heldNames) it
// asks nothing of the store, and with names it has not, as after a restart, which NewMachine
// starts with none of, it asks the store once, and a store that does not answer then sends no
// stop for it either. That ask runs on a context the decision's end does not cancel, bounded by
// the store write's own timeout (Machine.write): the daemon's stop must not cut a clean stop
// short (sessionStop.send says why), and a hold cancelled mid-ask would send none.
func (m *Machine) sessionGone(ctx context.Context) {
	if m.deps.SessionGone == nil || m.claim.Session == "" {
		return
	}
	if err := m.holdRecorded(context.WithoutCancel(ctx), m.claim.Session, m.claim.SessionFile); err != nil {
		if errors.Is(err, errHoldUnanswered) {
			m.log.Warn("supervise: no clean stop of the Envoy session: the store did not answer whether the claim holds it",
				"session", m.claim.Session, "error", err)
			return
		}
		m.log.Info("supervise: no clean stop of the Envoy session of a claim that does not hold it",
			"session", m.claim.Session, "error", err)
		return
	}
	m.deps.SessionGone(m.claim.Session)
}

// forgetSession drops the session the claim recorded, leaving the claim as one no agent has
// registered for: the next launch is a fresh spawn that provisions its workspace anew. The turns
// reported for that session go with it (freshRun): the next agent is one the provider has refused
// nothing of. The session is recorded as abandoned, with why it was, since nothing else keeps
// where it went and the fresh agent is told of it in its first task.
//
// It is the one place a claim leaves its session behind, so every start, retry or resume of a
// claim relaunches the session the claim records — by its file, or by its id where it records no
// file (start) — unless that launch forgets the session here, and a claim starts in a new session
// only after this ran. It has four callers: the launch's startFresh, for a session no store holds
// any more, one the pod's launcher could not look up and Oh My Pi did not resume by its id, and
// one a Claude Code claim records with no transcript file; the launch's replaceEndedSession, for
// the project controller's session whose launches keep ending before it finishes a turn
// (SessionRun); retry, when freshSession says the retry needs a new session; and the forget of a
// recorded session (forgetSession, the table's): the operator's forget-session, which `legion
// claims retry --fresh-session` and the controller's fresh_session run (api's freshSession), and
// the controller keeper's replacement of a session older than its age (internal/daemon's
// controllerKeeper). It clears the claim's mark of a start by Oh My Pi's own lookup with the
// session (Claim.AgentResumeGeneration), and, where it drops a session, the cause of an earlier
// forget the machine keeps for the fresh agent's launches; Machine.lostCause says why one that
// drops nothing keeps it. startFresh sets the cause of its own forget after this call.
//
// A fresh session is not told the old session's record of what the last let-go of its process
// ended (EndedWork), and this is the one place that rule is written. When it records the
// abandoned session, it ends that record too: a record that still tells is marked told, by no
// delivery (ToldBy empty), so no take-back of a confirmation ever untells it (moveTelling). The
// abandoned session's sentence (abandonedTask) already says that the turns, tool calls,
// subagents and background jobs of the old session are gone, and the fresh agent started none of
// them, so the record's sentence would tell it that work it never had has stopped. A record that
// tells beside an abandoned session that tells is therefore always one of the fresh session's own
// processes, and each reader tells both: the prompt's composer (Delivery.message), the delivery
// of the record alone (queueRelaunch), and the ready's sentence for an architect's catch-up
// (the daemon's applyTerminal). A delivery of the record alone that the claim still holds has
// nothing left to say, and its next send retires it (sendPending).
//
// Every caller runs it after the old process's record is written, so no let-go writes a record of
// that process later: the launch's startFresh and replaceEndedSession run after the launch let the
// old process go (launchNow, letGo), and retry's fresh session and the table's forget-session
// reach only a claim that runs nothing — queued, suspended, failed or retired — whose process the
// suspension, the failure or the retirement already let go (suspended, fail, retire), or that
// never had one. A let-go records nothing for a claim that records no process (recordEnd), so the
// next record written is of the fresh session's own process.
//
// It puts the claim's run of provider-error turns back to zero as well (Claim.ProviderErrorRun).
// The run counts the turns one session had refused in a row, and the new session has had none,
// so the project's controller can be told again if the new one goes the same way — which is what
// makes a fresh session a recovery the controller can repeat (api's freshSession). Without it a
// claim that was told once would never be told again until a turn of it ended with no error at
// all. The session's run of ended launches goes with it too (SessionRun): the new session has
// ended no launch and finished no turn.
//
// why is the sentence the abandoned session is recorded under, which can carry a runtime's or a
// provider's text, so it is kept as a far side's error text is (storetext.FarSideError).
func (m *Machine) forgetSession(why string) {
	if m.claim.Session != "" {
		// The kept cause goes only with a session dropped here (Machine.lostCause).
		m.lostCause = ""
		m.claim.Abandoned = AbandonedSession{
			Session: m.claim.Session, SessionFile: m.claim.SessionFile, Why: storetext.FarSideError(why), At: m.deps.Clock.Now(),
		}
		if e := &m.claim.EndedWork; e.Tells() {
			e.Told, e.ToldBy = true, ""
			m.log.Info("supervise: the claim abandons its session, so the record of what the old session's process ended is not told to the fresh agent; the abandoned session's sentence says that work is gone",
				"session", m.claim.Session, "cause", e.Cause, "ended", e.Generation)
		}
	}
	m.claim.Session, m.claim.SessionFile = "", ""
	// The mark of a start that had Oh My Pi resume the session by its id names a start of the
	// session dropped here, so it goes with it (Claim.AgentResumeGeneration).
	m.claim.AgentResumeGeneration = 0
	// The names the machine knows it holds are of the session it just let go, which an operator's
	// release may free later (ReleaseNames) and another claim then take: kept, they would let this
	// claim register that session again without asking the store. So the cache holds only the
	// names of the session the claim records now (review F2 of #4220's round 4).
	m.heldNames = nil
	m.admitted.Store(nil)
	m.freshRun()
	m.claim.ProviderErrorRun = 0
	m.claim.SessionRun = SessionRun{}
}

// fail puts the claim where nothing relaunches it: its timers stop, its locator goes, and the
// pending delivery stays for whoever decides what happens next — the round it is in stays too,
// so an ask the worker never read is still the round's to end. A process the claim still
// records — found dead, or never stopped — is suspended first, so nothing of it keeps holding
// what it ran on until the tree closes; a suspension that fails is logged, and the claim fails
// all the same.
//
// cause is the word for what failed it, from the site that decided: it is written with the
// sentence and the moment, and the pending cause this call has just consumed is cleared, since
// the episode it named has ended here (Claim.PendingCause). The process it lets go is recorded
// as ended by the failure, under that sentence (endFailed, recordEnd), unless the decision that
// failed it already recorded that process's end — a death's (died).
//
// why is the failure's sentence, which can carry a launcher's or a provider's error text ("launch
// failures ran out: …"), so the claim, the store, the state and the record of what the process
// ended all hold it as a far side's error text is kept (storetext.FarSideError). The line that
// met that error logs it whole (launchTries' "launch failed", logFarSideError).
func (m *Machine) fail(ctx context.Context, cause FailureCause, why string) error {
	why = storetext.FarSideError(why)
	ended := endFailed(why)
	if loc := m.claim.Locator; loc != nil {
		if err := m.endProcess(ctx, ended); err != nil {
			m.log.Error("supervise: could not suspend the failed claim's process", "incarnation", loc.Incarnation, "error", err)
		}
	}
	m.letGo(ctx, ended)
	m.claim.State, m.claim.Failure = StateFailed, why
	m.claim.FailureCause, m.claim.FailedAt = cause, m.deps.Clock.Now()
	m.claim.PendingCause = ""
	m.log.Error("supervise: claim failed", "cause", string(cause), "why", why,
		"launchFailures", m.claim.Budgets.LaunchFailures,
		"deaths", m.claim.Budgets.Deaths, "promptFailures", m.claim.Budgets.PromptFailures,
		"promptRetires", m.claim.Budgets.PromptRetires)
	return m.durableWrite(ctx, StateFailed, m.persist)
}

// farSideErrorInFull is the message of the lines that carry a far side's whole error, in parts,
// where the line that met the error keeps only its start (logFarSideError, storetext.LogWhole).
// Each part names that line's own message as "of".
const farSideErrorInFull = "supervise: a far side's error in full; the line it is of keeps its start"

// logFarSideError writes msg at level with args and then "error": err's text as the claim keeps a
// far side's error text (storetext.FarSideError), so the line holds no more of a relayed command's
// output than the store does and stays within what one log entry takes. It returns that text, so a
// caller that keeps the error keeps the text its line carries. The whole text follows as
// storetext.LogWhole writes it, under farSideErrorInFull rather than msg, each part with "of": msg
// and then args: a reader that counts msg counts one line for each error, however long, and a
// reader of the whole text joins the parts to their line by "of" and args. Nothing of the far
// side's answer is lost, only moved.
func (m *Machine) logFarSideError(ctx context.Context, level slog.Level, msg string, err error, args ...any) string {
	text := err.Error()
	kept := storetext.FarSideError(text)
	args = slices.Clip(args)
	m.log.Log(ctx, level, msg, append(args, "error", kept)...)
	storetext.LogWhole(m.log, farSideErrorInFull, text, append([]any{"of", msg}, args...)...)
	return kept
}

// release asks the runtime to let go of everything it holds for the claim — the process, when one
// runs. It is the one place the machine hands the runtime a claim to end, so the claim is taken
// once, from the claim the machine holds, for the operator's stop, the tree's close, and an exit.
func (m *Machine) release(ctx context.Context) error {
	defer m.stoppingProcess()()
	if err := m.deps.Runtime.Release(ctx, runtime.Known{Claim: m.claim.Token, Locator: m.claim.Locator}); err != nil {
		return fmt.Errorf("release %s: %w", m.claim.Token, err)
	}
	return nil
}

// suspendProcess asks the runtime to stop the claim's process and keep its session — release's
// counterpart for a claim that goes on — and lets the stopped process go, for the next launch of
// the same session to wait out, recording what it ended under cause (letGo). A suspension that
// fails leaves the claim holding its process, and records nothing. A claim whose process is
// retired and relaunched in one decision (chargePrompt, the registration deadline) calls it
// directly; one that does not run again afterwards calls endProcess.
func (m *Machine) suspendProcess(ctx context.Context, cause string) error {
	if err := m.deps.Runtime.Suspend(ctx, *m.claim.Locator); err != nil {
		return err
	}
	m.letGo(ctx, cause)
	return nil
}

// endProcess is suspendProcess for a claim that does not run again afterwards — suspended, failed,
// or a root that exited — so View says the process is being stopped (ClaimView.Stopping) while the
// runtime stops it, and nothing is routed to the session it stops. cause is suspendProcess's.
func (m *Machine) endProcess(ctx context.Context, cause string) error {
	defer m.stoppingProcess()()
	return m.suspendProcess(ctx, cause)
}

// retire ends the claim: nothing of it runs any more and nothing relaunches it. The round it was
// in goes with it (clearUpdate): a retired claim runs nothing on the pod a round is opened over,
// so a round left recorded on it would be read against whatever pod the replacement makes — the
// daemon's sweep would find a round nothing can answer, and tell the issue its work was lost at
// every tick for the rest of the tree's life. The ask that round left owed goes with the round
// (dropsUpdateTask): nothing of this claim will ever answer it, and no send of this daemon
// carries it (sendPending drops it), so an ask left pending would only stand in the stored row
// of a retired claim until the retry's ready dropped it. An ask a turn confirmed is left where
// it is, as it is at every other end of a round: that turn is the worker's, and settle retires
// the delivery once the claim is no longer working.
//
// The stop is one write. The drop is decided before the round's fields go, so the write that
// follows carries the claim exactly as it is ending — retired, in no round — together with the
// delete of the ask it is ending with (retirePending's RetireDelivery, one transaction).
// Dropping first and clearing afterwards stores the retired claim twice, and the first of the
// two is the claim the paragraph above forbids: retired, and still recording its round. A
// crash or a failed write between them would leave exactly that behind.
//
// A retirement records what the process it lets go ended under cause (letGo), and its one write
// above carries the record (persist, or retirePending's transaction), as every let-go's own write
// does (recordEnd). A stop passes its own cause (endStopped), whose comment names every retry
// that relaunches a stopped claim on the session it records, so the turn the stop cut off and the
// background work it ended are told to that session's next turn (relaunchSentence). The tree's
// or the issue's close and the agent's own exit pass noEnd: each ends the claim's work on
// purpose, so a record left on it would tell a later start of the claim that work it ended on
// purpose was lost.
//
// A launch the claim's issue pod's gate holds leaves it as this retirement's at the end of the
// decision, so the start the gate grants next names a retirement as what ended its wait
// (endPodPark, podRetirement).
func (m *Machine) retire(ctx context.Context, cause string) error {
	m.letGo(ctx, cause)
	m.claim.State = StateRetired
	drops := m.dropsUpdateTask()
	if drops {
		m.log.Info("supervise: the claim retires holding its round's ask, which nothing of it will answer; the ask goes with the round",
			"round", m.claim.UpdateRound, "delivery", m.claim.Pending.ID)
	}
	m.clearUpdate()
	if drops {
		if err := m.retirePending(ctx); err != nil {
			return err
		}
	} else if err := m.persist(ctx); err != nil {
		return err
	}
	// Every drop of a delivery answers the question a restart armed for it; a retired claim is
	// answered nothing there (answerAskFirstAfterDrop).
	return m.answerAskFirstAfterDrop(ctx)
}

// letGo ends what the machine had with the claim's process: no timer watches it — the ladder's
// wait after a provider error among them (providererror.go) — no relaunch waits on one
// (relaunchLater), no wait for a pod's update round stands, nor the pod it was for
// (podRoundWaiting, awaitsPod: a suspension, a retirement or a failure of the waiting claim ends
// that wait, and every launch, whatever raised it, keeps the pod through its own try, launchNow),
// no wake of the pacing waits its turn (reserveWake), no launch waits at its issue pod's gate,
// whose start leaves the gate at the end of the decision for what the decision left of the claim
// (endPodPark) — the launch it was to make is the one this end replaces, and the credit of the
// refresh gate a refresh of the stale-plugin lane held for that wake goes back at the end of this
// decision, which is where a credit whose wake is gone goes back wherever that wake went
// (endRefreshLane, dispatch://INFRA-2712 R1) — no send talks to it, no prompt of the pending task
// waits on a
// result from it (Machine.outstanding: the process that would answer is gone, and the next one
// is sent the task anew), no suspension is held for its turn,
// no stale-plugin relaunch waits for that turn either (refreshPlugin) — the process it was to
// replace is gone — no compaction runs against it (Machine.Compact, whose end then finds the
// claim records none and answers ErrCompactProcessGone, giving it back nothing and sending
// nothing: ownsCompaction), and none that a gone verdict dropped with it is kept for this
// record any more (lostCompaction) — and the process the claim records, when it records one,
// moves into previous — the claim no longer runs it (it was stopped, found dead, or left
// behind), and the next launch of the same session waits it out. The identity of the turn that
// process was running goes with it (turnsDelivery): a turn whose process is gone ends by a line
// of its own rather than by turnEnded, so nothing is left to read it, and a later turn must never end
// naming the delivery of a turn that died with its process. So does the record a busy answer
// made of that turn (busyTurn): the question it arms has no process left to answer it, and the
// next process is sent the task anew. What the runtime last reported of that process goes with it
// too, and what was charged for its death with that (processGone, Claim.DeathCharged): the claim
// records no process for a report to be about, and the one the next launch starts has been
// reported nothing and charged nothing. The clear of the charge reaches the store with the write
// every caller of this makes of the claim it leaves — the launch that records the new process,
// the suspension, the retirement, the failure — so no stored charge ever outlives the process it
// was made for.
//
// Before any of that, what the process ended is recorded for cause, the caller's reading of why
// it lets the process go (recordEnd): the turn it was in, whether that turn was the pending
// task's, and the background work its own activity report named. It is read here, first,
// because everything it reads goes with the process below, and the launch that follows clears
// the report. noEnd records nothing. The mark of a task taken back from that process's turn goes
// with the process too (cutTurnOf), and so do the marks of the sentences a prompt to it carried
// (carried): the next process has been sent nothing yet. So does the copy of the news of the
// approved design that a prompt to it carried, on a task no turn confirmed (Delivery.Carried,
// copyTurnNews, which writes none once no prompt is outstanding): no turn of that prompt can start
// any more, so a turn of the next process's own, after a restart, is never taken for it
// (unsentNews). The copy goes in memory here and, where it changed, reaches the store at the next
// write of the delivery, which settle makes before the next decision (unsaved).
func (m *Machine) letGo(ctx context.Context, cause string) {
	m.recordEnd(ctx, cause)
	m.disarmAll()
	m.relaunch, m.podRoundWaiting, m.awaitsPod = nil, false, nil
	m.wake = nil
	m.forgetSend()
	m.forgetOutstanding()
	if p := m.claim.Pending; p != nil && m.copyTurnNews(p) {
		m.unsaved = true
	}
	m.held, m.heldLaunch = nil, false
	m.staleRelaunch = nil
	m.busyTurn = false
	m.turnsDelivery, m.cutTurnOf, m.carried = "", "", sentences{}
	m.compacting, m.lostCompaction = nil, nil
	m.processGone, m.goneOutOfMemory, m.goneCause, m.claim.DeathCharged = false, false, "", false
	if enrollment := ClearEnrollment(&m.claim); enrollment != nil {
		m.revoke(*enrollment)
	}
	m.identity, m.enrollmentSent, m.helloKey = nil, false, helloKey{}
	m.stopRegistration()
	if m.claim.Locator != nil {
		m.previous, m.claim.Locator = m.claim.Locator, nil
	}
}

// judge acts on what a probe or the sweep says about the claim's process. alive is what a live
// process means to the caller; an uncertain one, or a runtime that could not answer, is asked
// again after the given wait on the given timer — and is never read as alive or gone.
//
// Nothing is judged at all while a launch the pacing holds waits over the process the observation
// is about (launchWaits): what happens to that process is already decided, and its turn replaces
// it (wakeNow). A second report of the one death is charged no second death wherever it is
// judged (died), but acted on here it would charge a second launch failure and replace the wake
// with a fresh token at the back of the queue, so a claim with work outstanding would fail
// before launch_failure_limit real failures and wait out another paced turn to do it. The same
// holds for every other verdict: a process found stale, displaced, uncertain or alive says
// nothing the relaunch this wake holds does not already answer.
//
// The report this guard answers is the runtime's sweep (observe), which observes the process
// whatever the claim is doing. The four other paths that report one — a closed connection
// (closed), the boot interval (bootInterval), the probe timer (reprobe) and the registration
// deadline (registrationDeadline) — ask the runtime through probe, whose own guard stops each of
// them inside the wait: no call is made, and nothing arms the probe interval there. So the pair
// covers every path, one report here and four in probe. The guard stays in the judgement rather
// than in the fence because it answers every verdict the sweep can carry — gone, stale,
// displaced, uncertain, alive — where each of them is acted on.
//
// Every report is recorded as what the runtime last reported of the process the claim records
// (reportedProcess), the ones this guard drops included: the guard decides what is acted on,
// not what the daemon last heard of the process. A park a stale address raised is the window
// that turns on it — that verdict found the process alive, so it cleared the record, and the
// process can die under the park — and a claim the refusal at the park's own turn leaves
// stored working over that dead process is told from one in a turn by this record alone
// (Claim.ProcessGone). Recording it changes nothing while the launch waits, since both doors
// read that launch first and a launch that runs clears the record with the process it lets go
// (api's relaunchWaits, launchNow, letGo).
//
// What that record is read for is written where it is applied (Claim.ProcessGone). The separate
// record of the charge that keeps one dead process to one death is durable, so a report that
// clears it is written here, before the guard below: the claim's process is running, the charge
// standing in the store is of a death that is over, and the window this guard drops is one
// where nothing else writes the claim (Claim.DeathCharged, died). A write the store would not
// take ends the report, and what the report would have done goes with it: judge returns above
// the guard and the verdict below it, so the sweep's own work on this observation is not run at
// all — no waiting delivery sent to the live claim it found, no ask whether an unanswered turn
// is over (observe, table's onObservation row), and no death, displacement or repoint charged
// from any other verdict. The next report of the same process repeats nothing of the write: the
// clear is already made in memory, so reportedProcess reports none to write. The write is owed
// where it failed, and the claim's next decision makes it good (owes, repair), as died's is;
// the sweep observes the process again at its next pass, and that report is judged whole.
func (m *Machine) judge(ctx context.Context, observation runtime.Observation, alive func(context.Context) error, again TimerKind, after time.Duration) error {
	if m.reportedProcess(observation) {
		if err := m.persist(ctx); err != nil {
			return err
		}
	}
	if m.launchWaits() {
		m.log.Info("supervise: an observation of the process a waiting launch replaces decides nothing; its turn replaces that process",
			"incarnation", observation.Locator.Incarnation, "observed", string(observation.Kind),
			"detail", observation.Detail, "due", m.wake.at)
		return nil
	}
	switch observation.Kind {
	case runtime.Alive:
		if m.claim.UncertainStreak > 0 {
			m.log.Info("supervise: process alive again", "after", m.claim.UncertainStreak)
			m.claim.UncertainStreak = 0
			if err := m.persist(ctx); err != nil {
				return err
			}
		}
		return alive(ctx)
	case runtime.Gone, runtime.NotRecordedProcess:
		// The process is gone either way the verdict splits, so a compaction running over it
		// loses it here, before anything else is decided (compactionLostProcess): the relaunch
		// below can be one the wake pacing parks, which writes nothing of the claim and so
		// leaves its end nothing else to read. The record of what that process's end cut off
		// still reads the compaction, until the let-go (Machine.lostCompaction, recordEnd).
		m.compactionLostProcess()
		if observation.Displaced {
			return m.displaced(ctx, observation)
		}
		return m.died(ctx, observation)
	case runtime.StaleAddress:
		return m.repoint(ctx, observation)
	case runtime.Uncertain:
		m.claim.UncertainStreak++
		m.log.Warn("supervise: process uncertain", "streak", m.claim.UncertainStreak, "detail", observation.Detail)
		m.arm(again, after, "")
		return m.persist(ctx)
	}
	return fmt.Errorf("supervise: an observation of kind %q", observation.Kind)
}

// reportedProcess records what this report found of the process the claim records, which is
// the one fact that tells a claim stored working over a process no launch is replacing from a
// claim in a turn (processGone, Claim.ProcessGone, which states what that is for). It runs for
// every report the machine judges, so one report is never read against a process an older one
// was about: the fence drops an observation of any other process (Machine.fence), and letGo
// clears this with the process the claim lets go. The verdict is read as the one read of a
// claim's process reads it (processReadOf), so the machine and its callers group the kinds
// one way.
//
// A verdict that found the process gone records, beside that, whether its pod ran out of memory
// (goneOutOfMemory) and the runtime's account of the death (goneCause), for the suspension that takes the dead turn's task back later
// (suspendOverADeadProcess). A verdict that found the process running clears both, the stale
// address among them: that process is alive, holding an address a process launched now is not
// handed (repoint), and the relaunch it raises can be refused at its turn exactly as a death's
// can — read as gone, such a claim would be refused "its process is gone" while its live
// process ran a turn of its own. A runtime that could not answer says nothing either way, so an
// uncertain verdict leaves what the last answer found standing until one comes (judge's
// uncertain verdict, probe).
//
// Such a verdict clears what was charged for that process's death with it (Claim.DeathCharged),
// and reports that it did so, for judge to write: the process is running, so the death a later
// report of it finds is a death of its own. Here and letGo clear both records of a running
// machine, and NormalizeClaim clears the charge of a claim read back recording no process.
func (m *Machine) reportedProcess(observation runtime.Observation) (clearedCharge bool) {
	switch processReadOf(observation.Kind) {
	case ProcessGone:
		m.processGone, m.goneOutOfMemory, m.goneCause = true, observation.OutOfMemory, deathCause(observation)
	case ProcessRuns:
		clearedCharge = m.claim.DeathCharged
		m.processGone, m.goneOutOfMemory, m.goneCause, m.claim.DeathCharged = false, false, "", false
	}
	return clearedCharge
}

// probe asks the runtime about the claim's process now and judges the answer. A claim can reach
// here with no Locator at all: onClosed (table.go) fires on a connection closing, which says
// nothing about whether its claim is still the one that opened it. There is nothing to probe and
// nothing to arm in that case — the claim's own relaunch path, not this probe, decides what
// happens to it next.
//
// A claim whose launch waits its turn over the process this would probe is asked nothing either.
// What becomes of that process is already decided — the turn lets it go and starts the next
// (wakeNow, launchNow) — so every answer the runtime could give decides nothing (judge), and
// the one that reaches no judgement at all, a runtime that cannot answer, would arm the probe
// interval the park disarmed (parkWake) and ask again at every interval of a wait that is tens
// of minutes long in a wave. The callers' own timers are not armed again here for the same
// reason: the launch's turn arms the new process's (armBoot).
func (m *Machine) probe(ctx context.Context, alive func(context.Context) error, again TimerKind, after time.Duration) error {
	if m.claim.Locator == nil {
		m.log.Info("supervise: probed with no process to probe; nothing to do")
		return nil
	}
	if m.launchWaits() {
		m.log.Info("supervise: the process a waiting launch replaces is not probed; its turn replaces that process",
			"incarnation", m.claim.Locator.Incarnation, "due", m.wake.at)
		return nil
	}
	observation, err := m.deps.Runtime.Probe(ctx, *m.claim.Locator)
	if err != nil {
		m.log.Warn("supervise: the runtime could not probe the process; probing again", "error", err)
		m.arm(again, after, "")
		return nil
	}
	return m.judge(ctx, observation, alive, again, after)
}

func nothing(context.Context) error { return nil }

func (m *Machine) armBoot() {
	m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
	m.armRegistration()
}

// armRegistration arms the registration deadline at its base Boot×RegistrationIntervals bound,
// plus the runtime's own ProvisionBound while the claim is still StateLaunching: a pod that has
// not said hello yet may still be provisioning. A claim already StateShimConnected — reached by
// a fresh hello (helloed) or restored there (NewMachine) — gets the base bound alone, the same
// one a tmux launch (ProvisionBound always zero) gets throughout, and so does a claim registered
// (register, or restored there), whose agent has that long to say it is ready.
func (m *Machine) armRegistration() {
	grace := time.Duration(0)
	if m.claim.State == StateLaunching {
		grace = m.deps.Runtime.ProvisionBound()
	}
	m.arm(TimerRegistration, runtime.RegistrationDeadline(m.deps.Timeouts.Boot, m.deps.Timeouts.RegistrationIntervals, grace), "")
}

// arm schedules one timer of a kind for a wait, replacing any of that kind already armed. Its
// event carries the arming's sequence number and generation, so a timer that fires after it was
// replaced, or after a relaunch, is fenced out even if stopping it came too late.
func (m *Machine) arm(kind TimerKind, after time.Duration, deliveryID string) {
	ev := m.nextTimer(kind, deliveryID)
	cancel := m.deps.Clock.AfterFunc(after, func() { m.fire(ev) })
	m.timers[kind] = armed{seq: ev.Seq, deliveryID: deliveryID, cancel: cancel, due: m.deps.Clock.Now().Add(after)}
}

// armAt is arm for a timer due at a moment (Clock.At).
func (m *Machine) armAt(kind TimerKind, at time.Time, deliveryID string) {
	ev := m.nextTimer(kind, deliveryID)
	cancel := m.deps.Clock.At(at, func() { m.fire(ev) })
	m.timers[kind] = armed{seq: ev.Seq, deliveryID: deliveryID, cancel: cancel, due: at}
}

// nextTimer disarms any timer of kind and answers the event the next one fires, under the next
// sequence number (arm, armAt).
func (m *Machine) nextTimer(kind TimerKind, deliveryID string) Timer {
	m.disarm(kind)
	m.seq++
	return Timer{Claim: m.claim.Token, Kind: kind, Generation: m.claim.Generation, Seq: m.seq, DeliveryID: deliveryID}
}

// fire hands the machine ev, a timer of its own firing, on the goroutine it fires on: the
// clock's, for a timer the clock holds (arm), and the one the pod gate posts the grant of a launch
// parked at its gate on (askPod, PodStarts.post). That goroutine holds no lock, so its decision
// runs on a fresh context.
func (m *Machine) fire(ev Timer) {
	if err := m.Handle(m.worker(), ev); err != nil {
		m.log.Error("supervise: timer", "timer", string(ev.Kind), "error", err)
	}
}

// worker is the context a goroutine the machine starts runs its decision on: the machine's own,
// through Deps.Worker when the daemon set one, called once per goroutine.
func (m *Machine) worker() context.Context {
	if m.deps.Worker == nil {
		return m.ctx
	}
	return m.deps.Worker(m.ctx)
}

func (m *Machine) disarm(kind TimerKind) {
	if a, ok := m.timers[kind]; ok {
		if a.cancel != nil {
			a.cancel.Stop()
		}
		delete(m.timers, kind)
	}
}

func (m *Machine) disarmAll() {
	for kind := range m.timers {
		m.disarm(kind)
	}
}

// forgetSend drops everything the machine knew about talking to the agent it had: a new process,
// or none, has no memory of the old one's prompts.
func (m *Machine) forgetSend() {
	m.send, m.helloDuringSend, m.askFirst = nil, false, false
}

// write runs one store write of a decision under the claim-write deadline (bounded), derived from
// the decision's own context so the daemon's stop still ends the write at once. Every claim and
// delivery write a decision makes while it holds the machine's lock goes through it but one, and
// those are the only calls that do: ReleaseNames makes ReleaseHolds once under bounded instead,
// since a second try of a release answers the names it freed as none (the last paragraph).
// Store.AdmitClaim and Store.CheckLaunch keep today's behaviour and take no deadline: what they
// wait on is an advisory lock another transaction holds, so a deadline there would fail a launch
// that was only queued behind its predecessor. So do the reads
// of the record — PhaseHolds, RootHears, RootTask, DesignNews, TreeClosable — which decide what
// the next write will say rather than hold the row, and whose failure costs the decision and
// nothing after it. The deadline is what keeps a database that has stopped answering from holding
// the machine's lock for the length of the outage.
//
// A call the deadline cut short after the database was handed it, while the decision's own context
// still runs, is made once more, at once, under a fresh deadline (claimWriteTries, cutByBound): one
// write a loaded database answered past defaultClaimWrite then costs its decision nothing. Before,
// such a write was owed and made again only at the claim's next decision, and the decisions that
// wait on their write came back far later than the database did: a launch whose write as launching
// was cut was tried again at the probe interval (writeTry, relaunchAtProbe), and the boot's release
// of an uncertain launch at the daemon's next orphan sweep (ReleaseUncertainLaunch;
// dispatch://INFRA-3904). A call made again may have been applied the first time, whatever its
// answer said, so each call that comes here must be safe to make twice, each for a reason of its
// own: PutClaim, PutClaimAndDelivery and PauseStopped are the writes the repair itself makes again
// (repair); PutDelivery is an upsert of one row; RetireDelivery reads ErrNoSuchDelivery after a
// landed first try as the retirement made; HoldSessions inserts on conflict do nothing. A new
// caller of write must have such a reason too. The one bounded write that has none is the
// exception to the retry and does not come here: ReleaseHolds answers the names it freed, so
// ReleaseNames makes it once under bounded. A second cut is owed as every write the database did
// not take is (owes, repair). Any other failure is answered at once and is not made again here.
func (m *Machine) write(ctx context.Context, call func(context.Context) error) error {
	for try := 1; ; try++ {
		writing, cancel := m.bounded(ctx)
		err := call(writing)
		cancel()
		if err == nil || try == claimWriteTries || !cutByBound(ctx, err) {
			return err
		}
		m.log.Warn(claimWriteAgainLine, "try", try, "bound", m.deps.claimWrite(), "error", err)
	}
}

// claimWriteTries is how many times write makes one store call the claim-write bound cuts short:
// the call, and once more. One more is what a single slow answer needs; a database that answers
// no write within the bound twice in a row is one the repair waits out, without the machine
// holding its lock through a third bound.
const claimWriteTries = 2

// BegunWriteError is the one contract between the store and write's retry about which errors a
// write may be made again for: an error of a transaction whose BEGIN the database answered, so the
// database was handed the write. The store's error type asserts at compile time that it implements
// this interface (store's begunError), and cutByBound reads nothing else, so the two cannot drift
// apart without the store failing to build.
type BegunWriteError interface {
	error
	WriteBegun() bool
}

// cutByBound is the one place that decides whether write makes a call again: err is a deadline,
// while the decision's own context still runs, on a BegunWriteError. So a dial the bound cut, a
// wait in the pool's queue that ran out its bound (defaultClaimAcquire), and a BEGIN that never
// came back are owed at once: the database was never handed the write, and a second try would
// hold the machine's lock another bound for a database that answers nothing. A connection that
// broke or was refused, a statement refused and a refusal of the store's own carry no deadline and
// are owed or answered at once too. A call the daemon's stop ended carries the stop's
// cancellation, or a deadline once ctx has ended, and is not made again: the daemon is going.
func cutByBound(ctx context.Context, err error) bool {
	var begun BegunWriteError
	return ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) && errors.As(err, &begun) && begun.WriteBegun()
}

// bounded is ctx for one bounded store call of a decision: the wait in the store's pool queue
// bounded by defaultClaimAcquire, and the call itself, from the moment it is handed a connection,
// by the claim-write bound (Deps.BoundWrite, Timeouts.ClaimWrite). With no BoundWrite the whole call
// is bounded by the claim-write bound, which is right for a store that queues for no connection.
func (m *Machine) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	if m.deps.BoundWrite == nil {
		return context.WithTimeout(ctx, m.deps.claimWrite())
	}
	return m.deps.BoundWrite(ctx, defaultClaimAcquire, m.deps.claimWrite())
}

// putClaim writes the claim alone, under the claim-write deadline, and marks nothing owed: a
// caller that rolls its own field back when the write fails must not leave a write owed for a
// state the machine has just put back, since the repair would then write a decision this machine
// has already taken back. Every write first normalizes state that is valid only while the claim
// records a running process: a capability belongs to a registered agent, a suspension source
// belongs to a suspended claim, and an enrollment belongs to the process it names. A confirmation
// and a retire write the claim together with the delivery they change, each in one transaction.
//
// A write that lands settles a write the machine owed (settleOwedWrite): it wrote the claim as
// the machine holds it, which is exactly what the repair would have written, so this write is
// the repair, whichever decision made it. While a project pause's stop is owed, the write is that
// stop's transaction instead (putStops, Deps.PauseStopped), so no write of the claim lands
// without the pause's records.
func (m *Machine) putClaim(ctx context.Context) error {
	c := m.stored()
	// Published here as well as when the decision ends: a launch writes the claim before it waits
	// in the runtime, and View then shows the launch rather than what it replaced.
	m.publish()
	if len(m.owedStops()) > 0 {
		return m.putStops(ctx, c)
	}
	if err := m.write(ctx, func(ctx context.Context) error { return m.deps.Store.PutClaim(ctx, c) }); err != nil {
		return err
	}
	m.settleOwedWrite("PutClaim")
	return nil
}

// putDelivery writes one delivery of this claim under the claim-write deadline, as putClaim
// writes the claim row under it. Every delivery write a decision makes goes through it, and it
// marks nothing: a delivery write that fails has a mark of its own (saved, settle).
func (m *Machine) putDelivery(ctx context.Context, d Delivery) error {
	return m.write(ctx, func(ctx context.Context) error { return m.deps.Store.PutDelivery(ctx, m.claim.Token, d) })
}

// putClaimAndDelivery writes the claim c and the delivery d in the store's one transaction, under
// the same deadline, and settles nothing at all: it writes what it is given, which is not always
// what memory holds. Its callers are queue, whose claim is memory's with the request's outbox
// row appended and whose delivery is not the claim's pending one until the store has taken both
// — what it writes is ahead of memory rather than equal to it, so neither the delivery's mark nor
// the claim write the machine may owe can be read off this write, and queue writes memory itself
// once the store has answered — takeBackPending, whose delivery is the pending one under the id
// it takes only once the store has taken it, beside a claim whose telling that take-back moved
// (moveTelling), so its saved mark is takeBackPending's own; interrupted, whose delivery is the
// task taken back under the id it takes only once the store has taken it, beside a claim whose
// telling a let-go took back (untellInFlight); and putClaimAndPending, which passes memory's own
// claim with memory's own pending delivery and is the call that settles both.
func (m *Machine) putClaimAndDelivery(ctx context.Context, c Claim, d Delivery) error {
	return m.write(ctx, func(ctx context.Context) error { return m.deps.Store.PutClaimAndDelivery(ctx, c, d) })
}

// putClaimAndPending writes the claim as the machine holds it together with the delivery it holds
// pending, in that same one transaction and under the same deadline, and settles what a write of
// exactly those two rows settles. The delivery is left nothing unsaved, because the row the store
// took is the one memory holds (saved, settle), and a claim write the machine owed is made good,
// because the claim the store took is the claim the repair would have written (settleOwedWrite)
// — which is the whole of repair's success path, since the repair it makes is the one this call
// settles. It marks nothing owed: the callers that owe their claim write say so themselves
// (confirmCarrying, repair).
//
// Its five callers are the decisions that write both of memory's own rows: confirmCarrying,
// refused where the confirmation it takes back takes its telling back with it (moveTelling),
// settle's write of a delivery an earlier write left unsaved, repair's branch for a claim that
// holds a pending delivery, and retirePending's write of a project pause's stop it owes before the
// retirement. Each has a pending delivery by the time it calls, which is what this reads. While a
// pause's stop is owed, the two rows go through that stop's transaction instead (putStops,
// Deps.PauseStopped), the delivery carried on the claim it is handed.
//
// It publishes the claim before it writes it, as putClaim does, and publishes what the write
// takes: stored normalizes the machine's live claim, so the publish follows that call and no
// reader sees a claim holding what only a running process may hold (Published, stored,
// NormalizeClaim). A turn's start writes the claim working here and then goes on to write its step
// record, and a reader of the published claim must not read it ready meanwhile (internal/daemon's
// refreshStalePlugins).
func (m *Machine) putClaimAndPending(ctx context.Context) error {
	if len(m.owedStops()) > 0 {
		c, pending := m.stored(), *m.claim.Pending
		c.Pending = &pending
		m.publish()
		if err := m.putStops(ctx, c); err != nil {
			return err
		}
		m.unsaved = false
		return nil
	}
	c, pending := m.stored(), *m.claim.Pending
	m.publish()
	if err := m.putClaimAndDelivery(ctx, c, pending); err != nil {
		return err
	}
	m.unsaved = false
	m.settleOwedWrite("PutClaimAndDelivery")
	return nil
}

// persist is putClaim with the mark, and is what every caller that leaves its decision standing
// in memory uses: the claim the machine holds came from the worker's own frame and nothing puts
// it back, so a write the store did not take is the only thing still missing, and it is owed and
// made again at the claim's next decision rather than lost with the event that failed.
func (m *Machine) persist(ctx context.Context) error {
	method := "PutClaim"
	if len(m.owedStops()) > 0 {
		method = "PauseStopped"
	}
	return m.owes(method, m.putClaim(ctx))
}

// putStep writes the claim's step columns and nothing else (Store.PutClaimStep): what the
// daemon last saw this claim's agent do. It is apart from persist because the record is no part
// of the machine's state — a claim write carries whatever the machine's in-memory claim holds,
// and that claim never carries the steps read since it was loaded.
//
// The one thing it keeps in memory is when this machine last read a step that moved the record's
// time (Machine.steppedAt), which the stall backstop's fence reads: a frame that fence cannot
// see is a frame whose turn the backstop would end for standing still.
//
// A store that no longer holds the claim is said once rather than returned: the frame is a fact
// about an agent, and the claim being gone is no failure of the write.
func (m *Machine) putStep(ctx context.Context, step StepRecord) error {
	if stepped := step.LastStep(); stepped.After(m.steppedAt) {
		m.steppedAt = stepped
	}
	known, err := m.deps.Store.PutClaimStep(ctx, m.claim.Token, step)
	if err != nil {
		return err
	}
	if !known {
		m.log.Warn("supervise: the store holds no claim for a step its agent reported", "tool", step.Tool, "call", step.CallID)
	}
	return nil
}

// openCall is what a tool call's start left for its end to be written with: the end frame
// repeats the tool and the call id and nothing else, so the length and the hash of the
// arguments, and the moment the start was received, live here until that end arrives. The
// arguments' own text is never held — nowhere in the daemon is it.
type openCall struct {
	argsBytes int
	argsHash  string
	startedAt time.Time
}

// openCallsHeld is how many calls of one claim may be open at once before the oldest is let go.
// A turn makes a handful of calls in parallel and ends each of them; what accumulates is a call
// whose end never came — the shim drops the oldest frames when no daemon is attached
// (internal/shim's backlogLimit), and a process killed mid-call ends nothing — so the map is
// bounded rather than left to grow for the life of a claim. Dropping the oldest keeps every
// recent call pairable, which is the only pairing anyone reads.
const openCallsHeld = 64

// holdOpenCall records a call's start until its end arrives, and drops the oldest start held
// when a new call would take this claim past openCallsHeld.
func (m *Machine) holdOpenCall(callID string, open openCall) {
	if m.openCalls == nil {
		m.openCalls = map[string]openCall{}
	}
	if _, held := m.openCalls[callID]; !held && len(m.openCalls) >= openCallsHeld {
		oldest, at := "", time.Time{}
		for id, call := range m.openCalls {
			if oldest == "" || call.startedAt.Before(at) {
				oldest, at = id, call.startedAt
			}
		}
		delete(m.openCalls, oldest)
		m.log.Warn("supervise: more tool calls are open than this claim holds starts for; the oldest is dropped",
			"dropped", oldest, "held", openCallsHeld)
	}
	m.openCalls[callID] = open
}

// takeOpenCall is the start held for callID, and whether one was held. The call is over, so
// nothing holds it after this.
func (m *Machine) takeOpenCall(callID string) (openCall, bool) {
	open, held := m.openCalls[callID]
	delete(m.openCalls, callID)
	return open, held
}

// NormalizeClaim clears state a claim can hold only while it records a compatible running process.
// It returns an enrollment that lost that process so callers can revoke it at the broker, and
// whether it changed anything of c, so a caller that read c from the store writes it back
// (internal/daemon's restore).
func NormalizeClaim(c *Claim) (enrollment *Enrollment, changed bool) {
	if !HoldsCapability(c.State) {
		if len(c.CapabilityHash) != 0 {
			changed = true
		}
		c.CapabilityHash = nil
	}
	if c.State != StateSuspended && c.SuspendedBy != "" {
		c.SuspendedBy, changed = "", true
	}
	if c.Locator == nil {
		// The charge is of the process the claim records, so a claim that records none carries
		// none: whatever the next launch starts has been charged nothing (DeathCharged, letGo).
		if c.DeathCharged {
			c.DeathCharged, changed = false, true
		}
		if enrollment = ClearEnrollment(c); enrollment != nil {
			changed = true
		}
	}
	return enrollment, changed
}

// DropStaleStartFailure clears the start failure of a claim stored ready, working or idle, with
// its time, and answers whether it cleared one. That claim's agent said it was ready
// after the start failed, which ended the failure (table.go's ready), so the sentence is a refusal
// its running agent came after. A daemon of an earlier release keeps it past that ready, and
// several projects' daemons share one database, so a daemon still on that release, or a rollback
// and a roll-forward, writes such a row after migration 20261123212127 cleared the rest. The boot
// asks this of every claim it reads, before it repairs any state (internal/daemon's restore).
func DropStaleStartFailure(c *Claim) bool {
	if !SaidReady(c.State) || c.LastStartFailure == "" {
		return false
	}
	c.LastStartFailure, c.LastStartFailureAt = "", time.Time{}
	return true
}

// ClearEnrollment removes c's enrollment and returns the broker record that must be revoked.
func ClearEnrollment(c *Claim) *Enrollment {
	if c.Enrollment == nil {
		return nil
	}
	enrollment := *c.Enrollment
	c.Enrollment = nil
	return &enrollment
}

// stored normalizes the machine's live claim, revoking any enrollment it detached, then returns the
// form a claim write hands the store. A pending delivery has a table of its own.
func (m *Machine) stored() Claim {
	if enrollment, _ := NormalizeClaim(&m.claim); enrollment != nil {
		m.revoke(*enrollment)
	}
	return StoredClaim(m.claim)
}

// HoldsCapability says whether a claim in state has a registered agent with a running process.
func HoldsCapability(state ClaimState) bool {
	switch state {
	case StateRegistered, StateReady, StateWorking, StateIdle:
		return true
	default:
		return false
	}
}

// SaidReady is whether a claim in state has an agent that said it can be prompted: ready, working
// or idle. A registered claim has not: the daemon records the registration before the agent's
// plugin reports ready, so a publish then still reaches the session that held the role before.
//
// Since INFRA-1573 the ready comes before the Envoy role as well — the plugin claims the role at
// its session's first turn, or at a bound if no turn comes — so a claim this answers true for
// may not answer to its name yet. The listener refuses a keep: false publish to a role no live
// session holds with 404 (notify.ErrNoHolder), and what that costs is the sending row's own, not
// one rule: noticeAnswered, mergeQueue and the merge forward's message each decide it for the rows
// they finish, and hookPrompt publishes with keep, which the listener answers by keeping the
// message for the role's next live holder (internal/daemon).
//
// The window is bounded at both ends: the name is taken inside the session's first turn — the
// daemon's own task starts one, on either harness — and at the first-turn bound (30 s by
// default) for a session that is given no task at all.
func SaidReady(state ClaimState) bool {
	return state == StateReady || state == StateWorking || state == StateIdle
}

func claimOf(ev Event) claim.Token {
	switch ev := ev.(type) {
	case RuntimeObservation:
		return ev.Observation.Locator.Claim
	case StreamHello:
		return ev.Claim
	case StreamTurnStart:
		return ev.Claim
	case StreamTurnEnd:
		return ev.Claim
	case IssueTurnEnded:
		return ev.Claim
	case StreamToolStep:
		return ev.Claim
	case StreamAnswered:
		return ev.Claim
	case StreamPromptResult:
		return ev.Claim
	case StreamClosed:
		return ev.Claim
	case StreamLateRefusal:
		return ev.Claim
	case PromptAcked:
		return ev.Claim
	case PromptRefused:
		return ev.Claim
	case Timer:
		return ev.Claim
	case StalePlugin:
		return ev.Claim
	case RecordedDeath:
		return ev.Claim
	case StallBackstop:
		return ev.Claim
	case TurnPastBound:
		return ev.Claim
	case PodUpdateOpen:
		return ev.Claim
	case PodUpdateEnded:
		return ev.Claim
	case PodUpdateSignalled:
		return ev.Claim
	case RequestSpawn:
		return ev.Claim
	case RequestRegister:
		return ev.Claim
	case RequestReady:
		return ev.Claim
	case RequestSuspend:
		return ev.Claim
	case RequestResume:
		return ev.Claim
	case RequestRuleResume:
		return ev.Claim
	case RequestStop:
		return ev.Claim
	case RequestTreeClose:
		return ev.Claim
	case RequestIssueClose:
		return ev.Claim
	case RequestOperatorClose:
		return ev.Claim
	case RequestRetry:
		return ev.Claim
	case RequestOperatorRetry:
		return ev.Claim
	case RequestForgetSession:
		return ev.Claim
	case RequestDeliver:
		return ev.Claim
	case RequestExit:
		return ev.Claim
	case RequestTurnOutcome:
		return ev.Claim
	case RequestCompact:
		return ev.Claim
	case CompactEnded:
		return ev.Claim
	case ResumePause:
		return ev.Claim
	}
	return ""
}

func copyClaim(c Claim) Claim {
	c.Locator = cloneLocator(c.Locator)
	c.AwaitsPod = cloneLocator(c.AwaitsPod)
	c.RegisteredPods = c.RegisteredPods.clone()
	if c.Enrollment != nil {
		e := *c.Enrollment
		c.Enrollment = &e
	}
	if c.Pending != nil {
		d := *c.Pending
		c.Pending = &d
	}
	c.BootTokenHash = bytes.Clone(c.BootTokenHash)
	c.CapabilityHash = bytes.Clone(c.CapabilityHash)
	c.TurnOutcomes = slices.Clone(c.TurnOutcomes)
	c.EndedWork.Kinds = slices.Clone(c.EndedWork.Kinds)
	c.SessionRun.Endings = slices.Clone(c.SessionRun.Endings)
	return c
}

// cloneLocator is a new pointer to a copy of the locator loc points to, and nil for nil, so a
// claim handed out of the machine shares no Locator value with the one the machine holds.
func cloneLocator(loc *runtime.Locator) *runtime.Locator {
	if loc == nil {
		return nil
	}
	copied := *loc
	return &copied
}

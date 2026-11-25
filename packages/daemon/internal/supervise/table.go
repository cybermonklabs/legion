package supervise

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The events. Each carries the claim it is for and the fields its fences read: an observation its
// incarnation, a hello its generation, a replayed turn start its delivery id, a prompt outcome its
// generation and delivery id, a timer its arming, an agent's own request its generation and
// session.

// RuntimeObservation is a process fact from the runtime's sweep. The claim is its locator's.
type RuntimeObservation struct{ Observation runtime.Observation }

// StreamHello is the shim connecting with this claim's boot token, resolved to its generation.
type StreamHello struct {
	Claim        claim.Token
	Generation   uint64
	AgentSecrets *AgentSecretsIdentity
}

// StreamTurnStart is the agent's turn starting. Oh My Pi's own agent_start carries no delivery id;
// only the shim's replay of one, answering a repeated prompt, does.
type StreamTurnStart struct {
	Claim      claim.Token
	DeliveryID string
}

// StreamTurnEnd is the agent's turn ending.
type StreamTurnEnd struct{ Claim claim.Token }

// IssueTurnEnded is a turn ending on another claim of this claim's issue, posted by the daemon to
// every other claim of the issue once that turn's own machine has recorded its end. The roles that
// work one working copy share one jj workspace, so a phase task this claim is holding rather than
// adopt a working copy under an open turn there (sendPending) may go now. It carries no fact of
// its own — the send asks again which claims are in a turn in that working copy — so one posted
// for a turn another role began meanwhile, or for a turn in another working copy, simply holds
// again or changes nothing.
type IssueTurnEnded struct{ Claim claim.Token }

// StreamToolStep is one tool call of the agent's turn beginning or ending, as the stream read
// the frame (stream.ToolStep): the only fact on the wire that says an agent mid-turn is still
// moving. It is what the claim's step record is built from (toolStepped), and it is no part of
// the machine's state: the record it writes goes through the store's own narrow call, never
// through a claim write.
//
// ArgsBytes is the length of the call's arguments as the start frame carried them and ArgsHash
// their fingerprint, both measured where the frame was read (stream.ToolStep), and both zero for
// an end, which carries no arguments. Their text reaches no queue of the daemon's and no record
// of it: it routinely holds a credential, and the record reaches every surface an operator reads
// (StepRecord). CallID is what pairs an end with its start, and the only thing that does; a
// frame that carries none is a step with no call to name (the Claude Code bridge writes one).
//
// ConnSequence is the registration sequence of the connection the frame arrived on
// (runtime.Conn.Sequence), as StreamLateRefusal carries it. A frame that arrived on a connection
// the claim has since replaced is evidence of what the agent was doing and none at all of what
// it is doing now, so it records its call and does not move the claim's last-step time.
type StreamToolStep struct {
	Claim        claim.Token
	Tool         string
	CallID       string
	ArgsBytes    int
	ArgsHash     string
	End          bool
	ConnSequence uint64
}

// StreamAnswered is the agent's model answering, as the stream read the frame (stream.Answered):
// Model is `<provider>/<model>`, or the model alone when the frame named no provider, or empty for
// an answer whose model no store can keep (stream.Answered). It is what the claim's last answer is
// built from (answered, LastAnswer), and it is no part of the claim the store holds: a record of a
// step reads it from memory, and a restarted daemon knows no model until the agent answers again.
// Generation is the launch whose connection read the frame (stream.Answered).
type StreamAnswered struct {
	Claim      claim.Token
	Model      string
	Generation uint64
}

// StreamPromptResult is Oh My Pi saying how one prompt's work ended (shimwire.PromptResult): the
// one frame that names the prompt a turn belonged to, where agent_start and agent_end name none.
// It arrives on the heels of the agent_end of the turn the prompt ran, so a turn that ends with
// the prompt's result still to come is a turn the prompt did not run: a background result's own,
// which is what wakes a settled session with background work on.
//
// AgentInvoked is false for a prompt that ran no turn at all — one a slash command or an
// extension answered locally — which is the definitive form of what the wait for the turn
// (TimerTurn) otherwise only times out on.
//
// Error is Oh My Pi's message for the failure, and Retryable, HTTPStatus, Provider and Model the
// rest of its error object: Oh My Pi's own word for a failure that may succeed on a later try,
// the status of the provider request that failed (zero when it had none, as a content-filter
// refusal does), and the provider and the model that request went to. The message is prose of
// the provider's and is never parsed, and HTTPStatus alone decides what such a failure means
// (providerErrored); the other three are logged, and the model is named in the controller's
// notice.
type StreamPromptResult struct {
	Claim        claim.Token
	DeliveryID   string
	AgentInvoked bool
	Error        string
	Retryable    bool
	HTTPStatus   int
	Provider     string
	Model        string
}

// StreamClosed is the claim's connection closing. It says nothing about the process by itself.
type StreamClosed struct{ Claim claim.Token }

// StreamLateRefusal is Oh My Pi refusing a prompt after it acknowledged it: a failed response for
// the same request, following the successful one.
type StreamLateRefusal struct {
	Claim      claim.Token
	DeliveryID string
	Error      string
	// Replayed is a refusal the connection did not send the prompt for: the shim's backlog
	// replaying what Oh My Pi answered while no daemon was connected. It answers an earlier prompt,
	// possibly of the very delivery this connection is re-sending, so it charges nothing, and it
	// clears the mark that prompt set. It takes the task back only when a turn not its own confirmed
	// it and the task was not sent through the connection it arrived on or a newer one
	// (takesBackConfirmed).
	Replayed bool
	// ConnSequence is the registration sequence of the connection the refusal arrived on
	// (runtime.Conn.Sequence).
	ConnSequence uint64
}

// PromptAcked is a send's prompt acknowledged, posted by the send's own goroutine. What that prompt
// carried of news of the approved design is the pending delivery's own news, read where the
// acknowledgement is decided (acked): the fence passes only an acknowledgement of the pending id
// (fenceOutcome), and every rendering of the news takes a new id (currentNews, currentRootTask).
type PromptAcked struct {
	Claim      claim.Token
	Generation uint64
	DeliveryID string
}

// PromptRefused is a send's prompt refused, or lost to the transport, posted by the send's own
// goroutine.
type PromptRefused struct {
	Claim      claim.Token
	Generation uint64
	DeliveryID string
	Err        error
}

// TimerKind names the waits a machine times.
type TimerKind string

const (
	// TimerBoot is the boot observation interval: an unregistered process is probed.
	TimerBoot TimerKind = "boot"
	// TimerRegistration is the registration deadline, which runs until the agent is ready: a live
	// process whose agent never registered, or registered and never said it was ready, is retired.
	TimerRegistration TimerKind = "registration"
	// TimerTurn is the wait for the turn an acknowledged prompt should start.
	TimerTurn TimerKind = "turn"
	// TimerProbe is the re-probe of an uncertain process.
	TimerProbe TimerKind = "probe"
	// TimerSuspend is the held suspension's timer (holdSuspension): the stop timeout of its wait for
	// the agent's turn, and the retry of a stop that failed.
	TimerSuspend TimerKind = "suspend"
	// TimerRelaunch is the retry of a relaunch left waiting with no process (relaunchLater): one whose
	// lifecycle check failed with no cleanup reserved, or a launch whose write failed. The claim waits
	// launching with no process until it fires.
	TimerRelaunch TimerKind = "relaunch"
	// TimerWake is the turn the wake pacing gave a launch it held back (reserveWake): the launch
	// runs when it fires (wakeNow). A claim with no process of its own waits launching with none
	// until then; one whose process still runs waits exactly as the wake found it.
	TimerWake TimerKind = "wake"
	// TimerPromptRetry is the wait before a task a provider error took back is sent to the same
	// session again (promptRetryDelay), armed on that task's own delivery id.
	TimerPromptRetry TimerKind = "prompt_retry"
	// TimerPause is the gate's own stop of a claim whose launch it held while the project is paused
	// (pauseGate, pauseStopDue): armed at once by the decision that held the launch, so the stop is
	// the claim's next decision, and again at the probe interval when that stop fails. A claim with
	// no process of its own waits launching until it fires; one whose process runs waits as the
	// gate found it.
	TimerPause TimerKind = "pause"
	// TimerPodStart is the grant of its issue pod's start to a launch parked at the pod's gate
	// (askPod): no wait of the clock's, but the gate's grant, posted to the machine as a timer's
	// firing is (podHold.notify, fire). The launch asks the runtime for its process when it fires
	// (podStartDue). The claim waits launching with no process until then, and whatever drops the
	// parked launch's wake takes its start out of the gate at the end of that decision, naming
	// that end, and disarms it (endPodPark).
	TimerPodStart TimerKind = "pod_start"
)

// Timer is a timer the machine armed, firing.
type Timer struct {
	Claim      claim.Token
	Kind       TimerKind
	Generation uint64
	Seq        uint64
	DeliveryID string
}

// StalePlugin is a re-adopted claim found still speaking a plugin contract this daemon does not,
// posted once at boot for every restored claim in that state (refreshStalePlugins in
// internal/daemon), before any of them is relaunched, and again at every orphan sweep for a claim
// still live whose contract still differs: the sweep is the one periodic pass that already reads
// every claim, so a relaunch the tree lifecycle or the store refuses — which leaves the claim
// live, its contract unchanged, and nothing else re-tried — rides that same pass rather than a
// timer of its own; without the sweep's re-post the claim would stay stale for the rest of the
// daemon's life. Trigger says which pass posted it — "boot" or "sweep" — for the log line the
// relaunch writes; a claim still launching is left to its own registration, which decides the
// comparison the moment it arrives.
type StalePlugin struct {
	Claim   claim.Token
	Trigger string
	// Credit is the credit of the stale-plugin refresh gate the pass that posted this took for
	// this claim, or nil: the pass posts the controller's claim and a claim it read in a turn
	// with none (internal/daemon's refreshStalePlugins). The decision of this post gives the
	// credit back unless the machine takes it as its own (Machine.Handle, settleUntakenCredit,
	// refreshPlugin): a row that ignores the post, a settle or fence that returns first, and a
	// post over a credit already held all give it back as that decision ends. A credit the
	// machine takes goes back at the end of the decision that ends with no refresh wake of this
	// claim's waiting (endRefreshLane), and so at once where its decision raises none.
	Credit *RefreshCredit
}

// RecordedDeath is a restored claim whose stored record says the process it records died: the
// daemon before this one judged that process gone and charged its death (Claim.DeathCharged),
// and the relaunch that death raised lived in that daemon's memory alone, a wake the pacing
// parked, so a restart lost it. The boot raises it once for every such claim, beside the
// unfinished launches it relaunches (internal/daemon's launchUnfinished), so that relaunch enters
// the restart's own wave rather than waiting for the runtime to report the death again
// (dispatch://INFRA-5472). That report needs the pod's launcher to reconnect when the pod lives
// on — a roles container that restarted — which is a probe interval after boot, by which time the
// boot's wave holds the pacing and a relaunch raised then goes in behind all of it. Generation is
// the claim's generation as the boot read it, so a claim a launch has moved on since is decided
// nothing (relaunchRecordedDeath).
//
// Turn is the moment the boot reserved for that relaunch in the wake pacing, as RequestSpawn.Turn
// is for an unfinished launch: the boot reserves the turns of both in one pass, in the order their
// claims' work was owed, so a recorded death waits behind a launch whose work has waited longer.
// The zero time is a replay the gate reserves a turn for itself.
type RecordedDeath struct {
	Claim      claim.Token
	Generation uint64
	Turn       time.Time
}

// StallBackstop is the daemon's stall backstop reaching this claim: the orphan sweep found it in
// a turn whose recorded last step (StepRecord) is older than its role's backstop bound, and the
// daemon ends that turn itself (internal/daemon's backstopStalledTurns, INFRA-1274's R5). The
// sweep posts it at every tick that finds the claim still stalled, so a stop the runtime refuses
// is tried again without a timer of its own, exactly as StalePlugin is re-posted.
//
// Bound is the role's bound (config.Config.StallBackstopFor), which the machine measures the
// claim's own record against before it acts: the sweep read the claims before this decision is
// taken, and a claim whose record moved in between is one whose agent is moving. Deadline is the
// moment the daemon decided it would wait no longer — that record's last step plus Bound — which
// is already past when the sweep posts. The request carries it rather than waiting for the turn
// to end, as a suspension held for a turn does (holdSuspension): the turn this exists for is one
// that never ends. Step is the record the sweep read, the store's own: the machine judges the
// moment from its own two sources (backstopStalledTurn) and reports this record to the daemon
// when it acts, because the claim a machine holds carries the record as it stood when that
// machine was built and the owner's notice must name the call the agent is actually in.
type StallBackstop struct {
	Claim    claim.Token
	Bound    time.Duration
	Deadline time.Time
	Step     StepRecord
}

// PodUpdateOpen opens, or keeps open, an update round over this claim: the process holding it is
// about to be replaced, and the role is asked first to do the steps of its update message
// (UpdateTask) and answer ready_for_update. Round is the round's identity and kind in one string
// (Claim.UpdateRound), and Reasons the drift the daemon read of the pod it runs in.
//
// The daemon's sweep posts it to every claim of the round at every tick while the round is open,
// so the action is idempotent: the first post of a round records the round, its reasons and its
// clock, and each later post of the same round changes nothing. The round's terms are fixed when
// it opens, so the ask a role reads and the record an operator reads name the same reasons. The
// machine asks the role nothing: the sweep enqueues the round's ask to the Envoy topic of this
// claim (record.UpdateAsk.Claim), and the outbox marks it on the claim once it reached a session
// (Machine.PublishUpdateAsk).
type PodUpdateOpen struct {
	Claim   claim.Token
	Round   string
	Reasons string
}

// UpdateTask is the text a pod's update round asks each role with. round is the round, which the
// role answers with. reasons is what the daemon found, as Claim.UpdateReasons holds it. endsAt is
// the moment the round ends at the latest: the round's recorded opening (Claim.UpdateOpenedAt)
// plus pod_update_grace_minutes. The sweep (internal/daemon) builds the text from the round's
// record, so the text names one moment for every role of the round, and the text rides
// record.UpdateAsk to the Envoy topic of the claim it asks. That message can arrive in the middle
// of a turn, so the text tells the role to act now rather than after the turn.
//
// The text names the round's own id, and the role passes it back in its answer
// (PodUpdateSignalled): an answer counts only for the round whose ask the role read, so the id
// in the role's hand is what says which round it is answering. A role that reads this ask while
// a later round has already replaced it names the round it read, and is refused rather than
// counted for the round it never saw (podUpdateSignalled).
//
// An answer from every role brings the round's end forward only while no role reports background
// work: before endsAt the round ends once every role it asks has answered, no role is in a turn,
// and no role with a process in the round's pod has an activity report that names a subagent or a
// background job that runs, or no report since its last turn started. internal/daemon's
// roundEnd.holding is the one statement of that rule. No answer, no turn and no background work
// holds the pod past endsAt, so the text names that moment and what goes with the pod then: the
// turn in progress, each subagent, each background job and each named service that still runs,
// each commit and each file the role did not push, the pod's test database and its caches. A
// named service is in no activity report, so it holds nothing and goes with the pod.
//
// So the text gives five steps, in this order (dispatch://INFRA-4203 R2): push, let each subagent
// and each background job the role started finish or stop it and push what it made, put a scratch
// file needed as evidence on Dispatch, answer, and end the turn. The push comes first, so a job
// that outlives endsAt never keeps a role from pushing its own commits. The answer comes after the
// work, so a role that follows the steps answers with nothing of its own running. A role whose
// work finishes after its answer pushes what that work made and answers again with the same
// round, which podUpdateSignalled records in place of the first answer. The text goes to role, and
// the reviewer creates no commit and pushes nothing (its role part), so its text says so in step 1
// and asks no commit or push of what its work made (createsNoCommit).
//
// The step to answer is the one step a role can be unable to do: the op list its `legion` tool
// holds is a closed enum compiled into its plugin, and a plugin below PodUpdateContract has no
// ready_for_update in it. Such a role is told to do the first three steps all the same, because
// its round ends at endsAt whatever it does.
//
// Only the role's own push keeps its work: the daemon saves nothing of the disk before the pod
// goes, so the text says that work not pushed before the pod goes is lost.
//
// The text is built in pieces, and reasons goes in a piece of its own. A reason is the JSON the
// daemon read of a pod (internal/runtime/sandbox's shapeValue), so a reason can hold a quote
// character. A value put between two constant pieces that both hold that same quote reads as the
// end of a quoted word of the text around it (CodeQL go/unsafe-quoting). The sentence that holds
// reasons has no quote character in it, and the sentences on each side of it are pieces of their
// own.
//
// Another agent reads it, so it follows ASD-STE100: one instruction per sentence, active voice,
// and no word that has a second reading.
func UpdateTask(role claim.Role, round, reasons string, endsAt time.Time) string {
	ends := endsAt.UTC().Format(time.RFC3339)
	push := "1. Push every commit that you want to keep to this issue's branch."
	made := " Then commit and push what it made."
	after := "If a subagent or a background job finishes after you answer, commit and push what it made. Then call `ready_for_update` again"
	if createsNoCommit(role) {
		push = "1. You create no commit, so you push nothing."
		made = ""
		after = "If a subagent or a background job finishes after you answer, call `ready_for_update` again"
	}
	return "The daemon must replace the pod that runs this issue's roles. " +
		fmt.Sprintf("The reasons are: %s.", reasons) +
		fmt.Sprintf(`

The round is %[1]s. The round ends at %[2]s (UTC). When the round ends, the daemon deletes the pod and the disk. The disk holds your workspace. Only your own push keeps your work. Work that you do not push before the pod goes is lost.

If every role answers, the round can end before that moment. It then ends once each role's last report names no subagent and no background job that runs, and no role is in a turn. Your plugin sends that report at the end of each turn. If a role does not answer, the round still ends at %[2]s. A turn in progress does not delay that moment. A subagent or a background job that runs does not delay it either: when the round ends, the pod goes, whatever runs in it.

This message can arrive while you are in a turn. Do these five steps, in this order:

%[3]s
2. Let each subagent and each background job that you started finish. Stop one that cannot finish before %[2]s.%[4]s
3. If you need a scratch file as evidence, put it on Dispatch.
4. Call the `+"`legion`"+` tool with op: "ready_for_update" and round: "%[1]s".
5. End your turn.

%[5]s with round: "%[1]s" before you end that turn.

A service that you started with a name is not in the report. It does not delay the round, and it goes with the pod.

If your `+"`legion`"+` tool has no "ready_for_update" operation, do steps 1, 2 and 3 now. Then end your turn. The round ends at %[2]s whatever you do.

These things go with the pod when the round ends:

- The turn that is in progress.
- Each subagent, each background job and each named service that still runs.
- Each commit and each file that you did not push.
- The pod's test database.
- The pod's caches.

The daemon starts your process again on the new pod, in this same session. The daemon holds your next task until your process runs again. Do not start new work now.`, round, ends, push, made, after)
}

// createsNoCommit says whether role creates no commit and pushes nothing: the reviewer, whose
// handoff is a record on the Dispatch issue (roles/reviewer.md). The update task asks no commit or
// push of it. The merger is not one: it makes and pushes the merge of the base forward in merging
// (roles/merger.md, workflow's mergeForward), and a merge commit it has not pushed when the pod is
// replaced is lost with the pod.
func createsNoCommit(role claim.Role) bool {
	return role == claim.RoleReviewer
}

// PodUpdateEnded is the update round over for this claim: every update_* field goes, the task the
// round held back is released, and nothing is relaunched for it. A pod's round: the pod it was
// opened over is gone — replaced by the round itself, replaced under it, or gone for a failure of
// its own — and each role comes back on its own session through the displaced path. A plugin
// round an earlier daemon left on the claim: no daemon on this release opens one, since the
// stale-plugin lane relaunches a stale process rather than asking its worker anything
// (refreshPlugin), and a claim found in one is taken out of it and left to that lane. A claim in
// no round at all is decided nothing by this event.
type PodUpdateEnded struct {
	Claim claim.Token
	// Round is the round this end is of, as the sweep read it from the claim. An end for any
	// other round decides nothing: the sweep posts from the claims it read at the start of a
	// tick, and a round can be replaced by another (openPodUpdate) or ended by a registration at
	// this daemon's contract (endCurrentPluginRound) before the post is handled, where ending
	// whichever round the claim stands in now would end a round nobody has weighed. Which kind
	// of round this names decides nothing by itself: every round this event finds leaves the
	// claim the same way (endPodUpdate, leaveRound).
	Round string
}

// PodUpdateSignalled is the worker's answer to the round's ask (api's claims/ready-for-update):
// it has pushed everything it wants kept, and the process holding this claim may now be replaced.
// Round is the round the worker answers for, which the ask it read named (UpdateTask): an answer
// counts only for the round the claim stands in, and one naming any other round is refused
// (podUpdateSignalled). Head is the commit it reports as the head of its issue branch at origin,
// "" when the branch is not there — a planner step that runs before the branch's first push — and
// is the worker's word, which nothing verifies. Generation and Session are the claim it says it
// holds, as every agent request carries them.
type PodUpdateSignalled struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Round      string
	Head       string
}

// RequestSpawn launches a queued claim.
type RequestSpawn struct {
	Claim claim.Token
	// Exempt is the caller saying this launch is not one the wake pacing may hold (reserveWake):
	// the daemon's operator API, whose spawn a person is waiting on, and no other caller.
	// The zero value is paced, so a caller that has not thought about it is paced: a flood of
	// launches is the fault the pacing exists to fix, and a wake let through by default would
	// put it back one caller at a time.
	Exempt bool
	// Start is the work a workflow start spawns the claim for, which the spawn judges again under
	// the machine's lock before it launches (StartWork); the zero value is judged by nothing.
	Start StartWork
	// Turn is a moment the caller has already reserved for this launch in the wake pacing, and the
	// zero time for every caller but one: the boot's relaunch of the launches the previous daemon
	// left waiting (internal/daemon's launchUnfinished), which hands the turns out in the order its
	// claims' work was owed before it posts every launch at once, so the queue a restart rebuilds
	// keeps the order the previous daemon was draining (dispatch://INFRA-5482).
	//
	// The rule the gate holds a carried turn to (revive, launchAfter, reserveWake), stated here
	// alone: a turn still to come is the launch's turn, and it parks for it; one the launch reached
	// less than one interval late (WakePace.Interval) is its turn late, and it runs at once; one it
	// reached one interval or more late is reserved a turn afresh, and the spent turn stays
	// charged, as a dropped wake's does (wakepace.Limiter.Reserve), since every launch the caller
	// posted together that reached its gate that late would otherwise run in one instant, outside
	// the rate and the burst. The gate logs the last case. A launch the gate does not reach —
	// refused, or held by a project pause — leaves its turn spent the same way.
	Turn time.Time
}

// RequestRegister is the agent's registration, already authenticated by its boot token: the
// session it became, the transcript it persists, and the hash of the secret it is being issued.
// The machine persists all three before Handle returns.
type RequestRegister struct {
	Claim          claim.Token
	Generation     uint64
	Session        string
	SessionFile    string
	CapabilityHash []byte
	// PluginContract is the contract the registering process's plugin declared, 0 when it
	// declared none.
	PluginContract int
}

// RequestReady is the agent saying it can be prompted.
type RequestReady struct {
	Claim      claim.Token
	Generation uint64
	Session    string
}

// RequestSuspend stops the claim's process and keeps its session. Reason is why, for the line the
// suspension logs; By is who asks, which the suspended claim records (Claim.SuspendedBy) and every
// suspension must name. For a daemon suspension, Deadline, when set, stops a hold at that moment
// when its turn has not ended (holdSuspension); without one, the hold waits for the turn's end. An
// operator suspension always uses the stop timeout from its arrival, whatever Deadline says.
//
// NeverHold is a suspension that may not be held: one a caller posts only because it read the
// claim in no turn, and whose refusal it reports to its own caller as nothing having happened. A
// claim whose state moved under it — into a turn (holdSuspension), or into a launch that has not
// registered (holdSuspendUntilReady) — refuses it with a *RefusedError naming that state, and
// records nothing: no hold, no timer, no write. Without it the hold would stop the agent at the
// turn's end, behind an answer that told the caller its request did nothing (api's freshSession).
// Every other suspension leaves it false and is held as above: the outbox retries its rows until
// the claim answers nil, and `legion claims suspend` waits the hold out.
//
// Pause is the project pause this suspension stops the claim for (store.Pause.ID, decision 4 of
// dispatch://INFRA-3759's plan), 0 for a suspension of no pause. It is set by the daemon's
// `pause_stop` row and by the gate's own stop of a launch it held (pauseStopDue), only with By
// SuspendedByReleased — any other source with a pause, or a negative id, is refused
// (suspendRequest). A suspension that carries one ends in the pause's stop transaction rather than
// the plain claim write (Deps.PauseStopped, suspendedFor), and a hold of one is the pause's: kept
// across a start while its pause is open (StartedBy), and dropped at the start of the claim's next
// decision once that pause has ended (dropEndedPauseHold), the resume's event (ResumePause) among
// them.
type RequestSuspend struct {
	Claim     claim.Token
	Reason    string
	By        SuspendSource
	Deadline  time.Time
	NeverHold bool
	Pause     int64
}

// RequestResume is the operator's resume: it relaunches a suspended claim's session, whoever
// suspended it.
type RequestResume struct{ Claim claim.Token }

// ResumePause is the daemon's word, posted to every machine of its project once a resume of the
// project has committed (decision 22 of dispatch://INFRA-3759's plan), that pause Pause is over.
// The daemon clears its flag (Deps.Paused) before it posts this, so the decision drops a
// suspension the machine holds for that pause before its row runs, as every decision drops the
// hold of a pause that has ended (dropEndedPauseHold): a role in a turn runs on and a launch held
// until its agent is ready goes ahead. The row takes a launch the pause's gate held for that pause
// into the wake pacing — a reservation from the limiter, then its timer — or, where a later pause
// holds the project by then, stops the claim for that one (pauseStopDue); and it sends the task
// the claim holds where it may now go (sendPending), so the daemon's resume needs nothing else to
// hand a role its held task.
// It touches nothing of any other pause: a hold or a held launch a later pause took over carries
// that pause's id, so an event that arrives after the next pause began finds nothing of its own.
// Pause is the ended pause's id (store.Pause.ID), never 0, which is refused.
type ResumePause struct {
	Claim claim.Token
	Pause int64
}

// RequestRuleResume is the daemon's own resume, by rule: it relaunches a suspended claim's session
// unless an operator suspended it (Claim.RuleMayResume): that suspension holds until the operator
// resumes the claim. The daemon asks it of
// a claim that owns its issue's current phase (the outbox's start of that phase, the boot's
// included) and of its own controller. It resumes nothing that is not suspended — a failed claim's
// launch budget is spent, and only the workflow's retry relaunches it — so it is asked again as
// often as the rule meets the claim without ever relaunching one that is running. Reason is the
// rule, for the line it logs.
type RequestRuleResume struct {
	Claim  claim.Token
	Reason string
	// Start is the work a workflow start resumes the claim for, judged as RequestSpawn's is.
	Start StartWork
}

// RequestStop ends one claim. The tree's root claim is not one it can end: a root ends only with
// its tree, so any other stop of it is refused, and suspending it is how its process is stopped.
//
// A close is its own event rather than a flag here, and there are two of them, because who decided
// the close is what says whether TreeClosable is asked. A flag has an unsafe default, and the
// caller that never thought about it gets that default; these two make a new caller name which
// close it is making, and the table holds a row for each.
type RequestStop struct{ Claim claim.Token }

// RequestTreeClose is the workflow closing a tree: every claim of a tree whose linger expired,
// the root's included. It is the workflow's own decision and is not put to TreeClosable — the
// workflow still holds the issue record of the tree it is closing, which is the very record
// TreeClosable refuses on, so asking would refuse exactly the closes it is entitled to make.
type RequestTreeClose struct{ Claim claim.Token }

// RequestIssueClose is the workflow closing a child issue as done (workflow's leave): the claim
// ends as a tree close ends it, keeping its whole session - the id, which the volume reaping reads
// as its evidence, and the resume path. It is not a lost workspace: nothing is recovered, and the
// issue's next run, should a person set it todo again, resumes that session on a pod and a disk of
// its own, or a new one once Machine.forgetSession has dropped it. That start asks for no new
// session itself. Like the tree's close it is the workflow's own decision and is not put to
// TreeClosable.
type RequestIssueClose struct{ Claim claim.Token }

// RequestOperatorClose is the operator closing a tree through the API, through its root claim; the
// tree's other claims are stopped, not closed (api.closeTree). It is put to TreeClosable: an
// operator may close a tree no workflow issue backs, and a tree one does back closes when its
// linger expires, never by hand.
type RequestOperatorClose struct{ Claim claim.Token }

// RequestRetry relaunches a failed or retired claim with fresh budgets. It has two callers: the
// outbox's start of a failed or retired claim (the daemon's outbox), which every start the workflow
// or admission queues for such a claim goes through, and the daemon's controller keeper.
//
// FreshSession asks for a new session in place of the recorded one. It is a field rather than an
// event of its own, as the closes are, because both answers are the same decision made by the same
// caller — the claim runs again — and neither skips a check the other makes: it says only which
// session the retry starts. A retry that asks for none resumes the recorded session, whatever the
// claim's run of refused turns (freshSession).
type RequestRetry struct {
	Claim        claim.Token
	FreshSession bool
	// Start is the work a workflow start retries the claim for, judged as RequestSpawn's is.
	Start StartWork
}

// RequestOperatorRetry is the operator relaunching a claim through the API: the retry RequestRetry
// makes, fresh budgets and the session that retry relaunches, for an architect and for a phase
// worker (operatorRetry). It is its own event, as the closes are, so the table answers each
// caller's retry its own way. FreshSession is `legion claims retry --fresh-session`, as
// RequestRetry takes it.
//
// It reaches a retired claim as well as a failed one. `legion claims stop` retires a claim, and
// while the operator's retry refused that state an operator who stopped a worker could start it
// again only by editing the claims store with the daemon stopped; that is the recovery
// dispatch://INFRA-956 R3 asked to be made of `legion` commands alone. The claim's tree lifecycle
// still decides whether the relaunch runs at all: a tree whose cleanup is reserved refuses the
// launch (revive, Store.AdmitClaim), as it refuses the workflow's own retry of a retired claim.
type RequestOperatorRetry struct {
	Claim        claim.Token
	FreshSession bool
}

// RequestForgetSession drops the named recorded session of a claim that runs nothing — queued,
// suspended, failed, or retired — so that whatever launches it next spawns a fresh agent instead
// of resuming those turns: the operator's own `legion claims forget-session`, and `legion claims
// retry --fresh-session` of a phase worker that failed or that an operator stopped, whose session
// is the operator's to leave behind (api's forgetSession and retryClaim). Session is the session
// the operator means, and a claim recording another is refused, so a decision that changed the
// claim since it was read cannot clear a session nobody asked about.
//
// It launches nothing, which is what makes it its own event rather than another retry: a phase
// worker is relaunched by the workflow's start of its phase, so the two halves of that retry have
// different owners — which session the claim starts on is the operator's to say, and when it
// starts is the workflow's.
type RequestForgetSession struct {
	Claim   claim.Token
	Session string
}

// RequestDeliver gives the claim a task. ID is a durable outbox delivery id when an outbox row
// drives the request; an empty ID asks the machine to mint an ordinary operator delivery id.
//
// Phase is the issue phase the task is for, and says when the task stops being the work to do: a
// task of a phase the issue has left is dropped rather than sent, and a suspension, which ends
// that phase, retires it. The workflow names it; an operator's own delivery names none, is never
// dropped for its issue's phase, and waits out a suspension to be sent on the resume.
type RequestDeliver struct {
	Claim claim.Token
	Task  string
	ID    string
	Phase phase.Phase
	// Generation is the issue generation the task belongs to, and is what a completion reported
	// for it is attributed to. An operator's own task names none.
	Generation uint64
	// Entry is the phase entry the task is of, as the transition raised it on the role's phases
	// row for the start that queued this delivery (record.SuperviseRequest.Entry). A completion
	// is held to it as well as to the run, since every phase entry of one run carries that run. A
	// task of no entry — an operator's own, and every start of no entry
	// (record.SuperviseRequest.Entry, which lists them) — names none, and leaves the entry the
	// claim is serving alone. It is also what decides a repeat: a request whose entry the claim
	// has already been queued a task of (Claim.GivenEntry) queues nothing (queue).
	Entry int64
	// ResendEntry marks a request that hands the role the work of the entry its phases row
	// already names (record.SuperviseRequest.ResendEntry, set by the held phase's retry): the
	// entry test above would decline it, so queue decides such a request by its outbox row
	// alone.
	ResendEntry bool
	// RoundNamed marks a task that ends with the daemon's two sentences of a reviewer's review
	// round: from the start row that queued it (record.SuperviseRequest.RoundNamed), or, for a
	// reviewer's start queued for news alone, which the queue named no round on, from the round
	// the start executor names when it composes that task (the daemon's newsForStart,
	// startNews.roundNamed). An operator's task and the controller's carry none
	// (Delivery.RoundNamed).
	RoundNamed bool
	// CheckLook is the look of a closed issue's production check this task is of, from the start
	// row that queued it (record.SuperviseRequest.CheckLook and CheckOpenedAt). A phase task, an
	// operator's own and a resume name none, and leave the look the claim is serving alone.
	CheckLook CheckLook
	// Row is the outbox row the task comes from (a workflow start), 0 for a task of no row (an
	// operator's own, the controller's start message). It decides a repeat of a request that
	// carries no entry: such a row the claim already queued a task from (Claim.DeliveredRows)
	// queues nothing, its task having been given once.
	Row int64
	// Kept is what the task names of what its tree is owed, as the row recorded it
	// (record.KeptDecisions, encoded JSON), carried onto the delivery and persisted with it, which
	// is what holds those rows until the delivery tells them (Delivery.Kept). Empty for a task
	// that names none.
	Kept []byte
	// Branch is the GitHub branch of the task whose step the start of this task starts, from the
	// start row (record.SuperviseRequest.Branch), "" for the issue's own: the delivery carries it
	// and its adoption takes that task's working copy (Delivery.Branch).
	Branch string
	// News is what the task carries of news of the approved design, as the start that queued it
	// read it from the record (record.DeliveryNews, encoded JSON), carried onto the delivery and
	// persisted with it, since the delivery's confirmation is what raises it as taken
	// (Delivery.News). Empty for a task that carries none.
	News []byte
	// NewsOnly marks a task queued for news of the approved design alone (the daemon's news-only
	// start, record.SuperviseRequest.News). Such a task holds no work of the claim's: it asks the
	// worker to read what changed and to answer it. So it takes the place of no task that holds
	// work (HoldsTask), where any other newer task takes the place of a confirmed task or of one a
	// provider error took back (queue): it is refused (ErrDeliveryPending), and its start waits on
	// its row's backoff until the claim holds no such task. It still takes the place of the
	// daemon's own delivery of a relaunch's record, which holds no work either. Once held, it
	// gives way to a task of an entry while no turn has taken it (Machine.supersedes), since the
	// start of an entry carries the same news itself.
	NewsOnly bool
}

// RequestExit is the agent reporting its own end.
type RequestExit struct {
	Claim      claim.Token
	Generation uint64
	Session    string
	Reason     string
}

// RequestTurnOutcome is the outcome of one turn, reported by the producer that ended it: the
// pi-legion agent_end hook for an Oh My Pi session, the Claude bridge for a Claude Code one, each
// over the claim's own HTTP route. Sequence is that producer's own count of the turns it has
// ended at Generation, from 1, and the pair is the key the claim keeps the report under
// (recordTurnOutcome). HTTPStatus is the status the provider refused the turn with, 0 where the
// producer could not read one, and Unprompted says the turn ran no prompt the daemon sent
// (claim.TurnOutcomeRequest), which is the one kind of turn a report is judged for (turnOutcome).
//
// Neither is fenced. A report naming a generation the claim has left is not stale: it is a turn of
// that generation, which belongs in its own place in the order the run is derived in, and the
// secret the route authenticates is the current registration's either way.
type RequestTurnOutcome struct {
	Claim      claim.Token
	Generation uint64
	Sequence   uint64
	Refused    bool
	Message    string
	HTTPStatus int
	Unprompted bool
}

// RequestCompact takes the claim for a compaction of its Oh My Pi session, which is what a claim
// whose provider keeps refusing its session needs before anything is started again
// (Machine.Compact). It carries no instructions: the ask itself is the caller's, made with no
// lock held, and this is only the moment the claim becomes busy.
type RequestCompact struct{ Claim claim.Token }

// CompactEnded is the compaction over, posted by whoever asked for it (Machine.Compact).
// Generation is the generation the claim was taken at, and an end that names one the claim no
// longer holds the compaction of is answered ErrCompactProcessGone rather than reaching a row
// (Machine.fence, ownsCompaction). Compacted is whether Oh My Pi compacted the session: false
// for a compaction it refused or skipped and for a wait that ran out, each of which gives the
// claim back and sends the owed task again all the same, and only true puts the claim's run of
// provider-error turns back to zero.
type CompactEnded struct {
	Claim      claim.Token
	Generation uint64
	Compacted  bool
}

func (RuntimeObservation) isEvent()   {}
func (StreamHello) isEvent()          {}
func (StreamTurnStart) isEvent()      {}
func (StreamTurnEnd) isEvent()        {}
func (IssueTurnEnded) isEvent()       {}
func (StreamToolStep) isEvent()       {}
func (StreamAnswered) isEvent()       {}
func (StreamPromptResult) isEvent()   {}
func (StreamClosed) isEvent()         {}
func (StreamLateRefusal) isEvent()    {}
func (PromptAcked) isEvent()          {}
func (PromptRefused) isEvent()        {}
func (Timer) isEvent()                {}
func (StalePlugin) isEvent()          {}
func (RecordedDeath) isEvent()        {}
func (StallBackstop) isEvent()        {}
func (PodUpdateOpen) isEvent()        {}
func (PodUpdateEnded) isEvent()       {}
func (PodUpdateSignalled) isEvent()   {}
func (RequestSpawn) isEvent()         {}
func (RequestRegister) isEvent()      {}
func (RequestReady) isEvent()         {}
func (RequestSuspend) isEvent()       {}
func (RequestResume) isEvent()        {}
func (RequestRuleResume) isEvent()    {}
func (RequestStop) isEvent()          {}
func (RequestTreeClose) isEvent()     {}
func (RequestIssueClose) isEvent()    {}
func (RequestOperatorClose) isEvent() {}
func (RequestRetry) isEvent()         {}
func (RequestOperatorRetry) isEvent() {}
func (RequestForgetSession) isEvent() {}
func (RequestDeliver) isEvent()       {}
func (RequestExit) isEvent()          {}
func (RequestTurnOutcome) isEvent()   {}
func (RequestCompact) isEvent()       {}
func (CompactEnded) isEvent()         {}
func (ResumePause) isEvent()          {}

// eventKind is what the table is keyed on beside the state: an event's type, with a Timer split
// by its kind, because each wait means a different thing.
type eventKind string

const (
	onObservation   eventKind = "runtime_observation"
	onHello         eventKind = "stream_hello"
	onTurnStart     eventKind = "stream_turn_start"
	onTurnEnd       eventKind = "stream_turn_end"
	onIssueTurnEnd  eventKind = "issue_turn_ended"
	onToolStep      eventKind = "stream_tool_step"
	onAnswered      eventKind = "stream_answered"
	onPromptResult  eventKind = "stream_prompt_result"
	onClosed        eventKind = "stream_closed"
	onLateRefusal   eventKind = "stream_late_refusal"
	onAcked         eventKind = "prompt_acked"
	onRefused       eventKind = "prompt_refused"
	onBootTimer     eventKind = timerPrefix + eventKind(TimerBoot)
	onDeadline      eventKind = timerPrefix + eventKind(TimerRegistration)
	onTurnTimer     eventKind = timerPrefix + eventKind(TimerTurn)
	onProbeTimer    eventKind = timerPrefix + eventKind(TimerProbe)
	onSuspendTimer  eventKind = timerPrefix + eventKind(TimerSuspend)
	onRelaunchTimer eventKind = timerPrefix + eventKind(TimerRelaunch)
	onWakeTimer     eventKind = timerPrefix + eventKind(TimerWake)
	// The wait a turn that ended in a provider error arms (promptResulted).
	onPromptRetryTimer eventKind = timerPrefix + eventKind(TimerPromptRetry)
	// The gate's own stop of a launch it held while the project is paused (pauseGate).
	onPauseTimer eventKind = timerPrefix + eventKind(TimerPause)
	// The grant of its issue pod's start to a launch parked at the pod's gate (askPod).
	onPodStartTimer eventKind = timerPrefix + eventKind(TimerPodStart)

	onSpawn         eventKind = "request_spawn"
	onRegister      eventKind = "request_register"
	onReady         eventKind = "request_ready"
	onSuspend       eventKind = "request_suspend"
	onResume        eventKind = "request_resume"
	onRuleResume    eventKind = "request_rule_resume"
	onStop          eventKind = "request_stop"
	onTreeClose     eventKind = "request_tree_close"
	onIssueClose    eventKind = "request_issue_close"
	onOperatorClose eventKind = "request_operator_close"
	onRetry         eventKind = "request_retry"
	onOperatorRetry eventKind = "request_operator_retry"
	onForgetSession eventKind = "request_forget_session"
	onDeliver       eventKind = "request_deliver"
	onExit          eventKind = "request_exit"
	onTurnOutcome   eventKind = "request_turn_outcome"
	onCompact       eventKind = "request_compact"

	onCompactEnded eventKind = "compact_ended"

	onStalePlugin   eventKind = "stale_plugin"
	onRecordedDeath eventKind = "recorded_death"
	onStallBackstop eventKind = "stall_backstop"

	onPodUpdateOpen      eventKind = "pod_update_open"
	onPodUpdateEnded     eventKind = "pod_update_ended"
	onPodUpdateSignalled eventKind = "pod_update_signalled"

	onResumePause eventKind = "resume_pause"

	timerPrefix eventKind = "timer:"
)

func kindOf(ev Event) eventKind {
	switch ev := ev.(type) {
	case RuntimeObservation:
		return onObservation
	case StreamHello:
		return onHello
	case StreamTurnStart:
		return onTurnStart
	case StreamTurnEnd:
		return onTurnEnd
	case IssueTurnEnded:
		return onIssueTurnEnd
	case StreamToolStep:
		return onToolStep
	case StreamAnswered:
		return onAnswered
	case StreamPromptResult:
		return onPromptResult
	case StreamClosed:
		return onClosed
	case StreamLateRefusal:
		return onLateRefusal
	case PromptAcked:
		return onAcked
	case PromptRefused:
		return onRefused
	case Timer:
		return timerPrefix + eventKind(ev.Kind)
	case StalePlugin:
		return onStalePlugin
	case RecordedDeath:
		return onRecordedDeath
	case StallBackstop:
		return onStallBackstop
	case PodUpdateOpen:
		return onPodUpdateOpen
	case PodUpdateEnded:
		return onPodUpdateEnded
	case PodUpdateSignalled:
		return onPodUpdateSignalled
	case RequestSpawn:
		return onSpawn
	case RequestRegister:
		return onRegister
	case RequestReady:
		return onReady
	case RequestSuspend:
		return onSuspend
	case RequestResume:
		return onResume
	case RequestRuleResume:
		return onRuleResume
	case RequestStop:
		return onStop
	case RequestTreeClose:
		return onTreeClose
	case RequestIssueClose:
		return onIssueClose
	case RequestOperatorClose:
		return onOperatorClose
	case RequestRetry:
		return onRetry
	case RequestOperatorRetry:
		return onOperatorRetry
	case RequestForgetSession:
		return onForgetSession
	case RequestDeliver:
		return onDeliver
	case RequestExit:
		return onExit
	case RequestTurnOutcome:
		return onTurnOutcome
	case RequestCompact:
		return onCompact
	case CompactEnded:
		return onCompactEnded
	case ResumePause:
		return onResumePause
	}
	return ""
}

// requestName is a request's name as a refusal says it; the other events are not requests.
func requestName(ev Event) (string, bool) {
	switch ev.(type) {
	case RequestSpawn:
		return "spawn", true
	case RequestRegister:
		return "register", true
	case RequestReady:
		return "ready", true
	case RequestSuspend:
		return "suspend", true
	case RequestResume, RequestRuleResume:
		return "resume", true
	case RequestStop:
		return "stop", true
	case RequestTreeClose, RequestOperatorClose, RequestIssueClose:
		return "close", true
	case RequestRetry, RequestOperatorRetry:
		return "retry", true
	case RequestForgetSession:
		return "forget-session", true
	case RequestDeliver:
		return "deliver", true
	case RequestExit:
		return "exit", true
	case RequestTurnOutcome:
		return "turn outcome", true
	case RequestCompact:
		return "compact", true
	case PodUpdateSignalled:
		return "ready_for_update", true
	}
	return "", false
}

type key struct {
	state ClaimState
	kind  eventKind
}

// rule is one cell of the table: a row — what the event does in that state, named, with the states
// it may leave the claim in besides the one it found — or an ignore, with the reason the event
// means nothing there. Handle refuses a request that meets an ignore, and holds every row to its
// destinations.
type rule struct {
	name   string
	to     []ClaimState
	act    func(m *Machine, ctx context.Context, ev Event) error
	ignore string
}

var (
	live        = []ClaimState{StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle}
	booting     = []ClaimState{StateLaunching, StateShimConnected}
	prompted    = []ClaimState{StateReady, StateIdle}
	processless = []ClaimState{StateQueued, StateLaunchUncertain, StateSuspended, StateFailed, StateRetired}
	unready     = []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered}
	// gone is where a claim's process has been let go: suspended, failed or retired. It is the one
	// statement of the set: Claim.Gone reads it in memory, and the store's queries take it as a
	// parameter (GoneStates).
	gone = []ClaimState{StateSuspended, StateFailed, StateRetired}
	// runsNothing is where a claim has no process of its own, running or coming up: queued, for which
	// nothing was launched, or gone. A launch whose outcome is uncertain may still run, so it is not
	// here. It is the one statement of the set: Claim.RunsNothing reads it in memory, and the store's
	// queries take it as a parameter (RunsNothingStates).
	runsNothing = append([]ClaimState{StateQueued}, gone...)
)

// LiveStates are the states of a claim whose process is up or coming up: launched, and not
// suspended, failed or retired. It is a copy, so no caller changes the table's own set.
func LiveStates() []ClaimState {
	return slices.Clone(live)
}

// RunsNothingStates are the states of a claim with no process of its own (runsNothing). It is a
// copy, so no caller changes the table's own set.
func RunsNothingStates() []ClaimState {
	return slices.Clone(runsNothing)
}

// GoneStates are the states of a claim whose process has been let go (gone). It is a copy, so no
// caller changes the table's own set.
func GoneStates() []ClaimState {
	return slices.Clone(gone)
}

// IsGone is whether state is one of GoneStates: a claim in it has had its process let go.
func IsGone(state ClaimState) bool { return slices.Contains(gone, state) }

// FailedOrRetired is whether state is failed or retired: gone, and started again by a retry alone.
func FailedOrRetired(state ClaimState) bool { return state == StateFailed || state == StateRetired }

// TakesCompaction is whether the machine takes a compaction of a claim in this state, read off
// the table's own onCompact rows rather than from a second list of them: the api's refusals
// name the states a compaction reaches, and this is what their test holds those words to
// (api's compactOp, the onCompact rows below). An ignore is no take, so a state the table
// names a reason for reads false here, as does a state the table does not cover at all.
func TakesCompaction(state ClaimState) bool {
	r, ok := table[key{state, onCompact}]
	return ok && r.act != nil
}

const (
	noProcess     = "no process of this claim is running"
	noSend        = "no prompt is sent in this state"
	failedClaim   = "the claim failed; what happens to it next is not the supervisor's decision"
	retiredClaim  = "the claim is retired"
	notRegistered = "the agent has not registered"
)

// table is the transition table: a row or a named ignore for every event kind in every state.
// TestTheTableHasARowOrANamedIgnoreForEveryEventInEveryState reads the sealed Event set, the
// states, and the timer kinds from this package's source and holds the table to all of them.
//
// It is filled in init because its actions reach Handle (a timer they arm calls it), and Handle
// reads the table: as a package-level initializer that is a cycle the compiler refuses.
var table map[key]rule

func init() { table = build(fillTable) }

func fillTable(t *builder) {
	// What the runtime says about the process.
	t.row(onObservation, "judge the process; alive and ready or idle, send what is waiting; working after an unanswered ask or a busy answer, ask whether the turn is over", observe,
		[]ClaimState{StateLaunching, StateFailed, StateWorking, StateIdle}, live...)
	t.ignore(onObservation, noProcess, processless...)

	// The shim.
	t.row(onHello, "the shim connected", helloed, []ClaimState{StateShimConnected}, StateLaunching)
	t.row(onHello, "the shim reconnected before ready: hand it the enrollment if it lacks one", enrolledOnly, nil, StateShimConnected, StateRegistered)
	t.row(onHello, "the shim reconnected: send what is pending", reconnected, []ClaimState{StateWorking}, prompted...)
	t.row(onHello, "the shim reconnected mid-turn: after a restart, ask whether the turn is still running",
		reconnectedMidTurn, []ClaimState{StateIdle, StateWorking}, StateWorking)
	t.ignore(onHello, "no launch has minted a boot token", StateQueued)
	t.ignore(onHello, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onHello, noProcess, gone...)

	t.row(onClosed, "the connection closed: probe the process", closed, []ClaimState{StateLaunching, StateFailed}, live...)
	t.ignore(onClosed, noProcess, processless...)

	// The prompt's own outcome, from the send's goroutine.
	t.row(onAcked, "the prompt was acknowledged: wait for its turn", acked, nil, prompted...)
	t.row(onAcked, "a turn already confirmed the delivery: the prompt is still outstanding until its result", ackedInTurn, nil, StateWorking)
	t.ignore(onAcked, noSend, unready...)
	t.ignore(onAcked, noSend, gone...)

	t.row(onRefused, "the prompt was refused (charged, unless the agent is in a turn of its own, which records the claim working) or lost (not charged): re-queue it", refused,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended, StateWorking}, StateReady, StateIdle, StateWorking)
	t.ignore(onRefused, noSend, unready...)
	t.ignore(onRefused, noSend, gone...)

	t.row(onLateRefusal, "the agent refused an acknowledged prompt (in a turn of its own, which records the claim working)", lateRefused,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended, StateWorking}, StateReady, StateIdle, StateWorking)
	t.row(onLateRefusal, "a prompt was refused while no process can be prompted: the mark it set goes",
		refusedUnprompted, nil, slices.Concat(unready, gone)...)

	// Turns.
	t.row(onTurnStart, "a turn started", turnStarted, []ClaimState{StateWorking}, prompted...)
	t.row(onTurnStart, "a turn is already in flight; the start of a turn a busy answer recorded is decided as any start is, and a turn under a compaction is recorded for its end", turnStartedWhileBusy, nil, StateWorking)
	t.ignore(onTurnStart, "the agent is not ready, so no delivery is in flight", unready...)
	t.ignore(onTurnStart, noProcess, gone...)

	// A turn whose end runs a stale-plugin relaunch held for it (refreshPlugin) ends launching, or
	// failed when that launch spends the claim's budget, as the stale-plugin row itself does.
	t.row(onTurnEnd, "the turn ended", turnEnded, []ClaimState{StateIdle, StateWorking, StateLaunching, StateFailed}, StateWorking)
	// A turn the daemon never saw begin ends here: a claim registered while the agent's own turn
	// was already running is ready rather than working, and the end is the only stream event of
	// that turn it will ever get. The task it holds unread is sent now, which is what left three
	// workers idle holding a task for 35 to 75 minutes (dispatch://INFRA-1691, INFRA-1545,
	// INFRA-722, A9) — a send reached only through a turn the daemon watched begin waits on an
	// event that has already happened.
	t.row(onTurnEnd, "a turn the daemon never saw begin ended: send the task it holds", blindTurnEnded,
		[]ClaimState{StateReady, StateIdle, StateWorking}, prompted...)
	t.ignore(onTurnEnd, "the agent is not ready, so no turn of a delivery is in flight", unready...)
	t.ignore(onTurnEnd, noProcess, gone...)

	t.row(onIssueTurnEnd, "a turn ended elsewhere on the issue: send the task that was waiting for the working copy they share",
		issueTurnEnded, []ClaimState{StateWorking}, prompted...)
	t.ignore(onIssueTurnEnd, "the claim is in a turn of its own; what it holds goes when that turn ends", StateWorking)
	t.ignore(onIssueTurnEnd, noSend, unready...)
	t.ignore(onIssueTurnEnd, noSend, gone...)

	// The calls of a turn, which are the one thing that says an agent mid-turn is still moving.
	// The row writes the claim's step columns through the store's own narrow call and leaves the
	// claim exactly where it found it (toolStepped), so it is a row of every state a process of
	// this claim can send a frame from — a tool call before the agent registers among them, since
	// the stream is up from the shim's hello.
	t.row(onToolStep, "the agent stepped: record the call its stream reports", toolStepped, nil, live...)
	t.ignore(onToolStep, noProcess, processless...)

	// The model that answered, which the record of a step names. The row keeps it in memory and
	// leaves the claim exactly where it found it (answered), so it is a row of every state a
	// process of this claim can send a frame from, as the tool step's is.
	t.row(onAnswered, "the agent's model answered: keep which model it was", answered, nil, live...)
	t.ignore(onAnswered, noProcess, processless...)

	t.row(onPromptResult, "the prompt's work ended: that names the turn it ran, and ends its delivery (busy, it records the claim working)", promptResulted,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended, StateWorking}, StateReady, StateIdle, StateWorking)
	t.ignore(onPromptResult, noSend, unready...)
	t.ignore(onPromptResult, noSend, gone...)

	// Timers.
	t.row(onBootTimer, "the boot interval: probe the unregistered process", bootInterval,
		[]ClaimState{StateLaunching, StateFailed}, booting...)
	t.ignore(onBootTimer, "the agent registered, so its process is no longer probed each interval", StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onBootTimer, "no boot is being watched", processless...)

	t.row(onDeadline, "the registration deadline: the agent never registered, or never said it was ready", registrationDeadline,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateLaunching, StateShimConnected, StateRegistered)
	t.ignore(onDeadline, "the agent is ready, so the boot watch is over", StateReady, StateWorking, StateIdle)
	t.ignore(onDeadline, "no boot is being watched", processless...)

	t.row(onTurnTimer, "an acknowledged prompt started no turn", noTurn, []ClaimState{StateLaunching, StateFailed, StateSuspended}, prompted...)
	t.ignore(onTurnTimer, "the turn started", StateWorking)
	t.ignore(onTurnTimer, noSend, unready...)
	t.ignore(onTurnTimer, noSend, gone...)

	// The ladder's wait after a turn that ended in a provider error. It is armed only where such
	// a turn can end — a claim working its task, or one whose turn already ended — and is dropped
	// with the task it is for (retirePending) and with the process it was armed under (letGo).
	t.row(onPromptRetryTimer, "the wait after a provider error is over: the task goes to the same session again", promptRetryDue,
		[]ClaimState{StateWorking}, StateReady, StateIdle, StateWorking)
	t.ignore(onPromptRetryTimer, noSend, unready...)
	t.ignore(onPromptRetryTimer, noSend, gone...)

	t.row(onProbeTimer, "probe the uncertain process again", reprobe, []ClaimState{StateLaunching, StateFailed}, live...)
	t.ignore(onProbeTimer, noProcess, processless...)

	t.row(onSuspendTimer, "the held suspension's deadline stops the turn, or its stop is tried again", suspendAtTimeout,
		[]ClaimState{StateSuspended}, StateWorking, StateIdle, StateReady, StateLaunching, StateShimConnected, StateRegistered)
	t.ignore(onSuspendTimer, "no suspension is held", StateQueued, StateLaunchUncertain, StateSuspended, StateRetired, StateFailed)

	t.row(onRelaunchTimer, "the waiting relaunch is tried again, or a suspension held meanwhile ends it", relaunchAgain,
		[]ClaimState{StateFailed, StateSuspended}, StateLaunching)
	t.ignore(onRelaunchTimer, "no relaunch is waiting", StateQueued, StateLaunchUncertain, StateShimConnected, StateRegistered,
		StateReady, StateWorking, StateIdle, StateSuspended, StateFailed, StateRetired)

	// The turn the wake pacing gave a launch it held back. A claim with no process of its own
	// waits launching; one whose process still runs waits in the state the wake found it, which
	// is every other live state.
	t.row(onWakeTimer, "the wake the pacing held runs its launch now, or a suspension held meanwhile ends it instead", wakeNow,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, live...)
	t.ignore(onWakeTimer, "no wake is waiting: a paced wake of a claim with no process is written launching before it waits, and a suspension, a failure, a retirement and an undecided pane each leave the claim without one",
		processless...)

	// The gate's own stop of a launch it held while the project is paused (pauseGate). A claim
	// with no process of its own waits launching for it; one whose process still runs waits in
	// the state the gate found it, which is every other live state. The stop ends the claim
	// suspended, or holds it for a turn's deadline of now (holdSuspension); a launch whose pause
	// has ended by then goes to the pacing instead, and may launch here (letOutHeldLaunch).
	t.row(onPauseTimer, "the gate held this claim's launch while the project is paused: stop the claim for the pause, or let the launch out if the pause is over", pauseStopDue,
		[]ClaimState{StateSuspended, StateLaunching, StateFailed}, live...)
	t.ignore(onPauseTimer, "no launch is held by the pause's gate: a claim with no process it holds is written launching, and a suspension, a failure, a retirement and an undecided pane each leave the claim without one",
		processless...)

	// The grant of its issue pod's start to a launch parked at the pod's gate (askPod). The park
	// keeps the claim launching with no process, and every decision that moves the claim out of
	// launching drops the parked launch's wake, and its end takes the start out of the gate for
	// what it left of the claim and disarms this timer (endPodPark, after suspended, retire, and
	// letGo, which a failure makes), so the grant reaches a launching claim alone. Its launch ends
	// as any launch does: started and launching, failed, or launching again for a retry; or held
	// by the pause's gate, where the project is paused at the grant (pauseGate).
	t.row(onPodStartTimer, "the issue pod's gate granted this claim's parked launch its start: ask the runtime for its process now", podStartDue,
		[]ClaimState{StateLaunching, StateFailed}, StateLaunching)
	t.ignore(onPodStartTimer, "no launch waits at the issue pod's gate: a parked launch keeps its claim launching, and every decision that moves the claim out of launching lets that launch go with its park",
		StateQueued, StateLaunchUncertain, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle,
		StateSuspended, StateFailed, StateRetired)

	// A re-adopted claim found still speaking another plugin contract, at boot or at a later sweep.
	t.row(onStalePlugin, "the process speaks another plugin contract: relaunch it onto the current one", refreshStalePlugin,
		[]ClaimState{StateLaunching, StateFailed}, StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onStalePlugin, "a claim still launching is left to its registration, which decides it the moment it arrives",
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateSuspended, StateFailed, StateRetired)

	// A restored claim whose stored record says its process died, at boot (RecordedDeath): the
	// relaunch that death raised is raised again now, through the pacing.
	t.row(onRecordedDeath, "the record says the process died: raise again the relaunch the death raised", relaunchRecordedDeath,
		[]ClaimState{StateLaunching, StateFailed}, live...)
	t.ignore(onRecordedDeath, "no process is recorded, so no death of one is", processless...)

	// The daemon's stall backstop over a claim whose agent stopped stepping mid-turn. It is a
	// row of the one state a turn runs in; the machine re-reads the claim's record before it
	// acts, so a record that moved since the sweep read it leaves the turn alone
	// (backstopStalledTurn). A held suspension ends in a suspension rather than a relaunch
	// (replaceProcess, endHeld), which is why suspended is among the destinations.
	t.row(onStallBackstop, "the agent stopped stepping mid-turn: end the turn, keep the task owed, and bring the session back", backstopStall,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateWorking)
	t.ignore(onStallBackstop, "the claim is in no turn, so its record names no turn to end",
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered, StateReady, StateIdle)
	t.ignore(onStallBackstop, noProcess, gone...)

	// The update round over the process holding this claim: opened and kept open by the daemon's
	// sweep, and ended once the replacement it waited for has been made. inRound is every state a
	// claim can hold an open round in.
	inRound := []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected,
		StateRegistered, StateReady, StateWorking, StateIdle, StateSuspended}
	t.row(onPodUpdateOpen, "record the update round, and send the task the old round's answer held; the sweep publishes the round's ask", openPodUpdate,
		[]ClaimState{StateWorking}, inRound...)
	t.ignore(onPodUpdateOpen, "the claim failed; what happens to it next is not the supervisor's decision, and no process of it would read the ask", StateFailed)
	t.ignore(onPodUpdateOpen, "the claim is retired; nothing of it runs on the pod being replaced", StateRetired)

	t.row(onPodUpdateEnded, "the update round is over: clear it, and relaunch the process or release the task it held", endPodUpdate,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended, StateWorking}, append(slices.Clone(inRound), StateFailed)...)
	t.ignore(onPodUpdateEnded, "the claim is retired; its update round ended with it", StateRetired)

	// The worker's answer to the round's ask: its work is pushed and its process may be
	// replaced. Answering is every state a claim whose agent is registered can give an answer
	// in, and unregistered is every state before that agent, where no process is there to
	// signal, whatever answer the claim's record still carries.
	answering := []ClaimState{StateRegistered, StateReady, StateWorking, StateIdle, StateSuspended}
	unregistered := []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected}
	t.row(onPodUpdateSignalled, "the worker signalled that its work is pushed and its process may be replaced", podUpdateSignalled,
		nil, answering...)
	t.ignore(onPodUpdateSignalled, "a claim with no registered agent cannot have signalled", unregistered...)
	t.ignore(onPodUpdateSignalled, "the claim failed; the process that would have signalled is gone", StateFailed)
	t.ignore(onPodUpdateSignalled, "the claim is retired; its update round ended with it", StateRetired)

	// Requests.
	t.row(onSpawn, "launch", spawn, []ClaimState{StateLaunching, StateFailed}, StateQueued)
	t.ignore(onSpawn, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onSpawn, "the claim is already launched", live...)
	t.ignore(onSpawn, "a suspended claim is resumed, not spawned", StateSuspended)
	t.ignore(onSpawn, failedClaim, StateFailed)
	t.ignore(onSpawn, retiredClaim, StateRetired)

	t.row(onRegister, "the agent registered", register, []ClaimState{StateRegistered, StateLaunching, StateFailed}, booting...)
	t.row(onRegister, "the recorded session registered again", reregister, []ClaimState{StateLaunching, StateFailed},
		StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onRegister, "no launch has minted a boot token", StateQueued)
	t.ignore(onRegister, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onRegister, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onRegister, failedClaim, StateFailed)
	t.ignore(onRegister, retiredClaim, StateRetired)

	t.row(onReady, "the agent is ready", ready, []ClaimState{StateReady, StateWorking, StateSuspended}, StateRegistered)
	t.row(onReady, "the agent said ready again", reready, []ClaimState{StateWorking}, StateReady, StateIdle, StateWorking)
	t.ignore(onReady, notRegistered, booting...)
	t.ignore(onReady, "no launch has minted a boot token", StateQueued)
	t.ignore(onReady, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onReady, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onReady, failedClaim, StateFailed)
	t.ignore(onReady, retiredClaim, StateRetired)

	t.row(onSuspend, "suspend: stop the process, keep the session", suspend, []ClaimState{StateSuspended},
		StateRegistered, StateReady, StateIdle)
	t.row(onSuspend, "a suspension of a claim in a turn is held for that turn's end, since stopping the agent now would cut its turn off, unless no process stands behind the claim: that one is suspended here",
		holdSuspension, []ClaimState{StateSuspended}, StateWorking)
	t.row(onSuspend, "a suspension of a launch stops the start, and is otherwise held until the agent is ready, unless its paced wake waits over no process: that one is suspended here", holdSuspendUntilReady,
		[]ClaimState{StateSuspended}, StateLaunching, StateShimConnected)
	t.row(onSuspend, "already suspended: an operator's suspend takes the suspension over", suspendedAgain, nil, StateSuspended)
	t.ignore(onSuspend, notRegistered,
		StateQueued, StateLaunchUncertain)
	t.ignore(onSuspend, failedClaim, StateFailed)
	t.ignore(onSuspend, retiredClaim, StateRetired)

	t.row(onResume, "resume the same session", resume, []ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.ignore(onResume, "the claim is not suspended", live...)
	t.ignore(onResume, "a queued claim is spawned, not resumed", StateQueued)
	t.ignore(onResume, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onResume, failedClaim, StateFailed)
	t.ignore(onResume, retiredClaim, StateRetired)

	t.row(onRuleResume, "resume the same session by rule, unless the operator suspended it", resumeByRule,
		[]ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.row(onRuleResume, "the claim runs: nothing to resume", nothingToDo, nil, live...)
	t.row(onRuleResume, "a queued claim is spawned by its start, not resumed", nothingToDo, nil, StateQueued)
	t.row(onRuleResume, "the previous launch's pane is still uncertain: the boot's reconciliation decides it", nothingToDo, nil, StateLaunchUncertain)
	t.row(onRuleResume, "a failed claim spent its launch budget or was refused in a way no try changes: only the workflow's retry relaunches it", nothingToDo, nil, StateFailed)
	t.row(onRuleResume, "the claim is retired", nothingToDo, nil, StateRetired)

	stoppable := []ClaimState{StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered,
		StateReady, StateWorking, StateIdle, StateSuspended, StateFailed}
	t.row(onStop, "release the claim and retire it; the tree's root only at the tree's close", stop, []ClaimState{StateRetired}, stoppable...)
	t.row(onStop, "already retired", nothingToDo, nil, StateRetired)

	t.row(onTreeClose, "the workflow's close of the tree: release the claim and retire it", treeClose, []ClaimState{StateRetired}, stoppable...)
	t.row(onTreeClose, "already retired", nothingToDo, nil, StateRetired)

	t.row(onIssueClose, "the workflow's close of the issue as done: release the claim and retire it, its session kept for a reopen", issueClose,
		[]ClaimState{StateRetired}, stoppable...)
	t.row(onIssueClose, "already retired, with its session kept for a reopen", nothingToDo, nil, StateRetired)

	t.row(onOperatorClose, "the operator's close of the tree, if no workflow issue backs it", operatorClose, []ClaimState{StateRetired}, stoppable...)
	// A close of a tree already retired is the outcome the operator asked for, so it answers as
	// the close that did it: an operator retrying after a timeout is not told their close failed.
	// It is still refused for a claim that is not its tree's root, and for a tree a workflow issue
	// backs: retiring is not a way past either.
	t.row(onOperatorClose, "the tree is already closed", operatorCloseRetired, nil, StateRetired)

	t.row(onRetry, "retry: fresh budgets, and the same session relaunched unless the retry asked for a new one or no store holds it", retry, []ClaimState{StateLaunching, StateFailed}, StateFailed, StateRetired)
	t.ignore(onRetry, "a queued claim is spawned, not retried", StateQueued)
	t.ignore(onRetry, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onRetry, "the claim has not failed", live...)
	t.ignore(onRetry, "a suspended claim is resumed, not retried", StateSuspended)

	// A retired claim is retried here as the workflow's own retry retries one: `legion claims stop`
	// retires a claim, and while this row refused that state the only way to start a stopped worker
	// again was an edit of the claims store with the daemon stopped (dispatch://INFRA-956 R3).
	t.row(onOperatorRetry, "the operator's retry of an architect or a phase worker, failed or stopped: fresh budgets, and the same session relaunched unless the retry asked for a new one or no store holds it", operatorRetry,
		[]ClaimState{StateLaunching, StateFailed}, StateFailed, StateRetired)
	t.ignore(onOperatorRetry, "a queued claim is spawned, not retried", StateQueued)
	t.ignore(onOperatorRetry, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onOperatorRetry, "the claim has not failed", live...)
	t.ignore(onOperatorRetry, "a suspended claim is resumed, not retried", StateSuspended)

	t.row(onForgetSession, "drop the session so the next launch is fresh", forgetSession, nil,
		StateQueued, StateSuspended, StateFailed, StateRetired)
	t.ignore(onForgetSession, "the claim has a live process or launch", StateLaunchUncertain, StateLaunching,
		StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle)

	t.row(onDeliver, "queue the task; it goes when the claim is next ready or idle", deliverLater, nil,
		StateQueued, StateLaunching, StateShimConnected, StateRegistered, StateWorking)
	t.ignore(onDeliver, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.row(onDeliver, "queue the task and send it", deliverNow, []ClaimState{StateWorking}, prompted...)
	t.row(onDeliver, "queue the task and resume the claim for it", deliverResuming,
		[]ClaimState{StateLaunching, StateFailed}, StateSuspended)
	t.ignore(onDeliver, failedClaim, StateFailed)
	t.ignore(onDeliver, retiredClaim, StateRetired)

	t.row(onExit, "the agent reported its exit: release a worker's claim, suspend the tree's root", exit,
		[]ClaimState{StateRetired, StateSuspended}, StateRegistered, StateReady, StateWorking, StateIdle)
	t.row(onExit, "the exit of a process the daemon already ended", nothingToDo, nil, gone...)
	t.ignore(onExit, notRegistered, StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected)

	// The outcome of a turn, from the producer that ended it. Every live state takes one: the
	// report is the producer's HTTP call, which races the turn's end on the worker stream and the
	// relaunch of a process that is coming up, and a report is about the provider rather than
	// about where the claim stands. A run of refused turns fails no claim; the one report that
	// fails it is that of a turn that ran no prompt of the daemon's, refused with a status that
	// cannot clear (turnOutcome).
	t.row(onTurnOutcome, "record the turn's outcome, and judge a turn that ran no prompt of the daemon's from it: a refusal that cannot clear fails the claim",
		turnOutcome, []ClaimState{StateFailed}, live...)
	t.ignore(onTurnOutcome, "no launch has minted a boot token", StateQueued)
	t.ignore(onTurnOutcome, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onTurnOutcome, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onTurnOutcome, failedClaim, StateFailed)
	t.ignore(onTurnOutcome, retiredClaim, StateRetired)

	// The compaction of the claim's Oh My Pi session, which reaches an agent that is running and
	// in no turn: the claim is working for as long as it runs, and the end gives it back
	// (internal/supervise/compact.go).
	t.row(onCompact, "take the claim for a compaction of its session", compactStart, []ClaimState{StateWorking}, prompted...)
	t.ignore(onCompact, "the claim is busy: a turn is running, or its session is already being compacted", StateWorking)
	t.ignore(onCompact, "the agent is not ready, so it has no session to compact", unready...)
	t.ignore(onCompact, "no process of this claim is running, so it has no session to compact", gone...)

	t.row(onCompactEnded, "the compaction is over: reset the run of provider errors it cleared, and send the owed task", compactEnded,
		[]ClaimState{StateReady, StateIdle, StateWorking, StateLaunching, StateFailed}, StateWorking)
	// A claim in one of these states holds no compaction, so the fence answers the end
	// ErrCompactProcessGone before any of them is read (Machine.fence, ownsCompaction); each is
	// still the table's word for the state, which every event of it must have.
	t.ignore(onCompactEnded, "the claim is not in a compaction: the process it ran on was let go first", prompted...)
	t.ignore(onCompactEnded, "the agent is not ready, so the compaction's process is gone", unready...)
	t.ignore(onCompactEnded, "no process of this claim is running, so the compaction's own is gone", gone...)

	// The project's resume, which the daemon posts to every machine of the project (ResumePause),
	// so every state takes it. A claim that holds nothing of the pause it names is changed by
	// nothing but the send of the task it holds (sendPending); one whose held launch is let out
	// launches, or fails, or is suspended by a tree whose cleanup is reserved, as a wake's turn is
	// (runWake), and one that is sent its task can be found in a turn already (sendPending).
	t.row(onResumePause, "the project's pause is over: drop that pause's held suspension, let out the launch its gate held, and send the task that waits", resumePause,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended, StateWorking}, slices.Concat(live, processless)...)
}

type builder struct{ rules map[key]rule }

func build(fill func(*builder)) map[key]rule {
	b := &builder{rules: map[key]rule{}}
	fill(b)
	return b.rules
}

func (b *builder) put(k key, r rule) {
	if _, taken := b.rules[k]; taken {
		panic(fmt.Sprintf("supervise: two rules for %s in %s", k.kind, k.state))
	}
	b.rules[k] = r
}

func (b *builder) row(kind eventKind, name string, act func(*Machine, context.Context, Event) error, to []ClaimState, states ...ClaimState) {
	for _, state := range states {
		b.put(key{state, kind}, rule{name: name, to: to, act: act})
	}
}

func (b *builder) ignore(kind eventKind, reason string, states ...ClaimState) {
	for _, state := range states {
		b.put(key{state, kind}, rule{ignore: reason})
	}
}

// The actions. Each runs under the machine's lock, after the event passed its fences.

// observe is the runtime's sweep. A live process in a ready or idle claim is also the retry of a
// delivery that is waiting — after a refusal, a prompt that started no turn, or a missing
// connection — when nothing else will come: the retry is bounded by the sweep's interval and by
// the prompt budget, never made on the spot. In a working claim it asks whether the turn is over
// where a mark arms the question (askTurnOver): the turn an earlier daemon saw start, when the
// last ask went unanswered, and the turn a busy answer recorded, at every sweep until the agent
// says it is not streaming or the turn's own start is read.
func observe(m *Machine, ctx context.Context, ev Event) error {
	return m.judge(ctx, ev.(RuntimeObservation).Observation, func(ctx context.Context) error {
		if err := m.ensureEnrolled(ctx); err != nil {
			return err
		}
		if m.claim.State == StateWorking {
			return m.askTurnOver(ctx, ev)
		}
		return m.sendPending(ctx)
	}, TimerProbe, m.deps.Timeouts.Probe)
}

// helloed is the shim's first connection: StateLaunching to StateShimConnected, and the only path
// where the process is known to hold a running agent process (OMP's own, not whatever the pod ran
// before it under init), so the registration deadline is re-armed from here at its base
// Boot×RegistrationIntervals alone (armRegistration derives the bound from m.claim.State, which
// this sets to StateShimConnected first): a Kubernetes pod can only dial its hello once both init
// containers have finished. An Oh My Pi that boots and never registers is retired on that base
// deadline alone, counted from the hello rather than from the launch.
func helloed(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateShimConnected
	m.armRegistration()
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.ensureEnrolled(ctx)
}

// enrolledOnly is a hello in a state the reconnect changes nothing about — the shim connected or
// the agent registered, not yet ready — except that the reconnected shim may need the enrollment
// it never received, or the resumed claim its first one.
func enrolledOnly(m *Machine, ctx context.Context, _ Event) error { return m.ensureEnrolled(ctx) }

func reconnected(m *Machine, ctx context.Context, _ Event) error {
	if m.send != nil {
		m.helloDuringSend = true
	}
	if err := m.ensureEnrolled(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// reconnectedMidTurn is the shim's hello while the claim is working. Within one daemon's life the
// shim's backlog replays whatever the turn did meanwhile, agent_end included; a restored claim
// may be waiting on an agent_end an earlier daemon received and never recorded, so its first
// hello asks. A turn a busy answer recorded is left to the backlog that follows the hello, as
// any turn of this daemon's life is: asked at the hello, an agent whose turn ended while the shim
// was away would end it here and be sent the task ahead of that turn's own replayed frames. The
// sweep asks for it (observe, askTurnOver).
func reconnectedMidTurn(m *Machine, ctx context.Context, ev Event) error {
	if err := m.ensureEnrolled(ctx); err != nil {
		return err
	}
	if !m.askFirst {
		return nil
	}
	return m.askTurnOver(ctx, ev)
}

// unansweredAsksWarn is the count of unanswered asks in a row from which askTurnOver logs each one
// at Warn: a working claim whose agent never answers stays working with no limit while its
// process lives, and an operator should see that.
const unansweredAsksWarn = 3

// askTurnOver asks the agent of a working claim whether its turn is still running, and ends it
// when the agent says it is not, where one of two marks arms the question: a restored claim's
// (askFirst), whose turn an earlier daemon saw start, and a busy answer's (busyTurn), whose turn
// the agent said it was in when it refused a prompt of the task and whose start no frame has
// shown (recordBusyTurn). An ask the agent does not answer says nothing about the turn: the claim
// stays working, the question stays armed, and the next sweep asks again. Taken as "not
// streaming", it would end a turn still running and set the claim idle, and the sweep would then
// send the task into the busy agent every interval.
//
// An agent that says it is streaming answers askFirst's question for good: the agent is still in
// the turn an earlier daemon saw start, and that turn's own end ends it (turnEnded). It does not
// answer busyTurn's: the record stands on the daemon's reading of a refusal, and the turn may
// send no frame this daemon reads, so the question stays armed and the next sweep asks again,
// until the turn's own start disarms it (turnStartedWhileBusy) or the agent says it is in no
// prompt. At the pin a session counts as streaming from a prompt's admission through its turn,
// pre-turn work such as a memory recall included, so the ask does not end a turn still being
// prepared. So a claim a busy answer recorded is bounded by one sweep after its agent stops
// streaming, by the stall backstop from the step the record wrote, and by a death.
//
// A turn that is over is ended through turnEnded, which clears askFirst and busyTurn with the
// turn. The agent is in no prompt, so no prompt of the task it acknowledged has a result still to
// come (Machine.outstanding): the wait for them ends with the turn, so a prompt lost on the way
// cannot hold the task, which the end sends again (afterTurn). The end is counted as every end is
// (TurnsEnded), so the daemon posts it to the issue's other claims, whose phase tasks may have
// been held for this turn (its wakeIssue).
func (m *Machine) askTurnOver(ctx context.Context, ev Event) error {
	if !m.askFirst && !m.busyTurn {
		return nil
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		// No shim yet (a sweep before its hello): the hello asks.
		return nil
	}
	unanswered := "supervise: the agent did not say whether the turn an earlier daemon saw start is over; the claim stays working and the next sweep asks again"
	over := "supervise: the turn an earlier daemon saw start is over"
	if m.busyTurn {
		unanswered = "supervise: the agent did not say whether the turn its busy answer recorded is over; the claim stays working and the next sweep asks again"
		over = "supervise: the agent is in no prompt; the turn its busy answer recorded is over"
	}
	streaming, answered := m.streaming(ctx, conn)
	if !answered {
		m.unansweredAsks++
		level := slog.LevelInfo
		if m.unansweredAsks >= unansweredAsksWarn {
			level = slog.LevelWarn
		}
		m.log.Log(ctx, level, unanswered, "unanswered", m.unansweredAsks)
		return nil
	}
	m.unansweredAsks = 0
	if streaming {
		// Streaming answers askFirst's question for good: the agent is still in the turn an
		// earlier daemon saw start, and its agent_end ends it (turnEnded). It leaves busyTurn's
		// armed, as the doc above says.
		m.askFirst = false
		return nil
	}
	if m.awaitsResult() {
		m.log.Info(over, "outstanding", m.outstanding)
	} else {
		m.log.Info(over)
	}
	m.forgetOutstanding()
	return turnEnded(m, ctx, ev)
}

func closed(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, nothing, TimerProbe, m.deps.Timeouts.Probe)
}

func reprobe(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, nothing, TimerProbe, m.deps.Timeouts.Probe)
}

// acked is the prompt taken: the task is marked read, the wait for its turn is armed, and the
// prompt is outstanding until Oh My Pi reports how its work ended (Machine.outstanding), which is
// what says which of the turns that follow was this prompt's. It stays outstanding when that wait
// runs out (noTurn) and when a later send of the task is answered busy: the pinned Oh My Pi
// acknowledges a prompt at its admission, before its pre-turn work, and its turn is the session's
// next whenever it starts, so a late start confirms the task (turnStarted) and the prompt's own
// result settles it (fence, promptResulted).
// It tells nothing of what the prompt carried, as confirmCarrying states, and so writes the
// delivery alone, as for every other prompt.
//
// It does record what the prompt carried of news of the approved design, the pending delivery's
// own news (PromptAcked says why that is the prompt's), with the prompt (awaitResult), and as the
// copy the task's confirmation credits (copyTurnNews): the news of the first prompt still
// outstanding, whose turn is the session's next (turnNews). The acknowledgement is the one word that
// the send's adoption of the working copy and its identity mint went through and that the agent
// took the prompt, so a send refused at either, or answered busy, records none, and a task no
// prompt of which was acknowledged stays one the question a restart leaves never takes a turn of
// the agent's own for (unsentNews). The copy is written with the delivery, so a restart reads what
// the prompt it may find running carried. An acknowledgement the shim gave from its own record, for
// a send under an id it already carried (shimwire's Dedupe), is of the same news as the send it
// forwarded, since every send under one id carries the same news.
func acked(m *Machine, ctx context.Context, _ Event) error {
	m.helloDuringSend = false
	p := m.claim.Pending
	m.awaitResult(p.ID, p.News)
	m.markRead(p)
	m.copyTurnNews(p)
	m.arm(TimerTurn, m.deps.Timeouts.RPC, p.ID)
	return m.putPending(ctx)
}

// ackedInTurn is the acknowledgement of a prompt whose turn the stream reported first: Oh My Pi's
// agent_start can land before the answer to the prompt that caused it, and that start has already
// confirmed the delivery (turnStarted). The mark and the wait for a turn are that turn's to need,
// and it has started — but the prompt itself is still outstanding, and which of the turns that
// follow is its own is still a question only its result answers, so it is recorded here too, with
// the news it carried. The copy the confirmation credits stays as that confirmation fixed it
// (turnAnswers). Nothing else moves, and nothing is written.
func ackedInTurn(m *Machine, _ context.Context, _ Event) error {
	m.helloDuringSend = false
	p := m.claim.Pending
	m.awaitResult(p.ID, p.News)
	return nil
}

// refused is a send that did not reach a turn from its own goroutine: the agent refused the prompt
// (runtime.ErrPromptRefused), the shim refused to adopt the working copy ahead of it
// (runtime.ErrAdoptRefused), or the transport lost the send. Either way a turn that confirmed the
// send meanwhile was not this send's, and the delivery keeps its id — the shim drops a refused
// delivery's record, and answers a lost one it did see without a second turn. The confirmation
// goes with it unless another prompt of the task is still outstanding (Machine.outstanding): the
// pinned Oh My Pi refuses this send because it is preparing or running that prompt's turn, the
// very turn whose start, with this send in flight, confirmed the task, so it stays the task's.
// A confirmation that goes takes back what it told (moveTelling, confirmCarrying): the turn that
// confirmed the task was another's, so the agent never read the sentence the task's prompt
// carried, and the next prompt carries it again. The credit it made of the news of the approved
// design goes with it: the copy goes (Delivery.Carried), and the write of the delivery without it
// ends that credit, since the store counts only the copy a confirmed row holds
// (design_news_told_in). A refused send writes no copy of its own news, since only an
// acknowledgement does (acked): a task whose every send was refused here — its adoption refused,
// its identity not minted, its prompt answered busy — is one no acknowledged prompt carried, which
// no turn of the agent's own confirms across a restart (unsentNews).
//
// Which failures cost budget is decided here, and this doc is where the split is stated.
// Charged are the two answers the far side gave about this very workspace: a refused prompt and a
// refused adoption. Uncharged are the failures that are no answer at all — no reply within the RPC
// timeout, a closed connection, a frame lost on the way, and a failure to mint the role's App
// identity, which never leaves the daemon and is an outage of the GitHub App rather than a fault of
// this workspace. Nothing is ever re-sent on the spot: every retry waits for the next sweep, hello,
// turn end, or ready.
//
// A refusal is the agent's answer and is charged like a prompt that started no turn (LEGION-60:
// an agent that refused every prompt would otherwise be retried forever). Oh My Pi's busy answer
// (agentBusy) is the exception, as it is for a late refusal (lateRefused): the agent is in a turn
// of its own — an Envoy notice's, which a tree publishes freely while its worker boots — so it is
// working, not refusing to work, and charging it would spend the budget of the very turn doing the
// work, three such collisions retiring the agent mid-turn. The task waits unread, charged nothing,
// and a ready or idle claim is recorded working (recordBusyTurn): sendPending holds the task for
// that turn, and the turn's end sends it — its agent_end, or the sweep's answer that the agent is
// in no prompt (askTurnOver). Left ready, the claim was sent the task again at every sweep and
// hello for the length of the turn, and answered busy each time.
//
// A refused adoption is charged the same way, and the busy exemption is not read for it: those are
// the reasons Oh My Pi gives about a turn of its own, while this is the shim's own answer about a
// working copy. Every `ok: false` the shim gives the adoption counts, its own command timeout
// included — a jj wedged on a stale lock reports as exactly that — and the refusal's text is never
// read to classify it, only carried into the log, where the shim's words already name the stale
// lock's path and its age (INFRA-1480: unbounded re-sends of a refused adoption held two planning
// tasks 31 and 33 hours, logged at warning level and charged nothing). With the shipped budgets
// (prompt_failure_limit 3, prompt_retire_limit 2) the sixth consecutive charge fails the claim,
// which workflow.Stranded and `legion state` report.
//
// A send lost to the transport is not charged and waits for the shim's next hello — which may
// already have come while the send was out. So does an identity the daemon could not mint
// (runtime.ErrIdentityUnavailable), which is uncharged for the same reason and logged under a
// name of its own: it is raised before any frame leaves, and an operator told the transport lost
// the prompt would look at the network while the GitHub App is the thing that is down.
//
// Every failure here, charged or not, is recorded on the pending delivery before the split acts:
// how many sends of this task have failed, when this one failed and the error it failed
// with (Delivery.SendFailures and its two companions). That record is a diagnosis rather than a
// second budget, and the uncharged failures are exactly the ones it exists for: a transport loss
// costs nothing and is retried for ever, so without it the only trace of a claim waiting days on
// a send that never lands is a log line inside its pod. It rides out on the write this already
// makes of the delivery, so one persist covers the taken-back confirmation and the record alike,
// and the claim goes with them where that confirmation's telling went back (putClaimAndPending);
// that write is unconditional because the record changes on every failed send, where the
// confirmation it takes back is there to clear only on some.
//
// Two things keep that record from deciding anything about the charge, because the charge is
// what bounds the re-sends and the record is only a diagnosis.
//
// The text is the far side's own — a refused adoption carries the wedged command's whole stderr
// and a refused prompt Oh My Pi's decoded `error` — so the delivery holds it, in memory as in the
// store, as the daemon keeps any far side's error text, a text the store takes whatever the far
// side wrote: the text the split's line carries, which logFarSideError writes and returns, so the
// line, the record and the charge's own line carry one text. That is why the split chooses its
// line before the record is written. The whole text follows the line under a message of its own
// (logFarSideError).
//
// The write may still fail — the store has its own weather — and a failed write is logged and
// carried past rather than returned, so what the split decided is always done. A return there would
// charge a refused adoption nothing and re-send it with no bound, which is the stall this split
// exists to end. Nothing of the delivery is lost by carrying on: the failed write left it
// unsaved (saved), and settle writes the claim and the delivery together before the next
// decision, so the taken-back confirmation and this record reach the store on the first write
// the store takes.
//
// A confirmation the refusal takes back that the claim's handshake recorded goes from the
// handshake too (takeBackConfirmation), and the write is then of the claim and the delivery
// together (putClaimAndPending), under the same mark and the same carrying on. A confirmation it
// keeps for an outstanding prompt keeps the handshake's as well: that prompt's turn is the task's.
func refused(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(PromptRefused)
	resend := m.helloDuringSend
	m.helloDuringSend = false
	p := m.claim.Pending
	untold, handshakeMoved := false, false
	if !m.awaitsResult() {
		// No acknowledged prompt of the task is left, so no copy of its news stands, and the
		// write below ends the credit of a confirmation it takes back (Delivery.Carried,
		// takeBackConfirmation).
		handshakeMoved = m.takeBackConfirmation(p)
		untold = m.moveTelling(p.ID, "")
	}
	// The split: the line that names this failure, and for a charged one the words its charge
	// puts before the far side's text ("" for a failure charged nothing).
	promptRefused := errors.Is(r.Err, runtime.ErrPromptRefused)
	busy := promptRefused && agentBusy(runtime.RefusalReason(r.Err))
	level, msg, charge := slog.LevelWarn, "supervise: the prompt was lost to the transport; the delivery waits", ""
	args := []any{"delivery", r.DeliveryID}
	switch {
	case busy && m.awaitsResult():
		level, msg = slog.LevelInfo, "supervise: the agent refused the prompt; it is busy with the turn of an earlier prompt of this task, whose result settles the task"
		args = append(args, "outstanding", m.outstanding)
	case busy:
		msg = "supervise: the agent refused the prompt; it is in a turn of its own, whose end sends the task again"
	case promptRefused:
		msg, charge = "supervise: the agent refused the prompt", "refused: "
	case errors.Is(r.Err, runtime.ErrAdoptRefused):
		msg, charge = "supervise: the shim refused to adopt the working copy; the prompt was never sent", "the working copy was not adopted: "
	case errors.Is(r.Err, runtime.ErrIdentityUnavailable):
		msg = "supervise: the role's GitHub App identity could not be minted; the prompt was never sent and the delivery waits"
	}
	p.SendFailures++
	p.LastSendFailureAt = m.deps.Clock.Now()
	p.LastSendError = m.logFarSideError(ctx, level, msg, r.Err, args...)
	var err error
	if untold || handshakeMoved {
		err = m.saved(m.putClaimAndPending(ctx))
	} else {
		err = m.putPending(ctx)
	}
	if err != nil {
		m.log.Warn("supervise: the failed send was not recorded on the delivery; settle writes it again and the charge goes on",
			"delivery", r.DeliveryID, "error", err)
	}
	switch {
	case busy:
		return m.recordBusyTurn(ctx)
	case charge != "":
		return m.chargePrompt(ctx, charge+p.LastSendError)
	case resend:
		return m.sendPending(ctx)
	}
	return nil
}

// refusedElsewhere is the part of a late refusal every state shares, and it reports whether that
// was all of it. Three kinds say only that a prompt this task was sent under never ran:
//
//   - A refusal naming a prompt of the task still outstanding other than the pending one
//     (Machine.outstanding). The wait for its turn ran out, re-queued the task under a new id and
//     charged the prompt (noTurn), so it is charged nothing more and the id is not rotated again;
//     what it can still take back is a confirmation a late start took for it, and Oh My Pi's busy
//     answer records a ready or idle claim working for the turn it names (waitedOutAnswer).
//   - Any other refusal naming a prompt other than the pending one. It is the prompt whose
//     acknowledgement set the read mark (the fence lets no other through). The wait that gave up
//     on it already re-queued the task under a new id and charged the prompt, so the mark goes and
//     nothing else; rotating or charging again would spend the budget twice for one prompt.
//   - A refusal the shim replayed from its backlog, answering a prompt an earlier connection sent.
//     The pending id may be the very delivery this connection is re-sending, whose own answer is
//     still to come, so it clears only the mark it set.
//
// A replayed refusal that takes back a task a turn not its own confirmed is the exception
// (takesBackConfirmed): the task goes back unread, charged nothing, so that turn's end sends it
// again rather than retiring it as served — unless another prompt of the task is still
// outstanding, whose turn that one is (takeBackPending).
func refusedElsewhere(m *Machine, ctx context.Context, r StreamLateRefusal) (bool, error) {
	p := m.claim.Pending
	switch {
	case m.takesBackConfirmed(r):
		m.log.Warn("supervise: a replayed refusal names the prompt a foreign turn confirmed; the task goes back unread",
			"delivery", r.DeliveryID, "error", r.Error)
		m.answered(r.DeliveryID)
		return true, m.takeBackPending(ctx, taskUnread)
	case r.DeliveryID != p.ID && m.outstandingPrompt(r.DeliveryID):
		return true, m.waitedOutAnswer(ctx, r.DeliveryID, taskUnread, agentBusy(r.Error), "refused: "+r.Error)
	case r.Replayed || r.DeliveryID != p.ID:
		m.log.Warn("supervise: a refused prompt had marked the task as read; the mark is cleared",
			"delivery", r.DeliveryID, "replayed", r.Replayed, "error", r.Error)
		return true, m.markUnread(ctx)
	}
	return false, nil
}

// takesBackConfirmed is whether a replayed refusal of the pending prompt says the turn that
// confirmed the task was not the task's. It is when no send is in flight and the task was not sent
// through the connection the refusal arrived on or a newer one: then the confirmation came from a
// turn the agent was already in, and the refused prompt was an older connection's. A send in
// flight, or one through that connection or a later one, may be the prompt whose turn confirmed
// the task, so the task stays confirmed and the refusal clears only the mark. Connections are
// compared by the order they were registered in, not by the order the machine handles their
// events in: a sweep can send through a connection before its hello is handled, the shim can redial
// before the machine reaches the old connection's backlog, and an acknowledgement is handled by
// its send's own goroutine.
func (m *Machine) takesBackConfirmed(r StreamLateRefusal) bool {
	p := m.claim.Pending
	return r.Replayed && p != nil && r.DeliveryID == p.ID && !p.ConfirmedAt.IsZero() &&
		m.send == nil && m.sentThrough < r.ConnSequence
}

// refusedUnprompted is a refusal landing while the claim cannot be prompted: relaunching after the
// process that refused died, or stopped. Nothing is charged or re-sent here, since the process that
// refused is not the one that will be prompted next. It still says the prompt never ran, so the
// mark it set goes; a task still carrying that prompt's id also goes back under a new one, so the
// next send is a new prompt that no refusal of the old one can name.
func refusedUnprompted(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(StreamLateRefusal)
	if done, err := refusedElsewhere(m, ctx, r); done {
		return err
	}
	m.log.Warn("supervise: a prompt was refused while no process can be prompted; the task goes back unread",
		"delivery", r.DeliveryID, "state", string(m.claim.State), "error", r.Error)
	m.answered(r.DeliveryID)
	return m.takeBackPending(ctx, taskUnread)
}

// lateRefused is the agent refusing a prompt it had acknowledged. OMP answers that way when it
// finds a turn it did not start — a notice delivered to the pane, a human's steer — and by the
// same route when the prompt fails before any turn begins, a provider answering "No API key
// found for …" among them.
//
// Which of the two it is, is the turn: one the stream already saw start, one Oh My Pi names in
// its own busy answer, or one the agent says it is in when asked. The busy answer is read
// because it is the authority — Oh My Pi refuses that way exactly when a turn is running — and
// because the ask can time out, where charging a working agent is the worse mistake. A turn of
// its own really running is the collision: the agent is working, not refusing to work, and the
// daemon delivers those notices itself, so a tree publishing them while a worker boots must not
// spend that worker's prompt budget. The task is taken back, charged nothing, and the turn's end
// sends it. A claim the stream saw in the turn stays working; a ready or idle one, whose turn's
// start the daemon never read, is recorded working (recordBusyTurn), so that nothing sends the
// task into the turn the refusal names before that turn ends — its agent_end, or the sweep's
// answer that the agent is in no prompt (askTurnOver).
//
// The turn running may be the task's own: a prompt of the task acknowledged earlier is still
// outstanding (Machine.outstanding), and the pinned Oh My Pi refuses this one because it is
// preparing or running that prompt's turn. The refused prompt is taken back all the same, under a
// new id, but the task keeps its confirmation and its read mark for that turn (takeBackPending),
// the claim is recorded working for it as for any other, and that prompt's start confirms the
// task and its result settles it.
//
// No turn running is the agent refusing the work. It is charged like any acknowledged prompt
// that becomes no turn, and the retry waits for the sweep that follows, so a refusal that never
// stops walks the budget to its end — a relaunch, and then the retirement — instead of looping
// on the spot for ever against an agent that cannot start a turn at all.
func lateRefused(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(StreamLateRefusal)
	if done, err := refusedElsewhere(m, ctx, r); done {
		return err
	}
	m.answered(r.DeliveryID)
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	streaming := func() bool { busy, _ := m.streaming(ctx, conn); return busy }
	if m.claim.State == StateWorking || agentBusy(r.Error) || (connected && streaming()) {
		if m.awaitsResult() {
			m.log.Info("supervise: the agent refused an acknowledged prompt; it is busy with the turn of an earlier prompt of this task, whose result settles the task",
				"delivery", r.DeliveryID, "outstanding", m.outstanding, "error", r.Error)
		} else {
			m.log.Warn("supervise: the agent refused an acknowledged prompt; it is in a turn of its own",
				"delivery", r.DeliveryID, "error", r.Error)
		}
		// The agent is in a turn this prompt did not start, so it never read this prompt: the
		// delivery goes back to waiting, unread unless an earlier prompt of the task is the turn's,
		// and nothing the worker reports from a turn of its own belongs to it. The turn is the
		// agent's word whatever became of the take-back's write, so the record is made either way.
		return errors.Join(m.takeBackPending(ctx, taskUnread), m.recordBusyTurn(ctx))
	}
	m.log.Warn("supervise: the agent refused an acknowledged prompt and is in no turn; the prompt is charged",
		"delivery", r.DeliveryID, "error", r.Error)
	// The prompt did not run: the agent answered it a failure with no turn of its own to explain
	// it — a provider that rejected before any turn began, an agent that will not take the work.
	// The task goes back unread, so nothing reported from whatever turn follows belongs to it.
	return m.promptFailed(ctx, "refused an acknowledged prompt with no turn running: "+r.Error, taskUnread)
}

// agentBusy is Oh My Pi's own refusal of a prompt for a turn already running, which it words
// exactly this way. Every other rejection — a provider that answered before any turn began among
// them — is the agent failing to take the work. Reading the message is the daemon's one handle on
// which it is. The pinned Oh My Pi also gives the refusal a kind, `code: "session_busy"`
// (shimwire.Response.Code, shimwire.PromptError.Code), but that kind covers two refusals besides
// this one, a session busy with maintenance or user work and a prompt that waited 30 s while
// another run held the agent (rpc-busy-code.ts), so it does not tell this one apart.
func agentBusy(reason string) bool {
	return strings.HasPrefix(reason, "Agent is already processing")
}

// recordBusyTurn records a ready or idle claim working on Oh My Pi's busy answer to a prompt of
// its pending task, at every place the daemon learns it: the send refused (refused), the
// acknowledged prompt refused (lateRefused), its result saying it ran no turn (promptResulted),
// and either answer naming an earlier prompt of the task whose wait for its turn already ran out
// (waitedOutAnswer, which records it wherever the task is left unconfirmed). The answer is the
// agent's own statement that its session is in a prompt — the pinned Oh My Pi counts a session as
// streaming from a prompt's admission through its turn, pre-turn work such as a memory recall
// included — so the claim is in a turn this daemon has not seen begin, and sendPending's first
// guard holds the task for that turn's end rather than sending it again at every trigger. Left
// ready, the claim was sent the task again at every sweep, hello and fenced acknowledgement, and
// answered busy each time, about every 2.5 seconds for the length of the turn
// (dispatch://INFRA-3276 R4). A claim already working is in the turn the stream showed it, and
// nothing is recorded.
//
// An answer that comes while another prompt of the task is still outstanding (Machine.outstanding)
// is recorded the same way. The turn the agent is preparing or running is that prompt's, so the
// task keeps its confirmation and its read mark for it (takeBackPending), and no further send of
// the task may go into it any more than into a turn of the agent's own; its start confirms the
// task and its result settles it, as from a ready claim (turnStartedWhileBusy, promptResulted).
//
// It is not answerAskFirstAfterDrop, which records the same state for a claim whose task was
// dropped and refuses one that holds a delivery: every claim here holds the task it was answered
// busy for.
//
// The record is durable, written through durableWrite as turnStarted's is, so the daemon hears
// the claim working (OnDurableState) — the busy answer is the agent's word that a woken pod's
// role is back in a prompt, which is what its first turn measures — and a daemon restarted inside
// the turn restores the claim working with its question armed (NewMachine's askFirst), rather than
// ready and prompting a busy agent. The turn's start line is written before that write, as every
// path into working writes one (startTurn), and it names no delivery: the daemon has seen no turn
// begin, so it cannot say whose the turn is. The turn's own agent_start writes no second one
// (turnStartedWhileBusy), so one start line and one end line still bracket the turn.
//
// The step written after it (putStep, the turn's start, as turnStarted writes one) keeps the stall
// backstop from judging the claim by a step an earlier turn left: the backstop takes every working
// claim whose last step is past its role's bound (the daemon's backstopStalledTurns), and a worker
// idle past that bound would otherwise be relaunched at the sweep before its turn starts.
//
// For the same reason the record dates the turn's start (Claim.LastTurnStart) at the busy answer,
// the daemon's first word that the turn exists, rather than leaving the start of the turn before
// it standing: a let-go of the process in this turn reads an activity report older than that
// start as no report of the turn (recordEnd), where the turn before's start would have counted a
// report made before this turn began as one of it. The turn's own agent_start, read later, dates
// it again at its own moment (beginTurn).
//
// The mark it sets (busyTurn) is what ends the record when no frame of the turn is ever read: the
// runtime's sweep asks the agent whether the turn is over and ends it through turnEnded once the
// agent says it is in no prompt (observe, askTurnOver). So such a claim is bounded by one sweep
// after the agent stops streaming, by the stall backstop from the step written here, and by a
// death.
func (m *Machine) recordBusyTurn(ctx context.Context) error {
	if state := m.claim.State; state != StateReady && state != StateIdle {
		return nil
	}
	m.claim.State, m.claim.LastTurnStart = StateWorking, m.deps.Clock.Now()
	m.busyTurn = true
	m.log.Info("supervise: the agent's busy answer records the claim working; its task waits for the end of the turn the agent is in",
		"delivery", pendingID(m.claim.Pending), "outstanding", m.outstanding)
	m.startTurn("")
	if err := m.durableWrite(ctx, StateWorking, m.persist); err != nil {
		return err
	}
	return m.putStep(ctx, StepRecord{TurnStartedAt: m.deps.Clock.Now()})
}

// turnStarted is a turn starting in a ready or idle claim. It confirms the pending delivery when
// it is that delivery's turn: a replay naming it, or — B1 — Oh My Pi's own agent_start while the
// delivery is in flight (sent and not yet answered, acknowledged and waiting, possibly sent by an
// earlier daemon, or acknowledged with its result still to come). Any other turn is a foreign
// one, and confirms nothing. Either way the claim records when this turn began
// (Claim.LastTurnStart), which is persisted with it: the time is a reader's only handle on how
// long the agent has been in the turn it is in.
//
// The persisted working state is reported to the daemon (Machine.OnDurableState), because a turn is
// the one observation that a woken pod's role came back and took work, whether or not the wake
// carried a delivery for it to confirm: a wake for a notice or a hook prompt carries none, and its
// resume would otherwise never be measured (R5).
//
// The question a restart left (askFirst) counts as in flight for every delivery but two. A root
// architect's task of a run (asksRoot) may be a task nobody has ever sent, held on the claim while
// its root had no architect to hear it, so a turn of the agent's own is not that task's. Confirmed
// by it, the task would tell the person's decisions it names — the confirmation is their telling —
// to an architect nobody prompted, and be retired unsent when that turn ended. A task carrying
// news of the approved design that no acknowledged prompt carried (unsentNews) is the other: its
// confirmation records the news as taken, so a turn the agent started by itself, an Envoy
// message's while the daemon was down, would record news nobody read — a task whose adoption or
// identity mint failed before any prompt went among them. Only a send of this machine's own or a
// prompt it waits for a turn on makes such a turn that task's, and its confirmation credits what
// that prompt carried (turnAnswers).
//
// A prompt of the task acknowledged and still outstanding (Machine.outstanding) makes the turn the
// task's however late it starts, past the wait for it (noTurn) and past any busy answer to a
// later send of the same task: the pinned Oh My Pi acknowledges a prompt at its admission, runs
// one admitted prompt at a time and refuses the rest, and hands each prompt's result to the first
// run after its admission (rpc-prompt-results.ts), so the session's next turn is that prompt's;
// the Claude Code bridge takes one prompt at a time too (internal/claudebridge's Busy and
// promptRun). Without it, a start after the five-second wait confirmed nothing: the worker's
// completion in that turn was refused as the earlier task's, and the task was sent again as
// the turn ended (dispatch://INFRA-3276).
// A start taken this way that was in fact another input's turn is corrected by the prompt's own
// result when it comes back not invoked: the task goes back to waiting and that turn's end sends
// it (promptResulted).
//
// The decision and its bookkeeping are beginTurn's, which the agent_start of a turn a busy answer
// recorded makes too (turnStartedWhileBusy).
func turnStarted(m *Machine, ctx context.Context, ev Event) error {
	m.claim.State = StateWorking
	return m.beginTurn(ctx, ev.(StreamTurnStart), m.startTurn)
}

// beginTurn is what a turn's start decides and records, in a claim the start has just put in
// working (turnStarted) or one a busy answer already recorded working (turnStartedWhileBusy):
// when the turn began, whether it is the pending delivery's turn — which confirms that delivery —
// the durable write, and the turn's step. logStart writes the start's line, naming the delivery
// the start confirms and "" for a start that confirms none.
func (m *Machine) beginTurn(ctx context.Context, start StreamTurnStart, logStart func(delivery string)) error {
	m.claim.LastTurnStart = m.deps.Clock.Now()
	p := m.claim.Pending
	news, inFlight := m.turnAnswers()
	own := p != nil && p.ConfirmedAt.IsZero() && (start.DeliveryID == p.ID || (start.DeliveryID == "" && inFlight))
	// The write is the confirmation where this start is the pending delivery's turn, and the
	// claim alone where it is a foreign one; the delivery is named in the start's line for the
	// same reason, so both are decided in the one place that asks the question.
	persist, task := m.persist, ""
	if own {
		persist, task = func(ctx context.Context) error { return m.confirmCarrying(ctx, news) }, p.ID
	}
	// The start is logged before the write, never after it: the decision this line exists for is
	// a turn start whose claim write failed, and a line written after the write would leave
	// exactly that decision silent again. The delivery is named only where this start is that
	// delivery's turn; a foreign turn is no task's and carries none.
	logStart(task)
	if err := m.durableWrite(ctx, StateWorking, persist); err != nil {
		return err
	}
	// The turn's own start, through the store's narrow call, written only once that durable write
	// has landed: a write the store did not take returns its error above, so no step records the
	// start of a turn whose claim never reached working. A claim whose agent sends no tool frame
	// at all is judged from this, so a turn that hangs before its first call reads as the turn it
	// is rather than as a claim that has never stepped. It carries no call, which leaves the call
	// the record names exactly as it stands — the one the agent was in when it stopped, which a
	// recovery's own turn must not erase (Store.PutClaimStep).
	return m.putStep(ctx, StepRecord{TurnStartedAt: m.deps.Clock.Now()})
}

// toolStepped records what the agent is doing: the call its stream just reported starting or
// ending, stamped on receipt, because no frame carries a time of its own. It writes the claim's
// step columns alone (Store.PutClaimStep) and touches no other field of the claim — the record
// is built from frames that arrive between a machine's writes, and a claim write would erase
// every step seen since that machine read it.
//
// The arguments were measured and hashed where the frame was read (stream.ToolStep) and their
// text reaches neither this handler nor anything it writes: a shell command routinely holds a
// credential, and this record reaches every surface an operator reads. The end frame repeats
// neither the arguments nor the intent, so what is written beside an end is what its start left
// (Machine.openCalls); an end whose start this daemon never read — a frame the shim's backlog
// dropped, or a call open across a restart — names the call with no arguments measured, which is
// what the daemon actually knows.
//
// A frame that arrived on a connection the claim has since replaced does not move the claim's
// last-step time (staleStep). It still records its call: that is what the agent was doing, and
// the record exists to say so after the process that was doing it is gone.
func toolStepped(m *Machine, ctx context.Context, ev Event) error {
	s := ev.(StreamToolStep)
	now := m.deps.Clock.Now()
	step := StepRecord{At: now}
	if m.staleStep(s) {
		step.At = time.Time{}
	}
	switch {
	case s.CallID == "":
		// A frame with no call id names no call to pair or to overwrite — the Claude Code
		// bridge's start is one — so all it says is that the agent stepped. An empty call id
		// is what tells the store to leave the six call columns as they stand.
	case s.End:
		open, held := m.takeOpenCall(s.CallID)
		if !held {
			m.log.Info("supervise: a tool call ended whose start this daemon never read; its arguments are unmeasured",
				"tool", s.Tool, "call", s.CallID)
		}
		step.Tool, step.CallID = s.Tool, s.CallID
		step.ArgsBytes, step.ArgsHash, step.StartedAt = open.argsBytes, open.argsHash, open.startedAt
		step.EndedAt = now
	default:
		open := openCall{argsBytes: s.ArgsBytes, argsHash: s.ArgsHash, startedAt: now}
		m.holdOpenCall(s.CallID, open)
		step.Tool, step.CallID = s.Tool, s.CallID
		step.ArgsBytes, step.ArgsHash, step.StartedAt = open.argsBytes, open.argsHash, open.startedAt
	}
	return m.putStep(ctx, step)
}

// staleStep is whether a tool frame arrived on a connection that is no longer the claim's: the
// claim has none, or the one it has was registered after the frame's. Both are frames of a
// replaced incarnation: the stream's own queue can hand a machine a frame of a connection that
// was replaced while the frame waited. The shim's replay of the frames it held while no daemon
// was attached is not one of them — it arrives on the next connection, which is the claim's live
// one, so those frames are stamped at receipt (store's PutClaimStep). Connections are compared
// by the order they were registered in, as a replayed refusal is (takesBackConfirmed): the order
// the machine handles their events in says nothing.
func (m *Machine) staleStep(s StreamToolStep) bool {
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	return !connected || s.ConnSequence < conn.Sequence()
}

// turnStartedWhileBusy is a turn starting in a claim that is already working. A claim working a
// turn is in the turn it is already in and nothing moves, except in two claims.
//
// One is a claim a busy answer recorded working (busyTurn, recordBusyTurn): the start is the start
// of the turn the record stands for, the first frame of it this daemon reads. It is decided and
// recorded as turnStarted decides and records a start (beginTurn): when the turn began
// (Claim.LastTurnStart), the confirmation of the pending task where the start is its turn —
// among them the late start of a prompt of the task still outstanding (Machine.outstanding), which
// is the turn the busy answer refused a later send of the task for — the durable write and the
// turn's step. The record's own question is answered by this start, so the mark goes and the
// sweep stops asking (askTurnOver): the turn's agent_end ends it from here. It writes no
// second start line, since recordBusyTurn wrote the turn's one, and names the delivery it confirms
// in a line of its own instead (busyTurnBegun).
//
// The other is a claim working because its session is being compacted: a turn of Oh My Pi's own
// starting under the compaction — a follow-up an Envoy delivery queued while the session was being
// compacted — is recorded on the compaction: its end would otherwise reach an idle claim, where it
// means nothing, and the compaction's end would send the owed task into a running agent
// (compactEnded). That turn's start line is written here (startTurn), because its end writes the
// end line (turnEnded): an operator reads an end with no start as a turn the daemon never saw
// begin, and the daemon saw this one. The daemon prompted nothing for it, so it is no task's turn
// and its line names no delivery.
//
// The two never meet: a compaction is taken only from a ready or idle claim (compactStart), and a
// busy answer records only a ready or idle one.
func turnStartedWhileBusy(m *Machine, ctx context.Context, ev Event) error {
	if m.busyTurn {
		m.busyTurn = false
		return m.beginTurn(ctx, ev.(StreamTurnStart), m.busyTurnBegun)
	}
	if m.compacting == nil {
		return nil
	}
	m.startTurn("")
	m.log.Info("supervise: a turn started while this claim's session is being compacted; the claim is left working for it",
		"generation", m.claim.Generation)
	m.compacting.inTurn, m.compacting.back = true, StateIdle
	return nil
}

// busyTurnBegun is the line of the agent_start of a turn a busy answer recorded
// (turnStartedWhileBusy), in place of the pinned start line recordBusyTurn already wrote for it.
// It names the delivery the start confirms, if any, and that delivery becomes the one the turn's
// end line names (Machine.turnsDelivery): the turn is now known to be that task's, which the
// start line written at the record could not yet say.
func (m *Machine) busyTurnBegun(delivery string) {
	if delivery != "" {
		m.turnsDelivery = delivery
	}
	m.logTurn("supervise: the turn a busy answer recorded has begun", delivery)
}

// turnEnded is the turn over: the delivery whose turn it was retires (settle), and one queued
// meanwhile goes — unless a suspension is held for this turn (holdSuspension), which runs
// instead; a stop the runtime refuses leaves the claim idle, still holding it. The agent just
// said where it is, so nothing is left to ask it after a restart. The claim records when the turn
// ended (Claim.LastTurnEnd) before any of that: every path out of here persists the claim, and
// the time is what tells an idle worker that finished a moment ago from one idle since yesterday.
//
// A turn that ends is also the end of the work the deaths were charged against (chargeDeath), so
// the count goes with it: an agent killed twice in its turns and through the third is making
// progress, and only deaths that keep a turn from ending should ever fail its claim.
//
// Whose turn it was is a question background work made real, and settle is where it is answered:
// a turn that ends while the task's prompt is still outstanding retires nothing, because Oh My
// Pi's agent_end names no prompt and a role's own background result starts turns the daemon
// never prompted.
//
// A stale-plugin relaunch held for this turn (refreshPlugin) runs here, before the queued task's
// send: the claim's process speaks a plugin contract that cannot answer an update round, so it is
// replaced now that its worker has finished the turn, and the task waiting behind it goes to the
// process the relaunch starts rather than to the one it replaces. The settlement runs first, so
// the turn's own finished task retires before the relaunch rather than being taken back and sent
// again to a worker that already did it. refreshPlugin is what decides it, not relaunchStale: by
// now a registration may have recorded this daemon's contract, and the claim's state and held
// suspension are what they are at this moment, not what they were when the sweep found it.
//
// That re-decision can come to nothing at all — the registration above is exactly the case
// (holdStaleRelaunch) — so the send runs after it rather than in its place: a task left unsent
// there would wait for an event nobody is going to post. A relaunch that did run leaves the claim
// launching, which sendPending hands nothing (delivery.go), so the send costs that case nothing.
//
// The turn's end is logged here (turnEndedLine), and this is the only exit from working that
// writes that line. The line names what this turn's own start line named (startTurn, endTurn),
// because no reading of the claim can answer what the ending turn was: the window this same
// function's resultPending branch leaves is one where the claim holds a task confirmed — its
// turn ended before Oh My Pi said which turn the prompt's was, so settle retired nothing and the
// task is still in the worker's hands — while the turn now ending is a turn of the agent's own,
// which started naming no delivery. Every other way out of a turn writes a line of its own
// instead, which is what a reader who found a start and no end looks for:
//
//   - a death of the process (died): "supervise: process died";
//   - a held suspension whose deadline runs out (suspendAtTimeout): "supervise: the agent's turn
//     has not ended; suspending it mid-turn";
//   - a failure of the claim (fail): "supervise: claim failed";
//   - a retirement (retire), which writes no line of its own: the agent's own exit says
//     "supervise: the agent reported its exit" before it, a stop that ended work of the agent
//     says "supervise: the relaunch ended work the agent had running" under the stop's cause
//     (recordEnd, endStopped), and the workflow's close of the issue or the tree retires the
//     claim with no line at all;
//   - a stale-plugin relaunch of a claim whose turn is not waited out (relaunchStale):
//     "supervise: the process speaks another plugin contract; relaunching it onto the current
//     plugin" — one that is waited out says "supervise: the process speaks another plugin
//     contract; its relaunch waits for the agent's turn to end" and comes back through here.
//
// Widening the turn line to every one of those exits is not this change: each of them already
// says in its own words what became of the turn, and the pair of turn lines is the turn the
// worker ran.
//
// The question a restart armed goes with the turn (askFirst), and so does the record a busy
// answer made of the turn (busyTurn): the turn it stood for is over, however its end came — its
// agent_end, or the agent's answer to the sweep that it is in no prompt (askTurnOver).
//
// A suspension held for the turn runs here, now that the turn is over (holdSuspension). Its stop
// cuts off no turn, so what the let-go records reads the claim as idle, the state this end gives
// it (suspendHeldAtEnd): the next process is told it was in no turn, and the task the turn
// confirmed is no task cut off in its turn (recordEnd). A project pause's hold waits instead for
// the result of the turn's prompt where Oh My Pi has not written it yet (stopWaitsForResult), and
// runs from that result (promptResulted, stopAtResult): only the result says whether the turn
// finished its task or a provider refused it, and a stop made first would retire a refused task
// the role is still owed.
//
// Every end this records is counted (TurnsEnded), the compaction's below included, so the daemon
// can post it to the issue's other claims once this decision is published (its wakeIssue): a phase
// task one of them holds rather than adopt the working copy they share under this turn may go now.
//
// A claim working because its session is being compacted is left exactly where it is: a
// compaction aborts whatever turn its session was running, so the agent_end of a background
// turn can arrive under one, and taking that for the compaction's end would hand the claim its
// owed task while Oh My Pi is still writing the summary. CompactEnded is the only end of a
// compaction (compactEnded). The end is recorded on the compaction all the same: the turn it
// ends is over, so the compaction's end has none to leave the claim working for, and a claim
// that has had a turn goes back idle rather than ready. Its end line is written before that
// return (endTurn), because the turn ended, and it closes a start line of its own: a turn that
// starts under a compaction writes one too (turnStartedWhileBusy), so a reader counting starts
// against ends finds the pair.
func turnEnded(m *Machine, ctx context.Context, _ Event) error {
	m.endTurn()
	m.turnsEnded.Add(1)
	m.claim.LastTurnEnd = m.deps.Clock.Now()
	m.claim.Budgets.Deaths = 0
	if m.compacting != nil {
		m.log.Info("supervise: a turn ended while this claim's session is being compacted; the claim stays working until the compaction ends")
		m.compacting.inTurn, m.compacting.back = false, StateIdle
		return nil
	}
	m.askFirst, m.busyTurn = false, false
	if m.held != nil && !m.stopWaitsForResult() {
		if err := m.suspendHeldAtEnd(ctx, StateIdle); err != nil {
			if m.held == nil {
				// The stop landed and let the process go; only its write failed, and the claim is
				// suspended in memory (suspended).
				return err
			}
			m.claim.State = StateIdle
			return errors.Join(err, m.persist(ctx))
		}
		return nil
	}
	m.claim.State = StateIdle
	if err := m.persist(ctx); err != nil {
		return err
	}
	if err := m.settle(ctx); err != nil {
		return err
	}
	if m.resultPending() {
		// Oh My Pi has not said yet which turn this prompt's was, so settle retired nothing and
		// the task is still in the worker's hands. What is held for the turn runs from the
		// result instead (promptResulted), which is where this turn is over for it.
		return nil
	}
	return m.afterTurn(ctx)
}

// afterTurn runs what the machine held for the agent's turn, once that turn is over for the
// delivery too: its end seen and its prompt's result in, whichever came first (settle,
// resultPending). What it holds reads the worker's hands — the stale-plugin relaunch would take
// an unretired task back from the process it replaces (refreshPlugin) — so it may not run while
// the task whose turn just ended is still unsettled. The send runs last, as it does from every
// other decision.
//
// The held relaunch is the refresh gate's second asker (RefreshGate, dispatch://INFRA-2712 R1):
// the claim took no credit while its turn ran (holdStaleRelaunch), so the relaunch run here
// reaches refreshPlugin with none and asks the gate at that guard, which is where every
// uncredited relaunch of this lane asks and which states why the ask lives there rather than
// here. The claim here is never the controller's, which that guard spares: m.staleRelaunch is
// written by holdStaleRelaunch alone, and that is reached under StateWorking and no controller.
// Refused, the relaunch is left to the next sweep, which posts StalePlugin for a claim that is
// idle by then and relaunches it on the first credit that comes free — a minute at the rate the
// lane is held to, against the hours a lane with no gate costs every other wake of the daemon.
// The mark is not re-armed for that: it waits for a turn, and this turn has ended. The pending
// send does not wait on the answer either: it runs whichever way the gate answered.
func (m *Machine) afterTurn(ctx context.Context) error {
	if held := m.staleRelaunch; held != nil {
		m.staleRelaunch = nil
		if err := m.refreshPlugin(ctx, *held, nil); err != nil {
			return err
		}
	}
	return m.sendPending(ctx)
}

// blindTurnEnded is a turn ending on a claim the daemon never saw start one: it is ready or idle,
// so no delivery of its own was in flight, and what the end tells it is that the agent's own turn
// is over. A task it holds that no agent has read goes now (sendPending, which holds it back from
// a turn in flight on this claim or another of its issue, so R6's fence stands); a task already
// read, or no task at all, leaves nothing to do, as this event did before.
//
// A busy answer no longer leaves a claim here holding its task: it records the claim working
// (recordBusyTurn), so the end of the turn it names reaches turnEnded. What still leaves a ready or
// idle claim holding a task no agent has read, while its agent is in a turn whose start this daemon
// never read, is a send that has not reached the agent — one waiting for the shim's hello, one held
// for another claim's turn on the issue, one lost on the way — and this end is the first word that
// the agent can take it.
func blindTurnEnded(m *Machine, ctx context.Context, _ Event) error {
	p := m.claim.Pending
	if p == nil || !p.ConfirmedAt.IsZero() || !p.DeliveredAt.IsZero() {
		return nil
	}
	m.log.Info("supervise: a turn this daemon never saw begin has ended; the task it holds unread is sent now",
		"delivery", p.ID, "phase", string(p.Phase), "generation", p.Generation)
	return m.sendPending(ctx)
}

// issueTurnEnded is a turn ending on another claim of this claim's issue. It carries nothing the
// claim records: the send it may unblock asks for itself which claims of the issue are in a turn
// (sendPending), so a wake that arrives while another role has started one holds the task again,
// and a wake that never arrives — a turn whose process died instead of ending it — costs nothing,
// because the sweep's next observation asks the same question.
func issueTurnEnded(m *Machine, ctx context.Context, _ Event) error { return m.sendPending(ctx) }

// promptResulted is Oh My Pi's answer to the one question the turn frames cannot settle: which
// turn was this prompt's. The prompt's work is over, so the delivery's life is over with it —
// whether the claim is idle because the turn already ended (the usual order, the result landing
// milliseconds after that turn's agent_end) or still working because the result came first. The
// retire itself is settle's, which runs around this decision as it does around every other.
//
// The prompt may be any prompt of the task still outstanding (Machine.outstanding), not only the
// one the pending id names: the wait for its turn may have run out and taken the task back under
// a new id, and a later send's busy answer may have moved it again, while the prompt's own turn,
// late, was the session's next all along. A result that says it ran a turn is the task's turn
// over, whichever id it names, and it ends the wait for every prompt of the task at once: the
// pinned Oh My Pi runs one admitted prompt at a time, so no other prompt of the task can be
// running, and one acknowledged before it that has no result yet never will.
//
// A prompt that ran no turn at all (AgentInvoked false) is the definitive form of what the wait
// for the turn (TimerTurn) otherwise only times out on: it is charged the same, and the task goes
// back to waiting with its read mark, since a prompt the agent took and answered without a turn
// may still have been read. One whose wait already ran out was charged then, and is answered by
// waitedOutAnswer, which charges nothing: what it can take back is a late start taken for it, a
// turn that was then another input's, and the task goes back to waiting for that turn's end; and
// a busy answer of such a prompt records the claim working, as the busy answer below does.
//
// Oh My Pi's busy answer (agentBusy) is the exception here as it is for a send it refused
// outright (refused) and for a refusal after its acknowledgement (lateRefused): the agent ran no
// turn for this prompt because it is in a turn of its own, so it is working rather than refusing
// the work, and the task goes back UNREAD — a prompt answered that way was never read — charged
// nothing, for that turn's end to send again. Charged, this was the third path's own loop: a
// relaunch resumes the session, the resume starts a turn, the task that follows meets it, three
// such answers retire the claim, and the retirement launches again, at a full session resume a
// cycle (dispatch://INFRA-3103). A ready or idle claim is recorded working for that turn
// (recordBusyTurn), so the task waits for its end — its agent_end, or the sweep's answer that the
// agent is in no prompt (askTurnOver) — rather than going into it again at the next sweep. While an
// earlier prompt of the task is still outstanding, the turn it is in is that prompt's: the task
// keeps its confirmation and its read mark for it (takeBackPending), the claim is recorded working
// all the same, and that prompt's start confirms the task and its result settles it.
//
// A result for a delivery the claim no longer holds — retired, or taken back under a new id with
// no prompt of it outstanding — is fenced out before this runs (fence).
//
// A turn that ended in a provider error does not retire its task (providerErrored): the work is
// still owed, and the daemon owes the worker another prompt of it rather than an idle claim
// (dispatch://INFRA-956 R2). A turn that ended with no error is the one that clears the claim's
// run of provider-error turns.
//
// A turn already ended is over for its delivery here, so this is where what was held for that
// turn runs when the end found the result still outstanding (afterTurn): settle first, so the
// round's end and the stale-plugin relaunch read a worker whose hands are empty. A result that
// beat its turn's end leaves both to that end. A project pause's hold that waited for this result
// (stopWaitsForResult) stops the claim here instead (stopAtResult), once the result has decided
// the task: a refused task taken back is one the stop owes the role a launch for, and a finished
// one is retired by the settle that follows every decision (Handle).
//
// A result that says the turn ran with no provider error makes final what the task's confirmation
// credited of news of the approved design: the delivery is marked so (Delivery.Ran), and the write
// that records the mark folds the copy into the told rows (the claim store's settleDesignNews), so
// the retirement that follows ends nothing of it. Until then the credit is the delivery's own row,
// so a task a newer one replaced while its turn still ran, or whose turn ended in a provider error
// that the fence then dropped as stale, owes its news again once that row goes. A write of the mark
// that fails leaves the delivery unsaved, and settle's next write of it folds the copy then. A task
// this result confirms is confirmed with the news its own prompt carried (promptNews), or, for a
// prompt this daemon did not see acknowledged, the copy the task holds.
func promptResulted(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(StreamPromptResult)
	if !r.AgentInvoked {
		busy := agentBusy(r.Error)
		why := "acknowledged, and Oh My Pi ran no turn for it"
		if r.Error != "" {
			why += ": " + r.Error
		}
		if r.DeliveryID != m.claim.Pending.ID {
			return m.waitedOutAnswer(ctx, r.DeliveryID, !busy, busy, why)
		}
		m.answered(r.DeliveryID)
		if busy {
			if m.awaitsResult() {
				m.log.Info("supervise: Oh My Pi ran no turn for the prompt; it is busy with the turn of an earlier prompt of this task, whose result settles the task",
					"delivery", r.DeliveryID, "outstanding", m.outstanding, "error", r.Error)
			} else {
				m.log.Warn("supervise: Oh My Pi ran no turn for the prompt; it is in a turn of its own, whose end sends the task again",
					"delivery", r.DeliveryID, "error", r.Error)
			}
			return errors.Join(m.takeBackPending(ctx, taskUnread), m.recordBusyTurn(ctx))
		}
		return m.promptFailed(ctx, why, taskRead)
	}
	ran, acknowledged := m.promptNews(r.DeliveryID)
	m.forgetOutstanding()
	if r.Error != "" {
		if err := m.providerErrored(ctx, r); err != nil {
			return err
		}
		return m.stopAtResult(ctx)
	}
	if m.providerErrorRunOver("supervise: a turn of this task ended with no provider error; the run of provider errors is over",
		"delivery", r.DeliveryID) {
		if err := m.persist(ctx); err != nil {
			return err
		}
	}
	if p := m.claim.Pending; p.ConfirmedAt.IsZero() {
		// No turn was ever matched to this prompt — each one the claim saw was taken for another's
		// — and yet Oh My Pi ran one for it: the agent did read the task.
		m.log.Info("supervise: the task's prompt ran a turn the daemon never matched to it; confirming it from the result",
			"delivery", r.DeliveryID)
		if !acknowledged {
			ran = p.Carried
		}
		if err := m.confirmCarrying(ctx, ran); err != nil {
			return err
		}
	}
	if p := m.claim.Pending; len(p.Carried) > 0 && !p.Ran {
		p.Ran = true
		if err := m.putPending(ctx); err != nil {
			return err
		}
	}
	if m.claim.State == StateWorking {
		return m.sendPending(ctx)
	}
	if m.pauseHoldAtResult() {
		return m.suspendHeld(ctx)
	}
	if err := m.settle(ctx); err != nil {
		return err
	}
	return m.afterTurn(ctx)
}

// waitedOutAnswer is the answer, with no turn run, of a prompt of the task whose wait for its turn
// already ran out (noTurn): a result that says it was not invoked, or a refusal, naming an
// outstanding id other than the pending one. The wait took the task back under a new id and
// charged that prompt then, so nothing is charged now: charging again would spend the budget
// twice for one prompt.
//
// What the answer can still take back is a confirmation a turn took for this prompt. With no other
// prompt of the task outstanding and no send of it in flight, the turn that confirmed the task was
// matched to this prompt (turnStarted), and the prompt says it never ran, so that turn was another
// input's: the task goes back to waiting (takeBackPending), and that turn's end sends it — or,
// when the end has already come and left what it held to this result (turnEnded, resultPending),
// it goes from here (afterTurn), as it does from a result of a turn that ran (promptResulted).
// While another prompt is outstanding, the turn and the read mark are that prompt's, and nothing
// moves. Otherwise only the mark goes, where the answer says the prompt was never read, as a
// refusal of the prompt that set it always took it (markUnread). read says whether the agent may
// have read it, as promptFailed's does: a busy answer or a refusal was never read.
//
// busy is whether the answer is Oh My Pi's busy answer (agentBusy): the agent is in a prompt now,
// so this is one more of the places the daemon learns that its session is busy, and a ready or
// idle claim is recorded working for that turn (recordBusyTurn) wherever the task is left
// unconfirmed — while another prompt of the task is outstanding, and where only the mark goes —
// so that no sweep or hello sends the task into the turn the agent named. A task a turn has
// confirmed is never sent (sendPending), and in a ready or idle claim the turn that confirmed it
// has already ended, so there is nothing to record. Where the answer takes a confirmation back,
// the claim is working while that turn runs, and recorded working only if it is ready; one left
// idle had that turn's end come already, and the answer names the turn that end closed, so the
// task goes from here as before rather than waiting a sweep for a turn that is over.
func (m *Machine) waitedOutAnswer(ctx context.Context, id string, read, busy bool, why string) error {
	m.answered(id)
	p := m.claim.Pending
	record := busy && p.ConfirmedAt.IsZero()
	switch {
	case m.awaitsResult():
		m.log.Info("supervise: a prompt whose wait for its turn ran out was answered with no turn; another prompt of the task is still outstanding, so the turn is still that one's",
			"delivery", id, "outstanding", m.outstanding, "why", why)
		if record {
			return m.recordBusyTurn(ctx)
		}
		return nil
	case !p.ConfirmedAt.IsZero() && m.send == nil:
		m.log.Warn("supervise: a prompt whose wait for its turn ran out was answered with no turn, so the turn taken for it was another's; the task goes back to waiting",
			"delivery", id, "pending", p.ID, "why", why)
		if err := m.takeBackPending(ctx, read); err != nil {
			return err
		}
		if m.claim.State == StateIdle {
			return m.afterTurn(ctx)
		}
		if busy {
			return m.recordBusyTurn(ctx)
		}
		return nil
	}
	m.log.Info("supervise: a prompt whose wait for its turn ran out was answered with no turn; it was charged when the wait ran out, so nothing more is charged",
		"delivery", id, "why", why)
	if read {
		return nil
	}
	if record {
		return errors.Join(m.markUnread(ctx), m.recordBusyTurn(ctx))
	}
	return m.markUnread(ctx)
}

// resultPending is whether the claim is still waiting on the result of a prompt of its pending task
// it has had acknowledged (Machine.outstanding): the window in which a turn that ends may be any
// turn at all. It is open only while the agent can still answer — a claim whose process a
// suspension, a death or a retirement has taken is told nothing more, and the turn's end is all
// the answer there will ever be.
func (m *Machine) resultPending() bool {
	if m.claim.Pending == nil || !m.awaitsResult() {
		return false
	}
	switch m.claim.State {
	case StateReady, StateIdle, StateWorking:
		return true
	}
	return false
}

func bootInterval(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, func(context.Context) error {
		m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
		return nil
	}, TimerBoot, m.deps.Timeouts.Boot)
}

// registrationDeadline is a process that has had its boot intervals and whose agent never became
// ready — it never registered, or registered and never said it was ready. Without a held
// suspension, it is retired, counted and relaunched. A relaunch the tree lifecycle refuses is
// checked before the retirement, so the process is kept and the deadline tried again at the probe
// interval, as one whose stop fails is. An operator suspension retained through a rule-resuming
// launch instead completes here: no ready will arrive to run it, and relaunching would drop the
// operator's hold. The retirement's stop records what it ended as the deadline's (endNeverReady,
// letGo) — what its agent never did is no turn, so a record comes only of background work the
// process reported — and the relaunch that follows lets nothing more go.
//
// The deadline of a process of a claim that records a session and no file is charged nothing
// where endUncharged says so: a process an earlier release started, whose registration the fence
// refused, and a start that had Oh My Pi resume the session by its id and never registered as it.
// Its process is retired all the same, and relaunched through launch, which charges no launch
// failure and waits no backoff, in place of relaunchAfterFailure. An agent that registered as the
// session records its file, so its deadline is charged as every other one is.
func registrationDeadline(m *Machine, ctx context.Context, _ Event) error {
	if m.held != nil && m.held.By == SuspendedByOperator {
		return m.suspendHeld(ctx)
	}
	never := "never registered"
	if m.claim.State == StateRegistered {
		never = "registered and never said it was ready"
	}
	return m.probe(ctx, func(ctx context.Context) error {
		if err := m.checkLaunch(ctx); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose agent %s: %w", m.claim.Token, never, err)
		}
		incarnation := m.claim.Locator.Incarnation
		if err := m.suspendProcess(ctx, endNeverReady(never)); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose agent %s: suspend: %w", m.claim.Token, never, err)
		}
		if m.endUncharged("the registration deadline") {
			// An end charged nothing counts against no budget, so its line names none.
			m.log.Warn("supervise: the agent "+never+"; retired its process", "incarnation", incarnation)
			return m.launch(ctx, noEnd)
		}
		// The line names the claim (the machine's logger carries it), what its agent never did, and
		// the launch this retirement counts against its budget (relaunchAfterFailure counts it next),
		// so a controller whose role claim waits out its registration deadline shows in the daemon's
		// log as that, attempt by attempt.
		m.log.Warn("supervise: the agent "+never+"; retired its process", "incarnation", incarnation,
			"launchFailure", m.claim.Budgets.LaunchFailures+1, "limit", m.deps.Limits.LaunchFailures)
		return m.relaunchAfterFailure(ctx, CauseNeverRegistered, noEnd, time.Time{})
	}, TimerRegistration, m.deps.Timeouts.Probe)
}

func noTurn(m *Machine, ctx context.Context, _ Event) error {
	// The bound alone keeps the mark: a turn that has not started within it may still be this
	// task's, starting late, and the agent read the prompt when the turn began. The prompt stays
	// outstanding too (Machine.outstanding): the pinned Oh My Pi acknowledges a prompt before its
	// pre-turn work, which can outlast the bound many times over, so its turn is still the
	// session's next — its start confirms the task and its result settles it. The charge and the
	// next trigger's send stand as they were: nothing here holds the task for that prompt, so one
	// that never starts a turn and never answers still walks the budget to the retirement.
	return m.promptFailed(ctx, fmt.Sprintf("acknowledged, and no turn started within %s", m.deps.Timeouts.RPC), taskRead)
}

// spawn launches a queued claim, at the turn its caller reserved when it carries one
// (RequestSpawn.Turn), and otherwise at the turn the gate reserves.
func spawn(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestSpawn)
	if err := m.startHolds(ctx, request.Start); err != nil {
		return err
	}
	return m.revive(ctx, exemptWake(ev), request.Turn, request.Start, nil)
}

// register records the session the agent became, once every name of it is held to this claim
// (holdRecorded): a session another claim recorded first, by its id or by its file, is refused
// before anything of the registration is recorded or enrolled. The session id and the file arrive
// as the store keeps them, as every event's text does (Handle), so the hold, the claim in memory
// and the stored claim name the same session, and a restart reads back what was held: a session
// id or a file holding a NUL is held as it is stored. It writes the session and its file together,
// and it is the first registration after any launch, since it answers a claim still booting: a
// claim that recorded a session and no file, resumed by the id, records the file again here, with
// the file's names held, and the mark of a start that had Oh My Pi resume the session by its id is
// cleared in the same write (Claim.AgentResumeGeneration), since that start has registered. The
// boot intervals end with the registration, but not the deadline: it is armed again at its base
// bound, and only the agent's ready ends it. A registration whose answer never reached its agent
// leaves a claim no ready will ever come for, and from here that looks exactly like an agent slow
// to boot. A registration whose plugin still differs from the daemon's is relaunched at once and
// refused (staleRegistration): the agent that just registered is not the one that relaunch starts.
func register(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	if err := checkSessionNames(r.Session, r.SessionFile); err != nil {
		return err
	}
	if err := m.holdRecorded(ctx, r.Session, r.SessionFile); err != nil {
		return err
	}
	m.claim.Session, m.claim.SessionFile = r.Session, r.SessionFile
	m.claim.AgentResumeGeneration = 0
	m.claim.CapabilityHash = bytes.Clone(r.CapabilityHash)
	m.claim.PluginContract = r.PluginContract
	if r.PluginContract == m.deps.PluginContract {
		m.claim.PluginRelaunchFor = 0
	}
	m.claim.State = StateRegistered
	m.disarm(TimerBoot)
	m.armRegistration()
	if err := m.persist(ctx); err != nil {
		return err
	}
	// A stale registration's process is let go by its relaunch, so it is never enrolled: the
	// refusal returns before the enrollment a current registration gets.
	if err := m.staleRegistration(ctx, r.PluginContract); err != nil {
		return err
	}
	return m.ensureEnrolled(ctx)
}

// reregister is the recorded session registering again: it is issued a new secret, and the
// deadline stays where the first registration set it, so an agent that keeps registering and never
// says it is ready is still relaunched. The session is held as register holds it, by every name of
// it: one the claim recorded before the daemon held sessions is the claim's, unless another claim
// recorded it too, when it is held for neither (the claim_sessions migration). It writes the file
// alone, since the session is the one register recorded with a file at this launch's first
// registration. A registration whose plugin still differs from the daemon's is relaunched at once
// and refused the same way register's is (staleRegistration).
func reregister(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	if err := checkSessionNames(r.Session, r.SessionFile); err != nil {
		return err
	}
	if err := m.holdRecorded(ctx, r.Session, r.SessionFile); err != nil {
		return err
	}
	m.claim.SessionFile = r.SessionFile
	m.claim.CapabilityHash = bytes.Clone(r.CapabilityHash)
	m.claim.PluginContract = r.PluginContract
	if r.PluginContract == m.deps.PluginContract {
		m.claim.PluginRelaunchFor = 0
	}
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.staleRegistration(ctx, r.PluginContract)
}

// ready is the agent saying it can be prompted. The time is kept on the claim (Claim.LastReady),
// since a death recorded before this ready (Claim.LastDeathAt) is one the agent running now has
// survived (machine.go's Claim).
func ready(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateReady
	// The cause of a forget is kept for the launches of the fresh agent until it comes up
	// (Machine.lostCause): the first launch after this ready continues a session of its own.
	m.lostCause = ""
	m.claim.LastReady = m.deps.Clock.Now()
	// The pending cause and the last start failure go with the launch failures they name: the
	// episode they measure ended when this agent came up (Claim.PendingCause,
	// Claim.LastStartFailure). A start failure kept past it would read, on a claim launching
	// again later, as the reason it is not running now (dispatch://INFRA-5463).
	// The start failure's time goes with it (Claim.LastStartFailureAt).
	m.claim.Budgets.LaunchFailures, m.claim.PendingCause = 0, ""
	m.claim.LastStartFailure, m.claim.LastStartFailureAt = "", time.Time{}
	m.disarm(TimerRegistration)
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	return finishReady(m, ctx)
}

// finishReady persists the current claim before publishing ready or delivering work. reready may
// reach it while the claim is idle or working, so it must not rewrite the state to ready.
//
// Between the ready's durable report and the send, a claim that holds no delivery while its
// record of what the last let-go ended is untold is given the daemon's own delivery of that
// record (queueRelaunch), so the send carries the sentence to the new process at once. The order
// is the point: the report is the workflow's ready, read in its own transaction while this
// decision runs (Machine.OnDurableState, the daemon's applyTerminal), and it must find the claim
// holding nothing, or a phase worker's ready would queue no resume task (workflow's
// readyWorkerTask). A claim that holds a delivery is sent it with the sentence in front, and one
// whose held task that send drops instead is given the daemon's own delivery there
// (sendPending's relaunchAfterDrop).
func finishReady(m *Machine, ctx context.Context) error {
	if err := m.durableWrite(ctx, StateReady, m.persist); err != nil {
		return err
	}
	if err := m.queueRelaunch(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// reready persists the in-memory ready state before answering an agent retry: a prior ready's
// claim write may have failed after the machine made the transition. A retained launch-time
// suspension still wins: the retry completes that suspension instead of publishing ready and
// releasing notices to a claim the operator or workflow is stopping.
func reready(m *Machine, ctx context.Context, _ Event) error {
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	return finishReady(m, ctx)
}

func resume(m *Machine, ctx context.Context, ev Event) error {
	if err := m.resumeRefusal(ctx, "resume"); err != nil {
		return err
	}
	return m.revive(ctx, exemptWake(ev), time.Time{}, StartWork{}, nil)
}

// resumeRefusal is a RefusedError when a suspended claim's recorded session is one it does not
// hold, asked before anything of the claim changes, and otherwise nil: a claim recording a session
// another claim holds, or one no claim holds, is refused its resume, and stays suspended, rather
// than launched into the hold's refusal at start, which charges each launch to its budget and
// fails the claim within it (acceptance of #4220). It asks the names a resume of the recorded
// session runs under: those the record gives (holdRecorded), and for every claim that records a
// session and no file those a resume by the id runs under (holdByID). For an Oh My Pi claim those
// are the names start holds, so an operator's resume is refused where the launch would be. A
// Claude Code claim that records a session and no file is asked them too, which is stricter than
// its launch: start sends that claim straight to the forget (startFresh) and asks no hold. Its
// remedy is a new session, `legion claims forget-session`, then the resume. A store that does not
// answer is no refusal: the resume goes on, and start asks again at the probe interval, charging
// nothing, as before. A claim recording no session resumes as before, and so does one holding its
// session, which costs no store call once admitted (R9).
func (m *Machine) resumeRefusal(ctx context.Context, request string) error {
	if m.claim.Session == "" {
		return nil
	}
	var err error
	if m.claim.SessionFile == "" {
		err = m.holdByID(ctx, m.claim.Session)
	} else {
		err = m.holdRecorded(ctx, m.claim.Session, m.claim.SessionFile)
	}
	var refusal claim.Refusal
	if !errors.As(err, &refusal) {
		return nil
	}
	return &RefusedError{State: m.claim.State, Request: request, Err: fmt.Errorf(
		"%s records session %s, which it does not hold (%w), so a relaunch that resumed it would be refused at every try until the claim failed; give it a new session first (`legion claims forget-session --claim %s --session %s`), then ask the %s again",
		m.claim.Token, m.claim.Session, err, m.claim.Token, m.claim.Session, request)}
}

// resumeByRule is the daemon's own resume of a suspended claim (RequestRuleResume): the same
// session relaunched, as the operator's resume relaunches it, unless an operator suspended the
// claim: that suspension holds until
// the operator resumes the claim. A launch that fails is charged to the claim's launch budget like
// any other, so a claim the rule resumes that keeps failing fails once the budget is spent, and a
// failed claim is never resumed by rule. A claim recording a session it does not hold is not
// resumed by rule either (resumeRefusal): it stays suspended, and the line says why.
func resumeByRule(m *Machine, ctx context.Context, ev Event) error {
	resume := ev.(RequestRuleResume)
	rule := resume.Reason
	if !m.claim.RuleMayResume() {
		m.log.Info("supervise: not resumed by rule: its suspension holds until the operator resumes it", "rule", rule, "suspendedBy", m.claim.SuspendedBy)
		return nil
	}
	if err := m.startHolds(ctx, resume.Start); err != nil {
		return err
	}
	if err := m.resumeRefusal(ctx, "resume by rule"); err != nil {
		m.log.Info("supervise: not resumed by rule: the claim records a session it does not hold", "rule", rule, "error", err)
		return nil
	}
	m.log.Info("supervise: resuming by rule", "rule", rule, "suspendedBy", m.claim.SuspendedBy)
	return m.revive(ctx, exemptWake(ev), time.Time{}, resume.Start, nil)
}

// retry is a failed or retired claim given another run: its budgets start over, and its session,
// when it has one, is relaunched after the process the claim last ran is gone. A pending delivery
// the claim kept goes once the agent is ready. Its event is the workflow's retry or the
// operator's own (operatorRetry), and the pacing reads each its own way (exemptWake).
//
// A retry that asks for a new session (`legion claims retry --fresh-session`, the controller's
// fresh_session) forgets the recorded session instead, so the launch spawns a fresh agent
// (freshSession). Every other retry, whoever makes it, resumes the recorded session with fresh
// budgets, whatever the claim's run of refused turns: a run that reached launch_failure_limit is
// the claim's own count of refusals, mostly ones that can clear, such as a team limit's 429 that
// refuses every session alike, and no sign that its session is bad (dispatch://INFRA-4508's R5).
//
// The failure record goes with the counts it names, and both go once the claim is admitted. A
// reader is served that record built from the counts the claim holds now (api.FailureViewOf),
// so zeroing the counts alone would serve the cause and the sentence of the failure being
// retried beside `attempts` of zeroes — a record of zeroes, which is what a notice of the same
// failure refuses to carry (workflow.noticeFailure) — for as long as this run has not reached
// its launch, which the wake pacing may park and a refused lifecycle recheck may leave
// unwritten. The claim being run again is what clears the record; the launch clearing it a
// second time (launchTries) costs nothing.
//
// Nothing is cleared before Store.AdmitClaim admits the claim, because the admission can refuse
// — the tree's cleanup is reserved — and a refused admission runs the claim no more than a
// refused relaunch does (relaunchRefused): the claim is left exactly as it was, with the record of
// the failure it still carries, which is what `legion claims list` reads off the live machine and
// what the next persist writes. A launch check that refuses after the admission — launchAfter's
// own, or a parked wake's recheck when its turn comes — finds the record already cleared on the
// live machine, while the store still holds it as AdmitClaim wrote it, until the next persist.
func retry(m *Machine, ctx context.Context, ev Event) error {
	// The workflow's retry names the work it brings the claim back for, judged before anything of
	// the claim changes; the operator's names none.
	var start StartWork
	if r, ok := ev.(RequestRetry); ok {
		start = r.Start
	}
	if err := m.startHolds(ctx, start); err != nil {
		return err
	}
	if fresh, why := freshSession(ev); fresh {
		// A session the claim does not hold is left behind only once its role no longer leads
		// to the process under it (mayForget); the retry is refused, and the claim left as it
		// was, until then.
		if err := m.mayForget(ctx, "retry"); err != nil {
			return err
		}
		// lastRefusal is the provider's words for the newest refused turn, which the claim stores
		// for the reason LastRefusal gives.
		m.log.Info("supervise: retrying in a new session", "why", why, "session", m.claim.Session,
			"refusedRun", m.claim.RefusedRun(), "lastRefusal", m.claim.LastRefusal())
		// The failure the claim carries is the fuller answer where it has one — a provider error
		// that cannot clear names its status, a death its cause — and the retry's own reason
		// answers for a claim that failed nothing, an operator's retry of a worker it stopped.
		m.forgetSession(cmp.Or(m.claim.Failure, why))
	} else if err := m.resumeRefusal(ctx, "retry"); err != nil {
		// A retry that relaunches the recorded session is refused it the way a resume is
		// (resumeRefusal), before the budgets start over.
		return err
	}
	return m.revive(ctx, exemptWake(ev), time.Time{}, start, func() {
		m.claim.Budgets, m.claim.PendingCause = Budgets{}, ""
		m.claim.Failure, m.claim.FailureCause, m.claim.FailedAt = "", "", time.Time{}
		m.claim.LeakCheckCause = ""
	})
}

// operatorRetry is the operator's retry of a claim that runs nothing: retry, for an architect —
// its tree's root, which no phase start relaunches (admission starts it with its tree, and a
// person's answer resumes it: workflow's StartArchitect and ResumeAwaitedRoot), or a child's own
// architect, which a start of the workflow's can also bring back (workflow's StartFirstPhase and
// reviveArchitect, for example) — and for a phase worker whose issue works no phase of its role,
// which the route has already established before it reaches the machine (api's retryStart starts
// the phase itself for every other one).
// The daemon's controller has a retry of its own, its keeper, so it alone is refused. The refusal
// reads the claim's role inside the machine's decision, which no re-role runs beside (Rerole holds
// the same lock), and the controller's claim holds no step and is never re-roled, so the refusal
// cannot race the decision it guards.
//
// It relaunches the session the claim records, unless the retry asks for a new one
// (RequestOperatorRetry.FreshSession): retry then forgets the session, so the launch spawns a
// fresh agent (freshSession). A launch whose session no session store holds any more starts a
// fresh agent too (startFresh).
func operatorRetry(m *Machine, ctx context.Context, ev Event) error {
	if m.claim.Role == claim.RoleController {
		return &RefusedError{State: m.claim.State, Request: "retry",
			Err: fmt.Errorf("%w; the daemon's controller keeper retries the controller", ErrRetryNotTheController)}
	}
	return retry(m, ctx, ev)
}

// stop ends one claim: the runtime releases it, and it retires, recording what the released
// process ended under the stop's own cause (endStopped, retire), so a retry that keeps the session,
// each of which endStopped's comment names, tells its next process the turn the stop cut off and
// the background work it ended. A release that fails changes nothing, so the stop can be asked
// again. The tree's root claim ends only with its tree: a retired root would leave the orphan
// sweep's known set, which would then take whatever the runtime holds for the root issue — under a
// sandbox, its Sandbox, and with it the pod and the disk that pod owns. Any other stop of it is
// refused, naming the operator's close when it is that close which ends the tree.
func stop(m *Machine, ctx context.Context, _ Event) error {
	if !m.claim.treeRoot() {
		return m.end(ctx, endStopped)
	}
	closable, err := m.treeClosable(ctx)
	if err != nil {
		return fmt.Errorf("stop %s: %w", m.claim.Token, err)
	}
	return &RefusedError{State: m.claim.State, Request: "stop", Err: rootStopRefusal(m.claim.State, closable)}
}

// treeClose is the workflow's close of the tree, which ends the root claim too. It is the
// workflow's own decision: it stops every claim of a tree whose linger expired, and the issue
// record it still holds for that tree is exactly what TreeClosable refuses on, so it is not asked.
func treeClose(m *Machine, ctx context.Context, _ Event) error { return m.end(ctx, noEnd) }

// issueClose is the workflow's close of a child issue as done: it releases the claim through the
// runtime, which ends at most this role's process and forgets the claim. It deletes no issue
// Sandbox — under Agent Sandbox a release deletes only the controller's, whose Sandbox is that
// claim's and nothing else (Runtime.Release, releaseControllerSandbox) — so an issue's pod and the
// disk it owns go with the separately fenced IssueSuspend, or with the tree cleanup once every
// claim of the tree has retired. That is why a merged close leaves the implementer's pod standing:
// its five siblings take this close and no IssueSuspend is queued (workflow/linger.go). A release
// that fails changes nothing, so the close can be asked again.
//
// The claim keeps the session it recorded, both halves of it. A done issue is reopened — a person
// moves it back, a production check fails — and its roles are started again on the same issue,
// where a session forgotten here would start each as a fresh agent that has read none of the work
// it is continuing (dispatch://INFRA-714 A10). The session id is what the Sandbox volume reaping
// reads as its evidence, and the resume path is the only handle either runtime resumes by, so
// dropping the path would be dropping the resume whatever the id said.
//
// A store that has since lost the session is not this decision's to guess at. Keeping the path
// costs one resume when the session is gone: the runtime answers runtime.ErrSessionFileMissing,
// the same try forgets the session and spawns a fresh agent, the claim is charged nothing, and
// the loss is reported to the issue, its architect and the controller, with the fresh agent told
// in its own prompt (Machine.start, startFresh, Deps.SessionLost; dispatch://INFRA-714 R8).
// Clearing the path here instead would reach that same fresh agent silently, which is the one
// outcome R8 forbids. Nothing of the workspace is recovered either way: a later start of the
// issue runs on a pod and a disk of its own, resuming the agent's session on it.
func issueClose(m *Machine, ctx context.Context, _ Event) error {
	if err := m.release(ctx); err != nil {
		return err
	}
	return m.retire(ctx, noEnd)
}

// forgetSession drops the named session record from a processless claim: the operator's
// `legion claims forget-session`, and the half of `legion claims retry --fresh-session` that is
// the claim's rather than the workflow's (api's retryClaim). It launches nothing — whatever starts
// the claim next, for a phase worker the workflow's own start of its phase, spawns a fresh agent
// rather than resuming the turns the operator asked to leave behind. The comparison runs under
// the machine lock, so a decision that changed the claim since an operator saw it cannot clear a
// later session. A session the claim records but does not hold is cleared only once the Envoy
// listener no longer names it as the claim's role holder (mayForget).
func forgetSession(m *Machine, ctx context.Context, ev Event) error {
	request := ev.(RequestForgetSession)
	if !m.claim.HoldsSession(request.Session) {
		return &RefusedError{Request: "forget-session", Err: fmt.Errorf(
			"%s records session %s; pass --session %s to start fresh", m.claim.Token, m.claim.Session, m.claim.Session,
		)}
	}
	if err := m.mayForget(ctx, "forget-session"); err != nil {
		return err
	}
	m.log.Info("supervise: forgetting the recorded session at the operator's ask", "session", m.claim.Session,
		"sessionFile", m.claim.SessionFile, "state", string(m.claim.State))
	m.forgetSession("an operator cleared it with `legion claims forget-session`, so this claim starts fresh")
	return m.persist(ctx)
}

// operatorClose is the operator's own close, asked of a claim through the API. TreeClosable is
// asked here rather than by the caller, so the answer and the stop it decides sit together rather
// than a round trip apart; it does not lock the record it reads.
func operatorClose(m *Machine, ctx context.Context, _ Event) error {
	if err := m.closeRefusal(ctx); err != nil {
		return err
	}
	return m.end(ctx, noEnd)
}

// operatorCloseRetired answers a close of a claim that has already retired. The outcome the
// operator asked for holds, so an operator retrying after a timeout is not told their close
// failed — but only for a close that would have been allowed: a claim that is not its tree's
// root, and a tree a workflow issue backs, are refused whatever state the claim is in. The caller
// reads a nil here as the close that did it and goes on to stop the rest of the tree, which is
// the whole tree's fate to hand to a claim that never held it.
func operatorCloseRetired(m *Machine, ctx context.Context, _ Event) error {
	return m.closeRefusal(ctx)
}

// closeRefusal is the refusal an operator's close earns, or nil. It is the two questions the
// close turns on, asked where the close is decided rather than a round trip before it: whether
// this claim is its tree's root, and whether a workflow issue backs the tree. Neither reads the
// claim's state, so a retired claim answers them the same way a live one does.
func (m *Machine) closeRefusal(ctx context.Context) error {
	if !m.claim.treeRoot() {
		return &RefusedError{State: m.claim.State, Request: "close",
			Err: fmt.Errorf("%s is not its tree's root claim; stop it instead", m.claim.Token)}
	}
	closable, err := m.treeClosable(ctx)
	if err != nil {
		return fmt.Errorf("close %s: %w", m.claim.Token, err)
	}
	if !closable {
		return &RefusedError{State: m.claim.State, Request: "close",
			Err: fmt.Errorf("%s is a workflow issue's tree, which closes when its linger expires", m.claim.Tree)}
	}
	return nil
}

// treeClosable is Deps.TreeClosable's answer for this claim's tree; with none, every tree closes.
// Each caller names the request it answers when the read fails.
func (m *Machine) treeClosable(ctx context.Context) (bool, error) {
	if m.deps.TreeClosable == nil {
		return true, nil
	}
	return m.deps.TreeClosable(ctx, m.claim)
}

// end releases the claim's process and retires the claim, which is what every stop and close does
// once it is allowed, recording what that process ended under cause (retire): the stop's own
// (endStopped), and noEnd for a close, which ends the claim's work on purpose.
func (m *Machine) end(ctx context.Context, cause string) error {
	if err := m.release(ctx); err != nil {
		return err
	}
	return m.retire(ctx, cause)
}

// rootStopRefusal is ErrRootStop with what stops the root's process in state instead: a suspension,
// which takes a registered agent, and nothing where no process runs. A tree no workflow issue backs
// (closable) ends by the operator's close, which the refusal names; a workflow issue's tree ends
// when its linger expires.
func rootStopRefusal(state ClaimState, closable bool) error {
	var err error
	switch {
	case slices.Contains(processless, state):
		err = fmt.Errorf("%w; %s", ErrRootStop, noProcess)
	case slices.Contains(booting, state):
		err = fmt.Errorf("%w; suspend it to stop its process once its agent has registered", ErrRootStop)
	default:
		err = fmt.Errorf("%w; suspend it to stop its process", ErrRootStop)
	}
	if closable {
		return fmt.Errorf("%w; no workflow issue backs its tree, so legion claims close ends it", err)
	}
	return err
}

func deliverLater(m *Machine, ctx context.Context, ev Event) error {
	return m.queue(ctx, ev.(RequestDeliver))
}

func deliverNow(m *Machine, ctx context.Context, ev Event) error {
	if err := m.queue(ctx, ev.(RequestDeliver)); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// deliverResuming queues a task on a suspended claim and resumes it for that task. The workflow's
// task (an outbox delivery, which carries a durable id) resumes a claim unless an operator
// suspended it: an operator's suspension answers ErrHeldByOperator, queuing
// nothing, and its start waits for the operator's resume. The operator's own delivery resumes the
// claim whoever suspended it.
//
// Which of the two it is decides the pacing too, and the id is what says which (RequestDeliver.ID,
// which the API's deliver route leaves empty). The workflow's own task is a wake of the daemon's
// own and waits its turn like every other (reserveWake): the task waits on the claim until the
// launch it waits for runs. The operator's task never waits — a person is waiting on it, as they
// are on the resume and the retry the same route sends (exemptWake) — so it launches where it
// arrives and takes nothing from the rate. A wake the pacing already holds over the claim is
// dropped before this row is reached, so the claim this decides is the claim the gate found
// (dropsWaitingWake, dropWakeFor).
func deliverResuming(m *Machine, ctx context.Context, ev Event) error {
	d := ev.(RequestDeliver)
	if d.ID != "" && !m.claim.RuleMayResume() {
		return fmt.Errorf("deliver %s to %s: %w", d.ID, m.claim.Token, m.current().RuleHoldError())
	}
	// A delivery that resumes the claim relaunches its recorded session, so it is refused one the
	// claim does not hold before the task is queued (resumeRefusal).
	if err := m.resumeRefusal(ctx, "deliver"); err != nil {
		return err
	}
	if err := m.queue(ctx, d); err != nil {
		return err
	}
	return m.revive(ctx, d.ID == "", time.Time{}, StartWork{}, nil)
}

// exit is the agent reporting its own end. A worker's or sub-architect's claim ends with it and is
// released. The tree's root claim ends only with its tree, whose close is a stop: its exit suspends
// it instead, so it stays resumable and known to the orphan sweep, which would otherwise take
// whatever the runtime holds for the tree with it. The agent has ended either way, so a runtime
// that cannot release or suspend its process is logged and the claim moves all the same: a
// finished agent's claim never stays live, its secret still authenticating. A root's stop records
// what it ended as a suspension by its exit (endSuspended), as every suspension does; a worker's
// retirement records nothing (noEnd, retire), since the exit is the agent's own end of its work.
// The root's suspension sends the clean stop of its Envoy session once the runtime's stop has
// returned, and none for a stop the runtime refused (stopForSuspension).
func exit(m *Machine, ctx context.Context, ev Event) error {
	m.log.Info("supervise: the agent reported its exit", "reason", ev.(RequestExit).Reason)
	if m.claim.treeRoot() {
		incarnation := m.claim.Locator.Incarnation
		if err := m.stopForSuspension(ctx, SuspendedByExit, stopOwed); err != nil {
			m.log.Error("supervise: could not suspend the exited root's process; suspending its claim anyway",
				"incarnation", incarnation, "error", err)
		}
		return m.suspended(ctx, SuspendedByExit, nil)
	}
	if err := m.release(ctx); err != nil {
		m.log.Error("supervise: could not release the exited agent's process; retiring its claim anyway", "error", err)
	}
	return m.retire(ctx, noEnd)
}

// openPodUpdate records this claim's update round (PodUpdateOpen). The daemon's sweep posts the
// event at every tick while the round runs, so the action is idempotent.
//
// A round the claim is not in yet starts its clock here — UpdateOpenedAt — with the round's id
// and the reasons the post carries, as the store keeps them, as every event's text is (Handle: the
// reasons quote what the runtime read of the pod, so the claim the operator's answers read holds
// the reasons the store holds), and clears what an earlier round left: the ask it published, the
// signal it got, the head that signal reported, and the notice it wrote. The same round again
// changes nothing, its reasons included: the round's terms are fixed when it opens
// (dispatch://INFRA-2981 R9), so the ask the sweep publishes and the record an operator reads name
// the same reasons for one round id, however the drift the sweep reads moves while the round runs.
//
// The machine asks the role nothing. The sweep enqueues the round's ask to the Envoy topic of this
// claim once the claim records the round (internal/daemon's openPodUpdate, record.UpdateAsk), and
// the outbox marks the claim asked once that publish reached a session (Machine.PublishUpdateAsk):
// an ask that reached none marks nothing, so a later sweep asks again. A suspended claim is not
// revived for the ask, and its machine records the round all the same. A suspension by the daemon
// itself (Claim.PassedOverByRound) takes the claim out of the round's ask and out of its wait: the
// sweep asks it nothing and the round does not wait for its answer (internal/daemon's roundAsks,
// dispatch://INFRA-4109 R1). The one exception is a claim a project's pause stopped, while the
// daemon's podHolds.owes in internal/daemon holds (internal/daemon's roundClock.passesOver). An
// operator's suspension leaves the claim in both: if no ask has reached it, the sweep asks it once
// its agent is ready again, and the round waits for the operator whatever its bound
// (dispatch://INFRA-2981 R5).
//
// A round replaces another the moment the daemon opens it, whatever the claim's worker is doing.
// The answer the worker gives carries the round its own ask named (PodUpdateSignalled), so an
// answer the worker was writing under the old round's words is refused by that id rather than
// filed against the round that replaced it: an answer given under one round's words, for the
// work and the pod that round named, can never end the round that replaced it, which asks about
// a pod of its own. The switch clears the old round's answer and its publish mark with the rest
// of its fields (clearUpdate), so the sweep publishes the new round's ask, which names the new
// round's id. An ask an older daemon queued as a task for the old round goes in the same step,
// with the mark a restart armed for it (dropUpdateTask).
//
// The claim is written only where this post changed it: a round the claim was not in. The sweep
// posts to every claim of every open round at every tick, for as long as a round runs, and a post
// that decided nothing has nothing to write — as refreshPlugin's own re-post returns before any
// write. A round the claim was not in also lifts the hold an answer to the old round put on the
// claim's next task (answeredOpenRound), so that task is sent here (sendPending): nothing else
// would send it before the claim's next observation.
func openPodUpdate(m *Machine, ctx context.Context, ev Event) error {
	open := ev.(PodUpdateOpen)
	if m.claim.UpdateRound == open.Round {
		return nil
	}
	m.log.Info("supervise: an update round is open on this claim; the sweep asks its role to do the steps of the update message (UpdateTask) and answer",
		"round", open.Round, "reasons", open.Reasons)
	if err := m.dropUpdateTask(ctx); err != nil {
		return err
	}
	opened := m.deps.Clock.Now()
	m.clearUpdate()
	m.claim.UpdateRound, m.claim.UpdateReasons, m.claim.UpdateOpenedAt = open.Round, open.Reasons, &opened
	if err := m.persist(ctx); err != nil {
		return err
	}
	// The answer that was holding the claim's own next task back (answeredOpenRound) went with
	// the old round (clearUpdate), and nothing else would send that task until the claim's next
	// observation. sendPending hands nothing to a claim in a turn, and nothing to a task another
	// hold stands over, so a claim with no held task costs nothing here.
	return m.sendPending(ctx)
}

// updateTaskPrefix starts the delivery id of every ask an update round queued as a task, which
// only an older daemon did; the round and a fresh word of that ask's own complete it
// (updateTaskID). This daemon publishes the round's ask to the Envoy topic of the claim it asks
// instead (record.UpdateAsk), so an id of this shape is one a restored claim brings back from the
// store.
const updateTaskPrefix = "update:"

// updateTaskID is a delivery id of one ask of round, and names no other ask of it. A rotation
// makes one (rotateTaskID): the shim answers a prompt carrying the delivery id it already holds
// from its own record itself — an acknowledgement, the replayed agent_start and, for a turn
// already over, an agent_end — and prompts Oh My Pi with nothing (internal/shimwire's Dedupe), so
// a task going back to waiting takes an id the shim has no record of.
//
// What makes it this ask's own is a random word, as an operator's own delivery id is (queue), not
// the clock: identity that reads the clock is identity a clock that steps back repeats.
func updateTaskID(round string) string {
	return updateTaskPrefix + round + ":" + rand.Text()
}

// updatesRound says whether d is an ask of round: every ask of a round carries the round in its
// delivery id, whichever ask of it this is. updateRoundOf is the one reader of that id's shape,
// so no two readers of it can disagree about where the round ends and the ask's own word begins.
func updatesRound(d *Delivery, round string) bool {
	if d == nil || round == "" {
		return false
	}
	of, ok := updateRoundOf(d.ID)
	return ok && of == round
}

// updateRoundOf is the round whose ask carries delivery id, and false for an id no update round
// made. An ask's id is updateTaskPrefix, the round, and this ask's own word, so the round is
// everything between the prefix and the last colon: a round's own id carries one colon (PodRound,
// PluginRoundPrefix) and the word none.
func updateRoundOf(id string) (string, bool) {
	rest, ok := strings.CutPrefix(id, updateTaskPrefix)
	if !ok {
		return "", false
	}
	cut := strings.LastIndex(rest, ":")
	if cut <= 0 {
		return "", false
	}
	return rest[:cut], true
}

// rotateTaskID is the new id a task going back to waiting takes, under which the shim's record
// holds nothing: an ask of an update round keeps that round (updateTaskID), the daemon's own
// delivery of a relaunch's record keeps its prefix (relaunchTaskID), and every other task takes a
// fresh word, as an operator's own delivery id does (queue).
//
// The round must survive the rotation. A delivery id is the only record of which round an ask is
// of, and three paths rotate one in the middle of a round — a late agent_start (noTurn), Oh My
// Pi's "Agent is already processing" refusal (lateRefused), and a death mid-turn (interrupted).
// An ask that lost its round would be read as a phase task by every reader of it: updatesRound,
// dropUpdateTask and sendPending's drop. sendPending would then send the older daemon's words to
// the worker, and that text can tell the worker that the end of its turn is its answer.
//
// The relaunch's prefix must survive for the same reason: it is the only record that the
// delivery is the record's sentence and no work of the claim's. Lost, the delivery would be
// prompted as a task behind that same sentence (Delivery.message), and a newer task would wait
// behind it rather than take its place (replaceable).
func rotateTaskID(id string) string {
	if round, ok := updateRoundOf(id); ok {
		return updateTaskID(round)
	}
	if relaunchTask(id) {
		return relaunchTaskID()
	}
	return rand.Text()
}

// dropsUpdateTask is whether the claim's pending delivery is the ask of the round the claim
// records now that no turn has confirmed: the one delivery a round leaving the claim takes
// back. It is a question of its own, apart from the drop, for the claim that ends in the same
// step: the retirement reads it, clears the round's fields and then writes the claim and the
// delete together, where a drop made first would store a retired claim that still records its
// round (machine.go's retire).
func (m *Machine) dropsUpdateTask() bool {
	p := m.claim.Pending
	return updatesRound(p, m.claim.UpdateRound) && p.ConfirmedAt.IsZero()
}

// dropUpdateTask retires an ask of this claim's round that an older daemon queued as a task and
// that no turn has confirmed. This daemon queues no such task and sends none (sendPending drops
// it), but a claim it restored from the store can hold one, and a round leaving the claim takes
// it back in the same step: the words name a round that is over — a pod this daemon has already
// replaced — and while the claim holds them, every task of the claim's next phase is refused
// behind this one (ErrDeliveryPending). An ask the claim owes is one of those: a turn of it that
// ended in a provider error left it unconfirmed and waiting on the ladder. A task a turn
// confirmed is left where it is: that turn is the worker's, and its end retires the delivery
// (settle). Every end of a round for this claim runs it — the sweep's (endPodUpdate,
// leaveRound), the round a new one replaces (openPodUpdate), and the registration's
// (endCurrentPluginRound). The claim's own retirement makes the same drop in its one write
// instead (machine.go's retire).
//
// The mark a restart armed for that ask goes in the same step, here rather than in each caller
// (answerAskFirstAfterDrop): left armed, it would make the claim's next task the running turn's,
// confirmed and retired unsent, and one caller forgetting the second call is enough for that. It
// is answered whether or not this drop took a task back: the question is the pending delivery's,
// and a claim that still holds one is answered nothing there.
func (m *Machine) dropUpdateTask(ctx context.Context) error {
	if m.dropsUpdateTask() {
		m.log.Info("supervise: the update round is over for this claim; the ask an older daemon queued as a task and nothing read is taken back",
			"round", m.claim.UpdateRound, "delivery", m.claim.Pending.ID)
		if err := m.retirePending(ctx); err != nil {
			return err
		}
	}
	return m.answerAskFirstAfterDrop(ctx)
}

// endPodUpdate is the round over for this claim (PodUpdateEnded): the ask it left unread goes,
// every field the round wrote goes, and the task the round held back is released (leaveRound).
// It launches nothing, whatever kind of round it finds and whatever state the claim is in.
//
// A claim in no round ends nothing. The sweep posts the end from what it read at the start of a
// tick, so a claim whose round ended in between — this same post at an earlier tick, a round the
// registration ended (endCurrentPluginRound), a round cleared by a retirement — is posted an end
// for a round that is already over, and ending whichever round it stands in now would end a
// round nobody has weighed.
//
// A pod's round ends only once the pod it was opened over is gone: the replacement the round
// itself made, a pod replaced under it, or a pod that went for a failure of its own. Each role's
// process died with that pod, and the displaced path brings each one back on its own session, so
// nothing is launched here, whatever state the claim is in: a claim that failed while the round
// ran is left failed for the workflow's retry to decide, and sendPending hands a claim with no
// agent nothing (delivery.go). Leaving the round on it instead would outlive the pod the round
// was opened over, and the sweep reads the round of every claim.
//
// A plugin round is the other round this can find, and it leaves the claim the same way. No
// daemon on this release opens one — the stale-plugin lane replaces a stale process and asks its
// worker nothing (refreshPlugin, relaunchStale) — so the only claim in one is a claim an earlier
// daemon left there, and taking it out of the round is the whole of what such a claim needs: the
// task the round held back goes to it, and the lane relaunches its process at a paced wake of
// its own. Until the round was deleted the relaunch was made here instead, once the worker had
// answered the ask that lane no longer sends, and it was the one thing that made this end wait
// for a turn, read the claim twice and take the launch path's own refusal.
func endPodUpdate(m *Machine, ctx context.Context, ev Event) error {
	ended := ev.(PodUpdateEnded)
	if m.claim.UpdateRound == "" {
		m.log.Info("supervise: this claim is in no update round; the end posted for it decides nothing")
		return nil
	}
	if ended.Round != m.claim.UpdateRound {
		m.log.Info("supervise: the end posted for this claim names a round it is not in; it decides nothing",
			"ended", ended.Round, "round", m.claim.UpdateRound)
		return nil
	}
	m.log.Info("supervise: the update round is over for this claim", "round", m.claim.UpdateRound)
	return m.leaveRound(ctx)
}

// leaveRound is the round going from this claim with no process this machine replaces: the ask
// nothing read goes with the mark a restart armed for it (dropUpdateTask), every field the round
// wrote goes (clearUpdate), the claim is written, and the task the round was holding back is
// sent. It is the whole of every round's end: a pod's round, whose pod is gone by the time that
// end is posted (endPodUpdate), and a plugin round, which no daemon on this release opens and
// which ends wherever an earlier daemon's is found — at a registration that speaks this daemon's
// contract (endCurrentPluginRound), or at the end the sweep posts. They differ only in what they
// log. sendPending hands nothing to a claim that is not ready or idle, so a failed or suspended
// claim is sent nothing by it.
func (m *Machine) leaveRound(ctx context.Context) error {
	if err := m.dropUpdateTask(ctx); err != nil {
		return err
	}
	m.clearUpdate()
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.sendPending(ctx)
}

// endCurrentPluginRound ends a plugin round over a claim whose registration has just recorded
// this daemon's own contract (staleRegistration). What the round waited for has happened: the
// process it was opened over is gone, and the one that registered speaks this daemon's plugin.
// The ask nothing read goes with it, and the task the round held back is sent — a round left open
// here would hold that task until a sweep ended the round. A claim in a pod round is left exactly
// as it is: that round is its pod's, which a registration says nothing about.
//
// No daemon on this release opens a plugin round: the stale-plugin lane relaunches a stale
// process and asks its worker nothing (refreshPlugin). This stays for the claim an earlier
// daemon left in one and a restart on this release re-adopts — the relaunch that lane makes for
// such a claim brings its registration here — and it goes when no daemon that can write a plugin
// round is left to restart from.
func (m *Machine) endCurrentPluginRound(ctx context.Context) error {
	if !m.claim.InPluginRound() {
		return nil
	}
	m.log.Info("supervise: the registration speaks this daemon's plugin contract; its plugin round is over",
		"round", m.claim.UpdateRound, "pluginContract", m.claim.PluginContract)
	return m.leaveRound(ctx)
}

// clearUpdate drops every field an update round writes, which is what a round leaving this claim
// means: the round the sweep ends (endPodUpdate), the round a new one replaces, whose fields are
// each the earlier round's (openPodUpdate), and the round a claim retires in (retire).
//
// It is the one place the round a claim left goes too (UpdateLeftRound): the end of the round is
// the only thing that may forget any part of it, and a relaunch, a suspension, a letGo and a
// restart each leave it exactly as it is.
func (m *Machine) clearUpdate() {
	m.claim.UpdateRound, m.claim.UpdateReasons, m.claim.UpdateHead = "", "", ""
	m.claim.UpdateOpenedAt, m.claim.UpdateTaskedAt = nil, nil
	m.claim.UpdateSignalledAt, m.claim.UpdateToldAt = nil, nil
	m.claim.UpdateLeftRound = ""
}

// podUpdateSignalled records the worker's answer to the round's ask — its work is pushed, and the
// process holding this claim may now be replaced — and the head it reported, which nothing
// verifies. It changes no state: the round's own sweep is what reads the signal and replaces the
// process, at the round's bound, or before it once every claim the round asks has answered, no
// turn runs and no claim with a process in the round's pod reports background work that runs or
// has sent no report since its last turn started (internal/daemon's roundEnd.holding, the one
// statement of that rule). So an answer brings the round's end forward only while no role reports
// background work.
//
// A second answer for the same round records again, the newer head in place of the first: the
// ask tells a role whose subagent or background job finishes after its answer to push what that
// work made and to answer again with the same round (UpdateTask).
//
// The answer counts for one round only: the one whose update message the worker read, which that
// message named and which the answer carries back (UpdateTask). An answer for any other round is
// refused and records nothing — a worker whose round was replaced while it worked answers the
// round it read, and the round that replaced it promises something else of the pod and the disk,
// so counting that answer would replace a pod no worker has pushed for. A claim in no round at
// all is the same case: every answer names a round it is not in. The refusal names both rounds,
// and tells the worker to answer the round the newest update message names, or to end its turn
// (foreignRoundRefusal).
//
// An answer counts only from a process in the round's own pod. A claim the daemon started again
// in another pod answers for work the dead pod's disk held and that process never saw
// (Claim.LeftPodRound), so that answer is refused too, under a refusal of its own. What the
// claim has left is the round's own record rather than where its process is now, so a claim
// suspended since — its locator gone with its process (letGo) — refuses the late answer the
// same way.
//
// The line it logs is the one durable record of the head: the claim's UpdateHead goes with the
// round that ends (clearUpdate), and the answer is what an operator reads the replacement back
// against — `git ls-remote` of each head a worker reported says whether the work survived the pod.
// The head is the worker's word, and the claim holds it as the store keeps it, as it holds every
// event's text (Handle), so the operator's answers read the head the store holds.
func podUpdateSignalled(m *Machine, ctx context.Context, ev Event) error {
	signal := ev.(PodUpdateSignalled)
	if signal.Round != m.claim.UpdateRound {
		m.log.Warn("supervise: the worker answered an update round this claim is not in; the answer is refused",
			"answered", signal.Round, "round", m.claim.UpdateRound, "head", signal.Head)
		return foreignRoundRefusal(m.claim, signal.Round)
	}
	if m.claim.LeftPodRound(m.claim.UpdateRound) {
		m.log.Warn("supervise: this claim left its update round when the daemon started it again in another pod; the answer is refused",
			"round", m.claim.UpdateRound, "answeredWhenItLeft", m.claim.UpdateSignalledAt != nil, "head", signal.Head)
		return leftPodRoundRefusal(m.claim)
	}
	at := m.deps.Clock.Now()
	m.claim.UpdateSignalledAt = &at
	m.claim.UpdateHead = signal.Head
	m.log.Info("supervise: the worker answered the update round; its work is pushed and its process may be replaced",
		"round", m.claim.UpdateRound, "head", signal.Head)
	return m.persist(ctx)
}

// foreignRoundRefusal is what a worker reads when its answer names a round the claim is not in
// (podUpdateSignalled). Another agent reads it, so it follows ASD-STE100 as the ask it answers
// does: one instruction per sentence, active voice, and no word that has a second reading. It
// names the round the answer carried and the round the claim stands in, and then what the worker
// does next.
//
// What it does next is never "answer the round named here". A round that replaced another is
// asked of the claim only at a later sweep (internal/daemon's openPodUpdate publishes once the
// claim records the round), so the id this refusal prints is one whose words the worker may not
// have read, and an answer given for it would be counted for a round the worker was never asked
// (podUpdateSignalled is the one guard that stops exactly that). So the worker is told how the
// daemon asks — once per round, as a message on its own role topic, the topic of its claim — and
// to answer the round the newest such message names, or to end its turn where no message names the
// claim's round. For a claim the daemon is asking nothing of, it is told so.
func foreignRoundRefusal(c Claim, answered string) error {
	in, next := fmt.Sprintf("%q", c.UpdateRound), fmt.Sprintf(
		"The daemon asks you one time in each update round. "+
			"The daemon sends that ask as a message on your role topic. "+
			"Answer with the round that the newest such message names. "+
			"If no such message names round %q, the daemon has nothing to ask you now. End your turn",
		c.UpdateRound)
	if c.UpdateRound == "" {
		in, next = "no update round", "The daemon asks this claim for no answer now"
	}
	return &RefusedError{State: c.State, Request: "ready_for_update", Err: fmt.Errorf(
		"%w. It answers %q, and this claim is in %s. %s", ErrForeignUpdateRound, answered, in, next)}
}

// leftPodRoundRefusal is what a worker reads when it answers a round its claim has left
// (podUpdateSignalled). Another agent reads it, so it follows ASD-STE100 as the ask it answers
// does: one instruction per sentence, active voice, and no word that has a second reading.
//
// It names one round, the round the claim left, which is the round the answer carries. It names
// no round of the pod the claim's process runs in now, because there is none: no round is open
// over that pod, and a worker that answered the id of one would be refused again, as answering
// a round that is not its claim's (foreignRoundRefusal). Where the process runs is nothing the
// worker can act on either, so the refusal does not say it.
//
// What the worker does next is nothing of that round. The round asks about work on a disk that
// is gone, which no turn of this process can push, and the daemon ends the round itself.
func leftPodRoundRefusal(c Claim) error {
	return &RefusedError{State: c.State, Request: "ready_for_update", Err: fmt.Errorf(
		"%w. It answers %q. The daemon started this claim in another pod than that round's own. Do not answer %q again. The daemon asks this claim for no answer now",
		ErrLeftPodRound, c.UpdateLeftRound, c.UpdateLeftRound)}
}

func nothingToDo(*Machine, context.Context, Event) error { return nil }

package supervise

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/wakepace"
)

// The events. Each carries the claim it is for and the fields its fences read: an observation its
// incarnation, a hello its generation, a timer its arming, an agent's own request its generation
// and session.

// RuntimeObservation is a process fact from the runtime's sweep. The claim is its locator's.
type RuntimeObservation struct{ Observation runtime.Observation }

// StreamHello is the shim connecting with this claim's boot token, resolved to its generation.
// BacklogWhole is the hello's word that the connection is handed every frame the shim held for a
// daemon since its last connection (shimwire.Hello2.BacklogWhole), which a take a daemon start
// restored needs before a report can give it its turn back (bindRestoredAnchor); false is a shim
// that dropped frames or cannot say, after which the machine no longer counts the process's turns
// off the frames it saw (readBacklog).
type StreamHello struct {
	Claim        claim.Token
	Generation   uint64
	AgentSecrets *AgentSecretsIdentity
	BacklogWhole bool
}

// StreamTurnStart is the agent's turn starting.
type StreamTurnStart struct{ Claim claim.Token }

// StreamTurnEnd is the agent's turn ending.
type StreamTurnEnd struct{ Claim claim.Token }

// StreamTurnAnswered is the agent's answer to the daemon's ask whether it is in a turn (get_state,
// Machine.streaming), as the worker stream delivers it: behind every frame the shim sent before
// that answer, its replay of the frames it held while no daemon was attached among them, since
// the shim writes that replay before any later frame on the connection and one claim's events are
// handled in the order they arrive. Streaming is what the agent said, and Ask the id of the ask it
// answers (runtime.ConnState.Ask). It is the decision of its own the stale-plugin relaunch of a
// claim a daemon start restored ready or idle waits for (Machine.askTurn, turnAnswered), which so
// reads the claim with every replayed turn already applied. It answers an ask made on the
// connection it arrives on whose request still waits for it: an answer to an earlier
// connection's ask, which the shim replays from its backlog after that connection broke, is
// delivered as none (stream's Conn.stateAnswered), and the next connection's hello asks again
// on that connection (reconnected, askRestoredTurn). The answer to the ask a placed count of the
// process's turns made is also what checks that count, behind the frames before it
// (learnTurns, checkPlacedCount), whatever the claim's row does with its words.
type StreamTurnAnswered struct {
	Claim     claim.Token
	Streaming bool
	Ask       string
}

// IssueTurnEnded is a turn ending on another claim of this claim's issue, posted by the daemon to
// every other claim of the issue once that turn's own machine has recorded its end. The roles of
// an issue work one jj workspace, so a phase task this claim is holding rather than adopt that
// working copy under an open turn (takeable) may be taken now. It carries no fact of its own — the
// take asks again which claims are in a turn — so one posted for a turn another role began
// meanwhile simply holds again.
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
// (runtime.Conn.Sequence). A frame that arrived on a connection the claim has since replaced is
// evidence of what the agent was doing and none at all of what it is doing now, so it records its
// call and does not move the claim's last-step time.
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

// StreamClosed is the claim's connection closing. It says nothing about the process by itself.
type StreamClosed struct{ Claim claim.Token }

// TimerKind names the waits a machine times.
type TimerKind string

const (
	// TimerBoot is the boot observation interval: an unregistered process is probed.
	TimerBoot TimerKind = "boot"
	// TimerRegistration is the registration deadline, which runs until the agent is ready: a live
	// process whose agent never registered, or registered and never said it was ready, is retired.
	// Once the agent is ready it is the reconnect deadline (connview.go, reconnectDeadline): of a
	// claim a daemon start restored with a process whose shim has not said hello since (NewMachine,
	// readyView), or whose connection closed while it held a pending task, no earlier than a whole
	// bound from that close (closeView, reconnectFromClose), or which was given a task over a
	// connection closed since its last hello (armReconnectForTask), a live process whose shim never
	// says hello by then is retired, and the hello ends it (reconnected, reconnectedMidTurn); and of
	// a reconnect hello over a pending task, a daemon start's own ask at its first hello included
	// (helloView), a process whose agent says nothing of its turn by then, and sends no turn frame,
	// is relaunched uncharged.
	TimerRegistration TimerKind = "registration"
	// TimerFinish is the end-frame bound: a task's turn ended, and the turn-outcome report that
	// names its number has that long to come (Timeouts.RPC) before the end of the turn finishes
	// the task (finishTask). Only a claim whose plugin names no number is given it
	// (TakesNoTask), and any claim whose report's finish the store refused (finishTask).
	TimerFinish TimerKind = "finish"
	// TimerProbe is the re-probe of an uncertain process.
	TimerProbe TimerKind = "probe"
	// TimerSuspend is the held suspension's timer (holdSuspension): the stop timeout of its wait for
	// the agent's turn, and the retry of a stop that failed.
	TimerSuspend TimerKind = "suspend"
	// TimerRelaunch is the retry of a relaunch left waiting with no process (relaunchLater): one whose
	// lifecycle check failed with no cleanup reserved, or a launch whose write failed. The claim waits
	// launching with no process until it fires.
	TimerRelaunch TimerKind = "relaunch"
	// TimerWake is the let-out of the place the wake pacing holds a launch in (reserveWake,
	// bindWake): the pacer's release of that place fires it, and the launch runs when it does
	// (wakeNow). A claim with no process of its own waits launching with none until then; one whose
	// process still runs waits exactly as the wake found it.
	TimerWake TimerKind = "wake"
	// TimerPromptRetry is the wait before a task a provider error took back is written again to
	// the same session's value (promptRetryDelay), armed on that task's own delivery id.
	TimerPromptRetry TimerKind = "prompt_retry"
	// TimerTake is R5's first bound (Timeouts.Take, waits.go): a value the worker reported seeing
	// that no take came at charges the worker when it passes, and a value nobody reported seeing
	// is written again under its number, charging nobody. The wait over a seen value runs only
	// while the claim's connection shows every frame of its turns (connview.go). Armed on the
	// pending task's id.
	TimerTake TimerKind = "take"
	// TimerTurnStart is R5's second bound (Timeouts.TurnStart, waits.go): an accepted take whose
	// turn has not started when it passes charges the worker and is taken back under its number.
	// It runs only while the claim's connection shows every frame of its turns (connview.go).
	// Armed on the pending task's id.
	TimerTurnStart TimerKind = "turn_start"
	// TimerPause is the gate's own stop of a claim whose launch it held while the project is paused
	// (pauseGate, pauseStopDue): armed at once by the decision that held the launch, so the stop is
	// the claim's next decision, and again at the probe interval when that stop fails. A claim with
	// no process of its own waits launching until it fires; one whose process runs waits as the
	// gate found it.
	TimerPause TimerKind = "pause"
	// TimerReport is the bound of the wait for the report of the pending take's turn, which ended
	// with that report still to come (reportwait.go, armReportWait): R5's second bound,
	// Timeouts.TurnStart, with work waiting for that report, or with nothing held where the bound
	// reaches the take (reportReaches). It is armed whole at the end of the take's turn, or, over a
	// take the bound reaches only with work held, at that work's arrival, and a decision that
	// leaves the take waiting keeps it where it was. When it passes the task is taken back with the
	// note of a cut turn, and what waited runs, or, with nothing held, the task's value is written
	// again under its number for the worker to take (reportBoundOver). It runs only while the
	// claim's connection shows every frame of its turns (connview.go). Armed on the pending task's
	// id.
	TimerReport TimerKind = "report"
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
// Place is the place the boot took for that relaunch in the wake pacing, as RequestSpawn.Place is
// for an unfinished launch: the boot places both in one pass, a recorded death in the pacing's
// first queue (WakeQueue, dispatch://INFRA-5682 R2) and each queue's places in the order their
// claims' work was owed. nil is a replay the gate joins a place for itself. A place the replay
// does not take up is given up as its decision ends (Machine.handle).
type RecordedDeath struct {
	Claim      claim.Token
	Generation uint64
	Place      *wakepace.Place
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
// machine asks the role nothing: the sweep enqueues the round's ask to the role's own Envoy topic
// (record.UpdateAsk), and the outbox marks it on the claim once it reached a session
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
// record.UpdateAsk to the role's own Envoy topic. That message can arrive in the middle of a
// turn, so the text tells the role to act now rather than after the turn.
//
// The text names the round's own id, and the role passes it back in its answer
// (PodUpdateSignalled): an answer counts only for the round whose ask the role read, so the id
// in the role's hand is what says which round it is answering. A role that reads this ask while
// a later round has already replaced it names the round it read, and is refused rather than
// counted for the round it never saw (podUpdateSignalled).
//
// An answer from every role brings the round's end forward only while no role reports background
// work: before endsAt the round ends once every role it asks has answered, no role is in a turn,
// no take whose turn ended still owes its report, and no role with a process in the round's pod
// has an activity report that names a subagent or a background job that runs, or no report since
// its last turn started. internal/daemon's roundEnd.holding is the one statement of that rule. No
// answer, no turn and no background work holds the pod past endsAt, so the text names that moment
// and what goes with the pod then: the turn in progress, each subagent, each background job and
// each named service that still runs, each commit and each file the role did not push, the pod's
// test database and its caches. A named service is in no activity report, so it holds nothing and
// goes with the pod.
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
// goes, so the text says that work not pushed before the pod goes is lost. The daemon reads the
// workspace before it replaces the pod, and a read that finds work that no branch or tag on
// origin holds, or that fails, ends the round with no replacement and keeps the pod (R7 of
// #5285, internal/daemon's roundKeeps), so the text says that a round can end with
// no replacement and the pod then stays, and says that the pod goes whatever runs in it only of a
// replacement the daemon makes. That keep has one exception, and the text names it where it holds
// (GPT-6.1 Sol's non-blocking point on #5285 at 0259f8af): a round over a drift that breaks the
// pod (runtime.Breaks: its recorded reasons open on runtime.BreakingDrift, today a drift of the
// pod's service account or of its exchange identity) replaces the pod whatever the read finds and
// tells the issue's architect what it took, so its ask promises no keep. A pod whose disk is a
// kept volume that the issue's next pod mounts is no exception: R7 keeps it as it keeps any other
// pod, so the text says that the keep holds there too.
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
	read := "When the round ends, the daemon reads the workspace on the pod's disk before it replaces the pod. " +
		"If the read finds work that no branch or tag on origin holds, or the read fails, the daemon does not replace the pod: " +
		"the round ends with no replacement, the pod and its disk stay, and every role keeps working in the pod on its old image and settings. " +
		"This is also true when the pod's disk is a volume that the next pod of this issue mounts. " +
		"If the read finds no such work, the daemon replaces the pod. It deletes the pod and the disk, unless the disk is a volume that the next pod of this issue mounts."
	held := "The daemon holds your next task until your process runs again, or until the round ends with no replacement."
	if runtime.Breaks(reasons) {
		read = "This round is over a change that stops each role from working in this pod. So when the round ends, the daemon replaces the pod whatever the workspace holds. " +
			"Before it replaces the pod, the daemon reads the workspace on the pod's disk. If the read finds work that no branch or tag on origin holds, or the read fails, the daemon tells the issue's architect what the read found. " +
			"The daemon deletes the pod and the disk, unless the disk is a volume that the next pod of this issue mounts."
		held = "The daemon holds your next task until your process runs again."
	}
	return "The daemon must replace the pod that runs this issue's roles. " +
		fmt.Sprintf("The reasons are: %s.", reasons) +
		fmt.Sprintf(`

The round is %[1]s. The round ends at %[2]s (UTC). %[3]s The disk holds your workspace. Only your own push keeps your work. Work that you do not push before the pod goes is lost.

If every role answers, the round can end before that moment. It then ends once each role's last report names no subagent and no background job that runs, and no role is in a turn. Your plugin sends that report at the end of each turn. If a role does not answer, the round still ends at %[2]s. A turn in progress does not delay that moment. A subagent or a background job that runs does not delay it either: when the daemon replaces the pod, the pod goes, whatever runs in it.

This message can arrive while you are in a turn. Do these five steps, in this order:

%[5]s
2. Let each subagent and each background job that you started finish. Stop one that cannot finish before %[2]s.%[6]s
3. If you need a scratch file as evidence, put it on Dispatch.
4. Call the `+"`legion`"+` tool with op: "ready_for_update" and round: "%[1]s".
5. End your turn.

%[7]s with round: "%[1]s" before you end that turn.

A service that you started with a name is not in the report. It does not delay the round, and it goes with the pod.

If your `+"`legion`"+` tool has no "ready_for_update" operation, do steps 1, 2 and 3 now. Then end your turn. The round ends at %[2]s whatever you do.

These things go with the pod when the daemon replaces it:

- The turn that is in progress.
- Each subagent, each background job and each named service that still runs.
- Each commit and each file that you did not push.
- The pod's test database.
- The pod's caches.

The daemon starts your process again on the new pod, in this same session. %[4]s Do not start new work now.`, round, ends, read, held, push, made, after)
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
// its own — and each role comes back on its own session through the displaced path; or the round
// kept the pod, because the read of its workspace found work that no branch or tag on origin
// holds or could not be made (R7 of #5285, internal/daemon's keepPodForRound),
// and each role works on in that pod. A plugin
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
	// Place is a place the caller has already taken for this launch in the wake pacing, and nil
	// for every caller but one: the boot's relaunch of the launches the previous daemon left
	// waiting (internal/daemon's launchUnfinished), which places every launch in its queue in the
	// order its claim's work was owed before it posts them all at once, so the queue a restart
	// rebuilds keeps the order the previous daemon was draining (dispatch://INFRA-5482).
	//
	// The rule the gate holds a carried place to (revive, launchAfter, reserveWake, placeFor),
	// stated here alone: the gate takes the place up, and a place that still waits is the launch's
	// place, and it parks on it; one the pacer let out less than one interval (WakePace.Interval)
	// before the gate reached it is its turn come a little early, and it runs at once; one the
	// pacer let out an interval or more before is not run at once, and the launch joins its queue
	// afresh, since every launch the caller posted together that reached its gate that late would
	// otherwise run in one instant, outside the rate and the burst. The gate logs the last case. A
	// launch the gate does not reach — refused, or held by a project pause — gives its place up
	// as its decision ends (Machine.handle), and the turn it would have taken goes to the wake
	// behind it.
	Place *wakepace.Place
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
// holds the project by then, stops the claim for that one (pauseStopDue); and it writes the value
// of the task the claim holds again where a take of it was refused and it may now be taken
// (offerPending), so the daemon's resume needs nothing else to hand a role its held task.
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
}

// MessageMarked is the daemon's word that a wake for a waiting Envoy message reached this claim:
// the listener's report of a message for the claim's role or session, or a start row whose wake
// carried one (dispatch://INFRA-5682 R1). At is the mark the store holds once the daemon has
// written it there, which is the earliest that stands. The claim's mark becomes At where none
// stands (Claim.MessageMarkAt, markMessage), and a wake of the claim waiting in the pacing's
// second queue moves to the first, so a role whose message Envoy keeps an hour is not woken
// behind a restart's backlog. It writes nothing to the store: the daemon persists the mark before
// it posts this, and posts it before the start it belongs to, so the launch that start raises
// joins the first queue. A claim in any state may be marked, and the mark decides nothing else,
// so every state takes it and none moves.
type MessageMarked struct {
	Claim claim.Token
	At    time.Time
}

// RequestStop ends one claim. The tree's root claim is not one it can end: a root ends only with
// its tree, so any other stop of it is refused, and suspending it is how its process is stopped.
//
// A close is its own event rather than a flag here, and there are two of them, because who decided
// the close is what says whether TreeClosable is asked. A flag has an unsafe default, and the
// caller that never thought about it gets that default; these two make a new caller name which
// close it is making, and the table holds a row for each.
//
// WaitForReport is the one flag it carries, and its default is the stop at once: it asks that the
// stop wait for the report of the claim's take's turn where it reaches the claim after that turn
// ended and before the report came (holdStop; rule two of T3 of dispatch://INFRA-2811's plan).
// Only the operator's stop of one claim asks it (api's stopEvent). The stops that end the work for
// good or must not wait leave it false and stop at once, which is what a caller that never thought
// about it gets: the operator's close of a tree, whose stops of the tree's other claims count any
// error as a claim not stopped (api's closeTree), and the controller's stop at a boot that leaves
// the controller to its operator (internal/daemon's stopLaunchedController). The workflow's close
// of the tree and the operator's close of a claim are events of their own (RequestTreeClose,
// RequestOperatorClose), and stop at once too.
type RequestStop struct {
	Claim         claim.Token
	WaitForReport bool
}

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
// claim's run of refused turns, unless the claim is held for a cyber-policy refusal, whose retry
// starts a new session whoever asks (freshSession).
//
// Decided says a person, the controller or the issue's architect asked for this retry: the outbox
// sets it from the start row's own mark (record.SuperviseRequest.Decided), which two starts
// carry: the held phase's retry (workflow's restart), which the architect's retry_or_escalate, the
// operator's `legion claims retry` of a phase worker and a person's todo on a held root each
// start, and admission's start of a promoted root's architect, which a todo on the root brings,
// the controller's restart of a tree among them (workflow's StartPromotedArchitect). Every other
// start of a failed or retired claim, and the controller keeper's retry, is a retry the daemon
// makes by itself, and is refused for a claim held for a cyber-policy refusal (Claim.RetryHeld,
// ErrHeldForCyberPolicy).
//
// OneAttemptLeft is a production check look's retry of its checking role: the half-open trial of a
// role that keeps failing, which gets one attempt of each budget its failure spent rather than a
// fresh budget (Budgets.OneAttemptLeft), so a role that cannot start is launched once a look and
// not a whole budget's worth of times. A failure that spent no budget runs again as any retry does.
type RequestRetry struct {
	Claim          claim.Token
	FreshSession   bool
	Decided        bool
	OneAttemptLeft bool
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
// task of a phase the issue has left is dropped rather than taken, and a suspension, which ends
// that phase, retires it. The workflow names it; an operator's own delivery names none, is never
// dropped for its issue's phase, and waits out a suspension to be taken after the resume.
type RequestDeliver struct {
	Claim claim.Token
	Task  string
	ID    string
	Phase phase.Phase
	// Origin is the source that queued the task (OriginPhaseStart and the rest), carried onto the
	// delivery (Delivery.Origin). A request that names none is read as originOf says.
	Origin string
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
	// News is what the task carries of news of the approved design, as the start that queued it
	// read it from the record (record.DeliveryNews, encoded JSON), carried onto the delivery and
	// persisted with it, since the delivery's accepted take is what credits it as taken
	// (Delivery.News, Delivery.Carried). Empty for a task that carries none.
	News []byte
	// NewsOnly marks a task queued for news of the approved design alone (the daemon's news-only
	// start, record.SuperviseRequest.News). Such a task holds no work of the claim's: it asks the
	// worker to read what changed and to answer it. So it takes the place of no task that holds
	// work (HoldsTask), where any other newer task takes the place of a taken task whose turn has
	// run or of one a provider error took back (queue): it is refused (ErrDeliveryPending), and its
	// start waits on its row's backoff until the claim holds no such task. It still takes the place
	// of the daemon's own delivery of a relaunch's record, which holds no work either. Once held,
	// it gives way to a task of an entry while no take of it is accepted (Machine.supersedes), since
	// the start of an entry carries the same news itself.
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
// (recordTurnOutcome).
//
// Task is the number of the task whose turn this was, 0 for a turn the producer names none for:
// the report that names the pending task's number is what finishes it (finishTask), and one that
// names another number, or none, finishes nothing. ProviderError is the provider error the turn
// ended in, nil for none, which keeps the task under its number and starts the ladder's wait
// (providerErrored). Held is the number of the accepted take whose text had not run at this
// turn's end, the producer still holding it or the agent never having taken it, 0 for none or a
// producer that does not say (claim.TurnOutcomeRequest): this turn was not that take's, which the
// daemon reads where its own count of the process's turns cannot say it (notTheTakesTurn,
// heldUnrun).
//
// HTTPStatus is the status the provider refused the turn with as a plugin from before the task's
// number names it, 0 where the producer could not read one (status), and Unprompted says the turn
// ran no task the daemon handed, which a plugin from before the task's number sends and a current
// one says by naming no Task (claim.TurnOutcomeRequest). A report marked Unprompted is read as one
// that names no task, whatever its Task says (turnOutcome): its refusal is judged as every turn's
// that was not the take's (unpromptedTurnEnded), and it finishes nothing. Code is the class the
// producer gave the refusal, codeCyberPolicy or nothing (the route takes any other word as none),
// which judges a refused turn beside its status, the take's (providerErrored) and any other
// (unpromptedTurnEnded) alike (classifyProviderError).
//
// None of them is fenced by generation. A report naming a generation the claim has left is not
// stale: it is a turn of that generation, which belongs in its own place in the order the run is
// derived in, and the secret the route authenticates is the current registration's either way.
// What is fenced is a repeat: a report under a key the window already holds, from an attempt that
// was applied, changes nothing more (turnOutcome).
type RequestTurnOutcome struct {
	Claim         claim.Token
	Generation    uint64
	Sequence      uint64
	Refused       bool
	Message       string
	Task          uint64
	ProviderError *ProviderError
	Held          uint64
	HTTPStatus    int
	Unprompted    bool
	Code          string
}

// status is the HTTP status the provider refused the report's turn with, 0 for none or one the
// producer could not read: ProviderError's Status, which a current plugin names, and HTTPStatus,
// which a plugin from before the task's number names, where ProviderError names none. A report
// that carries both is read by ProviderError's, the status of the very request the take's ladder
// judges (claim.TurnOutcomeRequest).
func (r RequestTurnOutcome) status() int {
	if r.ProviderError != nil && r.ProviderError.Status != 0 {
		return r.ProviderError.Status
	}
	return r.HTTPStatus
}

// refusedByProvider is whether the report says the provider refused its turn: a refused outcome,
// or a provider error named beside it. A current plugin names a provider error beside every
// refused turn (pi-legion's turn-outcome, claudebridge's turnEnded); a plugin from before the
// task's number names only the refused outcome and, from dispatch://INFRA-4508's contract on, its
// HTTPStatus. A report that says neither is clean, and ends the claim's run of provider-error
// turns (turnOutcome).
func (r RequestTurnOutcome) refusedByProvider() bool { return r.Refused || r.ProviderError != nil }

// ProviderError is how a provider failed the turn a turn-outcome report is of: the status of the
// provider request that failed (zero when it had none, as a content-filter refusal does), the
// provider and the model that request went to, the failure's message, and whether the agent
// called it retryable. The message is prose of the provider's and is never parsed; Status, with
// the report's Code, decides what the failure means (providerErrored, classifyProviderError); the
// rest are logged, and the model is named in the controller's notice.
type ProviderError struct {
	Status    int
	Provider  string
	Model     string
	Message   string
	Retryable bool
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
// claim back and offers the owed task again all the same, and only true puts the claim's run of
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
func (StreamTurnAnswered) isEvent()   {}
func (IssueTurnEnded) isEvent()       {}
func (StreamToolStep) isEvent()       {}
func (StreamAnswered) isEvent()       {}
func (StreamClosed) isEvent()         {}
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
func (MessageMarked) isEvent()        {}

// eventKind is what the table is keyed on beside the state: an event's type, with a Timer split
// by its kind, because each wait means a different thing.
type eventKind string

const (
	onObservation   eventKind = "runtime_observation"
	onHello         eventKind = "stream_hello"
	onTurnStart     eventKind = "stream_turn_start"
	onTurnEnd       eventKind = "stream_turn_end"
	onTurnAnswered  eventKind = "stream_turn_answered"
	onIssueTurnEnd  eventKind = "issue_turn_ended"
	onToolStep      eventKind = "stream_tool_step"
	onAnswered      eventKind = "stream_answered"
	onClosed        eventKind = "stream_closed"
	onTakeAdopted   eventKind = "take_adopted"
	onBootTimer     eventKind = timerPrefix + eventKind(TimerBoot)
	onDeadline      eventKind = timerPrefix + eventKind(TimerRegistration)
	onFinishTimer   eventKind = timerPrefix + eventKind(TimerFinish)
	onProbeTimer    eventKind = timerPrefix + eventKind(TimerProbe)
	onSuspendTimer  eventKind = timerPrefix + eventKind(TimerSuspend)
	onRelaunchTimer eventKind = timerPrefix + eventKind(TimerRelaunch)
	onWakeTimer     eventKind = timerPrefix + eventKind(TimerWake)
	// The wait a turn that ended in a provider error arms (providerErrored).
	onPromptRetryTimer eventKind = timerPrefix + eventKind(TimerPromptRetry)
	// R5's two bounds (waits.go).
	onTakeTimer      eventKind = timerPrefix + eventKind(TimerTake)
	onTurnStartTimer eventKind = timerPrefix + eventKind(TimerTurnStart)
	// The gate's own stop of a launch it held while the project is paused (pauseGate).
	onPauseTimer eventKind = timerPrefix + eventKind(TimerPause)
	// The bound of the wait for a take's report (reportwait.go).
	onReportTimer eventKind = timerPrefix + eventKind(TimerReport)
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
	onTaskSeen      eventKind = "request_task_seen"
	onTake          eventKind = "request_take"
	onTaskValueRead eventKind = "task_value_read"

	onCompactEnded eventKind = "compact_ended"

	onStalePlugin   eventKind = "stale_plugin"
	onRecordedDeath eventKind = "recorded_death"
	onStallBackstop eventKind = "stall_backstop"

	onPodUpdateOpen      eventKind = "pod_update_open"
	onPodUpdateEnded     eventKind = "pod_update_ended"
	onPodUpdateSignalled eventKind = "pod_update_signalled"

	onResumePause eventKind = "resume_pause"

	onMessageMarked eventKind = "message_marked"

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
	case StreamTurnAnswered:
		return onTurnAnswered
	case IssueTurnEnded:
		return onIssueTurnEnd
	case StreamToolStep:
		return onToolStep
	case StreamAnswered:
		return onAnswered
	case StreamClosed:
		return onClosed
	case TakeAdopted:
		return onTakeAdopted
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
	case RequestTaskSeen:
		return onTaskSeen
	case RequestTake:
		return onTake
	case TaskValueRead:
		return onTaskValueRead
	case RequestCompact:
		return onCompact
	case CompactEnded:
		return onCompactEnded
	case ResumePause:
		return onResumePause
	case MessageMarked:
		return onMessageMarked
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
	case RequestTaskSeen:
		return "task-seen", true
	case RequestTake:
		return "take", true
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
	noTake        = "no take is answered in this state"
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
	t.row(onObservation, "judge the process; alive and ready or idle, write again a value whose take a hold refused; working after an unanswered ask of a hello over no pending task, ask again whether the turn is over", observe,
		[]ClaimState{StateLaunching, StateFailed, StateWorking, StateIdle}, live...)
	t.ignore(onObservation, noProcess, processless...)

	// The shim.
	t.row(onHello, "the shim connected", helloed, []ClaimState{StateShimConnected}, StateLaunching)
	t.row(onHello, "the shim reconnected before ready: hand it the enrollment if it lacks one", enrolledOnly, nil, StateShimConnected, StateRegistered)
	t.row(onHello, "the shim reconnected: write again a value whose take a hold refused; after a restart, ask a stale claim's agent whether it is in a turn", reconnected, nil, prompted...)
	t.row(onHello, "the shim reconnected mid-turn: after a restart, ask whether the turn is still running",
		reconnectedMidTurn, []ClaimState{StateIdle, StateWorking}, StateWorking)
	t.ignore(onHello, "no launch has minted a boot token", StateQueued)
	t.ignore(onHello, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onHello, noProcess, gone...)

	t.row(onClosed, "the connection closed: probe the process", closed, []ClaimState{StateLaunching, StateFailed}, live...)
	t.ignore(onClosed, noProcess, processless...)

	// The worker's own word on its task: the value it was handed, and its take.
	t.row(onTaskSeen, "the worker saw its value at this revision", taskSeen, nil,
		StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onTaskSeen, notRegistered, StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected)
	t.ignore(onTaskSeen, noProcess, gone...)

	t.row(onTake, "the worker asks to take its task: answer it from the record", take, nil, prompted...)
	t.row(onTake, "a take into a turn is refused, unless a restart restored the claim working with its task owed", takeInTurn,
		[]ClaimState{StateIdle}, StateWorking)
	t.row(onTake, "the agent has not said it is ready: the take is refused", takeNotReady, nil,
		StateLaunching, StateShimConnected, StateRegistered)
	t.ignore(onTake, noTake, StateQueued, StateLaunchUncertain)
	t.ignore(onTake, noProcess, gone...)

	// The task bucket's value for the claim, as the daemon's boot and its watch of the bucket
	// read it. The record is the truth in every state, so every state compares it and writes the
	// record's value again where the bucket holds another (valueRead); no state moves.
	t.row(onTaskValueRead, "the task bucket's value: write the record's value again where the bucket holds another", valueRead,
		nil, append(slices.Clone(live), processless...)...)

	// The working copy's adoption a take made with the lock let go (Machine.Take). Every state
	// answers it, since a take is waiting on the answer: a ready or idle claim decides the take
	// again and accepts it, and charges a refused adoption, which can retire the process;
	// every other state refuses it by the state it is in.
	t.row(onTakeAdopted, "the take's adoption is done: accept the take, or refuse and charge it", adopted,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, prompted...)
	t.row(onTakeAdopted, "the claim moved while the take's adoption ran: the take is refused", adoptedTooLate, nil,
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered, StateWorking,
		StateSuspended, StateFailed, StateRetired)

	// Turns.
	t.row(onTurnStart, "a turn started", turnStarted, []ClaimState{StateWorking}, prompted...)
	t.row(onTurnStart, "a turn is already in flight; a turn under a compaction is recorded for its end", turnStartedWhileBusy, nil, StateWorking)
	t.ignore(onTurnStart, "the agent is not ready, so no take is awaiting its turn", unready...)
	t.ignore(onTurnStart, noProcess, gone...)

	// A turn whose end runs a stale-plugin relaunch held for it (refreshPlugin) ends launching, or
	// failed when that launch spends the claim's budget, as the stale-plugin row itself does.
	t.row(onTurnEnd, "the turn ended", turnEnded, []ClaimState{StateIdle, StateWorking, StateLaunching, StateFailed}, StateWorking)
	// A turn the daemon never saw begin ends here: a claim registered while the agent's own turn
	// was already running is ready rather than working, and the end is the only stream event of
	// that turn it will ever get. A value whose take that turn refused is written again now,
	// which is what left three workers idle holding a task for 35 to 75 minutes when nothing
	// did (dispatch://INFRA-1691, INFRA-1545, INFRA-722, A9).
	t.row(onTurnEnd, "a turn the daemon never saw begin ended: write again a value whose take it refused", blindTurnEnded,
		nil, prompted...)
	t.row(onTurnEnd, "a turn of the process ended before its agent said ready: the count of its turns is no longer known", turnEndedBeforeReady,
		nil, StateShimConnected, StateRegistered)
	t.ignore(onTurnEnd, "no process of the launch has said hello, so the turn that ended was of a process the launch replaces, whose turns no count of the launch's covers",
		StateLaunching)
	t.ignore(onTurnEnd, "the agent is not ready, so no turn of a task is in flight", StateQueued, StateLaunchUncertain)
	t.ignore(onTurnEnd, noProcess, gone...)

	// The agent's answer to whether it is in a turn, behind the frames its shim sent before it. It
	// decides the stale-plugin relaunch of a claim a daemon start restored ready or idle that waits
	// for it (turnAnswered): made working by an answer of a turn, then held for that turn's end or
	// relaunched, which leaves it launching, failed once the launch spends its budget, or suspended
	// where a held suspension ends it. After a reconnect hello over a pending task it makes the
	// connection's view whole again (answerView): an answer of a turn makes the claim working, and
	// one of no turn ends the turn the claim was read in, which leaves it idle, or launching, failed
	// or suspended where that end runs what was held for it. Every other state waits for no answer.
	t.row(onTurnAnswered, "the agent said whether it is in a turn: decide the stale-plugin relaunch a restored claim waited for, or read the turn a reconnect hello asked about",
		turnAnswered, []ClaimState{StateWorking, StateIdle, StateLaunching, StateFailed, StateSuspended}, StateReady, StateIdle, StateWorking)
	t.ignore(onTurnAnswered, "no claim the daemon's start restored ready or idle waits for its agent's word on its turn",
		StateQueued, StateLaunchUncertain, StateLaunching, StateShimConnected, StateRegistered)
	t.ignore(onTurnAnswered, noProcess, gone...)

	t.row(onIssueTurnEnd, "a turn ended elsewhere on the issue: write again the value of a task waiting for the working copy they share",
		issueTurnEnded, nil, prompted...)
	t.ignore(onIssueTurnEnd, "the claim is in a turn of its own; a take it refused is offered again when that turn ends", StateWorking)
	t.ignore(onIssueTurnEnd, noTake, unready...)
	t.ignore(onIssueTurnEnd, noTake, gone...)

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

	// Timers.
	t.row(onBootTimer, "the boot interval: probe the unregistered process", bootInterval,
		[]ClaimState{StateLaunching, StateFailed}, booting...)
	t.ignore(onBootTimer, "the agent registered, so its process is no longer probed each interval", StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onBootTimer, "no boot is being watched", processless...)

	t.row(onDeadline, "the registration deadline: the agent never registered, or never said it was ready", registrationDeadline,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateLaunching, StateShimConnected, StateRegistered)
	// The reconnect deadline (connview.go) of a claim a daemon start restored with its agent ready
	// (NewMachine), or whose connection closed over a pending task (closeView): its shim's hello
	// disarms it (reconnected, reconnectedMidTurn), so what fires is a process whose shim never
	// came back — or, armed again by that hello, the bound of an ask the agent left unanswered
	// (helloView, askRestoredTurn), which relaunches it uncharged.
	t.row(onDeadline, "the reconnect deadline: the shim of a restored claim, or of one whose connection closed over a task, never said hello, so probe its process and retire and relaunch it if it lives; or its agent never said whether it is in a turn, so relaunch it uncharged", reconnectDeadline,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateReady, StateWorking, StateIdle)
	t.ignore(onDeadline, "no boot is being watched", processless...)

	// R5's two bounds (waits.go). Each is armed at the end of a decision that leaves the claim
	// waiting on its worker (armWaits) and disarmed there once it no longer does, so a bound that
	// fires finds the claim as the wait it is for left it. A charge can retire the process
	// (chargePrompt), and a held suspension ends there instead.
	t.row(onTakeTimer, "the first bound: write again a value nobody reported seeing, or charge a worker that saw its value and asked no take", takeBoundOver,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateReady, StateIdle, StateWorking)
	t.ignore(onTakeTimer, "the agent is not ready, so it is waited on for no take", unready...)
	t.ignore(onTakeTimer, "no process of this claim is running, so nothing of it is waited on", gone...)

	t.row(onTurnStartTimer, "the second bound: charge a worker whose accepted take started no turn, and take the task back", turnStartBoundOver,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, StateReady, StateIdle)
	t.ignore(onTurnStartTimer, "the claim is in a turn, so no accepted take waits for its turn to start", StateWorking)
	t.ignore(onTurnStartTimer, "the agent is not ready, so no take of it was accepted", unready...)
	t.ignore(onTurnStartTimer, "no process of this claim is running, so nothing of it is waited on", gone...)

	// The end-frame bound after a task's turn ended with no report naming its number. It is
	// armed where that turn ends, and goes with the task (retireTask) and with the process
	// (letGo).
	t.row(onFinishTimer, "no report named the task whose turn ended: the end of that turn finishes it", finishAtTurnsEnd,
		[]ClaimState{StateLaunching, StateFailed}, StateReady, StateIdle, StateWorking)
	t.ignore(onFinishTimer, "no task's turn is awaiting its report", unready...)
	t.ignore(onFinishTimer, "no task's turn is awaiting its report", gone...)

	// The ladder's wait after a turn that ended in a provider error. It is armed only where such
	// a turn can end — a claim working its task, or one whose turn already ended — and is dropped
	// with the task it is for (retirePending) and with the process it was armed under (letGo).
	t.row(onPromptRetryTimer, "the wait after a provider error is over: the task's value is written again for the same session", promptRetryDue,
		nil, StateReady, StateIdle, StateWorking)
	t.ignore(onPromptRetryTimer, noTake, unready...)
	t.ignore(onPromptRetryTimer, noTake, gone...)

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

	// The bound of the wait for the report of the take's turn (reportwait.go). It is armed only
	// over a claim whose agent is ready, idle or working — working for a compaction, since a turn
	// running disarms it — with work waiting for that report or with nothing held, and goes with
	// the task (the fence on its delivery) and with the process (letGo). Where it takes the task
	// back, what waited for the report runs: the held suspension's stop, or the relaunch; a
	// compaction runs it at its end. With nothing held, the offer writes the task's value again
	// under its number, for the worker to take again (offerPending).
	t.row(onReportTimer, "the wait for the take's report is over: take the task back with the note of a cut turn, then run what waited for the report, or write the task's value again where nothing waited", reportBoundOver,
		[]ClaimState{StateLaunching, StateSuspended, StateFailed}, StateReady, StateIdle, StateWorking)
	t.ignore(onReportTimer, "no take's turn awaits its report", unready...)
	t.ignore(onReportTimer, "no take's turn awaits its report", gone...)

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
	t.row(onPodUpdateOpen, "record the update round, and offer the task the old round's answer held; the sweep publishes the round's ask", openPodUpdate,
		nil, inRound...)
	t.ignore(onPodUpdateOpen, "the claim failed; what happens to it next is not the supervisor's decision, and no process of it would read the ask", StateFailed)
	t.ignore(onPodUpdateOpen, "the claim is retired; nothing of it runs on the pod being replaced", StateRetired)

	t.row(onPodUpdateEnded, "the update round is over: clear it, and relaunch the process or release the task it held", endPodUpdate,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended}, append(slices.Clone(inRound), StateFailed)...)
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

	t.row(onReady, "the agent is ready", ready, []ClaimState{StateReady, StateSuspended}, StateRegistered)
	t.row(onReady, "the agent said ready again", reready, nil, StateReady, StateIdle, StateWorking)
	t.ignore(onReady, notRegistered, booting...)
	t.ignore(onReady, "no launch has minted a boot token", StateQueued)
	t.ignore(onReady, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onReady, "the claim is suspended and its process stopped", StateSuspended)
	t.ignore(onReady, failedClaim, StateFailed)
	t.ignore(onReady, retiredClaim, StateRetired)

	t.row(onSuspend, "suspend: stop the process, keep the session, unless the take's turn ended with its report still to come: the request is held for that report, or joins the hold that waits for it",
		suspend, []ClaimState{StateSuspended}, StateRegistered, StateReady, StateIdle)
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

	t.row(onRetry, "retry: fresh budgets, and the same session relaunched unless the retry asked for a new one or no store holds it; a claim held for a cyber-policy refusal only by a retry someone decided, in a new session", retry, []ClaimState{StateLaunching, StateFailed}, StateFailed, StateRetired)
	t.ignore(onRetry, "a queued claim is spawned, not retried", StateQueued)
	t.ignore(onRetry, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onRetry, "the claim has not failed", live...)
	t.ignore(onRetry, "a suspended claim is resumed, not retried", StateSuspended)

	// A retired claim is retried here as the workflow's own retry retries one: `legion claims stop`
	// retires a claim, and while this row refused that state the only way to start a stopped worker
	// again was an edit of the claims store with the daemon stopped (dispatch://INFRA-956 R3).
	t.row(onOperatorRetry, "the operator's retry of an architect or a phase worker, failed or stopped, and of the daemon's controller held for a cyber-policy refusal: fresh budgets, and the same session relaunched unless the retry asked for a new one, the claim is held for a cyber-policy refusal or no store holds it", operatorRetry,
		[]ClaimState{StateLaunching, StateFailed}, StateFailed, StateRetired)
	t.ignore(onOperatorRetry, "a queued claim is spawned, not retried", StateQueued)
	t.ignore(onOperatorRetry, "the previous launch's pane is still uncertain", StateLaunchUncertain)
	t.ignore(onOperatorRetry, "the claim has not failed", live...)
	t.ignore(onOperatorRetry, "a suspended claim is resumed, not retried", StateSuspended)

	t.row(onForgetSession, "drop the session so the next launch is fresh", forgetSession, nil,
		StateQueued, StateSuspended, StateFailed, StateRetired)
	t.ignore(onForgetSession, "the claim has a live process or launch", StateLaunchUncertain, StateLaunching,
		StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle)

	t.row(onDeliver, "queue the task and write its value; the worker takes it once its host is idle", deliver, nil,
		StateQueued, StateLaunching, StateShimConnected, StateRegistered, StateReady, StateWorking, StateIdle)
	t.ignore(onDeliver, "the previous launch's pane is still uncertain", StateLaunchUncertain)
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
	// about where the claim stands. A report naming the task a take holds finishes it, and the
	// finish runs what the turn held, a stale-plugin relaunch among it (reportedTask, afterTurn).
	// A run of refused turns fails no claim; a report fails it only for a provider error whose
	// status cannot clear, the take's turn's (providerErrored) or any other turn's
	// (unpromptedTurnEnded).
	t.row(onTurnOutcome, "record the turn's outcome and finish the task it names, and judge the turn's provider error from it: a refusal that cannot clear fails the claim",
		turnOutcome, []ClaimState{StateFailed, StateLaunching}, live...)
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

	t.row(onCompactEnded, "the compaction is over: reset the run of provider errors it cleared, and offer the owed task", compactEnded,
		[]ClaimState{StateReady, StateIdle, StateWorking, StateLaunching, StateFailed}, StateWorking)
	// A claim in one of these states holds no compaction, so the fence answers the end
	// ErrCompactProcessGone before any of them is read (Machine.fence, ownsCompaction); each is
	// still the table's word for the state, which every event of it must have.
	t.ignore(onCompactEnded, "the claim is not in a compaction: the process it ran on was let go first", prompted...)
	t.ignore(onCompactEnded, "the agent is not ready, so the compaction's process is gone", unready...)
	t.ignore(onCompactEnded, "no process of this claim is running, so the compaction's own is gone", gone...)

	// The project's resume, which the daemon posts to every machine of the project (ResumePause),
	// so every state takes it. A claim that holds nothing of the pause it names is changed by
	// nothing but the write again of the value of the task it holds (offerPending); one whose held
	// launch is let out launches, or fails, or is suspended by a tree whose cleanup is reserved,
	// as a wake's turn is (runWake), and a working claim stays in its turn.
	t.row(onResumePause, "the project's pause is over: drop that pause's held suspension, let out the launch its gate held, and write again the value of the task that waits", resumePause,
		[]ClaimState{StateLaunching, StateFailed, StateSuspended, StateWorking}, slices.Concat(live, processless)...)

	// A wake for a waiting Envoy message reaching the claim (MessageMarked). A claim in any state
	// may be marked, and the mark decides nothing but the queue a paced wake of the claim waits in
	// (WakeQueue), so every state takes it and none moves: a wake that waits in the pacing's second
	// queue moves to the first, and nothing else changes.
	t.row(onMessageMarked, "a wake for a waiting message marks the claim: its paced wake goes to the message queue", messageMarked,
		nil, slices.Concat(live, processless)...)
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

// observe is the runtime's sweep. A live process in a ready or idle claim is also where a take a
// hold refused gets its value written again once no hold stands, when nothing else came
// (offerPending): the sweep's write is bounded by the sweep's interval and by the worker asking
// once per revision, never made on the spot. In a working claim it asks again whether the turn an
// earlier daemon saw start is over, when the last ask went unanswered (askTurnOver); in a ready
// or idle one a daemon start restored, whether its agent is in a turn, while the stale-plugin
// relaunch that waits for the answer has none after the shim's hello (askRestoredTurn). It asks
// again only a connection whose hello asked over no pending task: one whose hello asked over a
// pending task (helloAsked) is asked nothing more, since the reconnect deadline armed at that
// hello bounds its ask (helloView, connview.go).
func observe(m *Machine, ctx context.Context, ev Event) error {
	return m.judge(ctx, ev.(RuntimeObservation).Observation, func(ctx context.Context) error {
		if err := m.ensureEnrolled(ctx); err != nil {
			return err
		}
		if m.busy() {
			if m.helloAsked {
				return nil
			}
			return m.askTurnOver(ctx)
		}
		if m.askTurn && !m.askedAt.IsZero() && !m.helloAsked {
			m.askRestoredTurn(ctx)
		}
		return m.offerPending(ctx)
	}, TimerProbe, m.deps.Timeouts.Probe)
}

// helloed is the shim's first connection: StateLaunching to StateShimConnected, and the only path
// where the process is known to hold a running agent process (OMP's own, not whatever the pod ran
// before it under init), so the registration deadline is re-armed from here at its base
// Boot×RegistrationIntervals alone (armRegistration derives the bound from m.claim.State, which
// this sets to StateShimConnected first): a Kubernetes pod can only dial its hello once both init
// containers have finished. An Oh My Pi that boots and never registers is retired on that base
// deadline alone, counted from the hello rather than from the launch. A new process's connection
// carries every frame of its turns from its first (connview.go).
func helloed(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateShimConnected
	m.setView(viewWhole)
	m.armRegistration()
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.ensureEnrolled(ctx)
}

// enrolledOnly is a hello in a state the reconnect changes nothing about — the shim connected or
// the agent registered, not yet ready — except that the reconnected shim may need the enrollment
// it never received, or the resumed claim its first one. An agent not yet ready is under no bound
// that reads its frames, and its connection is read whole from here (connview.go).
func enrolledOnly(m *Machine, ctx context.Context, _ Event) error {
	m.setView(viewWhole)
	return m.ensureEnrolled(ctx)
}

// reconnected is the shim's hello to a ready or idle claim: an adoption a take lost to the
// transport is one this ends, so a value whose take was refused is written again
// (offerPending). A claim a daemon start restored is waiting on exactly this hello (its reconnect
// deadline, NewMachine), and the hello ends that wait. Before any step of its decision that can
// fail on the store, the hello reads whether a daemon start's own ask serves it and opens the
// view it reads (helloView, connview.go): a claim holding a pending task waits for its agent's
// answer or a turn frame, under the reconnect deadline armed whole from the hello, so a decision
// that fails after that leaves the claim bounded. A restored claim whose stale-plugin relaunch
// waits for its agent's word on whether it is in a turn (askTurn) is asked at its first hello
// since the daemon start (askRestoredTurn), once those steps are done; the ask is dated before
// them, where the view is opened (askedAt, helloView), so a decision that fails between leaves
// the deadline the bound of that ask, and a sweep able to make it. The hello carries no turn, and
// the shim replays what it held only once this hello is answered. That ask serves that hello
// alone; a later hello asks on its own connection (askView). The first hello after a daemon start
// gives a restored take's anchor its connection, where the hello says its shim's backlog is whole
// (bindRestoredAnchor), before anything else of the hello can fail; a hello that does not say so
// loses the count of the process's turns and the mark of a start still to come (readBacklog).
// Neither reads the view the hello opened. A claim holding a pending task then asks its agent
// whether it is in a turn, once, unless the daemon start's own ask served the hello (askView).
func reconnected(m *Machine, ctx context.Context, ev Event) error {
	served := m.helloView()
	if err := m.readHello(ctx, ev.(StreamHello)); err != nil {
		return err
	}
	if err := m.ensureEnrolled(ctx); err != nil {
		return err
	}
	if m.askTurn && served {
		m.askRestoredTurn(ctx)
	}
	m.askView(ctx, served)
	return m.offerPending(ctx)
}

// readHello reads what a hello to a live claim says of the process, for reconnected and
// reconnectedMidTurn alike: a restored take's anchor is given the hello's connection first
// (bindRestoredAnchor), and the shim's backlog is read after it (readBacklog).
func (m *Machine) readHello(ctx context.Context, hello StreamHello) error {
	if err := m.bindRestoredAnchor(ctx, hello); err != nil {
		return err
	}
	m.readBacklog(hello)
	return nil
}

// askRestoredTurn asks the agent of a claim a daemon start restored ready or idle whether it is
// in a turn, for the stale-plugin relaunch that waits for the answer (askTurn). An answer is not
// acted on here: the shim replays the frames it held while no daemon was attached after its hello
// and before that answer, and the answer reaches the machine behind them as an event of its own
// (StreamTurnAnswered), where the relaunch is decided with every replayed turn applied
// (turnAnswered). An ask the agent does not answer is not asked again over the pending task the
// claim holds: the hello armed the reconnect deadline whole at itself for it, before this ask
// (helloView), so this ask arms none of its own, and a claim whose agent has not answered by that
// deadline, counted from its first hello, is relaunched there, uncharged (reconnectDeadline). Over
// no pending task the next sweep of a claim not busy asks again (observe), under the same bound,
// counted from the first hello's ask (askedAt), which the first unanswered ask arms and the ones
// after it keep, or arm again after a close took it (closeView). That hello dates the ask where it
// opens its view, before any step of its decision that can fail (helloView), and makes it here
// only once those steps are done (reconnected): a hello whose decision failed between, which made
// no ask, leaves it dated all the same, so over a pending task the deadline that hello armed is
// this ask's bound, at which the stale-plugin lane decides the relaunch, uncharged, whatever turn
// frame came first (seeView, reconnectDeadline), and over none the next sweep makes the ask. The
// ask serves only the hello that made it, since the stream reads an answer only on the connection
// that asked: a later hello over a pending task arms the reconnect deadline whole from itself
// (helloView) and asks once on its own connection (askView), and its answer decides the relaunch
// as this one's would (turnAnswered).
//
// A claim with no connection to ask on is held by the same bound. The stream queues a hello and
// then the close of its connection, and the close can come before the decision that reads the
// hello asks; a shim that cannot reach the daemon again sends no other hello. The relaunch waits
// for the answer, and the stale plugin holds the take's two waits until that relaunch
// (claimHold), so without the bound nothing would end the wait while the process lives.
func (m *Machine) askRestoredTurn(ctx context.Context) {
	if conn, ok := m.deps.Conns.Conn(m.claim.Token); ok {
		if _, answered := m.streaming(ctx, conn); answered {
			m.unansweredAsks = 0
			m.log.Info("supervise: the agent of a claim the daemon's start restored said whether it is in a turn; its stale-plugin relaunch is decided behind the frames its shim replayed")
			return
		}
		m.logUnansweredAsk(ctx, "supervise: the agent of a claim the daemon's start restored did not say whether it is in a turn; its stale-plugin relaunch waits for that answer or for the reconnect deadline of its hello",
			"askedAt", m.askedAt)
	}
	if _, armed := m.timers[TimerRegistration]; !armed {
		m.armAt(TimerRegistration, m.askedAt.Add(m.reconnectBound()), "")
	}
}

// turnAnswered is the agent's answer to whether it is in a turn, reaching the machine behind every
// frame its shim sent before it (StreamTurnAnswered). For a claim a daemon start restored ready or
// idle whose stale-plugin relaunch waits for it (askTurn), this is that relaunch's decision,
// whichever hello's ask it answers — the daemon start's own, at the first hello (askRestoredTurn),
// or a later hello's (askView):
//
//   - an agent in a turn is working: a replay that held the turn's agent_start has made the claim
//     working already, and one whose backlog dropped it is made working here, which also marks
//     the turn of a take accepted and awaiting it started (turnStarted, startTakesTurn) — no
//     confirmation by get_state, since the take was accepted already and the answer says only
//     that its turn is running — and the relaunch is held for that turn's end (refreshPlugin,
//     holdStaleRelaunch), where the task is settled first (turnEnded, afterTurn);
//   - an agent in no turn is relaunched now, every turn the replay showed already applied, so a
//     take whose turn started and ended in the replay is settled by its contract: for a plugin
//     below TaskTakeContract where the relaunch lets the process go (interrupted), and for one
//     that names its number by the report of that turn, which the relaunch waits for
//     (refreshPlugin, relaunchWaitsForReport); one no turn ran is given back with no note.
//
// Either answer is read in one decision, in the one place every answer makes the view whole and
// is applied by the rule of every answer (answerView): an answer of no turn ends a turn the
// claim still reads as running, and the relaunch is then decided with that turn applied. That
// order is live: a replayed start made the claim working, its connection broke with that turn's
// end frame in it, and the next hello's ask on its own connection is the only word of that end
// that will come.
//
// The reconnect deadline that bounded the ask goes with the answer, and the connection's view is
// whole again (answerView, connview.go). Over a view a reconnect hello left waiting for an answer,
// any answer on that connection makes it whole and is read against the claim, and it ends a daemon
// start's own ask of a claim restored working (askFirst) too. Any other answer — to the ask a
// restored working claim makes (askTurnOver), or one arriving after the relaunch was decided or the
// view made whole — changes nothing. A replay behind a hello that did not say its shim's backlog is
// whole (turnCount.cut, readBacklog) may have lost a take's whole turn, run while no daemon served
// the claim, or that turn's start alone, its end reaching the machine as a turn it never saw begin
// (blindTurnEnded). So behind such a replay an agent in no turn has a take whose turn no frame
// showed taken back with the note of a cut turn rather than unstarted (markRestoredTurnUnseen), as
// a take whose report never came is (reportMissed): the next process runs the task again told that
// its turn on it may have run (dispatch://INFRA-2811, T3's rule three). What no hello can show
// missing stays outside it: frames the daemon before the start read and never recorded, and frames
// the shim wrote to that daemon's connection as it closed.
func turnAnswered(m *Machine, ctx context.Context, ev Event) error {
	answer := ev.(StreamTurnAnswered)
	if !m.askTurn {
		if m.connSeen != viewAsked {
			return nil
		}
		return m.answerView(ctx, answer.Streaming)
	}
	if answer.Streaming && !m.busy() {
		m.log.Info("supervise: the agent of a claim the daemon's start restored is in a turn its shim's replay did not show begin; the claim is working")
	}
	if err := m.answerView(ctx, answer.Streaming); err != nil {
		return err
	}
	if !answer.Streaming {
		m.markRestoredTurnUnseen()
	}
	return m.refreshPlugin(ctx, m.askTrigger, nil, notAtReconnectDeadline)
}

// markRestoredTurnUnseen marks the take whose stale-plugin relaunch is decided now — at its agent's
// answer of no turn (turnAnswered), or at the reconnect deadline with no answer
// (reconnectDeadline) — as one whose turn may have run unseen (markTurnUnseen), where a hello of the
// process's generation did not say its shim's backlog is whole (turnCount.cut) and the take still
// awaits its turn: the let-go then takes it back as a cut turn (interrupted). The take is any the
// claim holds then, restored by the daemon start or accepted since.
func (m *Machine) markRestoredTurnUnseen() {
	if m.counted().cut {
		m.markTurnUnseen()
	}
}

// reconnectedMidTurn is the shim's hello while the claim is working. Within one daemon's life the
// shim's backlog replays whatever the turn did meanwhile, agent_end included; a restored claim
// may be waiting on an agent_end an earlier daemon received and never recorded, so its first
// hello asks. It ends the reconnect deadline a daemon start armed, as reconnected does, and opens
// the view it reads before any step of its decision that can fail, as that does (helloView): a
// claim holding a pending task waits for its agent's answer or a turn frame under the reconnect
// deadline armed whole at the hello, which bounds it if a later step fails. It then gives a
// restored take's anchor its connection as reconnected does (bindRestoredAnchor), and reads what
// the hello says of the shim's backlog as that does (readBacklog). A claim holding a pending task
// asks its agent whether it is in a turn, as reconnected does, under that deadline (askView): an
// end frame lost when the connection broke ends the turn at an answer of no turn. A daemon start's
// own ask of a claim restored working (askFirst) serves its first hello since that start instead,
// made here (askTurnOver) under that same deadline; a later hello's ask is the view's, so the
// connection is asked once, and no sweep asks it again (observe). Over no pending task the start's
// own ask is made here too, and a sweep asks it again until it is answered. That covers a claim
// restored ready or idle on a stale plugin (askTurn) that a replayed start made working, whose next
// hello lands here: its answer, or the reconnect deadline, decides the relaunch.
func reconnectedMidTurn(m *Machine, ctx context.Context, ev Event) error {
	served := m.helloView()
	if err := m.readHello(ctx, ev.(StreamHello)); err != nil {
		return err
	}
	if err := m.ensureEnrolled(ctx); err != nil {
		return err
	}
	if m.askView(ctx, served) {
		return nil
	}
	return m.askTurnOver(ctx)
}

// unansweredAsksWarn is the count of unanswered asks in a row from which askTurnOver and
// askRestoredTurn log each one at Warn (logUnansweredAsk): a restored working claim holding no
// pending task, whose shim came back but whose agent never answers, stays working, asked again at
// every sweep, until the stall backstop ends a turn that records no step (backstopStall), and an
// operator should see that. One holding a pending task is asked once at its hello and relaunched,
// uncharged, at the reconnect deadline that hello armed, and one whose shim never comes back at all
// is retired at its reconnect deadline (reconnectDeadline).
const unansweredAsksWarn = 3

// logUnansweredAsk counts an ask of whether the agent is in a turn that it did not answer
// (Machine.unansweredAsks), and logs line with that count first and args after it: at Info, and at
// Warn from the unansweredAsksWarn-th in a row (askRestoredTurn, askTurnOver).
func (m *Machine) logUnansweredAsk(ctx context.Context, line string, args ...any) {
	m.unansweredAsks++
	level := slog.LevelInfo
	if m.unansweredAsks >= unansweredAsksWarn {
		level = slog.LevelWarn
	}
	m.log.Log(ctx, level, line, append([]any{"unanswered", m.unansweredAsks}, args...)...)
}

// askTurnOver asks a restored working claim's agent whether the turn an earlier daemon saw start
// is still running, and ends it when the agent says it is not. An ask the agent does not answer
// says nothing about the turn: the claim stays working and the question stays armed. Taken as "not
// streaming", it would end a turn still running and set the claim idle, and the sweep would then
// write the value of a task into the busy agent every interval. Over a pending task the ask is the
// first hello's since the daemon start, under the reconnect deadline that hello armed (helloView),
// and no sweep asks it again; over none, the next sweep asks again (observe), which the line of an
// unanswered ask says (sweepAsksAgain).
//
// A turn that is over is ended by the rule every answer is applied by (answerView,
// turnEnded). The end is counted as every end is (TurnsEnded), so the daemon posts it to the
// issue's other claims, whose phase tasks may have been held for this turn (its wakeIssue). An
// answer either way makes the connection's view whole again where every answer does, which ends
// the reconnect deadline and askFirst with it (answerView), the ask a reconnect hello would make
// served by this one (connview.go).
func (m *Machine) askTurnOver(ctx context.Context) error {
	if !m.askFirst {
		return nil
	}
	conn, ok := m.deps.Conns.Conn(m.claim.Token)
	if !ok {
		// No shim yet (a sweep before its hello): the hello asks.
		return nil
	}
	state, answered := m.streaming(ctx, conn)
	if !answered {
		m.logUnansweredAsk(ctx, "supervise: the agent did not say whether the turn an earlier daemon saw start is over; the claim stays working",
			"sweepAsksAgain", !m.helloAsked)
		return nil
	}
	if !state.IsStreaming {
		m.log.Info("supervise: the turn an earlier daemon saw start is over")
	}
	return m.answerView(ctx, state.IsStreaming)
}

// closed is the claim's connection closing: the connection's view of the worker's turns is cut,
// which disarms the bounds that read it and sets the reconnect deadline of a claim whose agent
// said it is ready: over a pending task no earlier than a whole bound from the close, and over
// none disarmed (closeView, connview.go); and its process is probed. The view is cut first, so a
// death the probe finds reads it: a take awaiting its turn may have had that turn start with its
// start frame lost in the closed connection, and goes back as a turn cut off
// (turnMayHaveRunUnseen, interrupted).
func closed(m *Machine, ctx context.Context, _ Event) error {
	m.closeView()
	return m.probe(ctx, nothing, TimerProbe, m.deps.Timeouts.Probe)
}

func reprobe(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, nothing, TimerProbe, m.deps.Timeouts.Probe)
}

// turnStarted is a turn starting in a ready or idle claim. The first turn that starts after a
// take of the pending task was accepted is that task's turn (takeAwaitsTurn): the worker
// starts it on the take's answer. Its start records the take's turn started (startTakesTurn),
// which a report naming no task can undo (notTheTakesTurn); its line names the task's number. Any
// other turn is a foreign one — an Envoy notice's, a person's steer, a background result's — and
// is no task's. A take whose report came and whose finish the store refused is given no start
// either, whichever turn it awaits (takePhase, waitFinish): that report decides, and the end-frame
// bound makes its finish again at this turn's end (turnEnded). Either way the claim records when
// this turn began (Claim.LastTurnStart), which is persisted with it: the time is a reader's only
// handle on how long the agent has been in the turn it is in.
//
// Any turn's start puts the claim's prompt budgets back to zero, in the same write (R5: a turn
// start or a turn end resets the count, a take does not): the worker acted, so the charges R5's
// two waits and a refused adoption made of it are over (waits.go).
//
// The persisted working state is reported to the daemon (Machine.OnDurableState), because a turn is
// the one observation that a woken pod's role came back and took work, whether or not the wake
// carried a task for it to take: a wake for a notice or a hook prompt carries none, and its
// resume would otherwise never be measured (R5).
//
// One start is neither: the start of a turn whose turn-outcome report reached the daemon first and
// already finished the take's task, or took it back for a provider error (Machine.reportedAhead,
// startedUnseen). It is that take's turn, so its line names the task, and the turn's end names it
// too; but the report already ran what the take's turn start does — the take's turn recorded
// started and the serving credit earned — so none of it runs again, and the start of the task's
// turn is counted once. The start is that turn's only where it is the start of the turn the report
// is of, the turn after the ends the machine counted (turnCount): a later turn's start says the
// reported turn's start was lost, and the mark goes; an earlier turn's start leaves it for the
// reported one. A machine whose count of the process's turns is not known takes the next start
// for it.
//
// The turn the take of the pending task is given is counted too (turnCount.take), so its report
// tells that turn from every other (notTheTakesTurn). While the count is not known, the turn is
// given no sequence, since the count's ends are not the producer's; for a take the machine
// accepted meanwhile its place among the ends counted is kept instead (takeAnchor.given), where
// the report of the take's own turn can place the count (learnTurns). A start that a report has
// already said ran with the take's text held, one whose report came ahead of its start frame, is
// given no take (heldUnrun): its line names no task, and the take awaits the start after it.
func turnStarted(m *Machine, ctx context.Context, _ Event) error {
	m.claim.State = StateWorking
	m.claim.LastTurnStart = m.deps.Clock.Now()
	m.resetPromptBudgets()
	p := m.claim.Pending
	// The write is the take's turn starting where this start is the pending task's turn, and the
	// claim alone where it is a foreign one; the task is named in the start's line for the same
	// reason, so both are decided in the one place that asks the question.
	persist, task := m.persist, uint64(0)
	count := m.counted()
	running := count.ended + 1
	ahead := m.reportedAhead
	if ahead.task != 0 && count.known && ahead.sequence != running {
		if ahead.sequence < running {
			m.reportedAhead = aheadReport{}
		}
		ahead = aheadReport{}
	}
	switch {
	case ahead.task != 0:
		m.reportedAhead, task = aheadReport{}, ahead.task
	case p != nil && m.takeAwaitsTurn():
		count.take = 0
		switch {
		case count.known:
			persist, task = m.startTakesTurn, p.Number
			count.take = running
		case count.anchor.number == p.Number && count.anchor.unrun(0) && m.onAnchoredConn():
			m.log.Info(heldPassedOverLine, "task", p.Number)
			count.anchor.pass(0)
		default:
			persist, task = m.startTakesTurn, p.Number
			if count.anchor.number == p.Number {
				count.anchor.given = running
			}
		}
	}
	// The start is logged before the write, never after it: the decision this line exists for is
	// a turn start whose claim write failed, and a line written after the write would leave
	// exactly that decision silent again.
	m.startTurn(task)
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
// by the order they were registered in (ConnSequence against the live connection's Sequence):
// the order the machine handles their events in says nothing.
func (m *Machine) staleStep(s StreamToolStep) bool {
	conn, connected := m.deps.Conns.Conn(m.claim.Token)
	return !connected || s.ConnSequence < conn.Sequence()
}

// turnStartedWhileBusy is a turn starting in a claim that is already working. A claim working a
// turn is in the turn it is already in and nothing moves. A claim working because its session
// is being compacted has a turn of Oh My Pi's own starting under the compaction — a follow-up
// an Envoy delivery queued while the session was being compacted — and that turn is recorded on
// the compaction: its end would otherwise reach an idle claim, where it means nothing, and the
// compaction's end would offer the owed task while a turn runs (compactEnded).
//
// That turn's start line is written here (startTurn), because its end writes the end line
// (turnEnded): an operator reads an end with no start as a turn the daemon never saw begin, and
// the daemon saw this one. No take started it, so it is no task's turn and its line names none.
//
// A start over a turn running outside a compaction says the machine missed the end of a turn, or
// was sent a start twice, so its count of the process's turns is no longer known (turnCount), and
// the turn given a take accepted while it was not known is no longer the last start the machine
// saw, so that take's report places no count (takeAnchor, learnTurns): the report of a later
// take's own turn may. A count a take's report placed that the agent's answer has not checked
// yet goes back with it (turnCount.lose).
func turnStartedWhileBusy(m *Machine, _ context.Context, _ Event) error {
	if m.compacting == nil {
		m.counted().lose()
		return nil
	}
	m.startTurn(0)
	m.resetPromptBudgets()
	m.log.Info("supervise: a turn started while this claim's session is being compacted; the claim is left working for it",
		"generation", m.claim.Generation)
	m.compacting.inTurn, m.compacting.back = true, StateIdle
	return nil
}

// turnEnded is the turn over: what was held for it runs — a suspension held for this turn
// (holdSuspension), which runs instead of anything else; a stop the runtime refuses leaves the
// claim idle, still holding it — and a take a hold refused is offered again. The agent just said
// where it is, so nothing is left to ask it after a restart. The claim records when the turn
// ended (Claim.LastTurnEnd) before any of that: every path out of here persists the claim, and
// the time is what tells an idle worker that finished a moment ago from one idle since yesterday.
//
// A turn that ends is also the end of the work the deaths were charged against (chargeDeath), so
// the count goes with it: an agent killed twice in its turns and through the third is making
// progress, and only deaths that keep a turn from ending should ever fail its claim.
// The prompt budgets go with it too, as they do at a turn's start (resetPromptBudgets, R5): the
// worker acted, and R5's waits charge only a worker that does not.
//
// The end of a task's turn does not finish the task by itself. Oh My Pi's agent_end names no
// task, and with background work on, a role's own background result starts turns no take
// started: what finishes the task is the turn-outcome report that names its number, whose finish
// runs what was held for the turn (settleThenAfterTurn). Only for a claim whose plugin names no
// number in its reports, one below TaskTakeContract, does the end of its turn finish it where no
// report came within the RPC timeout (TakesNoTask, TimerFinish, finishAtTurnsEnd). A suspension
// held for the turn of such a claim's task runs at this end, and the settle after its stop
// finishes the task by this end. A suspension held for the turn of a task whose plugin names its
// number waits for the report instead, behind the wait for the take's report (reportwait.go,
// takePhase): the claim goes idle holding it, and the report's finish runs it (reportedTask,
// settleThenAfterTurn); that wait's bound (TimerReport) and the hold's own deadline (TimerSuspend)
// bound it, whichever is first, and either takes the task back as a cut turn before the stop
// (reportMissed).
// Run at this end, its stop would come before the report, which the suspended claim ignores, and
// settle would take back a task whose turn ran whole, to run again on resume. So does a hold over
// a take a report put back while this turn ran (putBack): it waits for the take's own turn, or for
// R5's second bound.
//
// A stale-plugin relaunch held for this turn (refreshPlugin) runs once the turn is over for its
// task too (afterTurn): here for a turn that was no task's, and at the task's finish for a task's
// turn, behind the same wait for the take's report.
// The claim's process speaks a plugin contract that cannot answer an update round, so it is
// replaced now that its worker has finished the turn, and the task waiting behind it is taken by
// the process the relaunch starts rather than by the one it replaces. Waiting for the finish is
// what has the turn's own task retire before the relaunch rather than be taken back and given
// again to a worker that already did it. refreshPlugin is what decides it, not relaunchStale: by
// then a registration may have recorded this daemon's contract, and the claim's state and held
// suspension are what they are at that moment, not what they were when the sweep found it.
//
// That re-decision can come to nothing at all — the registration above is exactly the case
// (holdStaleRelaunch) — so the offer runs after it rather than in its place: a take left refused
// there would wait for an event nobody is going to post. A relaunch that did run leaves the claim
// launching, which a take of is refused, so the offer costs that case nothing.
//
// The turn's end is logged here (turnEndedLine), and this is the only exit from working that
// writes that line. The line names what this turn's own start line named (startTurn, endTurn),
// because no reading of the claim can answer what the ending turn was: the window between a
// task's turn ending and its finish is one where the claim holds the task still, while a turn
// now ending may be a turn of the agent's own, which started naming no task. Every other way out
// of a turn writes a line of its own instead, which is what a reader who found a start and no end
// looks for:
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
// The mark a restart left (askFirst) goes with the turn.
//
// A suspension held for the turn runs here, now that the turn is over (holdSuspension), unless
// the turn was a task's that only its report settles, where it runs at that report's finish
// instead (above). A project pause's hold is no exception either way: only the report says
// whether the turn finished its task or a provider refused it, and a stop made first would leave
// a refused task taken while the role is still owed it. Its stop cuts off no turn, so what the
// let-go records reads the claim as idle, the state this end gives it (suspendHeldAtEnd): the
// next process is told it was in no turn, and the task whose turn this was is no task cut off in
// its turn (recordEnd).
//
// Every end this records is counted (TurnsEnded), the compaction's below included, so the daemon
// can post it to the issue's other claims once this decision is published (its wakeIssue): a phase
// task one of them holds rather than adopt the working copy they share under this turn may be
// taken now.
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
	m.counted().ended++
	m.claim.LastTurnEnd = m.deps.Clock.Now()
	m.claim.Budgets.Deaths = 0
	m.resetPromptBudgets()
	if m.compacting != nil {
		m.log.Info("supervise: a turn ended while this claim's session is being compacted; the claim stays working until the compaction ends")
		m.compacting.inTurn, m.compacting.back = false, StateIdle
		return nil
	}
	m.askFirst = false
	if m.held != nil {
		// The hold waits behind the wait for the take's report, or its stop runs now; a stop the
		// runtime refused leaves the claim idle over a task whose turn just ended, with the
		// end-frame bound armed as at any turn's end (runHeldAtEnd).
		return m.runHeldAtEnd(ctx, StateIdle, m.armEndFrameBound)
	}
	return m.endNothingHeld(ctx, StateIdle, m.armEndFrameBound)
}

// endNothingHeld is the end of the turn, or of the compaction, the claim is still stored working
// for (turnEnded, compactEnded), with no suspension held over it (runHeldAtEnd serves one that
// is): the claim goes back in back, the state that end gives it back in, and is written.
//
// Over a take whose turn ran, the end-frame bound is armed for the finish that end makes — a
// plugin that names no number, or a report's finish the store refused (armEndFrameBound) —
// through armFinish, as runHeldAtEnd arms it: whole at a turn's end (turnEnded passes
// armEndFrameBound), and at a compaction's only where none stands (compactEnded passes
// armEndFrameBoundUnlessArmed), so a bound armed at the take's turn's end keeps its time. A
// finish the store refused while the compaction ran (finishTask, Machine.finishOwed) is made
// again by that bound, which the refusal armed for no working claim.
//
// What is held for the turn runs now (afterTurn) only where the task the turn may have been of is
// settled already. Where it is not, it runs where the task settles instead, one rule for both
// ends: at the end-frame bound's finish over a take whose turn ran (finishAtTurnsEnd), and at the
// report over a take whose report is still to come (reportedTask, reportwait.go) — its turn over,
// or the take put back while the turn or the compaction ran, which waits for its own turn
// (putBack). A stale-plugin relaunch run at the end would let the process go before that finish
// or that report: its take-back would cut a take whose turn ran whole, or one whose start is on
// its way. A relaunch held for this end that now begins to wait for the report, or for the take's
// own turn again, is logged so, by the take's phase (waitsLine), once (Machine.staleForEnd); one
// that waited for it already is not logged again.
func (m *Machine) endNothingHeld(ctx context.Context, back ClaimState, armFinish func(id string)) error {
	id, finishes := m.backAtEnd(back)
	if err := m.persist(ctx); err != nil {
		return err
	}
	if finishes {
		armFinish(id)
	}
	if phase := m.takePhase(); finishes || phase.reportToCome() {
		if held := m.staleRelaunch; held != nil && phase.reportToCome() && m.staleForEnd {
			m.staleForEnd = false
			m.log.Info(phase.waitsLine(), "task", pendingNumber(m.claim.Pending), "held", "relaunch", "trigger", *held)
		}
		return nil
	}
	return m.afterTurn(ctx)
}

// backAtEnd gives the claim back in back, in memory, at the end of the turn, or of the compaction,
// it is still stored working for (runHeldAtEnd, endNothingHeld), and answers the pending take whose
// turn ran (takesTurnRan), over which that end arms the end-frame bound through its armFinish: the
// take's id, and false for none. The write and the arm stay each caller's, in its own order:
// runHeldAtEnd, over a stop the runtime refused, arms the bound whether or not its write lands, and
// endNothingHeld arms it only once its write has landed.
func (m *Machine) backAtEnd(back ClaimState) (id string, ran bool) {
	m.claim.State = back
	p := m.claim.Pending
	if p == nil || !m.takesTurnRan() {
		return "", false
	}
	return p.ID, true
}

// afterTurn runs what the machine held for the agent's turn, once that turn is over for the task
// too: its end seen, and the task it was the turn of settled — finished (finishTask), or taken back
// by a provider error's report, by the bound of the wait for its report, or by R5's second bound
// over a take a report put back (reportwait.go). What it holds reads the worker's hands — the
// stale-plugin relaunch would take an unfinished task back from the process it replaces
// (refreshPlugin) — so it may not run while the task whose turn just ended is still unsettled. The
// offer of a refused take runs last, as it does from every other decision. A held suspension that
// waited for the settle of the take runs instead of all of it (settleThenAfterTurn); one whose stop
// the runtime refused before, which waited for no take, is left to its retry at the probe interval
// (suspendHeld), the claim idle meanwhile.
//
// The held relaunch is the refresh gate's second asker (RefreshGate, dispatch://INFRA-2712 R1):
// the claim took no credit while its turn ran, or while the relaunch waited for the report of
// its take's turn (holdStaleRelaunch, relaunchWaitsForReport), so the relaunch run here reaches
// refreshPlugin with none and asks the gate at that guard, which is where every uncredited
// relaunch of this lane asks and which states why the ask lives there rather than here. That
// guard spares the controller's claim, whose relaunch only the wait for a report holds.
// Refused, the relaunch is left to the next sweep, which posts StalePlugin for a claim that is
// idle by then and relaunches it on the first credit that comes free — a minute at the rate the
// lane is held to, against the hours a lane with no gate costs every other wake of the daemon.
// The mark is not re-armed for that: it waits for a turn or a report, and both are over. The
// offer does not wait on the answer either: it runs whichever way the gate answered.
//
// A replacement held for the report of the take's turn (heldReplacement: the held repoint, or a
// parked replacement whose wake's turn came) runs first: raised anew from the top, it goes through
// the lifecycle recheck and the wake gate as every replacement does, and a suspension held
// meanwhile ends the process instead (replaceNow). Its relaunch is onto the current plugin, so the
// stale-plugin relaunch behind it finds the claim launching or its wake waiting and raises nothing
// of its own (refreshPlugin). A replacement whose recheck refuses it leaves the process, as a
// refused one does wherever it is raised, and the sweep raises a repoint again at its next pass.
//
// The operator's stop held for that report (heldStop) runs before all of it and instead of it
// (runHeldStop): it ends the claim for good, and the let-go it makes drops the held replacement
// and the held relaunch with the process they were for (letGo), and a retired claim is offered
// nothing.
func (m *Machine) afterTurn(ctx context.Context) error {
	if m.heldStop {
		return m.runHeldStop(ctx)
	}
	if held := m.heldReplacement; held != nil {
		m.heldReplacement = nil
		if _, err := held(ctx); err != nil {
			return err
		}
	}
	if held := m.staleRelaunch; held != nil {
		m.staleRelaunch = nil
		if err := m.refreshPlugin(ctx, *held, nil, notAtReconnectDeadline); err != nil {
			return err
		}
	}
	return m.offerPending(ctx)
}

// blindTurnEnded is a turn ending on a claim the daemon never saw start one: it is ready or idle,
// and what the end tells it is that the agent's own turn is over. A take that turn refused — a
// take into the turn the claim's agent was in — is offered again now (offerPending, which still
// holds it back from a turn on another claim of its issue, so R6's fence stands). The end is the
// worker's act whoever saw it begin, so it resets the prompt budgets as every turn's end does.
// A start a turn-outcome report ran ahead of is not coming any more where this end is of the
// reported turn or a later one (Machine.reportedAhead), or where the machine's count of the
// process's turns is not known: the turn it would have started is over. The end is counted with
// every other (turnCount), known or not, since the producer numbered that turn too: a count a
// take's report makes known again (learnTurns) holds it, and an end after the end of the turn
// given that take says another turn ran after that one, so that take's report places no count.
// It is a turn's end like any other for the bound of the wait for a take's report, too: the turn
// that ran was a start the bound would have been disarmed by, so it is armed whole again from this
// end (reportwait.go, armReportWait).
//
// Over a take awaiting its turn whose finish is not owed (markTurnUnseen, takeAwaitsTurn), a
// migrated worker's take among them, the turn that ended may have been the take's own: its start
// frame can have been lost in the socket's send buffer as the connection broke, and a hello that
// says the shim's backlog is whole cannot show it missing, since the shim keeps only a frame whose
// write failed. So the take is marked as one whose turn may have run unseen (Machine.turnUnseen),
// and a let-go before that turn's report takes it back as a turn cut off, with its note
// (recordEnd, interrupted, takeBackUnstarted). A start of the take's own turn that comes after
// reads as one, and the mark then says nothing (turnMayHaveRunUnseen).
func blindTurnEnded(m *Machine, ctx context.Context, _ Event) error {
	m.log.Info("supervise: a turn this daemon never saw begin has ended")
	m.disarm(TimerReport)
	m.markTurnUnseen()
	count := m.counted()
	count.ended++
	if !count.known || m.reportedAhead.sequence <= count.ended {
		m.reportedAhead = aheadReport{}
	}
	if m.resetPromptBudgets() {
		if err := m.persist(ctx); err != nil {
			return err
		}
	}
	return m.offerPending(ctx)
}

// turnEndedBeforeReady is a turn of the claim's process ending before its agent said ready: its
// shim has said hello at the claim's generation, so the turn was this process's, and no take
// awaits a turn before ready. Whether its producer numbered that turn is the producer's own:
// pi-legion numbers only the turns that end once its registration has answered, the Claude bridge
// every turn of its process. So the machine no longer knows how many of the process's turns the
// producer has ended (turnCount): counted or let by with the count still known, the end could
// leave the turn the daemon gives a take named as an earlier one, and that turn's late report
// would give back a take whose turn is running (notTheTakesTurn). The count is known again where
// the report of a take accepted after the ready places the turn given that take (learnTurns),
// whichever way the producer numbered this one; a take's anchor kept before it goes with it, and
// a placed count the agent's answer has not checked yet goes back (turnCount.lose).
func turnEndedBeforeReady(m *Machine, _ context.Context, _ Event) error {
	m.log.Info("supervise: a turn ended before the agent said ready; the count of its process's turns is no longer known")
	m.counted().lose()
	return nil
}

// issueTurnEnded is a turn ending on another claim of this claim's issue. It carries nothing the
// claim records: the offer it may unblock asks for itself which claims of the issue are in a turn
// (offerPending, takeable), so a wake that arrives while another role has started one holds the
// task again, and a wake that never arrives — a turn whose process died instead of ending it —
// costs nothing, because the sweep's next observation asks the same question.
func issueTurnEnded(m *Machine, ctx context.Context, _ Event) error { return m.offerPending(ctx) }

func bootInterval(m *Machine, ctx context.Context, _ Event) error {
	return m.probe(ctx, func(context.Context) error {
		m.arm(TimerBoot, m.deps.Timeouts.Boot, "")
		return nil
	}, TimerBoot, m.deps.Timeouts.Boot)
}

// registrationDeadline is a process that has had its boot intervals and whose agent never became
// ready — it never registered, or registered and never said it was ready. Without a held
// suspension, it is retired, counted and relaunched (retireNeverReady). A relaunch the tree
// lifecycle refuses is checked before the retirement, so the process is kept and the deadline
// tried again at the probe interval, as one whose stop fails is. An operator suspension retained
// through a rule-resuming launch instead completes here: no ready will arrive to run it, and
// relaunching would drop the operator's hold. The retirement's stop records what it ended as the
// deadline's (endNeverReady, letGo) — what its agent never did is no turn, so a record comes only
// of background work the process reported — and the relaunch that follows lets nothing more go.
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
	return m.retireNeverReady(ctx, neverReady{
		whose:     "agent " + never,
		never:     never,
		line:      "supervise: the agent " + never + "; retired its process",
		cause:     CauseNeverRegistered,
		uncharged: "the registration deadline",
	})
}

// neverReady is what one deadline retires a process for, whose agent never became ready to the
// daemon (retireNeverReady): whose names the claim's process in the errors ("agent never
// registered"), never is what its agent never did as its end records it (endNeverReady), line is
// the Warn line of the retirement, cause the charged launch failure, and before a step of the
// deadline's own between the relaunch's check and the stop, nil for none. uncharged names the end
// for endUncharged, which charges nothing for it where it says so; "" is a deadline whose end is
// charged always: the reconnect deadline, of a claim a daemon restart restored ready, idle or
// working, whose agent registered as its session, and register clears the mark endUncharged reads
// (Claim.AgentResumeGeneration) in the write that records the session's file.
type neverReady struct {
	whose     string
	never     string
	line      string
	cause     FailureCause
	before    func(context.Context) error
	uncharged string
}

// retireNeverReady is the sequence the registration deadline and the reconnect deadline share
// (registrationDeadline, reconnectDeadline): the process is probed, and one found gone takes the
// death path (probe). One found alive is retired and relaunched with a charged launch failure:
// the relaunch the tree lifecycle would refuse is checked first (checkLaunch), the deadline's own
// step runs (neverReady.before), the stop records what it ended as the deadline's (endNeverReady,
// letGo), and the relaunch that follows lets nothing more go. A deadline that names its end
// (neverReady.uncharged) asks endUncharged after the stop, and an end it answers true for is
// relaunched through launch, charged nothing, in place of relaunchAfterFailure. A refused check,
// a failed step and a stop the runtime refuses each keep the process, and the deadline is tried
// again at the probe interval.
func (m *Machine) retireNeverReady(ctx context.Context, r neverReady) error {
	return m.probe(ctx, func(ctx context.Context) error {
		err := m.checkLaunch(ctx)
		if err == nil && r.before != nil {
			err = r.before(ctx)
		}
		if err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose %s: %w", m.claim.Token, r.whose, err)
		}
		incarnation := m.claim.Locator.Incarnation
		if err := m.suspendProcess(ctx, endNeverReady(r.never)); err != nil {
			m.arm(TimerRegistration, m.deps.Timeouts.Probe, "")
			return fmt.Errorf("retire %s, whose %s: suspend: %w", m.claim.Token, r.whose, err)
		}
		if r.uncharged != "" && m.endUncharged(r.uncharged) {
			// An end charged nothing counts against no budget, so its line names none.
			m.log.Warn(r.line, "incarnation", incarnation)
			return m.launch(ctx, noEnd)
		}
		// The line names the claim (the machine's logger carries it), what its agent never did
		// (neverReady.line), and the launch this retirement counts against its budget
		// (relaunchAfterFailure counts it next), so a controller whose role claim waits out its
		// registration deadline shows in the daemon's log as that, attempt by attempt.
		m.log.Warn(r.line, "incarnation", incarnation,
			"launchFailure", m.claim.Budgets.LaunchFailures+1, "limit", m.deps.Limits.LaunchFailures)
		return m.relaunchAfterFailure(ctx, r.cause, noEnd, nil)
	}, TimerRegistration, m.deps.Timeouts.Probe)
}

// spawn launches a queued claim, in the place its caller took for it when it carries one
// (RequestSpawn.Place), and otherwise in the place the gate joins for it.
func spawn(m *Machine, ctx context.Context, ev Event) error {
	return m.revive(ctx, exemptWake(ev), ev.(RequestSpawn).Place, nil)
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
//
// The process that registers reads its value afresh (CurrentTask) and has seen and asked nothing
// at any revision of it, so the seen report and the take the record kept are the process before
// it's: they go (Claim.TaskSeenRevision, TaskTakeState), and the value reads as
// one nobody reported seeing until the new process reports it seen. A registration whose read of
// the bucket failed answers the process no value, and the listener hands it none either, since
// neither its session nor the revision moved; kept, the old process's report would put that
// process in R5's first wait as one that saw the value and took nothing, and charge it for a
// value it was never handed. Cleared, the value charges nobody and is written again once per
// first bound until the new process reports a revision seen (takeBoundOver). The registration
// is logged (agentRegisteredLine), as the agent of a new process.
//
// The registering process's pod is one an agent of the claim registered a session in, so it goes
// in the claim's set (Claim.RegisteredPods) and stays there, whatever the claim does next. A
// process in no pod (a tmux pane) adds nothing.
func register(m *Machine, ctx context.Context, ev Event) error {
	r := ev.(RequestRegister)
	if err := checkSessionNames(r.Session, r.SessionFile); err != nil {
		return err
	}
	if err := m.holdRecorded(ctx, r.Session, r.SessionFile); err != nil {
		return err
	}
	m.claim.Session, m.claim.SessionFile = r.Session, r.SessionFile
	m.registeredPods.add(locatorPod(m.claim.Locator))
	m.claim.AgentResumeGeneration = 0
	m.claim.CapabilityHash = bytes.Clone(r.CapabilityHash)
	m.claim.PluginContract = r.PluginContract
	if r.PluginContract == m.deps.PluginContract {
		m.claim.PluginRelaunchFor = 0
	}
	m.claim.TaskSeenRevision, m.claim.TaskTakeState = TaskRevision{}, ""
	m.claim.State = StateRegistered
	m.log.Info(agentRegisteredLine, "generation", m.claim.Generation, "session", r.Session)
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
//
// It is the same process registering again, the launch the claim's generation names, so the take
// the record kept at the newest revision is this process's own and stays (Claim.TaskTakeState): a
// take of it that was refused or taken back is what the end of its hold writes the value again
// for (offerPending), and a plugin that asks once per revision would otherwise wait on a value it
// had already asked at.
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
// survived (machine.go's Claim). The registration deadline ends here over a connection whose view
// is whole; over one that is not — a daemon start's before the shim's first hello, or a close
// before this ready — it stays as the reconnect deadline (readyView, connview.go).
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
	m.readyView()
	if m.held != nil {
		return m.suspendHeld(ctx)
	}
	return finishReady(m, ctx)
}

// finishReady persists the current claim before publishing ready, then offers again a take the
// claim's not being ready refused. reready may reach it while the claim is idle or working, so it
// must not rewrite the state to ready.
//
// Between the ready's durable report and the offer, a claim that holds no delivery while its
// record of what the last let-go ended is untold and shows running work is given the daemon's own
// delivery of that record (queueRelaunch), whose queue writes its value, so the new process takes
// the sentence at once. The order is the point: the report is the workflow's ready, read in its
// own transaction while this decision runs (Machine.OnDurableState, the daemon's applyTerminal),
// and it must find the claim holding nothing, or a phase worker the workflow still owes a resume
// task would be queued none (workflow's readyWorkerTask). A claim that holds a delivery takes it
// with the sentence in front, and one whose held task the offer drops instead is given the
// daemon's own delivery there (takeable, queueRelaunch).
func finishReady(m *Machine, ctx context.Context) error {
	if err := m.durableWrite(ctx, StateReady, m.persist); err != nil {
		return err
	}
	if err := m.queueRelaunch(ctx); err != nil {
		return err
	}
	return m.offerPending(ctx)
}

// reready persists the in-memory ready state before answering an agent retry: a prior ready's
// claim write may have failed after the machine made the transition. A retained launch-time
// suspension still wins: the retry completes that suspension instead of publishing ready and
// releasing notices to a claim the operator or workflow is stopping, as it completes one whose
// stop the runtime refused at a turn's end, the claim idle meanwhile. Two holds do not run here,
// both for what they wait on. One held over a claim that is working waits for the end of its turn
// or its compaction (holdSuspension, turnEnded, compactEnded): run here, its stop would cut off
// that turn, which is what the hold exists to keep whole. And one that waits for the report of its
// take's turn (reportwait.go, heldWaits): run here, its stop would come before that report, and
// settle would take back as a cut turn a task whose turn ran whole, or, over a take a report put
// back, cut off a turn whose start may be on its way; that report's finish runs it
// (settleThenAfterTurn), or the bound of its wait (reportBoundOver, turnStartBoundOver), or its
// own deadline (suspendAtTimeout). The ready is answered as any other is meanwhile, the take a
// hold refuses refused still (claimHold). pi-legion sends its ready again from its capability
// schedule while it reads its session idle, and a turn can start before that ready reaches the
// daemon.
func reready(m *Machine, ctx context.Context, _ Event) error {
	if m.heldRunsNow() {
		return m.suspendHeld(ctx)
	}
	return finishReady(m, ctx)
}

func resume(m *Machine, ctx context.Context, ev Event) error {
	if err := m.resumeRefusal(ctx, "resume"); err != nil {
		return err
	}
	return m.revive(ctx, exemptWake(ev), nil, nil)
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
	rule := ev.(RequestRuleResume).Reason
	if !m.claim.RuleMayResume() {
		m.log.Info("supervise: not resumed by rule: its suspension holds until the operator resumes it", "rule", rule, "suspendedBy", m.claim.SuspendedBy)
		return nil
	}
	if err := m.resumeRefusal(ctx, "resume by rule"); err != nil {
		m.log.Info("supervise: not resumed by rule: the claim records a session it does not hold", "rule", rule, "error", err)
		return nil
	}
	m.log.Info("supervise: resuming by rule", "rule", rule, "suspendedBy", m.claim.SuspendedBy)
	return m.revive(ctx, exemptWake(ev), nil, nil)
}

// retry is a failed or retired claim given another run: its budgets start over, and its session,
// when it has one, is relaunched after the process the claim last ran is gone. A pending delivery
// the claim kept goes once the agent is ready. Its event is the workflow's retry or the
// operator's own (operatorRetry), and the pacing reads each its own way (exemptWake). A production
// check look's retry (RequestRetry.OneAttemptLeft) starts no budget over that the failure spent:
// each is left one attempt short of its limit, and the rest stand as the failure left them
// (Budgets.OneAttemptLeft).
//
// A retry that asks for a new session (`legion claims retry --fresh-session`, the controller's
// fresh_session) forgets the recorded session instead, so the launch spawns a fresh agent
// (freshSession), and so does every retry of a claim held for OpenAI's cyber-policy refusal. Every
// other retry, whoever makes it, resumes the recorded session with fresh budgets, whatever the
// claim's run of refused turns: a run that reached launch_failure_limit is the claim's own count
// of refusals, mostly ones that can clear, such as a team limit's 429 that refuses every session
// alike, and no sign that its session is bad (dispatch://INFRA-4508's R5).
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
// the failure it still carries and the session it records, which is what `legion claims list`
// reads off the live machine and what the next persist writes. So a retry in a new session asks
// whether the session may be left behind before the admission (mayForget), which changes nothing,
// and leaves it behind only once the claim is admitted (forgetSession), beside the record's clear.
// A launch check that refuses after the admission — launchAfter's own, or a parked wake's recheck
// when its turn comes — finds both already cleared on the live machine, while the store still
// holds them as AdmitClaim wrote them, until the next persist.
//
// The agent of a claim held for OpenAI's cyber-policy refusal (Claim.HeldForCyberPolicy) is
// launched again by a retry someone decided alone: the held phase's retry (RequestRetry.Decided)
// and the operator's (RequestOperatorRetry), each in a new session (freshSession). A retry the
// daemon makes by itself is refused before anything moves (Claim.CyberPolicyRefusal), since
// launching the agent again would send the session's content to OpenAI again.
func retry(m *Machine, ctx context.Context, ev Event) error {
	if r, own := ev.(RequestRetry); own && m.claim.RetryHeld(r) {
		return m.claim.CyberPolicyRefusal()
	}
	fresh, why := freshSession(m.claim, ev)
	if fresh {
		// A session the claim does not hold is left behind only once its role no longer leads
		// to the process under it (mayForget); the retry is refused, and the claim left as it
		// was, until then.
		if err := m.mayForget(ctx, "retry"); err != nil {
			return err
		}
	} else if err := m.resumeRefusal(ctx, "retry"); err != nil {
		// A retry that relaunches the recorded session is refused it the way a resume is
		// (resumeRefusal), before the budgets start over.
		return err
	}
	oneAttemptLeft := false
	if r, ok := ev.(RequestRetry); ok {
		oneAttemptLeft = r.OneAttemptLeft
	}
	return m.revive(ctx, exemptWake(ev), nil, func() {
		if fresh {
			// lastRefusal is the provider's words for the newest refused turn, which the claim
			// stores for the reason LastRefusal gives.
			m.log.Info("supervise: retrying in a new session", "why", why, "session", m.claim.Session,
				"refusedRun", m.claim.RefusedRun(), "lastRefusal", m.claim.LastRefusal())
			// The failure the claim carries is the fuller answer where it has one — a provider
			// error that cannot clear names its status, a death its cause — and the retry's own
			// reason answers for a claim that failed nothing, an operator's retry of a worker it
			// stopped. It is read here, before the record below is cleared.
			m.forgetSession(cmp.Or(m.claim.Failure, why))
		}
		budgets := Budgets{}
		if oneAttemptLeft {
			budgets = m.claim.Budgets.OneAttemptLeft(m.deps.Limits)
			m.log.Info("supervise: retrying with one attempt left of each budget the failure spent",
				"failed", m.claim.Budgets, "budgets", budgets, "cause", m.claim.FailureCause)
		}
		m.claim.Budgets, m.claim.PendingCause = budgets, ""
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
// The daemon's controller has a retry of its own, its keeper, so it alone is refused, unless it is
// held for a cyber-policy refusal, which the keeper retries no more, so the operator's retry is
// its one way back (Claim.KeeperRetries). A claim's role never changes, and whether it is held is
// the machine's own state, read inside this decision, so the refusal cannot race the decision it
// guards.
//
// It relaunches the session the claim records, unless the retry asks for a new one
// (RequestOperatorRetry.FreshSession) or the claim is held for a cyber-policy refusal: retry then
// forgets the session, so the launch spawns a fresh agent (freshSession). A launch whose session
// no session store holds any more starts a fresh agent too (startFresh).
func operatorRetry(m *Machine, ctx context.Context, ev Event) error {
	if m.claim.KeeperRetries() {
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
//
// The operator's stop of one claim (RequestStop.WaitForReport) that reaches it after its take's
// turn ended and before that turn's report reached the daemon waits for the report instead, as a
// suspension does, with the held repoint's bound (boundedReportHolds, holdStop): stopped here, the
// process would take the report with it, and settle would take back as a cut turn a task whose turn
// ran whole, which a retry of the stopped claim's session would run again. A stop during a running
// turn cuts that turn off as before, since the hold begins only once the turn is over. Every other
// stop ends the claim here at once.
func stop(m *Machine, ctx context.Context, ev Event) error {
	if !m.claim.treeRoot() {
		if ev.(RequestStop).WaitForReport && m.boundedReportHolds() {
			return m.holdStop()
		}
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

// deliver queues the task and writes its value (queue): the worker takes it once its host is idle,
// whatever the claim's state when it was queued, so no state of a running claim sends it.
func deliver(m *Machine, ctx context.Context, ev Event) error {
	return m.queue(ctx, ev.(RequestDeliver))
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
	return m.revive(ctx, d.ID == "", nil, nil)
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
// The machine asks the role nothing. The sweep enqueues the round's ask to the role's own Envoy
// topic once the claim records the round (internal/daemon's openPodUpdate, record.UpdateAsk), and
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
// round's id. An ask an older daemon queued as a task for the old round goes in the same step
// (dropUpdateTask).
//
// The claim is written only where this post changed it: a round the claim was not in. The sweep
// posts to every claim of every open round at every tick, for as long as a round runs, and a post
// that decided nothing has nothing to write — as refreshPlugin's own re-post returns before any
// write. A round the claim was not in also lifts the hold an answer to the old round put on the
// claim's next task (answeredOpenRound), so that task is offered here (offerPending): nothing
// else would write its value again before the claim's next observation.
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
	// the old round (clearUpdate), and nothing else would write that task's value again until the
	// claim's next observation. offerPending writes nothing for a claim in a turn, and nothing for
	// a task another hold stands over, so a claim with no held task costs nothing here.
	return m.offerPending(ctx)
}

// updateTaskPrefix starts the delivery id of every ask an update round queued as a task, which
// only an older daemon did: the prefix, the round, a colon and a word of that ask's own
// (updateRoundOf). This daemon publishes the round's ask to the role's Envoy topic instead
// (record.UpdateAsk), so an id of this shape is one a restored claim brings back from the store.
// The id never changes, so it is the one record of which round such an ask is of, which every
// reader of an ask reads off it (updatesRound, dropUpdateTask and takeable's drop).
const updateTaskPrefix = "update:"

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

// dropsUpdateTask is whether the claim's pending delivery is the ask of the round the claim
// records that no take holds: the one delivery a round leaving the claim takes back. It is a
// question of its own, apart from the drop, for the claim that ends in the same step: the
// retirement reads it, clears the round's fields and then writes the claim and the delete
// together, where a drop made first would store a retired claim that still records its round
// (machine.go's retire).
func (m *Machine) dropsUpdateTask() bool {
	p := m.claim.Pending
	return updatesRound(p, m.claim.UpdateRound) && !p.taken()
}

// dropUpdateTask drops an ask of this claim's round that an older daemon queued as a task and
// that no take holds. This daemon queues no such task, and a take of one is dropped (takeable),
// but a claim it restored from the store can hold one, and a round leaving the claim drops it in
// the same step: the words name a round that is over — a pod this daemon has already replaced —
// and while the claim holds them, every task of the claim's next phase is refused behind this one
// (ErrDeliveryPending). An ask the claim owes is one of those: a turn of it that ended in a
// provider error took it back to the ladder's wait. An ask a take holds is left where it is: that
// turn is the worker's, and its finish retires the delivery. Every end of a round for this claim
// runs it — the sweep's (endPodUpdate, leaveRound), the round a new one replaces (openPodUpdate),
// and the registration's (endCurrentPluginRound). The claim's own retirement makes the same drop
// in its one write instead (machine.go's retire).
func (m *Machine) dropUpdateTask(ctx context.Context) error {
	if !m.dropsUpdateTask() {
		return nil
	}
	m.log.Info("supervise: the update round is over for this claim; the ask an older daemon queued as a task and nothing took is dropped",
		"round", m.claim.UpdateRound, "task", m.claim.Pending.Number, "delivery", m.claim.Pending.ID)
	return m.dropPending(ctx, valueDeletedDropped)
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
// ran is left failed for the workflow's retry to decide, and a take from a claim with no agent is
// refused (claimHold). Leaving the round on it instead would outlive the pod the round
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
// nothing took goes (dropUpdateTask), every field the round wrote goes (clearUpdate), the claim
// is written, and the task the round was holding back is offered again. It is the whole of every
// round's end: a pod's round, whose pod is gone by the time that end is posted (endPodUpdate),
// and a plugin round, which no daemon on this release opens and which ends wherever an earlier
// daemon's is found — at a registration that speaks this daemon's contract
// (endCurrentPluginRound), or at the end the sweep posts. They differ only in what they log.
// offerPending writes nothing for a claim that is not ready or idle, so a failed or suspended
// claim is offered nothing by it.
func (m *Machine) leaveRound(ctx context.Context) error {
	if err := m.dropUpdateTask(ctx); err != nil {
		return err
	}
	m.clearUpdate()
	if err := m.persist(ctx); err != nil {
		return err
	}
	return m.offerPending(ctx)
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
// turn runs, no take whose turn ended still owes its report and no claim with a process in the
// round's pod reports background work that runs or has sent no report since its last turn started
// (internal/daemon's roundEnd.holding, the one statement of that rule). So an answer brings the
// round's end forward only while no role reports background work.
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
// asked of the role only at a later sweep (internal/daemon's openPodUpdate publishes once the
// claim records the round), so the id this refusal prints is one whose words the worker may not
// have read, and an answer given for it would be counted for a round the worker was never asked
// (podUpdateSignalled is the one guard that stops exactly that). So the worker is told how the
// daemon asks — once per round, as a message on the role's own topic — and to answer the round
// the newest such message names, or to end its turn where no message names the claim's round.
// For a claim the daemon is asking nothing of, it is told so.
func foreignRoundRefusal(c Claim, answered string) error {
	in, next := fmt.Sprintf("%q", c.UpdateRound), fmt.Sprintf(
		"The daemon asks each role one time in each update round. "+
			"The daemon sends that ask as a message on the role's own topic. "+
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

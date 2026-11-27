package supervise

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// EndedWork is the claim's record of what the last let-go of its process ended of the work its
// agent had running (dispatch://INFRA-3276 R3): when, why in the daemon's own words, whether a
// turn was cut off and whether that turn was the pending task's, what background work the
// process's own activity report named, whether the daemon held a report of that process that
// speaks of its last turn at all, and whether the record has been told. The next process is told
// it in a prompt: the sentence RelaunchSentence composes from it, in front of the task the claim
// holds or as a delivery of the daemon's own when it holds none (Delivery.message,
// queueRelaunch), and marked told at the confirmation of the prompt that carried it
// (confirmCarrying). This is the record that sentence is built from, written where the machine
// lets the process go (letGo, recordEnd) and persisted on the claim's row (store's ended_work),
// since that prompt can be a relaunch, a wake and a daemon restart away.
//
// The zero value is a claim whose let-go ended nothing worth telling, or that has been let go of
// none: Cause is empty, but for the two exceptions FromOutside and Displaced state.
type EndedWork struct {
	// At is when the process was let go, and Cause why: a sentence the daemon writes, beginning
	// with what the decision that let the process go found (the cause constants and functions
	// below), so the sentence it is told in can name it after "because". Both are of one end, the
	// one whose cause is told, save where an end that ended nothing worth telling leaves its typed
	// record over an earlier launch's that still tells (leaveRecord): At is then that end's, and
	// Cause, with Death, the earlier end's, whose words are what is told. A record joined over an
	// earlier one still untold (joined) takes the newer end's moment and cause, and so does a
	// record of a death a running read ended that the next judgement types (retype: died,
	// markFromOutside, markDisplaced), since that end is the one that let the process go.
	At    time.Time `json:"at"`
	Cause string    `json:"cause"`
	// Death is the kind of the cause the runtime read of the death this record is of, where Cause
	// opens with the daemon's plain sentence of it (runtime.DeathCause, plainCause, endDied), and
	// "" for every other let-go. It is what the record is decided and read by where that sentence
	// matters: a death of a claim holding a task is recorded whatever it cut off (recordEnd), and
	// the out-of-memory note in front of a task keeps only its guidance where the record already
	// names the `roles` container's memory limit (outOfMemoryNote). It is stored in the claim's
	// JSON with the rest of the record, and an earlier daemon reading the row passes over the key.
	Death runtime.DeathCauseKind `json:"death,omitempty"`
	// FromOutside is whether the end this record is of was one from outside the pod: the runtime
	// read it so (runtime.PodEndNotOurs) and the daemon recorded it so (endedFromOutside), before
	// the take-back and the decision that follow. It is written where the record is made
	// (recordDisplaced, markFromOutside), or by markFromOutside over a record of the same launch
	// that no end from outside the pod made (markFromOutside says which), never read off Cause,
	// and it is what every decision reads such an end by: a report of the ended process (judge,
	// recordedFromOutside) and the boot's replay of the decision after a restart
	// (decidedFromOutside). So a reword of endFromOutside, a sentence told to roles, leaves the
	// records an earlier daemon wrote read as they were.
	//
	// It is one of two exceptions to the rule that a let-go that ended nothing worth telling
	// records none (recordEnd, Claim.EndedWork, the store's ended_work), Displaced being the
	// other: an end from outside the pod always leaves a record of its launch's end, whatever
	// that end ended, so the decision's write carries this field. Where the end ended nothing
	// worth telling, the record is told already, by no delivery (Told with ToldBy empty), so no
	// prompt carries it and the next process is told nothing of that end. It is stored in the
	// claim's JSON with the rest of the record, and an earlier daemon reading the row passes
	// over the key.
	FromOutside bool `json:"fromOutside,omitempty"`
	// Died is whether the end this record is of was a death died judged. died records the end
	// before its take-back and its decision (recordGoneEnd), and the store can refuse the
	// take-back, so a record of the claim's launch that says so stands for a judged death whether
	// or not that death is decided yet. It is written where died judges the end, on the record
	// it makes or on the one of the same launch it finds standing (died), as FromOutside is,
	// never read off Cause, and it is what a later report of the process read as from outside
	// the pod finds that death by (judge, judgedDeath): the first judgement stands, and a reword
	// of a sentence leaves the records an earlier daemon wrote read as they were. A verdict that
	// finds the process running clears it, with the charge (reportedProcess): the process lives,
	// so the death judged of it is over, and a later end of it is judged afresh. It is stored in
	// the claim's JSON with the rest of the record, and an earlier daemon reading the row passes
	// over the key. A record an earlier daemon stored without it reads as no death died judged,
	// so where no decision was stored with it, a later report of that end read as outside goes
	// to endedFromOutside, as it did on that daemon.
	Died bool `json:"died,omitempty"`
	// Displaced is whether the end this record is of was one displaced judged: the daemon's own
	// replacement of the pod, or its take-over of another runtime's process
	// (runtime.Observation.Displaced). It is written where the record is made (recordDisplaced,
	// markDisplaced), as Died and FromOutside are, never read off Cause, and it is what a later
	// report of the process read neither way finds that judgement by (judge, recordedDisplaced):
	// the first judgement of a process stands, and a displacement is one, so such a report over
	// no death died judged first (judgedDeath) is that displacement again, charged nothing, and
	// never a death. A later report read as from outside the pod types the record as one from
	// outside the pod and leaves this as it is (markFromOutside), so the record still says
	// displaced judged the end first.
	//
	// It is the second exception to the rule FromOutside states: a displacement leaves a record of
	// its launch's end whatever that end ended, but over an end already decided
	// (Claim.DeathCharged, markDisplaced), where the decision's record or its absence stands; it is
	// told already by no delivery where it ended nothing worth telling, so a later report finds the
	// judgement standing whatever the claim held. It is stored in the claim's JSON with the rest of
	// the record, and an earlier daemon reading the row passes over the key. A record an earlier
	// daemon stored without it reads as no displacement, so a later report of that end read
	// neither way goes to died, as it did on that daemon.
	Displaced bool `json:"displaced,omitempty"`
	// Outlived is whether a verdict that found the process running ended the death judgement this
	// record carried (reportedProcess clears Died and sets this): the process ran on, so the end
	// this record is of never came, and the verdict marks it told by no delivery, so no prompt,
	// relaunch or catch-up tells it. What a later end of the process cuts off is that end's own to
	// record and tell: recordEnd records such an end afresh over this record, with its own turn,
	// task and report, untold, under its own cause and type, as for a first end of the launch, so
	// the role is told that end's own cause where the end cut off its work. Where that end ends
	// nothing worth telling, the judgement it is types this record under its own cause instead,
	// untold again (retype), so the role is told that cause alone. Every write of a type clears
	// it. It is stored in the claim's JSON with the rest of the record, and an earlier daemon
	// reading the row passes over the key.
	Outlived bool `json:"outlived,omitempty"`
	// SentAgain is the id of the task the death this record judged took back (interrupted), where
	// a verdict that found the process running ended that death (reportedProcess): the task goes
	// again to the process that never ended, behind the one sentence that says the daemon read
	// that process wrongly as ended and tells no end (Delivery.message, Claim.sentAgain). The
	// take-back's marks on the task go with the death. Every write of a type clears it, and a
	// later end recorded afresh leaves a record without it. It is stored in the claim's JSON with
	// the rest of the record, and an earlier daemon reading the row passes over the key, so a
	// rollback loses the sentence alone, the record being told either way.
	SentAgain string `json:"sentAgain,omitempty"`
	// Generation is the launch whose process this records the end of, which is what keeps one
	// process's end to one record: a decision that records the end of a process it already knows
	// is gone (died, displaced) and the let-go that follows it in the same decision or at a wake's
	// turn are two calls about one launch, and the second finds this launch's record standing
	// (recordEnd).
	Generation uint64 `json:"generation"`
	// Turn is whether a turn was running when the process was let go: the claim was working for
	// one, which is a turn the stream showed begin, one an earlier daemon's process was in, or
	// one the agent's busy answer said it was in (recordBusyTurn). A claim working for nothing
	// that runs is in no turn (endState): one whose compaction has no turn under it (compactStart
	// puts the claim in working in no turn), and one whose held suspension runs at the end of its
	// turn or its compaction (suspendHeldAtEnd). TaskTurn is whether the pending task's turn was
	// cut off: the decision that let the process go took the task back from it (interrupted,
	// Machine.cutTurnOf), whatever the claim's state, or a turn was running and had confirmed the
	// task. So TaskTurn can stand without Turn: a turn that ended while its prompt's result was
	// still to come leaves the claim idle with the task confirmed, and a death, a displacement, a
	// repoint, a stale-plugin relaunch or a paced wake's turn takes that task back, to be sent
	// again as interrupted (Delivery.message). A confirmed task no decision takes back is in no
	// turn cut off: that of the held stop at a turn's end, which settle retires, and that of an
	// idle claim whose let-go takes nothing back (suspendNow).
	Turn     bool `json:"turn,omitempty"`
	TaskTurn bool `json:"taskTurn,omitempty"`
	// Background is whether the process's own last activity report of its launch said background
	// work of its session was running, and Kinds the kinds that report named (the store keeps the
	// known ones alone: claim.BackgroundWork.KnownKinds). A report that said work ran and named no
	// known kind is Background with no Kinds. They are taken from that report however old it is: a
	// report read before the start of the process's last turn still says what background work ran
	// in that process, though it says nothing of that turn (Reported, recordEnd).
	Background bool     `json:"background,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
	// Reported is whether the store held an activity report of that launch (Deps.ClaimActivity)
	// that speaks of what the process last did: for a process that ran a turn, a report read no
	// earlier than the start of its last turn (Claim.LastTurnStart), since a report older than
	// that turn has said nothing about it — the idle rule's own rule (claim.ReadSessionWork).
	// Such a report still gives the record its Background and Kinds, so a record that is not
	// Reported can name background work. A record that is not Reported is one whose process ran a
	// turn and left no report of it — a Claude Code worker reports only at its Stop hook, so a turn
	// cut off before its end has none and the newest report is the turn before's, and a stale
	// plugin's report is refused — so what background work that turn started is unknown, as is all
	// of the process's where it left no report at all, which is said rather than read as nothing
	// (dispatch://INFRA-3276 R3, "unknown is not silence").
	Reported bool `json:"reported"`
	// Told is whether the record has been told to a later process, and ToldBy the delivery whose
	// confirmation told it, as confirmCarrying states, or "" for an old session's record the
	// abandonment of that session ended (forgetSession), for a record told at its making
	// (FromOutside, Displaced), and for the record of a death a running verdict ended (Outlived).
	// A record not yet told is replaced by a newer one only as the union of the two (joined), so
	// what the earlier let-go ended is not lost before anyone reads it.
	Told   bool   `json:"told,omitempty"`
	ToldBy string `json:"toldBy,omitempty"`
}

// Tells is whether the next prompt to this claim's agent must still carry the record: a let-go
// recorded something and no turn has confirmed a prompt that carried it.
func (e EndedWork) Tells() bool { return e.Cause != "" && !e.Told }

// joined is newer recorded over e, an earlier record nobody has been told yet: the newer moment,
// cause, death and launch, and the union of what each ended — a turn either cut off, the task's
// turn either cut off, every background kind either named, and no report where either process
// left none. What the earlier process ended is still true and still untold, so the newer record
// keeps it rather than dropping it. The cause, its death and whether the end was from outside the
// pod are the newer's together, since the last two say what kind of end the cause names; so are
// whether it was a death died judged (Died) and whether displaced judged it (Displaced).
func (e EndedWork) joined(newer EndedWork) EndedWork {
	newer.Turn = newer.Turn || e.Turn
	newer.TaskTurn = newer.TaskTurn || e.TaskTurn
	newer.Background = newer.Background || e.Background
	kinds := slices.Clone(e.Kinds)
	for _, kind := range newer.Kinds {
		if !slices.Contains(kinds, kind) {
			kinds = append(kinds, kind)
		}
	}
	newer.Kinds = kinds
	newer.Reported = newer.Reported && e.Reported
	return newer
}

// retype makes e, the record of the claim's launch that no judgement types (unjudgedRecord), the
// record of the end judged now: at its moment, under its cause and death kind, and no longer one
// a running verdict outlived. The caller writes the judgement's type. A record a running verdict
// marked told (Outlived) is untold again, since the end it recorded never came and this one is
// the process's first to tell.
func (e *EndedWork) retype(at time.Time, cause string, death runtime.DeathCauseKind) {
	if e.Outlived {
		e.Told, e.ToldBy = false, ""
	}
	e.At, e.Cause, e.Death, e.Outlived, e.SentAgain = at, cause, death, false, ""
}

// ActivityReport is one launch's last activity report as the store holds it
// (PutClaimActivity's columns, the phase reminder aside): when the session read what it was
// doing, whether background work of its own was running, and the kinds it named.
type ActivityReport struct {
	At         time.Time
	Background bool
	Kinds      []string
}

// The causes a let-go records, in the daemon's words: each is what the decision that let the
// process go found, written so that it reads after "because" (EndedWork.Cause). noEnd is the
// cause of a let-go that records nothing: a launch of a claim that records no process to let go
// — a spawn, a resume, a retry, and every relaunch whose process an earlier step of the same
// recovery already let go — and a retirement that ends the claim's work on purpose, the tree's
// or the issue's close and the agent's own exit (retire). endStopped is a stop's (RequestStop:
// `legion claims stop`, the operator's close of a tree stopping its other agents, and the daemon
// stopping a controller it launched once the deployment leaves the controller to its operator),
// since every retry that keeps the session relaunches a stopped agent on the session its claim
// records — the operator's (`legion claims retry`: RequestAskedRetry for an architect or a worker
// whose issue works no phase of its role, and the workflow's start of that phase, below, for a
// worker in its phase), the controller keeper's retry of a retired controller (daemon's
// controllerKeeper.keep, RequestRetry) and the workflow's start of a retired worker's phase
// (daemon's outbox.supervise, RequestRetry) — and that session's next turn reads what the stop
// ended (dispatch://INFRA-3276 R3). endDisplaced and endFromOutside are the two causes of a process
// found gone through no fault of its own, which recordDisplaced records, and they say who ended
// it: endDisplaced an act of the daemon's own (runtime.Observation.Displaced: its replacement of
// the pod, or its take-over of another runtime's process; displaced), and endFromOutside an end
// that something outside the pod made (runtime.PodEndNotOurs, endedFromOutside), which the daemon
// did not make and so never tells as a replacement.
//
// The cause is read into an agent's prompt as it stands (RelaunchSentence), so every word of it
// that is not the daemon's own — the runtime's account of a death, the failure sentence that can
// carry a provider's or a launcher's text — is quoted as a Go string (strconv.Quote), as
// abandonedTask quotes the provider's reason: the quotes bound it, and a newline or a quote inside
// it is escaped rather than read as a sentence of the daemon's own.
const (
	noEnd          = ""
	endRetired     = "the daemon retired it after its prompts came to nothing"
	endStalePlugin = "it ran an older plugin and was started again on the current one"
	endDisplaced   = "its pod was replaced"
	endFromOutside = "its pod was ended from outside it"
	endRepointed   = "its address changed"
	endStopped     = "its claim was stopped"
)

// endDied is a death's cause. Where the runtime read a cause the daemon tells in plain words it is
// that sentence (plainCause), the daemon's own and unquoted, then, for an eviction for the pod's
// own disk, the note of what the kubelet counted and where the role's large files belong, and then
// the runtime's own account of the death (deathAccount), quoted; a death of any other cause is the
// account alone, quoted, behind "the process died". An eviction for the pod's disk limit gets the
// note of what that limit counts, the pod's test database among it where the pod kept that on a
// counted volume (diskLimitTask); one over its ephemeral-storage request keeps the note of R9
// alone (diskTask), since the kubelet's message there names only the container's own layer and
// logs. The record's sentence carries it into the role's next task (RelaunchSentence,
// interruptedTask), so that task reads what ended the process and its number before the
// runtime's detail (dispatch://INFRA-1117 R9, R11).
func endDied(observation runtime.Observation) string {
	account := strconv.Quote(deathAccount(observation))
	plain := plainCause(observation.Cause)
	if plain == "" {
		return "the process died: " + account
	}
	told := plain + ". "
	if note := diskNote(observation.Cause); note != "" {
		told += note + " "
	}
	return told + runtimeDetail + account
}

// endRecordedDeath is the cause of a death the previous daemon recorded, read back from the claim
// (Claim.LastDeathCause, which deathCause wrote) by the boot that relaunches it
// (relaunchRecordedDeath): its plain sentence and the runtime's account, quoted, where the death
// had a cause the daemon tells in plain words, and otherwise the account behind "the process
// died", quoted whole, as endDied writes them. The claim keeps no kind beside the sentence, so the
// recorded cause is read as a plain sentence only where it opens as one of plainCause's does
// (opensAsPlainCause): the account alone, a death of no plain cause, opens with its
// observation's kind, and the pod's events and log lines in it can carry runtimeDetail, which a
// cut of the account would put into the next task unquoted, as the daemon's words. A plain
// sentence carries no runtimeDetail of its own, so the first one ends it. The disk notes endDied
// adds come from the observation, which the claim does not keep, so a recorded disk death is told
// without them.
func endRecordedDeath(cause string) string {
	if opensAsPlainCause(cause) {
		if plain, account, ok := strings.Cut(cause, runtimeDetail); ok {
			return plain + runtimeDetail + strconv.Quote(account)
		}
	}
	return "the process died: " + strconv.Quote(cause)
}

// endStalled is the stall backstop's cause, with the bound the claim's last step passed. It names
// a turn only where the stop finds one (inTurn: the claim reads working, endState). The daemon's
// sweep posts over any claim stored working past its bound (the daemon's backstopStalledTurns),
// and that includes a claim working for a compaction with no turn started under it, which endState
// reads as no turn: there it is the claim that showed no progress, so the sentence the next process
// reads never says a turn stalled and then that it was in none (relaunchSentence).
func endStalled(bound time.Duration, inTurn bool) string {
	if inTurn {
		return fmt.Sprintf("its turn showed no progress for %s", bound)
	}
	return fmt.Sprintf("its claim showed no progress for %s", bound)
}

// endedPastBound is the cause of the controller keeper's end of a controller turn open longer
// than its bound (Machine.endTurnPastBound, dispatch://INFRA-2045 R2), with that bound, as
// endStalled is the stall backstop's.
func endedPastBound(bound time.Duration) string {
	return fmt.Sprintf("its turn stayed open longer than %s, so the daemon ended it", bound)
}

// endSuspended is a suspension's cause, naming who suspended the claim.
func endSuspended(by SuspendSource) string { return fmt.Sprintf("it was suspended (%s)", by) }

// endFailed is a failure's cause, with the sentence the failure is recorded under (fail), quoted:
// it can carry the provider's refusal or the launcher's error.
func endFailed(why string) string { return "its claim failed: " + strconv.Quote(why) }

// endNeverReady is the registration deadline's cause, with what its agent never did
// (registrationDeadline), and then that the daemon retired the process for it: the cause reads
// after "because" and carries no "because" of its own.
func endNeverReady(never string) string { return "its agent " + never + ", so the daemon retired it" }

// recordEnd records what letting the claim's process go ends, for cause, before anything of the
// process is cleared: letGo calls it first, and so do the decisions that find the process already
// gone (died, recordDisplaced), through recordGoneEnd, before they take its task back, since what
// they find is the whole of what that process's end did and the let-go they lead to can be a
// wake's turn away.
//
// It records nothing for a claim that records no process, for noEnd, for a launch whose end is
// recorded already (EndedWork.Generation), and for a process whose end is decided
// (Claim.DeathCharged): the decision recorded what that end ended before it decided it, died
// and endedFromOutside alike, or found nothing worth recording, and that stands through every
// let-go of that process — the relaunch's, run in the decision or at a wake's turn, a held
// suspension's, a failure's — and every later report of it. Read again here, the launch's
// activity could answer otherwise than it did for the decision, a store that refuses the read
// among them, and a role whose death ended nothing worth telling would be told that its process
// died (queueRelaunch). A verdict that finds the process running clears the decision
// (reportedProcess), so the next end of that process is recorded as its own. Otherwise it reads
// what it decides on: whether a turn was running (the claim is working for one: endState),
// whether the pending task's turn was
// cut off (this let-go's own decision took the task back, whatever the claim's state: cutTurnOf;
// or the turn that was running had confirmed it), and the launch's own activity report from the
// store (launchActivity), read here because the machine's claim holds only what it read at its
// own boot and because the launch that follows clears it (store's putClaim). Of a process that
// ran a turn, a report read before that turn began (Claim.LastTurnStart) is no report of that
// turn, as the idle rule holds (claim.ReadSessionWork): the record is not Reported, rather than
// reading the turn before's "nothing runs" as this one's. It still takes that report's background
// work, which the report saw running in this process, so the sentence of a turn cut off names
// that work and then says that the daemon has no report of what the turn started
// (relaunchSentence). Nothing is written when no turn was running, no task was taken back and a
// report of the last turn says nothing runs, or when no turn was running, no task was taken back
// and none had run in this process, which can have started no background work; a process that
// ran a turn and left no report of it is recorded as unreported rather than as having ended
// nothing. A take-back always leaves a record of its let-go, since the task it took is sent again
// behind that record's sentence (Delivery.message). A record still untold is replaced as the
// union of the two (joined).
//
// A claim stored working is in a turn, except where it is working for nothing that runs
// (endState): while a compaction runs with no turn started under it, since compactStart puts the
// claim in working in no turn; once a gone verdict has dropped such a compaction with its process
// (compactionLostProcess), until the let-go, which a parked relaunch makes a wake's turn after the
// death or the displacement that calls this first; and while a held suspension runs at the end of
// the turn or the compaction the claim was working for (suspendHeldAtEnd). The let-go there cuts
// off no turn: the record is not Turn, its task is in no turn cut off unless the let-go's own
// decision takes it back (above), and whether the process ran a turn is read from the state that
// compaction or that end gives the claim back in, ready or idle. A turn Oh My Pi started under
// the compaction (compacting.inTurn) is a turn as any other.
//
// A record written here is new words, so a prompt that carried the sentence of the record it
// replaces carried none of this one: the mark of such a prompt goes (Machine.carried), and only a
// prompt sent from here on tells this record when its turn begins (confirmCarrying).
//
// Before anything else it takes back a telling the let-go cannot vouch for, that of a confirmation
// a turn took while the send of its prompt was in flight or a prompt of the task still waited on
// its result (untellInFlight, where that rule is), so the record that telling had marked told is
// untold again and the record written here joins it.
//
// It writes nothing itself: the record is a field of the claim, and the let-go's own write — the
// launch's, the suspension's, the failure's, the retirement's, the death's charge, or the decision
// of an end from outside the pod (endedFromOutside) — carries it, and the untelling with it. So it
// costs the relaunch one read and waits for nothing else.
//
// death is the kind of the cause the runtime read of a death this let-go records, whose cause
// opens with the daemon's plain sentence of it (endDied, EndedWork.Death), and "" for every other
// let-go. Such a death of a claim that holds a task is recorded whatever the let-go cut off,
// though no turn ran and nothing was taken back: the death is charged (chargeDeath) and the task
// is the role's next one, which must say what killed the process (dispatch://INFRA-1117 R9). A
// task the agent never confirmed, of a claim in no turn, is in no turn the death cut off, and
// without this the record would be skipped. A claim holding no task is left to the rule above:
// its death is not charged, and a record kept for it would wake a parked agent with a delivery of
// its own (queueRelaunch).
//
// fromOutside is whether the end is one from outside the pod, which the record carries from its
// making (EndedWork.FromOutside): true where endedFromOutside records it (recordDisplaced), and
// false for every other let-go. displaced is whether displaced judged the end, which the record
// carries from its making too (EndedWork.Displaced): true where displaced records it
// (recordDisplaced), and false for every other let-go. died is whether the end is a death died
// judged, which the record carries from its making as well (EndedWork.Died): true where died
// records it, and false for every other let-go.
func (m *Machine) recordEnd(ctx context.Context, cause string, death runtime.DeathCauseKind, fromOutside, displaced, died bool) {
	m.untellInFlight()
	if cause == noEnd || m.claim.Locator == nil {
		return
	}
	if m.claim.DeathCharged {
		return
	}
	// A record of this launch stands for its end, but one a running verdict outlived
	// (EndedWork.Outlived): the process ran on after it, so this end is recorded as its own.
	if held := m.claim.EndedWork; held.Cause != "" && held.Generation == m.claim.Generation && !held.Outlived {
		return
	}
	state := m.endState()
	ended := EndedWork{Cause: cause, Death: death, FromOutside: fromOutside, Displaced: displaced, Died: died,
		Generation: m.claim.Generation, Turn: state == StateWorking}
	// The pending task's turn was cut off where this let-go's own decision takes the task back
	// (interrupted, cutTurnOf), whatever the claim's state: a turn that ended before its prompt's
	// result came leaves the claim idle with the task confirmed, and a take-back from there sends
	// it again as interrupted, behind this record's sentence (Delivery.message). Where a turn was
	// running, a task it confirmed was in it. A confirmed task no decision takes back is in no
	// turn cut off: the turn whose end runs a held suspension (suspendHeldAtEnd), whose task
	// settle retires instead, and an idle claim's let-go that takes nothing back (suspendNow).
	if p := m.claim.Pending; p != nil {
		taken := m.cutTurnOf != "" && p.ID == m.cutTurnOf
		ended.TaskTurn = taken || (ended.Turn && !p.ConfirmedAt.IsZero())
	}
	// A claim idle or working has run a turn in this process: idle is reached only by a turn's
	// end, and a relaunched process comes up through ready. A claim working for a compaction in
	// no turn has run one where the compaction took it from idle (endState).
	ranTurn := state == StateWorking || state == StateIdle
	report, reported := m.launchActivity(ctx)
	if reported && report.Background {
		ended.Background, ended.Kinds = true, slices.Clone(report.Kinds)
	}
	// A report read before the claim's last turn began has said nothing about that turn
	// (claim.ReadSessionWork), so it is no report of it, though the background work it saw is
	// kept above. LastTurnStart is the daemon's first word of the turn (Claim.LastTurnStart's doc
	// lists who dates it). A daemon restarted under a working claim reads the one the daemon before
	// it stored.
	ended.Reported = reported && !(ranTurn && report.At.Before(m.claim.LastTurnStart))
	kept := ended.Death != "" && HoldsTask(m.claim.Pending)
	if !kept && !ended.Turn && !ended.TaskTurn && !ended.Background && (ended.Reported || !ranTurn) {
		return
	}
	ended.At = m.deps.Clock.Now()
	m.log.Warn("supervise: the relaunch ended work the agent had running",
		"cause", cause, "generation", ended.Generation, "turn", ended.Turn, "taskTurn", ended.TaskTurn,
		"background", ended.Background, "kinds", ended.Kinds, "reported", ended.Reported,
		"delivery", pendingID(m.claim.Pending))
	if held := m.claim.EndedWork; held.Tells() {
		ended = held.joined(ended)
	}
	m.claim.EndedWork, m.carried.ended = ended, false
}

// recordGoneEnd is recordEnd for the decisions that find the claim's process already gone and
// record its end before they take its task back (died, recordDisplaced). Each takes a confirmed
// task back whatever the claim's state — died and endedFromOutside at once, displaced first thing
// in its replacement (replaceProcess, replaceNow) — so the task is marked here as taken by this
// decision (Machine.cutTurnOf), under the id it still holds, and the record says its turn was cut
// off as the record of a let-go that follows its take-back says it. The take-back then moves the
// mark to the id it gives the task (interrupted), and letGo clears it with the process. A
// take-back the store refuses leaves the mark on the task it left confirmed, which no later
// let-go of this process reads while the record of this launch stands (EndedWork.Generation);
// a verdict that finds the process running, after which a later end records afresh
// (EndedWork.Outlived), clears the mark with the judgement it ends (reportedProcess).
// death, fromOutside, displaced and died are recordEnd's.
func (m *Machine) recordGoneEnd(ctx context.Context, cause string, death runtime.DeathCauseKind, fromOutside, displaced, died bool) {
	if p := m.claim.Pending; p != nil && !p.ConfirmedAt.IsZero() {
		m.cutTurnOf = p.ID
	}
	m.recordEnd(ctx, cause, death, fromOutside, displaced, died)
}

// endState is the claim's state as a let-go reads it for the turn it cuts off (recordEnd): the
// claim's own, except for a claim stored working for nothing that runs, which reads as the state
// it is given back in. That is the state a held suspension running at the end of the turn or the
// compaction the claim was working for finds it in (Machine.heldStopBack); and, for a claim working
// for a compaction with no turn started under it, the compaction in flight or the one a gone
// verdict dropped with its process (Machine.compacting, Machine.lostCompaction), the state that
// compaction took the claim from, or idle once a turn ended under it (compacting.back). A turn
// Oh My Pi started under the compaction (compacting.inTurn) is a turn, and reads working. A
// claim no longer working — the turn of a dead connection that ended after its compaction was
// dropped (compactionLostProcess) — reads as it stands.
func (m *Machine) endState() ClaimState {
	if m.claim.State != StateWorking {
		return m.claim.State
	}
	if m.heldStopBack != "" {
		return m.heldStopBack
	}
	run := m.compacting
	if run == nil {
		run = m.lostCompaction
	}
	if run != nil && !run.inTurn {
		return run.back
	}
	return StateWorking
}

// launchActivity is the activity report the store holds for the launch the claim records now
// (Deps.ClaimActivity), and whether it holds one. Only a report of this launch counts: the store
// writes a report onto the launch it was authenticated for alone, and clears the report on the
// write that moves the generation, so a report of an earlier launch is never this one's. Whether
// the report it reads speaks of the turn the process last ran is recordEnd's to decide: one read
// before that turn began is no report of that turn (claim.ReadSessionWork), though the background
// work it says ran is kept.
//
// The read is bounded as the writes of a decision are (bounded), so a store that has stopped
// answering costs the record and never holds the let-go: a read that fails, and a daemon that
// wires no read, are a launch with no report, which the record says rather than reading it as
// nothing running.
func (m *Machine) launchActivity(ctx context.Context) (ActivityReport, bool) {
	if m.deps.ClaimActivity == nil {
		return ActivityReport{}, false
	}
	reading, cancel := m.bounded(ctx)
	defer cancel()
	report, held, err := m.deps.ClaimActivity(reading, m.claim.Token, m.claim.Generation)
	if err != nil {
		m.log.Warn("supervise: the activity report of the process being let go could not be read; its end records no report",
			"generation", m.claim.Generation, "error", err)
		return ActivityReport{}, false
	}
	return report, held
}

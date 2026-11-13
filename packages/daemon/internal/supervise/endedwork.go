package supervise

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"time"
)

// EndedWork is the claim's record of what the last let-go of its process ended of the work its
// agent had running (dispatch://INFRA-3276 R3): when, why in the daemon's own words, whether a
// turn was cut off and whether that turn was the pending task's, what background work the
// process's own activity report named, whether the daemon held a report of that process that
// speaks of its last turn at all, and whether the record has been told. The next process is told
// it in a prompt: the sentence RelaunchSentence composes from it, in front of the task the claim
// holds or as a delivery of the daemon's own when it holds none (Delivery.message,
// queueRelaunch), and marked told at the confirmation of the prompt that carried it (confirm).
// This is the record that sentence is built from, written where the machine lets the process go
// (letGo, recordEnd) and persisted on the claim's row (store's ended_work), since that prompt can
// be a relaunch, a wake and a daemon restart away.
//
// The zero value is a claim whose let-go ended nothing worth telling, or that has been let go of
// none: Cause is empty.
type EndedWork struct {
	// At is when the process was let go, and Cause why: a sentence the daemon writes, beginning
	// with what the decision that let the process go found (the cause constants and functions
	// below), so the sentence it is told in can name it after "because".
	At    time.Time `json:"at"`
	Cause string    `json:"cause"`
	// Generation is the launch whose process this records the end of, which is what keeps one
	// process's end to one record: a decision that records the end of a process it already knows
	// is gone (died, displaced) and the let-go that follows it in the same decision or at a wake's
	// turn are two calls about one launch, and the second finds this launch's record standing
	// (recordEnd).
	Generation uint64 `json:"generation"`
	// Turn is whether a turn was running when the process was let go: the claim was working,
	// which is a turn the stream showed begin, one an earlier daemon's process was in, or one the
	// agent's busy answer said it was in (recordBusyTurn). TaskTurn is whether the pending task
	// was in a turn of that process: confirmed by one, or taken back from one by the decision that
	// let the process go (interrupted, Machine.cutTurnOf).
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
	// that turn has said nothing about it — the idle rule's own rule (pods.IdleClaim.Reported).
	// Such a report still gives the record its Background and Kinds, so a record that is not
	// Reported can name background work. A record that is not Reported is one whose process ran a
	// turn and left no report of it — a Claude Code worker reports only at its Stop hook, so a turn
	// cut off before its end has none and the newest report is the turn before's, and a stale
	// plugin's report is refused — so what background work that turn started is unknown, as is all
	// of the process's where it left no report at all, which is said rather than read as nothing
	// (dispatch://INFRA-3276 R3, "unknown is not silence").
	Reported bool `json:"reported"`
	// Told is whether the record has been told to a later process, and ToldBy the delivery whose
	// confirmation told it. Both follow the one rule confirm states for the two records a prompt
	// tells: told at the confirmation of a prompt that carried the sentence, never at its
	// acknowledgement, and back to untold when that confirmation is taken back as another turn's
	// (moveTelling). The one record told anywhere else is an old session's, which the abandonment of
	// that session ends by no delivery (forgetSession), since the fresh session is told everything of
	// the old one is gone. A record not yet told is replaced by a newer one only as the union of the
	// two (joined), so what the earlier let-go ended is not lost before anyone reads it.
	Told   bool   `json:"told,omitempty"`
	ToldBy string `json:"toldBy,omitempty"`
}

// Tells is whether the next prompt to this claim's agent must still carry the record: a let-go
// recorded something and no turn has confirmed a prompt that carried it.
func (e EndedWork) Tells() bool { return e.Cause != "" && !e.Told }

// joined is newer recorded over e, an earlier record nobody has been told yet: the newer moment,
// cause and launch, and the union of what each ended — a turn either cut off, the task's turn
// either cut off, every background kind either named, and no report where either process left
// none. What the earlier process ended is still true and still untold, so the newer record keeps
// it rather than dropping it.
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
// recovery already let go — and a retirement, which ends the claim (retire).
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
	endRepointed   = "its address changed"
)

// endDied is a death's cause, with the runtime's own account of it (Claim.LastDeathCause), quoted.
func endDied(cause string) string { return "the process died: " + strconv.Quote(cause) }

// endStalled is the stall backstop's cause, with the bound the turn passed.
func endStalled(bound time.Duration) string {
	return fmt.Sprintf("its turn showed no progress for %s", bound)
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
// process is cleared: letGo calls it first, and so do the two decisions that find the process
// already gone (died, displaced), before they take its task back, since what they find is the
// whole of what that process's end did and the let-go they lead to can be a wake's turn away.
//
// It records nothing for a claim that records no process, for noEnd, and for a launch whose end
// is recorded already (EndedWork.Generation). Otherwise it reads what it decides on: whether a
// turn was running (the claim is working), whether the pending task was in a turn of this
// process (confirmed, or taken back from one by this let-go's own decision: cutTurnOf), and the
// launch's own activity report from the store (launchActivity), read here because the machine's
// claim holds only what it read at its own boot and because the launch that follows clears it
// (store's putClaim). Of a process that ran a turn, a report read before that turn began
// (Claim.LastTurnStart) is no report of that turn, as the idle rule holds
// (pods.IdleClaim.Reported): the record is not Reported, rather than reading the turn before's
// "nothing runs" as this one's. It still takes that report's background work, which the report saw
// running in this process, so the sentence of a turn cut off names that work and then says that
// the daemon has no report of what the turn started (relaunchSentence). Nothing is written when no
// turn was running, no delivery was in a turn and a report of the last turn says nothing runs, or
// when no turn was running and none had run in this process, which can have started no background
// work; a process that ran a turn and left no report of it is recorded as unreported rather than as
// having ended nothing. A record still untold is replaced as the union of the two (joined).
//
// A record written here is new words, so a prompt that carried the sentence of the record it
// replaces carried none of this one: the mark of such a prompt goes (Machine.carried), and only a
// prompt sent from here on tells this record when its turn begins (confirm).
//
// It writes nothing itself: the record is a field of the claim, and the let-go's own write — the
// launch's, the suspension's, the failure's, or the death's charge — carries it. So it costs the
// relaunch one read and waits for nothing else.
func (m *Machine) recordEnd(ctx context.Context, cause string) {
	if cause == noEnd || m.claim.Locator == nil {
		return
	}
	if held := m.claim.EndedWork; held.Cause != "" && held.Generation == m.claim.Generation {
		return
	}
	ended := EndedWork{Cause: cause, Generation: m.claim.Generation, Turn: m.claim.State == StateWorking}
	if p := m.claim.Pending; p != nil {
		ended.TaskTurn = !p.ConfirmedAt.IsZero() || (m.cutTurnOf != "" && p.ID == m.cutTurnOf)
	}
	// A claim idle or working has run a turn in this process: idle is reached only by a turn's
	// end, and a relaunched process comes up through ready.
	ranTurn := m.claim.State == StateWorking || m.claim.State == StateIdle
	report, reported := m.launchActivity(ctx)
	if reported && report.Background {
		ended.Background, ended.Kinds = true, slices.Clone(report.Kinds)
	}
	// A report read before the claim's last turn began has said nothing about that turn
	// (pods.IdleClaim.Reported), so it is no report of it, though the background work it saw is
	// kept above. LastTurnStart is when this daemon, or the one before it, first learned that turn
	// had begun: its agent_start (beginTurn), or the busy answer that recorded a turn whose start
	// it never read (recordBusyTurn).
	ended.Reported = reported && !(ranTurn && report.At.Before(m.claim.LastTurnStart))
	if !ended.Turn && !ended.TaskTurn && !ended.Background && (ended.Reported || !ranTurn) {
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

// launchActivity is the activity report the store holds for the launch the claim records now
// (Deps.ClaimActivity), and whether it holds one. Only a report of this launch counts: the store
// writes a report onto the launch it was authenticated for alone, and clears the report on the
// write that moves the generation, so a report of an earlier launch is never this one's. Whether
// the report it reads speaks of the turn the process last ran is recordEnd's to decide: one read
// before that turn began is no report of that turn (pods.IdleClaim.Reported), though the background
// work it says ran is kept.
//
// The read is bounded by the claim-write deadline, as the writes of a decision are, so a store that
// has stopped answering costs the record and never holds the let-go: a read that fails, and a daemon
// that wires no read, are a launch with no report, which the record says rather than reading it as
// nothing running.
func (m *Machine) launchActivity(ctx context.Context) (ActivityReport, bool) {
	if m.deps.ClaimActivity == nil {
		return ActivityReport{}, false
	}
	reading, cancel := context.WithTimeout(ctx, m.deps.claimWrite())
	defer cancel()
	report, held, err := m.deps.ClaimActivity(reading, m.claim.Token, m.claim.Generation)
	if err != nil {
		m.log.Warn("supervise: the activity report of the process being let go could not be read; its end records no report",
			"generation", m.claim.Generation, "error", err)
		return ActivityReport{}, false
	}
	return report, held
}

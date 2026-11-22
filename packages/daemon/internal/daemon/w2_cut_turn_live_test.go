//go:build e2e

// Check w2 of this change's verification (R2: a resumed session is told its turn was cut), run on
// the rig (RIG.md) as the check states it. scripts/e2e/cut-turn-note.sh builds this driver and runs
// it; with --base-commit it runs step 1 with file sessions against the base's code and plugins, the
// check's negative control.
//
// The whole run is made twice, each time on a rig of its own: with file sessions, and with SQL
// sessions (rig_sql_sessions_live_test.go), where every worker keeps its session as rows of a
// database of the run's own and every resume reads it from its row. Each time two implementer
// claims, each a real pane whose turns go to the model the run names, answer their start's task
// and stand idle, and four steps run:
//
//   - step 1: a notice published to the first claim's role starts a turn, which runs a tool call
//     (`sleep`); the claim's Oh My Pi is killed in that call; the daemon resumes the session, its
//     next turn runs, then a message the rig publishes starts a turn, and the transcript is read;
//   - step 2: a task the operator's route gives the second claim starts a turn running a shorter
//     `sleep`; a notice published while that call runs joins the turn (the host writes its card
//     once the call returns) and has the turn run the long `sleep`, in which the claim's Oh My Pi
//     is killed; the transcript the daemon resumes is read once the daemon has sent the task again;
//   - step 3: R2's "whatever starts that turn" for a card: the relay holds the daemon's prompts to
//     a third claim (rigRelay.holdPrompts), a notice's turn is cut as in step 1, and while the
//     resumed process's prompt from the daemon is held a message the rig publishes starts its
//     first turn; the prompt is let go once that turn has replied;
//   - step 4: the daemon restarts as `controller: daemon`, so it runs the project's controller as
//     a claim of its own; once its start turn has ended a tick wake is queued for it (the daemon's
//     own controller notice, through its outbox), the controller's Oh My Pi is killed in the turn
//     the wake starts, and its resumed transcript is read after its start message;
//   - step 5: an Oh My Pi session that holds no Legion claim (rigRuntime.startSession) is killed in
//     a tool call and resumed.
//
// A last step compares the notes the two stores gave.
//
// Every kill is SIGKILL of the pane's process group and of every process under it, so Oh My Pi
// ends with no word of its own and its tool's process with it, as a pod's end takes them.
//
// What a step reads is the session's own transcript, from the file the claim recorded or, with SQL
// sessions, from its row, every entry the killed process left and every entry the resumed one
// wrote after it, beside the operator's claim listing and the runtime's launches. Every plugin or
// daemon text a step matches is written here as a literal (the plugin's cut-turn.ts, the daemon's
// relaunch sentence), so the driver builds against the base, whose code names none of the head's
// symbols. Each observation is printed under its step and appended to w2-results.jsonl in the
// run's evidence directory as it is made, and each transcript a step read is kept there.
package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/api"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/shimwire"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The steps, by the names the control reads back.
const (
	w2NoticeStep     = "step1-a-turn-a-notice-started-is-told-cut-once-before-the-next-turn"
	w2TaskStep       = "step2-a-task-turn-a-notice-joined-holds-both-notes"
	w2CardStep       = "step3-a-card-that-starts-the-resumed-first-turn-comes-after-the-note"
	w2ControllerStep = "step4-the-controllers-tick-turn-is-told-cut-before-its-start-turn"
	w2NoClaimStep    = "step5-a-session-that-holds-no-claim-gets-no-note"
	w2StoresStep     = "both-stores-give-the-same-notes"
)

// The plugin's words a step reads, as packages/pi-envoy/src/cut-turn.ts writes them at this head
// (CUT_TURN_NOTICE, TURN_MARK_ENTRY, cutTurnNoticeText's sentences), and the daemon's own note in
// front of a task its process died in (supervise's relaunchSentence, which busy_relaunch_live_test.go
// reads as relaunchCutTaskWords).
const (
	w2NoteNotice   = "previous turn interrupted"
	w2NoteCut      = "Your previous turn was interrupted."
	w2NoteReadIt   = "Before you start any of that work again, check what it already did."
	w2NoteJoined   = "Those messages started or joined that turn."
	w2NoteNotAgain = "No message is sent to you again."
	w2TurnMark     = "envoy-turn-mark"
	w2DaemonCut    = "Your previous turn on this task was interrupted."
)

// The bounds of a step's waits. The panes' turns go to the shared gateway on a loaded host, so
// each is minutes: a wait that runs out fails the step naming what it waited for.
const (
	// w2SleepSeconds is the tool call each cut turn runs: long enough that the kill lands in it
	// and short enough that a resumed agent that runs it again is done within the step.
	w2SleepSeconds = 120
	// w2TaskSleepSeconds is the first call of step 2's task, which the notice joins. Oh My Pi writes
	// a card handed to it while a foreground call runs once that call returns, and moves a bash call
	// still running after bash.autoBackground.thresholdMs (60 s by default) to a background job,
	// which answers it: this is short of that, and long enough that the notice reaches the session
	// while it runs.
	w2TaskSleepSeconds = 45
	w2ToolCallWithin   = 6 * time.Minute
	w2CardWithin       = 2 * time.Minute
	w2RelaunchWithin   = 8 * time.Minute
	w2QuietWithin      = 10 * time.Minute
	w2ReplyWithin      = 8 * time.Minute
	// w2StartWithin bounds the controller's start procedure, which reads its skill, the daemon's
	// state and Dispatch before it ends its turn.
	w2StartWithin = 25 * time.Minute
	w2TickWithin  = 3 * time.Minute
	// w2Quiet is how long a claim must hold no task and run no turn to be read quiet: a resumed
	// claim is given the daemon's delivery at its ready, and a controller is woken by cards.
	w2Quiet = 20 * time.Second
)

// cutTurnLive is one store's run of w2.
type cutTurnLive struct {
	*rig
	// store is "file" or "sql"; sql is the SQL sessions' seam, nil with file sessions.
	store string
	sql   *rigSQLSessions
	// notice, task and card are the three issues, one implementer each, of steps 1 to 3.
	notice, task, card string
	// running is the step running now, which every record names.
	running string
	// notes is what each step's note read, with what differs between runs (the event ids, the
	// project) named rather than spelled, for the last step's comparison.
	notes   map[string]string
	results *os.File
}

// TestAResumedSessionIsToldItsTurnWasCutLive runs w2's steps under each store, then compares them.
func TestAResumedSessionIsToldItsTurnWasCutLive(t *testing.T) {
	notes := map[string]map[string]string{}
	for _, store := range []string{"file", "sql"} {
		t.Run(store+"-sessions", func(t *testing.T) {
			r := newCutTurnLive(t, store)
			notes[store] = r.notes
			r.step(w2NoticeStep, r.noticeTurnCut)
			r.step(w2TaskStep, r.taskTurnCut)
			r.step(w2CardStep, r.cardStartsTheFirstTurn)
			r.step(w2ControllerStep, r.controllerTickCut)
			r.step(w2NoClaimStep, r.sessionWithoutClaim)
		})
	}
	t.Run(w2StoresStep, func(t *testing.T) { compareCutTurnNotes(t, notes) })
}

func newCutTurnLive(t *testing.T, store string) *cutTurnLive {
	t.Helper()
	r := &cutTurnLive{rig: startRig(t, rigOptions{script: "scripts/e2e/cut-turn-note.sh"}), store: store, notes: map[string]string{}}
	r.running = "setup"
	path := filepath.Join(r.evidence, "w2-results.jsonl")
	results, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open the results file %s: %v", path, err)
	}
	t.Cleanup(func() { results.Close() })
	r.results = results
	// The listener's own lines (each publish's `listener publish` record, with its outcome) are kept
	// beside the results, since the rig holds them only in memory.
	listenerLog := filepath.Join(r.evidence, store+"-listener.log")
	t.Cleanup(func() { _ = os.WriteFile(listenerLog, []byte(r.listener.Output()), 0o600) })
	r.note("%s sessions; every observation is appended to %s as it is made, and the listener's log is kept at %s", store, path, listenerLog)
	if store == "sql" {
		r.sql = startRigSQLSessions(t, r.rig)
	}
	r.notice, r.task, r.card = r.cfg.Project+"-1", r.cfg.Project+"-2", r.cfg.Project+"-3"
	var seeds []rigSeed
	for _, key := range []string{r.notice, r.task, r.card} {
		seeds = append(seeds, rigSeed{issue: key, phase: phase.Implementing, role: claim.RoleImplementer,
			task: "Reply with the one word READY and nothing else. Call no tool."})
	}
	r.seed(seeds...)
	r.boot()
	for _, key := range []string{r.notice, r.task, r.card} {
		token := r.token(key, claim.RoleImplementer)
		r.awaitReady(token)
		r.awaitQuiet(token, w2QuietWithin)
		c := r.claim(token)
		r.record("session", fmt.Sprintf("%s is idle at generation %d in session %s, recorded at %s", token, c.Generation, c.Session, c.SessionFile))
		r.storedAsTheStoreSays(c.SessionFile)
	}
	return r
}

// ---- the steps ----------------------------------------------------------------------------------

// noticeTurnCut is the check's first step. Its negative control is this step over the base's
// plugins, where the resumed transcript holds no note (cut-turn-note).
func (r *cutTurnLive) noticeTurnCut() {
	r.running = w2NoticeStep
	token := r.token(r.notice, claim.RoleImplementer)
	before := r.claim(token)
	path := before.SessionFile
	marker, follow := "W2NOTICE"+randomSuffix(r.t), "W2RESUMED"+randomSuffix(r.t)
	from := len(r.transcript(path))
	notice := r.publishToRole(token, fmt.Sprintf("Run this one shell command with your bash tool, in the foreground, and wait for it "+
		"to finish before you do anything else: `sleep %d`. Then reply with the one word %s.", w2SleepSeconds, marker))
	card := r.awaitCard(path, notice, from)
	r.record("notice", fmt.Sprintf("published %q to %s at %s (%s, %d sends); its card is entry %d of the transcript, event %s",
		notice.summary, notice.topic, clockAt(notice.at), notice.dedupeKey, notice.sends, card, notice.eventID))
	call := r.awaitToolCall(path, card, w2SleepSeconds)
	cut := r.kill(token, path, call)
	r.holdsTheCutTurn(cut, notice.eventID, card, call)

	resumed, launch := r.awaitResumed(token, before, cut.at)
	r.awaitQuiet(token, w2QuietWithin)
	r.record("first-turn", fmt.Sprintf("the resumed process's first turn has ended (claim %s, last turn %s to %s)",
		r.claim(token).State, w2ClockAt(r.claim(token).LastTurnStart), w2ClockAt(r.claim(token).LastTurnEnd)))
	message := r.publishToRole(token, "Reply with the one word "+follow+" and nothing else. Call no tool.")
	messageCard := r.awaitCard(path, message, cut.length)
	r.record("message", fmt.Sprintf("the rig published %q to %s at %s (%s); its card is entry %d, event %s",
		message.summary, message.topic, clockAt(message.at), message.dedupeKey, messageCard, message.eventID))
	r.awaitReply(path, follow, messageCard)

	entries := r.transcript(path)
	r.keepTranscript(path)
	r.describeResumed(entries, cut.length)
	note := r.oneNote(entries, cut.length)
	r.notes[w2NoticeStep] = r.normalize(note.text(), map[string]string{notice.eventID: "<the notice>"})
	r.noteSays(note, notice.eventID)
	r.shownOnce(entries, notice, cut.length)
	r.resumedFromItsStore(path, launch, resumed)
	r.noteBeforeTheNextTurn(entries, cut.length, note)
}

// taskTurnCut is the check's second step: a turn a task started, which a notice joined. The host
// writes the card of a notice that reaches it while a foreground call runs once that call returns
// (2026-11-16's run: the card handed over at 10:34:24.050Z was written after the call's result at
// 10:35:23.873Z), so the task's first call is short, the notice joins the turn at its return, and
// the notice has the turn run the long call the kill lands in.
func (r *cutTurnLive) taskTurnCut() {
	r.running = w2TaskStep
	token := r.token(r.task, claim.RoleImplementer)
	before := r.claim(token)
	path := before.SessionFile
	marker, joined := "W2TASK"+randomSuffix(r.t), "W2JOINED"+randomSuffix(r.t)
	task := fmt.Sprintf("Run this one shell command with your bash tool, in the foreground, and wait for it to finish before you "+
		"do anything else: `sleep %d`. Then reply with the one word %s.", w2TaskSleepSeconds, marker)
	from := len(r.transcript(path))
	r.deliver(token, task)
	r.record("task", fmt.Sprintf("the operator's route gave %s the task %q at %s", token, task, clockAt(time.Now())))
	first := r.awaitToolCall(path, from, w2TaskSleepSeconds)
	if pending := r.claim(token).Pending; pending == nil {
		r.fails("task-turn", "%s holds no task while the turn that runs its call is open; the call is not the task's turn", token)
	}
	notice := r.publishToRole(token, fmt.Sprintf("This notice joins the turn you are in. When your command has finished, run this one "+
		"shell command with your bash tool, in the foreground, and wait for it to finish: `sleep %d`. Then also say the word %s.",
		w2SleepSeconds, joined))
	card := r.awaitCard(path, notice, first)
	r.record("notice", fmt.Sprintf("published %q to %s at %s (%s, %d sends) while the task's call at entry %d ran; its card is entry %d, event %s",
		notice.summary, notice.topic, clockAt(notice.at), notice.dedupeKey, notice.sends, first, card, notice.eventID))
	call := r.awaitToolCall(path, card, w2SleepSeconds)
	cut := r.kill(token, path, call)
	r.holdsTheCutTurn(cut, notice.eventID, card, call)
	r.joinedTheTaskTurn(cut, marker, first, card)

	resumed, launch := r.awaitResumed(token, before, cut.at)
	prompt := r.awaitEntry(path, cut.length, "the task sent again behind the daemon's note", w2ReplyWithin, func(e w2Entry) bool {
		return e.isPrompt() && strings.Contains(e.text(), w2DaemonCut) && strings.Contains(e.text(), marker)
	})
	r.awaitEntry(path, prompt, "the first reply of the task's turn", w2ReplyWithin, func(e w2Entry) bool { return e.isReply() })

	entries := r.transcript(path)
	r.keepTranscript(path)
	r.describeResumed(entries, cut.length)
	daemonNote := entries[prompt].text()
	r.record("daemon-note", fmt.Sprintf("entry %d, the task sent again, carries the daemon's own note:\n%s", prompt, indentOutput(daemonNote)))
	note := r.oneNote(entries, cut.length)
	r.notes[w2TaskStep] = r.normalize(note.text(), map[string]string{notice.eventID: "<the notice>"})
	r.notes[w2TaskStep+"/daemon"] = r.normalize(firstParagraph(daemonNote), nil)
	r.noteSays(note, notice.eventID)
	if !strings.Contains(note.text(), w2NoteJoined) {
		r.fails("note-says", "the note does not say %q", w2NoteJoined)
	}
	r.shownOnce(entries, notice, cut.length)
	r.resumedFromItsStore(path, launch, resumed)
	r.noteBeforeTheNextTurn(entries, cut.length, note)
}

// w2RelaunchOpens opens the daemon's own delivery that tells a claim holding no task what its
// relaunch ended (supervise's RelaunchSentence for a death, which busy_relaunch_live_test.go reads
// as relaunchCatchUpOpens).
const w2RelaunchOpens = "Your previous process ended because"

// cardStartsTheFirstTurn is R2's "whatever starts that turn" for a card. The daemon starts a
// resumed worker's first turn with a prompt of its own whenever its process ended in a turn
// (supervise's recordEnd records the turn, and queueRelaunch queues its delivery at the ready), so
// a card starts that turn only where it reaches the new process first: a daemon that answers late.
// The relay holds the daemon's prompts to the claim's shims (rigRelay.holdPrompts) from before the
// kill, the rig publishes a message to the role once the daemon's prompt is held, and the relay
// lets the prompt go as soon as the card's turn has started, so the prompt reaches the shim inside
// that turn, as a late daemon's does. That is inside the daemon's RPC timeout (WorkerRPCTimeout):
// a prompt held past it is one the daemon gave up on, which is a stalled stream rather than a
// late daemon, and the daemon then never hears Oh My Pi answer it busy (2026-11-17's run held it
// until the card's turn replied, 5.6 s, and the daemon's prompt never came). Oh My Pi answers the
// prompt busy, and the daemon sends its delivery again once the card's turn has ended. The card's
// entry comes first in the resumed transcript, the daemon's prompt after it, and the note before
// the card.
func (r *cutTurnLive) cardStartsTheFirstTurn() {
	r.running = w2CardStep
	token := r.token(r.card, claim.RoleImplementer)
	before := r.claim(token)
	path := before.SessionFile
	marker, first := "W2CARDCUT"+randomSuffix(r.t), "W2FIRST"+randomSuffix(r.t)
	from := len(r.transcript(path))
	notice := r.publishToRole(token, fmt.Sprintf("Run this one shell command with your bash tool, in the foreground, and wait for it "+
		"to finish before you do anything else: `sleep %d`. Then reply with the one word %s.", w2SleepSeconds, marker))
	card := r.awaitCard(path, notice, from)
	r.record("notice", fmt.Sprintf("published %q to %s at %s (%s, %d sends); its card is entry %d, event %s",
		notice.summary, notice.topic, clockAt(notice.at), notice.dedupeKey, notice.sends, card, notice.eventID))
	call := r.awaitToolCall(path, card, w2SleepSeconds)
	r.relay.holdPrompts(token)
	defer r.relay.releasePrompts(token)
	r.record("hold", fmt.Sprintf("from %s the relay holds every prompt the daemon sends %s's shims, and every line behind it", clockAt(time.Now()), token))
	cut := r.kill(token, path, call)
	r.holdsTheCutTurn(cut, notice.eventID, card, call)

	resumed, launch := r.awaitResumed(token, before, cut.at)
	var held relayFrame
	r.await("the daemon's prompt to the resumed process, held at the relay", w2RelaunchWithin, func() bool {
		for _, c := range r.relay.connsOf(launch.spec.BootToken) {
			if f, ok := c.firstFrame(shimwire.TypePrompt); ok {
				held = f
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
		return false
	})
	r.record("held-prompt", fmt.Sprintf("the daemon sent generation %d its prompt (request %s) at %s, and the relay holds it", launch.spec.Generation, held.id, clockAt(held.at)))
	released := r.releaseAtTheCardsTurn(token, path, cut.length, first)
	message := r.publishToRole(token, "Reply with the one word "+first+" and nothing else. Call no tool.")
	messageCard := r.awaitCard(path, message, cut.length)
	r.record("message", fmt.Sprintf("the rig published %q to %s at %s (%s, %d sends) while the daemon's prompt was held; its card is entry %d, event %s",
		message.summary, message.topic, clockAt(message.at), message.dedupeKey, message.sends, messageCard, message.eventID))
	var let w2Release
	select {
	case let = <-released:
	case <-time.After(w2CardWithin):
		r.fails("release", "the turn of the card at entry %d did not start within %s, and the relay still holds the daemon's prompt", messageCard, w2CardWithin)
	}
	if let.err != nil {
		r.t.Fatalf("read the transcript for the card's turn: %v", let.err)
	}
	heldFor := let.at.Sub(held.at).Round(time.Millisecond)
	r.record("release", fmt.Sprintf("%s; the relay let the daemon's prompt go at %s, %s after the daemon sent it (its RPC timeout is %s)",
		let.why, clockAt(let.at), heldFor, r.cfg.WorkerRPCTimeout))
	if heldFor >= r.cfg.WorkerRPCTimeout {
		r.fails("hold", "the relay held the daemon's prompt %s, not less than the daemon's RPC timeout of %s: the daemon gave up on that request, which is a stalled stream rather than a daemon that answers late",
			heldFor, r.cfg.WorkerRPCTimeout)
	}
	r.awaitReply(path, first, messageCard)
	prompt := r.awaitEntry(path, messageCard, "the daemon's released prompt in the transcript", w2ReplyWithin, func(e w2Entry) bool {
		return e.isPrompt() && strings.Contains(e.text(), w2RelaunchOpens)
	})

	entries := r.transcript(path)
	r.keepTranscript(path)
	r.describeResumed(entries, cut.length)
	note := r.oneNote(entries, cut.length)
	r.notes[w2CardStep] = r.normalize(note.text(), map[string]string{notice.eventID: "<the notice>"})
	r.noteSays(note, notice.eventID)
	starter := -1
	for _, e := range entries[cut.length:] {
		if e.isPrompt() || (e.Type == "custom_message" && e.CustomType == "envoy-message" && !e.isNote()) {
			starter = e.index
			break
		}
	}
	if starter != messageCard || prompt < messageCard {
		r.fails("card-starts-the-first-turn", "the resumed process's first turn opens at entry %d, and the rig's card is entry %d and the daemon's prompt entry %d: the card did not start that turn",
			starter, messageCard, prompt)
	}
	r.record("card-first", fmt.Sprintf("the rig's card, entry %d, is the first entry that opens a turn in the resumed transcript; the daemon's released prompt is entry %d, after it:\n%s",
		messageCard, prompt, indentOutput(entries[prompt].text())))
	r.shownOnce(entries, notice, cut.length)
	r.resumedFromItsStore(path, launch, resumed)
	r.noteBeforeTheNextTurn(entries, cut.length, note)
}

// controllerTickCut is the check's fourth step: the project's controller run as a daemon claim, a
// tick wake, and the controller's Oh My Pi killed in the turn the wake starts.
func (r *cutTurnLive) controllerTickCut() {
	r.running = w2ControllerStep
	r.cfg.ControllerLaunch = config.ControllerLaunchDaemon
	stopped, serving := r.restartDaemon()
	r.record("controller", fmt.Sprintf("the daemon stopped at %s and served again at %s with controller: daemon", clockAt(stopped), clockAt(serving)))
	token := claim.ControllerToken(r.project)
	r.await(string(token)+", the daemon's own controller, to register and report ready", w2RelaunchWithin, func() bool {
		c, ok := r.findClaim(token)
		if ok && c.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed; its output is under %s", token, r.rt.paneLogs)
		}
		return ok && (c.State == string(supervise.StateReady) || c.State == string(supervise.StateIdle) || c.State == string(supervise.StateWorking))
	})
	ready := time.Now()
	r.awaitQuietWithin(token, w2StartWithin, 2*w2Quiet)
	before := r.claim(token)
	path := before.SessionFile
	r.record("controller", fmt.Sprintf("%s was ready at %s in session %s (%s) and ended its start turn by %s",
		token, clockAt(ready), before.Session, path, clockAt(time.Now())))
	r.storedAsTheStoreSays(path)

	from := len(r.transcript(path))
	queued := time.Now()
	if err := r.st.Tx(context.Background(), func(tx pgx.Tx) error {
		row, err := record.NewOutboxRow(r.cfg.Project, record.ControllerNotice{Kind: record.TickNotice}, queued)
		if err != nil {
			return err
		}
		return r.records.Enqueue(context.Background(), tx, row)
	}); err != nil {
		r.t.Fatalf("queue the tick wake: %v", err)
	}
	tick := r.awaitStoredTick(queued)
	card := r.awaitCard(path, tick, from)
	r.record("tick", fmt.Sprintf("queued a tick wake at %s; the daemon's outbox published it on %s, stored at %s (event %s, %s, %q); its card is entry %d",
		clockAt(queued), tick.topic, clockAt(tick.at), tick.eventID, tick.dedupeKey, tick.summary, card))
	turn := r.awaitTurnOn(token, path, card)
	cut := r.kill(token, path, turn)
	r.holdsTheWakeTurn(cut, tick.eventID, card)

	resumed, launch := r.awaitResumed(token, before, cut.at)
	start := r.awaitEntry(path, cut.length, "the keeper's start message", w2ReplyWithin, func(e w2Entry) bool {
		return e.isPrompt() && strings.Contains(e.text(), ControllerStartMessage)
	})
	r.awaitEntry(path, start, "the first reply of the start turn", w2ReplyWithin, func(e w2Entry) bool { return e.isReply() })

	entries := r.transcript(path)
	r.keepTranscript(path)
	r.describeResumed(entries, cut.length)
	r.record("start-message", fmt.Sprintf("entry %d is the start message the keeper sent at the resumed process's ready:\n%s", start, indentOutput(entries[start].text())))
	note := r.oneNote(entries, cut.length)
	r.notes[w2ControllerStep] = r.normalize(note.text(), map[string]string{tick.eventID: "<the tick wake>"})
	r.noteSays(note, tick.eventID)
	r.shownOnce(entries, tick, cut.length)
	r.resumedFromItsStore(path, launch, resumed)
	r.noteBeforeTheNextTurn(entries, cut.length, note)
}

// sessionWithoutClaim is the check's last step: a session no Legion claim holds, killed mid-turn
// and resumed, gets no note.
func (r *cutTurnLive) sessionWithoutClaim() {
	r.running = w2NoClaimStep
	var env []string
	if r.sql != nil {
		env = r.sql.env()
	}
	marker, back := "W2BARE"+randomSuffix(r.t), "W2BACK"+randomSuffix(r.t)
	first := r.runtime.startSession(r.store+"-first", r.cfg.EnvoyTokenFile, env)
	r.negotiate(first)
	r.prompt(first, "p1", fmt.Sprintf("Run this one shell command with your bash tool, in the foreground, and wait for it to finish "+
		"before you do anything else: `sleep %d`. Then reply with the one word %s.", w2SleepSeconds, marker))
	path := r.sessionFileOf(first)
	r.record("session", fmt.Sprintf("the session %s (pid %d), holding no Legion claim, keeps its session at %s; its environment names no LEGION_ variable",
		first.name, first.pid, path))
	call := r.awaitToolCall(path, 0, w2SleepSeconds)
	cut := r.killSession(first, path, call)
	r.holdsTheCutTurn(cut, "", -1, call)

	second := r.runtime.startSession(r.store+"-resumed", r.cfg.EnvoyTokenFile, env, "--resume="+path)
	r.negotiate(second)
	r.prompt(second, "p2", "Reply with the one word "+back+" and nothing else. Call no tool.")
	r.record("resume", fmt.Sprintf("resumed it as %s (pid %d) with --resume=%s and sent it a prompt", second.name, second.pid, path))
	r.awaitReply(path, back, cut.length)

	entries := r.transcript(path)
	r.keepTranscript(path)
	r.describeResumed(entries, cut.length)
	if notes := cutNotes(entries); len(notes) != 0 {
		r.fails("no-note", "the session that holds no Legion claim was told its turn was cut, at entry %d:\n%s", notes[0].index, indentOutput(notes[0].text()))
	}
	marks := 0
	for _, e := range entries {
		if _, ok := e.mark(); ok {
			marks++
		}
	}
	r.record("no-note", fmt.Sprintf("its transcript of %d entries holds no cut-turn note and %d turn marks", len(entries), marks))
	r.storedAsTheStoreSays(path)
	r.notes[w2NoClaimStep] = "no note"
}

// compareCutTurnNotes is the last step: each note the file sessions gave against the one the SQL
// sessions gave for the same step.
func compareCutTurnNotes(t *testing.T, notes map[string]map[string]string) {
	started := time.Now()
	fmt.Printf("== %s\n", w2StoresStep)
	files, sqls := notes["file"], notes["sql"]
	if files == nil || sqls == nil {
		fmt.Printf("FAIL %s at both-stores (%ds)\n", w2StoresStep, int(time.Since(started).Seconds()))
		t.Fatalf("both-stores: a store's run made no notes: file %v, sql %v", files != nil, sqls != nil)
	}
	failed := false
	for _, step := range []string{w2NoticeStep, w2TaskStep, w2TaskStep + "/daemon", w2CardStep, w2ControllerStep, w2NoClaimStep} {
		file, sql := files[step], sqls[step]
		switch {
		case file == "" || sql == "":
			failed = true
			t.Errorf("same-notes: %s gave no note to compare under file sessions (%t) or SQL sessions (%t)", step, file != "", sql != "")
		case file != sql:
			failed = true
			t.Errorf("same-notes: %s's note differs between the stores:\nfile:\n%s\nsql:\n%s", step, indentOutput(file), indentOutput(sql))
		default:
			fmt.Printf("   %s: both stores gave\n%s\n", step, indentOutput(file))
		}
	}
	if failed {
		fmt.Printf("FAIL %s at same-notes (%ds)\n", w2StoresStep, int(time.Since(started).Seconds()))
		return
	}
	fmt.Printf("ok %s (%ds)\n", w2StoresStep, int(time.Since(started).Seconds()))
}

// ---- what a step drives --------------------------------------------------------------------------

// w2Published is one message published through the listener: the event id its card shows (the
// first the listener forwarded it as, until the card is read), every event id a send of it was
// forwarded as, its dedupe key, its topic, its summary line, when the listener answered, whether
// the listener said the holder confirmed it, and whether it said it stored the forward for the
// holder.
type w2Published struct {
	eventID, dedupeKey, topic, summary string
	eventIDs                           []string
	at                                 time.Time
	confirmed, stored                  bool
	// token and message are what publishToRole sent, so a message the listener forwarded to no
	// holder can be sent again under its own dedupe key (awaitCard); empty for a message the daemon
	// published.
	token   claim.Token
	message string
	sends   int
}

// publishToRole publishes message to token's role topic through the listener's publish route, as
// the daemon publishes a notice (notify.HTTPPublisher: keep false, the listener's bearer, a dedupe
// key of its own). The listener answers 200 once the role's holder confirmed the message, 504
// `receipt_timeout` when it forwarded the message, the stream stored the forward for the holder,
// and no receipt came within its window, or 404 when it found no live holder and forwarded
// nothing. The listener production pins asks for a receipt that this head's sessions no longer
// send (they read every frame from their stream consumer), so a forward to one of them is answered
// 504 `receipt_timeout`, and the session reads the stored forward all the same.
func (r *cutTurnLive) publishToRole(token claim.Token, message string) *w2Published {
	r.t.Helper()
	p := &w2Published{dedupeKey: "w2-" + randomSuffix(r.t), topic: notify.RoleTopicPrefix + string(token), token: token, message: message}
	r.send(p)
	return p
}

func (r *cutTurnLive) send(p *w2Published) {
	r.t.Helper()
	body, err := json.Marshal(map[string]any{"topic": p.topic, "message": p.message, "dedupe_key": p.dedupeKey, "keep": false})
	if err != nil {
		r.t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, r.front.url+"/v1/messages/publish", bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+r.listener.Token())
	response, err := (&http.Client{Timeout: 2 * time.Minute}).Do(request)
	if err != nil {
		r.t.Fatalf("publish to %s: %v", p.topic, err)
	}
	defer response.Body.Close()
	answer, _ := io.ReadAll(response.Body)
	var envelope struct {
		EventID        string `json:"event_id"`
		PayloadSummary string `json:"payload_summary"`
		Reason         string `json:"reason"`
	}
	_ = json.Unmarshal(answer, &envelope)
	p.sends++
	p.at = time.Now()
	switch {
	case response.StatusCode == http.StatusOK && envelope.EventID != "":
		p.confirmed = true
	case response.StatusCode == http.StatusGatewayTimeout && envelope.Reason == notify.ReasonReceiptTimeout && envelope.EventID != "":
		p.stored = true
		r.record("publish", fmt.Sprintf("send %d of %s: the listener forwarded event %s, the stream stored it for the holder, and no receipt came within the listener's window: %s",
			p.sends, p.dedupeKey, envelope.EventID, strings.TrimSpace(string(answer))))
	case response.StatusCode == http.StatusNotFound:
		r.record("publish", fmt.Sprintf("send %d of %s: the listener found no live holder of %s and forwarded nothing: %s",
			p.sends, p.dedupeKey, p.topic, strings.TrimSpace(string(answer))))
	default:
		r.t.Fatalf("publish to %s = %s %s, want 200, 504 receipt_timeout, or 404", p.topic, response.Status, answer)
	}
	if envelope.EventID != "" {
		p.eventIDs = append(p.eventIDs, envelope.EventID)
		if p.eventID == "" {
			p.eventID = envelope.EventID
		}
	}
	switch {
	case envelope.PayloadSummary != "":
		p.summary = envelope.PayloadSummary
	case p.summary == "":
		p.summary = p.message
	}
}

// awaitStoredTick is the tick wake the daemon's outbox published on the project's controller
// topic after queued, as the stream stored it.
func (r *cutTurnLive) awaitStoredTick(queued time.Time) *w2Published {
	r.t.Helper()
	topic := notify.ControllerTopic(r.project)
	var tick *w2Published
	r.await("the daemon's outbox to publish the tick wake on "+topic, w2TickWithin, func() bool {
		for _, stored := range r.nats.storedOn(topic) {
			if stored.at.Before(queued.Add(-time.Second)) {
				continue
			}
			notice, ok := noticeIn(stored.data)
			if !ok || notice.kind != string(record.TickNotice) {
				continue
			}
			var envelope struct {
				EventID        string `json:"event_id"`
				DedupeKey      string `json:"dedupe_key"`
				PayloadSummary string `json:"payload_summary"`
			}
			if json.Unmarshal(stored.data, &envelope) != nil || envelope.EventID == "" {
				continue
			}
			tick = &w2Published{eventID: envelope.EventID, dedupeKey: envelope.DedupeKey, topic: topic,
				summary: envelope.PayloadSummary, at: stored.at, confirmed: true}
			return true
		}
		time.Sleep(time.Second)
		return false
	})
	return tick
}

// deliver gives the claim one task through the route `legion claims deliver` posts to.
func (r *cutTurnLive) deliver(token claim.Token, task string) {
	r.t.Helper()
	status, body := r.d.request(http.MethodPost, "/legion/v1/operator/claims/"+string(token)+"/deliver", api.DeliverRequest{Task: task}, true)
	if status != http.StatusOK {
		r.t.Fatalf("deliver to %s = %d %s, want 200", token, status, body)
	}
}

// w2Cut is one kill: when, the processes it ended, and the transcript the killed process left.
type w2Cut struct {
	at      time.Time
	ended   []w2Process
	entries []w2Entry
	length  int
}

// kill SIGKILLs token's pane's process group and every process under it, which ends its Oh My
// Pi with the tool call call is, and reads the transcript the killed process left.
func (r *cutTurnLive) kill(token claim.Token, path string, call int) w2Cut {
	r.t.Helper()
	launch := r.runtime.current(token)
	if launch == nil || !launch.running() {
		r.t.Fatalf("%s has no running process to kill", token)
	}
	cut := r.killTree(launch.pane.pid, func() bool { return !launch.running() })
	if !waitGone(launch.pane, localStopGrace) {
		r.fails("kill", "%s's process %d did not end after SIGKILL", token, launch.pane.pid)
	}
	r.readTheCut(&cut, path, fmt.Sprintf("%s's process group %d (generation %d)", token, launch.pane.pid, launch.spec.Generation), call)
	return cut
}

// killSession is kill for a session the rig started by hand.
func (r *cutTurnLive) killSession(s *rigSession, path string, call int) w2Cut {
	r.t.Helper()
	cut := r.killTree(s.pid, func() bool { return !s.running() })
	r.readTheCut(&cut, path, fmt.Sprintf("the session %s's process group %d", s.name, s.pid), call)
	return cut
}

// killTree SIGKILLs the process group root leads and every process under root, and waits for each
// to be gone.
func (r *cutTurnLive) killTree(root int, ended func() bool) w2Cut {
	r.t.Helper()
	tree := w2Descendants(root)
	cut := w2Cut{at: time.Now(), ended: tree}
	_ = syscall.Kill(-root, syscall.SIGKILL)
	for _, p := range tree {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
	deadline := time.Now().Add(localStopGrace)
	for {
		left := 0
		for _, p := range tree {
			if alive(p.pid) && !w2Zombie(p.pid) {
				left++
			}
		}
		if left == 0 && ended() {
			return cut
		}
		if time.Now().After(deadline) {
			r.fails("kill", "%d of the %d processes under %d still run %s after SIGKILL: %v", left, len(tree), root, localStopGrace, tree)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readTheCut reads the transcript the killed process left and records the kill.
func (r *cutTurnLive) readTheCut(cut *w2Cut, path, what string, call int) {
	cut.entries = r.transcript(path)
	cut.length = len(cut.entries)
	var names []string
	for _, p := range cut.ended {
		names = append(names, fmt.Sprintf("%s %d", p.comm, p.pid))
	}
	since := "an unrecorded time"
	if started := r.entryTime(cut.entries, call); !started.IsZero() {
		since = cut.at.Sub(started).Round(time.Millisecond).String()
	}
	r.record("kill", fmt.Sprintf("SIGKILLed %s at %s, %s after the call at entry %d was written: it ended %s; the killed process left %d entries",
		what, clockAt(cut.at), since, call, strings.Join(names, ", "), cut.length))
}

// awaitResumed waits for the daemon to launch the claim's next process, resuming the session the
// claim recorded, and for that process to report ready.
func (r *cutTurnLive) awaitResumed(token claim.Token, was api.OperatorClaim, killed time.Time) (api.OperatorClaim, *rigLaunch) {
	r.t.Helper()
	var now api.OperatorClaim
	r.await(string(token)+" to be resumed and report ready", w2RelaunchWithin, func() bool {
		c, ok := r.findClaim(token)
		if !ok {
			return false
		}
		if c.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed after its process was killed; its output is under %s", token, r.rt.paneLogs)
		}
		now = c
		return c.Generation > was.Generation && (c.State == string(supervise.StateReady) || c.State == string(supervise.StateIdle) || c.State == string(supervise.StateWorking))
	})
	launch := r.runtime.current(token)
	r.record("resumed", fmt.Sprintf("the daemon launched generation %d at %s, %s after the kill, resuming %s; it reported ready by %s, in session %s (was %s)",
		launch.spec.Generation, clockAt(launch.at), launch.at.Sub(killed).Round(time.Millisecond), launch.spec.ResumeSessionFile,
		clockAt(time.Now()), now.Session, was.Session))
	if launch.spec.ResumeSessionFile != was.SessionFile || now.Session != was.Session {
		r.fails("resumed-the-session", "generation %d resumed %q in session %s, want the recorded %q in session %s",
			launch.spec.Generation, launch.spec.ResumeSessionFile, now.Session, was.SessionFile, was.Session)
	}
	return now, launch
}

// negotiate and prompt speak a hand-started session's RPC, as the shim speaks a pane's.
func (r *cutTurnLive) negotiate(s *rigSession) {
	r.t.Helper()
	r.rpc(s, map[string]any{"type": "negotiate_protocol", "id": "n-" + s.name, "protocolVersion": 2})
}

func (r *cutTurnLive) prompt(s *rigSession, id, message string) {
	r.t.Helper()
	r.rpc(s, map[string]any{"type": "prompt", "id": id, "message": message})
}

// rpc sends command and waits for the session's successful answer to it.
func (r *cutTurnLive) rpc(s *rigSession, command map[string]any) map[string]any {
	r.t.Helper()
	id, _ := command["id"].(string)
	if err := s.send(command); err != nil {
		r.t.Fatalf("send %v to the session %s: %v", command["type"], s.name, err)
	}
	var answer map[string]any
	r.await(fmt.Sprintf("the session %s to answer %v", s.name, command["type"]), paneReady, func() bool {
		if !s.running() {
			r.t.Fatalf("the session %s ended before it answered %v; its log is %s", s.name, command["type"], s.log)
		}
		var ok bool
		answer, ok = s.answer(id)
		return ok
	})
	if success, _ := answer["success"].(bool); !success {
		r.t.Fatalf("the session %s refused %v: %v", s.name, command["type"], answer)
	}
	return answer
}

// sessionFileOf is where a hand-started session keeps its session, as its get_state says.
func (r *cutTurnLive) sessionFileOf(s *rigSession) string {
	r.t.Helper()
	answer := r.rpc(s, map[string]any{"type": "get_state", "id": "state-" + s.name})
	data, _ := answer["data"].(map[string]any)
	path, _ := data["sessionFile"].(string)
	if path == "" {
		r.t.Fatalf("the session %s's state names no session file: %v", s.name, answer)
	}
	return path
}

// ---- what a step reads ---------------------------------------------------------------------------

// w2Entry is one transcript entry as Oh My Pi writes it, with its index.
type w2Entry struct {
	index      int
	raw        string
	Type       string          `json:"type"`
	Timestamp  string          `json:"timestamp"`
	CustomType string          `json:"customType"`
	Content    json.RawMessage `json:"content"`
	Details    json.RawMessage `json:"details"`
	Data       json.RawMessage `json:"data"`
	Message    *struct {
		Role       string          `json:"role"`
		StopReason string          `json:"stopReason"`
		Content    json.RawMessage `json:"content"`
	} `json:"message"`
}

// transcript is the session at path as its store holds it now: the file, or with SQL sessions its
// row. A line the kill cut short is left out.
func (r *cutTurnLive) transcript(path string) []w2Entry {
	r.t.Helper()
	return w2Entries(r.stored(path))
}

// w2Entries is a session's text as its entries, each with its index; a line that does not parse,
// as one a kill cut short does not, is left out.
func w2Entries(text string) []w2Entry {
	var entries []w2Entry
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e w2Entry
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		e.index, e.raw = len(entries), line
		entries = append(entries, e)
	}
	return entries
}

// stored is the session's whole text, "" before Oh My Pi has written any of it.
func (r *cutTurnLive) stored(path string) string {
	r.t.Helper()
	text, err := r.storedText(path)
	if err != nil {
		r.t.Fatal(err)
	}
	return text
}

// storedText is stored for a reader that is not the test's own goroutine, which cannot end it.
func (r *cutTurnLive) storedText(path string) (string, error) {
	if r.sql != nil {
		text, _, err := r.sql.read(path)
		if err != nil {
			return "", fmt.Errorf("read the session row %s: %w", path, err)
		}
		return text, nil
	}
	body, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("read the session file %s: %w", path, err)
	}
	return string(body), nil
}

// keepTranscript writes the session's text to the evidence directory, under the store and step.
func (r *cutTurnLive) keepTranscript(path string) {
	dir := filepath.Join(r.evidence, "transcripts")
	_ = os.MkdirAll(dir, 0o700)
	out := filepath.Join(dir, r.store+"-"+r.running+".jsonl")
	if err := os.WriteFile(out, []byte(r.stored(path)), 0o600); err != nil {
		r.t.Fatalf("keep the transcript: %v", err)
	}
	r.record("transcript", "kept at "+out)
}

func w2Text(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var texts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			texts = append(texts, b.Text)
		}
	}
	return strings.Join(texts, "\n")
}

// text is what the entry says: a custom message's content, or a message's text blocks.
func (e w2Entry) text() string {
	if e.Message != nil {
		return w2Text(e.Message.Content)
	}
	return w2Text(e.Content)
}

func (e w2Entry) role() string {
	if e.Type != "message" || e.Message == nil {
		return ""
	}
	return e.Message.Role
}

func (e w2Entry) isPrompt() bool { return e.role() == "user" }
func (e w2Entry) isReply() bool  { return e.role() == "assistant" }

// isNote is the plugin's cut-turn note: an envoy-message whose envoy block's notice is
// CUT_TURN_NOTICE.
func (e w2Entry) isNote() bool {
	if e.Type != "custom_message" || e.CustomType != "envoy-message" {
		return false
	}
	for _, line := range strings.Split(e.text(), "\n") {
		if value, ok := strings.CutPrefix(line, "  notice: "); ok && strings.Trim(value, `"`) == w2NoteNotice {
			return true
		}
	}
	return false
}

// cardIdentity is the frame an envoy-message card shows, as the plugin reads it back
// (writtenCardIdentity): the event id and dedupe key its details carry (the plugin's ledger
// identity), else the id its envoy block names. ok is false for every other entry, the plugin's
// own notes included.
func (e w2Entry) cardIdentity() (eventID, dedupeKey string, ok bool) {
	if e.Type != "custom_message" || e.CustomType != "envoy-message" || e.isNote() {
		return "", "", false
	}
	var details struct {
		EventID   string `json:"event_id"`
		DedupeKey string `json:"dedupe_key"`
	}
	if json.Unmarshal(e.Details, &details) == nil && (details.EventID != "" || details.DedupeKey != "") {
		return details.EventID, details.DedupeKey, true
	}
	for _, line := range strings.Split(e.text(), "\n") {
		if value, found := strings.CutPrefix(line, "  id: "); found {
			return strings.Trim(value, `"`), "", true
		}
	}
	return "", "", false
}

// cardOf is whether the entry is the card of the frame eventID.
func (e w2Entry) cardOf(eventID string) bool {
	id, _, ok := e.cardIdentity()
	return ok && id == eventID
}

// cardFor is whether the entry is the card of the message p: an event id a send of it was
// forwarded as, or its dedupe key.
func (e w2Entry) cardFor(p *w2Published) bool {
	id, key, ok := e.cardIdentity()
	return ok && ((id != "" && (id == p.eventID || slices.Contains(p.eventIDs, id))) || (key != "" && key == p.dedupeKey))
}

// mark is what the plugin's turn mark says, and whether the entry is one.
func (e w2Entry) mark() (ended bool, ok bool) {
	if e.Type != "custom" || e.CustomType != w2TurnMark {
		return false, false
	}
	var data struct {
		Ended *bool `json:"ended"`
	}
	if json.Unmarshal(e.Data, &data) != nil || data.Ended == nil {
		return false, false
	}
	return *data.Ended, true
}

// callsSleep is whether the entry is a reply that stops to call `sleep seconds`.
func (e w2Entry) callsSleep(seconds int) bool {
	return e.isReply() && e.Message.StopReason == "toolUse" && strings.Contains(e.raw, "sleep "+strconv.Itoa(seconds))
}

// toolCallIDs is the id of every tool call a reply makes.
func (e w2Entry) toolCallIDs() []string {
	if e.Message == nil {
		return nil
	}
	var blocks []struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	_ = json.Unmarshal(e.Message.Content, &blocks)
	var ids []string
	for _, b := range blocks {
		if b.Type == "toolCall" && b.ID != "" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// executionStarted is the call id of the host's own `tool_execution_start` entry, which Oh My Pi
// writes as it hands a call to its tool, and whether the entry is one.
func (e w2Entry) executionStarted() (string, bool) {
	if e.Type != "custom" || e.CustomType != "tool_execution_start" {
		return "", false
	}
	var data struct {
		ToolCallID string `json:"toolCallId"`
	}
	if json.Unmarshal(e.Data, &data) != nil || data.ToolCallID == "" {
		return "", false
	}
	return data.ToolCallID, true
}

// describe is the entry in one line, for a step's record of what the resumed process wrote.
func (e w2Entry) describe() string {
	oneLine := func(s string, n int) string {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > n {
			return s[:n] + "…"
		}
		return s
	}
	if ended, ok := e.mark(); ok {
		return fmt.Sprintf("%d turn mark ended=%t", e.index, ended)
	}
	switch {
	case e.isNote():
		return fmt.Sprintf("%d the plugin's cut-turn note", e.index)
	case e.Type == "custom_message" && e.CustomType == "envoy-message":
		return fmt.Sprintf("%d card: %s", e.index, oneLine(e.text(), 140))
	case e.isPrompt():
		return fmt.Sprintf("%d prompt: %s", e.index, oneLine(e.text(), 140))
	case e.isReply():
		return fmt.Sprintf("%d reply (%s): %s", e.index, e.Message.StopReason, oneLine(e.text(), 100))
	case e.role() != "":
		return fmt.Sprintf("%d %s message", e.index, e.role())
	}
	return fmt.Sprintf("%d %s %s", e.index, e.Type, e.CustomType)
}

// w2ClockAt is clockAt of a moment the claim listing may not hold. The driver defines it rather
// than read another proof's helper, since the negative control builds it over the base with the rig
// alone.
func w2ClockAt(at *time.Time) string {
	if at == nil {
		return clockAt(time.Time{})
	}
	return clockAt(*at)
}

// entryTime is the entry's own timestamp, or the zero time where it has none.
func (r *cutTurnLive) entryTime(entries []w2Entry, index int) time.Time {
	if index < 0 || index >= len(entries) {
		return time.Time{}
	}
	at, _ := time.Parse(time.RFC3339Nano, entries[index].Timestamp)
	return at
}

// awaitEntry waits for an entry at or after from that want holds of, and answers its index.
func (r *cutTurnLive) awaitEntry(path string, from int, what string, within time.Duration, want func(w2Entry) bool) int {
	r.t.Helper()
	found := -1
	r.await(what, within, func() bool {
		entries := r.transcript(path)
		for i := max(from, 0); i < len(entries); i++ {
			if want(entries[i]) {
				found = i
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
		return false
	})
	return found
}

// w2Resend is how long awaitCard waits for the card of a message the listener forwarded to no
// holder before it sends the message again under its dedupe key; w2Sends bounds the sends. A
// message the listener forwarded is not sent again: every forward of a dedupe key to a holder
// carries one Nats-Msg-Id (bus.ForwardMsgID), so a send inside the stream's duplicate window is
// dropped and one after it is a second copy.
const (
	w2Resend = 45 * time.Second
	w2Sends  = 5
)

// awaitCard waits for the card of p at or after from, and answers its index. The card's own event
// id becomes p's. A message the listener forwarded to no holder is sent again under its own key
// while no card of it is written.
func (r *cutTurnLive) awaitCard(path string, p *w2Published, from int) int {
	r.t.Helper()
	found := -1
	r.await("the card of "+p.dedupeKey, w2CardWithin+w2Sends*w2Resend, func() bool {
		entries := r.transcript(path)
		for _, e := range entries[min(max(from, 0), len(entries)):] {
			if e.cardFor(p) {
				found = e.index
				p.eventID, _, _ = e.cardIdentity()
				return true
			}
		}
		if !p.confirmed && !p.stored && p.message != "" && p.sends < w2Sends && time.Since(p.at) > w2Resend {
			r.send(p)
		}
		time.Sleep(200 * time.Millisecond)
		return false
	})
	return found
}

// w2Release is when and why releaseAtTheCardsTurn let the relay's held prompts go, or the read
// that stopped it.
type w2Release struct {
	at  time.Time
	why string
	err error
}

// releaseAtTheCardsTurn lets the relay's held prompts to token go as soon as the turn of the rig's
// card has started: the first card at or after entry from whose text holds word, then the
// plugin's start mark or a reply after it. It reads the transcript off the test's goroutine,
// since the publish that sends the card waits out the listener's receipt window first, and stops
// at the end of the step. A read that fails lets the prompts go too, and answers the error.
func (r *cutTurnLive) releaseAtTheCardsTurn(token claim.Token, path string, from int, word string) <-chan w2Release {
	released := make(chan w2Release, 1)
	stop := make(chan struct{})
	r.t.Cleanup(func() { close(stop) })
	go func() {
		for {
			text, err := r.storedText(path)
			why, started := "", false
			if err == nil {
				why, started = cardsTurnStarted(w2Entries(text), from, word)
			}
			if err != nil || started {
				r.relay.releasePrompts(token)
				released <- w2Release{at: time.Now(), why: why, err: err}
				return
			}
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	return released
}

// cardsTurnStarted is whether the turn of the first card at or after entry from whose text holds
// word has started, and what says so.
func cardsTurnStarted(entries []w2Entry, from int, word string) (string, bool) {
	card := -1
	for _, e := range entries[min(max(from, 0), len(entries)):] {
		if card < 0 {
			if _, _, ok := e.cardIdentity(); ok && strings.Contains(e.text(), word) {
				card = e.index
			}
			continue
		}
		if ended, ok := e.mark(); ok && !ended {
			return fmt.Sprintf("the card at entry %d started a turn: the plugin's start mark is entry %d", card, e.index), true
		}
		if e.isReply() {
			return fmt.Sprintf("the card at entry %d started a turn: its reply is entry %d", card, e.index), true
		}
	}
	return "", false
}

// awaitReply waits for a reply at or after from that says word.
func (r *cutTurnLive) awaitReply(path, word string, from int) int {
	r.t.Helper()
	return r.awaitEntry(path, from, "a reply saying "+word, w2ReplyWithin, func(e w2Entry) bool {
		return e.isReply() && strings.Contains(e.text(), word)
	})
}

// awaitToolCall waits, from entry after on, for the turn to be running `sleep seconds`: a reply
// that stops to call it, the host's own `tool_execution_start` entry for that call, and no tool
// result after it. The host runs a bash call's command in its own shell, with no process of the
// command's to read, so the transcript is where a running call is seen. A call the host moved to
// the background answers a result, and fails the step: the kill must land while the call runs.
func (r *cutTurnLive) awaitToolCall(path string, after, seconds int) int {
	r.t.Helper()
	call, started := -1, -1
	r.await(fmt.Sprintf("the turn to run `sleep %d`", seconds), w2ToolCallWithin, func() bool {
		entries := r.transcript(path)
		call, started = -1, -1
		for i := max(after, 0); i < len(entries); i++ {
			if entries[i].callsSleep(seconds) {
				call = i
			}
		}
		if call < 0 {
			time.Sleep(200 * time.Millisecond)
			return false
		}
		ids := entries[call].toolCallIDs()
		for _, e := range entries[call+1:] {
			if e.role() == "toolResult" {
				r.fails("tool-call-running", "the call at entry %d had its result at entry %d before the kill, so the turn did not stay in it: %s",
					call, e.index, strings.Join(strings.Fields(e.text()), " "))
			}
			if id, ok := e.executionStarted(); ok && slices.Contains(ids, id) {
				started = e.index
			}
		}
		if started < 0 {
			time.Sleep(100 * time.Millisecond)
			return false
		}
		return true
	})
	r.record("tool-call", fmt.Sprintf("entry %d calls `sleep %d`, entry %d is the host's tool_execution_start of it, and no tool result follows, at %s",
		call, seconds, started, clockAt(time.Now())))
	return call
}

// awaitTurnOn waits for the turn the card at entry card starts: the daemon reads the claim
// working, or a reply or a start mark follows the card. It answers the index of the entry the
// kill is measured from (the card).
func (r *cutTurnLive) awaitTurnOn(token claim.Token, path string, card int) int {
	r.t.Helper()
	var why string
	r.await("the turn the tick wake starts", w2CardWithin, func() bool {
		if c, ok := r.findClaim(token); ok && c.State == string(supervise.StateWorking) {
			why = "the daemon reads the controller working"
			return true
		}
		for _, e := range r.transcript(path)[card+1:] {
			if ended, ok := e.mark(); ok && !ended {
				why = fmt.Sprintf("a start mark follows the card at entry %d", e.index)
				return true
			}
			if e.isReply() {
				why = fmt.Sprintf("a reply follows the card at entry %d", e.index)
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
		return false
	})
	r.record("wake-turn", why+" at "+clockAt(time.Now()))
	return card
}

// awaitQuiet waits until the claim holds no task and runs no turn, and has stayed so for w2Quiet.
func (r *cutTurnLive) awaitQuiet(token claim.Token, within time.Duration) {
	r.awaitQuietWithin(token, within, w2Quiet)
}

func (r *cutTurnLive) awaitQuietWithin(token claim.Token, within, settle time.Duration) {
	r.t.Helper()
	var since time.Time
	r.await(string(token)+" to hold no task and run no turn", within, func() bool {
		c, ok := r.findClaim(token)
		if !ok {
			return false
		}
		if c.State == string(supervise.StateFailed) {
			r.t.Fatalf("%s failed; its output is under %s", token, r.rt.paneLogs)
		}
		switch {
		case c.Pending != nil || (c.State != string(supervise.StateReady) && c.State != string(supervise.StateIdle)):
			since = time.Time{}
		case since.IsZero():
			since = time.Now()
		case time.Since(since) >= settle:
			return true
		}
		time.Sleep(200 * time.Millisecond)
		return false
	})
}

// findClaim is token as the operator's claim listing shows it now, and false while it lists none.
func (r *cutTurnLive) findClaim(token claim.Token) (api.OperatorClaim, bool) {
	r.t.Helper()
	status, body := r.d.request(http.MethodGet, "/legion/v1/operator/claims", nil, true)
	if status != http.StatusOK {
		return api.OperatorClaim{}, false
	}
	var list api.OperatorClaims
	if err := json.Unmarshal(body, &list); err != nil {
		r.t.Fatalf("decode the claim listing %s: %v", body, err)
	}
	for _, held := range list.Claims {
		if held.Token == token {
			return held, true
		}
	}
	return api.OperatorClaim{}, false
}

// ---- what a step holds ---------------------------------------------------------------------------

// holdsTheCutTurn holds that the killed process left the turn cut in its tool call: the card of
// eventID (when the step names one) at entry card, the reply calling `sleep` at entry call after
// it, and no tool result after that.
func (r *cutTurnLive) holdsTheCutTurn(cut w2Cut, eventID string, card, call int) {
	r.t.Helper()
	if call >= cut.length || !cut.entries[call].callsSleep(w2SleepSeconds) {
		r.fails("cut-turn", "the killed process left %d entries, and the call the step saw at entry %d is not among them", cut.length, call)
	}
	if eventID != "" && (card >= cut.length || !cut.entries[card].cardOf(eventID)) {
		r.fails("cut-turn", "the killed process left no card of %s at entry %d", eventID, card)
	}
	for _, e := range cut.entries[call+1:] {
		if e.role() == "toolResult" || e.isReply() {
			r.fails("cut-turn", "the killed process left %s after the call at entry %d: its turn went on past the call", e.describe(), call)
		}
	}
	from := call
	if card >= 0 && card < from {
		from = card
	}
	r.record("cut-turn", fmt.Sprintf("the killed process's last entries end in the cut turn: %s", r.describeRange(cut.entries, max(0, from-1), cut.length)))
}

// joinedTheTaskTurn holds that the notice's card joined the turn the task started: the task's
// prompt, then its first call at entry first, the card after that call, and no end mark from the
// prompt to the kill. It records when the plugin handed the card to the host (the entry's own
// time) against that call's start and result.
func (r *cutTurnLive) joinedTheTaskTurn(cut w2Cut, marker string, first, card int) {
	r.t.Helper()
	prompt := -1
	for i := first - 1; i >= 0; i-- {
		if cut.entries[i].isPrompt() && strings.Contains(cut.entries[i].text(), marker) {
			prompt = i
			break
		}
	}
	if prompt < 0 || card <= first {
		r.fails("joined-the-task-turn", "the task's prompt is entry %d, its first call entry %d and the notice's card entry %d: the card is not after the task's call",
			prompt, first, card)
	}
	result := -1
	for _, e := range cut.entries[prompt+1:] {
		if ended, ok := e.mark(); ok && ended {
			r.fails("joined-the-task-turn", "the killed process left an end mark at entry %d, after the task's prompt at entry %d: the task's turn ended before the kill",
				e.index, prompt)
		}
		if result < 0 && e.index > first && e.index < card && e.role() == "toolResult" {
			result = e.index
		}
	}
	r.record("joined", fmt.Sprintf("the task's prompt is entry %d, its call `sleep %d` entry %d (written %s), the call's result entry %d (written %s); the plugin handed the notice's card to the host at %s, and the host wrote it as entry %d; no end mark follows the prompt",
		prompt, w2TaskSleepSeconds, first, clockAt(r.entryTime(cut.entries, first)), result, clockAt(r.entryTime(cut.entries, result)),
		clockAt(r.entryTime(cut.entries, card)), card))
}

// holdsTheWakeTurn holds that the killed controller left the turn the wake started open: the card
// of eventID at entry card, and no end mark after it.
func (r *cutTurnLive) holdsTheWakeTurn(cut w2Cut, eventID string, card int) {
	r.t.Helper()
	if card >= cut.length || !cut.entries[card].cardOf(eventID) {
		r.fails("wake-turn", "the killed process left no card of %s at entry %d", eventID, card)
	}
	for _, e := range cut.entries[card+1:] {
		if ended, ok := e.mark(); ok && ended {
			r.fails("wake-turn", "the killed process left an end mark at entry %d, after the card: the wake's turn ended before the kill", e.index)
		}
	}
	r.record("cut-turn", fmt.Sprintf("the killed controller's last entries end in the wake's turn: %s", r.describeRange(cut.entries, max(0, card-1), cut.length)))
}

// describeResumed records every entry the resumed process wrote, from the cut on.
func (r *cutTurnLive) describeResumed(entries []w2Entry, cut int) {
	r.record("resumed-transcript", fmt.Sprintf("the resumed process wrote entries %d to %d: %s", cut, len(entries)-1, r.describeRange(entries, cut, len(entries))))
}

func (r *cutTurnLive) describeRange(entries []w2Entry, from, to int) string {
	var lines []string
	for _, e := range entries[from:to] {
		lines = append(lines, e.describe())
	}
	return "\n" + indentOutput(strings.Join(lines, "\n"))
}

// cutNotes is every cut-turn note of the transcript.
func cutNotes(entries []w2Entry) []w2Entry {
	var notes []w2Entry
	for _, e := range entries {
		if e.isNote() {
			notes = append(notes, e)
		}
	}
	return notes
}

// oneNote holds that the transcript carries exactly one cut-turn note, written by the resumed
// process, and answers it.
func (r *cutTurnLive) oneNote(entries []w2Entry, cut int) w2Entry {
	r.t.Helper()
	notes := cutNotes(entries)
	if len(notes) == 0 {
		r.fails("cut-turn-note", "the resumed transcript holds no note that its previous turn was cut: none of the %d entries the resumed process wrote after the kill is an envoy-message with notice %q",
			len(entries)-cut, w2NoteNotice)
	}
	if len(notes) != 1 {
		var at []string
		for _, n := range notes {
			at = append(at, strconv.Itoa(n.index))
		}
		r.fails("one-note", "the transcript holds %d cut-turn notes, at entries %s, want one", len(notes), strings.Join(at, ", "))
	}
	note := notes[0]
	if note.index < cut {
		r.fails("one-note", "the cut-turn note is entry %d, which the killed process wrote", note.index)
	}
	r.record("note", fmt.Sprintf("entry %d is the plugin's note, written at %s:\n%s", note.index, note.Timestamp, indentOutput(note.text())))
	return note
}

// noteSays holds that the note says the turn was cut, names eventID, and says to read what that
// turn did before acting.
func (r *cutTurnLive) noteSays(note w2Entry, eventID string) {
	r.t.Helper()
	text := note.text()
	for _, said := range []string{w2NoteCut, eventID, w2NoteReadIt, w2NoteNotAgain} {
		if !strings.Contains(text, said) {
			r.fails("note-says", "the note does not say %q; it reads:\n%s", said, indentOutput(text))
		}
	}
}

// noteBeforeTheNextTurn holds that the note comes before the resumed process's first turn (R2:
// "before the first turn that the session runs, whatever starts that turn"): ahead of the entry
// that opens that turn, which is its prompt, its card, or the plugin's own start mark of the run,
// whichever the host wrote first, and so ahead of its first reply. It records the order first, so
// a step that fails here still says where the note stood.
func (r *cutTurnLive) noteBeforeTheNextTurn(entries []w2Entry, cut int, note w2Entry) {
	r.t.Helper()
	opened, replied := -1, -1
	for _, e := range entries[cut:] {
		ended, isMark := e.mark()
		opens := e.isPrompt() || (isMark && !ended) || (e.Type == "custom_message" && e.CustomType == "envoy-message" && !e.isNote())
		if opened < 0 && opens {
			opened = e.index
		}
		if replied < 0 && e.isReply() {
			replied = e.index
		}
	}
	opening := "no entry"
	if opened >= 0 {
		opening = entries[opened].describe()
	}
	r.record("note-position", fmt.Sprintf("the note is entry %d; the resumed process's first turn opens at entry %d (%s) and first replies at entry %d",
		note.index, opened, opening, replied))
	if opened >= 0 && opened < note.index {
		r.fails("note-before-the-next-turn", "the note is entry %d, after entry %d, which opened the resumed process's first turn (%s): it is that turn's context, not ahead of the turn",
			note.index, opened, opening)
	}
	if replied >= 0 && replied < note.index {
		r.fails("note-before-the-next-turn", "the resumed process replied at entry %d, before the note at entry %d", replied, note.index)
	}
}

// shownOnce holds that the card of the delivery p is in the transcript once, written by the killed
// process, and not shown again by the resumed one: no card of its event id or of its dedupe key,
// which every send of it carries, after the cut.
func (r *cutTurnLive) shownOnce(entries []w2Entry, p *w2Published, cut int) {
	r.t.Helper()
	var at []int
	for _, e := range entries {
		if e.cardFor(p) {
			at = append(at, e.index)
		}
	}
	if len(at) != 1 || at[0] >= cut {
		r.fails("not-shown-again", "the card of %s (%s) is at entries %v, want once and before the kill's cut at entry %d", p.eventID, p.dedupeKey, at, cut)
	}
	r.record("not-shown-again", fmt.Sprintf("the card of %s (%s) is in the transcript once, at entry %d, which the killed process wrote; the resumed process wrote none",
		p.eventID, p.dedupeKey, at[0]))
}

// storedAsTheStoreSays holds that the session at path is kept where the run's store keeps it: a
// file, or with SQL sessions a row and no file.
func (r *cutTurnLive) storedAsTheStoreSays(path string) {
	r.t.Helper()
	_, statErr := os.Stat(path)
	if r.sql == nil {
		if statErr != nil {
			r.fails("store", "the session file %s: %v", path, statErr)
		}
		return
	}
	text, ok, err := r.sql.read(path)
	if err != nil || !ok {
		r.fails("store", "the session %s has no row in %s (%v)", path, r.sql.database, err)
	}
	if !os.IsNotExist(statErr) {
		r.fails("store", "a file is at %s, which a session kept in %s never writes (%v)", path, r.sql.database, statErr)
	}
	r.record("store", fmt.Sprintf("%s is a row of %s (%d bytes) and no file", path, r.sql.database, len(text)))
}

// resumedFromItsStore holds that the resumed process read and wrote the session where the store
// keeps it; with SQL sessions, from its row, no file at its path then or now.
func (r *cutTurnLive) resumedFromItsStore(path string, launch *rigLaunch, resumed api.OperatorClaim) {
	r.t.Helper()
	r.storedAsTheStoreSays(path)
	r.record("resumed-from", fmt.Sprintf("generation %d was started with --resume=%s and wrote its entries there (%s sessions); session %s",
		launch.spec.Generation, launch.spec.ResumeSessionFile, r.store, resumed.Session))
}

// ---- what a step records -------------------------------------------------------------------------

// record prints one observation under the running step and appends it to the results file at once.
func (r *cutTurnLive) record(what, value string) {
	r.note("%s: %s", what, value)
	line, _ := json.Marshal(map[string]string{
		"at": time.Now().UTC().Format(time.RFC3339Nano), "store": r.store, "step": r.running, "what": what, "value": value,
	})
	if _, err := r.results.Write(append(line, '\n')); err == nil {
		_ = r.results.Sync()
	}
}

// normalize is a note with what differs between two runs named: each of names, the project, and
// the runtime's report of the kill that the daemon's note quotes (w2DeathCause).
func (r *cutTurnLive) normalize(text string, names map[string]string) string {
	for spelled, name := range names {
		text = strings.ReplaceAll(text, spelled, name)
	}
	text = strings.ReplaceAll(text, r.cfg.Project, "<project>")
	text = strings.ReplaceAll(text, r.project, "<project>")
	return w2DeathCause.ReplaceAllString(text, `$1"<the runtime's report of the kill>"`)
}

// w2DeathCause is the runtime's report of a kill, as the daemon's note quotes it: "gone: pid
// 1735015 is gone", or "gone: the worker's process group ended; its log is <the pane's log>",
// whichever the runtime saw first. It names the run's processes and pane logs and the order of
// two observations, which no session store decides.
var w2DeathCause = regexp.MustCompile(`(because the process died: )"(?:[^"\\]|\\.)*"`)

// ---- the processes under a pane -------------------------------------------------------------------

// w2Process is one process as /proc names it.
type w2Process struct {
	pid  int
	comm string
}

// w2Descendants is root and every live process under it, read from /proc.
func w2Descendants(root int) []w2Process {
	entries, _ := os.ReadDir("/proc")
	parent, comm := map[int]int{}, map[int]string{}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		lp, rp := bytes.IndexByte(stat, '('), bytes.LastIndexByte(stat, ')')
		if lp < 0 || rp < lp {
			continue
		}
		fields := strings.Fields(string(stat[rp+1:]))
		if len(fields) < 2 {
			continue
		}
		ppid, _ := strconv.Atoi(fields[1])
		parent[pid], comm[pid] = ppid, string(stat[lp+1:rp])
	}
	var tree []w2Process
	for pid := range parent {
		for at := pid; at > 1; at = parent[at] {
			if at == root {
				tree = append(tree, w2Process{pid: pid, comm: comm[pid]})
				break
			}
			if _, known := parent[at]; !known {
				break
			}
		}
	}
	return tree
}

// w2Zombie is whether pid has exited and waits to be reaped.
func w2Zombie(pid int) bool {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	rp := bytes.LastIndexByte(stat, ')')
	fields := strings.Fields(string(stat[rp+1:]))
	return len(fields) > 0 && fields[0] == "Z"
}

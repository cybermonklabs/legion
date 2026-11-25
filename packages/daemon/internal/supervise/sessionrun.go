package supervise

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/store/storetext"
)

// controllerSessionEnds is how many launches of the project controller's session in a row may end
// before that session finishes a turn: the launch after the second such end starts a fresh session
// (replaceEndedSession, dispatch://INFRA-2045 R1). One end is routine — a node is lost, a pod is
// replaced — and in each measured outage the session failed at every resume, so the second end
// already shows that the session is the cause.
const controllerSessionEnds = 2

// The lines of the controller session's replacement (dispatch://INFRA-2045 R4), which an operator
// and a production check read whole: the Warn line of the launch that replaces the session
// (replaceEndedSession), the Error line of a session that is not replaced because it never finished
// a turn (sessionEnded), and the line of the fresh session's ready, which names the session it
// replaced (the table's ready).
const (
	sessionReplacedLine    = "supervise: the controller's session ended two launches in a row before it finished a turn; this launch starts a fresh session in its place"
	sessionNotReplacedLine = "supervise: the controller's session ended two launches in a row and has never finished a turn, so it is not replaced: a fresh session also failed, and the keeper retries it with its waits"
	freshSessionReadyLine  = "supervise: the controller's fresh session is ready in place of the session it replaced"
)

// SessionRun is the run of the recorded session's launches since that session last finished a
// turn (Claim.SessionRun, dispatch://INFRA-2045 R1): how many of them ended in a way the machine
// charges, how the last of them ended, and whether the session has finished a turn under this
// claim at all.
//
// An end is counted where the machine charges it (sessionEnded's callers): a process that died,
// before or after its agent said ready, which is charged a launch failure or a death (died); a
// process whose agent never registered or never said ready, retired at its deadline and charged a
// launch failure (registrationDeadline); and a controller turn the daemon ended because it stayed
// open past its bound (turnPastBound). An end the machine charges nothing is not counted: an
// operator's suspension, the stale-plugin relaunch, a process found at a stale address or
// displaced, a death the roll edge or a resume by Oh My Pi's own lookup leaves uncharged
// (endUncharged), and a start the runtime refused, which is retried inside its own launch
// (launchTries) and says nothing of the session — a pod the cluster cannot place is not the
// session's fault.
//
// A finished turn starts the run again (turnEnded), and ready alone does not: a session killed
// seconds after its agent came up would never reach two ends if its ready reset them. A forgotten
// session takes its run with it (forgetSession), so the session after it starts with no end and no
// finished turn. The run is persisted with the claim, so a daemon restart between two ends reads
// the count the daemon before it kept.
//
// It decides one thing, for the project's controller alone: a session that has finished a turn and
// then ended controllerSessionEnds launches in a row is forgotten by the next launch, which spawns
// a fresh agent (replaceEndedSession). A session that has never finished a turn is not replaced: a
// fresh session that fails the same way shows the cause is not the session, and it is left to the
// controller keeper's retries, with one Error line (sessionEnded).
type SessionRun struct {
	// Ends is how many launches of the session ended, charged, since it last finished a turn.
	Ends int `json:"ends,omitempty"`
	// Endings is how the last of those ends came about, the oldest first, at most
	// controllerSessionEnds of them: what the replacement's line and the fresh session's first
	// prompt name.
	Endings []string `json:"endings,omitempty"`
	// TurnFinished is whether the session has finished a turn under this claim.
	TurnFinished bool `json:"turnFinished,omitempty"`
}

// sessionEnded counts one charged end of a launch of the recorded session (SessionRun), how being
// the words for what ended it. A claim that records no session — a fresh spawn whose agent has not
// registered yet — has no session the end is of, and counts nothing. The end that brings the
// project controller's session to controllerSessionEnds while that session has never finished a
// turn writes one Error line: such a session is not replaced, and only the keeper's retries bring
// it back.
func (m *Machine) sessionEnded(how string) {
	if m.claim.Session == "" {
		return
	}
	run := &m.claim.SessionRun
	run.Ends++
	run.Endings = append(run.Endings, storetext.FarSideText(how))
	if over := len(run.Endings) - controllerSessionEnds; over > 0 {
		run.Endings = slices.Clone(run.Endings[over:])
	}
	if m.claim.Role == claim.RoleController && run.Ends == controllerSessionEnds && !run.TurnFinished {
		m.log.Error(sessionNotReplacedLine, "session", m.claim.Session, "endings", run.Endings)
	}
}

// diedEnding is how a death the machine charged ended the session's launch (died), for the run's
// record: whether the agent had said ready, and the runtime's account of the death.
func diedEnding(beforeReady bool, cause string) string {
	if beforeReady {
		return "its process died before the controller was ready: " + cause
	}
	return "its process died after the controller was ready: " + cause
}

// replaceEndedSession forgets the project controller's recorded session where its run says the
// session has failed (SessionRun): it finished a turn, and then controllerSessionEnds of its
// launches in a row ended before it finished another. It runs in every launch, after that launch
// let the old process go (launchNow, letGo), so the launch the second end raised — the death's
// relaunch, the relaunch of a turn ended past its bound, or the keeper's retry of a claim that end
// failed — spawns a fresh agent, and what the old process ended is recorded before the forget, which
// tells the fresh agent none of it (forgetSession). The fresh agent is told in its first prompt
// which session it replaces and why (abandonedTask), and one Warn line names the old session, the
// reason and how each of the two launches ended (dispatch://INFRA-2045 R4). The old session's
// transcript stays in its store.
//
// A session the claim may not clear yet — one it records and does not hold, whose role the Envoy
// listener still names it the holder of (mayForget) — is resumed as before, and the line says why;
// the next launch asks again.
func (m *Machine) replaceEndedSession(ctx context.Context) {
	run := m.claim.SessionRun
	if m.claim.Role != claim.RoleController || m.claim.Session == "" || !run.TurnFinished || run.Ends < controllerSessionEnds {
		return
	}
	old := m.claim.Session
	if err := m.mayForget(ctx, "the replacement of the controller's session"); err != nil {
		m.log.Warn("supervise: the controller's session ended two launches in a row before it finished a turn, but it cannot be cleared yet, so this launch resumes it",
			"session", old, "endings", run.Endings, "error", err)
		return
	}
	why := fmt.Sprintf("its last %d launches ended before it finished a turn: %s", run.Ends, strings.Join(run.Endings, "; then "))
	m.log.Warn(sessionReplacedLine, "session", old, "requirement", "R1", "reason", why, "endings", run.Endings)
	m.forgetSession(why)
}

// turnPastBound is the TurnPastBound event's row, for the one state a turn runs in.
func turnPastBound(m *Machine, ctx context.Context, ev Event) error {
	return m.endTurnPastBound(ctx, ev.(TurnPastBound))
}

// endTurnPastBound is the controller keeper's end of a controller turn open longer than its bound
// (TurnPastBound, dispatch://INFRA-2045 R2). It acts only on the turn the keeper read: one still
// open, begun at the moment the keeper read, and still past the bound now, since a turn that ended
// and one that began since are the controller working. A claim that already holds a wake is left to
// that wake, as the stall backstop leaves one (backstopStalledTurn): its turn replaces the process.
//
// Otherwise it ends the launch as the stall backstop ends a stalled turn, whether or not steps still
// arrive in the turn (replaceProcess): the task the turn served goes back to waiting and is sent to
// the relaunched agent again, a held suspension ends in a suspension, and the recorded session is
// resumed on a new process — unless this end is the session's second in a row, when the relaunch
// starts a fresh one (sessionEnded, replaceEndedSession). The end itself charges no budget, as the
// backstop's does not, so a controller ended past its bound does not spend its launch budget.
func (m *Machine) endTurnPastBound(ctx context.Context, ev TurnPastBound) error {
	start, now := m.claim.LastTurnStart, m.deps.Clock.Now()
	if !m.turnOpen() || !start.Equal(ev.TurnStart) || !start.Add(ev.Bound).Before(now) {
		m.log.Info("supervise: the controller turn the keeper read open past its bound has ended, or another began; it is left alone",
			"turnStartedAt", start, "read", ev.TurnStart, "bound", ev.Bound)
		return nil
	}
	if m.wake != nil {
		m.log.Info("supervise: the controller turn open past its bound is left to the launch already queued for this claim",
			"kind", m.wake.kind, "due", m.wake.at, "bound", ev.Bound)
		return nil
	}
	m.log.Warn("supervise: the controller's turn stayed open past its bound; the daemon ends the launch, and the task it was serving stays owed",
		"turnStartedAt", start, "bound", ev.Bound, "open", now.Sub(start).Round(time.Second))
	// The end is counted before the replacement, because the launch the replacement makes is the
	// one that reads the count (launchNow, replaceEndedSession), and a second end in a row must make
	// that launch fresh. A replacement that ended nothing — a take-back the store refused, a launch
	// the lifecycle refused (EndNotMade) — leaves the same turn open with the process it found, and
	// the keeper's next look posts it again, so its count is taken back: one turn is one end.
	counted := m.claim.SessionRun
	counted.Endings = slices.Clone(counted.Endings)
	m.sessionEnded(fmt.Sprintf("its turn stayed open longer than %s, from %s", ev.Bound, start.UTC().Format(time.RFC3339)))
	end, err := m.replaceProcess(ctx, "the daemon ended a controller turn that stayed open past its bound", ordinaryEnd, endTurnPastBound(ev.Bound))
	if end == EndNotMade {
		m.claim.SessionRun = counted
	}
	m.log.Info("supervise: the end of the controller turn open past its bound", "end", end)
	return err
}

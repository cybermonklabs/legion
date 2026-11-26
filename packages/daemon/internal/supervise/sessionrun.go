package supervise

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

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
// and a production check read whole: the Warn line of the launch that replaces the session under
// R1 (replaceEndedSession) and under R3 (replaceAgedSession), the Error line of a session that is
// not replaced because it never finished a turn (reportNotReplaced), whose `replacement` says
// whether that session took the place of an earlier one, the Warn line of a session id R3 can read
// no age off (replaceAgedSession), and the line of the fresh session's ready, which names the
// session it replaced (the table's ready).
const (
	sessionReplacedLine    = "supervise: the controller's session ended two launches in a row before it finished a turn; this launch starts a fresh session in its place"
	sessionAgedOutLine     = "supervise: the controller's session is older than controller_session_max_age_hours; this relaunch starts a fresh session in its place"
	sessionUnagedLine      = "supervise: the controller's session id is no UUIDv7, so controller_session_max_age_hours cannot read its age and its relaunches resume it"
	sessionNotReplacedLine = "supervise: the controller's session ended two launches in a row and has never finished a turn, so it is not replaced, and the keeper retries it with its waits"
	freshSessionReadyLine  = "supervise: the controller's fresh session is ready in place of the session it replaced"
)

// SessionRun is the run of the recorded session's launches since that session last finished a
// turn (Claim.SessionRun, dispatch://INFRA-2045 R1): how many of them ended in one of the three
// ways counted, how the last of them ended, and whether the session has finished a turn under this
// claim at all.
//
// Three ends are counted (countEnd), and nothing else: a process that died, before or after its
// agent said ready, where the machine charges the death a launch failure or a death (died); a
// process whose agent never registered or never said ready, retired at its deadline and charged a
// launch failure (registrationDeadline); and a controller turn the daemon ended because it stayed
// open past its bound (endTurnPastBound), which charges no budget. Not counted: an operator's
// suspension, a held suspension, the stale-plugin relaunch, a process found at a stale address or
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
	// Ends is how many launches of the session ended in one of the three ways counted since it
	// last finished a turn.
	Ends int `json:"ends,omitempty"`
	// Endings is how the last of those ends came about, the oldest first, at most
	// controllerSessionEnds of them: what the replacement's line and the fresh session's first
	// prompt name.
	Endings []string `json:"endings,omitempty"`
	// TurnFinished is whether the session has finished a turn under this claim.
	TurnFinished bool `json:"turnFinished,omitempty"`
}

// sessionEnded counts one charged end of a launch of the project controller's recorded session
// (SessionRun), how being the words for what ended it, and writes the Error line of a session that
// never finished a turn where this end brings it to controllerSessionEnds (countEnd,
// reportNotReplaced).
func (m *Machine) sessionEnded(how string) {
	if m.countEnd(how) {
		m.reportNotReplaced(m.claim.Session, m.claim.SessionRun.Endings, m.claim.Abandoned.Session)
	}
}

// countEnd is sessionEnded's count alone, and whether this end brought a session that never
// finished a turn to controllerSessionEnds, for a caller that writes the Error line only once it
// knows the end stands (endTurnPastBound). Only the controller's run is counted, since only the
// controller's launch reads it (replaceEndedSession). A claim that records no session — a fresh
// spawn whose agent has not registered yet — has no session the end is of, and counts nothing.
func (m *Machine) countEnd(how string) (reached bool) {
	if m.claim.Role != claim.RoleController || m.claim.Session == "" {
		return false
	}
	run := &m.claim.SessionRun
	run.Ends++
	run.Endings = append(run.Endings, storetext.FarSideText(how))
	if over := len(run.Endings) - controllerSessionEnds; over > 0 {
		run.Endings = slices.Clone(run.Endings[over:])
	}
	return run.Ends == controllerSessionEnds && !run.TurnFinished
}

// reportNotReplaced is the one Error line of a controller session that ended
// controllerSessionEnds launches in a row and never finished a turn: it is not replaced, and only
// the keeper's retries bring it back. Such a session is the controller's first, or one that
// started in place of a session the claim left behind (replaced, Claim.Abandoned.Session: a fresh
// session of R1 or R3, of a retry in a new session, or of an operator's forget-session), and the
// line says which (`replacement`, and `replaced` names the session left behind). An agent that
// registered the left-behind session itself is no replacement of it. The caller names the
// session, its endings and the session left behind as they stood at the end, since a relaunch
// made since may have changed all three (endTurnPastBound).
func (m *Machine) reportNotReplaced(session string, endings []string, replaced string) {
	if replaced == session {
		replaced = ""
	}
	m.log.Error(sessionNotReplacedLine, "session", session, "endings", endings,
		"replacement", replaced != "", "replaced", replaced)
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
//
// It answers whether the rule applied, forgotten or refused, so the launch asks the same session's
// forget no second time for R3 (launchNow, replaceAgedSession): a refusal of R1's is a lookup under
// the machine's lock, and its line already says the session is resumed.
func (m *Machine) replaceEndedSession(ctx context.Context) (applied bool) {
	run := m.claim.SessionRun
	if m.claim.Role != claim.RoleController || m.claim.Session == "" || !run.TurnFinished || run.Ends < controllerSessionEnds {
		return false
	}
	old := m.claim.Session
	if err := m.mayForget(ctx, "the replacement of the controller's session"); err != nil {
		m.log.Warn("supervise: the controller's session ended two launches in a row before it finished a turn, but it cannot be cleared yet, so this launch resumes it",
			"session", old, "endings", run.Endings, "error", err)
		return true
	}
	why := fmt.Sprintf("%d of its launches in a row ended before it finished a turn; the last %d: %s",
		run.Ends, len(run.Endings), strings.Join(run.Endings, "; then "))
	m.log.Warn(sessionReplacedLine, "session", old, "requirement", "R1", "reason", why, "endings", run.Endings)
	m.forgetSession(why)
	return true
}

// replaceAgedSession forgets the project controller's recorded session where its id says it began
// more than Deps.ControllerSessionMaxAge ago, so the relaunch running it spawns a fresh agent
// (dispatch://INFRA-2045 R3). It runs in every launch, after the launch let the old process go
// (launchNow, letGo), which is what makes it a rule of relaunches alone: the daemon stops no live
// controller for its session's age, and an old session is replaced at the next launch the daemon
// makes anyway — every launch of the controller that no operator's request raised, a death's, a
// launch failure's, a pod update's or displacement's, a stale-plugin refresh's, a turn ended past
// its bound, the keeper's spawn, retry or resume by rule, and the boot relaunch of an unfinished
// launch among them. It is not asked where replaceEndedSession applied, forgotten or refused, so
// one launch asks mayForget once. A launch an operator's own request raises keeps its session,
// because the operator asked for that session: an operator's resume (RequestResume), and a task an
// operator delivers to a suspended controller (deliverResuming). Those are the controller's two
// exempt launches that keep a session (exemptWake; an operator's retry reaches the controller only
// when it is held for a cyber-policy refusal, Claim.KeeperRetries, and that retry leaves the session
// behind before its launch, freshSession; the operator's spawn route names an issue; and the
// keeper's start message, the daemon's own task of no row, resumes nothing:
// RequestDeliver.ResumesNothing), and the wake gate lets every launch of the controller through by
// identity, so for the controller exempt is read for this rule alone. The
// fresh agent is told in its first prompt which session it replaces and why (abandonedTask), and
// one Warn line names the old session, the reason and when it started (R4). The old session's
// transcript stays in its store.
//
// The age is the time the session id records, a UUIDv7, so it survives a daemon restart and a new
// pod. An id of another form says nothing of its age: the session is resumed, and one Warn line,
// once for each such session, says the rule cannot read it. A session the claim may not clear yet
// (mayForget) is resumed as replaceEndedSession resumes one, and the next relaunch asks again.
func (m *Machine) replaceAgedSession(ctx context.Context, exempt bool) {
	maxAge, old := m.deps.ControllerSessionMaxAge, m.claim.Session
	if maxAge <= 0 || exempt || m.claim.Role != claim.RoleController || old == "" {
		return
	}
	started, ok := sessionStarted(old)
	if !ok {
		if m.unagedSession != old {
			m.unagedSession = old
			m.log.Warn(sessionUnagedLine, "session", old)
		}
		return
	}
	now := m.deps.Clock.Now()
	if !started.Add(maxAge).Before(now) {
		return
	}
	if err := m.mayForget(ctx, "the replacement of the controller's old session"); err != nil {
		m.log.Warn("supervise: the controller's session is older than controller_session_max_age_hours, but it cannot be cleared yet, so this launch resumes it",
			"session", old, "sessionStartedAt", started, "error", err)
		return
	}
	why := fmt.Sprintf("the session is older than controller_session_max_age_hours (%s): it started at %s, %s before this relaunch",
		maxAge, started.UTC().Format(time.RFC3339), now.Sub(started).Round(time.Minute))
	m.log.Warn(sessionAgedOutLine, "session", old, "requirement", "R3", "reason", why, "sessionStartedAt", started)
	m.forgetSession(why)
}

// sessionStarted is the time an Oh My Pi session id records, which is a UUIDv7: its first 48 bits
// are the milliseconds since the Unix epoch at which the session began. An id of any other form —
// another version, or no UUID at all — says nothing of its age, and answers false.
func sessionStarted(id string) (time.Time, bool) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed.Version() != 7 {
		return time.Time{}, false
	}
	sec, nsec := parsed.Time().UnixTime()
	return time.Unix(sec, nsec), true
}

// turnPastBound is the TurnPastBound event's row, for the one state a turn runs in.
func turnPastBound(m *Machine, ctx context.Context, ev Event) error {
	return m.endTurnPastBound(ctx, ev.(TurnPastBound))
}

// endTurnPastBound is the controller keeper's end of a controller turn open longer than its bound
// (TurnPastBound, dispatch://INFRA-2045 R2). It acts only on the turn the keeper read: one still
// open, begun at the moment the keeper read, and still past the bound now, since a turn that ended
// and one that began since are the controller working. What is a turn is the machine's own answer:
// the claim reads working at a let-go (endState), which a claim working for a compaction with no
// turn under it does not, and the claim's times show the turn open (Claim.OpenTurn), which every
// turn that runs does. No wake can be waiting to replace the process first: the controller's
// launch is never parked, since the wake gate, the project pause and the pod-start gate each pass
// it (gatePaces, pauseHolding, gated).
//
// Otherwise it ends the launch as the stall backstop ends a stalled turn, whether or not steps still
// arrive in the turn (replaceProcess): the task the turn served goes back to waiting and is sent to
// the relaunched agent again, a held suspension ends in a suspension, and the recorded session is
// resumed on a new process — unless this end is the session's second in a row, when the relaunch
// starts a fresh one (countEnd, replaceEndedSession). The end itself charges no budget, as the
// backstop's does not, so a controller ended past its bound does not spend its launch budget.
func (m *Machine) endTurnPastBound(ctx context.Context, ev TurnPastBound) error {
	start, open := m.claim.OpenTurn()
	now := m.deps.Clock.Now()
	if m.endState() != StateWorking || !open || !start.Equal(ev.TurnStart) || !start.Add(ev.Bound).Before(now) {
		m.log.Info("supervise: the controller turn the keeper read open past its bound has ended, or another began; it is left alone",
			"turnStartedAt", start, "read", ev.TurnStart, "bound", ev.Bound)
		return nil
	}
	m.log.Warn("supervise: the controller's turn stayed open past its bound; the daemon ends the launch, and the task it was serving stays owed",
		"turnStartedAt", start, "bound", ev.Bound, "open", now.Sub(start).Round(time.Second))
	// The end is counted before the replacement, because the launch the replacement makes is the
	// one that reads the count (launchNow, replaceEndedSession), and a second end in a row must make
	// that launch fresh. A replacement that ended nothing — a take-back the store refused, a launch
	// the lifecycle refused (EndNotMade) — leaves the same turn open with the process it found, and
	// the keeper's next look posts it again, so its count is taken back: one turn is one end.
	counted, ended := m.claim.SessionRun, m.claim.LastTurnEnd
	reached := m.countEnd(fmt.Sprintf("its turn stayed open longer than %s, from %s", ev.Bound, start.UTC().Format(time.RFC3339)))
	// The session, its endings and the session it replaced as this end leaves them, for the Error
	// line written once the end stands: the relaunch the end makes may start a fresh session, which
	// clears the first two and records this session as the one left behind.
	session, endings, replaced := m.claim.Session, m.claim.SessionRun.Endings, m.claim.Abandoned.Session
	// The end closes the turn (Claim.LastTurnEnd), as a death's does (chargeDeath): a death of the
	// relaunched agent is then judged on the turn it finds rather than charged against this one
	// (turnOpen).
	m.claim.LastTurnEnd = now
	end, err := m.replaceProcess(ctx, "the daemon ended a controller turn that stayed open past its bound", ordinaryEnd, endedPastBound(ev.Bound))
	switch {
	case end == EndNotMade:
		// The take-back can have written the claim with the end counted and the turn closed before
		// the lifecycle refused the launch, which writes nothing for a claim that still records its
		// process (relaunchRefused). The restore is written too, or a daemon restarted before the
		// keeper's next look would read the turn closed and never end it.
		m.claim.SessionRun, m.claim.LastTurnEnd = counted, ended
		err = errors.Join(err, m.persist(ctx))
	case reached:
		// Written once the end stands, so a take-back writes no second line for one end.
		m.reportNotReplaced(session, endings, replaced)
	}
	m.log.Info("supervise: the end of the controller turn open past its bound", "end", end)
	return err
}

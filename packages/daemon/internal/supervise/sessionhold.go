package supervise

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/wait"
)

// errHoldUnanswered is a hold the store did not answer (holdRecorded): it says nothing about the
// claim or its session, so it refuses nothing and is charged to no budget.
var errHoldUnanswered = errors.New("the store did not answer the session's hold")

// MaxHeldNamesPerClaim is the most names of sessions one claim may hold (holdRecorded). Every
// registration of a new session, and every re-registration naming a new file, holds its names,
// and a hold ends only by an operator's release (ReleaseNames), so without a bound one role could
// grow claim_sessions without end and make every boot that reads it slower (review N2 of #4220).
//
// The number: 10× what the busiest claim in production could hold, rounded up to a power of two.
// Over 28 days of the daemons' `supervise: launched` lines (2026-10-20 to 2026-11-16: 11,391
// claims, 36,345 launches), the most fresh sessions one claim launched was 42
// (a planner); a resumed launch registers the session and file it recorded,
// which holds nothing new; and the launched controllers re-registered at most 5 sessions. A session
// gives at most 4 names (RecordedHolds), so that claim holds at most 42 × 4 + 8 (the two sessions
// the migration held for it) = 176 names, and 10× is 1,760. Even the most-launched claim, at
// generation 193 (an architect), would hold 193 × 4 + 8 = 780 if every launch
// had started a fresh session. The average is about 4.5.
const MaxHeldNamesPerClaim = 2048

// ErrHeldNamesCap is the store's answer to a hold that would take its claim past
// MaxHeldNamesPerClaim (Store.HoldSessions): it records none of the call's names, and the claim
// keeps every name it holds.
var ErrHeldNamesCap = errors.New("the claim would hold more session names than one claim may")

// heldNamesCapLine is the Warn line of a registration refused at MaxHeldNamesPerClaim. Its
// `release` field is the operator's command that frees the names of the claim's other sessions
// (ReleaseNamesCommand, Machine.ReleaseNames); an operator reads the line whole, so it is pinned
// here.
const heldNamesCapLine = "supervise: refused a session whose names would take the claim past the most one claim may hold; a session it records still resumes, and the release command frees the names of its other sessions for a new one once the secrets broker carries nothing of them"

// ReleaseNamesCommand is the operator's command that runs token's claim's release
// (Machine.ReleaseNames), as the cap's log line gives it.
func ReleaseNamesCommand(token claim.Token) string {
	return "legion claims release-names --claim " + string(token) + " --operator-token-file <the daemon's operator_token_file>"
}

// ReleasedNames is what a release freed (Machine.ReleaseNames): the names it deleted, how it knew
// no grant follows one of them, and the session the claim last abandoned whose names it freed,
// which the claim's record now marks released.
type ReleasedNames struct {
	Freed []Hold
	// Checked is CheckedByBroker when the secrets broker answered that it carries nothing of the
	// names, CheckedNoBroker when this daemon has no broker configured to ask, and CheckedNothing
	// when the release had no name to free and asked nobody.
	Checked   string
	Abandoned string
}

const (
	// CheckedByBroker is a release the secrets broker answered: no enrollment this daemon's login
	// launched carries anything for a freed name.
	CheckedByBroker = "the secrets broker carries nothing of them"
	// CheckedNoBroker is a release by a daemon with no secrets broker configured (no agent_secrets
	// block): no broker was asked. It says that and no more, since a daemon that enrolled pods
	// while the block was on may still have enrollments that carry grants (ReleaseNames).
	CheckedNoBroker = "this daemon has no secrets broker configured, so no broker was asked"
	// CheckedNothing is a release that had no name to free, so it asked no broker; it may still
	// have marked the abandoned session released (ReleaseNames).
	CheckedNothing = "it held no name to free, so no broker was asked"
)

// ErrCarryUnanswered is a release the secrets broker did not answer, or answered with an outage
// (a 5xx, a 408 or a 429) or another refusal a retry can change: the release frees nothing, since
// nothing else can say that no grant follows a name it would free. A run again can change it.
var ErrCarryUnanswered = errors.New("the secrets broker did not answer which of the names it still carries")

// ErrCarryRefused is a release whose question the secrets broker refused for good (a
// PermanentError, a 4xx other than a 408 or a 429): the release frees nothing, and a run again
// answers the same until the refusal is fixed, but for a broker from before the question, which
// answers it 404 or 405 and may be one replica of several while the broker rolls.
var ErrCarryRefused = errors.New("the secrets broker refused the question of which of the names it still carries")

// ErrReleaseUnanswered is a release the secrets broker did not answer that it released the claim's
// session names (Registrar.ReleasePodSessions) at any attempt: no answer, a 5xx, a 408, a 429, or a
// 404 or 405 from a broker of a build before the release route, which may be one replica of several
// while the broker rolls. The release frees nothing, since a registration of this daemon's may
// still admit its pod's trade under a name it frees. A run again can change it.
var ErrReleaseUnanswered = errors.New("the secrets broker did not answer that it released the claim's session names")

// ErrReleaseRefused is a release whose session names the secrets broker refused for good to
// release (a PermanentError that is no RouteUnservedError: any 4xx but a 404, a 405, a 408 and a
// 429): the release frees nothing, and a run again answers the same until the refusal is fixed.
var ErrReleaseRefused = errors.New("the secrets broker refused to release the claim's session names")

// RouteUnservedError is a broker answer that it serves no such route (cipher's
// APIError.RouteUnserved, a 404 or a 405): a broker of a build before the route, which may be one
// replica of several while the broker rolls, so a release reads it as no answer, never as a
// refusal for good.
type RouteUnservedError interface{ RouteUnserved() bool }

// releaseAttempts is how many times a release asks the secrets broker to release the claim's
// session names (releaseSessions) before it refuses: the second a second after the first, the
// third two after the second, all inside the release's RPC bound.
const releaseAttempts = 3

// UnlistedCarryError is an answer of the secrets broker's too long for the daemon to read whole
// (cipher's CarriedAnswerTooLargeError): the broker answered, and an answer that long names
// at least one enrollment, so a release reads it as a carry it cannot list (NamesCarriedError's
// Unlisted), never as an outage (side review of #4355).
type UnlistedCarryError interface{ UnlistedCarry() bool }

// NamesCarriedError refuses a release: the secrets broker still carries something for some of the
// names it would free, under an enrollment this daemon's login launched, so the next enrollment of
// that name by any claim of this daemon would take it (envoy's adopt). Carried is each such
// enrollment, as the broker answered it; Unlisted, when set, is the broker's answer that was too
// long to list (UnlistedCarryError), and Carried is then empty. The release frees nothing.
// ReleaseNames returns it inside a RefusedError, which the operator's route answers 409.
type NamesCarriedError struct {
	Claim    claim.Token
	Carried  []CarriedEnrollment
	Unlisted error
}

func (e *NamesCarriedError) Error() string {
	if e.Unlisted != nil {
		return fmt.Sprintf("the secrets broker still carries grants or requests for names the release of %s would free, so it frees none: %v; run it again once fewer of them carry, or each has ended",
			e.Claim, e.Unlisted)
	}
	parts := make([]string, len(e.Carried))
	for i, c := range e.Carried {
		switch c.State {
		case CarriedLive:
			parts[i] = fmt.Sprintf("%s (enrollment %s, live: its lease ends at %s unless its process renews it)", c.SessionID, c.EnrollmentID, c.EndsAt.UTC().Format(time.RFC3339))
		default:
			parts[i] = fmt.Sprintf("%s (enrollment %s, %s: its carry ends at %s)", c.SessionID, c.EnrollmentID, c.State, c.EndsAt.UTC().Format(time.RFC3339))
		}
	}
	return fmt.Sprintf("the secrets broker still carries grants or requests for %d of the names the release of %s would free, so it frees none: %s; run it again once each has ended",
		len(e.Carried), e.Claim, strings.Join(parts, "; "))
}

// ReleaseNames is the operator's release (`legion claims release-names`): it frees every name the
// claim holds except those of the session it records now, which it still resumes, so a claim at
// MaxHeldNamesPerClaim can start a new session. A name of the current session is its id, a name
// containing that id, or a part of its recorded file's path (the names RecordedHolds gives it, and
// an earlier file of it named for its id); and so is the session id the claim's live pod-session
// registration names, the id a launch chose before its agent registered.
//
// It frees them only when nothing can follow a freed name to a grant. The daemon enrolls a
// session's process with the secrets broker by its session id, and the broker hands what an ended
// enrollment still carries — its live grants and its pending requests — to the next enrollment of
// the same session id that this daemon's login launches (envoy's adopt); another claim registering
// a freed name would be that enrollment. So the release first asks the broker which of the names
// it would free name an enrollment of this daemon's that is live or still carries something
// (Enroller.Carried), and any answer refuses the whole release with a RefusedError holding a
// NamesCarriedError, naming each enrollment and when its lease or its carry ends, or saying the
// answer was too long to list (UnlistedCarryError); a broker that does not answer, or answers with
// an outage, refuses it with ErrCarryUnanswered, and one that refuses the question for good with
// ErrCarryRefused. Nothing is freed in any of them. The broker's own rows answer every case a
// time window only estimated: a revoke the daemon missed, a lease a broker outage held live, a
// sweep that ran late or kept failing, a longer BROKER_SESSION_CARRY_SECONDS.
//
// Nor can a pod trade under a name it frees. The broker honours a pod-session registration of this
// daemon's (Deps.Registrar, exchange_audience) for a day past its last write, whether or not its
// renewal has stopped, so a pod still holding a token for its old session could trade under a name
// another claim then registers. So a daemon with a registrar has the broker release the session
// names among those it frees, after the carry check and before the commit (releaseSessions):
// nothing is freed until the broker answers that every one is released, and a broker that answers
// so at no attempt refuses the release with ErrReleaseUnanswered, or with ErrReleaseRefused when
// its last answer is a refusal for good. No renewal writes such a name by then, which would make
// its registration live again: the live registration's session is current and kept, an earlier
// registration's renewal was stopped when the claim moved off it or let its process go
// (ensureRegistered, stopRegistration), and that stop waited for its write in flight. Without a
// registrar no registration exists to release, and none is asked.
//
// A daemon with no broker configured (Deps.Secrets nil) asks none about a carry, frees with no
// check, and says so (CheckedNoBroker). That is safe only for a daemon that never enrolled a pod
// with a broker. A pod enrolled while agent_secrets was on keeps an enrollment its own shim renews
// for as long as it runs, which no daemon revokes once the block is off (RevokeEnrollment leaves
// it to its lease), and a stopped one carries its grants for BROKER_SESSION_CARRY_SECONDS from its
// lease's lapse. No bound an operator can work out covers a pod that still runs, so such a
// daemon's names are released only with agent_secrets on (docs/kubernetes.md; review of #4355).
//
// A release with no name to free asks no broker (CheckedNothing), and still marks an abandoned
// session the record does not yet mark released. Every name of that session is freed or is a name
// of the current session by then, so the mark frees nothing; it repairs a mark that a release's
// commit made and its lost answer kept out of memory, which the claim's next write then erased
// (review of #4355).
//
// It runs under the machine's lock, so no decision of this claim — a registration of one of the
// names among them — falls between the broker's answer and the delete, and no other claim can take
// a name before the delete, since the table holds it for this one. So nothing between the answer
// and the commit can give a name it frees an enrollment that carries anything: the daemon enrolls
// a session only for a claim the table holds every name of it for (holdRecorded comes before
// every enrollment), which for a name this release frees is this claim alone until the commit,
// and this claim's decisions wait on this lock.
//
// The delete and the claim write are one transaction (Store.ReleaseHolds): the session the claim
// last abandoned has its names freed with the rest, and its record is marked released
// (AbandonedSession.Released) in that same transaction, so the boot holds none of its names again
// (store's recordedNames) and the claim keeps the record of where that session went. The two
// commit together or not at all: a daemon that stopped between them would leave names other
// claims may take while the next boot took back for this claim every one nobody took. The mark
// joins memory's claim only once the transaction commits, so a refused write leaves the claim as
// it was. The names it would free leave the machine's cache (heldNames) whatever the write
// answers: a commit whose answer was lost freed them all the same, and a name left cached would
// let this claim register it again without asking the store, after another claim took it
// (review of #4355). A name dropped from the cache that the store still holds costs one store
// call at the claim's next registration of it.
//
// The broker is asked before that transaction, outside it, and never after it. No transaction
// or row lock is held while the broker answers, so a slow broker holds only this claim's lock, as
// every other call the machine makes to the broker does (Enroll, Revoke). And no failure of the
// broker can follow a commit: a broker that fails, still carries something or releases nothing
// leaves nothing committed, and a commit that fails after the broker's answers may or may not
// have freed the names, which the operator's next run, asking the broker again, settles either
// way; a release of names the broker already released answers the same again. The commit is the
// release's last step, so a release that committed is whole.
//
// A claim that never switched sessions and needs a new one is given one by forget-session, and
// this then frees the forgotten session's names once the broker's carry of it ends: no second
// switch is needed.
func (m *Machine) ReleaseNames(ctx context.Context) (ReleasedNames, error) {
	m.mu.Lock()
	defer m.unlock()
	// The read is bounded as a claim write is (bounded), as HoldsItsSession bounds its own, so a
	// database that has stopped answering does not hold the machine's lock for the outage.
	reading, cancel := m.bounded(ctx)
	holds, err := m.deps.Store.ClaimHolds(reading, m.claim.Project, m.claim.Token)
	cancel()
	if err != nil {
		return ReleasedNames{}, err
	}
	free := releasable(holds, m.claim.Session, m.claim.SessionFile)
	// The session the claim's live pod-session registration names is current too: before its
	// agent registers, a launch's chosen session id is held and registered for the pod to trade
	// under (ensureRegistered) while the claim records no session yet (review of #4355).
	if r := m.registration; r != nil && r.session != m.claim.Session {
		free = releasable(free, r.session, "")
	}
	a := m.claim.Abandoned
	// An abandoned record of the session the claim records again (a relaunch that found no file
	// abandoned it, then its agent registered it again), or of the one its live registration
	// names, is a current session: none of its names is freed, so the release neither reports it
	// nor marks it released (acceptance of #4220).
	current := a.Session == m.claim.Session || m.registration != nil && a.Session == m.registration.session
	mark := a.Session != "" && !current && a.Released.IsZero()
	released := ReleasedNames{Checked: CheckedNothing}
	switch {
	case len(free) == 0:
		if !mark {
			return released, nil
		}
	case m.deps.Secrets == nil:
		released.Checked = CheckedNoBroker
	default:
		names := make([]string, len(free))
		for i, h := range free {
			names[i] = h.Name
		}
		slices.Sort(names)
		names = slices.Compact(names)
		asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
		carried, err := m.deps.Secrets.Carried(asking, names)
		cancel()
		var unlisted UnlistedCarryError
		var permanent PermanentError
		switch {
		case errors.As(err, &unlisted) && unlisted.UnlistedCarry():
			return ReleasedNames{}, m.refuseCarried(&NamesCarriedError{Claim: m.claim.Token, Unlisted: err}, len(free))
		case errors.As(err, &permanent) && permanent.Permanent():
			return ReleasedNames{}, fmt.Errorf("release the names of %s: %w: %w", m.claim.Token, ErrCarryRefused, err)
		case err != nil:
			return ReleasedNames{}, fmt.Errorf("release the names of %s: %w: %w", m.claim.Token, ErrCarryUnanswered, err)
		case len(carried) > 0:
			return ReleasedNames{}, m.refuseCarried(&NamesCarriedError{Claim: m.claim.Token, Carried: carried}, len(free))
		}
		released.Checked = CheckedByBroker
	}
	if m.deps.Registrar != nil {
		if err := m.releaseSessions(ctx, free); err != nil {
			return ReleasedNames{}, err
		}
	}
	// The release writes the claim row alone, so a take-back the claim must carry goes first
	// (carryPending, writeCarried).
	if err := m.writeCarried(ctx); err != nil {
		return ReleasedNames{}, err
	}
	c := m.stored()
	if mark {
		c.Abandoned.Released, released.Abandoned = m.deps.Clock.Now(), a.Session
	}
	// The exception to write's retry (Machine.write): made once under its bound, since a second try
	// after a cut whose first try freed the names would free none and answer an empty Freed as a
	// success, losing the names the first try freed.
	releasing, cancel := m.bounded(ctx)
	released.Freed, err = m.deps.Store.ReleaseHolds(releasing, c, free)
	cancel()
	// The names leave the cache whatever the write answered (the doc above).
	for _, h := range free {
		delete(m.heldNames, h)
	}
	if err != nil {
		return ReleasedNames{}, err
	}
	// The write was the claim as memory holds it, the mark aside, so it is also any claim write
	// the machine owed (settleOwedWrite).
	m.claim.Abandoned.Released = c.Abandoned.Released
	m.settleOwedWrite("ReleaseHolds")
	m.log.Info("supervise: released the names of a claim's other sessions",
		"freed", len(released.Freed), "checked", released.Checked, "abandoned", released.Abandoned)
	return released, nil
}

// refuseCarried is a release refused because the secrets broker still carries something for some
// of the names it would free, names of them: logged at Warn, and answered as a RefusedError, which
// the operator's route answers 409 (operationFailure).
func (m *Machine) refuseCarried(refusal *NamesCarriedError, names int) error {
	m.log.Warn("supervise: refused a release of a claim's names: the secrets broker still carries some of them",
		"names", names, "carried", len(refusal.Carried), "error", refusal.Error())
	return &RefusedError{State: m.claim.State, Request: "release-names", Err: refusal}
}

// releaseSessions has the secrets broker release the session names among free
// (Registrar.ReleasePodSessions) before the release frees any name (ReleaseNames): names of kind
// HoldSessionID alone, the one kind a pod-session registration names, and none when free holds
// none. A broker that releases every name answers nil. The call is tried releaseAttempts times, a
// second and then two apart, all inside one RPC bound, and each failure is logged at Warn with the
// claim, the names' count, the attempt and the broker's reason. After the last failure it answers
// ErrReleaseRefused for a refusal a retry cannot change (a PermanentError that is no
// RouteUnservedError) and ErrReleaseUnanswered for any other, each wrapping that failure.
func (m *Machine) releaseSessions(ctx context.Context, free []Hold) error {
	var names []string
	for _, h := range free {
		if h.Kind == HoldSessionID {
			names = append(names, h.Name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	releasing, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	defer cancel()
	var err error
	for attempt := 1; ; attempt++ {
		if err = m.deps.Registrar.ReleasePodSessions(releasing, names); err == nil {
			return nil
		}
		m.log.Warn("supervise: the secrets broker did not release a claim's session names; they stay held",
			"names", len(names), "attempt", attempt, "error", err)
		if attempt == releaseAttempts || m.deps.Clock.Sleep(releasing, time.Duration(attempt)*time.Second) != nil {
			break
		}
	}
	var permanent PermanentError
	var unserved RouteUnservedError
	if errors.As(err, &permanent) && permanent.Permanent() && !(errors.As(err, &unserved) && unserved.RouteUnserved()) {
		return fmt.Errorf("release the names of %s: %w: %w", m.claim.Token, ErrReleaseRefused, err)
	}
	return fmt.Errorf("release the names of %s: %w: %w", m.claim.Token, ErrReleaseUnanswered, err)
}

// releasable is every one of holds that is not a name of the session a claim records, session and
// sessionFile (ReleaseNames): its id, a name containing the id, or a part of the file's path. In
// holds' order.
func releasable(holds []Hold, session, sessionFile string) []Hold {
	var free []Hold
	for _, h := range holds {
		current := session != "" && strings.Contains(h.Name, session) || sessionFile != "" && strings.Contains(sessionFile, h.Name)
		if !current {
			free = append(free, h)
		}
	}
	return free
}

// SQLLiteral is s as a PostgreSQL escape string literal (E'…'): each backslash and each quote
// doubled, so no text of s ends the literal. An escape string reads its backslashes as escapes
// whatever standard_conforming_strings the session sets, so the literal means s under both
// settings; a plain '…' literal with only its quotes doubled means s only with the setting on,
// and with it off a name such as `\' or true --` ends the literal and turns the statement's
// guard into a comment (review of round 11 of #4220, F1). The statement the daemon's boot prints
// for an operator to settle a name held for no claim writes every name an agent chose, and every
// other value, this way (internal/daemon's session conflicts); the daemon runs no statement it
// builds so, and binds its own statements' values as parameters.
func SQLLiteral(s string) string {
	return "E'" + strings.NewReplacer(`\`, `\\`, `'`, `''`).Replace(s) + "'"
}

// HoldKind is which of a session's names a hold keeps (Hold): each is a key some resume path
// finds a session by.
type HoldKind string

const (
	// HoldSessionID is a session id: the one an agent registers, which the secrets broker enrolls
	// as its wake address (ensureEnrolled), the one a Claude Code launch resumes, `--resume <id>`,
	// read from its transcript's name (claudelaunch.ResumeSession), and the one an Oh My Pi launch
	// resumes the claim's recorded session by (runtime/sandbox's ompSessionOf and agentArgv): the
	// one word `--resume=<id>`, in any pod, where the claim records no file of the session, and
	// `--resume <id>` beside a recorded file, in a pod that trades an exchange identity.
	HoldSessionID HoldKind = "session"
	// HoldFileName is a session file's base name (filepath.Base). It is every key a resume finds
	// a session by its file with: an Oh My Pi launch that names no id resumes the recorded path
	// (`--resume=<file>`, runtime/sandbox's agentArgv and the tmux runtime's launcher), the launcher
	// finds the session's row in the session table by that path (launcher's resumable and
	// claudeKeeper), and where the path is missing the tmux runtime resumes a file of the same name
	// one folder below a sessions directory (sessionFileOf). Two paths that are one have one name,
	// so holding the name holds the path too: no hold of the path alone is kept, since it could
	// refuse nothing the name's hold does not.
	HoldFileName HoldKind = "file-name"
	// HoldFileID is the session id an Oh My Pi file name `<timestamp>_<id>.jsonl` gives, the part
	// after its first `_`: the tmux runtime also resumes any `*_<id>.jsonl` there (newestSession),
	// and an Oh My Pi launch in a pod that trades resumes that id, `--resume <id>`, for a claim that
	// recorded no session id (runtime/sandbox's ompSessionOf, which reads it through RecordedHolds).
	HoldFileID HoldKind = "file-id"
)

// Hold is one name of a session, held to the first claim of its project that recorded it
// (holdRecorded).
type Hold struct {
	Kind HoldKind
	Name string
}

// CompareHolds orders holds as the store answers them: kind, then name, byte for byte (the
// `collate "C"` order ClaimHolds reads in).
func CompareHolds(a, b Hold) int {
	return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
}

// RecordedHolds is every name a claim's record of session and sessionFile gives its session, each
// a key some resume path or the secrets broker finds a session by (HoldKind), so that no claim
// registers, relaunches into or enrolls another claim's session by any of them: the session id;
// the file's base name, which holds its path too (HoldFileName); the id its Oh My Pi name
// `<timestamp>_<id>.jsonl` gives; and the Claude Code session a file named `<uuid>.jsonl` gives,
// where that is not the session id itself. The last is wider than a Claude Code resume, which also
// asks for a `projects` directory two levels up (claudelaunch.Transcript): it refuses more, never
// less, and no Oh My Pi file is named for a bare uuid. An empty session or file names nothing, and
// a session id or file name longer than claim.MaxSessionNameBytes names nothing either: a
// registration of one is refused (checkSessionNames), and holdRecorded refuses a claim whose
// record an earlier daemon wrote with one (UnholdableNames), so nothing is held for it and nothing
// takes it. Nor does a file whose last part is `.` or `..` name a file: its base name is not the
// file's, and holding it would hold, for no claim where two records end so, a name whose settle
// statement the boot would print (review round 12 of #4220); a registration of such a file is
// refused as not clean, and holdRecorded refuses a claim recording one. The claim_sessions
// migration's backfill spells the rule in SQL, and its test holds the two to one answer; it holds
// such a base name, the one difference, which no clean path gives and no claim's names include.
func RecordedHolds(session, sessionFile string) []Hold {
	var holds []Hold
	if session != "" && len(session) <= claim.MaxSessionNameBytes {
		holds = append(holds, Hold{Kind: HoldSessionID, Name: session})
	}
	if sessionFile == "" {
		return holds
	}
	base := filepath.Base(sessionFile)
	if len(base) > claim.MaxSessionNameBytes || base == "." || base == ".." {
		return holds
	}
	holds = append(holds, Hold{Kind: HoldFileName, Name: base})
	if stem, ok := strings.CutSuffix(base, ".jsonl"); ok {
		if _, id, ok := strings.Cut(stem, "_"); ok && id != "" {
			holds = append(holds, Hold{Kind: HoldFileID, Name: id})
		}
		if claudelaunch.IsSessionID(stem) && stem != session {
			holds = append(holds, Hold{Kind: HoldSessionID, Name: stem})
		}
	}
	return holds
}

// byIDHolds is every name a resume of session by its id alone runs under (Machine.start, for a
// claim that records a session and no file): the session id, and the id an Oh My Pi file
// `<timestamp>_<id>.jsonl` gives (HoldFileID), since that resume continues the file whose name
// ends in `_<id>.jsonl` — the newest such row of the launcher's session table or, under
// runtime.tmux.session_store postgres, of the tmux runtime's, the newest such file one folder
// below the tmux pane's sessions directory, or the file Oh My Pi's own `--resume=<id>` finds. The
// file's base name is not known before the agent registers it, and that registration holds it
// with the rest (register, RecordedHolds). RecordedHolds(session, "") gives the session id alone,
// which is all its callers need: each acts on a registered process or its session, and a claim
// with no file has no registered process. An empty session, or one past
// claim.MaxSessionNameBytes, names nothing, as in RecordedHolds.
func byIDHolds(session string) []Hold {
	if session == "" || len(session) > claim.MaxSessionNameBytes {
		return nil
	}
	return []Hold{{Kind: HoldSessionID, Name: session}, {Kind: HoldFileID, Name: session}}
}

// invalidName is one name of a session that no hold can cover, with what makes it one (why, as a
// refusal's error and log line say it) and the refusal a registration of it answers.
type invalidName struct {
	hold    Hold
	why     string
	refusal claim.Refusal
}

// invalidSessionNames is the one rule for a session name no hold can cover, in the order a
// registration is refused by them: a session id longer than claim.MaxSessionNameBytes; a session
// file that is not the clean path it names, since a last part of `.` or `..` gives a base name that
// is not the file's, so the file-name hold would hold another name, and a doubled or trailing `/` is
// refused too, so that one file has one recorded spelling; and a file whose base name is longer than
// the bound. A registration is refused by the first (checkSessionNames); a claim a daemon from before
// the hold recorded under any of them takes nothing under it (UnholdableNames).
func invalidSessionNames(session, sessionFile string) []invalidName {
	var invalid []invalidName
	if len(session) > claim.MaxSessionNameBytes {
		invalid = append(invalid, invalidName{Hold{Kind: HoldSessionID, Name: session}, pastTheBound(len(session)), claim.SessionNameTooLong("session id", len(session))})
	}
	if sessionFile == "" {
		return invalid
	}
	if filepath.Clean(sessionFile) != sessionFile {
		return append(invalid, invalidName{Hold{Kind: HoldFileName, Name: sessionFile}, "that is not a clean path", claim.SessionFileNotClean(sessionFile)})
	}
	if base := filepath.Base(sessionFile); len(base) > claim.MaxSessionNameBytes {
		invalid = append(invalid, invalidName{Hold{Kind: HoldFileName, Name: base}, pastTheBound(len(base)), claim.SessionNameTooLong("session file's name", len(base))})
	}
	return invalid
}

// pastTheBound is why a name of n bytes is one no hold can cover, as invalidName says it.
func pastTheBound(n int) string {
	return fmt.Sprintf("of %d bytes, past the bound of %d", n, claim.MaxSessionNameBytes)
}

// checkSessionNames refuses a registration of a session name no hold can cover, with the first
// refusal invalidSessionNames gives: no real session has one.
func checkSessionNames(session, sessionFile string) error {
	if invalid := invalidSessionNames(session, sessionFile); len(invalid) > 0 {
		return invalid[0].refusal
	}
	return nil
}

// UnholdableNames is every name of a claim's record of session and sessionFile that no hold can
// cover (invalidSessionNames), which checkSessionNames refuses at registration, so only a daemon
// from before the hold recorded one. Each is treated as held for no claim: holdRecorded refuses the
// claim that records it, and the daemon's boot names it and suspends that claim
// (store.SessionConflicts).
func UnholdableNames(session, sessionFile string) []Hold {
	invalid := invalidSessionNames(session, sessionFile)
	names := make([]Hold, len(invalid))
	for i, n := range invalid {
		names[i] = n.hold
	}
	return names
}

// holdRecorded holds every name a claim's record gives its session (RecordedHolds) to the claim,
// whatever the claim holding one became since: queued, working, idle, suspended, failed or retired
// (plan E's t-e-54). The daemon enrolls the session a claim's agent reports
// with the secrets broker, whose enrollment takes over the grants and pending requests of every
// other enrollment of the same pod account and session, and a relaunch resumes the session the
// record names, its transcript read and written; every pod of a project enrolls under the
// project's one account. So the names are held before anything records, resumes or enrolls them:
// a claim may take only a name it recorded before or one no claim has recorded, and the store
// keeps which claim recorded each first (Store.HoldSessions), all of one record's names or none.
// Any other is refused with claim.SessionHeldRefusal, which names no other claim; the log names
// the holder. A name held for no claim (two claims recorded it before the hold) is refused to
// every claim, and so is a name no row can hold (UnholdableNames, claim.SessionNameUnheldRefusal),
// which only a daemon from before the hold recorded: skipped, it would be enrolled and resumed
// with no hold at all (review round 11 of #4220).
//
// A name of the current session the store answered as this claim's is kept and not asked again
// while the claim records that session: the enrollment that follows a registration asks nothing
// more, forgetSession empties the cache (Machine.heldNames), and an operator's release takes out
// of it every name it frees (ReleaseNames). A refusal is asked again every time, because an
// operator settles a name held for no claim by recording its rightful holder, and that claim's
// next decision on this same machine must see it. A hold that would take the claim past
// MaxHeldNamesPerClaim is refused with claim.HeldNamesCapRefusal and records nothing; one of names
// the claim holds already adds none, so a claim that records a session still resumes it, while
// one that needs a new session starts none until an operator runs the command the line gives
// (ReleaseNamesCommand). A store that does not answer refuses nothing: its error wraps
// errHoldUnanswered, which a launch tries again at the probe interval charging nothing
// (launchTries), and any other decision is asked again by the next one.
func (m *Machine) holdRecorded(ctx context.Context, session, sessionFile string) error {
	return m.holdNames(ctx, session, sessionFile, RecordedHolds(session, sessionFile))
}

// holdByID is holdRecorded for a resume of session by its id alone, under the same rule: it holds
// the names that resume runs under (byIDHolds), the file-id name beside the session id. start asks
// it before such a resume, which only an Oh My Pi claim makes: start sends a Claude Code claim with
// a session and no file straight to the forget (startFresh) and asks no hold. resumeRefusal asks it
// of every claim that records a session and no file, a Claude Code claim's included, which is
// stricter than that claim's launch: an operator's resume of such a claim whose by-id names another
// claim holds is refused, though its launch would forget the session rather than resume it.
func (m *Machine) holdByID(ctx context.Context, session string) error {
	return m.holdNames(ctx, session, "", byIDHolds(session))
}

// holdNames is the hold holdRecorded and holdByID make: names are the names of session and
// sessionFile that the caller's resume or record runs under, and each is held to the claim, or the
// call is refused, by the rule holdRecorded gives. The session and file it admits are the ones
// HoldsItsSession and Admits read (Machine.admitted).
func (m *Machine) holdNames(ctx context.Context, session, sessionFile string, names []Hold) error {
	if invalid := invalidSessionNames(session, sessionFile); len(invalid) > 0 {
		n := invalid[0]
		what := "session id"
		if n.hold.Kind == HoldFileName {
			what = "session file"
		}
		m.warnRefusal("supervise: refused a session whose name this daemon cannot hold",
			"kind", n.hold.Kind, "name", n.hold.Name, "bytes", len(n.hold.Name), "why", n.why, "holder", "none")
		return fmt.Errorf("%s as %s %s: %w", m.claim.Token, what, n.why, claim.SessionNameUnheldRefusal(what))
	}
	var ask []Hold
	for _, h := range names {
		if !m.heldNames[h] {
			ask = append(ask, h)
		}
	}
	if len(ask) == 0 {
		m.refusalLogged = ""
		m.admitted.Store(&admittedSession{session, sessionFile})
		return nil
	}
	var holders []claim.Token
	if err := m.write(ctx, func(ctx context.Context) error {
		var err error
		holders, err = m.deps.Store.HoldSessions(ctx, m.claim.Project, m.claim.Token, ask)
		return err
	}); errors.Is(err, ErrHeldNamesCap) {
		m.log.Warn(heldNamesCapLine,
			"names", len(ask), "cap", MaxHeldNamesPerClaim, "error", err,
			"release", ReleaseNamesCommand(m.claim.Token))
		return fmt.Errorf("hold the session of %s: %w", m.claim.Token, claim.HeldNamesCapRefusal(MaxHeldNamesPerClaim))
	} else if err != nil {
		return fmt.Errorf("hold the session of %s: %w: %w", m.claim.Token, errHoldUnanswered, err)
	}
	if len(holders) != len(ask) {
		return fmt.Errorf("hold the session of %s: %w: it answered %d holders for %d names", m.claim.Token, errHoldUnanswered, len(holders), len(ask))
	}
	if h, holder, refused := firstHeldElsewhere(ask, holders, m.claim.Token); refused {
		named := string(holder)
		if named == "" {
			named = "none"
		}
		m.warnRefusal("supervise: refused a session another claim recorded first",
			"kind", h.Kind, "name", h.Name, "holder", named)
		return &heldElsewhereError{hold: h, holder: holder, err: fmt.Errorf("%s as %s %s: %w", m.claim.Token, h.Kind, h.Name, claim.SessionHeldRefusal(h.Name))}
	}
	if m.heldNames == nil {
		m.heldNames = map[Hold]bool{}
	}
	for _, h := range ask {
		m.heldNames[h] = true
	}
	m.refusalLogged = ""
	m.admitted.Store(&admittedSession{session, sessionFile})
	return nil
}

// firstHeldElsewhere is the first of holds, whose holders the store answered in order
// (Store.HoldSessions), that is held for another claim than token or for none, with its holder;
// refused is false when every one is token's.
func firstHeldElsewhere(holds []Hold, holders []claim.Token, token claim.Token) (h Hold, holder claim.Token, refused bool) {
	for i, h := range holds {
		if holders[i] != token {
			return h, holders[i], true
		}
	}
	return Hold{}, "", false
}

// heldElsewhereError is holdRecorded's refusal of a name held for another claim, or for none
// (holder empty), carrying that name and its holder for a caller that names the holder to an
// operator (mayForget). Its text and the claim.Refusal it wraps are the refusal's own, so a
// registration answers exactly what it answered before.
type heldElsewhereError struct {
	hold   Hold
	holder claim.Token
	err    error
}

func (e *heldElsewhereError) Error() string { return e.err.Error() }
func (e *heldElsewhereError) Unwrap() error { return e.err }

// IsSessionHeldElsewhere is whether err, anywhere in its chain, is holdRecorded's refusal of a
// name held for another claim or for none (claim.SessionHeldRefusal, carried by
// heldElsewhereError), whichever of the session's names was refused: its id, its file's name, or
// an id the file's name gives. It matches the refusal by its kind, not by its value or its status:
// the two other refusals of a hold, claim.SessionNameUnheldRefusal and claim.HeldNamesCapRefusal,
// are 409s of the same claim.Refusal type, and neither is this. The controller keeper reads it to
// ask for a new session when a retry's resume of the recorded session is refused for that alone
// (internal/daemon's controllerKeeper.keep).
func IsSessionHeldElsewhere(err error) bool {
	var held *heldElsewhereError
	return errors.As(err, &held)
}

// mayForget is nil when the claim may clear the session it records (forgetSession, retry's fresh
// session), and otherwise why not. A claim holding its session, or recording none, forgets as
// before and asks nothing more (R9).
//
// A claim that records a session it does not hold is the one the outbox withholds its role's
// messages from (HoldsItsSession), and only while it records that session: once the session is
// cleared, the claim holds what it records, and its role topic opens. The Envoy listener still
// names the cleared session as the role's holder while its claim stands, and where the name is
// held for another claim, as after a rollback, that claim's live process keeps the claim
// standing: the role claim reaper keeps a claim whose holder's key the session bucket still
// holds. The role's messages would then reach that process (review round 16 of #4220). So before
// such a claim clears its session, the listener is asked once who holds the claim's role, and the
// clear is refused while the listener names the cleared session, in any state: a holder it
// reports unresponsive takes the role's messages again once its heartbeats land, and one it
// reports gone still holds the claim until the reaper's next pass, so a process registering the
// session again in between, the rightful holder's resumed or a stalled one, keeps the role
// (review round 17). The clear goes on only once the listener names no holder, or another
// session. A listener that does not answer refuses the clear: nothing says the route is closed.
// The remedy the refusal names is to stop the process under that session first, through the
// daemon, since a pod or pane stopped by hand is relaunched under the same id (Machine.died).
//
// The lookup is one bounded call (Timeouts.RPC) inside the decision, on a claim with no process,
// so nothing else of this claim waits behind it but whoever asked: an operator's forget or
// retry, the controller keeper's retry, or a workflow start row's (review round 16 of #4220, #66).
func (m *Machine) mayForget(ctx context.Context, request string) error {
	session := m.claim.Session
	if session == "" {
		return nil
	}
	err := m.holdRecorded(ctx, session, m.claim.SessionFile)
	if err == nil {
		return nil
	}
	var refusal claim.Refusal
	if !errors.As(err, &refusal) {
		return fmt.Errorf("%s: ask whether %s holds session %s before clearing it: %w", request, m.claim.Token, session, err)
	}
	if m.deps.RoleHolder == nil {
		return nil
	}
	asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
	holder, state, lookupErr := m.deps.RoleHolder(asking, string(m.claim.Token))
	cancel()
	if lookupErr != nil {
		// A lookup the listener refuses the same way however often it is asked is a fault an
		// operator fixes, not a wait: no stop of any process clears it (notify.RoleLookupRefused).
		if refusedLookup := (*notify.RoleLookupRefused)(nil); errors.As(lookupErr, &refusedLookup) {
			return &RefusedError{State: m.claim.State, Request: request, Err: fmt.Errorf(
				"%s records session %s, which it does not hold (%v), and the Envoy listener refuses the lookup of which session holds its role, as it will each time it is asked, so the session is not cleared until an operator fixes what the listener refuses: %w",
				m.claim.Token, session, err, lookupErr)}
		}
		return &RefusedError{State: m.claim.State, Request: request, Err: forgetWait{fmt.Errorf(
			"%s records session %s, which it does not hold (%v), and the Envoy listener did not say which session holds its role, so clearing the session could let the role's messages reach another claim's process; try again once the listener answers: %w",
			m.claim.Token, session, err, lookupErr)}}
	}
	if holder != session {
		return nil
	}
	// The claim to suspend is the one holding the session id the listener names: a refusal of
	// another name (the file's, or a transcript's id) is held by a claim whose process need not
	// run under that id, and stopping it would change nothing at the listener.
	whose := "the process registered under it"
	stop := "first stop it, through the daemon (a pod or pane stopped by hand is relaunched under the same id)"
	var held *heldElsewhereError
	if errors.As(err, &held) && held.holder != "" && held.hold == (Hold{Kind: HoldSessionID, Name: session}) {
		whose = fmt.Sprintf("the claim that holds it, %s", held.holder)
		// A holder that is suspended, retired or failed runs no process under the session, so
		// its suspend changes nothing: the process under it may be this claim's own, which only
		// the wait ends (review round 16 of #4220, #61).
		stop = fmt.Sprintf("first stop %s's process, through the daemon, if it runs (the operator's `legion claims suspend --claim %s`, or the `suspend` of the controller's `legion` tool; a pod or pane stopped by hand is relaunched under the same id); if %s is suspended, retired or failed, no process of it runs under %s, and the process under it may be this claim's own, which the wait below ends", held.holder, held.holder, held.holder, session)
	}
	return &RefusedError{State: m.claim.State, Request: request, Err: forgetWait{fmt.Errorf(
		"%s records session %s, which it does not hold (%v), and the Envoy listener names %s (%s) as the holder of its role, so with the session cleared the role's messages would reach %s: %s; then ask again once GET /v1/roles/%s answers 404 with no holder (reason unclaimed) or names another session, at most %s after that process's last registration (ten minutes with the defaults, about 65 minutes where the session bucket keeps records past the gone horizon), longer across a listener restart or a reaper read that fails",
		m.claim.Token, session, err, session, state, whose, stop, m.claim.Token, roleFreedBound)}}
}

// roleFreedBound is when the Envoy listener frees a role whose holder's session no process
// registers any longer, after that session's last registration, as the listener's settings name
// it: the record reads gone at the earlier of the session bucket's maximum age and the gone
// horizon, the role claim is kept while its renewal is a live window old, and the role claim
// reaper frees it at its next pass (review round 16 of #4220, #60). With the defaults (a bucket
// created with no maximum age keeps records five minutes) that is ten minutes; where the bucket
// keeps records past the gone horizon, an hour plus the reap interval.
const roleFreedBound = "max(min(ENVOY_SESSION_MAX_AGE, ENVOY_SESSION_GONE_HORIZON), ENVOY_SESSION_LIVE_WINDOW) + ENVOY_ROLE_CLAIM_REAP_INTERVAL"

// ErrForgetWaitsForRole is mayForget's refusal of a clear of a session the claim does not hold
// while the Envoy listener names that session as the claim's role holder, or could not be asked
// who does. It is a wait (it matches wait.ErrWaiting too), which ends when the process under that
// session stops and the listener frees the role: in the case it exists for, another claim's live
// process runs under the session, and only an operator's suspend of that claim stops it, so a
// caller that retries on a backoff of its own logs it where an operator sees it (the controller
// keeper at Warn; settle raises a waiting row to Warn from waitWarnAttempts). A lookup the listener
// refuses for good is no wait but a fault (notify.RoleLookupRefused).
var ErrForgetWaitsForRole = errors.New("the clear of the session waits for the Envoy listener to free the claim's role")

// forgetWait is a mayForget refusal's sentence, which errors.Is reads as ErrForgetWaitsForRole
// and as wait.ErrWaiting, while the refusal's text stays the sentence alone.
type forgetWait struct{ error }

func (forgetWait) Is(target error) bool {
	return target == ErrForgetWaitsForRole || target == wait.ErrWaiting
}
func (w forgetWait) Unwrap() error { return w.error }

// admittedSession is a session and file every name of which the store last answered as this
// claim's (holdRecorded, HoldsItsSession), kept for HoldsItsSession and Admits to read without
// the lock.
type admittedSession struct{ session, file string }

// Admits reports whether the store last answered every name of session and sessionFile as this
// claim's (Machine.admitted), with no lock and no store call: what a reader inside a fact's
// transaction, which holds the fact lock, may ask of the machine (the daemon's step record). False
// says only that the machine has not seen that answer, not that the claim does not hold them.
func (m *Machine) Admits(session, sessionFile string) bool {
	a := m.admitted.Load()
	return a != nil && *a == (admittedSession{session, sessionFile})
}

// HoldsItsSession is nil when every name of the session the claim records is held to it, or it
// records none, and otherwise the hold's refusal (a claim.Refusal) or the store's error. The
// daemon's outbox asks it before it publishes to the claim's Envoy role topic: a claim the boot
// could not suspend keeps its process, and that process keeps the role its session claimed, so a
// message to the role would reach a process working under another claim's session, or one no
// claim holds (review round 13 of #4220).
//
// It never takes the machine's lock, which a decision holds through its runtime call, a relaunch
// for minutes: the outbox reads claims through View for that reason (LEGION-650), and a publish
// that waited would hold its issue's lane behind the relaunch (review round 14 of #4220). It
// reads the claim from View, and answers from the session the store last admitted for the claim
// (Machine.admitted) when that is the session the view records, so a claim holding its session
// costs no store call; otherwise it asks the store itself, as holdRecorded would, and logs
// nothing: the publisher that asked says what it did not publish.
//
// Read with no lock, the view can predate a forget-session and a release that ran since, so the
// store call can hold again for this claim names that release just freed (review of #4355). That
// is the safe direction: only this claim holds them, no other claim can take them, no grant
// follows them, and the next release frees them again.
func (m *Machine) HoldsItsSession(ctx context.Context) error {
	c := m.View().Claim
	if m.Admits(c.Session, c.SessionFile) {
		return nil
	}
	if invalid := invalidSessionNames(c.Session, c.SessionFile); len(invalid) > 0 {
		what := "session id"
		if invalid[0].hold.Kind == HoldFileName {
			what = "session file"
		}
		return fmt.Errorf("%s as %s: %w", c.Token, what, claim.SessionNameUnheldRefusal(what))
	}
	holds := RecordedHolds(c.Session, c.SessionFile)
	if len(holds) > 0 {
		asking, cancel := m.bounded(ctx)
		holders, err := m.deps.Store.HoldSessions(asking, c.Project, c.Token, holds)
		cancel()
		switch {
		case errors.Is(err, ErrHeldNamesCap):
			return fmt.Errorf("hold the session of %s: %w", c.Token, claim.HeldNamesCapRefusal(MaxHeldNamesPerClaim))
		case err != nil:
			return fmt.Errorf("hold the session of %s: %w: %w", c.Token, errHoldUnanswered, err)
		case len(holders) != len(holds):
			return fmt.Errorf("hold the session of %s: %w: it answered %d holders for %d names", c.Token, errHoldUnanswered, len(holders), len(holds))
		}
		if h, _, refused := firstHeldElsewhere(holds, holders, c.Token); refused {
			return fmt.Errorf("%s as %s %s: %w", c.Token, h.Kind, h.Name, claim.SessionHeldRefusal(h.Name))
		}
	}
	m.admitted.Store(&admittedSession{c.Session, c.SessionFile})
	return nil
}

// warnRefusal writes a refused hold's Warn line unless it is the line this machine wrote last
// (Machine.refusalLogged). A claim that does not hold its session is asked again at every decision
// that would send its process anything, sendPending at every sweep among them, and each such line
// says nothing the first did not (review round 13 of #4220). A hold admitted clears it, so a
// refusal after an admission is written again.
func (m *Machine) warnRefusal(msg string, args ...any) {
	line := fmt.Sprint(append([]any{msg}, args...)...)
	if line == m.refusalLogged {
		return
	}
	m.refusalLogged = line
	m.log.Warn(msg, args...)
}

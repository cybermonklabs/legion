package supervise

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/claudelaunch"
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
// (legion-infra-infra-3330-planner); a resumed launch registers the session and file it recorded,
// which holds nothing new; and the launched controllers re-registered at most 5 sessions. A session
// gives at most 4 names (RecordedHolds), so that claim holds at most 42 × 4 + 8 (the two sessions
// the migration held for it) = 176 names, and 10× is 1,760. Even the most-launched claim, at
// generation 193 (legion-infra-infra-1148-architect), would hold 193 × 4 + 8 = 780 if every launch
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
	// names, and CheckedNoBroker when this daemon enrolls no process with a broker.
	Checked   string
	Abandoned string
}

const (
	// CheckedByBroker is a release the secrets broker answered: no enrollment this daemon's login
	// launched carries anything for a freed name.
	CheckedByBroker = "the secrets broker carries nothing of them"
	// CheckedNoBroker is a release by a daemon that enrolls no process with a secrets broker, so no
	// enrollment of it carries a grant for any name, and none of its pods can adopt one.
	CheckedNoBroker = "this daemon enrolls no process with a secrets broker, so nothing of its carries a grant"
)

// ErrCarryUnanswered is a release the secrets broker did not answer: the release frees nothing,
// since nothing else can say that no grant follows a name it would free.
var ErrCarryUnanswered = errors.New("the secrets broker did not answer which of the names it still carries")

// NamesCarriedError refuses a release: the secrets broker still carries something for some of the
// names it would free, under an enrollment this daemon's login launched, so the next enrollment of
// that name by any claim of this daemon would take it (envoy's adopt). Carried is each such
// enrollment, as the broker answered it. The release frees nothing.
type NamesCarriedError struct {
	Claim   claim.Token
	Carried []CarriedEnrollment
}

func (e *NamesCarriedError) Error() string {
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
// an earlier file of it named for its id).
//
// It frees them only when nothing can follow a freed name to a grant. The daemon enrolls a
// session's process with the secrets broker by its session id, and the broker hands what an ended
// enrollment still carries — its live grants and its pending requests — to the next enrollment of
// the same session id that this daemon's login launches (envoy's adopt); another claim registering
// a freed name would be that enrollment. So the release first asks the broker which of the names
// it would free name an enrollment of this daemon's that is live or still carries something
// (Enroller.Carried), and any answer refuses the whole release with NamesCarriedError, naming each
// enrollment and when its lease or its carry ends; a broker that does not answer refuses it with
// ErrCarryUnanswered. Nothing is freed either way. The broker's own rows answer every case a time
// window only estimated: a revoke the daemon missed, a lease a broker outage held live, a sweep
// that ran late or kept failing, a longer BROKER_SESSION_CARRY_SECONDS. A daemon with no broker
// (Deps.Secrets nil) enrolls nothing, so its pods adopt nothing, and its release frees with no
// check and says so (CheckedNoBroker).
//
// It runs under the machine's lock, so no decision of this claim — a registration of one of the
// names among them — falls between the broker's answer and the delete, and no other claim can take
// a name before the delete, since the table holds it for this one. The names freed leave the
// machine's cache (heldNames). The session the claim last abandoned has its names freed with the
// rest, and its record is marked released (AbandonedSession.Released), so the boot holds none of
// its names again (store's recordedNames) and the claim keeps the record of where that session
// went. A claim that never switched sessions and needs a new one is given one by forget-session,
// and this then frees the forgotten session's names once the broker's carry of it ends: no second
// switch is needed.
func (m *Machine) ReleaseNames(ctx context.Context) (ReleasedNames, error) {
	m.mu.Lock()
	defer m.unlock()
	var holds []Hold
	if err := m.write(ctx, func(ctx context.Context) error {
		var err error
		holds, err = m.deps.Store.ClaimHolds(ctx, m.claim.Project, m.claim.Token)
		return err
	}); err != nil {
		return ReleasedNames{}, fmt.Errorf("read the names %s holds: %w", m.claim.Token, err)
	}
	free := releasable(holds, m.claim.Session, m.claim.SessionFile)
	released := ReleasedNames{Checked: CheckedNoBroker}
	if m.deps.Secrets != nil {
		released.Checked = CheckedByBroker
	}
	if len(free) == 0 {
		return released, nil
	}
	if m.deps.Secrets != nil {
		names := make([]string, 0, len(free))
		for _, h := range free {
			if !slices.Contains(names, h.Name) {
				names = append(names, h.Name)
			}
		}
		asking, cancel := context.WithTimeout(ctx, m.deps.Timeouts.RPC)
		carried, err := m.deps.Secrets.Carried(asking, names)
		cancel()
		if err != nil {
			return ReleasedNames{}, fmt.Errorf("release the names of %s: %w: %w", m.claim.Token, ErrCarryUnanswered, err)
		}
		if len(carried) > 0 {
			refusal := &NamesCarriedError{Claim: m.claim.Token, Carried: carried}
			m.log.Warn("supervise: refused a release of a claim's names: the secrets broker still carries some of them",
				"claim", m.claim.Token, "names", len(free), "carried", len(carried), "error", refusal.Error())
			return ReleasedNames{}, refusal
		}
	}
	if err := m.write(ctx, func(ctx context.Context) error {
		var err error
		released.Freed, err = m.deps.Store.FreeHolds(ctx, m.claim.Project, m.claim.Token, free)
		return err
	}); err != nil {
		return ReleasedNames{}, fmt.Errorf("free the names of %s: %w", m.claim.Token, err)
	}
	for _, h := range released.Freed {
		delete(m.heldNames, h)
	}
	var err error
	if a := &m.claim.Abandoned; a.Session != "" && a.Released.IsZero() {
		a.Released, released.Abandoned = m.deps.Clock.Now(), a.Session
		err = m.persist(ctx)
	}
	m.log.Info("supervise: released the names of a claim's other sessions",
		"claim", m.claim.Token, "freed", len(released.Freed), "checked", released.Checked, "abandoned", released.Abandoned)
	return released, err
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

// SQLLiteral is s as a PostgreSQL string literal under standard_conforming_strings, the default
// since PostgreSQL 9.1: quoted, with each quote doubled, so no text of s ends the literal. The
// statement the daemon's boot prints for an operator to settle a name held for no claim writes
// every name an agent chose this way (internal/daemon's session conflicts).
func SQLLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// HoldKind is which of a session's names a hold keeps (Hold): each is a key some resume path
// finds a session by.
type HoldKind string

const (
	// HoldSessionID is a session id: the one an agent registers, which the secrets broker enrolls
	// as its wake address (ensureEnrolled), and the one a Claude Code launch resumes, `--resume
	// <id>`, read from its transcript's name (claudelaunch.ResumeSession).
	HoldSessionID HoldKind = "session"
	// HoldFileName is a session file's base name (filepath.Base). It is every key a resume finds
	// a session by its file with: an Oh My Pi launch resumes the recorded path (`--resume=<file>`,
	// runtime/sandbox's agentArgv and the tmux runtime's launcher), the launcher finds the
	// session's row in the session table by that path (launcher's resumable and claudeKeeper), and
	// where the path is missing the tmux runtime resumes a file of the same name one folder below a
	// sessions directory (sessionFileOf). Two paths that are one have one name, so holding the name
	// holds the path too: no hold of the path alone is kept, since it could refuse nothing the
	// name's hold does not.
	HoldFileName HoldKind = "file-name"
	// HoldFileID is the session id an Oh My Pi file name `<timestamp>_<id>.jsonl` gives, the part
	// after its first `_`: the tmux runtime also resumes any `*_<id>.jsonl` there (newestSession).
	HoldFileID HoldKind = "file-id"
)

// Hold is one name of a session, held to the first claim of its project that recorded it
// (holdRecorded).
type Hold struct {
	Kind HoldKind
	Name string
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
// registration of one is refused (checkSessionNames), and one a daemon from before the bound
// recorded is left unheld rather than stopping the boot that would hold it. The claim_sessions
// migration's backfill spells the same rule in SQL, and its test holds the two to one answer.
func RecordedHolds(session, sessionFile string) []Hold {
	var holds []Hold
	if session != "" && len(session) <= claim.MaxSessionNameBytes {
		holds = append(holds, Hold{Kind: HoldSessionID, Name: session})
	}
	if sessionFile == "" {
		return holds
	}
	base := filepath.Base(sessionFile)
	if len(base) > claim.MaxSessionNameBytes {
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

// checkSessionNames refuses a registration whose session id, or whose session file's base name, is
// longer than claim.MaxSessionNameBytes (claim.SessionNameTooLong): such a name could not be held,
// and no real session has one.
func checkSessionNames(session, sessionFile string) error {
	if len(session) > claim.MaxSessionNameBytes {
		return claim.SessionNameTooLong("session id", len(session))
	}
	if base := filepath.Base(sessionFile); sessionFile != "" && len(base) > claim.MaxSessionNameBytes {
		return claim.SessionNameTooLong("session file's name", len(base))
	}
	return nil
}

// holdRecorded holds every name a claim's record gives its session (RecordedHolds) to the claim,
// whatever the claim holding one became since: queued, working, idle, suspended, failed or retired
// (dispatch://INFRA-840, plan E's t-e-54). The daemon enrolls the session a claim's agent reports
// with the secrets broker, whose enrollment takes over the grants and pending requests of every
// other enrollment of the same pod account and session, and a relaunch resumes the session the
// record names, its transcript read and written; every pod of a project enrolls under the
// project's one account. So the names are held before anything records, resumes or enrolls them:
// a claim may take only a name it recorded before or one no claim has recorded, and the store
// keeps which claim recorded each first (Store.HoldSessions), all of one record's names or none.
// Any other is refused with claim.SessionHeldRefusal, which names no other claim; the log names
// the holder. A name held for no claim (two claims recorded it before the hold) is refused to
// every claim.
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
// (launchNow), and any other decision is asked again by the next one.
func (m *Machine) holdRecorded(ctx context.Context, session, sessionFile string) error {
	var ask []Hold
	for _, h := range RecordedHolds(session, sessionFile) {
		if !m.heldNames[h] {
			ask = append(ask, h)
		}
	}
	if len(ask) == 0 {
		return nil
	}
	var holders []claim.Token
	if err := m.write(ctx, func(ctx context.Context) error {
		var err error
		holders, err = m.deps.Store.HoldSessions(ctx, m.claim.Project, m.claim.Token, ask)
		return err
	}); errors.Is(err, ErrHeldNamesCap) {
		m.log.Warn(heldNamesCapLine,
			"claim", m.claim.Token, "names", len(ask), "cap", MaxHeldNamesPerClaim, "error", err,
			"release", ReleaseNamesCommand(m.claim.Token))
		return fmt.Errorf("hold the session of %s: %w", m.claim.Token, claim.HeldNamesCapRefusal(MaxHeldNamesPerClaim))
	} else if err != nil {
		return fmt.Errorf("hold the session of %s: %w: %w", m.claim.Token, errHoldUnanswered, err)
	}
	if len(holders) != len(ask) {
		return fmt.Errorf("hold the session of %s: %w: it answered %d holders for %d names", m.claim.Token, errHoldUnanswered, len(holders), len(ask))
	}
	for i, h := range ask {
		if holders[i] != m.claim.Token {
			m.log.Warn("supervise: refused a session another claim recorded first",
				"kind", h.Kind, "name", h.Name, "claim", m.claim.Token, "holder", holders[i])
			return fmt.Errorf("%s as %s %s: %w", m.claim.Token, h.Kind, h.Name, claim.SessionHeldRefusal(h.Name))
		}
	}
	if m.heldNames == nil {
		m.heldNames = map[Hold]bool{}
	}
	for _, h := range ask {
		m.heldNames[h] = true
	}
	return nil
}

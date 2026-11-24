package workflow

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The bounds on who is told of a session whose Oh My Pi session store stops taking its transcript
// (dispatch://INFRA-5660/spec#b-r2), and the lines that say which way each notice went.
//
// A session reports the failure on every activity reading while its store refuses its writes,
// and a failure that clears inside SessionStoreFailureStood tells nobody: a store that refuses a
// write and takes the next one has lost nothing anybody must act on. One claim's architect is told
// at most once a SessionStoreNoticeInterval, counted from the last telling whatever cleared and
// came back since and across a relaunch, as one pod's volume notices are held to its hour
// (volumeNoticeHeldBy). More than SessionStoreBurstSize claims of one project whose failures began
// within SessionStoreBurstWindow of each other are one fault of the store they share: on
// 2026-11-23 one store error stopped 163 pods' stores in seven minutes, which would have been
// about 190 notices to architects none of whom could fix it. So such a burst tells the project's
// controller once, and none of their architects.
//
// Every line begins with SessionStoreLinePrefix and names the issue, the role and the session, and
// a held line the bound that held it, because the issue's production check reads them from the
// daemon's log to prove the bounds hold. They are constants so the check and this package name one
// string.
const (
	SessionStoreFailureStood   = time.Minute
	SessionStoreNoticeInterval = time.Hour
	SessionStoreBurstWindow    = 5 * time.Minute
	SessionStoreBurstSize      = 5

	SessionStoreLinePrefix     = "session store failing: "
	SessionStoreArchitectTold  = SessionStoreLinePrefix + "the claim's architect was told its session store refuses its transcript"
	SessionStoreControllerTold = SessionStoreLinePrefix + "the project's controller was told of a burst of session stores refusing their transcripts, and no architect of them"
	SessionStoreNoticeHeld     = SessionStoreLinePrefix + "a bound held the architect's notice of a claim whose session store refuses its transcript"
)

// SessionStoreNoticeOwed is whether a claim whose session store has failed since since, and of
// which the daemon last told at toldAt (the zero time for never), is owed a decision at now, and
// whether the hour of its last telling holds that decision. It is owed once the failure has stood
// SessionStoreFailureStood and nobody has been told of this failure: a telling at or after since
// is of it, and an earlier one is of a failure that cleared before this one began. The daemon's
// minute sweep reads it to choose the claims it applies a fact for (internal/daemon's
// tellSessionStoreFailures), and the handler reads it again inside the fact's transaction
// (sessionStoreFailed), so the two hold one rule.
//
// A telling is matched to a failure by the daemon's clock against the session's, the start being
// the session's reading bounded to the daemon's clock at receipt (store's PutClaimActivity): a
// session whose clock runs behind the daemon's by more than the gap between a telling and a new
// failure has that failure read as told.
func SessionStoreNoticeOwed(since, toldAt, now time.Time) (owed, held bool) {
	if since.IsZero() || now.Before(since.Add(SessionStoreFailureStood)) {
		return false, false
	}
	if toldOfFailure(since, toldAt) {
		return false, false
	}
	return true, inNoticeHour(toldAt, now)
}

// toldOfFailure is whether a telling at toldAt is of the failure that began at since.
func toldOfFailure(since, toldAt time.Time) bool {
	return !toldAt.IsZero() && !toldAt.Before(since)
}

// inNoticeHour is whether a telling at toldAt is inside the hour that holds the next one at now.
func inNoticeHour(toldAt, now time.Time) bool {
	return !toldAt.IsZero() && now.Sub(toldAt) < SessionStoreNoticeInterval
}

// sessionStoreFailed applies one intake.SessionStoreFailed: it decides, on the claims of the
// project read inside the fact's own transaction, who is told of the failure of the claim's session
// store, and records the telling with the notice. In order, each rule overriding the ones after it:
//
//   - A failure that has cleared or moved since the sweep read it is a later fact's, and one told of
//     already, by a burst that named it since, needs nothing. Neither writes a line.
//   - A claim told of within the hour is held: one line names the bound, and nothing is written.
//   - A claim whose failure began within SessionStoreBurstWindow of one a burst already told the
//     controller of, inside that claim's hour, joins that burst: it is marked told to the
//     controller now, with one held line naming the burst, and nobody is told again.
//   - A claim whose failure began in a burst, more than SessionStoreBurstSize failing claims of the
//     project within one SessionStoreBurstWindow, has the controller told once of every claim of
//     that window, in one notice filed under the project key; each claim of it nobody was told of
//     is marked told to the controller now, so it counts as told for its hour, and gets a held line.
//   - Otherwise the issue's architect is told, and the claim marked told to the architect now.
//
// A burst counts every failing claim of its window, one whose failure has stood less than a minute
// among them, since the count is of failures that began; the minute only says when a claim is
// decided on, so a claim that is not yet decided joins the burst at its own minute. Every store
// error is returned and never written over: ApplyFact commits the notice, the mark and the fact's
// applied mark together, so the next sweep applies the identical fact again.
func (e *Engine) sessionStoreFailed(ctx context.Context, tx pgx.Tx, fact intake.SessionStoreFailed) (intake.Result, error) {
	failing, err := e.store.ProjectSessionStoreFailures(ctx, tx, fact.Token)
	if err != nil {
		return intake.Result{}, err
	}
	at := -1
	for i, c := range failing {
		if c.Token == fact.Token {
			at = i
			break
		}
	}
	if at < 0 || failing[at].Generation != fact.Generation || !failing[at].Since.Equal(fact.Since) {
		return intake.Result{}, nil
	}
	failed, now := failing[at], e.now()
	owed, held := SessionStoreNoticeOwed(failed.Since, failed.ToldAt, now)
	if !owed {
		// A failure that has not stood a minute is no fact the sweep mints; one that has, and that
		// a burst named since the sweep read it, needs nothing more.
		return intake.Result{Unspent: now.Before(failed.Since.Add(SessionStoreFailureStood))}, nil
	}
	if held {
		e.logOnCommit(ctx, SessionStoreNoticeHeld, append(sessionStoreAttrs(failed),
			"bound", fmt.Sprintf("this claim's last notice went to its %s at %s, inside the %s this bound gives one claim",
				failed.ToldTo, failed.ToldAt.UTC().Format(time.RFC3339), SessionStoreNoticeInterval))...)
		return intake.Result{}, nil
	}
	if joined, ok := sessionStoreBurstJoined(failing, failed, now); ok {
		if err := e.store.TellSessionStoreFailures(ctx, tx, []claim.Token{failed.Token}, supervise.StoreFailureToldController, now); err != nil {
			return intake.Result{}, err
		}
		e.logOnCommit(ctx, SessionStoreNoticeHeld, append(sessionStoreAttrs(failed),
			"bound", fmt.Sprintf("it joins the burst its project's controller was told of at %s: %s's %s (session %s) began failing at %s, within %s of it",
				joined.ToldAt.UTC().Format(time.RFC3339), joined.Issue, joined.Role, joined.Session,
				joined.Since.UTC().Format(time.RFC3339), SessionStoreBurstWindow))...)
		return intake.Result{}, nil
	}
	if burst := sessionStoreBurst(failing, failed); len(burst) > 0 {
		return intake.Result{}, e.tellSessionStoreBurst(ctx, tx, burst, now)
	}
	if err := e.notice(ctx, tx, failed.Issue, record.Notice{Kind: record.SessionStoreFailedNotice, Role: claim.RoleArchitect,
		Reason: sessionStoreFailedReason(failed, now)}); err != nil {
		return intake.Result{}, err
	}
	if err := e.store.TellSessionStoreFailures(ctx, tx, []claim.Token{failed.Token}, supervise.StoreFailureToldArchitect, now); err != nil {
		return intake.Result{}, err
	}
	e.logOnCommit(ctx, SessionStoreArchitectTold, append(sessionStoreAttrs(failed), "error", failed.Error)...)
	return intake.Result{}, nil
}

// tellSessionStoreBurst tells the project's controller of burst in one notice, filed under the
// project key as every notice of the project's own is, and marks each claim of it nobody was told
// of, inside its hour, told to the controller: so none of their architects is told of this
// failure, and none of them for the hour.
func (e *Engine) tellSessionStoreBurst(ctx context.Context, tx pgx.Tx, burst []record.StoreFailingClaim, now time.Time) error {
	var marked []claim.Token
	var named []string
	for _, c := range burst {
		named = append(named, fmt.Sprintf("%s/%s/%s", c.Issue, c.Role, c.Session))
		if toldOfFailure(c.Since, c.ToldAt) || inNoticeHour(c.ToldAt, now) {
			continue
		}
		marked = append(marked, c.Token)
	}
	if err := e.enqueue(ctx, tx, e.cfg.Project, record.ControllerNotice{Kind: record.SessionStoreBurstNotice,
		Reason: sessionStoreBurstReason(burst, now)}); err != nil {
		return err
	}
	if err := e.store.TellSessionStoreFailures(ctx, tx, marked, supervise.StoreFailureToldController, now); err != nil {
		return err
	}
	e.logOnCommit(ctx, SessionStoreControllerTold, "project", e.cfg.Project, "count", len(burst), "claims", named)
	for _, c := range burst {
		if !slices.Contains(marked, c.Token) {
			continue
		}
		e.logOnCommit(ctx, SessionStoreNoticeHeld, append(sessionStoreAttrs(c),
			"bound", fmt.Sprintf("it is one of a burst of %d claims of the project whose stores began failing within %s of each other, which its project's controller is told of in place of their architects",
				len(burst), SessionStoreBurstWindow))...)
	}
	return nil
}

// sessionStoreBurstJoined is the claim of an earlier burst that failed's failure joins, and
// whether there is one: another claim whose failure stands, which a burst told the controller of
// (its told mark is the controller's, of this failure, inside its hour), and whose failure began
// within SessionStoreBurstWindow of failed's, before or after.
func sessionStoreBurstJoined(failing []record.StoreFailingClaim, failed record.StoreFailingClaim, now time.Time) (record.StoreFailingClaim, bool) {
	for _, c := range failing {
		if c.Token == failed.Token || c.ToldTo != supervise.StoreFailureToldController ||
			!toldOfFailure(c.Since, c.ToldAt) || !inNoticeHour(c.ToldAt, now) {
			continue
		}
		if gap := c.Since.Sub(failed.Since); gap <= SessionStoreBurstWindow && gap >= -SessionStoreBurstWindow {
			return c, true
		}
	}
	return record.StoreFailingClaim{}, false
}

// sessionStoreBurst is the burst failed's failure began in, every failing claim of it in the order
// failing holds them, and nil where it began in none: of the windows of SessionStoreBurstWindow
// that start at a failure no later than failed's and hold failed's, the one that holds the most
// failing claims, when it holds more than SessionStoreBurstSize. failing is read oldest failure
// first (record.Store.ProjectSessionStoreFailures), so the first such window is the earliest.
func sessionStoreBurst(failing []record.StoreFailingClaim, failed record.StoreFailingClaim) []record.StoreFailingClaim {
	var burst []record.StoreFailingClaim
	for _, start := range failing {
		if start.Since.After(failed.Since) || failed.Since.Sub(start.Since) > SessionStoreBurstWindow {
			continue
		}
		var window []record.StoreFailingClaim
		for _, c := range failing {
			if !c.Since.Before(start.Since) && !c.Since.After(start.Since.Add(SessionStoreBurstWindow)) {
				window = append(window, c)
			}
		}
		if len(window) > len(burst) {
			burst = window
		}
	}
	if len(burst) <= SessionStoreBurstSize {
		return nil
	}
	return burst
}

// sessionStoreAttrs is what every line of a notice of c names: its issue, its role, its session,
// its claim and when its failure began.
func sessionStoreAttrs(c record.StoreFailingClaim) []any {
	return []any{"issue", c.Issue, "role", c.Role, "session", c.Session, "claim", c.Token,
		"since", c.Since.UTC().Format(time.RFC3339)}
}

// sessionStoreFailedReason is the architect's notice: which claim, since when, the store's error,
// what that costs and what is the architect's to decide.
func sessionStoreFailedReason(c record.StoreFailingClaim, now time.Time) string {
	return fmt.Sprintf("%s's %s, session %s, reports that its session store has refused its transcript since %s, %s ago, with the error %s. "+
		"Its process goes on working, but nothing it has done since then is in the store: a relaunch or a replacement of its pod resumes the session without it. "+
		"Decide whether the role should push its work now; the claim's view in the state carries the failure (storeFailure) until its session reads its store taking the transcript again, "+
		"and you are told of this claim's store at most once an hour.",
		c.Issue, c.Role, c.Session, c.Since.UTC().Format(time.RFC3339), now.Sub(c.Since).Round(time.Second), strconv.Quote(c.Error))
}

// sessionStoreBurstReason is the controller's notice of a burst: how many claims, each by its
// issue, role, session and start, the stores' error, said once where every claim of the burst
// read the same one and beside each claim otherwise, and that a claim told of before the burst is
// named as such.
func sessionStoreBurstReason(burst []record.StoreFailingClaim, now time.Time) string {
	oneError := true
	for _, c := range burst {
		oneError = oneError && c.Error == burst[0].Error
	}
	names := make([]string, len(burst))
	for i, c := range burst {
		name := fmt.Sprintf("%s's %s (session %s, failing since %s", c.Issue, c.Role, c.Session, c.Since.UTC().Format(time.RFC3339))
		if toldOfFailure(c.Since, c.ToldAt) || inNoticeHour(c.ToldAt, now) {
			name += fmt.Sprintf(", its %s told at %s", c.ToldTo, c.ToldAt.UTC().Format(time.RFC3339))
		}
		name += ")"
		if !oneError {
			name += ": " + strconv.Quote(c.Error)
		}
		names[i] = name
	}
	stores := ""
	if oneError {
		stores = fmt.Sprintf(" Every store gave the error %s.", strconv.Quote(burst[0].Error))
	}
	return fmt.Sprintf("%d claims of this project began failing to write their session stores within %s of each other, so the fault is the store they share and no one session's, and none of their architects is told: %s.%s "+
		"Each process goes on working, but nothing it has done since its failure began is in the store, so a relaunch or a replacement of its pod resumes its session without it. "+
		"A claim of the project whose store begins failing within %s of one of these joins this burst, and you are told nothing more of it; each claim's view in the state carries its failure (storeFailure) until its session reads its store taking the transcript again.",
		len(burst), SessionStoreBurstWindow, strings.Join(names, "; "), stores, SessionStoreBurstWindow)
}

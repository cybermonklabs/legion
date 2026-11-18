package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/fleetalert"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/runtime"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/timewords"
)

// The bounds of R5 and R6 the keeper counts beside the 20 minutes a session may go unregistered
// (supervise.UnreachableAfter): the guard's refusal that becomes a case for a person, the wait
// after a relaunch before one that did not bring the claim back is, and the 20 minutes of failed
// reads or of a list with no live session that write R6's line (fleetalert.ListSilentFor, which
// that line's sentence names).
const (
	guardHeldAfter = 60 * time.Minute
	notBackAfter   = 20 * time.Minute
	silentListFor  = fleetalert.ListSilentFor
)

// The causes the two relaunches record on the claim (supervise.RequestRelaunch.Cause), each read
// after "because" in the cut-turn note the task the claim owed comes back behind.
var (
	relaunchedByController = "Envoy could not reach its session for " + timewords.Words(supervise.UnreachableAfter) +
		", so the project's controller relaunched it on the same session"
	relaunchedByDaemon = "Envoy could not reach its session for " + timewords.Words(supervise.UnreachableAfter) +
		", so the daemon relaunched it on the same session"
)

// reachKeeper is the daemon's reading of which claims Envoy cannot reach (R5), and what it tells
// and does about them. Once a minute, on the orphan sweep that already reads every claim, it reads
// Envoy's whole session list with its unresponsive sessions (notify.HTTPPublisher.ListSessions) and
// judges each claim's session from the last registration it has read of it: the listener's bucket
// forgets a session after 5 minutes, so the keeper keeps that time itself (seen), for each session
// a watched claim records.
//
// A claim is out of reach when the latest of its session's last registration, the daemon's boot,
// its session's first ready (supervise.Reach.FirstReady), the end of the keeper's last failed read
// and its last read that found no live session in the list is more than 20 minutes old. Counting
// from the boot, a failed read and an empty list keeps a daemon outage, a listener outage and its
// end from marking every claim at once; counting from the first ready of the session keeps a
// relaunch, which keeps the session, from counting the 20 minutes again. The mark is the claim's
// reach record (supervise.Reach), which the claim's own row holds and the state shows, and which
// changes no state, so every rule that reads the state reads it as before.
//
// The record is kept in memory, seeded from what the store read of each claim, and written
// through to the store at each change (store's PutClaimReach): the sweep and the `relaunch` route
// are its only writers, and both write under mu, so neither writes over the other's change. A
// write the store refuses is written again at the next sweep (dirty).
//
// Two facts of it are written with the act they record rather than after it, so no end of the
// daemon and no refused write between the two can lose one or repeat the other. A notice is
// written with the record that marks it told — Reported, ArchitectTold, ReadyTold, HeldTold,
// NotBackTold or ExitTold — in one transaction (reachMark, tell and enqueue). A relaunch is
// written by the launch it makes, onto the claim's own row (supervise.Claim.LastRelaunch), and
// the keeper takes it into its record from the claim at its next read of it
// (relaunchTakenIn), whether this daemon or the next made it.
type reachKeeper struct {
	reachKeeperOptions

	wg sync.WaitGroup

	mu      sync.Mutex
	records map[claim.Token]supervise.Reach
	dirty   map[claim.Token]bool
	seen    map[string]time.Time
	// sessions is each session of the last list that answered, with the Envoy roles it holds.
	sessions map[string]notify.ListedSession
	// failingSince is the first of a run of failed reads, and failedAt the end of the last failed
	// read; noLiveSince is the first of a run of reads that found no live session, noLiveAt the
	// last such read, and noLiveTold whether the controller has been told of that run.
	failingSince, failedAt time.Time
	noLiveSince, noLiveAt  time.Time
	noLiveTold             bool
	// relaunching is the controller's claim while the daemon's relaunch of it is in flight.
	relaunching claim.Token
	// loaded is whether this daemon has read the project's notice records yet (loadNotices), and
	// noLiveUncleared whether the record of a no-live run that was told still waits for its
	// delete, a read having found a live session since.
	loaded          bool
	noLiveUncleared bool
}

// reachNotices is the store of the project's own notice records (reachKeeperOptions.notices).
type reachNotices interface {
	ReachProjectNotices(ctx context.Context, project string) ([]store.ReachProjectNotice, error)
	PutReachProjectNotice(ctx context.Context, n store.ReachProjectNotice) error
	DeleteReachProjectNotice(ctx context.Context, project, kind string, token claim.Token) error
}

// reachMark is a record a notice marks told, written in the notice's own transaction: a claim's
// reach record, or for a notice that speaks for no claim's reach record the project's own record
// of it (project). A daemon that ends after the notice commits never tells it again, and a notice
// that does not commit has marked nothing.
type reachMark struct {
	token   claim.Token
	reach   supervise.Reach
	project *store.ReachProjectNotice
}

// reachKeeperOptions are what a keeper is built from, every field of it that is not its own state.
// A daemon builds its keeper once, from all of them, and never sets one afterwards.
type reachKeeperOptions struct {
	log              *slog.Logger
	project          string
	boot             time.Time
	daemonController bool
	now              func() time.Time
	list             func(context.Context) ([]notify.ListedSession, error)
	put              func(context.Context, claim.Token, supervise.Reach) (bool, error)
	// reachNotify is the workflow's two writes the keeper tells through: a controller notice with
	// its marks (tell) and an architect's notice with its marks (enqueue), each in one
	// transaction. Both are nil in a deployment with no workflow, which tells nobody.
	reachNotify
	alerts *fleetalert.Writer
	// relaunchController is the daemon's own relaunch of its controller's claim at a moment of
	// the keeper's clock, which may replace that claim's pod, answering the claim as the relaunch
	// left it; nil where the operator runs the controller.
	relaunchController func(context.Context, supervise.Claim, time.Time) (supervise.Claim, error)
	// step reads the step record the store holds for a claim (store's ClaimStep); nil reads the
	// record the claim carries (stepOf).
	step func(context.Context, claim.Token) (supervise.StepRecord, error)
	// notices is the store of the project's own notice records (store.ReachProjectNotice), which
	// holds whether the notice that Envoy lists no live session was told, written beside that
	// notice in its own transaction; nil keeps it in this daemon's memory alone.
	notices reachNotices
}

func newReachKeeper(o reachKeeperOptions) *reachKeeper {
	return &reachKeeper{reachKeeperOptions: o,
		records: map[claim.Token]supervise.Reach{}, dirty: map[claim.Token]bool{}, seen: map[string]time.Time{}}
}

// wait waits for the controller relaunch a sweep started and for every record of R6's lines the
// writer still owes the store, for the daemon's stop and for a test.
func (k *reachKeeper) wait() {
	k.wg.Wait()
	k.alerts.Wait()
}

// registrationRead takes in a registration of session read outside the minute's list: the one the
// daemon's clean stop of a dead role's session reads just before it deletes it (sessionStop),
// which takes it out of Envoy's list before the next minute's read can see it. Like a listed one,
// it counts only when it is later than every registration of the session already read, so a
// claim whose session registered in the last 20 minutes is never marked.
func (k *reachKeeper) registrationRead(session string, lastSeen time.Time) {
	if session == "" || lastSeen.IsZero() {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if lastSeen.After(k.seen[session]) {
		k.seen[session] = lastSeen
	}
}

// stepOf is the step record the store holds for c, the freshest durable reading of its worker
// stream that a relaunch's guard reads (supervise.RequestRelaunch.Step), and the record c carries
// where the keeper reads no store.
func (k *reachKeeper) stepOf(ctx context.Context, c supervise.Claim) (supervise.StepRecord, error) {
	if k.step == nil {
		return c.Step, nil
	}
	return k.step(ctx, c.Token)
}

// reachWatched is whether the keeper judges c at all: a workflow role's claim, or the project
// controller's where the daemon runs it, and nothing else.
func (k *reachKeeper) reachWatched(c supervise.Claim) bool {
	if c.Role == claim.RoleController {
		return k.daemonController
	}
	return claim.IsRole(c.Role)
}

// recordOf is the keeper's record of c, seeded from the store's on its first sight. The claim's
// last relaunch is the claim's own (supervise.Claim.LastRelaunch, LastRelaunchGeneration), and no
// part of the record, which takes it in from the claim (relaunchTakenIn). The caller holds mu.
func (k *reachKeeper) recordOf(c supervise.Claim) supervise.Reach {
	if held, ok := k.records[c.Token]; ok {
		return held
	}
	k.records[c.Token] = c.Reach
	return c.Reach
}

// relaunchTakenIn is rec once it has taken in c's last relaunch, as c's own row records it
// (supervise.Claim.LastRelaunch): a relaunch later than the one rec holds is the relaunch of the
// episode rec marks, if it marks one, which the not-back notice counts 20 minutes from and whose
// own launch is no exit of the claim's (UnreachableExitNotice), and the guard's refusals before
// it are over. The relaunch's launch wrote it, so it is read here whether this daemon made it or
// one that ended before its own write of rec. The launches are counted from the generation the
// relaunch's launch left the claim at (LastRelaunchGeneration), so a later launch of the claim,
// made before rec takes the relaunch in, is told as the exit it is; a relaunch whose launch is not
// recorded counts from the claim's generation as c holds it. That launch can land at a later
// generation than that first read: its own loop retries a refused start, a store that did not
// answer has it tried again at the probe interval, or a daemon that ended mid-start spawns the
// claim once more. So the generation the launch records later is taken in too, while the episode
// has told no exit, and no launch of the relaunch's own is told as one.
func relaunchTakenIn(c supervise.Claim, rec supervise.Reach) supervise.Reach {
	if !c.LastRelaunch.After(rec.RelaunchedAt) {
		if c.LastRelaunch.Equal(rec.RelaunchedAt) && rec.Relaunched && !rec.ExitTold && c.LastRelaunchGeneration > rec.Generation {
			rec.Generation = c.LastRelaunchGeneration
		}
		return rec
	}
	rec.RelaunchedAt = c.LastRelaunch
	if rec.Unreachable() {
		launched := c.Generation
		if c.LastRelaunchGeneration != 0 {
			launched = c.LastRelaunchGeneration
		}
		rec.Relaunched, rec.Generation, rec.NotBackTold = true, launched, false
		rec.GuardRefusedAt, rec.ReadyTold, rec.HeldTold = time.Time{}, false, false
	}
	return rec
}

// store keeps next as token's record, and writes it through when it changed or an earlier write
// of it was refused, answering the store's refusal: the record stays in memory and dirty, and the
// next sweep writes it again. The caller holds mu.
func (k *reachKeeper) store(ctx context.Context, token claim.Token, next supervise.Reach) error {
	if k.records[token] == next && !k.dirty[token] {
		return nil
	}
	k.records[token] = next
	if k.put == nil {
		return nil
	}
	known, err := k.put(ctx, token, next)
	if err != nil {
		k.dirty[token] = true
		return fmt.Errorf("write the reach record of %s: %w", token, err)
	}
	if !known {
		k.log.Info("reach: the store holds no claim to write a reach record onto", "claim", token)
	}
	delete(k.dirty, token)
	return nil
}

// storeEach is store for the sweep, which writes again at its next pass whatever the store
// refused now, and so logs the refusal and goes on. The caller holds mu.
func (k *reachKeeper) storeEach(ctx context.Context, token claim.Token, next supervise.Reach) {
	if err := k.store(ctx, token, next); err != nil && ctx.Err() == nil {
		k.log.Error("reach: the store would not write a claim's reach record; writing it again at the next sweep", "claim", token, "error", err)
	}
}

// marked takes the records marks wrote with their notice as the store now holds them. The
// caller holds mu.
func (k *reachKeeper) marked(marks []reachMark) {
	for _, mark := range marks {
		if mark.project != nil {
			k.projectTold(*mark.project)
			continue
		}
		k.records[mark.token] = mark.reach
		delete(k.dirty, mark.token)
	}
}

// projectTold takes in the project's own record of the no-live notice, as the store holds it: the
// no-live run it says was told. The caller holds mu.
func (k *reachKeeper) projectTold(n store.ReachProjectNotice) {
	if n.Told && record.NoticeKind(n.Kind) == record.EnvoyNoLiveSessionNotice {
		k.noLiveTold, k.noLiveUncleared = true, false
	}
}

// loadNotices reads the project's own record of the no-live notice, once, and answers whether it
// is read: the no-live run the last daemon told. Until it is read, the no-live notice is not told,
// since whether it was told is not known; a read that fails is made again at the next sweep. The
// caller holds mu.
func (k *reachKeeper) loadNotices(ctx context.Context) bool {
	if k.loaded {
		return true
	}
	if k.notices == nil {
		k.loaded = true
		return true
	}
	records, err := k.notices.ReachProjectNotices(ctx, k.project)
	if err != nil {
		if ctx.Err() == nil {
			k.log.Error("reach: the project's notice records could not be read; no notice they mark is told until they are", "error", err)
		}
		return false
	}
	for _, n := range records {
		k.projectTold(n)
	}
	k.loaded = true
	return true
}

// clearNoLive deletes the record of a no-live run that was told, a read having found a live
// session, which ends that run; a delete the store refuses is made again at the next read that
// finds one. The caller holds mu.
func (k *reachKeeper) clearNoLive(ctx context.Context) {
	if k.notices == nil {
		return
	}
	if err := k.notices.DeleteReachProjectNotice(ctx, k.project, string(record.EnvoyNoLiveSessionNotice), ""); err != nil {
		k.noLiveUncleared = true
		if ctx.Err() == nil {
			k.log.Error("reach: the record of a told no-live run was not deleted; deleting it again at the next live read", "error", err)
		}
		return
	}
	k.noLiveUncleared = false
}

// sweep is one minute's read of Envoy's list and judgement of every watched claim of claims.
//
// A read that fails changes no mark and sends no notice: it says nothing of any session. Its end
// is one of the moments the 20 minutes are counted from, and reads that fail for 20 minutes write
// R6's line once, until a read answers (fleetalert.SessionListUnreadable). A list that holds no
// live session — Legion's or any other — marks none: its read is counted from as well, and after
// 20 minutes of such reads the controller is told once a run, written with the project's record
// that it was told, so the next daemon tells that run no more, and R6's line is written once
// (record.EnvoyNoLiveSessionNotice, fleetalert.NoLiveSession). A read that finds a live session
// ends the run and deletes its record.
//
// The project's notice record of the no-live run is read first, at the first sweep that can, and
// the record of a claim the sweep no longer reads is forgotten (forgetGone). The lock-out notices
// the minute sweep resends are no reading of the list and are kept apart (lockOutNotices.resend).
//
// R6's writer loads the lines the store holds open at the boot, and every sweep, the first after
// that load included, closes each line whose case does not hold now: the lines of a case this
// configuration does not judge (closeUnjudgedLines), and once every claim is judged, the
// controller's lines whose case the judgement found ended (closeEndedControllerLines). A line the
// daemon before this one wrote is open in the store until its case ends, and that daemon may have
// run in another configuration, or been a release that writes no such line and so closes none.
// Left open, such a line would silence the next line of its case.
func (k *reachKeeper) sweep(ctx context.Context, claims []supervise.Claim) {
	k.mu.Lock()
	k.forgetGone(claims)
	k.closeUnjudgedLines()
	k.mu.Unlock()
	if k.list == nil {
		return
	}
	sessions, err := k.list(ctx)
	now := k.now()
	k.mu.Lock()
	defer k.mu.Unlock()
	if err != nil {
		if k.failingSince.IsZero() {
			k.failingSince = now
		}
		k.failedAt = now
		if now.Sub(k.failingSince) >= silentListFor {
			k.alerts.Open(fleetalert.SessionListUnreadable, "", "since", k.failingSince, "error", err.Error())
		}
		if ctx.Err() == nil {
			k.log.Warn("reach: Envoy's session list could not be read; no claim's mark changes and nobody is told", "error", err)
		}
		return
	}
	k.failingSince = time.Time{}
	k.alerts.Close(fleetalert.SessionListUnreadable, "")
	// Envoy's list names every session of the fleet, about 1,100 a minute, and the keeper reads
	// only the sessions its watched claims record: the registration of any other is forgotten,
	// so seen and sessions hold no more sessions than the daemon has claims.
	recorded := make(map[string]bool, len(claims))
	for _, c := range claims {
		if c.Session != "" && k.reachWatched(c) {
			recorded[c.Session] = true
		}
	}
	maps.DeleteFunc(k.seen, func(session string, _ time.Time) bool { return !recorded[session] })
	live := false
	k.sessions = make(map[string]notify.ListedSession, len(recorded))
	for _, s := range sessions {
		live = live || s.Live()
		if !recorded[s.Session] {
			continue
		}
		if s.LastSeen.After(k.seen[s.Session]) {
			k.seen[s.Session] = s.LastSeen
		}
		k.sessions[s.Session] = s
	}
	if live {
		// The records are read once, at the first read that can, and only a run some daemon told
		// has a record to delete; a read of them the store refuses deletes the record unread,
		// which is the one way to end a run whose record cannot be read.
		if !k.loadNotices(ctx) || k.noLiveTold || k.noLiveUncleared {
			k.clearNoLive(ctx)
		}
		k.noLiveSince, k.noLiveTold = time.Time{}, false
		k.alerts.Close(fleetalert.NoLiveSession, "")
	} else {
		if k.noLiveSince.IsZero() {
			k.noLiveSince = now
		}
		k.noLiveAt = now
		if now.Sub(k.noLiveSince) >= silentListFor {
			k.alerts.Open(fleetalert.NoLiveSession, "", "since", k.noLiveSince)
			if !k.noLiveTold && k.loadNotices(ctx) && !k.noLiveTold {
				notice := record.ControllerNotice{Kind: record.EnvoyNoLiveSessionNotice,
					Reason: fmt.Sprintf("Envoy's session list has held no live session since %s, %s, so no agent of the fleet can hear anything; the daemon marks no claim out of reach while it holds none",
						stamp(k.noLiveSince), timewords.Words(silentListFor))}
				k.tellController(ctx, notice, k.project, projectMark(k.log,
					store.ReachProjectNotice{Project: k.project, Kind: string(notice.Kind), Issue: k.project, Told: true}, notice)...)
			}
		}
	}
	var listed []supervise.Claim
	for _, c := range claims {
		if !k.reachWatched(c) {
			continue
		}
		next, newly := k.judge(ctx, c, now)
		if newly {
			listed = append(listed, c)
		}
		k.storeEach(ctx, c.Token, next)
	}
	k.report(ctx, listed)
	for token := range k.dirty {
		k.storeEach(ctx, token, k.records[token])
	}
	k.closeEndedControllerLines(claims)
}

// closeUnjudgedLines closes the lines of every case this configuration does not judge: the
// controller's two where the daemon does not run its controller, and every line of a case the
// list's reads judge where no list is read (a deployment with no Envoy URL), the controller's
// among them, since the controller is judged at those reads alone. The caller holds mu.
func (k *reachKeeper) closeUnjudgedLines() {
	none := func(string) bool { return false }
	if k.list == nil {
		k.alerts.CloseEnded(fleetalert.NoLiveSession, none)
		k.alerts.CloseEnded(fleetalert.SessionListUnreadable, none)
	}
	if k.list == nil || !k.daemonController {
		k.alerts.CloseEnded(fleetalert.ControllerUnreachable, none)
		k.alerts.CloseEnded(fleetalert.ControllerRelaunchRefused, none)
	}
}

// closeEndedControllerLines closes each controller line whose case this sweep's judgement found
// ended, once the sweep has judged every claim: either line holds only while its controller's
// claim is one the keeper watches and its record is marked out of reach. A controller back in
// reach, or gone from the claims, holds neither. The list's own lines need no such pass: every
// read that answers closes the unreadable list's line, and every list with a live session the
// no-live line. The caller holds mu.
func (k *reachKeeper) closeEndedControllerLines(claims []supervise.Claim) {
	marked := map[string]bool{}
	for _, c := range claims {
		if c.Role == claim.RoleController && k.reachWatched(c) && k.records[c.Token].Unreachable() {
			marked[string(c.Token)] = true
		}
	}
	holds := func(token string) bool { return marked[token] }
	k.alerts.CloseEnded(fleetalert.ControllerUnreachable, holds)
	k.alerts.CloseEnded(fleetalert.ControllerRelaunchRefused, holds)
}

// forgetGone forgets the record of each claim the daemon no longer holds, with any write of it
// still owed: claims is every claim the sweep read, so a claim that retired and was deleted leaves
// nothing behind in the keeper. The caller holds mu.
func (k *reachKeeper) forgetGone(claims []supervise.Claim) {
	held := make(map[claim.Token]bool, len(claims))
	for _, c := range claims {
		held[c.Token] = true
	}
	maps.DeleteFunc(k.records, func(token claim.Token, _ supervise.Reach) bool { return !held[token] })
	maps.DeleteFunc(k.dirty, func(token claim.Token, _ bool) bool { return !held[token] })
}

// reachVerdict is what one minute's reading says of a claim's reach (reading), the same for every
// role; what follows from it is the role's (judge).
type reachVerdict int

const (
	// reachOver: the claim no longer records a session a process runs on — suspended, failed,
	// retired, or between launches with no session yet.
	reachOver reachVerdict = iota
	// reachRegistered: its session has registered with Envoy in the last 20 minutes.
	reachRegistered
	// reachNotDue: no mark is made yet: the session has not said ready, the 20 minutes have not
	// passed since what the count reads, or the agent has not settled.
	reachNotDue
	// reachMarked: the claim is marked out of reach.
	reachMarked
)

// judge is one claim's record after this minute's read, and whether the claim is newly out of
// reach and not yet reported, which the caller lists in one notice: the reading, then what the
// claim's role makes of it. The caller holds mu.
func (k *reachKeeper) judge(ctx context.Context, c supervise.Claim, now time.Time) (supervise.Reach, bool) {
	rec, verdict := k.reading(c, now)
	if c.Role == claim.RoleController {
		return k.judgeController(ctx, c, rec, verdict, now), false
	}
	switch verdict {
	case reachOver, reachRegistered:
		return k.episodeOver(c, rec, verdict), false
	case reachMarked:
		// Every marked claim not yet reported is listed, so one whose list the outbox did not take
		// is listed again at this sweep.
		return k.tellOfMarked(ctx, c, rec, now), !rec.Reported
	}
	return rec, false
}

// reading is c's record after this minute's read and what it says (reachVerdict): it takes in
// c's last relaunch (relaunchTakenIn), begins a record anew for a session c records afresh, counts
// from the session's first ready, and makes the mark, which it logs; it tells nobody, and it ends
// no episode, which is the role's to decide (judge). The one session change that keeps the record
// is the daemon's own controller's relaunch launching it on a fresh session (carriesItsEpisode).
// The caller holds mu.
func (k *reachKeeper) reading(c supervise.Claim, now time.Time) (supervise.Reach, reachVerdict) {
	rec := relaunchTakenIn(c, k.recordOf(c))
	if k.carriesItsEpisode(c, rec) {
		if c.Session == "" {
			// The relaunch's own launch has recorded no session yet: the episode goes on.
			return rec, reachMarked
		}
		// The fresh session has said no ready of its own yet, and the mark stands.
		rec.Session, rec.FirstReady = c.Session, time.Time{}
	}
	if c.Session == "" || !holdsEpisode(c.State) {
		return rec, reachOver
	}
	if rec.Session != c.Session {
		rec = supervise.Reach{Session: c.Session, RelaunchedAt: rec.RelaunchedAt}
	}
	if rec.FirstReady.IsZero() {
		switch {
		case supervise.SaidReady(c.State) && !c.LastReady.IsZero():
			rec.FirstReady = c.LastReady
		case !rec.Unreachable():
			// This session has not said ready yet: nothing is counted for it.
			return rec, reachNotDue
		}
	}
	lastSeen := k.seen[c.Session]
	if !lastSeen.IsZero() && now.Sub(lastSeen) <= supervise.UnreachableAfter {
		return rec, reachRegistered
	}
	// The boot, the session's first ready, a failed read's end and a read of a list with no live
	// session each hold back a mark not made yet; a mark already made stays as it is, since none of
	// them is a registration of the claim's session (a read that fails changes no mark).
	countedFrom := latest(lastSeen, k.boot, rec.FirstReady, k.failedAt, k.noLiveAt)
	if !rec.Unreachable() && now.Sub(countedFrom) <= supervise.UnreachableAfter {
		return rec, reachNotDue
	}
	if !rec.Unreachable() {
		if !supervise.SaidReady(c.State) {
			// A mark is made of a settled agent; once made it holds in every launch state of the
			// session (holdsEpisode).
			return rec, reachNotDue
		}
		rec.Since, rec.LastSeen, rec.Generation = countedFrom, lastSeen, c.Generation
		k.log.Warn("reach: the claim's session has not registered with Envoy for "+timewords.Words(supervise.UnreachableAfter)+"; it is out of reach, and keeps its state",
			"claim", c.Token, "state", c.State, "since", countedFrom, "lastSeen", lastSeen)
	}
	return rec, reachMarked
}

// episodeOver is rec once its episode is over, read as verdict, with the line that says so where
// the record marked one. The caller holds mu.
func (k *reachKeeper) episodeOver(c supervise.Claim, rec supervise.Reach, verdict reachVerdict) supervise.Reach {
	ended := rec.EndEpisode()
	switch {
	case verdict == reachOver && rec.Session != "" && rec != ended:
		k.log.Info("reach: the claim no longer runs on the session it was judged on; its mark is over", "claim", c.Token, "state", c.State)
	case verdict == reachRegistered && (rec.Unreachable() || rec.Reported || rec.Relaunched):
		k.log.Info("reach: the claim's session registered with Envoy again; it is reachable", "claim", c.Token, "lastSeen", k.seen[c.Session])
	}
	return ended
}

// tellOfMarked sends the per-claim notices of a marked claim that are due (each once): the guard
// that refused its relaunch would now let it through, or has refused it for 60 minutes; a relaunch
// reached it and it is still out of reach 20 minutes later; or it owes no task and its process
// was launched again since the mark, which is a process that exits at its Envoy role claim. The
// architect is told of the claim here too, once an episode, beside the controller's list. Each
// notice is written with the record that marks it told (tellOf), and one the outbox did not take
// leaves the record as it was, to be told at the next sweep: an exit's record does not move to
// its launch until the exit is told, so a refused notice is not lost with the launch it was of.
// The caller holds mu.
func (k *reachKeeper) tellOfMarked(ctx context.Context, c supervise.Claim, rec supervise.Reach, now time.Time) supervise.Reach {
	if !rec.ArchitectTold {
		rec.ArchitectTold = k.tellArchitect(ctx, c, rec)
	}
	if c.Generation > rec.Generation {
		if supervise.HoldsTask(c.Pending) || c.State == supervise.StateWorking || rec.ExitTold {
			rec.Generation = c.Generation
		} else {
			rec = k.tellOf(ctx, c, rec, record.UnreachableExitNotice, fmt.Sprintf(
				"%s is out of reach of Envoy and owes no task, and its process ended and was launched again on the same session at generation %d: a process that exits at its Envoy role claim. Each launch clears its activity report and exits again, so relaunch would add nothing to the daemon's own. This is a case for a person.",
				c.Token, c.Generation), func(told *supervise.Reach) { told.Generation, told.ExitTold = c.Generation, true })
		}
	}
	guard := supervise.RelaunchGuard(c, time.Time{}, now)
	if !rec.GuardRefusedAt.IsZero() && !rec.ReadyTold {
		switch {
		case guard == "":
			rec = k.tellOf(ctx, c, rec, record.RelaunchReadyNotice, fmt.Sprintf(
				"The relaunch guard refused to relaunch %s at %s, and would now let it through: its worker stream shows no step for %[3]s, its session has sent no activity report for %[3]s in a turn it is still in, its open call is none or a wait, and its last activity report shows nothing running. relaunch it again.",
				c.Token, stamp(rec.GuardRefusedAt), timewords.Words(supervise.UnreachableAfter)), func(told *supervise.Reach) { told.ReadyTold = true })
		case !rec.HeldTold && now.Sub(rec.GuardRefusedAt) >= guardHeldAfter:
			rec = k.tellOf(ctx, c, rec, record.RelaunchGuardHeldNotice, fmt.Sprintf(
				"The relaunch guard has refused to relaunch %s since %s, %s: %s. A process that still steps or reports in its turn runs while Envoy cannot reach it, and a frozen process sends nothing newer than the report that refuses it, so the guard will not let it through. This is a case for a person.",
				c.Token, stamp(rec.GuardRefusedAt), timewords.Words(guardHeldAfter), guard), func(told *supervise.Reach) { told.HeldTold = true })
		}
	}
	if rec.Relaunched && !rec.NotBackTold && now.Sub(rec.RelaunchedAt) >= notBackAfter {
		rec = k.tellOf(ctx, c, rec, record.RelaunchNotBackNotice, fmt.Sprintf(
			"relaunch reached %s at %s, and its session has not registered with Envoy in the %s since: the cause is in its pod or its connection, not in its process. This is a case for a person.",
			c.Token, stamp(rec.RelaunchedAt), timewords.Words(notBackAfter)), func(told *supervise.Reach) { told.NotBackTold = true })
	}
	return rec
}

// tellOf tells the controller the per-claim notice of kind about c, written with rec as told marks
// it, in one transaction (reachMark), and answers the record as it then stands: the marked one
// once the notice is queued, and rec as it was otherwise, to be told at the next sweep. The caller
// holds mu.
func (k *reachKeeper) tellOf(ctx context.Context, c supervise.Claim, rec supervise.Reach, kind record.NoticeKind, reason string, told func(*supervise.Reach)) supervise.Reach {
	marked := rec
	told(&marked)
	if k.tellController(ctx, k.claimNotice(kind, c, reason), c.Issue, reachMark{token: c.Token, reach: marked}) {
		return marked
	}
	return rec
}

// judgeController is the daemon's own relaunch of its marked controller (`controller: daemon`):
// nobody else can relaunch it, since the controller is the one that would decide. It relaunches
// it at most once in 6 hours, under the same guard as `relaunch`, replacing the controller's pod
// where the launch needs it or its launcher is not connected (supervise.RequestRelaunch's
// MayReplacePod). The controller hears nothing of itself, since it is the one out of reach, so
// each case nobody else can act on writes R6's line instead, once until the controller is
// reachable again (fleetalert.Writer.Open, whose store keeps which lines are open): the relaunch
// did not bring it back within 20 minutes, it is out of reach a second time within the 6 hours,
// or the guard refused the relaunch or its process outlived the stop. A refused relaunch uses no
// part of the 6 hours. The relaunch it made is the one the controller's claim records
// (supervise.Claim.LastRelaunch), which the record has taken in by now (relaunchTakenIn), so a
// daemon that ended before its own write of the record does not relaunch the controller a second
// time within the 6 hours.
//
// A relaunched controller's episode lasts past two readings that end any other claim's: its claim
// failed on the same session, as a controller whose path to Envoy stays blocked does once each
// boot fails its Envoy claim and exits; and its session registered, until the controller is back
// (controllerBack), since a process that registers and goes no further, or says ready and never
// takes the controller's Envoy role, hears nothing as the controller (R6). The caller holds mu.
func (k *reachKeeper) judgeController(ctx context.Context, c supervise.Claim, rec supervise.Reach, verdict reachVerdict, now time.Time) supervise.Reach {
	switch verdict {
	case reachNotDue:
		return rec
	case reachOver:
		if !rec.Relaunched || c.Session == "" || c.Session != rec.Session || c.State != supervise.StateFailed {
			k.controllerReachable(c)
			return k.episodeOver(c, rec, verdict)
		}
	case reachRegistered:
		if !rec.Relaunched || k.controllerBack(c, rec) {
			k.controllerReachable(c)
			return k.episodeOver(c, rec, verdict)
		}
	}
	switch {
	case rec.Relaunched:
		if now.Sub(rec.RelaunchedAt) >= notBackAfter {
			k.alerts.Open(fleetalert.ControllerUnreachable, string(c.Token), "relaunchedAt", rec.RelaunchedAt,
				"reason", "the daemon's relaunch did not bring it back: in the "+timewords.Words(notBackAfter)+
					" since, it has not said ready with the controller's Envoy role held by its session")
		}
	case !rec.RelaunchedAt.IsZero() && now.Sub(rec.RelaunchedAt) < supervise.RelaunchAgainAfter:
		k.alerts.Open(fleetalert.ControllerUnreachable, string(c.Token), "relaunchedAt", rec.RelaunchedAt,
			"reason", fmt.Sprintf("it is out of reach a second time within %[1]s of the daemon's relaunch of it, which is made once in %[1]s", timewords.Words(supervise.RelaunchAgainAfter)))
	case k.relaunching == c.Token || k.relaunchController == nil:
	default:
		if why := supervise.RelaunchGuard(c, time.Time{}, now); why != "" {
			k.alerts.Open(fleetalert.ControllerRelaunchRefused, string(c.Token), "reason", "the relaunch guard refuses it: "+why)
			return rec
		}
		k.relaunching = c.Token
		k.wg.Add(1)
		go k.relaunchOwnController(ctx, c, now)
	}
	return rec
}

// relaunchOwnController runs the daemon's relaunch of its controller off the sweep, since a
// Sandbox relaunch waits minutes on its pods, and records what it did. The relaunch's launch has
// written the relaunch onto the controller's claim, so a write of the record the store refuses
// here is written again at the next sweep and costs no part of the 6 hours.
func (k *reachKeeper) relaunchOwnController(ctx context.Context, c supervise.Claim, at time.Time) {
	defer k.wg.Done()
	relaunched, err := k.relaunchController(ctx, c, at)
	k.mu.Lock()
	defer k.mu.Unlock()
	k.relaunching = ""
	switch {
	case err == nil:
		k.log.Warn("reach: the daemon relaunched its own controller, which Envoy could not reach, on its same session", "claim", c.Token, "generation", relaunched.Generation)
		k.alerts.Close(fleetalert.ControllerRelaunchRefused, string(c.Token))
		k.storeEach(ctx, c.Token, relaunchTakenIn(relaunched, k.recordOf(c)))
	case errors.Is(err, supervise.ErrRelaunchGuard), errors.Is(err, runtime.ErrProcessOutlivedStop):
		k.alerts.Open(fleetalert.ControllerRelaunchRefused, string(c.Token), "reason", err.Error())
	default:
		if ctx.Err() == nil {
			k.log.Error("reach: the daemon's relaunch of its own controller failed; it is tried again at the next sweep", "claim", c.Token, "error", err)
		}
	}
}

// controllerReachable ends the controller's open lines, its episode being over. The caller holds mu.
func (k *reachKeeper) controllerReachable(c supervise.Claim) {
	k.alerts.Close(fleetalert.ControllerUnreachable, string(c.Token))
	k.alerts.Close(fleetalert.ControllerRelaunchRefused, string(c.Token))
}

// controllerBack is whether the daemon's own controller c, which a relaunch reached in the
// episode rec marks, is back from that relaunch (R6): it said ready after the relaunch, and its
// session holds the controller's Envoy role in the last list read. The caller holds mu.
func (k *reachKeeper) controllerBack(c supervise.Claim, rec supervise.Reach) bool {
	return supervise.SaidReady(c.State) && c.LastReady.After(rec.RelaunchedAt) &&
		k.sessions[c.Session].HoldsRole(string(c.Token))
}

// carriesItsEpisode is whether c, the daemon's own controller, has left the session rec marks
// for its relaunch's own launch (R6): a resume that finds no session starts the controller on a
// fresh one, or has recorded none yet. That launch is the relaunch's while c records the relaunch
// rec has taken in and the generation it left the claim at, or none yet, and a later launch is
// not. So the episode, and R6's line 20 minutes after the relaunch, goes on through that change,
// for a launch that runs, is starting, or has failed; a worker's relaunch keeps its session, and
// a controller suspended or retired has nothing left to judge.
func (k *reachKeeper) carriesItsEpisode(c supervise.Claim, rec supervise.Reach) bool {
	return c.Role == claim.RoleController && rec.Relaunched && rec.Unreachable() && c.Session != rec.Session &&
		c.LastRelaunch.Equal(rec.RelaunchedAt) &&
		(c.LastRelaunchGeneration == 0 || c.Generation == c.LastRelaunchGeneration) &&
		(holdsEpisode(c.State) || c.State == supervise.StateFailed)
}

// report tells the controller, in one notice, of every claim newly out of reach this minute, and
// marks each reported in the notice's own transaction: a list the wake did not queue — no
// controller registered, or an earlier list still waiting unsent — marks none, and leaves its
// claims to be listed at the next sweep. The caller holds mu.
func (k *reachKeeper) report(ctx context.Context, listed []supervise.Claim) {
	if len(listed) == 0 {
		return
	}
	rows := make([]record.UnreachableClaim, 0, len(listed))
	marks := make([]reachMark, 0, len(listed))
	for _, c := range listed {
		rec := k.records[c.Token]
		rows = append(rows, record.UnreachableClaim{Claim: string(c.Token), Issue: c.Issue, Role: c.Role,
			State: string(c.State), LastSeen: rec.LastSeen})
		rec.Reported = true
		marks = append(marks, reachMark{token: c.Token, reach: rec})
	}
	k.tellController(ctx, record.ControllerNotice{Kind: record.UnreachableClaimsNotice, Unreachable: rows}, k.project, marks...)
}

// tellArchitect tells the architect of c's tree that c is out of reach, written with rec marked
// told in the notice's own transaction, and answers whether it did: a notice of the claim on its
// issue, which the owner walk takes to the architect that owns the issue, or for a child's own
// architect on its tree's root. The tree architect's own claim is told to nobody here, since it
// could not hear it, and the controller's list names it. The notice asks nothing, since the
// controller decides the relaunch. The caller holds mu.
func (k *reachKeeper) tellArchitect(ctx context.Context, c supervise.Claim, rec supervise.Reach) bool {
	if k.enqueue == nil || c.Issue == "" || claim.IsTreeArchitect(c.Role, c.Issue, c.Tree) {
		return true
	}
	issue := c.Issue
	if c.Role == claim.RoleArchitect {
		issue = c.Tree
	}
	notice := record.Notice{Kind: record.ClaimOutOfReachNotice, Role: c.Role, Claim: string(c.Token), Reason: fmt.Sprintf(
		"%s's session has not registered with Envoy since %s, more than %s, so it cannot hear its notices. Its claim keeps its state. The project's controller decides whether to relaunch it; nothing is asked of you.",
		c.Token, stamp(rec.Since), timewords.Words(supervise.UnreachableAfter))}
	rec.ArchitectTold = true
	marks := []reachMark{{token: c.Token, reach: rec}}
	if err := k.enqueue(ctx, issue, notice, marks); err != nil {
		if ctx.Err() == nil {
			k.log.Error("reach: the architect's notice of an unreachable claim was not written; writing it again at the next sweep", "claim", c.Token, "error", err)
		}
		return false
	}
	k.marked(marks)
	return true
}

// claimNotice is a per-claim controller notice of kind about c.
func (k *reachKeeper) claimNotice(kind record.NoticeKind, c supervise.Claim, reason string) record.ControllerNotice {
	return record.ControllerNotice{Kind: kind, Role: c.Role, Claim: string(c.Token), Reason: reason}
}

// tellController queues notice on issue, with the records marks says it marks told written in the
// same transaction, and answers whether it queued it (tellQueued), taking the marks in where it
// did. The caller holds mu.
func (k *reachKeeper) tellController(ctx context.Context, notice record.ControllerNotice, issue string, marks ...reachMark) bool {
	told := tellQueued(ctx, k.log, k.tell, notice, issue, marks...)
	if told {
		k.marked(marks)
	}
	return told
}

// relaunchRefusal is why the controller's `relaunch` of c is refused before c's machine is asked,
// and nil where it goes on: the three things only the keeper knows (supervision.relaunch). c must
// be marked out of reach; its last relaunch — the later of the one the keeper's record has taken
// in and the one c's own row records, which the relaunch's launch wrote — must be at least 6 hours
// old; and of siblings, the live claims of c's pod with c among them, two or more must not every
// one be out of reach, since a relaunch into a pod that reaches Envoy for none of them fails each
// worker at its deaths limit and recovers none. The caller reads siblings before this takes mu.
func (k *reachKeeper) relaunchRefusal(c supervise.Claim, siblings []supervise.Claim, now time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	rec := relaunchTakenIn(c, k.recordOf(c))
	last := rec.RelaunchedAt
	switch {
	case !rec.Unreachable():
		return supervise.ErrClaimReachable
	case !last.IsZero() && now.Sub(last) < supervise.RelaunchAgainAfter:
		return fmt.Errorf("%w: at %s", supervise.ErrRelaunchedRecently, stamp(last))
	case len(siblings) < 2:
		return nil
	}
	tokens := make([]claim.Token, 0, len(siblings))
	for _, other := range siblings {
		if !k.recordOf(other).Unreachable() {
			return nil
		}
		tokens = append(tokens, other.Token)
	}
	slices.Sort(tokens)
	return fmt.Errorf("%w: pod %s holds %d live claims, every one out of reach: %v", supervise.ErrPodUnreachable, c.Locator.Sandbox.PodUID, len(tokens), tokens)
}

// guardRefused records that the relaunch guard refused token's relaunch at at: the first refusal
// of the episode, or the first after the controller was told the guard would let it through, which
// is a refusal of its own. It answers the store's refusal of the record, which the next sweep
// writes again: until it does, a daemon that ends loses the refusal the notices count from.
func (k *reachKeeper) guardRefused(ctx context.Context, token claim.Token, at time.Time) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	rec := k.records[token]
	if rec.GuardRefusedAt.IsZero() || rec.ReadyTold {
		rec.GuardRefusedAt, rec.ReadyTold, rec.HeldTold = at, false, false
	}
	return k.store(ctx, token, rec)
}

// relaunched takes the relaunch c's machine has just made into the keeper's record of c at once,
// rather than at the next sweep, so the notices that follow it count from it, and writes the
// record through. The relaunch's launch has written it onto the claim already
// (supervise.Claim.LastRelaunch), so the store's refusal, which it answers, costs no part of the
// 6-hour refusal, and the next sweep writes the record again.
func (k *reachKeeper) relaunched(ctx context.Context, c supervise.Claim) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.store(ctx, c.Token, relaunchTakenIn(c, k.recordOf(c)))
}

// runsOnSession is whether a claim in state runs, or is starting, a process on the session it
// records: every live state.
func runsOnSession(state supervise.ClaimState) bool {
	switch state {
	case supervise.StateLaunching, supervise.StateShimConnected, supervise.StateRegistered,
		supervise.StateReady, supervise.StateWorking, supervise.StateIdle:
		return true
	}
	return false
}

// holdsEpisode is whether a claim in state keeps the episode its reach record marks: every live
// state (runsOnSession), so a claim relaunched again and again on its session stays marked
// through its launches, and the two a launch waits in. A boot restores a claim the last daemon
// left mid-launch as launch_uncertain, and a claim with nothing launched yet is queued: neither
// has left the session it records, so a claim reported before the boot is not reported again
// after it, and a relaunch's not-back notice still counts from that relaunch (R5).
func holdsEpisode(state supervise.ClaimState) bool {
	return runsOnSession(state) || state == supervise.StateLaunchUncertain || state == supervise.StateQueued
}

// latest is the latest of moments.
func latest(moments ...time.Time) time.Time {
	var last time.Time
	for _, at := range moments {
		if at.After(last) {
			last = at
		}
	}
	return last
}

// stamp is a moment as a notice writes it.
func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

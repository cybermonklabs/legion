//go:build !race

package api

import (
	"context"
	"errors"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The mention sender's claim model: every interleaving, within small bounds, of the package's own
// mention sender (newMentionSender) - its lanes, its parks and their timers, its claim refresh
// (refreshHeldClaims) and its re-reads of claims a lost refresh may have written (rereadUnsure,
// claimKnown) - with the statements its sends, its refresh and its re-reads make on two comments'
// attempt rows, the delivery sweep, and the outbox sending the earlier events the comments wait
// behind. It is the sender's part of the mention-copy order model; the outbox's part is
// TestTheMentionCopyOrderModel in internal/dispatch/outbox.
//
// The sender runs inside a testing/synctest bubble, so the model knows when every goroutine it
// started has stopped, and each of its goroutines stops only where the model holds it: at every
// Lock of the sender's lock (mentionSender.mu), which the model replaces with its own (cmLock),
// and inside the sender's send, refresh and reread functions. So each locked section of the
// sender is a step of its own, by construction, and the search tries every order of them against
// each other and against the model's own steps; no seam is placed in the sender's code for it.
// The search keys on the sender's maps, on each of its goroutines - in the order they started,
// with where it waits, the comment of the send it last carried and what the model knows of that
// send's try - and on the model's own state, every claim in it named by its order among the claims
// in play and its age, which is all the code compares (model.DeliveryClaimSinceSQL,
// model.DeliveryClaimFreeSQL), and each attempt's lost count by whether it moved since a re-read
// in flight began, which is all the code compares of it. A lane goroutine is keyed with the lane
// it serves and its park's pending timer.
//
// What the model holds:
//   - comments A-1 and A-2 of issue T-1, each mentioning session s1 under attempt 1, which commits
//     claimed and is queued on the sender (createCommentFor, enqueue): A-1 at the start, A-2 at
//     any step;
//   - one earlier event of T-1 that s1 must take first, and one written between the comments that
//     only A-2 waits behind, each until the outbox sends it, at any step;
//   - the rows' claims, and the statements on them, restated from their SQL (cmOwns, cmFree,
//     cmRefreshWrites, owed; TestTheClaimModelsRulesAreTheSQLsRules): a send's try
//     (sendCommentMention), which reads the rule alone for a send that waited before and otherwise
//     claims the attempt as claimCommentDeliveryAttempt does - a free claim, or one at or past the
//     send's own and less than a lease after it - then waits for an earlier event or sends; the
//     refresh's statement (refreshMentionClaims), which writes only a row that carries the live
//     claim the sender names, and which may fail before it commits or lose its answer after; the
//     re-read (rereadMentionClaims), which answers a row's claim while it is at or past the claim
//     named and less than a lease after it, and which may fail; and the delivery sweep
//     (resumeOwedDelivery), which takes an attempt committed a lease ago whose claim lapsed, and
//     sends it, or, as a step of its own, hands it back to the sender (enqueue);
//   - time in slots of half a claim-refresh period (mentionClaimRefresh), which a step of its own
//     ends: the sender's loop (keepClaims) starts a refresh as every period begins, a claim lapses
//     one lease (model.DeliveryClaimLapse, cmLapsePeriods periods) after it was written, and every
//     thing that takes milliseconds or seconds - a try and its re-read, the refresh, a park's
//     timer, at most two seconds (mentionOrderRetryMax) - ends no later than the slot after the
//     one it began in, so time moves on no further while one is still to end; a locked section
//     and the sweep's hand-off take microseconds, so time moves on none while one waits, but a
//     statement may run while a goroutine waits for the lock, as a goroutine waiting for a core
//     may for milliseconds (enabled);
//   - a claim and the moment it is read are each known to a slot, so a claim read one lease and
//     part of a slot after it was written may be live or lapsed: the model reads it as the sender's
//     own statements would have it - live, or free when another holds it - and as the sweep would
//     not, so every send it finds refused, and every attempt the sweep takes, is so at any timing.
//
// What it checks, in every state, carrying each run on past a stale claim or a dropped send to
// what follows from it:
//   - stale: every claim the sender records for an attempt and trusts is the claim the attempt's
//     row carries, whenever no refresh has written rows it has not recorded yet, no try has
//     claimed that row for itself and not parked yet, and the sweep is not handing it over. A
//     claim a lost answer left unknown (mentionClaimState.unsure) is not trusted, and the sender
//     reads its row before it acts on it. A trusted record behind its row is never written again,
//     since the refresh writes only a row that carries the claim the sender names, so the row's
//     claim lapses with its send still held;
//   - dropped: an attempt that is pending under a claim live at every timing is the sender's to
//     send - it holds it (mentionSender.held), or the sweep is handing it over: otherwise nothing
//     sends it until that claim lapses, a lease on;
//   - unnamed: every held lane's wait names the earlier event its send waited behind, and every
//     queued send whose wait a late WARN would report (reportLate, reportedWaitLocked) names a
//     real one, which the held gauges and the WARN report;
//   - order and twice: s1 takes A-1 before A-2, and each of them once. Either ends the run; an
//     A-2 the delivery sweep sends first is counted apart (deliver).
//
// What it leaves out: the drain (wait), a keyed repeat (replayComment), the queue's bound, more
// than one recipient or owner, and a send or a statement of the sweep's that fails.
//
// Its runner is in comment_mention_claim_model_run_test.go, its steps in
// comment_mention_claim_model_steps_test.go, its checks in comment_mention_claim_model_checks_test.go,
// and its controls in TestTheMentionSenderClaimModel.

// cmLapsePeriods is model.DeliveryClaimLapse in claim-refresh periods (mentionClaimRefresh), and
// cmSlots how many slots of the model's time one period is.
const (
	cmLapsePeriods = 3
	cmSlots        = 2
	cmLapseSlots   = cmLapsePeriods * cmSlots
)

// cmSlot is one slot of the model's time.
const cmSlot = mentionClaimRefresh / cmSlots

// cmBase is a bubble's clock, which stands still here: the model counts time in its own slots.
var cmBase = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

var cmComments = [2]string{"A-1", "A-2"}

// cmEarlier is the earlier event both comments wait behind, and cmBetween one written between
// them, which only A-2 waits behind: it lets A-2's first try, which claims its attempt, wait and
// park long after a refresh began, as a send queued behind a busy lane's does.
var (
	cmEarlier = model.MentionCopyHolder{EventID: 1}
	cmBetween = model.MentionCopyHolder{EventID: 2}
)

// holder is the earlier event comment c waits behind now, if any.
func (r *cmRun) holder(c int) (model.MentionCopyHolder, bool) {
	switch {
	case !r.published:
		return cmEarlier, true
	case c == 1 && !r.publishedBetween:
		return cmBetween, true
	}
	return model.MentionCopyHolder{}, false
}

var cmChecks = []string{"stale", "dropped", "order", "twice", "unnamed"}

// errCmLost is a statement's answer the model loses, or a read it fails.
var errCmLost = errors.New("the model loses the answer: the connection dropped")

type cmStepKind uint8

const (
	cmCommit cmStepKind = iota
	cmLockRuns
	cmRereadRuns
	cmRereadAnswers
	cmRereadFails
	cmTryRuns
	cmTryAnswers
	cmTimerRuns
	cmTimePasses
	cmRefreshRereadRuns
	cmRefreshRereadAnswers
	cmRefreshRereadFails
	cmRefreshWrites
	cmRefreshFailsUncommitted
	cmRefreshRecords
	cmRefreshAnswerLost
	cmPublish
	cmPublishBetween
	cmSweepClaims
	cmSweepHandsOver
)

// cmStep is one step of a run: its kind, the comment a commit, a try, its re-read or a sweep is
// of, and, for a lock, the goroutine that takes it, by its rank among the live ones in the order
// they started. It holds no pointer, so the search's queue of runs to replay is memory the
// garbage collector does not scan.
type cmStep struct {
	kind    cmStepKind
	comment int
	g       int
}

type cmVariant struct {
	name string
	// alwaysLoses loses every refresh's answer; otherwise each may be lost or arrive, so a run
	// with every answer lost is among the runs explored.
	alwaysLoses bool
	// readFailures is how many re-reads of one run may fail.
	readFailures int
	// outages is how many refresh statements of one run may fail before they commit: they write
	// nothing and answer an error, as a database the sender cannot reach does, so the claims they
	// named age while the sender holds them.
	outages int
	// blind has every re-read answer no row: the control's sender learns nothing of a lost write.
	blind bool
	// revivesLapsed is a refresh statement that writes a row whose recorded claim has lapsed, and
	// takeNeedsLive a take that counts the row's claim the send's own only while the claim it
	// carries is itself live: the control the outages must break.
	revivesLapsed, takeNeedsLive bool
}

var (
	cmAsBuilt     = cmVariant{name: "the sender as built: any refresh answer lost, two re-reads failed", readFailures: 2}
	cmBlindRepair = cmVariant{
		name: "control: every refresh answer lost, every re-read answers no row", alwaysLoses: true, blind: true,
	}
	cmOutage = cmVariant{
		name: "the sender as built: up to two refreshes failed uncommitted, any answer lost", outages: 2,
	}
	cmLapseBlindStatements = cmVariant{
		name:    "control: the same, with a refresh that writes a lapsed claim and a take that needs a live one",
		outages: 2, revivesLapsed: true, takeNeedsLive: true,
	}
)

// cmRow is a comment's attempt row: whether it committed and is still pending, when it committed,
// and the claim it carries (zero for none).
type cmRow struct {
	committed, pending bool
	created, claim     time.Time
}

// cmRead is a re-read in flight: the claims it names, each attempt's lost count as the read
// began, and, once it read the rows, what it found.
type cmRead struct {
	named []mentionClaim
	lost  map[string]uint64
	reply chan cmReadAnswer
	ran   bool
	found []mentionClaim
}

// newRead is a re-read of named, which arrived now, its reply reply.
func (r *cmRun) newRead(named []mentionClaim, reply chan cmReadAnswer) *cmRead {
	read := &cmRead{named: named, reply: reply, lost: map[string]uint64{}}
	r.lock.Mutex.Lock()
	for _, claim := range named {
		read.lost[claim.key.comment] = r.sender.held[claim.key].lost
	}
	r.lock.Mutex.Unlock()
	return read
}

// readsInFlight is every re-read in flight or answered and not yet acted on: the tries', the
// refresh's, and those their goroutines hold.
func (r *cmRun) readsInFlight() []*cmRead {
	var reads []*cmRead
	for _, try := range r.tries {
		if try.read != nil {
			reads = append(reads, try.read)
		}
	}
	if r.refresh != nil && r.refresh.read != nil {
		reads = append(reads, r.refresh.read)
	}
	for _, g := range r.gs {
		if g.answered != nil {
			reads = append(reads, g.answered)
		}
	}
	return reads
}

type cmReadAnswer struct {
	claims []mentionClaim
	err    error
}

// cmTry is a lane goroutine at one of its statements: the re-read of its send's claim
// (claimKnown), or the send itself (sendCommentMention) - the item it was handed and, once its
// statements ran, its answer and whether they claimed the row for itself.
type cmTry struct {
	g      *cmG
	began  int
	read   *cmRead
	item   mentionSend
	reply  chan cmAnswer
	ran    bool
	answer cmAnswer
	took   bool
}

type cmAnswer struct {
	item  mentionSend
	waits bool
}

// cmRefresh is what the refresh in flight is at: its re-read while one is in flight, and its
// statement once it has named its claims, kept until the refresh records its answer under its
// next lock; named is that its statement has named its claims, so a re-read now is the one after a
// lost answer (refreshHeldClaims), after which it ends.
type cmRefresh struct {
	read  *cmRead
	stmt  *cmStatement
	named bool
}

// cmStatement is the refresh's statement: the claims it names, and once it ran, what it wrote, and
// whether its answer is sent.
type cmStatement struct {
	queued   []mentionClaim
	reply    chan cmWritten
	wrote    bool
	written  []mentionClaim
	answered bool
	// lost is that the answer sent was an error.
	lost bool
}

type cmWritten struct {
	claims []mentionClaim
	err    error
}

// cmTimer is the held lane's park timer the sender started, and the slot it started in.
type cmTimer struct {
	run   func()
	began int
}

// cmG is one of the sender's goroutines: a lane goroutine (serve), or the refresh
// (refreshHeldClaims). It waits for the sender's lock at a site, or at a statement (cmTry,
// cmRefresh), or runs: between two steps it never does. Its fields past running are what the
// goroutine holds in its own variables that the sender's state does not show, which the search's
// key must therefore carry.
type cmG struct {
	refresh bool
	born    int
	lock    chan struct{}
	site    string
	running bool
	// last is the comment of the send a lane goroutine last carried (-1 for none), done whether
	// its last try ended not waiting, ended its last try when that try claimed the row for itself
	// and waits, until its park records the claim, and givenUp whether its park left it no lane,
	// the park's timer to start once it takes another (serve).
	last    int
	done    bool
	ended   *cmTry
	givenUp bool
	// lane is the lane a lane goroutine serves (serve's lane), and carried the send it carries
	// (serve's item): set as next hands them over.
	lane    laneKey
	carried mentionSend
	// answered is a re-read this goroutine has been answered and not yet acted on under the
	// lock, and readFailed whether that answer was a failure.
	answered   *cmRead
	readFailed bool
	// tried is that the try of the send it carries has answered, from that answer until it starts
	// another try or next hands it its next send: a sender whose serve reaches one lock site both
	// before a try and after it, as one that reads an unknown claim again before its park does,
	// acts there on which it is.
	tried bool
}

// cmSweep is the delivery sweep between its two statements: it claimed comment's attempt, which
// waits behind an earlier event, and hands the send over next (resumeOwedDelivery).
type cmSweep struct {
	item mentionSend
}

type cmArrival struct {
	refresh bool
	lock    chan struct{}
	site    string
	item    mentionSend
	try     chan cmAnswer
	queued  []mentionClaim
	stmt    chan cmWritten
	named   []mentionClaim
	reread  chan cmReadAnswer
}

// cmLock is the sender's lock under the model: the mutex that guards the sender's state, taken
// by every caller, and, for the sender's own goroutines - those serving a lane, and the refresh -
// a wait for the model to let it be taken, which makes each locked section a step. The model's
// own calls into the sender (enqueue, unpark, wait) take the mutex alone, and its reads of the
// sender's state take the embedded mutex directly. A lock taken from any other function of the
// sender panics, naming it: a locked section the model does not know of would otherwise run
// outside every step, and the search would miss each window it opens.
type cmLock struct {
	sync.Mutex
	r *cmRun
}

func (l *cmLock) Lock() {
	switch site := cmCaller(1); {
	case site.lane || site.refresh:
		grant := make(chan struct{})
		select {
		case l.r.arrivals <- cmArrival{refresh: site.refresh, lock: grant, site: site.name}:
			select {
			case <-grant:
			case <-l.r.done:
			}
		case <-l.r.done:
		}
	case !site.model:
		panic("the claim model: the sender takes its lock at " + site.name +
			", which is neither a step of a lane goroutine or the refresh nor a call of the model's own (cmCaller)")
	}
	l.Mutex.Lock()
}

// cmSite is where a call came from: the function and line, and whose goroutine makes it - a lane
// goroutine's (serve), the refresh's (refreshHeldClaims), or the model's own call. Each of the
// sender's functions that takes its lock, or calls its reread, runs on one of them only.
type cmSite struct {
	name                 string
	lane, refresh, model bool
}

// cmSites caches cmCaller's answer by program counter.
var cmSites sync.Map

// cmCaller is the site of the function skip frames above cmCaller's caller, classed by the list of
// the sender's lock sites (mentionSenderLockSites), which TestTheLockSiteListNamesTheSendersLocks
// holds to the sender's source in every run of the package.
//
//go:noinline
func cmCaller(skip int) cmSite {
	var pc [1]uintptr
	if runtime.Callers(skip+2, pc[:]) == 0 {
		return cmSite{}
	}
	if site, ok := cmSites.Load(pc[0]); ok {
		return site.(cmSite)
	}
	frame, _ := runtime.CallersFrames(pc[:]).Next()
	function := frame.Function[strings.LastIndex(frame.Function, ".")+1:]
	site := cmSite{name: function + ":" + strconv.Itoa(frame.Line)}
	switch mentionSenderLockSites[function] {
	case lockSiteLane:
		site.lane = true
	case lockSiteRefresh:
		site.refresh = true
	case lockSiteModel:
		site.model = true
	}
	cmSites.Store(pc[0], site)
	return site
}

// cmRun is one run of the model in its own bubble.
type cmRun struct {
	// fault is the model's own failure: a step its bookkeeping cannot follow, which stops the run.
	fault    string
	v        cmVariant
	slots    int
	sender   *mentionSender
	lock     *cmLock
	stop     context.CancelFunc
	done     chan struct{}
	arrivals chan cmArrival
	// mu guards started, the park timers the sender started in the step being taken.
	mu               sync.Mutex
	started          []func()
	timer            *cmTimer
	slot             int
	seq              time.Duration
	published        bool
	publishedBetween bool
	readFailures     int
	outages          int
	rows             [2]cmRow
	took             []int
	// sweptFirst is that the delivery sweep has claimed A-1's attempt, whose claim lapsed (owed).
	sweptFirst bool
	// gs is the sender's live goroutines in the order they started, and released the one the step
	// being taken let go on. A step lets one of them go on, or starts at most one - a commit's or a
	// hand-off's enqueue, a timer's unpark, a period's refresh - and the sender's goroutines start
	// none, so whatever comes to the lock or to a statement in a step is the one it let go on, or
	// else the one it started.
	gs       []*cmG
	released *cmG
	tries    map[int]*cmTry
	refresh  *cmRefresh
	sweeps   map[int]*cmSweep
	// broke is every check the run has broken, in order, and fresh those its last step broke.
	broke, fresh []string
	// tracing has the run write its trace: a replay of a run the search found breaking a check,
	// or failing the model, and no other, since the text is only read then.
	tracing bool
	trace   []string
}

//go:build !race

package api

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing/synctest"
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
// in flight began, which is all the code compares of it. A lane goroutine's lane variable is left
// out of the key, the sender's lanes and ready list standing for it.
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
//     and the sweep's hand-off take microseconds, so time moves on none while one waits;
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
// Its controls are in TestTheMentionSenderClaimModel.

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
	// revivesLapsed is the refresh statement before this change, which wrote a row whose recorded
	// claim had lapsed, and takeNeedsLive the take before it, which counted the row's claim the
	// send's own only while the claim it carried was itself live: the control the outages must
	// break.
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
	cmOldStatements = cmVariant{
		name:    "control: the same, with the refresh and the take before this change",
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
// own calls into the sender (enqueue, unpark, wait) and its reads of the sender's state take the
// mutex alone.
type cmLock struct {
	sync.Mutex
	r *cmRun
}

func (l *cmLock) Lock() {
	if site := cmCaller(1); site.lane || site.refresh {
		grant := make(chan struct{})
		select {
		case l.r.arrivals <- cmArrival{refresh: site.refresh, lock: grant, site: site.name}:
			select {
			case <-grant:
			case <-l.r.done:
			}
		case <-l.r.done:
		}
	}
	l.Mutex.Lock()
}

// cmSite is where a call came from: the function and line, and whose goroutine makes it - a lane
// goroutine's (serve), the refresh's (refreshHeldClaims), or neither, the model's own call. Each
// of the sender's functions that takes its lock, or calls its reread, runs on one of them only.
type cmSite struct {
	name          string
	lane, refresh bool
}

// cmSites caches cmCaller's answer by program counter.
var cmSites sync.Map

// cmCaller is the site of the function skip frames above cmCaller's caller.
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
	switch function {
	case "next", "claimKnown", "markTrying", "claimedDuring", "park", "reportLate":
		site.lane = true
	case "refreshHeldClaims", "rereadUnsure":
		site.refresh = true
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

func newCmRun(v cmVariant, periods int, tracing bool) *cmRun {
	lifetime, stop := context.WithCancel(context.Background())
	r := &cmRun{
		v: v, slots: periods * cmSlots, stop: stop, done: make(chan struct{}), tracing: tracing,
		arrivals: make(chan cmArrival, 16), tries: map[int]*cmTry{}, sweeps: map[int]*cmSweep{},
	}
	arrive := func(a cmArrival) bool {
		select {
		case r.arrivals <- a:
			return true
		case <-r.done:
			return false
		}
	}
	r.sender = newMentionSender(lifetime, 1000*time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			reply := make(chan cmAnswer, 1)
			if arrive(cmArrival{item: item, try: reply}) {
				select {
				case answer := <-reply:
					return answer.item, answer.waits
				case <-r.done:
				}
			}
			return mentionSend{}, false
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) {
			reply := make(chan cmWritten, 1)
			if arrive(cmArrival{refresh: true, queued: slices.Clone(queued), stmt: reply}) {
				select {
				case written := <-reply:
					return written.claims, written.err
				case <-r.done:
				}
			}
			return nil, nil
		},
		func(_ context.Context, named []mentionClaim) ([]mentionClaim, error) {
			reply := make(chan cmReadAnswer, 1)
			if arrive(cmArrival{refresh: cmCaller(1).refresh, named: slices.Clone(named), reread: reply}) {
				select {
				case read := <-reply:
					return read.claims, read.err
				case <-r.done:
				}
			}
			return nil, nil
		},
		func(context.Context, []mentionClaim) error { return nil },
	)
	// The sender's own goroutine so far, keepClaims, takes the lock only at its ticker's tick,
	// which a refresh period of 1,000 hours never brings.
	r.lock = &cmLock{r: r}
	r.sender.mu = r.lock
	r.sender.after = func(_ time.Duration, f func()) {
		r.mu.Lock()
		r.started = append(r.started, f)
		r.mu.Unlock()
	}
	r.commit(0)
	return r
}

func (r *cmRun) shutdown() {
	r.stop()
	close(r.done)
	r.sender.wait()
}

func (r *cmRun) log(format string, args ...any) {
	if r.tracing {
		r.trace = append(r.trace, fmt.Sprintf(format, args...))
	}
}

// failModel records the model's own failure, the first one, which stops the run.
func (r *cmRun) failModel(format string, args ...any) {
	if r.fault == "" {
		r.fault = "the model: " + fmt.Sprintf(format, args...)
	}
}

// now is a claim written now: in this slot, after every claim before it.
func (r *cmRun) now() time.Time {
	r.seq++
	return cmBase.Add(time.Duration(r.slot)*cmSlot + r.seq)
}

// cmSlotOf is the slot claim was written in.
func cmSlotOf(claim time.Time) int {
	return int(claim.Sub(cmBase) / cmSlot)
}

// age is how many slots ago claim was written.
func (r *cmRun) age(claim time.Time) int {
	return r.slot - cmSlotOf(claim)
}

// lapsed is whether claim has lapsed at every timing within the slots: written more than a lease
// and a slot ago. A claim a lease and part of a slot old may have lapsed or not.
func (r *cmRun) lapsed(claim time.Time) bool {
	return !claim.IsZero() && r.age(claim) > cmLapseSlots
}

// cmFree is model.DeliveryClaimFreeSQL on a row carrying claim, read now, at some timing within
// the slots: no claim, or one written a lease or more ago.
func (r *cmRun) cmFree(claim time.Time) bool {
	return claim.IsZero() || r.age(claim) >= cmLapseSlots
}

// cmOwns is model.DeliveryClaimSinceSQL(recorded) on a row carrying row, at some timing within the
// slots: a claim at or past recorded and less than a lease after it, the lease counted to the slot.
// The take (claimCommentDeliveryAttempt) and the re-read (rereadMentionClaims) both read it.
func cmOwns(row, recorded time.Time) bool {
	return !row.IsZero() && !row.Before(recorded) && cmSlotOf(row)-cmSlotOf(recorded) <= cmLapseSlots
}

// cmRefreshWrites is whether the refresh's statement (refreshMentionClaims) writes a row that is
// pending and carries row, naming named, read now: the row carries the claim named, which is live
// at some timing within the slots - or, for the control's refresh before this change, at all.
func (r *cmRun) cmRefreshWrites(row cmRow, named time.Time) bool {
	return row.pending && row.claim.Equal(named) && (r.v.revivesLapsed || !r.lapsed(named))
}

// owed is whether the delivery sweep takes comment c's attempt: pending, committed a lease ago,
// and with no live claim on it (model.OwedMentionAttemptSQL), each at every timing within the
// slots.
func (r *cmRun) owed(c int) bool {
	row := r.rows[c]
	return row.committed && row.pending && (row.claim.IsZero() || r.lapsed(row.claim)) && r.lapsed(row.created)
}

// name is claim as the trace writes it, and nothing when the run writes no trace.
func (r *cmRun) name(claim time.Time) string {
	switch {
	case !r.tracing:
		return ""
	case claim.IsZero():
		return "none"
	}
	slot := int(claim.Sub(cmBase) / cmSlot)
	return fmt.Sprintf("p%d.%d#%d", slot/cmSlots, slot%cmSlots, claim.Sub(cmBase)%cmSlot)
}

func (r *cmRun) names(claims []mentionClaim) string {
	if !r.tracing {
		return ""
	}
	var names []string
	for _, claim := range claims {
		names = append(names, fmt.Sprintf("%s %s", claim.key.comment, r.name(claim.claimedAt)))
	}
	slices.Sort(names)
	return "[" + strings.Join(names, ", ") + "]"
}

// gName is goroutine g as the trace writes it.
func (r *cmRun) gName(g *cmG) string {
	if g.refresh {
		return "the refresh"
	}
	if g.last < 0 {
		return fmt.Sprintf("lane goroutine %d", slices.Index(r.gs, g))
	}
	return fmt.Sprintf("lane goroutine %d (last %s)", slices.Index(r.gs, g), cmComments[g.last])
}

func cmIndex(comment string) int {
	return slices.Index(cmComments[:], comment)
}

func (r *cmRun) item(c int) mentionSend {
	session := "s1"
	return mentionSend{
		comment: model.Comment{ID: cmComments[c], IssueKey: new("T-1")},
		mention: ResolvedMention{Target: "session:s1", SessionID: &session}, attempt: 1,
	}
}

// commit is comment c committing its attempt claimed and queuing its send (createCommentFor).
func (r *cmRun) commit(c int) {
	now := r.now()
	r.rows[c] = cmRow{committed: true, pending: true, created: now, claim: now}
	item := r.item(c)
	item.claimed = now
	r.log("%s commits its attempt claimed %s and queues its send", cmComments[c], r.name(now))
	r.sender.enqueue(context.Background(), item)
	r.settle()
}

// deliver is s1 taking comment c, by road; bySweep is that the delivery sweep sent it itself. The
// sweep sends only an attempt whose claim has lapsed - its sender stopped, or wrote no claim for
// a lease - and sends the attempts it finds owed side by side (resumeOwedDeliveries), so it keeps
// no order between two comments: an A-2 it sends first is "lapsed", which the spec puts outside
// this change and the model counts apart.
func (r *cmRun) deliver(c int, road string, bySweep bool) {
	r.log("%s sends %s to s1", road, cmComments[c])
	switch {
	case slices.Contains(r.took, c):
		r.fail("twice", "s1 takes %s a second time", cmComments[c])
	case c == 1 && !slices.Contains(r.took, 0) && bySweep:
		r.fail("lapsed", "the delivery sweep sends A-2, whose claim lapsed, before A-1")
	case c == 1 && !slices.Contains(r.took, 0):
		r.fail("order", "s1 takes A-2 before A-1")
	}
	r.took = append(r.took, c)
	r.rows[c].pending = false
}

// fail records that the step being taken broke check kind, unless the run broke it before.
func (r *cmRun) fail(kind, format string, args ...any) {
	if slices.Contains(r.broke, kind) {
		return
	}
	r.broke = append(r.broke, kind)
	r.fresh = append(r.fresh, kind)
	r.log("BROKEN (%s): %s", kind, fmt.Sprintf(format, args...))
}

// ended is whether the run stops here: s1 took the comments out of order or one of them twice.
func (r *cmRun) ended() bool {
	return slices.Contains(r.broke, "order") || slices.Contains(r.broke, "twice")
}

// runRead is a re-read's statement (rereadMentionClaims): each row named that is pending and
// carries a claim this sender owns by the one named (cmOwns).
func (r *cmRun) runRead(read *cmRead) {
	read.ran = true
	if r.v.blind {
		return
	}
	for _, claim := range read.named {
		row := r.rows[cmIndex(claim.key.comment)]
		if row.pending && cmOwns(row.claim, claim.claimedAt) {
			read.found = append(read.found, mentionClaim{key: claim.key, claimedAt: row.claim})
		}
	}
}

// runTry is a try's statements (sendCommentMention): a send that waited before reads the rule
// alone and, while an earlier event still owes s1 a send, writes nothing; otherwise the try
// claims the attempt as claimCommentDeliveryAttempt does - the row's claim free (cmFree), or one
// the send owns (cmOwns) - and then waits behind an earlier event or sends.
func (r *cmRun) runTry(try *cmTry) {
	try.ran = true
	item := try.item
	c := cmIndex(item.comment.ID)
	if _, waits := r.holder(c); item.heldFor != "" && waits {
		try.answer = cmAnswer{item: item, waits: true}
		r.log("%s's try reads the rule alone: an earlier event still owes s1, so it writes nothing", cmComments[c])
		return
	}
	row := &r.rows[c]
	if !row.pending {
		r.log("%s's try finds its attempt settled and sends nothing", cmComments[c])
		return
	}
	mine := cmOwns(row.claim, item.claimed)
	if r.v.takeNeedsLive {
		mine = !row.claim.Before(item.claimed) && !r.lapsed(item.claimed)
	}
	if !r.cmFree(row.claim) && !mine {
		r.log("%s's try carries claim %s and finds the row's %s: the attempt is another sender's, and the send ends unsent",
			cmComments[c], r.name(item.claimed), r.name(row.claim))
		return
	}
	row.claim = r.now()
	try.took = true
	if by, waits := r.holder(c); waits {
		item.claimed = row.claim
		item.held(&mentionHeldForOrder{target: item.mention, claim: attemptClaim{attempt: 1, claimedAt: row.claim}, by: by})
		try.answer = cmAnswer{item: item, waits: true}
		r.log("%s's try claims its attempt %s and waits behind event %d", cmComments[c], r.name(row.claim), by.EventID)
		return
	}
	r.deliver(c, fmt.Sprintf("%s's try claims its attempt %s and", cmComments[c], r.name(row.claim)), false)
}

// sweepClaims is the delivery sweep's first statement on comment c's owed attempt
// (resumeOwedDelivery, deliverResolvedCommentMention): it claims it, and sends it, or, while an
// earlier event still owes s1 a send, goes on to hand it to the sender.
func (r *cmRun) sweepClaims(c int) {
	row := &r.rows[c]
	row.claim = r.now()
	by, waits := r.holder(c)
	if !waits {
		r.deliver(c, fmt.Sprintf("the delivery sweep claims %s's owed attempt %s and", cmComments[c], r.name(row.claim)), true)
		return
	}
	item := r.item(c)
	item.claimed = row.claim
	item.held(&mentionHeldForOrder{target: item.mention, claim: attemptClaim{attempt: 1, claimedAt: row.claim}, by: by})
	r.sweeps[c] = &cmSweep{item: item}
	r.log("the delivery sweep claims %s's owed attempt %s, which waits behind event %d", cmComments[c], r.name(row.claim),
		by.EventID)
}

func (r *cmRun) lanes() map[laneKey][]mentionKey {
	r.lock.Mutex.Lock()
	defer r.lock.Mutex.Unlock()
	lanes := map[laneKey][]mentionKey{}
	for lane, sends := range r.sender.lanes {
		for _, send := range sends {
			lanes[lane] = append(lanes[lane], send.key())
		}
	}
	return lanes
}

// start is the record of a goroutine the step being taken started: the refresh, or a lane
// goroutine serving the one lane no other goroutine is on, none waits for one in ready, and none
// is parked, its timer pending or about to start (queueLocked, unpark).
func (r *cmRun) start(refresh bool) *cmG {
	g := &cmG{refresh: refresh, born: r.slot, last: -1}
	r.gs = append(r.gs, g)
	if refresh {
		r.refresh = &cmRefresh{}
		return g
	}
	r.lock.Mutex.Lock()
	defer r.lock.Mutex.Unlock()
	var served []laneKey
	for lane := range r.sender.lanes {
		taken := slices.Contains(r.sender.ready, lane)
		for _, other := range r.gs {
			taken = taken || other != g && !other.refresh && (other.lane == lane || other.givenUp && lane.held)
		}
		if !taken && !(r.timer != nil && lane.held) {
			served = append(served, lane)
		}
	}
	if len(served) != 1 {
		r.failModel("a lane goroutine started, on none or several of the lanes %v", served)
		return g
	}
	g.lane = served[0]
	return g
}

// settle waits for every goroutine of the bubble to stop, then takes in where the one the step
// let go on, or started, came to - the sender's lock, or one of its statements - and the park
// timer the step started, and drops the goroutine it let go on when it came nowhere, which has
// ended.
func (r *cmRun) settle() {
	synctest.Wait()
	var arrived []cmArrival
	for more := true; more; {
		select {
		case a := <-r.arrivals:
			arrived = append(arrived, a)
		default:
			more = false
		}
	}
	g := r.released
	r.released = nil
	switch {
	case len(arrived) > 1:
		r.failModel("%d of the sender's goroutines came somewhere in one step", len(arrived))
		return
	case len(arrived) == 1 && g == nil:
		g = r.start(arrived[0].refresh)
	case len(arrived) == 1 && g.refresh != arrived[0].refresh:
		r.failModel("the goroutine the step let go on came back as another")
		return
	}
	for _, a := range arrived {
		g.running = false
		switch {
		case a.lock != nil:
			g.lock, g.site = a.lock, a.site
		case a.try != nil:
			c := cmIndex(a.item.comment.ID)
			if r.tries[c] != nil || g.refresh {
				r.failModel("a second try of %s in flight", a.item.comment.ID)
				return
			}
			r.tries[c] = &cmTry{g: g, began: r.slot, item: a.item, reply: a.try}
			g.last, g.done, g.carried = c, false, a.item
			r.log("%s starts %s's try, carrying claim %s", r.gName(g), a.item.comment.ID, r.name(a.item.claimed))
		case a.reread != nil && g.refresh:
			r.refresh.read = r.newRead(a.named, a.reread)
			r.log("the refresh re-reads the unknown claims %s", r.names(a.named))
		case a.reread != nil:
			c := cmIndex(a.named[0].key.comment)
			if r.tries[c] != nil {
				r.failModel("a second try of %s in flight", a.named[0].key.comment)
				return
			}
			r.tries[c] = &cmTry{g: g, began: r.slot, read: r.newRead(a.named, a.reread)}
			g.last = c
			r.log("%s re-reads %s's unknown claim %s", r.gName(g), cmComments[c], r.names(a.named))
		case a.stmt != nil:
			r.refresh.stmt = &cmStatement{queued: a.queued, reply: a.stmt}
			r.refresh.named = true
			r.log("the refresh's statement names %s", r.names(a.queued))
		}
	}
	r.gs = slices.DeleteFunc(r.gs, func(g *cmG) bool {
		if g.running && g.refresh {
			r.refresh = nil
		}
		return g.running
	})
	r.mu.Lock()
	started := r.started
	r.started = nil
	r.mu.Unlock()
	switch {
	case len(started) > 1 || len(started) == 1 && r.timer != nil:
		r.failModel("a second park timer for the held lane")
	case len(started) == 1:
		r.timer = &cmTimer{run: started[0], began: r.slot}
	}
}

// release lets goroutine g go on: the one the step lets go on.
func (r *cmRun) release(g *cmG) {
	if r.released != nil {
		r.failModel("a step let two goroutines go on")
	}
	g.running = true
	r.released = g
}

func (r *cmRun) apply(s cmStep) {
	if r.fault != "" {
		return
	}
	r.fresh = nil
	switch s.kind {
	case cmCommit:
		r.commit(s.comment)
	case cmLockRuns:
		g := r.gs[s.g]
		grant := g.lock
		g.lock = nil
		// claimKnown, after its re-read answered, and claimedDuring end with the send carrying the
		// claim this sender records for it, when it records one.
		recarries := strings.HasPrefix(g.site, "claimKnown:") && g.answered != nil && !g.readFailed ||
			strings.HasPrefix(g.site, "claimedDuring:")
		// The goroutine acts under this lock on what it was answered last.
		g.answered, g.readFailed = nil, false
		parking := strings.HasPrefix(g.site, "park:")
		if parking {
			// The park records the claim the try took (park).
			g.ended = nil
		}
		if g.refresh && r.refresh != nil && r.refresh.stmt != nil && r.refresh.stmt.answered {
			// The refresh records what its statement answered under this lock.
			r.refresh.stmt = nil
		}
		// next hands the goroutine the head of one lane, which is then the lane it serves.
		heads := map[laneKey]mentionSend{}
		if strings.HasPrefix(g.site, "next:") {
			r.lock.Mutex.Lock()
			for lane, sends := range r.sender.lanes {
				if len(sends) > 0 {
					heads[lane] = sends[0]
				}
			}
			r.lock.Mutex.Unlock()
		}
		r.log("%s takes the lock at %s", r.gName(g), g.site)
		r.release(g)
		hadTimer := r.timer != nil
		close(grant)
		r.settle()
		if g.running {
			break
		}
		if len(heads) > 0 {
			r.lock.Mutex.Lock()
			for lane, head := range heads {
				if sends := r.sender.lanes[lane]; len(sends) == 0 || sends[0].key() != head.key() {
					head.claimed = r.sender.held[head.key()].claimed
					g.lane, g.carried, g.givenUp = lane, head, false
				}
			}
			r.lock.Mutex.Unlock()
		}
		if recarries {
			r.lock.Mutex.Lock()
			if state, mine := r.sender.held[g.carried.key()]; mine {
				g.carried.claimed = state.claimed
			}
			r.lock.Mutex.Unlock()
		}
		switch {
		case parking && r.timer == nil:
			g.givenUp, g.lane = true, laneKey{}
		case !hadTimer && r.timer != nil:
			g.givenUp = false
		}
	case cmRereadRuns:
		r.runRead(r.tries[s.comment].read)
		r.log("%s's re-read finds %s", cmComments[s.comment], r.names(r.tries[s.comment].read.found))
	case cmRereadAnswers, cmRereadFails:
		try := r.tries[s.comment]
		delete(r.tries, s.comment)
		answer := cmReadAnswer{claims: try.read.found}
		if s.kind == cmRereadFails {
			r.readFailures++
			answer = cmReadAnswer{err: errCmLost}
			r.log("%s's re-read fails: its claim is still unknown", cmComments[s.comment])
		} else {
			r.log("%s's re-read answers", cmComments[s.comment])
		}
		try.g.answered, try.g.readFailed = try.read, s.kind == cmRereadFails
		r.release(try.g)
		try.read.reply <- answer
		r.settle()
	case cmTryRuns:
		r.runTry(r.tries[s.comment])
	case cmTryAnswers:
		try := r.tries[s.comment]
		delete(r.tries, s.comment)
		try.g.done = !try.answer.waits
		try.g.ended = nil
		if try.took && try.answer.waits {
			try.g.ended = try
		}
		if try.answer.waits {
			r.log("%s's try answers that it waits, under claim %s", cmComments[s.comment], r.name(try.answer.item.claimed))
		} else {
			r.log("%s's try answers that it is done", cmComments[s.comment])
		}
		r.release(try.g)
		try.reply <- try.answer
		r.settle()
	case cmTimerRuns:
		timer := r.timer
		r.timer = nil
		r.log("the timer of the parked held lane runs")
		timer.run()
		r.settle()
	case cmTimePasses:
		r.slot++
		if r.slot%cmSlots != 0 {
			r.log("time passes to period %d, slot %d", r.slot/cmSlots, r.slot%cmSlots)
			break
		}
		r.log("period %d begins: the sender's loop starts a refresh", r.slot/cmSlots)
		go r.sender.refreshHeldClaims(context.Background())
		r.settle()
	case cmRefreshRereadRuns:
		r.runRead(r.refresh.read)
		r.log("the refresh's re-read finds %s", r.names(r.refresh.read.found))
	case cmRefreshRereadAnswers, cmRefreshRereadFails:
		answer := cmReadAnswer{claims: r.refresh.read.found}
		if s.kind == cmRefreshRereadFails {
			r.readFailures++
			answer = cmReadAnswer{err: errCmLost}
			r.log("the refresh's re-read fails: those claims stay unknown")
		} else {
			r.log("the refresh's re-read answers")
		}
		reply := r.refresh.read.reply
		g := r.refreshG()
		g.answered, g.readFailed = r.refresh.read, s.kind == cmRefreshRereadFails
		r.refresh.read = nil
		r.release(g)
		reply <- answer
		r.settle()
	case cmRefreshWrites:
		stmt := r.refresh.stmt
		now := r.now()
		var names []string
		for _, claim := range stmt.queued {
			row := &r.rows[cmIndex(claim.key.comment)]
			if r.cmRefreshWrites(*row, claim.claimedAt) {
				row.claim = now
				stmt.written = append(stmt.written, mentionClaim{key: claim.key, claimedAt: now})
				names = append(names, claim.key.comment)
			}
		}
		slices.Sort(names)
		stmt.wrote = true
		r.log("the refresh's statement commits, writing %s on %v", r.name(now), names)
	case cmRefreshFailsUncommitted:
		r.outages++
		r.log("the refresh's statement fails before it commits: it writes nothing")
		// The statement wrote nothing and answers an error, which the refresh acts on under its
		// next lock as it does a lost answer.
		stmt := r.refresh.stmt
		stmt.answered, stmt.lost = true, true
		r.release(r.refreshG())
		stmt.reply <- cmWritten{err: errCmLost}
		r.settle()
	case cmRefreshRecords, cmRefreshAnswerLost:
		stmt := r.refresh.stmt
		answer := cmWritten{claims: stmt.written}
		if s.kind == cmRefreshAnswerLost {
			answer = cmWritten{err: errCmLost}
			r.log("the refresh's answer is lost after its statement committed")
		} else {
			r.log("the refresh's answer arrives")
		}
		stmt.answered, stmt.lost = true, s.kind == cmRefreshAnswerLost
		r.release(r.refreshG())
		stmt.reply <- answer
		r.settle()
	case cmPublish:
		r.published = true
		r.log("the outbox sends the earlier event to s1")
	case cmPublishBetween:
		r.publishedBetween = true
		r.log("the outbox sends the event between the comments to s1")
	case cmSweepClaims:
		r.sweepClaims(s.comment)
	case cmSweepHandsOver:
		sweep := r.sweeps[s.comment]
		delete(r.sweeps, s.comment)
		r.log("the delivery sweep hands %s, claim %s, to the sender", cmComments[s.comment], r.name(sweep.item.claimed))
		r.sender.enqueue(context.Background(), sweep.item)
		r.settle()
	}
	if r.fault != "" {
		return
	}
	if !slices.Contains(r.broke, "stale") {
		r.checkClaims()
	}
	if !slices.Contains(r.broke, "dropped") {
		r.checkDropped()
	}
	if !slices.Contains(r.broke, "unnamed") {
		r.checkHeldReport()
	}
}

// refreshG is the refresh's goroutine.
func (r *cmRun) refreshG() *cmG {
	for _, g := range r.gs {
		if g.refresh {
			return g
		}
	}
	r.failModel("no refresh goroutine")
	return &cmG{}
}

// checkHeldReport is the unnamed check: every held lane's wait names the earlier event its send
// waited behind, and every queued send that waited, and has had no WARN, names a real earlier event
// through what its late WARN would report (reportedWaitLocked): a send parked with no event to
// wait behind - its claim could not be read - is no held copy, and a WARN of it would name event 0.
func (r *cmRun) checkHeldReport() {
	r.lock.Mutex.Lock()
	defer r.lock.Mutex.Unlock()
	for lane, wait := range r.sender.waits {
		if wait.by.EventID == 0 {
			r.fail("unnamed", "the held lane's wait (%v) names event 0, which does not exist", lane.held)
			return
		}
	}
	for lane, sends := range r.sender.lanes {
		for _, send := range sends {
			if send.heldSince.IsZero() || send.warned {
				continue
			}
			if wait := r.sender.reportedWaitLocked(lane, send); wait.by.EventID == 0 {
				r.fail("unnamed", "%s waits on the %s since it parked, and a WARN of it would name event 0, which does not exist",
					send.comment.ID, cmLaneName(lane))
				return
			}
		}
	}
}

func cmLaneName(lane laneKey) string {
	if lane.held {
		return "held lane"
	}
	return "own lane"
}

// checkClaims is the stale check: every claim the sender records and trusts is its row's, unless a
// refresh has written rows it has not recorded yet, a try has claimed the row for itself and not
// parked yet, or the sweep is handing the attempt over.
func (r *cmRun) checkClaims() {
	if r.refresh != nil && r.refresh.stmt != nil && r.refresh.stmt.wrote {
		return
	}
	r.lock.Mutex.Lock()
	claims := map[mentionKey]time.Time{}
	for key, state := range r.sender.held {
		if !state.unsure {
			claims[key] = state.claimed
		}
	}
	r.lock.Mutex.Unlock()
	for key, claimed := range claims {
		c := cmIndex(key.comment)
		row := r.rows[c]
		if !row.pending || row.claim.Equal(claimed) || r.sweeps[c] != nil {
			continue
		}
		if try := r.tries[c]; try != nil && try.took {
			continue
		}
		if slices.ContainsFunc(r.gs, func(g *cmG) bool { return g.ended != nil && g.last == c }) {
			continue
		}
		r.fail("stale", "the sender trusts %s's claim as %s while its row carries %s: no refresh writes that row again",
			key.comment, r.name(claimed), r.name(row.claim))
		return
	}
}

// checkDropped is the dropped check: an attempt pending under a claim live at every timing is held
// by the sender, or the sweep is handing it over; otherwise nothing sends it until the claim
// lapses and the sweep takes it, a lease on.
func (r *cmRun) checkDropped() {
	for c, row := range r.rows {
		if !row.committed || !row.pending || r.cmFree(row.claim) || r.sweeps[c] != nil {
			continue
		}
		r.lock.Mutex.Lock()
		_, held := r.sender.held[r.item(c).key()]
		r.lock.Mutex.Unlock()
		if !held {
			r.fail("dropped", "%s is pending under the live claim %s, and the sender holds no send of it", cmComments[c],
				r.name(row.claim))
			return
		}
	}
}

// enabled is every step the run may take next, but for those it defers while a locked section
// waits: an outbox send (cmPublish, cmPublishBetween) and a re-read's reading of the rows
// (cmRereadRuns, cmRefreshRereadRuns). A locked section reads neither the outbox nor the rows, and
// those steps change only what no locked section reads and no check compares, so taking one of
// them before a locked section reaches the state taking it after does, through states that break
// the same checks; and a locked section always ends, its goroutine coming to a statement or
// ending, so each of them is taken in time.
func (r *cmRun) enabled() []cmStep {
	if r.ended() || r.fault != "" {
		return nil
	}
	var steps []cmStep
	if !r.rows[1].committed {
		steps = append(steps, cmStep{kind: cmCommit, comment: 1})
	}
	mayFail := r.readFailures < r.v.readFailures
	// Time moves on only while nothing that began a slot ago is still to end, and while no
	// locked section and no hand-off waits, which take microseconds.
	locks := false
	waits := len(r.sweeps) > 0
	for i, g := range r.gs {
		if g.lock != nil {
			steps = append(steps, cmStep{kind: cmLockRuns, g: i})
			waits, locks = true, true
		}
		if g.refresh && g.born < r.slot {
			waits = true
		}
	}
	for c := range 2 {
		try := r.tries[c]
		if try == nil {
			continue
		}
		waits = waits || try.began < r.slot
		switch {
		case try.read != nil && !try.read.ran:
			if !locks {
				steps = append(steps, cmStep{kind: cmRereadRuns, comment: c})
			}
		case try.read != nil:
			steps = append(steps, cmStep{kind: cmRereadAnswers, comment: c})
			if mayFail {
				steps = append(steps, cmStep{kind: cmRereadFails, comment: c})
			}
		case !try.ran:
			steps = append(steps, cmStep{kind: cmTryRuns, comment: c})
		default:
			steps = append(steps, cmStep{kind: cmTryAnswers, comment: c})
		}
	}
	if r.timer != nil {
		steps = append(steps, cmStep{kind: cmTimerRuns})
		waits = waits || r.timer.began < r.slot
	}
	if f := r.refresh; f != nil {
		switch {
		case f.read != nil && !f.read.ran:
			if !locks {
				steps = append(steps, cmStep{kind: cmRefreshRereadRuns})
			}
		case f.read != nil:
			steps = append(steps, cmStep{kind: cmRefreshRereadAnswers})
			if mayFail {
				steps = append(steps, cmStep{kind: cmRefreshRereadFails})
			}
		case f.stmt == nil || f.stmt.answered:
		case !f.stmt.wrote:
			steps = append(steps, cmStep{kind: cmRefreshWrites})
			if r.outages < r.v.outages {
				steps = append(steps, cmStep{kind: cmRefreshFailsUncommitted})
			}
		default:
			if !r.v.alwaysLoses {
				steps = append(steps, cmStep{kind: cmRefreshRecords})
			}
			steps = append(steps, cmStep{kind: cmRefreshAnswerLost})
		}
	}
	if !waits && r.slot+1 < r.slots && (r.refresh == nil || (r.slot+1)%cmSlots != 0) {
		steps = append(steps, cmStep{kind: cmTimePasses})
	}
	if !r.published && !locks {
		steps = append(steps, cmStep{kind: cmPublish})
	}
	if !r.publishedBetween && !locks {
		steps = append(steps, cmStep{kind: cmPublishBetween})
	}
	for c := range 2 {
		switch {
		case r.sweeps[c] != nil:
			steps = append(steps, cmStep{kind: cmSweepHandsOver, comment: c})
		case r.owed(c):
			steps = append(steps, cmStep{kind: cmSweepClaims, comment: c})
		}
	}
	return steps
}

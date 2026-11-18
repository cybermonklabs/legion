//go:build !race

package api

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The mention sender's claim model: every interleaving, within small bounds, of the package's own
// mention sender (newMentionSender) - its lanes, its parks and their timers, its claim refresh
// (refreshHeldClaims) and its re-reads of claims a lost refresh may have written (rereadUnsure,
// claimKnown) - with the statements its sends, its refresh and its re-reads make on two comments'
// attempt rows, the delivery sweep, and the outbox sending the earlier event both comments wait
// behind. It is the sender's part of the mention-copy order model; the outbox's part is
// TestTheMentionCopyOrderModel in internal/dispatch/outbox.
//
// The sender runs inside a testing/synctest bubble, so the model knows when every goroutine it
// started has stopped, and each of them stops only where the model holds it: inside the sender's
// send, refresh or reread function. Each step of the model is one thing that may happen next,
// and the search tries every one from every state it reaches, the sender's own code running
// between them. The search keys on the sender's maps and the model's own state, every claim in it
// named by its order among the claims in play and its age, which is all the code compares
// (model.DeliveryClaimMineSQL, model.DeliveryClaimFreeSQL).
//
// What the model holds:
//   - comments A-1 and A-2 of issue T-1, each mentioning session s1 under attempt 1, which commits
//     claimed and is queued on the sender (createCommentFor, enqueue): A-1 at the start, A-2 at
//     any step;
//   - one earlier event of T-1 that s1 must take first, until the outbox sends it, at any step;
//   - the rows' claims, and the statements on them, restated from their SQL: a send's try
//     (sendCommentMention), which reads the rule alone for a send that waited before and otherwise
//     claims the attempt as claimCommentDeliveryAttempt does - its own live claim with the row's
//     at or past it, or a free one - then waits for the earlier event or sends; the refresh's
//     statement (refreshMentionClaims), which writes only a row that carries the claim the sender
//     names, and whose answer may be lost once the statement has committed, as a connection that
//     drops after the commit loses it; the re-read (rereadMentionClaims), which answers a row's
//     claim while it is at or past the claim named and less than a lease after it
//     (model.DeliveryClaimSinceSQL), and which may fail once a run; and the delivery sweep
//     (resumeOwedDelivery), which takes an attempt committed a lease ago whose claim lapsed, and
//     sends it or hands it back to the sender;
//   - time in slots of half a claim-refresh period (mentionClaimRefresh), which a step of its own
//     ends: the sender's loop (keepClaims) starts a refresh as every period begins, a claim lapses
//     one lease (model.DeliveryClaimLapse, cmLapsePeriods periods) after it was written, and every
//     thing that takes milliseconds or seconds - a try and its re-read, the refresh's re-read,
//     statement and record, a park's timer, at most two seconds (mentionOrderRetryMax) - ends no
//     later than the slot after the one it began in, so time moves on no further while one is
//     still to end;
//   - a claim and the moment it is read are each known to a slot, so a claim read one lease and
//     part of a slot after it was written may be live or lapsed: the model reads it as the sender's
//     own statements would have it - live, or free when another holds it - and as the sweep would
//     not, so every send it finds refused, and every attempt the sweep takes, is so at any timing.
//
// What it checks, in every state, carrying each run on past a stale claim or a dropped send to
// what follows from it:
//   - stale: every claim the sender records for an attempt and trusts is the claim the attempt's
//     row carries, whenever no refresh has written rows it has not recorded yet and no try has
//     claimed that row for itself. A claim a lost answer left unknown (mentionClaimState.unsure)
//     is not trusted, and the sender reads its row before it acts on it. A trusted record behind
//     its row is never written again, since the refresh writes only a row that carries the claim
//     the sender names, so the row's claim lapses with its send still held;
//   - dropped: no try of a send the sender holds finds its attempt held by another sender;
//   - order and twice: s1 takes A-1 before A-2, and each of them once. Either ends the run.
//
// What it leaves out: the drain (wait), a keyed repeat (replayComment), the queue's bound, more
// than one recipient or owner, and a send or a statement of the sweep's that fails.
//
// The control loses every refresh's answer and has every re-read answer no row: a sender that
// learns nothing of what its refresh wrote, which the model must see as a stale claim, a dropped
// send and A-2 before A-1.

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

var cmEarlier = model.MentionCopyHolder{EventID: 1}

var cmChecks = []string{"stale", "dropped", "order", "twice"}

// errCmLost is a statement's answer the model loses, or a read it fails.
var errCmLost = errors.New("the model loses the answer: the connection dropped")

type cmStepKind uint8

const (
	cmCommit cmStepKind = iota
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
	cmRefreshRecords
	cmRefreshAnswerLost
	cmPublish
	cmSweep
)

// cmStep is one step of a run: its kind, the comment a commit, a try, its re-read or a sweep is
// of, and whether the lane whose park a timer ends is a held lane (cmLane). It holds no pointer,
// so the search's queue of runs to replay is memory the garbage collector does not scan.
type cmStep struct {
	kind    cmStepKind
	comment int
	held    bool
}

// cmLane is the model's one recipient's own lane, or its one owner's held lane.
func cmLane(held bool) laneKey {
	own := laneKey{recipient: "session:s1"}
	if held {
		return heldLane(own, "T-1")
	}
	return own
}

type cmVariant struct {
	name string
	// alwaysLoses loses every refresh's answer; otherwise each may be lost or arrive, so a run
	// with every answer lost is among the runs explored.
	alwaysLoses bool
	// readFailures is how many re-reads of one run may fail.
	readFailures int
	// blind has every re-read answer no row: the control's sender learns nothing of a lost write.
	blind bool
}

var (
	cmAsBuilt     = cmVariant{name: "the sender as built: any refresh answer lost, one re-read failed", readFailures: 1}
	cmBlindRepair = cmVariant{
		name: "control: every refresh answer lost, every re-read answers no row", alwaysLoses: true, blind: true,
	}
)

// cmRow is a comment's attempt row: whether it committed and is still pending, when it committed,
// and the claim it carries (zero for none).
type cmRow struct {
	committed, pending bool
	created, claim     time.Time
}

// cmRead is a re-read in flight: the claims it names, and, once it read the rows, what it found.
type cmRead struct {
	named []mentionClaim
	reply chan cmReadAnswer
	ran   bool
	found []mentionClaim
}

type cmReadAnswer struct {
	claims []mentionClaim
	err    error
}

// cmTry is a send the sender has started: the lane the goroutine serves, the slot it started in,
// the re-read of its claim while one is in flight, and once its try began, the item it was
// handed and, once its statements ran, its answer and whether they claimed the row for itself.
type cmTry struct {
	lane   laneKey
	began  int
	read   *cmRead
	trying bool
	// atPark is a re-read the send makes after a try that waited and took no claim, before it
	// parks, its claim having turned unknown while the try was in flight.
	atPark bool
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

// cmRefresh is the refresh in flight: the slot it began in, its re-read while one is in flight,
// the claims its statement names, and, once its statement ran, what the statement wrote.
type cmRefresh struct {
	began   int
	read    *cmRead
	queued  []mentionClaim
	reply   chan cmWritten
	wrote   bool
	written []mentionClaim
}

type cmWritten struct {
	claims []mentionClaim
	err    error
}

// cmTimer is a park's timer the sender started, and the slot it started in.
type cmTimer struct {
	run   func()
	began int
}

type cmArrival struct {
	item    mentionSend
	try     chan cmAnswer
	queued  []mentionClaim
	refresh chan cmWritten
	named   []mentionClaim
	reread  chan cmReadAnswer
}

// cmRun is one run of the model in its own bubble.
type cmRun struct {
	// fault is the model's own failure: a step its bookkeeping cannot follow, which stops the run.
	fault    string
	v        cmVariant
	slots    int
	sender   *mentionSender
	stop     context.CancelFunc
	done     chan struct{}
	arrivals chan cmArrival
	// mu guards started, the park timers the sender started in the step being taken.
	mu      sync.Mutex
	started []func()
	timers  map[laneKey]cmTimer
	// refreshing is that the step being taken runs the refresh's goroutine.
	refreshing   bool
	slot         int
	seq          time.Duration
	published    bool
	readFailures int
	rows         [2]cmRow
	took         []int
	tries        map[int]*cmTry
	refresh      *cmRefresh
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
		arrivals: make(chan cmArrival, 16), timers: map[laneKey]cmTimer{}, tries: map[int]*cmTry{},
	}
	r.sender = newMentionSender(lifetime, 1000*time.Hour,
		func(_ context.Context, item mentionSend) (mentionSend, bool) {
			reply := make(chan cmAnswer, 1)
			r.arrivals <- cmArrival{item: item, try: reply}
			select {
			case answer := <-reply:
				return answer.item, answer.waits
			case <-r.done:
				return mentionSend{}, false
			}
		},
		func(_ context.Context, queued []mentionClaim) ([]mentionClaim, error) {
			reply := make(chan cmWritten, 1)
			r.arrivals <- cmArrival{queued: slices.Clone(queued), refresh: reply}
			select {
			case written := <-reply:
				return written.claims, written.err
			case <-r.done:
				return nil, nil
			}
		},
		func(_ context.Context, named []mentionClaim) ([]mentionClaim, error) {
			reply := make(chan cmReadAnswer, 1)
			r.arrivals <- cmArrival{named: slices.Clone(named), reread: reply}
			select {
			case read := <-reply:
				return read.claims, read.err
			case <-r.done:
				return nil, nil
			}
		},
		func(context.Context, []mentionClaim) error { return nil },
	)
	r.sender.mu.Lock()
	r.sender.after = func(_ time.Duration, f func()) {
		r.mu.Lock()
		r.started = append(r.started, f)
		r.mu.Unlock()
	}
	r.sender.mu.Unlock()
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

// mayHaveLapsed is whether claim has lapsed at some timing within the slots: none, or written a
// lease or more ago.
func (r *cmRun) mayHaveLapsed(claim time.Time) bool {
	return claim.IsZero() || r.age(claim) >= cmLapseSlots
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
	r.settle(nil)
}

// deliver is s1 taking comment c, by road.
func (r *cmRun) deliver(c int, road string) {
	r.log("%s sends %s to s1", road, cmComments[c])
	switch {
	case slices.Contains(r.took, c):
		r.fail("twice", "s1 takes %s a second time", cmComments[c])
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
// carries a claim at or past the one named and less than a lease after it, at some timing within
// the slots.
func (r *cmRun) runRead(read *cmRead) {
	read.ran = true
	if r.v.blind {
		return
	}
	for _, claim := range read.named {
		row := r.rows[cmIndex(claim.key.comment)]
		if row.pending && !row.claim.Before(claim.claimedAt) && cmSlotOf(row.claim)-cmSlotOf(claim.claimedAt) <= cmLapseSlots {
			read.found = append(read.found, mentionClaim{key: claim.key, claimedAt: row.claim})
		}
	}
}

// runTry is a try's statements (sendCommentMention): a send that waited before reads the rule
// alone and, while the earlier event still owes s1 a send, writes nothing; otherwise the try
// claims the attempt as claimCommentDeliveryAttempt does - the row's claim free, or the send's own
// claim live with the row's at or past it, each at some timing within the slots - and then waits
// behind the earlier event or sends.
func (r *cmRun) runTry(try *cmTry) {
	try.ran = true
	item := try.item
	c := cmIndex(item.comment.ID)
	if item.heldFor != "" && !r.published {
		try.answer = cmAnswer{item: item, waits: true}
		r.log("%s's try reads the rule alone: the earlier event still owes s1, so it writes nothing", cmComments[c])
		return
	}
	row := &r.rows[c]
	if !row.pending {
		r.log("%s's try finds its attempt settled and sends nothing", cmComments[c])
		return
	}
	free := r.mayHaveLapsed(row.claim)
	mine := !row.claim.Before(item.claimed) && !r.lapsed(item.claimed)
	if !free && !mine {
		r.log("%s's try carries claim %s and finds the row's %s: the attempt is another sender's, and the send ends unsent",
			cmComments[c], r.name(item.claimed), r.name(row.claim))
		r.fail("dropped", "the sender's own send of %s is refused as another sender's: it carries claim %s, the row's %s is live",
			cmComments[c], r.name(item.claimed), r.name(row.claim))
		return
	}
	row.claim = r.now()
	try.took = true
	if !r.published {
		item.claimed = row.claim
		item.held(&mentionHeldForOrder{target: item.mention, claim: attemptClaim{attempt: 1, claimedAt: row.claim}, by: cmEarlier})
		try.answer = cmAnswer{item: item, waits: true}
		r.log("%s's try claims its attempt %s and waits behind the earlier event", cmComments[c], r.name(row.claim))
		return
	}
	r.deliver(c, fmt.Sprintf("%s's try claims its attempt %s and", cmComments[c], r.name(row.claim)))
}

// owed is whether the delivery sweep takes comment c's attempt: pending, committed a lease ago,
// and with no live claim on it (model.OwedMentionAttemptSQL), each at every timing within the
// slots.
func (r *cmRun) owed(c int) bool {
	row := r.rows[c]
	return row.committed && row.pending && (row.claim.IsZero() || r.lapsed(row.claim)) && r.lapsed(row.created)
}

// sweep is the delivery sweep taking comment c's owed attempt (resumeOwedDelivery): it claims it,
// and sends it, or hands it to the sender while the earlier event still owes s1 a send.
func (r *cmRun) sweep(c int) {
	row := &r.rows[c]
	row.claim = r.now()
	if r.published {
		r.deliver(c, fmt.Sprintf("the delivery sweep claims %s's owed attempt %s and", cmComments[c], r.name(row.claim)))
		return
	}
	item := r.item(c)
	item.claimed = row.claim
	item.held(&mentionHeldForOrder{target: item.mention, claim: attemptClaim{attempt: 1, claimedAt: row.claim}, by: cmEarlier})
	r.log("the delivery sweep claims %s's owed attempt %s, which waits behind the earlier event, and hands it to the sender",
		cmComments[c], r.name(row.claim))
	r.sender.enqueue(context.Background(), item)
	r.settle(nil)
}

func (r *cmRun) lanes() map[laneKey][]mentionKey {
	r.sender.mu.Lock()
	defer r.sender.mu.Unlock()
	lanes := map[laneKey][]mentionKey{}
	for lane, sends := range r.sender.lanes {
		for _, send := range sends {
			lanes[lane] = append(lanes[lane], send.key())
		}
	}
	return lanes
}

// settle waits for every goroutine of the bubble to stop, then takes in what the step started: a
// try or its re-read, the refresh's re-read or statement, and a park's timer. parked is the try
// whose goroutine the step let go on with a send that waits, whose park starts the timer.
func (r *cmRun) settle(parked *cmTry) {
	synctest.Wait()
	var arrived *cmArrival
collect:
	for {
		select {
		case a := <-r.arrivals:
			switch {
			case r.refreshing && a.refresh != nil:
				if r.refresh == nil {
					r.refresh = &cmRefresh{began: r.slot}
				}
				r.refresh.read, r.refresh.queued, r.refresh.reply = nil, a.queued, a.refresh
				r.log("the refresh's statement names %s", r.names(a.queued))
			case r.refreshing && a.reread != nil:
				r.refresh = &cmRefresh{began: r.slot, read: &cmRead{named: a.named, reply: a.reread}}
				r.log("the refresh re-reads the unknown claims %s", r.names(a.named))
			case a.refresh != nil:
				r.failModel("a refresh statement the model did not begin")
				return
			case arrived != nil:
				r.failModel("two of a lane's statements started in one step")
				return
			default:
				arrived = &a
			}
		default:
			break collect
		}
	}
	r.refreshing = false
	r.mu.Lock()
	started := r.started
	r.started = nil
	r.mu.Unlock()
	switch {
	case len(started) > 1:
		r.failModel("two park timers started in one step")
		return
	case len(started) == 1:
		if parked == nil {
			r.failModel("a park timer started by no park")
			return
		}
		lane := heldLane(parked.lane, parked.item.owner())
		if lane != cmLane(lane.held) {
			r.failModel("a park timer for lane %v, which is not one of the model's two", lane)
			return
		}
		if _, waits := r.timers[lane]; waits {
			r.failModel("a second timer for parked lane %v", lane)
			return
		}
		r.timers[lane] = cmTimer{run: started[0], began: r.slot}
	}
	if arrived == nil {
		return
	}
	item := arrived.item
	if arrived.reread != nil {
		item = mentionSend{comment: model.Comment{ID: arrived.named[0].key.comment}}
	}
	c := cmIndex(item.comment.ID)
	if arrived.reread != nil && parked != nil && cmIndex(parked.item.comment.ID) == c {
		r.tries[c] = &cmTry{lane: parked.lane, began: parked.began, atPark: true, item: parked.item,
			read: &cmRead{named: arrived.named, reply: arrived.reread}}
		r.log("the claim of %s turned unknown during its try: it re-reads it %s before it parks", item.comment.ID,
			r.names(arrived.named))
		return
	}
	if try := r.tries[c]; try != nil {
		// The goroutine whose re-read answered goes on to the try, on the lane it serves.
		if try.read != nil || try.trying || arrived.try == nil {
			r.failModel("a second try of %s in flight", item.comment.ID)
			return
		}
		try.trying, try.item, try.reply = true, arrived.item, arrived.try
		r.log("the sender starts %s's try, carrying claim %s", item.comment.ID, r.name(arrived.item.claimed))
		return
	}
	// A lane the sender holds is served by one goroutine, waits in ready, or is parked with its
	// timer pending, and every goroutine that serves one is inside a try or its re-read. So the
	// lane of the one that started is the one served lane no other try is on.
	r.sender.mu.Lock()
	served := map[laneKey]bool{}
	for lane := range r.sender.lanes {
		served[lane] = true
	}
	for _, lane := range r.sender.ready {
		delete(served, lane)
	}
	r.sender.mu.Unlock()
	for lane := range r.timers {
		delete(served, lane)
	}
	for _, try := range r.tries {
		delete(served, try.lane)
	}
	if len(served) != 1 {
		r.failModel("the send of %s started on none or several of the lanes %v (lanes %v)", item.comment.ID, served, r.lanes())
		return
	}
	try := &cmTry{began: r.slot}
	for lane := range served {
		try.lane = lane
	}
	r.tries[c] = try
	if arrived.reread != nil {
		try.read = &cmRead{named: arrived.named, reply: arrived.reread}
		r.log("the sender takes %s on its %s and re-reads its unknown claim %s first", item.comment.ID,
			cmLaneName(try.lane), r.names(arrived.named))
		return
	}
	try.trying, try.item, try.reply = true, arrived.item, arrived.try
	r.log("the sender starts %s's try on its %s, carrying claim %s", item.comment.ID, cmLaneName(try.lane),
		r.name(arrived.item.claimed))
}

func cmLaneName(lane laneKey) string {
	if lane.held {
		return "held lane"
	}
	return "own lane"
}

func (r *cmRun) apply(s cmStep) {
	if r.fault != "" {
		return
	}
	r.fresh = nil
	switch s.kind {
	case cmCommit:
		r.commit(s.comment)
	case cmRereadRuns:
		r.runRead(r.tries[s.comment].read)
		r.log("%s's re-read finds %s", cmComments[s.comment], r.names(r.tries[s.comment].read.found))
	case cmRereadAnswers, cmRereadFails:
		try := r.tries[s.comment]
		answer := cmReadAnswer{claims: try.read.found}
		switch {
		case s.kind == cmRereadFails:
			r.readFailures++
			answer = cmReadAnswer{err: errCmLost}
			r.log("%s's re-read fails: its claim is still unknown, and the send parks", cmComments[s.comment])
		default:
			r.log("%s's re-read answers", cmComments[s.comment])
		}
		reply := try.read.reply
		try.read = nil
		var parked *cmTry
		if try.atPark || s.kind == cmRereadFails {
			// The goroutine parks the send, with no try when the read before it failed, and moves on.
			delete(r.tries, s.comment)
			try.item = r.item(s.comment)
			parked = try
		}
		reply <- answer
		r.settle(parked)
	case cmTryRuns:
		r.runTry(r.tries[s.comment])
	case cmTryAnswers:
		try := r.tries[s.comment]
		delete(r.tries, s.comment)
		var parked *cmTry
		if try.answer.waits {
			r.log("%s's try answers that it waits, under claim %s", cmComments[s.comment], r.name(try.answer.item.claimed))
			try.item, parked = try.answer.item, try
		} else {
			r.log("%s's try answers that it is done", cmComments[s.comment])
		}
		try.reply <- try.answer
		r.settle(parked)
	case cmTimerRuns:
		lane := cmLane(s.held)
		timer := r.timers[lane]
		delete(r.timers, lane)
		r.log("the timer of the parked %s runs", cmLaneName(lane))
		timer.run()
		r.settle(nil)
	case cmTimePasses:
		r.slot++
		if r.slot%cmSlots != 0 {
			r.log("time passes to period %d, slot %d", r.slot/cmSlots, r.slot%cmSlots)
			break
		}
		r.refreshing = true
		r.log("period %d begins: the sender's loop starts a refresh", r.slot/cmSlots)
		go r.sender.refreshHeldClaims(context.Background())
		r.settle(nil)
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
		r.refreshing = true
		r.refresh.read.reply <- answer
		r.refresh.read = nil
		r.settle(nil)
		if r.refresh != nil && r.refresh.reply == nil {
			r.refresh = nil
		}
	case cmRefreshWrites:
		now := r.now()
		var written []mentionClaim
		var names []string
		for _, claim := range r.refresh.queued {
			row := &r.rows[cmIndex(claim.key.comment)]
			if row.pending && row.claim.Equal(claim.claimedAt) {
				row.claim = now
				written = append(written, mentionClaim{key: claim.key, claimedAt: now})
				names = append(names, claim.key.comment)
			}
		}
		slices.Sort(names)
		r.refresh.wrote, r.refresh.written = true, written
		r.log("the refresh's statement commits, writing %s on %v", r.name(now), names)
	case cmRefreshRecords, cmRefreshAnswerLost:
		answer := cmWritten{claims: r.refresh.written}
		if s.kind == cmRefreshAnswerLost {
			answer = cmWritten{err: errCmLost}
			r.log("the refresh's answer is lost after its statement committed")
		} else {
			r.log("the refresh records what its statement wrote")
		}
		began := r.refresh.began
		r.refresh.reply <- answer
		r.refresh = nil
		// A lost answer has the refresh re-read the claims it left unknown at once.
		r.refreshing = true
		r.settle(nil)
		if r.refresh != nil {
			r.refresh.began = began
		}
	case cmPublish:
		r.published = true
		r.log("the outbox sends the earlier event to s1")
	case cmSweep:
		r.sweep(s.comment)
	}
	if !slices.Contains(r.broke, "stale") {
		r.checkClaims()
	}
}

// checkClaims is the stale check: every claim the sender records and trusts is its row's, unless a
// refresh has written rows it has not recorded yet, or a try has claimed the row for itself.
func (r *cmRun) checkClaims() {
	if r.refresh != nil && r.refresh.wrote {
		return
	}
	r.sender.mu.Lock()
	claims := map[mentionKey]time.Time{}
	for key, state := range r.sender.held {
		if !state.unsure {
			claims[key] = state.claimed
		}
	}
	r.sender.mu.Unlock()
	for key, claimed := range claims {
		c := cmIndex(key.comment)
		row := r.rows[c]
		if !row.pending || row.claim.Equal(claimed) {
			continue
		}
		if try := r.tries[c]; try != nil && try.took {
			continue
		}
		r.fail("stale", "the sender trusts %s's claim as %s while its row carries %s: no refresh writes that row again",
			key.comment, r.name(claimed), r.name(row.claim))
		return
	}
}

func (r *cmRun) enabled() []cmStep {
	if r.ended() || r.fault != "" {
		return nil
	}
	var steps []cmStep
	if !r.rows[1].committed {
		steps = append(steps, cmStep{kind: cmCommit, comment: 1})
	}
	mayFail := r.readFailures < r.v.readFailures
	// Time moves on only while nothing that began a slot ago is still to end.
	waits := r.refresh != nil && r.refresh.began < r.slot
	for c := range 2 {
		try := r.tries[c]
		if try == nil {
			continue
		}
		waits = waits || try.began < r.slot
		switch {
		case try.read != nil && !try.read.ran:
			steps = append(steps, cmStep{kind: cmRereadRuns, comment: c})
		case try.read != nil:
			steps = append(steps, cmStep{kind: cmRereadAnswers, comment: c})
			if mayFail {
				steps = append(steps, cmStep{kind: cmRereadFails, comment: c})
			}
		case try.trying && !try.ran:
			steps = append(steps, cmStep{kind: cmTryRuns, comment: c})
		case try.trying:
			steps = append(steps, cmStep{kind: cmTryAnswers, comment: c})
		}
	}
	for _, timer := range r.timers {
		waits = waits || timer.began < r.slot
	}
	for _, held := range []bool{false, true} {
		if _, parked := r.timers[cmLane(held)]; parked {
			steps = append(steps, cmStep{kind: cmTimerRuns, held: held})
		}
	}
	if f := r.refresh; f != nil {
		switch {
		case f.read != nil && !f.read.ran:
			steps = append(steps, cmStep{kind: cmRefreshRereadRuns})
		case f.read != nil:
			steps = append(steps, cmStep{kind: cmRefreshRereadAnswers})
			if mayFail {
				steps = append(steps, cmStep{kind: cmRefreshRereadFails})
			}
		case !f.wrote:
			steps = append(steps, cmStep{kind: cmRefreshWrites})
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
	if !r.published {
		steps = append(steps, cmStep{kind: cmPublish})
	}
	for c := range 2 {
		if r.owed(c) {
			steps = append(steps, cmStep{kind: cmSweep, comment: c})
		}
	}
	return steps
}

// key is the run's state as the search keys on it: every claim in play named by its order among
// them and its age up to a lease and a slot, then the model's rows, tries, timers and refresh,
// and the sender's maps.
func (r *cmRun) key() string {
	r.sender.mu.Lock()
	defer r.sender.mu.Unlock()
	var claims []time.Time
	add := func(claim time.Time) {
		if !claim.IsZero() {
			claims = append(claims, claim)
		}
	}
	addAll := func(named []mentionClaim) {
		for _, claim := range named {
			add(claim.claimedAt)
		}
	}
	for _, row := range r.rows {
		add(row.created)
		add(row.claim)
	}
	for _, state := range r.sender.held {
		add(state.claimed)
	}
	for _, sends := range r.sender.lanes {
		for _, send := range sends {
			add(send.claimed)
		}
	}
	for _, try := range r.tries {
		add(try.item.claimed)
		add(try.answer.item.claimed)
		if try.read != nil {
			addAll(try.read.named)
			addAll(try.read.found)
		}
	}
	if r.refresh != nil {
		addAll(r.refresh.queued)
		addAll(r.refresh.written)
		if r.refresh.read != nil {
			addAll(r.refresh.read.named)
			addAll(r.refresh.read.found)
		}
	}
	slices.SortFunc(claims, time.Time.Compare)
	claims = slices.CompactFunc(claims, time.Time.Equal)
	canon := func(claim time.Time) string {
		if claim.IsZero() {
			return "-"
		}
		i, _ := slices.BinarySearchFunc(claims, claim, time.Time.Compare)
		return fmt.Sprintf("%d/%d", i, min(r.age(claim), cmLapseSlots+1))
	}
	canonAll := func(named []mentionClaim) []string {
		var out []string
		for _, claim := range named {
			out = append(out, claim.key.comment+":"+canon(claim.claimedAt))
		}
		slices.Sort(out)
		return out
	}
	read := func(read *cmRead) string {
		if read == nil {
			return "-"
		}
		return fmt.Sprintf("%v %t %v", canonAll(read.named), read.ran, canonAll(read.found))
	}
	send := func(s mentionSend) string {
		return fmt.Sprintf("%s:%s:%s", s.comment.ID, canon(s.claimed), s.heldFor)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "s%d pub%t took%v broke%v failed%d|", r.slot, r.published, r.took, r.broke, r.readFailures)
	for _, row := range r.rows {
		fmt.Fprintf(&b, "row %t %t %s %s|", row.committed, row.pending, canon(row.created), canon(row.claim))
	}
	if f := r.refresh; f != nil {
		fmt.Fprintf(&b, "refresh %t %s %v %t %v|", f.began < r.slot, read(f.read), canonAll(f.queued), f.wrote,
			canonAll(f.written))
	}
	for c := range 2 {
		if try := r.tries[c]; try != nil {
			fmt.Fprintf(&b, "try %d %v %t %s %t %s %t %t %s %t|", c, try.lane, try.began < r.slot, read(try.read),
				try.trying, send(try.item), try.ran, try.took, send(try.answer.item), try.answer.waits)
		}
	}
	var timers []string
	for lane, timer := range r.timers {
		timers = append(timers, fmt.Sprintf("%v:%t", lane, timer.began < r.slot))
	}
	slices.Sort(timers)
	fmt.Fprintf(&b, "timers %v|", timers)
	// Each attempt's trying is left out: the sender sets it exactly while the model's try of that
	// comment is trying, which the key names.
	var held []string
	for key, state := range r.sender.held {
		entry := key.comment + ":" + canon(state.claimed)
		if state.unsure {
			entry += ":unsure"
		}
		held = append(held, entry)
	}
	slices.Sort(held)
	fmt.Fprintf(&b, "held %v|", held)
	var lanes []string
	for lane, sends := range r.sender.lanes {
		var names []string
		for _, s := range sends {
			names = append(names, send(s))
		}
		lanes = append(lanes, fmt.Sprintf("%v=%v", lane, names))
	}
	slices.Sort(lanes)
	var waits []string
	for lane := range r.sender.waits {
		waits = append(waits, fmt.Sprint(lane))
	}
	slices.Sort(waits)
	fmt.Fprintf(&b, "lanes %v waits %v ready %v serving %d", lanes, waits, r.sender.ready, r.sender.serving)
	return b.String()
}

// cmResult is what one search found: the states it reached, the longest run, and, by check, how
// many states broke it first and the trace of the first of them, a shortest one.
type cmResult struct {
	variant  cmVariant
	periods  int
	states   int
	depth    int
	found    map[string]int
	first    map[string][]string
	duration time.Duration
}

func (r cmResult) String() string {
	return fmt.Sprintf("| %s | %d | %d | %d | %d | %d | %d | %d | %s |", r.variant.name, r.periods, r.states, r.depth,
		r.found["stale"], r.found["dropped"], r.found["order"], r.found["twice"], r.duration.Round(time.Millisecond))
}

// cmVisit is what replaying one run's steps came to: the SHA-256 of its state's key, the checks
// its last step broke, the steps it may take next, the model's own fault, if any, and, from a
// replay with its trace on, the run's trace.
type cmVisit struct {
	key          [sha256.Size]byte
	fault        string
	fresh, trace []string
	enabled      []cmStep
}

// cmVisitOf replays steps in a bubble of its own, writing the run's trace when tracing.
func cmVisitOf(t *testing.T, v cmVariant, periods int, steps []cmStep, tracing bool) (visit cmVisit) {
	synctest.Test(t, func(*testing.T) {
		r := newCmRun(v, periods, tracing)
		defer r.shutdown()
		for _, s := range steps {
			r.apply(s)
		}
		visit = cmVisit{key: sha256.Sum256([]byte(r.key())), fault: r.fault, fresh: r.fresh, enabled: r.enabled(),
			trace: r.trace}
	})
	return visit
}

// cmExplore searches every state of v within periods breadth first, each run replayed from the
// start in a bubble of its own and the runs of one depth spread over at most 16 workers, then
// taken in order, so the first trace of each check is a shortest one and the same on every run.
// The search keeps a state's SHA-256 rather than its key, and replays with its trace on only the
// run that first breaks a check, or that fails the model; the replay must reach the same state.
func cmExplore(t *testing.T, v cmVariant, periods int) cmResult {
	began := time.Now()
	result := cmResult{variant: v, periods: periods, found: map[string]int{}, first: map[string][]string{}}
	seen := map[[sha256.Size]byte]struct{}{}
	traced := func(steps []cmStep, key [sha256.Size]byte) cmVisit {
		visit := cmVisitOf(t, v, periods, steps, true)
		if visit.key != key {
			t.Fatalf("the model is not deterministic: a replay with its trace on reached another state, after:\n\t%s",
				strings.Join(visit.trace, "\n\t"))
		}
		return visit
	}
	frontier := [][]cmStep{nil}
	for depth := 0; len(frontier) > 0; depth++ {
		visits := make([]cmVisit, len(frontier))
		work := make(chan int)
		var workers sync.WaitGroup
		for range min(runtime.GOMAXPROCS(0), 16) {
			workers.Go(func() {
				for i := range work {
					visits[i] = cmVisitOf(t, v, periods, frontier[i], false)
				}
			})
		}
		for i := range frontier {
			work <- i
		}
		close(work)
		workers.Wait()
		var next [][]cmStep
		for i, visit := range visits {
			if visit.fault != "" {
				visit = traced(frontier[i], visit.key)
				t.Fatalf("%s, after:\n\t%s", visit.fault, strings.Join(visit.trace, "\n\t"))
			}
			if _, ok := seen[visit.key]; ok {
				continue
			}
			seen[visit.key] = struct{}{}
			result.states++
			result.depth = depth
			for _, kind := range visit.fresh {
				result.found[kind]++
				if result.first[kind] == nil {
					result.first[kind] = traced(frontier[i], visit.key).trace
				}
			}
			for _, s := range visit.enabled {
				next = append(next, append(slices.Clip(frontier[i]), s))
			}
		}
		frontier = next
	}
	result.duration = time.Since(began)
	return result
}

// The sender's claim model: the sender as built must keep every claim it trusts in step with its
// row, never have its own send refused and never let s1 take A-2 first or a comment twice, with
// any refresh's answer lost, every one of them among the runs; and the control, a sender whose
// re-reads learn nothing, must show the model sees a stale claim, a dropped send and A-2 before
// A-1.
func TestTheMentionSenderClaimModel(t *testing.T) {
	if model.DeliveryClaimLapse != cmLapsePeriods*mentionClaimRefresh {
		t.Fatalf("the model counts a claim's lease as %d refresh periods; model.DeliveryClaimLapse is %v and mentionClaimRefresh %v",
			cmLapsePeriods, model.DeliveryClaimLapse, mentionClaimRefresh)
	}
	// The sender logs every lost answer and failed read at ERROR, which the model makes by the
	// thousand.
	logs := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(logs) })
	periods := 4
	if os.Getenv("ENVOY_MENTION_ORDER_MODEL") == "full" {
		periods = 7
	}
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	t.Logf("| sender | periods | states | longest run | stale claims | dropped sends | A-2 before A-1 | sent twice | time |")
	t.Logf("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	built := cmExplore(t, cmAsBuilt, periods)
	t.Logf("%s", built)
	for _, kind := range cmChecks {
		if trace := built.first[kind]; trace != nil {
			t.Errorf("%s: the %s check broke in %d states; first:\n\t%s", cmAsBuilt.name, kind, built.found[kind],
				strings.Join(trace, "\n\t"))
		}
	}
	control := cmExplore(t, cmBlindRepair, periods)
	t.Logf("%s", control)
	for _, kind := range []string{"stale", "dropped", "order"} {
		if control.found[kind] == 0 {
			t.Errorf("the model is blind: the control broke no %s check", kind)
		} else {
			t.Logf("control, first %s:\n\t%s", kind, strings.Join(control.first[kind], "\n\t"))
		}
	}
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}

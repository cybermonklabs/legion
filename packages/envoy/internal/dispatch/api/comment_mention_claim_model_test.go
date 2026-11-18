package api

import (
	"context"
	"fmt"
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
// mention sender (newMentionSender) - its lanes, its parks and their timers, and its claim refresh
// (refreshHeldClaims) - with the statements its sends and its refresh make on two comments'
// attempt rows, the delivery sweep, and the outbox sending the earlier event both comments wait
// behind. It is the sender's part of the mention-copy order model; the outbox's part is
// TestTheMentionCopyOrderModel in internal/dispatch/outbox.
//
// The sender runs inside a testing/synctest bubble, so the model knows when every goroutine it
// started has stopped, and each of them stops only where the model holds it: a send inside the
// sender's send function, the refresh inside its refresh function. Each step of the model is one
// thing that may happen next, and the search tries every one from every state it reaches, the
// sender's own code running between them. The search keys on the sender's maps and the model's
// own state, every claim in it named by its order among the claims in play and its age, which is
// all the code compares (model.DeliveryClaimMineSQL, model.DeliveryClaimFreeSQL).
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
//     names; and the delivery sweep (resumeOwedDelivery), which takes an attempt committed a lease
//     ago whose claim lapsed, and sends it or hands it back to the sender;
//   - time in slots of half a claim-refresh period (mentionClaimRefresh), which a step of its own
//     ends: the sender's loop (keepClaims) starts a refresh as every period begins, a claim lapses
//     one lease (model.DeliveryClaimLapse, cmLapsePeriods periods) after it was written, and every
//     thing that takes milliseconds or seconds - a try, the refresh's statement and its record, a
//     park's timer, at most two seconds (mentionOrderRetryMax) - ends no later than the slot after
//     the one it began in, so time moves on no further while one is still to end;
//   - a claim and the moment it is read are each known to a slot, so a claim read one lease and
//     part of a slot after it was written may be live or lapsed: the model reads it as the sender's
//     own try would have it - live, or free when another holds it - and as the sweep would not, so
//     every send it finds refused, and every attempt the sweep takes, is so at any timing.
//
// What it checks, in every state, carrying each run on past a stale claim or a dropped send to
// what follows from it:
//   - stale: every claim the sender records for an attempt is the claim the attempt's row carries,
//     whenever no refresh has written rows it has not recorded yet and no try has claimed that
//     row for itself. A record behind its row is never written again, since the refresh writes
//     only a row that carries the claim the sender names, so the row's claim lapses with its send
//     still held;
//   - dropped: no try of a send the sender holds finds its attempt held by another sender;
//   - order and twice: s1 takes A-1 before A-2, and each of them once. Either ends the run.
//
// What it leaves out: the drain (wait), a keyed repeat (replayComment), the queue's bound, more
// than one recipient or owner, and a statement that fails.
//
// The control is a refresh whose statement commits and whose answer the sender never reads, as
// a connection lost after the commit leaves it: the model must find a record left behind its row,
// a dropped send and A-2 before A-1.

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

type cmStepKind uint8

const (
	cmCommit cmStepKind = iota
	cmTryRuns
	cmTryAnswers
	cmTimerRuns
	cmTimePasses
	cmRefreshWrites
	cmRefreshRecords
	cmPublish
	cmSweep
)

// cmStep is one step of a run: its kind, the comment a commit, a try or a sweep is of, and the
// lane whose park a timer ends.
type cmStep struct {
	kind    cmStepKind
	comment int
	lane    laneKey
}

type cmVariant struct {
	name string
	// losesAnswer is the control's refresh: its statement commits, and the sender reads no answer.
	losesAnswer bool
}

var (
	cmAsBuilt    = cmVariant{name: "the sender as built"}
	cmLostAnswer = cmVariant{name: "control: a refresh's answer lost after its statement commits", losesAnswer: true}
)

// cmRow is a comment's attempt row: whether it committed and is still pending, when it committed,
// and the claim it carries (zero for none).
type cmRow struct {
	committed, pending bool
	created, claim     time.Time
}

// cmTry is a send the sender has started: the item it was handed, the lane the goroutine serves,
// the slot it started in, and, once its statements ran, its answer and whether they claimed the
// row for itself.
type cmTry struct {
	item   mentionSend
	lane   laneKey
	reply  chan cmAnswer
	began  int
	ran    bool
	answer cmAnswer
	took   bool
}

type cmAnswer struct {
	item  mentionSend
	waits bool
}

// cmRefresh is the refresh in flight: the slot it began in, the claims its snapshot holds, and,
// once its statement ran, what the statement wrote.
type cmRefresh struct {
	began   int
	queued  []mentionClaim
	reply   chan []mentionClaim
	wrote   bool
	written []mentionClaim
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
	refresh chan []mentionClaim
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
	// beginning is that the step being taken starts a refresh.
	beginning bool
	slot      int
	seq       time.Duration
	published bool
	rows      [2]cmRow
	took      []int
	tries     map[int]*cmTry
	refresh   *cmRefresh
	// broke is every check the run has broken, in order, and fresh those its last step broke.
	broke, fresh []string
	trace        []string
}

func newCmRun(v cmVariant, periods int) *cmRun {
	lifetime, stop := context.WithCancel(context.Background())
	r := &cmRun{
		v: v, slots: periods * cmSlots, stop: stop, done: make(chan struct{}),
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
			reply := make(chan []mentionClaim, 1)
			r.arrivals <- cmArrival{queued: slices.Clone(queued), refresh: reply}
			select {
			case written := <-reply:
				return written, nil
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
	r.trace = append(r.trace, fmt.Sprintf(format, args...))
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

// age is how many slots ago claim was written.
func (r *cmRun) age(claim time.Time) int {
	return r.slot - int(claim.Sub(cmBase)/cmSlot)
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

func (r *cmRun) name(claim time.Time) string {
	if claim.IsZero() {
		return "none"
	}
	slot := int(claim.Sub(cmBase) / cmSlot)
	return fmt.Sprintf("p%d.%d#%d", slot/cmSlots, slot%cmSlots, claim.Sub(cmBase)%cmSlot)
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
	pre := r.lanes()
	now := r.now()
	r.rows[c] = cmRow{committed: true, pending: true, created: now, claim: now}
	item := r.item(c)
	item.claimed = now
	r.log("%s commits its attempt claimed %s and queues its send", cmComments[c], r.name(now))
	r.sender.enqueue(context.Background(), item)
	r.settle(pre, nil)
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
	pre := r.lanes()
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
	r.settle(pre, nil)
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
// try, the refresh, and a park's timer.
func (r *cmRun) settle(pre map[laneKey][]mentionKey, answered *cmTry) {
	synctest.Wait()
	var arrived *cmTry
collect:
	for {
		select {
		case a := <-r.arrivals:
			if a.refresh != nil {
				if !r.beginning || r.refresh != nil {
					r.failModel("a refresh the model did not begin")
					return
				}
				r.refresh = &cmRefresh{began: r.slot, queued: a.queued, reply: a.refresh}
				continue
			}
			if arrived != nil {
				r.failModel("two tries started in one step")
				return
			}
			arrived = &cmTry{item: a.item, reply: a.try, began: r.slot}
		default:
			break collect
		}
	}
	r.beginning = false
	r.mu.Lock()
	started := r.started
	r.started = nil
	r.mu.Unlock()
	switch {
	case len(started) > 1:
		r.failModel("two park timers started in one step")
		return
	case len(started) == 1:
		if answered == nil || !answered.answer.waits {
			r.failModel("a park timer started by no park")
			return
		}
		lane := heldLane(answered.lane, answered.answer.item.owner())
		if _, parked := r.timers[lane]; parked {
			r.failModel("a second timer for parked lane %v", lane)
			return
		}
		r.timers[lane] = cmTimer{run: started[0], began: r.slot}
	}
	if arrived == nil {
		return
	}
	// A lane the sender holds is served by one goroutine, waits in ready, or is parked with its
	// timer pending, and every goroutine that serves one is inside a try. So the lane of the try
	// that started is the one served lane no other try is on.
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
		r.failModel("the try of %s started on none or several of the lanes %v (lanes before %v)",
			arrived.item.comment.ID, served, pre)
		return
	}
	for lane := range served {
		arrived.lane = lane
	}
	c := cmIndex(arrived.item.comment.ID)
	if r.tries[c] != nil {
		r.failModel("a second try of %s in flight", arrived.item.comment.ID)
		return
	}
	r.tries[c] = arrived
	r.log("the sender starts %s's try on its %s, carrying claim %s", arrived.item.comment.ID, cmLaneName(arrived.lane),
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
	pre := r.lanes()
	switch s.kind {
	case cmCommit:
		r.commit(s.comment)
	case cmTryRuns:
		r.runTry(r.tries[s.comment])
	case cmTryAnswers:
		try := r.tries[s.comment]
		delete(r.tries, s.comment)
		if try.answer.waits {
			r.log("%s's try answers that it waits, under claim %s", cmComments[s.comment], r.name(try.answer.item.claimed))
		} else {
			r.log("%s's try answers that it is done", cmComments[s.comment])
		}
		try.reply <- try.answer
		r.settle(pre, try)
	case cmTimerRuns:
		timer := r.timers[s.lane]
		delete(r.timers, s.lane)
		r.log("the timer of the parked %s runs", cmLaneName(s.lane))
		timer.run()
		r.settle(pre, nil)
	case cmTimePasses:
		r.slot++
		if r.slot%cmSlots != 0 {
			r.log("time passes to period %d, slot %d", r.slot/cmSlots, r.slot%cmSlots)
			break
		}
		r.beginning = true
		r.log("period %d begins: the sender's loop starts a refresh", r.slot/cmSlots)
		go r.sender.refreshHeldClaims(context.Background())
		r.settle(pre, nil)
		if r.refresh != nil {
			var names []string
			for _, claim := range r.refresh.queued {
				names = append(names, fmt.Sprintf("%s %s", claim.key.comment, r.name(claim.claimedAt)))
			}
			slices.Sort(names)
			r.log("the refresh's snapshot: %s", strings.Join(names, ", "))
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
		r.log("the refresh's statement writes %s on %v", r.name(now), names)
	case cmRefreshRecords:
		written := r.refresh.written
		if r.v.losesAnswer {
			written = nil
			r.log("the refresh's answer is lost: the sender records nothing")
		} else {
			r.log("the refresh records what its statement wrote")
		}
		r.refresh.reply <- written
		r.refresh = nil
		r.settle(pre, nil)
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

// checkClaims is the stale check: every claim the sender records is its row's, unless a refresh
// has written rows it has not recorded yet, or a try has claimed the row for itself.
func (r *cmRun) checkClaims() {
	if r.refresh != nil && r.refresh.wrote {
		return
	}
	r.sender.mu.Lock()
	claims := map[mentionKey]time.Time{}
	for key, claimed := range r.sender.claims {
		claims[key] = claimed
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
		r.fail("stale", "the sender records %s's claim as %s while its row carries %s: no refresh writes that row again",
			key.comment, r.name(claimed), r.name(row.claim))
		return
	}
}

func (r *cmRun) enabled() []cmStep {
	if r.ended() {
		return nil
	}
	var steps []cmStep
	if !r.rows[1].committed {
		steps = append(steps, cmStep{kind: cmCommit, comment: 1})
	}
	// Time moves on only while nothing that began a slot ago is still to end.
	waits := r.refresh != nil && r.refresh.began < r.slot
	for c := range 2 {
		if try := r.tries[c]; try != nil {
			if try.ran {
				steps = append(steps, cmStep{kind: cmTryAnswers, comment: c})
			} else {
				steps = append(steps, cmStep{kind: cmTryRuns, comment: c})
			}
			waits = waits || try.began < r.slot
		}
	}
	var parked []laneKey
	for lane, timer := range r.timers {
		parked = append(parked, lane)
		waits = waits || timer.began < r.slot
	}
	slices.SortFunc(parked, func(a, b laneKey) int { return strings.Compare(fmt.Sprint(a), fmt.Sprint(b)) })
	for _, lane := range parked {
		steps = append(steps, cmStep{kind: cmTimerRuns, lane: lane})
	}
	if r.refresh != nil {
		if r.refresh.wrote {
			steps = append(steps, cmStep{kind: cmRefreshRecords})
		} else {
			steps = append(steps, cmStep{kind: cmRefreshWrites})
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
	for _, row := range r.rows {
		add(row.created)
		add(row.claim)
	}
	for _, claimed := range r.sender.claims {
		add(claimed)
	}
	for _, sends := range r.sender.lanes {
		for _, send := range sends {
			add(send.claimed)
		}
	}
	for _, try := range r.tries {
		add(try.item.claimed)
		add(try.answer.item.claimed)
	}
	if r.refresh != nil {
		for _, claim := range r.refresh.queued {
			add(claim.claimedAt)
		}
		for _, claim := range r.refresh.written {
			add(claim.claimedAt)
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
	send := func(s mentionSend) string {
		return fmt.Sprintf("%s:%s:%s", s.comment.ID, canon(s.claimed), s.heldFor)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "s%d pub%t took%v broke%v|", r.slot, r.published, r.took, r.broke)
	for _, row := range r.rows {
		fmt.Fprintf(&b, "row %t %t %s %s|", row.committed, row.pending, canon(row.created), canon(row.claim))
	}
	if r.refresh != nil {
		var queued, written []string
		for _, claim := range r.refresh.queued {
			queued = append(queued, claim.key.comment+":"+canon(claim.claimedAt))
		}
		for _, claim := range r.refresh.written {
			written = append(written, claim.key.comment+":"+canon(claim.claimedAt))
		}
		slices.Sort(queued)
		slices.Sort(written)
		fmt.Fprintf(&b, "refresh %t %v %t %v|", r.refresh.began < r.slot, queued, r.refresh.wrote, written)
	}
	for c := range 2 {
		if try := r.tries[c]; try != nil {
			fmt.Fprintf(&b, "try %v %s %t %t %t %s %t|", try.lane, send(try.item), try.began < r.slot, try.ran,
				try.took, send(try.answer.item), try.answer.waits)
		}
	}
	var timers []string
	for lane, timer := range r.timers {
		timers = append(timers, fmt.Sprintf("%v:%t", lane, timer.began < r.slot))
	}
	slices.Sort(timers)
	fmt.Fprintf(&b, "timers %v|", timers)
	var recorded, held []string
	for key, claimed := range r.sender.claims {
		recorded = append(recorded, key.comment+":"+canon(claimed))
	}
	for key := range r.sender.held {
		held = append(held, key.comment)
	}
	slices.Sort(recorded)
	slices.Sort(held)
	fmt.Fprintf(&b, "claims %v held %v|", recorded, held)
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

// cmVisit is what replaying one run's steps came to: its state's key, the checks its last step
// broke, the steps it may take next, the model's own fault, if any, and, with a check broken or a
// fault, the run's trace.
type cmVisit struct {
	key, fault   string
	fresh, trace []string
	enabled      []cmStep
}

// cmVisitOf replays steps in a bubble of its own.
func cmVisitOf(t *testing.T, v cmVariant, periods int, steps []cmStep) (visit cmVisit) {
	synctest.Test(t, func(*testing.T) {
		r := newCmRun(v, periods)
		defer r.shutdown()
		for _, s := range steps {
			r.apply(s)
		}
		visit = cmVisit{key: r.key(), fault: r.fault, fresh: r.fresh, enabled: r.enabled()}
		if r.fault != "" || len(r.fresh) > 0 {
			visit.trace = r.trace
		}
	})
	return visit
}

// cmExplore searches every state of v within periods breadth first, each run replayed from the
// start in a bubble of its own and the runs of one depth spread over at most 16 workers, then
// taken in order, so the first trace of each check is a shortest one and the same on every run.
func cmExplore(t *testing.T, v cmVariant, periods int) cmResult {
	began := time.Now()
	result := cmResult{variant: v, periods: periods, found: map[string]int{}, first: map[string][]string{}}
	seen := map[string]bool{}
	frontier := [][]cmStep{nil}
	for depth := 0; len(frontier) > 0; depth++ {
		visits := make([]cmVisit, len(frontier))
		work := make(chan int)
		var workers sync.WaitGroup
		for range min(runtime.GOMAXPROCS(0), 16) {
			workers.Go(func() {
				for i := range work {
					visits[i] = cmVisitOf(t, v, periods, frontier[i])
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
				t.Fatalf("%s, after:\n\t%s", visit.fault, strings.Join(visit.trace, "\n\t"))
			}
			if seen[visit.key] {
				continue
			}
			seen[visit.key] = true
			result.states++
			result.depth = depth
			for _, kind := range visit.fresh {
				result.found[kind]++
				if result.first[kind] == nil {
					result.first[kind] = visit.trace
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

// The sender's claim model: the sender as built must keep every claim it records in step with its
// row, never have its own send refused and never let s1 take A-2 first or a comment twice; and the
// control, a refresh whose answer is lost, must show the model sees a stale claim, a dropped send
// and A-2 before A-1.
func TestTheMentionSenderClaimModel(t *testing.T) {
	if model.DeliveryClaimLapse != cmLapsePeriods*mentionClaimRefresh {
		t.Fatalf("the model counts a claim's lease as %d refresh periods; model.DeliveryClaimLapse is %v and mentionClaimRefresh %v",
			cmLapsePeriods, model.DeliveryClaimLapse, mentionClaimRefresh)
	}
	periods := 5
	if os.Getenv("ENVOY_MENTION_ORDER_MODEL") == "full" {
		periods = 7
	}
	t.Logf("start %s", time.Now().UTC().Format(time.RFC3339Nano))
	t.Logf("| sender | periods | states | longest run | stale claims | dropped sends | A-2 before A-1 | sent twice | time |")
	t.Logf("| --- | --- | --- | --- | --- | --- | --- | --- | --- |")
	built := cmExplore(t, cmAsBuilt, periods)
	t.Logf("%s", built)
	control := cmExplore(t, cmLostAnswer, periods)
	t.Logf("%s", control)
	for _, kind := range cmChecks {
		if trace := built.first[kind]; trace != nil {
			t.Errorf("the sender as built breaks the %s check in %d states; first:\n\t%s", kind, built.found[kind],
				strings.Join(trace, "\n\t"))
		}
	}
	for _, kind := range []string{"stale", "dropped", "order"} {
		if control.found[kind] == 0 {
			t.Errorf("the model is blind: the control's lost refresh answer broke no %s check", kind)
		} else {
			t.Logf("control, first %s:\n\t%s", kind, strings.Join(control.first[kind], "\n\t"))
		}
	}
	t.Logf("end %s", time.Now().UTC().Format(time.RFC3339Nano))
}

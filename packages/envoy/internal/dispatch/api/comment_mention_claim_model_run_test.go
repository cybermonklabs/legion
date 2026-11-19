//go:build !race

package api

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing/synctest"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// The sender claim model's runner (comment_mention_claim_model_test.go holds the model's state
// and what it checks): a run's bubble and the sender in it, the model's clock and the claim
// rules it restates from their SQL, the statements of the sender's sends, re-reads and the
// delivery sweep, and the start and settling of the sender's goroutines.

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
// sweep takes only an attempt whose claim has lapsed - its sender stopped, or wrote no claim for
// a lease - and sends the attempts it finds owed side by side (resumeOwedDeliveries), so it keeps
// no order between two comments: an A-2 it sends first, and an A-2 the lane sends after the sweep
// took A-1, are "lapsed", which the spec puts outside this change and the model counts apart.
// Every other A-2 that s1 takes first is "order".
func (r *cmRun) deliver(c int, road string, bySweep bool) {
	r.log("%s sends %s to s1", road, cmComments[c])
	switch {
	case slices.Contains(r.took, c):
		r.fail("twice", "s1 takes %s a second time", cmComments[c])
	case c == 1 && !slices.Contains(r.took, 0) && bySweep:
		r.fail("lapsed", "the delivery sweep sends A-2, whose claim lapsed, before A-1")
	case c == 1 && !slices.Contains(r.took, 0) && r.sweptFirst:
		r.fail("lapsed", "s1 takes A-2 after the delivery sweep took A-1, whose claim lapsed, and before A-1")
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
	if c == 0 {
		r.sweptFirst = true
	}
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
			g.last, g.done, g.carried, g.tried = c, false, a.item, false
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

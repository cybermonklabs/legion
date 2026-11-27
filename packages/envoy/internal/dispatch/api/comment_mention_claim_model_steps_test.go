//go:build !race

package api

import (
	"context"
	"slices"
	"strings"
)

// The sender claim model's steps (comment_mention_claim_model_test.go holds the model's state
// and what it checks): what each step does to the run (apply), and which steps a run may take
// next (enabled).

// cmLaneHead is one lane and the send at its head, as a lock step reads them out.
type cmLaneHead struct {
	lane laneKey
	send mentionSend
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
		if strings.HasPrefix(g.site, "next:") {
			g.tried = false
		}
		parking := strings.HasPrefix(g.site, "park:")
		if parking {
			// The park records the claim the try took (park).
			g.ended = nil
		}
		if g.refresh && r.refresh != nil && r.refresh.stmt != nil && r.refresh.stmt.answered {
			// The refresh records what its statement answered under this lock.
			r.refresh.stmt = nil
		}
		// next hands the goroutine the head of one lane, which is then the lane it serves. The
		// heads go into a buffer this run reuses, filled only where next reads them: a map was
		// made on every lock step, and a send is a kilobyte to copy into one.
		r.heads = r.heads[:0]
		if strings.HasPrefix(g.site, "next:") {
			r.lock.Mutex.Lock()
			for lane, sends := range r.sender.lanes {
				if len(sends) > 0 {
					r.heads = append(r.heads, cmLaneHead{lane: lane, send: sends[0]})
				}
			}
			r.lock.Mutex.Unlock()
		}
		if r.tracing {
			r.log("%s takes the lock at %s", r.gName(g), g.site)
		}
		r.release(g)
		hadTimer := r.timer != nil
		close(grant)
		r.settle()
		if g.running {
			break
		}
		if len(r.heads) > 0 {
			r.lock.Mutex.Lock()
			for _, head := range r.heads {
				if sends := r.sender.lanes[head.lane]; len(sends) == 0 || sends[0].key() != head.send.key() {
					head.send.claimed = r.sender.held[head.send.key()].claimed
					g.lane, g.carried, g.givenUp = head.lane, head.send, false
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
		try.g.done, try.g.tried = !try.answer.waits, true
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

// enabled is every step the run may take next. A statement - a try, a re-read's reading of the
// rows, a refresh's write, an outbox send - may run while a goroutine waits for the sender's lock:
// a goroutine that could take the lock may still wait milliseconds for a core, as long as a
// statement takes, so a refresh may re-read a row and write it again between a try's answer and
// the park that records the try's claim
// (TestAParkedSendKeepsTheRefreshsClaimWhenARefreshLandsBetweenItsTryAndItsPark). Time moves on a
// slot only while no goroutine waits for the lock: nothing waits that long.
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
	// goroutine waits for the lock and no hand-off waits, which take less than a slot.
	waits := len(r.sweeps) > 0
	for i, g := range r.gs {
		if g.lock != nil {
			steps = append(steps, cmStep{kind: cmLockRuns, g: i})
			waits = true
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
			steps = append(steps, cmStep{kind: cmRereadRuns, comment: c})
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
			steps = append(steps, cmStep{kind: cmRefreshRereadRuns})
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
	if !r.published {
		steps = append(steps, cmStep{kind: cmPublish})
	}
	if !r.publishedBetween {
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

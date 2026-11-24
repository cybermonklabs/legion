//go:build !race

package api

import (
	"slices"
	"time"
)

// The sender claim model's checks (comment_mention_claim_model_test.go says what each one
// holds): unnamed (checkHeldReport), stale (checkClaims) and dropped (checkDropped).

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

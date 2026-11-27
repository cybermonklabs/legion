// Package signals holds R11's signals as the daemon's code defines them: each signal's value at a
// moment, and the next moment after it at which the signal would cross a limit if no new event
// came. It imports the standard library alone, so the replay of a recorded window and the daemon's
// own admission pass read one definition, and nothing in the daemon imports the replay.
//
// A caller supplies what it knows as plain values: the moments of its holders, kills and waits,
// the provider's minutes, and where its record holds each signal (Coverage). The replay reads
// them from a window's files and from the models fitted on the window; the daemon reads them
// from its events. Coverage edges, minutes of the future the record already holds, and the decoding
// of files and events stay with the caller.
//
// Every crossing is computed by the comparison a reading makes (Value.Binds): a moment is a
// crossing where the signal binds at it by that comparison and did not bind before, or the other
// way round. A signal that is not recorded at a moment, and a signal with no limit, bind nothing.
package signals

import (
	"cmp"
	"slices"
	"sort"
	"time"
)

// The signals of R11 by name. The provider's signal is its headroom alone.
const (
	Idle      = "idle"
	Queue     = "queue"
	Provider  = "provider"
	Kills     = "kills"
	Approvals = "approvals"
)

// order is every signal a limit can name, in R11's order.
var order = []string{Idle, Queue, Provider, Kills, Approvals}

// Names is every signal a limit can name, in R11's order: the idle share, the merge queue's wait,
// the provider's headroom, the memory guard's kills and the approvals.
func Names() []string { return slices.Clone(order) }

// DefaultIdleAfter is d, how long a slot holder has been idle before the idle signal counts it: an
// hour, as R11 gives it. The comparison also reads d at 10 minutes and at 4 hours.
const DefaultIdleAfter = time.Hour

// The spans a signal counts its events over: the merge queue's exits over the 6 hours before a
// moment, and the memory guard's kills over the hour before it.
const (
	QueueExitSpan = 6 * time.Hour
	KillSpan      = time.Hour
)

// Value is one signal at a moment: its value, lower being better for every signal, and whether the
// caller's record holds the signal there. A signal with no record is unrecorded, never zero, and
// binds no rule.
type Value struct {
	Recorded bool
	Value    float64
}

// Binds says whether the signal holds admission against limit: it is recorded and at or above the
// limit, since a rule admits while each signal is under its limit. It is the one comparison of a
// limit with a signal, the one a reading and every crossing make.
func (v Value) Binds(limit float64) bool { return v.Recorded && v.Value >= limit }

// Coverage is what a caller knows of where its record holds a signal: Held answers whether it does
// at a moment, nil where it does at every moment (the daemon's own), and Edges is each moment after
// the one asked about at which Held changes its answer, so that the end of a record is a crossing
// of a signal that binds there. The zero value is a record with no gap and no edge.
type Coverage struct {
	Held  func(time.Time) bool
	Edges []time.Time
}

// held says whether the record holds the signal at at.
func (c Coverage) held(at time.Time) bool { return c.Held == nil || c.Held(at) }

// FirstChange is the first of moments after at at which binds answers otherwise than at at, and
// whether one does. It sorts moments in place.
func FirstChange(at time.Time, moments []time.Time, binds func(time.Time) bool) (time.Time, bool) {
	slices.SortFunc(moments, time.Time.Compare)
	now := binds(at)
	for _, moment := range slices.CompactFunc(moments, time.Time.Equal) {
		if moment.After(at) && binds(moment) != now {
			return moment, true
		}
	}
	return time.Time{}, false
}

// upTo is how many of the moments ts, in order, are at or before at.
func upTo(ts []time.Time, at time.Time) int {
	return sort.Search(len(ts), func(i int) bool { return ts[i].After(at) })
}

// earlier is the earlier of a and b.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// WaitsOnDefault says whether an ask waits on the project's default assignee, which is the
// approvals signal's count of one ask: it waits on its asked_of person, else its issue's assignee,
// else the project's default assignee, and counts where that person is the default assignee. An
// empty default assignee is nobody, and no ask waits on it.
func WaitsOnDefault(askedOf, assignee, defaultAssignee string) bool {
	return defaultAssignee != "" && cmp.Or(askedOf, assignee, defaultAssignee) == defaultAssignee
}

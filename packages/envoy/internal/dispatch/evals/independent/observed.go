package independent

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// observed.go is idle time by role and slot wait, read from R6's words and the method texts of the
// two evals (evals/methods/legion-idle-time.md and legion-slot-wait.md) alone. Both rules ask what
// the daemon last observed of an issue at each moment of its time: its phase, whether it held a
// slot, and each role's claim. So this reading cuts the issue's time at every moment one of those
// observations, or one of the issue's status writes, was recorded (pieces), and asks each piece, at
// its start, what the newest of each said (sightingAt). Between two cuts nothing either rule reads
// changes, so a piece counts whole or not at all. The other rules of this reading draw each fact
// as stretches and intersect them; these two draw none.
//
// The words leave two cases open, and this reading takes each as the words stand. A `null`
// observation names nothing: an issue whose newest phase observation is `null` has no observed
// phase, and one whose newest slot observation is `null` holds no slot, whatever else was recorded
// at that moment, though the sampler writes `null` for every field of an issue the daemon stops
// reporting. And `design`, on the architect's bar, is the daemon's `admitted` while the issue
// holds a slot of its own, as dispatch://INFRA-1077's words make it ("A root in `admitted` that
// holds a slot is in design"), for a root and a child alike, as phase time's bars read it
// (sampledOf): R6 has idle time and time per phase read one rule of bars.

// sighting is what an issue's newest observations and its newest status write say as of one
// moment: the phase the daemon observed ("" for none), whether the issue held a slot of its own,
// whether it sat parked in `backlog` or `icebox`, and each role whose newest claim observation is
// `working`.
type sighting struct {
	phase   string
	holds   bool
	parked  bool
	working map[string]bool
}

// sightingAt is what the issue's newest observations and status write recorded at or before at say.
func (i *issue) sightingAt(at time.Time) sighting {
	seen := sighting{working: map[string]bool{}}
	if newest, held := newestAt(i.observed[fieldPhase], at); held {
		seen.phase = jsonString(newest.value)
	}
	if newest, held := newestAt(i.observed[fieldSlot], at); held {
		// The sampler records the slot an issue holds as its number, and none as `null`.
		seen.holds = newest.value != "" && newest.value != "null"
	}
	if newest, held := newestAt(i.statusMarks, at); held {
		seen.parked = slices.Contains(parkedStatuses, newest.value)
	}
	for field, marks := range i.observed {
		role, claim := strings.CutPrefix(field, fieldClaimPrefix)
		if !claim {
			continue
		}
		if newest, held := newestAt(marks, at); held && jsonString(newest.value) == "working" {
			seen.working[role] = true
		}
	}
	return seen
}

// newestAt is the newest of marks, oldest first, recorded at or before at, found false where none
// was: an observation holds from the moment it was recorded until the next of its field, and a
// status write until the next status write.
func newestAt(marks []mark, at time.Time) (mark, bool) {
	after := sort.Search(len(marks), func(index int) bool { return marks[index].at.After(at) })
	if after == 0 {
		return mark{}, false
	}
	return marks[after-1], true
}

// cuts is every moment the issue's phase, slot, claim or status was recorded at.
func (i *issue) cuts() []time.Time {
	var moments []time.Time
	for field, marks := range i.observed {
		if field != fieldPhase && field != fieldSlot && !strings.HasPrefix(field, fieldClaimPrefix) {
			continue
		}
		for _, recorded := range marks {
			moments = append(moments, recorded.at)
		}
	}
	for _, recorded := range i.statusMarks {
		moments = append(moments, recorded.at)
	}
	return moments
}

// pieces is [from, to) cut at each of moments that falls inside it, in order.
func pieces(from, to time.Time, moments []time.Time) []span {
	if !to.After(from) {
		return nil
	}
	starts := []time.Time{from}
	for _, at := range moments {
		if at.After(from) && at.Before(to) {
			starts = append(starts, at)
		}
	}
	slices.SortFunc(starts, time.Time.Compare)
	starts = slices.CompactFunc(starts, time.Time.Equal)
	cut := make([]span, len(starts))
	for index, start := range starts {
		cut[index] = span{from: start, to: to}
		if index+1 < len(starts) {
			cut[index].to = starts[index+1]
		}
	}
	return cut
}

// slotWait is the issue's slot wait in hours: "the hours of the issue's observed life in which its
// phase is admitted and it holds no slot. It counts the whole life and leaves out the hours the
// issue is in `backlog` or `icebox`" (R6), "from the creation of the issue to its end", "the phase
// is the one the daemon observed", and "an issue that the daemon never observed has no slot wait"
// (legion-slot-wait.md).
func (r *reading) slotWait(key string) float64 {
	held := r.population.issues[key]
	var waited time.Duration
	for _, piece := range pieces(held.created, r.population.end(key), held.cuts()) {
		if seen := held.sightingAt(piece.from); seen.phase == phaseAdmitted && !seen.holds && !seen.parked {
			waited += piece.length()
		}
	}
	return waited.Hours()
}

// idleByRole is the issue's idle time by role, in hours for each of the six roles: "For role R, it
// is the hours of the issue's worked spans in which all three are true: the issue holds its own
// slot; its observed phase is in R's bar; and R's last observed claim state is not `working`. No
// claim counts as not working" (R6). The worked spans are "the spans that its work time counts"
// (legion-idle-time.md), worked, and R's bar is its bar of phase time (bars), on which the observed
// `admitted` of an issue that holds its own slot is `design`.
func (r *reading) idleByRole(key string, worked []span) map[string]float64 {
	held := r.population.issues[key]
	moments := held.cuts()
	idle := map[string]time.Duration{}
	for _, stretch := range worked {
		for _, piece := range pieces(stretch.from, stretch.to, moments) {
			seen := held.sightingAt(piece.from)
			if !seen.holds {
				continue
			}
			phase := seen.phase
			if phase == phaseAdmitted {
				phase = phaseDesign
			}
			for _, one := range bars {
				if one.role != noRole && slices.Contains(one.phases, phase) && !seen.working[one.role] {
					idle[one.role] += piece.length()
				}
			}
		}
	}
	hours := make(map[string]float64, len(bars)-1)
	for _, one := range bars {
		if one.role != noRole {
			hours[one.role] = rounded(idle[one.role].Hours())
		}
	}
	return hours
}

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
// The two method texts say what a drop is: "When the daemon stops reporting an issue, its sampler
// writes `null` for every field of the issue at one moment, and that is a drop, not a release. The
// phase and the slot that the issue held run on through a drop until the next observation that
// names a phase, and from that moment the issue holds a slot only if a slot observation at the same
// moment names one; where no such observation comes, they run to the issue's end." Idle time's
// adds: "Each claim that a drop writes `null` is no claim, so it counts as not working." So the
// phase at a moment is the one named by the newest observation that names a phase (phaseAt), and
// whether the issue holds a slot asks first whether a drop is in force (holdsSlotAt). `design`, on
// the architect's bar, is the daemon's `admitted` while the issue holds a slot of its own, for a
// child as for a root: dispatch://INFRA-1077's words give an issue a design phase "whether or not
// it has a parent", and R6 has idle time and time per phase read one rule of bars (sampledOf).

// sighting is what an issue's observations and its newest status write say as of one moment: the
// phase the daemon observed ("" for none), whether the issue held a slot of its own, whether it sat
// parked in `backlog` or `icebox`, and each role whose newest claim observation is `working`.
type sighting struct {
	phase   string
	holds   bool
	parked  bool
	working map[string]bool
}

// sightingAt is what the issue's observations and status writes recorded at or before at say.
func (i *issue) sightingAt(at time.Time) sighting {
	seen := sighting{
		phase:   phaseAt(i.observed[fieldPhase], at),
		holds:   holdsSlotAt(i.observed[fieldPhase], i.observed[fieldSlot], at, true),
		working: map[string]bool{},
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

// phaseAt is the phase named by the newest of phases (oldest first) recorded at or before at that
// names one, "" where none does: a drop's `null` names none, so the phase before it runs on.
func phaseAt(phases []mark, at time.Time) string {
	seen := recordedBy(phases, at, true)
	for index := len(seen) - 1; index >= 0; index-- {
		if phase := jsonString(seen[index].value); phase != "" {
			return phase
		}
	}
	return ""
}

// holdsSlotAt is whether an issue whose phase and slot observations are phases and slots (each
// oldest first) holds a slot of its own as of at, counting the observations recorded at at where
// through is true. With no drop among the phase observations, it holds one where its newest slot
// observation names one. While the newest drop is in force, no observation having named a phase
// since, it holds what it held as the drop came. Once an observation names a phase again, it holds
// one only where a slot observation recorded at or after that moment names one.
func holdsSlotAt(phases, slots []mark, at time.Time, through bool) bool {
	phases, slots = recordedBy(phases, at, through), recordedBy(slots, at, through)
	drop := len(phases) - 1
	for drop >= 0 && jsonString(phases[drop].value) != "" {
		drop--
	}
	if drop < 0 {
		return len(slots) > 0 && namesSlot(slots[len(slots)-1].value)
	}
	if drop == len(phases)-1 {
		return holdsSlotAt(phases, slots, phases[drop].at, false)
	}
	back := phases[drop+1].at
	since := slots[sort.Search(len(slots), func(index int) bool { return !slots[index].at.Before(back) }):]
	return len(since) > 0 && namesSlot(since[len(since)-1].value)
}

// namesSlot is whether a slot observation names a slot: the sampler records the slot an issue
// holds as its number, and none as `null`.
func namesSlot(value string) bool {
	return value != "" && value != "null"
}

// newestAt is the newest of marks, oldest first, recorded at or before at, found false where none
// was: a claim observation holds from the moment it was recorded until the next of its field, and a
// status write until the next status write.
func newestAt(marks []mark, at time.Time) (mark, bool) {
	seen := recordedBy(marks, at, true)
	if len(seen) == 0 {
		return mark{}, false
	}
	return seen[len(seen)-1], true
}

// recordedBy is marks, oldest first, cut to those recorded before at, and at at too where through
// is true.
func recordedBy(marks []mark, at time.Time, through bool) []mark {
	return marks[:sort.Search(len(marks), func(index int) bool {
		if through {
			return marks[index].at.After(at)
		}
		return !marks[index].at.Before(at)
	})]
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

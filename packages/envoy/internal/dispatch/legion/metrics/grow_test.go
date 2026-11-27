package metrics

import (
	"maps"
	"testing"
)

// TestABoardEntrysRatesAreTheGrowthOfTheFactsItCarries: Rates reads, from a growth keyed as the
// stored rows key it, the rates of the work time, the slot hours, the last moment of measured
// work and each column's hours, and leaves out every other name - a bar, an idle role, the parked,
// queued and slot-wait hours, the hours after the first review, the first moment of work - and a
// rate of 0. Each of the facts the entry carries grows the entry alone; a growth of facts the
// entry does not carry grows nothing it carries, as an empty one does.
func TestABoardEntrysRatesAreTheGrowthOfTheFactsItCarries(t *testing.T) {
	growth := Growth{
		"work_hours": 1, "slot_hours": 2, "work_last_at": 1, "phase_hours.implementing": 1,
		"phase_hours.reviewing": 0, "backlog_or_icebox_hours": 1, "queued_hours": 1, "slot_wait_hours": 1,
		"implementing_hours_after_first_review": 1, "testing_hours_after_first_review": 1,
		"work_first_at": 1, "bars.implementer.hours": 1, "bars.implementer.segments.idle": 1,
		"idle_by_role.reviewer": 1,
	}
	rates := growth.Rates()
	wantPhase := map[string]float64{"implementing": 1}
	if rates.WorkHours != 1 || rates.SlotHours != 2 || rates.WorkLastAt != 1 || !maps.Equal(rates.HoursByPhase, wantPhase) || !rates.Grows() {
		t.Errorf("%v reads as %+v, grows %t; want work hours 1, slot hours 2, work_last_at 1 and the phase hours %v, which grow",
			growth, rates, rates.Grows(), wantPhase)
	}

	for _, one := range []Growth{{"work_hours": 1}, {"slot_hours": 1}, {"work_last_at": 1}, {"phase_hours.queue": 1}} {
		if !one.Rates().Grows() {
			t.Errorf("%v reads as %+v, which does not grow: it is a rate of a fact the entry carries", one, one.Rates())
		}
	}

	for name, still := range map[string]Growth{
		"an empty growth": nil,
		"a growth of facts the entry does not carry": {
			"backlog_or_icebox_hours": 1, "work_first_at": 1, "bars.implementer.hours": 1, "idle_by_role.reviewer": 1,
			"phase_hours.design": 0,
		},
	} {
		rates := still.Rates()
		if rates.Grows() || rates.WorkHours != 0 || rates.SlotHours != 0 || rates.WorkLastAt != 0 || rates.HoursByPhase != nil {
			t.Errorf("%s, %v, reads as %+v, grows %t; want no rate at all", name, still, rates, rates.Grows())
		}
	}
}

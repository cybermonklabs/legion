package legionfacts

import (
	"maps"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/metrics"
)

// precisionTolerance is how far apart a stored row read at a moment and the computation at that
// moment may stand: a ten-thousandth of an hour, what R2 holds a read to, and 1e-9 beside it, so
// floating-point noise in the sum of a value and its rate times the hours fails no run.
const precisionTolerance = 1e-4 + 1e-9

// TestAStoredRowGrownToAMomentIsTheComputationAtThatMomentToATenThousandthOfAnHour: a pass writes
// the rows of the rig's open issues, and the board's read of the open rows (ReadOpen) of each at a
// later moment, with no input in between, gives every hours fact the board shows - the work time,
// the slot hours and the hours of each column - within a ten-thousandth of an hour of the
// computation at that moment: ProjectFactsClosed over the same inputs, grown to the moment
// (ClosedFacts.FactsAt, which metrics' own precision test holds to the computation run at the
// moment). A read that grew a row from another moment than the one its facts are closed at (its
// updated_at in place of its as_of) runs ahead of or behind the computation by the time between
// the two, which this test fails. It logs the largest difference it found.
func TestAStoredRowGrownToAMomentIsTheComputationAtThatMomentToATenThousandthOfAnHour(t *testing.T) {
	t.Parallel()
	run := newConsumer(t, 0)
	run.pass(t)
	pool, project := run.database.Pool, run.fixture.Project

	population, err := metrics.ReadProject(t.Context(), pool, project, run.now)
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	closed := metrics.ProjectFactsClosed(population, nil)

	compared, grown := 0, 0
	largest, largestAt := 0.0, ""
	for _, later := range []time.Duration{time.Minute, 90 * time.Minute, 30 * time.Hour} {
		moment := run.now.Add(later)
		rows, _, err := ReadOpen(t.Context(), pool, project, moment)
		if err != nil {
			t.Fatalf("read the stored rows at %s: %v", moment, err)
		}
		for _, key := range slices.Sorted(maps.Keys(rows)) {
			row := rows[key]
			computation, has := closed[key]
			if !has {
				t.Errorf("%s has a stored row and the computation answers no facts of it", key)
				continue
			}
			if computation.ClosedAt != nil {
				continue
			}
			if len(computation.Growth) > 0 {
				grown++
			}
			compared++
			want := computation.FactsAt(moment)
			got := map[string]float64{"work_hours": row.WorkHours, "slot_hours": row.SlotHours}
			wanted := map[string]float64{"work_hours": want.WorkHours, "slot_hours": want.SlotHours}
			for column, hours := range row.PhaseHours {
				got["hours_by_phase."+column] = hours
			}
			for column, hours := range want.PhaseHours {
				wanted["hours_by_phase."+column] = hours
			}
			names := slices.Sorted(maps.Keys(got))
			for name := range wanted {
				if !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
			for _, name := range names {
				difference := math.Abs(got[name] - wanted[name])
				if difference > largest {
					largest, largestAt = difference, key+" "+name+" at "+moment.UTC().Format(time.RFC3339)
				}
				if difference > precisionTolerance {
					t.Errorf("%s %s read at %s = %v, the computation at that moment %v: %g hours apart",
						key, name, moment.UTC(), got[name], wanted[name], difference)
				}
			}
		}
	}
	if compared == 0 || grown == 0 {
		t.Fatalf("compared %d open rows at a moment, %d of them growing: this test examined nothing that grows", compared, grown)
	}
	t.Logf("%d reads of open rows compared, %d of them growing; the largest difference %g hours (%s)", compared, grown, largest, largestAt)
}

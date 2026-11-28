package metrics

import (
	"maps"
	"math"
	"slices"
	"testing"
	"time"
)

// precisionTolerance is how far apart a grown fact and the computation at the same moment may
// stand: a ten-thousandth of an hour, what R2 holds a stored row read at its own clock to, and
// 1e-9 beside it, so floating-point noise in the sum of a value and its rate times the hours
// fails no run.
const precisionTolerance = 1e-4 + 1e-9

// TestFactsGrownToAMomentAreTheComputationAtThatMomentToATenThousandthOfAnHour: over the closing
// fixtures (seedClosingHistory), each open issue's facts as ProjectFactsClosed closes them, grown
// to each closing moment (ClosedFacts.FactsAt), hold every hours fact the computation run at that
// moment over the same inputs answers (factsAt) to a ten-thousandth of an hour. The property test
// beside it holds the same facts to a tenth of an hour; this one holds the bound a stored row's
// read is held to. It logs the largest difference it found.
func TestFactsGrownToAMomentAreTheComputationAtThatMomentToATenThousandthOfAnHour(t *testing.T) {
	seeded := seedClosingHistory(t)
	pool, project := seeded.database.Pool, seeded.fixture.Project
	population, err := ReadProject(t.Context(), pool, project, seeded.now)
	if err != nil {
		t.Fatalf("read the population: %v", err)
	}
	closed := ProjectFactsClosed(population, nil)
	computed := &computedAt{t: t, pool: pool, project: project, held: map[time.Time]map[string]IssueFacts{}}

	compared, grown := 0, 0
	largest, largestAt := 0.0, ""
	for _, key := range slices.Sorted(maps.Keys(closed)) {
		facts := closed[key]
		if facts.ClosedAt != nil {
			continue
		}
		if len(facts.Growth) > 0 {
			grown++
		}
		for _, moment := range closingMoments(facts.AsOf) {
			want, has := computed.at(moment)[key]
			if !has {
				t.Errorf("%s has open facts and the computation at %s answers no facts of it", key, moment.UTC())
				continue
			}
			compared++
			got, wanted := flatHours(facts.FactsAt(moment)), flatHours(want)
			for _, name := range slices.Sorted(maps.Keys(eitherSide(got, wanted))) {
				difference := math.Abs(got[name] - wanted[name])
				if difference > largest {
					largest, largestAt = difference, key+" "+name+" at "+moment.UTC().Format(time.RFC3339)
				}
				if difference > precisionTolerance {
					t.Errorf("%s %s grown to %s (%s after its closing moment %s) = %v, the computation at that moment %v: %g hours apart",
						key, name, moment.UTC(), moment.Sub(facts.AsOf), facts.AsOf.UTC(), got[name], wanted[name], difference)
				}
			}
		}
	}
	if compared == 0 || grown == 0 {
		t.Fatalf("compared %d open issues' facts at a moment, %d of them growing: this test examined nothing that grows", compared, grown)
	}
	t.Logf("%d comparisons of open issues' grown facts, %d issues growing; the largest difference %g hours (%s)", compared, grown, largest, largestAt)
}

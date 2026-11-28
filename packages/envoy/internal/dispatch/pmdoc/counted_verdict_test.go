package pmdoc

import (
	"fmt"
	"strings"
	"testing"
)

// countedMargin leaves room for changes in work without losing the floor that proves the
// counters saw the input or the ceiling that catches expensive work at both sizes.
const countedMargin = 4

// countedBlockLimit refuses a 32-bit coverage counter halfway to wrapping. A larger input
// could otherwise make the next count look smaller than the work it counted.
const countedBlockLimit = 1 << 31

// countedStatementRun holds a child's statement total before any baseline is subtracted.
type countedStatementRun struct {
	units      int
	statements uint64
}

// countedStatementDifferences subtracts both baselines after checking that neither subtraction
// can wrap. A nil none means the leaf only measures the operation. Otherwise withPreparation
// includes the statements spent building the input, even if the difference is zero.
func countedStatementDifferences(name string, size int, none *countedStatementRun, prepared, whole countedStatementRun) (operation, withPreparation uint64, err error) {
	if none != nil {
		if none.statements == 0 {
			return 0, 0, fmt.Errorf("%s at size %d: the none baseline counted no statements", name, size)
		}
		if whole.statements < none.statements {
			return 0, 0, fmt.Errorf("%s at size %d: the whole run counted %d statements, fewer than the none baseline's %d", name, size, whole.statements, none.statements)
		}
	}
	if prepared.units != whole.units {
		return 0, 0, fmt.Errorf("%s at size %d: the prepare-only run holds %d units and the whole run holds %d", name, size, prepared.units, whole.units)
	}
	if whole.statements < prepared.statements {
		return 0, 0, fmt.Errorf("%s at size %d: the whole run counted %d statements, fewer than the prepare-only run's %d", name, size, whole.statements, prepared.statements)
	}
	if none != nil {
		withPreparation = whole.statements - none.statements
	}
	return whole.statements - prepared.statements, withPreparation, nil
}

// countedReading sets a leaf's floor from the statements per added builder unit (past).
// Ceilings normally use the larger count per unit of the two sizes. For nested directive
// statements the ceiling uses the full body's count per character. A zero stage reading
// records no work in that stage.
type countedReading struct {
	past, most             float64
	searchedMost, scanMost float64
}

// recordLine formats a reading so a run can supply the exact values to record for its leaf.
func (r countedReading) recordLine() string {
	return fmt.Sprintf("reading to record: {past: %g, most: %g, searchedMost: %g, scanMost: %g}",
		r.past, r.most, r.searchedMost, r.scanMost)
}

// countedLeaf names the builder unit and the bounds the leaf applies. A heavy body also measures
// statements with preparation and allocation in the invoking build.
type countedLeaf struct {
	name, unit         string
	reading            countedReading
	heavyBody          bool
	fixedSize          bool
	ceilingAtFullBody  bool
	perCharacterGrowth bool
}

// countedSample keeps the builder's size apart from the body's length and the two builds' counts
// apart. Searched bytes sum ParseWork and RenderWork; scan bytes count ScanWork alone.
type countedSample struct {
	units, characters           int
	operation, withPreparation  uint64
	countedBytes, invokingBytes uint64
	searchedBytes, scanBytes    uint64
	busiest                     uint64
	busiestBlock                string
	operationByModule           map[string]uint64
	updateItems                 uint64
	walks                       *countedSample
}

// countedVerdict returns one diagnostic per broken bound and the readings. A fixed budget
// measures only the cap. Growth leaves judge the two search stages separately.
func countedVerdict(leaf countedLeaf, samples [2]countedSample) (failures []string, readings string) {
	small, large := samples[0], samples[1]
	measured := samples[:]
	if leaf.fixedSize {
		measured = measured[:1]
	}
	for _, sample := range measured {
		readings += fmt.Sprintf("%d %ss (%d body characters): %d operation-only statements, %d bytes allocated in the counted build, "+
			"%d searched bytes, %d scan bytes, busiest block %q ran %d times; ", sample.units, leaf.unit, sample.characters,
			sample.operation, sample.countedBytes, sample.searchedBytes, sample.scanBytes, sample.busiestBlock, sample.busiest)
		if len(sample.operationByModule) != 0 {
			readings += fmt.Sprintf("operation-only statements by module: %v; ", sample.operationByModule)
		}
		if leaf.heavyBody {
			readings += fmt.Sprintf("%d statements with preparation, %d bytes allocated in the invoking build; ",
				sample.withPreparation, sample.invokingBytes)
		}
	}
	readings = leaf.name + ": " + readings
	if leaf.fixedSize {
		return countedFixedBudget(leaf, small, readings)
	}
	alternating := small.walks != nil || large.walks != nil
	if small.units <= 0 || large.units <= small.units {
		return []string{fmt.Sprintf("%s: the builder sizes must be positive and increasing, got %d and %d", leaf.name, small.units, large.units)}, readings
	}
	if leaf.heavyBody || leaf.perCharacterGrowth || leaf.ceilingAtFullBody {
		if small.characters <= 0 || large.characters <= small.characters {
			return []string{fmt.Sprintf("%s: the body lengths must be positive and increasing, got %d and %d", leaf.name, small.characters, large.characters)}, readings
		}
	}
	units := [2]int{small.units, large.units}
	unit, slack := leaf.unit, float64(linearSlack)
	if leaf.perCharacterGrowth {
		units = [2]int{small.characters, large.characters}
		unit, slack = "body character", growthPerCharacter
	}
	growth := float64(units[1]) / float64(units[0])
	limit := slack * growth
	checkGrowth := func(what string, before, after uint64, allowNone bool) {
		if allowNone && before == 0 && after == 0 {
			return
		}
		if before == 0 {
			failures = append(failures, fmt.Sprintf("%s: counted no %s at %d %ss and %d at %d %ss: it is not linear in its %ss",
				leaf.name, what, units[0], unit, after, units[1], unit, unit))
		} else if ratio := float64(after) / float64(before); ratio > limit {
			failures = append(failures, fmt.Sprintf("%s: %s grow from %d to %d, %.2f times over %.2f times the %ss, more than %.2f: it is not linear in its %ss",
				leaf.name, what, before, after, ratio, growth, unit, limit, unit))
		}
	}
	checkNondecreasing := func(what string, before, after uint64) {
		if after < before {
			failures = append(failures, fmt.Sprintf("%s: %s fall from %d to %d: work that shrinks as the input grows is a coverage counter that wrapped",
				leaf.name, what, before, after))
		}
	}
	checkNondecreasing("operation-only statements", small.operation, large.operation)
	if !alternating {
		checkGrowth("operation-only statements", small.operation, large.operation, false)
	}
	checkGrowth("counted build bytes allocated", small.countedBytes, large.countedBytes, false)
	if leaf.heavyBody {
		checkNondecreasing("statements with preparation", small.withPreparation, large.withPreparation)
		checkGrowth("statements with preparation", small.withPreparation, large.withPreparation, false)
		checkGrowth("invoking build bytes allocated", small.invokingBytes, large.invokingBytes, false)
	}

	// The floor measures added builder units even when growth and ceilings use body characters.
	var added float64
	if large.operation >= small.operation {
		added = float64(large.operation - small.operation)
	} else {
		added = -float64(small.operation - large.operation)
	}
	past := added / float64(large.units-small.units)
	ceilingUnits, ceilingUnit := units, unit
	if leaf.ceilingAtFullBody {
		ceilingUnits, ceilingUnit = [2]int{small.characters, large.characters}, "body character"
	}
	perUnit := func(count uint64, i int) float64 { return float64(count) / float64(ceilingUnits[i]) }
	reading := countedReading{
		past:         past,
		most:         max(perUnit(small.operation, 0), perUnit(large.operation, 1)),
		searchedMost: max(perUnit(small.searchedBytes, 0), perUnit(large.searchedBytes, 1)),
		scanMost:     max(perUnit(small.scanBytes, 0), perUnit(large.scanBytes, 1)),
	}
	if leaf.ceilingAtFullBody {
		reading.most = perUnit(large.operation, 1)
	}
	record := reading.recordLine()
	readings += fmt.Sprintf("%.2f times the %ss (growth at most %.2f); %s", growth, unit, limit, record)
	if leaf.reading.past <= 0 || leaf.reading.most <= 0 {
		failures = append(failures, fmt.Sprintf("%s: no reading is recorded; %s", leaf.name, record))
	}
	if floor := leaf.reading.past / countedMargin; !alternating && past < floor {
		failures = append(failures, fmt.Sprintf("%s: each %s past %d costs %.2f operation-only statements, fewer than %.2f: the counters did not see the operation read every %s",
			leaf.name, leaf.unit, small.units, past, floor, leaf.unit))
	}
	checkCeiling := func(what string, count uint64, i int, recorded float64) {
		if ceiling := countedMargin * recorded; recorded > 0 && perUnit(count, i) > ceiling {
			failures = append(failures, fmt.Sprintf("%s: %d %ss count %.2f %s a %s, more than %.2f",
				leaf.name, ceilingUnits[i], ceilingUnit, perUnit(count, i), what, ceilingUnit, ceiling))
		}
	}
	checkWrap := func(what string, sample countedSample) {
		if failure := countedWrapFailure(leaf, sample, what); failure != "" {
			failures = append(failures, failure)
		}
	}
	for i, sample := range samples {
		if !alternating {
			checkCeiling("operation-only statements", sample.operation, i, leaf.reading.most)
		}
		checkWrap("", sample)
		if leaf.heavyBody {
			failures = append(failures, countedHeavyBodyFailures(leaf, sample)...)
		}
	}
	for _, stage := range []struct {
		name          string
		before, after uint64
		recorded      float64
	}{
		{"searched bytes", small.searchedBytes, large.searchedBytes, leaf.reading.searchedMost},
		{"scan bytes", small.scanBytes, large.scanBytes, leaf.reading.scanMost},
	} {
		checkGrowth(stage.name, stage.before, stage.after, true)
		if stage.recorded == 0 && (stage.before != 0 || stage.after != 0) {
			failures = append(failures, fmt.Sprintf("%s: no reading is recorded for %s; %s", leaf.name, stage.name, record))
		}
		checkCeiling(stage.name, stage.before, 0, stage.recorded)
		checkCeiling(stage.name, stage.after, 1, stage.recorded)
	}
	if alternating {
		if small.walks == nil || large.walks == nil {
			failures = append(failures, leaf.name+": missing a ygo comparison")
		} else {
			checkNondecreasing("ygo Delete and Insert statements", small.walks.operation, large.walks.operation)
			for _, sample := range samples {
				checkWrap(" in ygo's Delete and Insert comparison", *sample.walks)
				broken, details := countedAlternatingBounds(leaf.name, sample)
				failures = append(failures, broken...)
				readings += "; " + details
			}
		}
	}
	return failures, readings
}

// countedFixedBudget applies the shared heavy-body limits once, at the cap rather than at
// two sizes. The recorded reading has no floor because the leaf does not measure growth.
func countedFixedBudget(leaf countedLeaf, sample countedSample, readings string) ([]string, string) {
	if sample.units <= 0 || sample.characters <= 0 {
		return []string{fmt.Sprintf("%s: want positive builder size and body length, got %d and %d",
			leaf.name, sample.units, sample.characters)}, readings
	}
	failures := countedHeavyBodyFailures(leaf, sample)
	if failure := countedWrapFailure(leaf, sample, ""); failure != "" {
		failures = append(failures, failure)
	}
	units := float64(sample.units)
	record := countedReading{
		most:         float64(sample.operation) / units,
		searchedMost: float64(sample.searchedBytes) / units,
		scanMost:     float64(sample.scanBytes) / units,
	}.recordLine()
	if leaf.reading.most <= 0 {
		failures = append(failures, fmt.Sprintf("%s: no reading is recorded; %s", leaf.name, record))
	}
	return failures, readings + record
}

// countedWrapFailure identifies a block whose counter ran halfway to wrapping.
func countedWrapFailure(leaf countedLeaf, sample countedSample, what string) string {
	if sample.busiest < countedBlockLimit {
		return ""
	}
	return fmt.Sprintf("%s: the block %s ran %d times at %d %ss, at or past 2^31: its coverage counter can wrap at the next size%s",
		leaf.name, sample.busiestBlock, sample.busiest, sample.units, leaf.unit, what)
}

// countedAlternatingBounds compares the write with the work ygo necessarily does at each
// replaced index. The Update reading includes its read-back, which only tightens the comparison.
func countedAlternatingBounds(name string, sample countedSample) (failures []string, reading string) {
	walks := ygoWalks(sample.units)
	allowed := walks + alternatingAllowance*uint64(sample.units)
	perAllowed := float64(sample.operation) / float64(allowed)
	comparison := sample.walks
	at := fmt.Sprintf("%s at %d blocks", name, sample.units)
	reading = fmt.Sprintf("%d blocks: Update reached %d items (ygo walks %d, allowed %d); %.6f statements per item allowed; "+
		"ygo comparison counted %d statements, busiest block %q ran %d times",
		sample.units, sample.updateItems, walks, allowed, perAllowed, comparison.operation, comparison.busiestBlock, comparison.busiest)
	if sample.updateItems > allowed {
		failures = append(failures, fmt.Sprintf("%s: Update reached %d items, more than %d (ygo walks %d plus %d a block)",
			at, sample.updateItems, allowed, walks, alternatingAllowance))
	}
	if perAllowed < alternatingFloor || perAllowed > alternatingCeiling {
		failures = append(failures, fmt.Sprintf("%s: Update counted %.6f statements per item allowed, outside %.6f to %.6f",
			at, perAllowed, alternatingFloor, alternatingCeiling))
	}
	if comparison.operation == 0 {
		failures = append(failures, at+": ygo's Delete and Insert comparison counted no statements")
	} else if sample.operation > 0 && (sample.operation-1)/alternatingAllowance >= comparison.operation {
		failures = append(failures, fmt.Sprintf("%s: Update counted %d statements, more than %d times ygo's %d",
			at, sample.operation, alternatingAllowance, comparison.operation))
	}
	return failures, reading
}

// countedHeavyBodyFailures applies the same fixed limits to each build, in that build's units.
// The counted build uses builder units. The invoking build and the count with preparation use
// body characters, which include a fixed prefix and may be fewer or more than the builder units.
func countedHeavyBodyFailures(leaf countedLeaf, sample countedSample) (failures []string) {
	for _, budget := range []struct {
		name, unit string
		count      uint64
		units      int
		perUnit    uint64
	}{
		{"operation-only statements", leaf.unit, sample.operation, sample.units, statementsPerCharacter},
		{"counted build bytes allocated", leaf.unit, sample.countedBytes, sample.units, bytesPerCharacter},
		{"statements with preparation", "body character", sample.withPreparation, sample.characters, statementsPerCharacter},
		{"invoking build bytes allocated", "body character", sample.invokingBytes, sample.characters, bytesPerCharacter},
	} {
		// Divide the count rather than multiplying the limit, which could overflow.
		if budget.count > 0 && (budget.count-1)/uint64(budget.units) >= budget.perUnit {
			failures = append(failures, fmt.Sprintf("%s: %d %s at %d %ss exceed the fixed limit of %d a %s",
				leaf.name, budget.count, budget.name, budget.units, budget.unit, budget.perUnit, budget.unit))
		}
	}
	return failures
}

// TestCountGrowthVerdictCatchesQuadraticWorkAndRefusesACountOfNone checks each refusal without
// running a counted child. Its two-fold step is a control: at that step the slack hides a
// quadratic count, unlike at the step the growth leaves use.
func TestCountGrowthVerdictCatchesQuadraticWorkAndRefusesACountOfNone(t *testing.T) {
	t.Parallel()
	leaf := countedLeaf{name: "synthetic input", unit: "character", reading: countedReading{past: 1000, most: 1000}}
	linear := func(small, large int) [2]countedSample {
		return [2]countedSample{
			{units: small, characters: small, operation: uint64(small) * 1000, countedBytes: uint64(small) * 1000},
			{units: large, characters: large, operation: uint64(large) * 1000, countedBytes: uint64(large) * 1000},
		}
	}
	const small, large = 1000, 1000 * growthStep
	base := linear(small, large)
	quadratic := func(samples *[2]countedSample) {
		for i := range samples {
			units := uint64(samples[i].units)
			samples[i].operation = units * units
		}
	}
	check := func(t *testing.T, leaf countedLeaf, samples [2]countedSample, want ...string) {
		t.Helper()
		failures, readings := countedVerdict(leaf, samples)
		if len(failures) != len(want) {
			t.Errorf("failures = %q, want %q (%s)", failures, want, readings)
		}
		for _, words := range want {
			found := false
			for _, failure := range failures {
				if strings.Contains(failure, words) {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("no failure contains %q (got %q; %s)", words, failures, readings)
			}
		}
	}

	for _, tc := range []struct {
		name   string
		change func(*countedLeaf, *[2]countedSample)
		want   []string
	}{
		{"linear", nil, nil},
		{"linear with fixed overhead", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[0].operation += 500
			samples[1].operation += 500
		}, nil},
		{"quadratic ratio and ceiling", func(_ *countedLeaf, samples *[2]countedSample) {
			quadratic(samples)
		}, []string{"it is not linear", "statements a character, more than"}},
		{"no work at either size", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[0].operation, samples[1].operation = 0, 0
		}, []string{"counted no operation-only statements", "the counters did not see"}},
		{"no work at the smaller size", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[0].operation = 0
		}, []string{"counted no operation-only statements"}},
		{"constant work", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[0].operation, samples[1].operation = 17, 17
		}, []string{"the counters did not see"}},
		{"quadratic counted bytes", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[0].countedBytes, samples[1].countedBytes = small*small, large*large
		}, []string{"counted build bytes allocated grow"}},
		{"no counted bytes", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[0].countedBytes, samples[1].countedBytes = 0, 0
		}, []string{"counted no counted build bytes allocated"}},
		{"no recorded reading", func(leaf *countedLeaf, _ *[2]countedSample) {
			leaf.reading = countedReading{}
		}, []string{"synthetic input: no reading is recorded; reading to record: {past: 1000, most: 1000"}},
		{"block limit at the smaller size", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[0].busiest, samples[0].busiestBlock = 1<<31, "scan.go:10.2,12.3"
		}, []string{"the block scan.go:10.2,12.3 ran 2147483648 times at 1000 characters"}},
		{"block limit at the larger size", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[1].busiest, samples[1].busiestBlock = 1<<31, "walk.go:20.2,22.3"
		}, []string{"the block walk.go:20.2,22.3 ran 2147483648 times at 8000 characters"}},
		{"shrinking work", func(_ *countedLeaf, samples *[2]countedSample) {
			samples[1].operation = samples[0].operation - 1
		}, []string{"a coverage counter that wrapped", "the counters did not see"}},
		{"quadratic passes at the old two-fold step", func(_ *countedLeaf, samples *[2]countedSample) {
			*samples = linear(small, 2*small)
			quadratic(samples)
		}, nil},
		{"scan bytes cannot mask absent searched bytes", func(leaf *countedLeaf, samples *[2]countedSample) {
			leaf.reading.searchedMost, leaf.reading.scanMost = 1, 1
			samples[1].searchedBytes = 8
			samples[0].scanBytes, samples[1].scanBytes = 100, 800
		}, []string{"counted no searched bytes"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf, samples := leaf, base
			if tc.change != nil {
				tc.change(&leaf, &samples)
			}
			check(t, leaf, samples, tc.want...)
		})
	}

	for _, stage := range []struct {
		name    string
		set     func(*countedSample, uint64)
		setMost func(*countedReading, float64)
	}{
		{"searched bytes", func(s *countedSample, n uint64) { s.searchedBytes = n }, func(r *countedReading, n float64) { r.searchedMost = n }},
		{"scan bytes", func(s *countedSample, n uint64) { s.scanBytes = n }, func(r *countedReading, n float64) { r.scanMost = n }},
	} {
		t.Run(stage.name, func(t *testing.T) {
			for _, tc := range []struct {
				name          string
				before, after uint64
				most          float64
				want          string
			}{
				{"smaller ceiling only", 4100, 32_000, 1, "1000 characters count 4.10 " + stage.name + " a character, more than"},
				{"larger ceiling only", 4000, 32_800, 1, "8000 characters count 4.10 " + stage.name + " a character, more than"},
				{"none then some", 0, 8, 1, "counted no " + stage.name},
				{"growth only", 100, 2500, 1, stage.name + " grow from 100 to 2500"},
				{"unrecorded stage", 100, 800, 0, "no reading is recorded for " + stage.name + "; reading to record:"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					leaf, samples := leaf, base
					stage.setMost(&leaf.reading, tc.most)
					stage.set(&samples[0], tc.before)
					stage.set(&samples[1], tc.after)
					check(t, leaf, samples, tc.want)
				})
			}
		})
	}

	heavyLeaf := leaf
	heavyLeaf.unit, heavyLeaf.heavyBody = "repeated character", true
	heavySamples := func(samples [2]countedSample) [2]countedSample {
		for i := range samples {
			samples[i].withPreparation = 2 * samples[i].operation
			samples[i].invokingBytes = samples[i].countedBytes
		}
		return samples
	}
	t.Run("fixed budget only measures the cap", func(t *testing.T) {
		leaf := heavyLeaf
		leaf.fixedSize, leaf.reading = true, countedReading{most: 1000}
		samples := [2]countedSample{heavySamples(base)[0]}
		check(t, leaf, samples)
		samples[0].withPreparation = statementsPerCharacter*small + 1
		samples[0].invokingBytes = bytesPerCharacter*small + 1
		samples[0].busiest = countedBlockLimit
		check(t, leaf, samples, "statements with preparation at 1000 body characters exceed the fixed limit",
			"invoking build bytes allocated at 1000 body characters exceed the fixed limit", "coverage counter can wrap")
		leaf.reading = countedReading{}
		check(t, leaf, [2]countedSample{heavySamples(base)[0]}, "no reading is recorded; reading to record: {past: 0, most: 1000")
	})
	t.Run("full body statement ceiling uses body characters", func(t *testing.T) {
		leaf := leaf
		leaf.ceilingAtFullBody = true
		leaf.reading.most = 100
		samples := base
		samples[0].characters, samples[1].characters = small*5, large*10
		check(t, leaf, samples)
		_, readings := countedVerdict(leaf, samples)
		if !strings.Contains(readings, "most: 100,") {
			t.Errorf("reading does not record the full body: %s", readings)
		}
		samples[1].operation = 400*uint64(samples[1].characters) + 1
		check(t, leaf, samples, "operation-only statements grow", "operation-only statements a body character, more than")
	})
	t.Run("no invoking build bytes", func(t *testing.T) {
		samples := heavySamples(base)
		samples[0].invokingBytes, samples[1].invokingBytes = 0, 0
		check(t, heavyLeaf, samples, "counted no invoking build bytes allocated")
	})

	for _, body := range []struct {
		name              string
		units, characters int
		budgetUnit        string
		statements, bytes string
	}{
		{"fixed prefix", 21_700, 32_000, "repeated character", "operation-only statements", "counted build bytes allocated"},
		{"shorter than its builder size", 3999, 3992, "body character", "statements with preparation", "invoking build bytes allocated"},
	} {
		t.Run(body.name, func(t *testing.T) {
			for _, metric := range []struct {
				name  string
				limit uint64
				set   func(*countedSample, uint64)
			}{
				{body.statements, statementsPerCharacter, func(s *countedSample, n uint64) { s.operation, s.withPreparation = n, n }},
				{body.bytes, bytesPerCharacter, func(s *countedSample, n uint64) { s.countedBytes, s.invokingBytes = n, n }},
			} {
				for at, size := range []string{"smaller", "larger"} {
					t.Run(metric.name+" at the "+size+" size", func(t *testing.T) {
						leaf := heavyLeaf
						leaf.reading.most = statementsPerCharacter
						samples := heavySamples(linear(body.units, body.units*growthStep))
						for i := range samples {
							s := &samples[i]
							s.characters = s.units + body.characters - body.units
							count := metric.limit * uint64(min(s.units, s.characters))
							if i == at {
								count++
							}
							metric.set(s, count)
						}
						s := samples[at]
						want := fmt.Sprintf("%s at %d %ss exceed the fixed limit", metric.name, min(s.units, s.characters), body.budgetUnit)
						check(t, leaf, samples, want)
					})
				}
			}
		})
	}

	t.Run("per character growth", func(t *testing.T) {
		leaf := heavyLeaf
		leaf.perCharacterGrowth = true
		leaf.reading.searchedMost, leaf.reading.scanMost = 1, 1
		samples := heavySamples(base)
		// The builder sizes grow eight-fold but the body grows six-fold. A count that grows
		// eighteen-fold is three times the body's growth, below even 2.5 times the builder's.
		samples[0].characters, samples[1].characters = 2000, 12_000
		t.Run("one and a half times passes", func(t *testing.T) {
			runs := samples
			runs[1].operation = 9 * runs[0].operation
			check(t, leaf, runs)
		})
		for _, metric := range []struct {
			name   string
			change func(*[2]countedSample)
		}{
			{"operation-only statements", func(s *[2]countedSample) {
				s[1].operation = 18 * s[0].operation
				s[1].withPreparation = s[1].operation
			}},
			{"statements with preparation", func(s *[2]countedSample) { s[1].withPreparation = 18 * s[0].withPreparation }},
			{"counted build bytes allocated", func(s *[2]countedSample) { s[1].countedBytes = 18 * s[0].countedBytes }},
			{"invoking build bytes allocated", func(s *[2]countedSample) { s[1].invokingBytes = 18 * s[0].invokingBytes }},
			{"searched bytes", func(s *[2]countedSample) { s[0].searchedBytes, s[1].searchedBytes = 100, 1800 }},
			{"scan bytes", func(s *[2]countedSample) { s[0].scanBytes, s[1].scanBytes = 100, 1800 }},
		} {
			t.Run(metric.name+" grows three times", func(t *testing.T) {
				runs := samples
				metric.change(&runs)
				check(t, leaf, runs, metric.name+" grow from")
			})
		}
		t.Run("ceiling uses body length", func(t *testing.T) {
			runs := samples
			leaf := leaf
			leaf.reading.most = 150
			check(t, leaf, runs, "12000 body characters count 666.67 operation-only statements a body character, more than")
		})
	})

	t.Run("preparation exceeds the main statement limit", func(t *testing.T) {
		samples := heavySamples(base)
		none := countedStatementRun{statements: 200}
		for i := range samples {
			sample := &samples[i]
			total := uint64(sample.characters)*statementsPerCharacter + none.statements
			if i == 0 {
				total++
			}
			prepared := countedStatementRun{sample.units, total - sample.operation}
			op, withPreparation, err := countedStatementDifferences(leaf.name, sample.units, &none, prepared, countedStatementRun{sample.units, total})
			if err != nil || op != sample.operation || withPreparation != total-none.statements {
				t.Fatalf("differences = %d, %d, %v; want %d, %d, nil", op, withPreparation, err, sample.operation, total-none.statements)
			}
			sample.operation, sample.withPreparation = op, withPreparation
		}
		check(t, heavyLeaf, samples, "statements with preparation at 1000 body characters exceed the fixed limit")
	})

	t.Run("baseline refusals", func(t *testing.T) {
		const name, size = "synthetic baseline", 64
		for _, tc := range []struct {
			name            string
			none            uint64
			prepared, whole countedStatementRun
			want            string
		}{
			{"zero none baseline", 0, countedStatementRun{64, 20}, countedStatementRun{64, 30}, "the none baseline counted no statements"},
			{"whole below none", 31, countedStatementRun{64, 20}, countedStatementRun{64, 30}, "fewer than the none baseline's 31"},
			{"prepare-only unit mismatch", 10, countedStatementRun{63, 20}, countedStatementRun{64, 30}, "the prepare-only run holds 63 units and the whole run holds 64"},
			{"whole below prepare-only", 10, countedStatementRun{64, 31}, countedStatementRun{64, 30}, "fewer than the prepare-only run's 31"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				none := countedStatementRun{statements: tc.none}
				op, withPreparation, err := countedStatementDifferences(name, size, &none, tc.prepared, tc.whole)
				if err == nil || !strings.HasPrefix(err.Error(), "synthetic baseline at size 64: ") || !strings.Contains(err.Error(), tc.want) {
					t.Errorf("error = %v, want shape and size followed by %q", err, tc.want)
				}
				if op != 0 || withPreparation != 0 {
					t.Errorf("refused reading returned differences %d and %d", op, withPreparation)
				}
			})
		}
	})
	t.Run("operation without a none baseline", func(t *testing.T) {
		op, withPreparation, err := countedStatementDifferences(leaf.name, small, nil, countedStatementRun{small, 20}, countedStatementRun{small, 30})
		if op != 10 || withPreparation != 0 || err != nil {
			t.Errorf("differences = %d, %d, %v; want 10, 0, nil", op, withPreparation, err)
		}
	})
	t.Run("alternating index walks", func(t *testing.T) {
		leaf := countedLeaf{
			name: "alternating synthetic input", unit: "block",
			reading: countedReading{past: 1, most: 1},
		}
		for _, tc := range []struct {
			name   string
			change func(*countedSample)
			want   []string
		}{
			{"quadratic index walks pass", nil, nil},
			{"too many items reached", func(s *countedSample) { s.updateItems++ }, []string{"Update reached"}},
			{"too few statements per item", func(s *countedSample) { s.operation = s.updateItems }, []string{"statements per item allowed"}},
			{"too many statements per item", func(s *countedSample) {
				ceiling := alternatingCeiling
				s.operation = (uint64(ceiling) + 1) * s.updateItems
				s.walks.operation = (s.operation + alternatingAllowance - 1) / alternatingAllowance
			}, []string{"statements per item allowed"}},
			{"too many statements compared to ygo", func(s *countedSample) {
				s.walks.operation = (s.operation - 1) / alternatingAllowance
			}, []string{"more than 4 times ygo's"}},
			{"wrap in ygo", func(s *countedSample) { s.walks.busiest = countedBlockLimit }, []string{"coverage counter can wrap"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				samples := linear(10, 40)
				for i := range samples {
					sample := &samples[i]
					sample.updateItems = ygoWalks(sample.units) + alternatingAllowance*uint64(sample.units)
					sample.operation = 12 * sample.updateItems
					sample.walks = &countedSample{units: sample.units, operation: sample.operation / alternatingAllowance}
				}
				if tc.change != nil {
					tc.change(&samples[0])
				}
				check(t, leaf, samples, tc.want...)
			})
		}
	})
}

package experiment

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/config"
)

// twoArms is the experiment the mode cases below draw over: two arms of equal weight, the first of
// them the arm `on` names.
func twoArms(mode config.ExperimentMode) config.Experiment {
	return config.Experiment{
		Name:      "memory",
		Mode:      mode,
		Unit:      config.ExperimentTree,
		OnVariant: "memory",
		Variants: []config.Variant{
			{Name: "memory", Weight: 1},
			{Name: "none", Weight: 1},
		},
	}
}

// Each mode gives the variant its word names: `off` gives the empty string, a unit recorded at no
// variant; `on` gives the variant on_variant names; and `random` gives the arm the drawn value
// falls in, over a source read once. A mode the draw does not know — including the empty string,
// which the loader never leaves behind — is refused naming it, and a source that will not answer
// is the draw's own failure, wrapped under the experiment's name.
func TestEachModeDrawsTheVariantItsWordNames(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    config.ExperimentMode
		answers []uint64
		want    string
		reads   int
	}{
		{name: "off", mode: config.ExperimentOff, want: "", reads: 0},
		{name: "on", mode: config.ExperimentOn, want: "memory", reads: 0},
		{name: "random, the value in the first arm", mode: config.ExperimentRandom, answers: []uint64{0}, want: "memory", reads: 1},
		{name: "random, the value in the second arm", mode: config.ExperimentRandom, answers: []uint64{1}, want: "none", reads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &seededSource{answers: tc.answers}
			got, err := Draw(twoArms(tc.mode), source.draw)
			if err != nil {
				t.Fatalf("draw under mode %q: %v", tc.mode, err)
			}
			if got != tc.want {
				t.Fatalf("draw under mode %q = %q, want %q", tc.mode, got, tc.want)
			}
			if source.reads != tc.reads {
				t.Fatalf("draw under mode %q read the source %d times, want %d", tc.mode, source.reads, tc.reads)
			}
		})
	}

	for _, unknown := range []config.ExperimentMode{"sometimes", ""} {
		source := &seededSource{answers: []uint64{0}}
		variant, err := Draw(twoArms(unknown), source.draw)
		if err == nil || !strings.Contains(err.Error(), `"`+string(unknown)+`"`) {
			t.Fatalf("draw under the unknown mode %q = %q, %v; want an error naming it", unknown, variant, err)
		}
		if variant != "" || source.reads != 0 {
			t.Fatalf("draw under the unknown mode %q = %q over %d reads; want no variant and no read",
				unknown, variant, source.reads)
		}
	}

	refused := errors.New("the random source is closed")
	source := &seededSource{err: refused}
	variant, err := Draw(twoArms(config.ExperimentRandom), source.draw)
	if !errors.Is(err, refused) || variant != "" {
		t.Fatalf("draw over a failing source = %q, %v; want no variant and the source's own error", variant, err)
	}
	if !strings.Contains(err.Error(), `"memory"`) {
		t.Fatalf("the failing source's error is %q, want it to name the experiment", err)
	}
}

// The walk is over the variants' cumulative weights in their declared order, so the arms of
// weights 2, 3 and 5 take the values 0-1, 2-4 and 5-9 of a draw below 10. Every boundary is
// checked — the first value of each arm's range and the last — so a walk that compares one value
// too early or too late fails here. The bound handed to the source is the total weight, since a
// draw at the total itself belongs to no arm.
func TestTheCumulativeWalkNamesTheArmEachBoundaryValueFallsIn(t *testing.T) {
	experiment := config.Experiment{
		Name: "overlays",
		Mode: config.ExperimentRandom,
		Unit: config.ExperimentIssue,
		Variants: []config.Variant{
			{Name: "control", Weight: 2},
			{Name: "notes", Weight: 3},
			{Name: "notes-and-overlays", Weight: 5},
		},
	}
	for _, tc := range []struct {
		drawn uint64
		want  string
	}{
		{drawn: 0, want: "control"},
		{drawn: 1, want: "control"},
		{drawn: 2, want: "notes"},
		{drawn: 4, want: "notes"},
		{drawn: 5, want: "notes-and-overlays"},
		{drawn: 9, want: "notes-and-overlays"},
	} {
		source := &seededSource{answers: []uint64{tc.drawn}}
		got, err := Draw(experiment, source.draw)
		if err != nil {
			t.Fatalf("draw over the source value %d: %v", tc.drawn, err)
		}
		if got != tc.want {
			t.Fatalf("the source value %d drew %q, want %q", tc.drawn, got, tc.want)
		}
		if len(source.bounds) != 1 || source.bounds[0] != 10 {
			t.Fatalf("the source value %d was asked for under the bounds %v, want one draw below 10",
				tc.drawn, source.bounds)
		}
	}

	// A source that answers at its own bound belongs to no arm, and is refused rather than
	// rounded into the last one.
	beyond := &seededSource{answers: []uint64{10}}
	if got, err := Draw(experiment, beyond.draw); err == nil || got != "" {
		t.Fatalf("a source answering 10 below the bound 10 drew %q, %v; want no variant and an error", got, err)
	}
}

// The source is read once for each unit of each experiment that draws, and not at all for an
// experiment whose mode settles the arm by itself: five units over one `random`, one `on` and one
// `off` experiment read it five times.
func TestTheSourceIsReadOncePerExperimentPerUnitAndOnlyUnderRandom(t *testing.T) {
	const units = 5
	experiments := []config.Experiment{
		twoArms(config.ExperimentRandom),
		twoArms(config.ExperimentOn),
		twoArms(config.ExperimentOff),
	}
	source := &seededSource{}
	for range units {
		for _, experiment := range experiments {
			source.answers = []uint64{0}
			if _, err := Draw(experiment, source.draw); err != nil {
				t.Fatalf("draw the experiment %q under mode %q: %v", experiment.Name, experiment.Mode, err)
			}
		}
	}
	if source.reads != units {
		t.Fatalf("%d units over one random, one on and one off experiment read the source %d times, want %d",
			units, source.reads, units)
	}
}

// Many draws fall in the arms in the shares the weights declare: 2,000 units over the arms of
// weights 1 and 3 give the second arm between 70% and 80% of them, and 2,000 units over three arms
// of equal weight give each arm between 29% and 38%. The draws are the daemon's own source, so the
// bands hold over crypto/rand rather than over a fixture.
func TestTheWeightsDecideTheSharesOfManyDraws(t *testing.T) {
	const units = 2000
	t.Run("two arms of weights 1 and 3", func(t *testing.T) {
		experiment := config.Experiment{
			Name: "overlays",
			Mode: config.ExperimentRandom,
			Variants: []config.Variant{
				{Name: "control", Weight: 1},
				{Name: "overlays", Weight: 3},
			},
		}
		drawn := drawMany(t, experiment, units)
		share := float64(drawn["overlays"]) / units * 100
		if share < 70 || share > 80 {
			t.Errorf("the arm of weight 3 took %d of %d draws (%.1f%%), want between 70%% and 80%%",
				drawn["overlays"], units, share)
		}
		if drawn["control"]+drawn["overlays"] != units {
			t.Errorf("%d draws fell in the two arms %d and %d times, want every draw in an arm",
				units, drawn["control"], drawn["overlays"])
		}
	})
	t.Run("three arms of equal weight", func(t *testing.T) {
		experiment := config.Experiment{
			Name: "instructions",
			Mode: config.ExperimentRandom,
			Variants: []config.Variant{
				{Name: "control", Weight: 1},
				{Name: "short", Weight: 1},
				{Name: "long", Weight: 1},
			},
		}
		drawn := drawMany(t, experiment, units)
		for _, name := range []string{"control", "short", "long"} {
			share := float64(drawn[name]) / units * 100
			if share < 29 || share > 38 {
				t.Errorf("the arm %q took %d of %d draws (%.1f%%), want between 29%% and 38%%",
					name, drawn[name], units, share)
			}
		}
	})
}

// drawMany draws experiment for units units over the daemon's own source and counts the arms.
func drawMany(t *testing.T, experiment config.Experiment, units int) map[string]int {
	t.Helper()
	drawn := map[string]int{}
	for range units {
		variant, err := Draw(experiment, CryptoSource)
		if err != nil {
			t.Fatalf("draw the experiment %q: %v", experiment.Name, err)
		}
		drawn[variant]++
	}
	return drawn
}

// A `random` experiment the daemon could not draw is refused by its own message, and no source is
// read for it: one that declares no variant at all, one whose weights total zero, and one whose
// weights total more than a draw carries, which is a sum of many large weights rather than one
// negative one.
func TestAZeroTotalWeightAndAnOverflowingWeightSumAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name     string
		variants []config.Variant
		message  string
	}{
		{
			name:     "no variant at all",
			variants: nil,
			message:  "declares no variant to draw among",
		},
		{
			name:     "a total weight of zero",
			variants: []config.Variant{{Name: "control", Weight: 0}, {Name: "overlays", Weight: 0}},
			message:  "the variants' weights total zero",
		},
		{
			name: "a total weight that does not fit",
			variants: []config.Variant{
				{Name: "control", Weight: math.MaxInt},
				{Name: "overlays", Weight: math.MaxInt},
				{Name: "instructions", Weight: math.MaxInt},
			},
			message: fmt.Sprintf("the variants' weights total more than %d", uint64(math.MaxUint64)),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &seededSource{answers: []uint64{0}}
			experiment := config.Experiment{Name: "overlays", Mode: config.ExperimentRandom, Variants: tc.variants}
			variant, err := Draw(experiment, source.draw)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("draw over %s = %q, %v; want an error saying %q", tc.name, variant, err, tc.message)
			}
			if variant != "" || source.reads != 0 {
				t.Fatalf("draw over %s = %q over %d reads; want no variant and no read",
					tc.name, variant, source.reads)
			}
		})
	}
}

// The source the daemon draws `random` with is crypto/rand's, not math/rand's. Many draws answer
// inside the bound and are no constant; and the source read is crypto/rand.Reader itself, which
// this test proves by putting a reader of its own there: CryptoSource reads that reader — a
// math/rand source would not touch it — and reports what it refuses to read. The bound 256 is one
// byte wide, so crypto/rand.Int takes one read of it and keeps the byte as it stands.
func TestTheDefaultSourceReadsCryptoRandAndNotMathRand(t *testing.T) {
	const bound = 7
	answers := map[uint64]int{}
	for range 400 {
		drawn, err := CryptoSource(bound)
		if err != nil {
			t.Fatalf("draw below %d over the default source: %v", bound, err)
		}
		if drawn >= bound {
			t.Fatalf("the default source answered %d below the bound %d, want a value under it", drawn, bound)
		}
		answers[drawn]++
	}
	if len(answers) != bound {
		t.Fatalf("400 draws below %d gave %d of the %d values, want every one: the source is no uniform draw",
			bound, len(answers), bound)
	}

	original := rand.Reader
	t.Cleanup(func() { rand.Reader = original })
	source := &countingReader{}
	rand.Reader = source
	for _, byteCase := range []struct {
		give byte
		want uint64
	}{{give: 0x2A, want: 42}, {give: 0x00, want: 0}, {give: 0xFF, want: 255}} {
		source.give = byteCase.give
		drawn, err := CryptoSource(256)
		if err != nil {
			t.Fatalf("CryptoSource over a reader of bytes %#x: %v", byteCase.give, err)
		}
		if drawn != byteCase.want {
			t.Fatalf("CryptoSource over a reader of bytes %#x = %d, want %d: it read some other source",
				byteCase.give, drawn, byteCase.want)
		}
	}
	if source.reads != 3 {
		t.Fatalf("CryptoSource read crypto/rand.Reader %d times in three draws, want one read each: it reads another source",
			source.reads)
	}

	source.err = errors.New("no entropy here")
	if drawn, err := CryptoSource(256); !errors.Is(err, source.err) || drawn != 0 {
		t.Fatalf("CryptoSource over a refusing reader = %d, %v; want no value and the reader's own error", drawn, err)
	}

	if drawn, err := CryptoSource(0); err == nil || drawn != 0 {
		t.Fatalf("CryptoSource below the bound 0 = %d, %v; want no value and an error rather than a panic", drawn, err)
	}
}

// seededSource stands in for the random source: it answers its values in their order, keeps every
// bound it was asked under, counts the reads, and refuses them all once err is set.
type seededSource struct {
	answers []uint64
	bounds  []uint64
	err     error
	reads   int
}

func (s *seededSource) draw(bound uint64) (uint64, error) {
	s.reads++
	s.bounds = append(s.bounds, bound)
	if s.err != nil {
		return 0, s.err
	}
	if len(s.answers) == 0 {
		return 0, fmt.Errorf("the source was read %d times, more often than it has answers", s.reads)
	}
	answer := s.answers[0]
	s.answers = s.answers[1:]
	return answer, nil
}

// countingReader stands in for crypto/rand.Reader: it serves give for every byte asked of it,
// counts the reads, and refuses them all once err is set.
type countingReader struct {
	give  byte
	err   error
	reads int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	if r.err != nil {
		return 0, r.err
	}
	for i := range p {
		p[i] = r.give
	}
	return len(p), nil
}

var _ io.Reader = (*countingReader)(nil)

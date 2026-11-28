package cputest

import "testing"

// Growth reads how the work of a run grows from small to large: a run of growthSteps steps of user
// work at small and of scale times as many at large grows about scale times, and one of scale times
// scale times as many at large, as a walk for each pair of units does, about scale times scale,
// past any bound of the first. The inputs of a third pair take growthBuildSteps to build, many times
// the work of a run, and its growth is still about scale: MeasureThread builds every run of a window
// before the window opens, where a build in the window would read the smaller side's eight builds
// against the larger side's one, under a growth of 2.
func TestGrowthReadsHowTheWorkOfARunGrows(t *testing.T) {
	const scale = 8
	steps := func(n int) func() func() { return Again(func() { growthSpin(n) }) }
	built := func(n int) func() func() {
		return func() func() {
			growthSpin(growthBuildSteps)
			return func() { growthSpin(n) }
		}
	}
	for _, test := range []struct {
		name         string
		small, large func() func()
		least, most  float64
	}{
		{"work in proportion to the input", steps(growthSteps), steps(scale * growthSteps), scale / 2, scale * 2},
		{"work for each pair of units", steps(growthSteps), steps(scale * scale * growthSteps), scale * scale / 2, scale * scale * 2},
		{"work in proportion to the input, each input long to build", built(growthSteps), built(scale * growthSteps), scale / 2, scale * 2},
	} {
		growth, smallWork, largeWork := Growth(3, scale, test.small, test.large)
		t.Logf("%s: %d runs at small %v, one at large %v: %.1f times", test.name, scale, smallWork, largeWork, growth)
		if growth < test.least || growth > test.most {
			t.Errorf("%s: Growth read %.1f times, %v for %d runs at small and %v for one at large, want %.0f to %.0f times",
				test.name, growth, smallWork, scale, largeWork, test.least, test.most)
		}
	}
}

// growthSteps is how many steps of growthSpin a run at small takes, a few milliseconds on a core of
// a few GHz, and growthBuildSteps how many the build of one input of the third pair takes.
const (
	growthSteps      = 2_000_000
	growthBuildSteps = 20_000_000
)

// growthSink keeps the compiler from dropping the work of growthSpin.
var growthSink uint64

// growthSpin is steps of xorshift, work in user mode that makes no system call and allocates
// nothing.
func growthSpin(steps int) {
	x := growthSink | 1
	for range steps {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
	}
	growthSink = x
}

package launcher

import (
	"math"
	"testing"
)

// One statfs of this pod's volume, measured with `stat -f /legion` in an issue pod on
// 2026-11-10. The two free counts are what make the formula worth a test: f_bfree is every free
// block and f_bavail is what an unprivileged writer may still use, and they differ by the
// filesystem's reserve, so a percentage taken over the total blocks reads a different mark from
// the one df prints. `df -h /legion` printed `10%` for this same reading, where the unrounded
// fraction is 9.26 per cent, because df rounds up.
const (
	measuredBlockSize = 4096
	measuredBlocks    = 12_822_648
	measuredFree      = 11_635_892 // f_bfree
	measuredAvailable = 11_631_796 // f_bavail
)

// The two fractions the measurement above gives, to six figures: the one df computes, which
// Percent must answer, and the one a denominator of total blocks gives. They are written out
// rather than computed from the counts, because a test that computed its expectation with the
// formula under test would pass for any formula.
const (
	measuredPercent          = 9.258112 // used over used plus available
	measuredPercentOverTotal = 9.255151 // the wrong formula: used over the volume's total blocks
)

// Percent is used over used plus available, the fraction df computes, and the negative control
// is the same reading over the volume's total blocks: on a filesystem with a reserve the two
// read different marks, so a trimmer written with the wrong denominator would sweep at a
// fraction no operator reading df could predict. Both fractions here come from one measured
// statfs of this pod's volume, so what the case shows is the wrong formula giving a different
// answer rather than one number asserted against itself.
func TestTheVolumesPercentageIsUsedOverUsedPlusAvailable(t *testing.T) {
	t.Parallel()
	used := uint64(measuredBlocks-measuredFree) * measuredBlockSize
	available := uint64(measuredAvailable) * measuredBlockSize
	total := uint64(measuredBlocks) * measuredBlockSize

	got := volumeUsage{UsedBytes: used, AvailableBytes: available}.Percent()
	if math.Abs(got-measuredPercent) > 1e-5 {
		t.Errorf("Percent of the measured reading (used %d B, available %d B) = %.6f, want %.6f, the fraction df computes",
			used, available, got, measuredPercent)
	}
	overTotal := 100 * float64(used) / float64(total)
	if math.Abs(overTotal-measuredPercentOverTotal) > 1e-5 {
		t.Fatalf("the control reads %.6f over the volume's %d total blocks, want %.6f: the measured counts in this file no longer say what they did",
			overTotal, uint64(measuredBlocks), measuredPercentOverTotal)
	}
	if got == overTotal {
		t.Errorf("the control over total blocks reads the same mark %.6f as Percent, so this case proves nothing: the reserve between f_bfree %d and f_bavail %d is what makes the two differ",
			got, uint64(measuredFree), uint64(measuredAvailable))
	}
	// df prints 10% for this reading and the trimmer acts on 9.26: an operator comparing the two
	// in the pod must know that df rounds up, which is why Percent does not.
	if ceil := math.Ceil(got); ceil != 10 {
		t.Errorf("Percent %.6f rounds up to %.0f%%, want the 10%% df printed for this reading", got, ceil)
	}

	// The two readings a sweep can meet that are not a measurement: a path nothing is mounted at,
	// which reads 0 per cent and sweeps nothing, and a volume with nothing left, which reads 100.
	for _, tc := range []struct {
		name    string
		reading volumeUsage
		want    float64
	}{
		{"a volume nothing is mounted at", volumeUsage{}, 0},
		{"a volume with no available bytes left", volumeUsage{UsedBytes: total}, 100},
	} {
		if got := tc.reading.Percent(); got != tc.want {
			t.Errorf("Percent of %s = %.6f, want %.6f", tc.name, got, tc.want)
		}
	}
}

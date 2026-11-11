package launcher

import (
	"math"
	"runtime"
	"strings"
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
	examined := 1
	for _, tc := range []struct {
		name    string
		reading volumeUsage
		want    float64
	}{
		{"a volume nothing is mounted at", volumeUsage{}, 0},
		{"a volume with no available bytes left", volumeUsage{UsedBytes: total}, 100},
	} {
		examined++
		if got := tc.reading.Percent(); got != tc.want {
			t.Errorf("Percent of %s = %.6f, want %.6f", tc.name, got, tc.want)
		}
	}
	if examined != 3 {
		t.Fatalf("the case examined %d readings, want all 3", examined)
	}
}

// The reader itself is one statfs on Linux and a refusal naming the platform anywhere else, and
// the trimmer injects both through the same package variable (readVolumeUsage), so this case
// reads whichever twin the binary was built with. On Linux it reads a real filesystem — the
// test's own temporary directory, since a machine running the tests has no /legion — and holds
// the reading to what statfs can answer: a volume of some size, with the used bytes no more than
// the volume holds.
func TestTheVolumeReaderAnswersOneReadingOfTheFilesystemItIsGiven(t *testing.T) {
	dir := t.TempDir()
	reading, err := readVolumeUsage(dir)
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatalf("readVolumeUsage(%s) answered %+v on %s, want a refusal naming the platform", dir, reading, runtime.GOOS)
		}
		if !strings.Contains(err.Error(), runtime.GOOS) {
			t.Errorf("readVolumeUsage refused with %q, want the platform %q named in it", err, runtime.GOOS)
		}
		return
	}
	if err != nil {
		t.Fatalf("readVolumeUsage(%s): %v", dir, err)
	}
	if reading.UsedBytes+reading.AvailableBytes == 0 {
		t.Fatalf("readVolumeUsage(%s) = %+v, want a filesystem of some size", dir, reading)
	}
	if percent := reading.Percent(); percent < 0 || percent > 100 {
		t.Errorf("readVolumeUsage(%s) = %+v, %.6f per cent, want a fraction between 0 and 100", dir, reading, percent)
	}
}

//go:build linux

package volumestat

import (
	"testing"

	"golang.org/x/sys/unix"
)

// One statfs of an issue pod's volume, measured with `stat -f /legion` in an issue pod on
// 2026-11-10 (the launcher's volumeusage_test.go holds the same reading for the percentage its
// trimmer takes from it). The two free counts differ by the filesystem's reserve: f_bfree is every
// free block and f_bavail is what an unprivileged writer may still use.
const (
	measuredBlockSize = 4096
	measuredBlocks    = 12_822_648
	measuredFree      = 11_635_892 // f_bfree
	measuredAvailable = 11_631_796 // f_bavail
)

// The block counts of a statfs are in units of f_frsize, which is the unit df reads them in, and
// f_bsize is the unit only of a reading that leaves f_frsize 0. The counts are the measured reading
// above with a fragment size that is not the block size, so a reader that multiplied by f_bsize
// answers eight times the bytes df prints; and the same counts with no fragment size, so a reader
// with no fallback answers a volume of no bytes at all, which a trimmer reads as 0 per cent and
// sweeps nothing, and a cache reports as a volume with no room.
func TestTheVolumesBytesAreCountedInFragments(t *testing.T) {
	const fragment = measuredBlockSize / 8
	size := uint64(measuredBlocks) * fragment
	used := uint64(measuredBlocks-measuredFree) * fragment
	available := uint64(measuredAvailable) * fragment
	for _, tc := range []struct {
		name string
		fs   unix.Statfs_t
		want Reading
	}{
		{"a fragment smaller than the block", unix.Statfs_t{Bsize: measuredBlockSize, Frsize: fragment,
			Blocks: measuredBlocks, Bfree: measuredFree, Bavail: measuredAvailable}, Reading{SizeBytes: size, UsedBytes: used, AvailableBytes: available}},
		{"no fragment size", unix.Statfs_t{Bsize: measuredBlockSize,
			Blocks: measuredBlocks, Bfree: measuredFree, Bavail: measuredAvailable}, Reading{SizeBytes: 8 * size, UsedBytes: 8 * used, AvailableBytes: 8 * available}},
	} {
		if got := readingOfStatfs(&tc.fs); got != tc.want {
			t.Errorf("the reading of %s (f_bsize %d, f_frsize %d) = %+v, want %+v", tc.name, tc.fs.Bsize, tc.fs.Frsize, got, tc.want)
		}
	}
}

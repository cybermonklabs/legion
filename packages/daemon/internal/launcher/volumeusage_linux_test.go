//go:build linux

package launcher

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The block counts of a statfs are in units of f_frsize, which is the unit df reads them in, and
// f_bsize is the unit only of a reading that leaves f_frsize 0. The counts are this file's
// measured reading (volumeusage_test.go) with a fragment size that is not the block size, so a
// reader that multiplied by f_bsize answers eight times the bytes df prints; and the same counts
// with no fragment size, so a reader with no fallback answers a volume of no bytes at all, which
// reads 0 per cent and sweeps nothing.
func TestTheVolumesBytesAreCountedInFragments(t *testing.T) {
	const fragment = measuredBlockSize / 8
	used := uint64(measuredBlocks-measuredFree) * fragment
	available := uint64(measuredAvailable) * fragment
	for _, tc := range []struct {
		name string
		fs   unix.Statfs_t
		want volumeUsage
	}{
		{"a fragment smaller than the block", unix.Statfs_t{Bsize: measuredBlockSize, Frsize: fragment,
			Blocks: measuredBlocks, Bfree: measuredFree, Bavail: measuredAvailable}, volumeUsage{UsedBytes: used, AvailableBytes: available}},
		{"no fragment size", unix.Statfs_t{Bsize: measuredBlockSize,
			Blocks: measuredBlocks, Bfree: measuredFree, Bavail: measuredAvailable}, volumeUsage{UsedBytes: 8 * used, AvailableBytes: 8 * available}},
	} {
		if got := usageOfStatfs(&tc.fs); got != tc.want {
			t.Errorf("the reading of %s (f_bsize %d, f_frsize %d) = %+v, want %+v", tc.name, tc.fs.Bsize, tc.fs.Frsize, got, tc.want)
		}
	}
}

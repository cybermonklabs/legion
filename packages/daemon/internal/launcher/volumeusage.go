package launcher

import (
	"fmt"

	"github.com/sjawhar/legion/daemon/internal/volumestat"
)

// readVolumeUsage is one statfs of path, the pod's volume (VolumeRoot), as the used and available
// bytes df takes its percentage from: volumestat.Read, the module's one statfs reader, which the
// repository cache reads its own volume with too, and which refuses by name on every platform but
// Linux (volumestat_other.go). Its error is volumestat's, under this package's prefix.
//
// It is a package-level variable rather than a function because it is the trimmer's one reading
// of the machine: every test of a sweep injects a reading through it and counts the calls, which
// is this package's seam for a thing a test must fake (roleInitLaunch, testPGInterval). The statfs
// itself is volumestat's, shared with the repository cache, so this package holds no reader of its
// own.
var readVolumeUsage = func(path string) (volumeUsage, error) {
	reading, err := volumestat.Read(path)
	if err != nil {
		return volumeUsage{}, fmt.Errorf("launcher: %w", err)
	}
	return volumeUsage{UsedBytes: reading.UsedBytes, AvailableBytes: reading.AvailableBytes}, nil
}

// volumeUsage is one reading of the pod's volume.
//
// The reading itself is one statfs, which is Linux's call and has no portable form, so it is
// volumestat's, whose package holds the Linux reader and the non-Linux twin that refuses by name
// (volumestat_linux.go, volumestat_other.go); this type holds what the trimmer takes of it, so the
// fraction below has one body and a reading taken on either platform is read the same way.
type volumeUsage struct{ UsedBytes, AvailableBytes uint64 }

// Percent is the fraction df prints: used over used plus available, as a float. Available is what
// an unprivileged writer may still use, which on a filesystem with a reserve is less than what is
// free, so the denominator is neither the volume's size nor its total blocks: measured on this
// pod's volume on 2026-11-10, `stat -f /legion` gave f_bfree 11,635,892 against f_bavail
// 11,631,796, and a fraction taken over the total blocks reads a different mark from the one df
// prints (TestTheVolumesPercentageIsUsedOverUsedPlusAvailable).
//
// df rounds its percentage up and this does not: on the same volume `df -h /legion` printed 10%
// where the unrounded fraction is 9.26 per cent. The trimmer compares this unrounded fraction
// with the operator's marks (shimwire.LauncherVolumeTrim), so an operator reading df in the pod
// sees a number up to one point higher than the one the trimmer acted on.
//
// A volume whose used and available bytes are both zero — nothing is mounted at the path, or a
// filesystem reports neither — is 0 per cent rather than a division by zero, which is under every
// high mark the trimmer accepts (shimwire.VolumeMarkMin), so no sweep runs on it.
func (u volumeUsage) Percent() float64 {
	total := u.UsedBytes + u.AvailableBytes
	if total == 0 {
		return 0
	}
	return 100 * float64(u.UsedBytes) / float64(total)
}

package launcher

// volumeUsage is one reading of the pod's volume.
//
// The reading itself is one statfs, which is Linux's call and has no portable form, so it sits in
// a file of its own with a non-Linux twin beside it (volumeusage_linux.go, volumeusage_other.go),
// as role-init's namespaces do (roleinit_linux.go, roleinit_other.go); this file holds what both
// platforms share, as roleinit.go does, so the fraction below has one body and a reading taken on
// either platform is read the same way.
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
// filesystem reports neither — is 0 per cent rather than a division by zero, which is the one
// reading that makes no sweep run.
func (u volumeUsage) Percent() float64 {
	total := u.UsedBytes + u.AvailableBytes
	if total == 0 {
		return 0
	}
	return 100 * float64(u.UsedBytes) / float64(total)
}

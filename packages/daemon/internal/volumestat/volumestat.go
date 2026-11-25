// Package volumestat is the module's one reader of a volume's size and room: one statfs of a path,
// as df reads it. The launcher's trimmer reads the pod's volume with it (launcher.readVolumeUsage),
// and the repository cache reads its own volume with it at every /healthz answer
// (repocache.readVolume).
//
// The reading itself is one statfs, which is Linux's call and has no portable form, so it sits in
// a file of its own with a non-Linux twin beside it that refuses by name (volumestat_linux.go,
// volumestat_other.go); this file holds what both platforms share.
package volumestat

// Reading is one statfs of a volume, in bytes: its size (the total blocks), what is used (the total
// blocks less the free ones), and what is available, f_bavail, which is what an unprivileged writer
// may still use. On a filesystem with a reserve the available bytes are fewer than the free ones,
// so UsedBytes plus AvailableBytes may be less than SizeBytes.
type Reading struct {
	SizeBytes      uint64
	UsedBytes      uint64
	AvailableBytes uint64
}

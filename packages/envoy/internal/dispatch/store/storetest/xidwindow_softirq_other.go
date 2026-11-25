//go:build !linux

package storetest

// readXidSoftirqs counts nothing: this build pins no thread and reads no CPU's softirqs.
func readXidSoftirqs() xidSoftirqReading { return xidSoftirqReading{} }

// xidReadInterruptTimeCharged answers false: this build reads no kernel's interrupt accounting,
// and judges no gap to need it.
func xidReadInterruptTimeCharged() bool { return false }

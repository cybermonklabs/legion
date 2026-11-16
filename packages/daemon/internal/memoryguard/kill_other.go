//go:build !linux

package memoryguard

// defaultSignal is none: the pidfd kill is Linux's, and a guard with no kill to send turns off
// with one line when it runs (Guard.Run), rather than reading a memory it cannot act on.
func defaultSignal() Signaler { return nil }

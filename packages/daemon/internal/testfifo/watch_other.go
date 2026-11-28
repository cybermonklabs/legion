//go:build !linux

package testfifo

// Opened answers ErrUnsupported: no kernel but Linux's keeps the record a Watch reads.
func (w *Watch) Opened() (bool, error) { return false, ErrUnsupported }

// supported answers ErrUnsupported, for the reason Opened gives.
func supported() error { return ErrUnsupported }

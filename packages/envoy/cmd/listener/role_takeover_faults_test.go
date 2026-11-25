package main

import "testing"

// setTakeoverFaults puts faults in force on cfg's listener for the rest of t (its takeoverFaults,
// which every copy of cfg - the listener's routes, relays and sweep - shares), and none once t
// ends. A configuration that holds no seams is a rig that would force nothing, so it fails t rather
// than letting a row pass without the fault it names.
func setTakeoverFaults(t testing.TB, cfg listenerDeliveryHandlerConfig, faults takeoverFaultSet) {
	t.Helper()
	if cfg.takeoverFaults == nil {
		t.Fatal("this listener configuration holds no takeover fault seams (newListenerDeliveryHarnessOn gives one)")
	}
	cfg.takeoverFaults.set.Store(&faults)
	t.Cleanup(func() { cfg.takeoverFaults.set.Store(nil) })
}

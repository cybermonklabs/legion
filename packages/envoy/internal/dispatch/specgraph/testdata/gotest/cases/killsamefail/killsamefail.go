// Package killsamefail is package killsame with a test that fails: its TestSame waits until the
// TestSame of package killsamefail_test starts, and then fails, and that one sleeps past -timeout
// 1s, so go test kills it.
package killsamefail

// ExternalStarted is closed by the TestSame of package killsamefail_test when it starts.
var ExternalStarted = make(chan struct{})

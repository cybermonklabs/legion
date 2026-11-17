// Package killsame has two parallel tests named TestSame, one of package killsame and one of
// package killsame_test. The first waits until the second starts, and then passes. The second
// sleeps past -timeout 1s, so go test kills it. Both runs start before either ends, and the report
// names each event by the test's name alone, so it cannot say whose end the one end event is.
package killsame

// ExternalStarted is closed by the TestSame of package killsame_test when it starts.
var ExternalStarted = make(chan struct{})

//go:build !race

package natscallout

// raceEnabled reports whether the package was built with the race detector. What reads it is the
// restart storm (storm_test.go), which opens fewer connections under -race: the clients, the hub
// and the service all run in the test's own process, and the detector's instrumentation makes
// each connect several times the cost, so at full size the setup itself fails on a loaded runner
// before anything under test has started. envoy-go-alone's step "Test the xid window rows, the
// API's issue-list bound and the auth service's storm, alone" is its full-size run, on a runner
// nothing else loads, and envoy-go-race runs the smaller one.
const raceEnabled = false

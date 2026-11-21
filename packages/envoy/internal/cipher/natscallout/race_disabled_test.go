//go:build !race

package natscallout

// raceEnabled reports whether the package was built with the race detector. What reads it is the
// restart storm (storm_test.go), which opens fewer connections under -race: the clients, the hub
// and the service all run in the test's own process, and the detector's instrumentation makes
// each connect several times the cost, so at full size the setup itself fails on a loaded runner
// before anything under test has started. envoy-go-race's step "Test the wall-clock measurements
// alone" is its full-size run, and envoy-go-race's race step runs the smaller one.
const raceEnabled = false

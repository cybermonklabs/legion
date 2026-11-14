//go:build !race

package memoryguard

// raceEnabled reports whether the package was built with the race detector. What reads it is the
// time TestMemoryGuardDecidesWithinAnIntervalOfKillingAProcessWithAMebibyteOfArguments holds the
// decision after a kill to: the interval of 100 ms is the bar for the build the launcher runs,
// where the decision ends 4 to 19 ms after the kill, while the detector's instrumentation slows
// the credential rules' regular expressions about fourteen times, so under -race the time says
// nothing of the guard and the test skips that one assertion, keeping its kill line's. CI runs
// this package without -race: it is not among DAEMON_RACE_PACKAGES in
// .github/scripts/ci_job_reads.py.
const raceEnabled = false

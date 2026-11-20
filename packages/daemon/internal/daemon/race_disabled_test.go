//go:build !race

package daemon

// raceEnabled reports whether the package was built with the race detector. What reads it is the
// sweep's field test (podupdate_sweep_contract_test.go). The plain build runs all of it: the groups
// that put each kind of claim beside every other role in every state (sweepKindGroups), each pair
// of a claim's fields alone, on its own, at the round's opening and beside another role's ready
// claim, and each state of the machine's view at the opening and beside each field. The
// race build cuts the groups to the same-state group of each kind the sweep asks and each asked
// claim beside the ready claim of every other role (sweepRaceKindGroups), and sets no pair alone
// and no view state at the opening or beside a field, at a cost the race step's bound holds; what
// it keeps is the ask and its publish beside the machines' own work. daemon-go's step of what the
// race build cannot run runs that test plain by name (.github/scripts/ci_job_reads.py,
// DAEMON_RACE_IMPOSSIBLE_SCRIPT).
const raceEnabled = false

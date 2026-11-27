package main

import (
	"strconv"
	"strings"
	"time"
)

// ghDeadlineVar names the deadline a caller of `legion gh` gives its call, in Unix milliseconds:
// the moment the caller stops waiting for it. Where GitHub refuses the leak guard's REST
// visibility read for a spent core limit whose reset is near, the guard waits for the reset and
// reads again, but only where that wait and the reads after it end before the deadline of the
// context it runs under (leakguard's Guard.waitFits), so the wait stays inside the call's own
// bound. A caller with a bound of its own, the capability report's `github` row with its 8 s
// (packages/pi-legion's checkGitHub), ends the call from outside, which no context of this process
// shows. So the caller names its bound here, runGh runs the guard under it, and a wait the caller
// would not see the end of is not made: the guard reads the visibility through GraphQL at once.
// gh is never given it (ghChildKeeps names it not).
const ghDeadlineVar = "LEGION_GH_DEADLINE"

// ghCallerDeadline is the deadline env's LEGION_GH_DEADLINE names, and whether it names one: a
// base-10 count of Unix milliseconds, digits alone, later than now. An unset or empty variable, one
// that does not parse, and a time not later than now name none, and the call runs as one whose
// caller gave no deadline: a stale or mistaken value never moves a call onto another path.
func ghCallerDeadline(env map[string]string, now time.Time) (time.Time, bool) {
	value := env[ghDeadlineVar]
	if value == "" || strings.Trim(value, "0123456789") != "" {
		return time.Time{}, false
	}
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	deadline := time.UnixMilli(ms)
	if !deadline.After(now) {
		return time.Time{}, false
	}
	return deadline, true
}

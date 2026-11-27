package procgroup

import "sync/atomic"

// ControlLateWaits counts the runs ControlLateWait is asked about. CONTROL ONLY (r63 latewait),
// never merged.
var ControlLateWaits atomic.Int64

// ControlLateWait is true for the first run of each process and every second one after it: such a
// run waits 1.5 s between its command's start and its Wait. CONTROL ONLY (r63 latewait), never
// merged.
func ControlLateWait() bool { return ControlLateWaits.Add(1)%2 == 1 }

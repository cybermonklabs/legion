package daemon

import "sync/atomic"

// plantedGateStarts counts pluginGate.run's starts (CONTROL ONLY, never merged): the first, and
// every second one after it, fails.
var plantedGateStarts atomic.Int64

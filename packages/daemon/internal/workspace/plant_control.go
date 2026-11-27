package workspace

// PLANT (control commit, never for merge): the reap-delay faults of the wsbudgets twin rows of
// cybermonklabs/legion#5206. A plant is armed by a file under /work/plant, not an environment
// variable, because this package's TestMain guards the process's environment (fixtureenv.Guard).

import (
	"os"
	"strings"
	"sync"
	"time"
)

// plantReapOnce holds, per plant name, whether the process has already fired it.
var plantReapOnce sync.Map

// plantReapDelayOnce is the duration the file /work/plant/name holds, on the first call of the
// process for that name alone, and zero on every later call or when the file is absent. A value
// that is no duration panics rather than leave the plant silently unarmed.
func plantReapDelayOnce(name string) time.Duration {
	data, err := os.ReadFile("/work/plant/" + name)
	if err != nil {
		return 0
	}
	value := strings.TrimSpace(string(data))
	delay, err := time.ParseDuration(value)
	if err != nil {
		panic("/work/plant/" + name + " = " + value + ": " + err.Error())
	}
	if _, fired := plantReapOnce.LoadOrStore(name, true); fired {
		return 0
	}
	return delay
}

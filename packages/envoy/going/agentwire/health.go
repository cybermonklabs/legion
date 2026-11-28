package agentwire

import "runtime/metrics"

// The runtime/metrics samples RuntimeHealth reads, in Health's order.
var healthSamples = [...]string{
	"/sched/goroutines:goroutines",
	"/gc/heap/live:bytes",
	"/memory/classes/total:bytes",
	"/gc/gomemlimit:bytes",
}

// RuntimeHealth is this process's Go runtime figures as Health holds them: its goroutines, its
// live heap as of the last collection, all the memory the runtime has mapped, and its memory limit
// (GOMEMLIMIT, math.MaxInt64 where none is set). The agent sends its own with each renew, and the
// server reads its own the same way, so the two processes' figures mean the same.
func RuntimeHealth() Health {
	samples := make([]metrics.Sample, len(healthSamples))
	for index, name := range healthSamples {
		samples[index].Name = name
	}
	metrics.Read(samples)
	value := func(index int) uint64 {
		if samples[index].Value.Kind() != metrics.KindUint64 {
			return 0
		}
		return samples[index].Value.Uint64()
	}
	return Health{
		Goroutines:       int(value(0)),
		HeapLiveBytes:    value(1),
		TotalBytes:       value(2),
		MemoryLimitBytes: value(3),
	}
}

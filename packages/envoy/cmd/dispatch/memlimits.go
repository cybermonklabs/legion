package main

import (
	"errors"
	"fmt"
	"math"

	"github.com/KimMachineGun/automemlimit/memlimit"
)

// agentMemory is the memory and the slots of the command bar's agent process, and the server's own
// memory limit beside it, for one container (agentLimits).
type agentMemory struct {
	// Ceiling is the resident memory above which the server kills the agent (VmRSS, read every
	// agentMemoryEvery).
	Ceiling int64
	// AgentLimit is the agent's GOMEMLIMIT, 75% of the ceiling, so its collector works hard well
	// before the kill.
	AgentLimit int64
	// ServerLimit is the server's own Go memory limit, 90% of what the container's limit leaves
	// after the agent's ceiling; 0 for a container with no limit, where the server sets none.
	ServerLimit int64
	// Slots is how many runs the agent works at once.
	Slots int
}

// The design values of section 5 of the background-runs design.
const (
	// agentCeilingMost is the agent's ceiling in a container of 24 GiB or more, or with no limit.
	agentCeilingMost = 4 << 30
	// agentLimitMost is the agent's GOMEMLIMIT with that ceiling, 75% of it.
	agentLimitMost = 3 << 30
	// agentSlots is how many runs the agent works at once. It is the design's value: the load test
	// of t19 (8 Deep runs at their limits) measures one run's peak, and the slots are then set to
	// the agent's claim gate, 70% of its GOMEMLIMIT, less its base, divided by that peak, 8 at the
	// most.
	agentSlots = 8
)

// agentLimits is the agent's memory and slots for a container whose memory limit is container
// bytes, 0 for none: the ceiling is the smaller of 4 GiB and a sixth of the container, the agent's
// GOMEMLIMIT 75% of the ceiling, and the server's limit 90% of what the container leaves after the
// ceiling. So 24 GiB gives 4, 3 and 18 GiB, 8 GiB gives 1.33, 1 and 6 GiB, and no limit gives 4
// GiB, 3 GiB and no server limit.
func agentLimits(container int64) agentMemory {
	limits := agentMemory{Ceiling: agentCeilingMost, AgentLimit: agentLimitMost, Slots: agentSlots}
	if container <= 0 {
		return limits
	}
	// 75% of a sixth is an eighth, which keeps the agent's limit whole where the sixth is not.
	limits.Ceiling = min(agentCeilingMost, container/6)
	limits.AgentLimit = min(agentLimitMost, container/8)
	limits.ServerLimit = (container - limits.Ceiling) * 9 / 10
	return limits
}

// containerMemoryLimit is the memory limit of the container the process runs in, 0 where there is
// none, read by automemlimit's reader (memlimit.FromCgroup): memory.max under control group v2 and
// memory.limit_in_bytes under v1, the smallest limit of the process's group and the groups above
// it. No limit, no group and no control groups on the system each read as 0.
func containerMemoryLimit() (int64, error) {
	return readContainerMemoryLimit(memlimit.FromCgroup)
}

// readContainerMemoryLimit is the limit provider reads, as containerMemoryLimit answers it.
func readContainerMemoryLimit(provider memlimit.Provider) (int64, error) {
	limit, err := provider()
	switch {
	case errors.Is(err, memlimit.ErrNoLimit), errors.Is(err, memlimit.ErrNoCgroup), errors.Is(err, memlimit.ErrCgroupsNotSupported):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read the container's memory limit: %w", err)
	case limit == 0 || limit > math.MaxInt64:
		return 0, nil
	}
	return int64(limit), nil
}

package docs

import (
	"math"
	"strconv"
	"strings"
)

// memoryLimit is the memory this process may use, as far as anything says (processMemoryLimit),
// and what said it, one of the memoryLimit* sources. bytes is math.MaxInt64 when nothing does,
// the Go runtime's own figure for no limit (debug.SetMemoryLimit), and source is then
// memoryLimitNone. The service's two memory budgets follow it (readBudgetFor, roomBudgetFor).
type memoryLimit struct {
	bytes  int64
	source string
}

// The sources of a memoryLimit, as the service's boot line names them.
const (
	memoryLimitGo       = "GOMEMLIMIT"
	memoryLimitCgroupV2 = "cgroup v2"
	memoryLimitCgroupV1 = "cgroup v1"
	memoryLimitNone     = "none"
)

// cgroupV2MemoryMax and cgroupV1MemoryLimit are where a container's memory limit is read: cgroup
// v2's memory.max, which holds the literal "max" for no limit, and cgroup v1's
// memory.limit_in_bytes.
const (
	cgroupV2MemoryMax   = "/sys/fs/cgroup/memory.max"
	cgroupV1MemoryLimit = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
)

// cgroupV1NoLimit is the least cgroup v1 limit taken for none. cgroup v1 writes no limit as the
// largest count of whole pages it can hold, math.MaxInt64 rounded down to a page
// (9223372036854771712 with 4 KiB pages), and no machine has the 4 EiB this is.
const cgroupV1NoLimit = 1 << 62

// processMemoryLimit is the memory this process may use: the Go memory limit when one is set
// (goLimit, debug.SetMemoryLimit(-1), which is math.MaxInt64 when GOMEMLIMIT is not set), since
// the runtime holds the heap to it; else the limit of the container's cgroup, read through
// readFile from cgroup v2's memory.max, and from cgroup v1's memory.limit_in_bytes on a box with
// no memory.max; else none. A file that is absent, or holds no positive count of bytes - v2's
// "max", v1's page-rounded math.MaxInt64 - says nothing. Production runs with no GOMEMLIMIT in a
// container of 20 GiB (agent-infra dispatch/Pulumi.prod.yaml, appMemory), which is the cgroup's
// limit; the Go runtime does not read that limit itself, so without this the budgets would take
// the process for one with no limit at all.
func processMemoryLimit(goLimit int64, readFile func(name string) ([]byte, error)) memoryLimit {
	if goLimit != math.MaxInt64 {
		return memoryLimit{bytes: goLimit, source: memoryLimitGo}
	}
	if text, err := readFile(cgroupV2MemoryMax); err == nil {
		if limit, ok := cgroupMemoryLimit(text); ok {
			return memoryLimit{bytes: limit, source: memoryLimitCgroupV2}
		}
		return memoryLimit{bytes: math.MaxInt64, source: memoryLimitNone}
	}
	if text, err := readFile(cgroupV1MemoryLimit); err == nil {
		if limit, ok := cgroupMemoryLimit(text); ok && limit < cgroupV1NoLimit {
			return memoryLimit{bytes: limit, source: memoryLimitCgroupV1}
		}
	}
	return memoryLimit{bytes: math.MaxInt64, source: memoryLimitNone}
}

// cgroupMemoryLimit reads a cgroup memory limit file's text as a count of bytes; ok is false for
// anything but a positive count, v2's "max" among them.
func cgroupMemoryLimit(text []byte) (int64, bool) {
	limit, err := strconv.ParseInt(strings.TrimSpace(string(text)), 10, 64)
	return limit, err == nil && limit > 0
}

//go:build !linux

package main

import "errors"

// schedIdle is the number Linux gives its SCHED_IDLE scheduling policy (sched(7)). No other
// kernel has the policy, so threadPolicy and setThreadIdle refuse there. Off Linux neither the
// agent's priority setting nor its memory ceiling runs, since both read Linux's /proc:
// lowerPriority finds no thread in /proc/<pid>/task to lower, to SCHED_IDLE or to nice agentNice,
// so the agent runs at the server's priority; and residentMemory reads no VmRSS from
// /proc/<pid>/status, so the supervisor never kills the agent for its memory.
const schedIdle = 5

// threadPolicy refuses: only Linux answers a thread's scheduling policy.
func threadPolicy(int) (int, error) {
	return 0, errors.ErrUnsupported
}

// setThreadIdle refuses: only Linux has SCHED_IDLE.
func setThreadIdle(int) error {
	return errors.ErrUnsupported
}

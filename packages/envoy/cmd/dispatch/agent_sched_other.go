//go:build !linux

package main

import "errors"

// schedIdle is the number Linux gives its SCHED_IDLE scheduling policy (sched(7)). No other
// kernel has the policy, so threadPolicy and setThreadIdle refuse there, and lowerPriority sets
// the agent's threads to nice agentNice instead.
const schedIdle = 5

// threadPolicy refuses: only Linux answers a thread's scheduling policy.
func threadPolicy(int) (int, error) {
	return 0, errors.ErrUnsupported
}

// setThreadIdle refuses: only Linux has SCHED_IDLE.
func setThreadIdle(int) error {
	return errors.ErrUnsupported
}

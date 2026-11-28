package main

import "golang.org/x/sys/unix"

// schedIdle is Linux's SCHED_IDLE scheduling policy (sched(7)), the one lowerPriority puts the
// agent's threads under.
const schedIdle = unix.SCHED_IDLE

// threadPolicy is thread id's scheduling policy, as sched_getattr(2) answers it.
func threadPolicy(id int) (int, error) {
	attr, err := unix.SchedGetAttr(id, 0)
	if err != nil {
		return 0, err
	}
	return int(attr.Policy), nil
}

// setThreadIdle puts thread id under SCHED_IDLE.
func setThreadIdle(id int) error {
	return unix.SchedSetAttr(id, &unix.SchedAttr{Policy: schedIdle}, 0)
}

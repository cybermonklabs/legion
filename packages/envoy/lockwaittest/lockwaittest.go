// Package lockwaittest spells the read a Go test of this repository makes to learn that a
// PostgreSQL backend waits on a lock: Waiting, a condition on a row of pg_stat_activity, and
// WaitingOn, the same for one kind of lock. docs/testing.md, "A test reads a lock wait from
// pg_locks", states the rule; this documentation gives its reason and what was measured.
//
// # Why pg_locks
//
// A backend waits on a lock for as long as pg_locks holds an ungranted request of its pid. The
// request is queued when the backend asks and stays until the lock is granted or the wait is
// cancelled. pg_stat_activity's wait_event_type is not that fact. It reads Lock only while the
// backend sleeps on its latch, and a waiting backend leaves that sleep each time it is woken, by
// its deadlock check at deadlock_timeout or by a signal, so a read at that moment sees no wait.
// pg_blocking_pids reads the same queue as pg_locks.
//
// What was measured: a probe sampled one waiting backend's wait event and its pg_locks request
// together, beside the whole of internal/daemon under -race, with deadlock_timeout at its default
// of 1 s. In 2 of 10 waits it read no wait event at 1.007, 1.009 and 1.011 s into the wait, with
// the request still queued. TestReadyzIsFalseUntilTheDaemonServes read the wait event once and
// counted 0 of the daemon's backends waiting while its one connection waited (CI run
// 45206462019); how far into its wait that read fell was not measured.
//
// Any wait event reads nothing while a signal wakes its backend, a lock's or not: an idle holder's
// Client/ClientRead too. A test that decides on one sample of a wait event reads it again, within
// a bound, until it holds.
//
// # The check
//
// Envoy's internal/textcheck holds the three Go modules (Envoy, Envoy's infra and the daemon) to
// this. TestEveryTestReadsALockWaitThroughLockwaittest's doc comment, in lock_wait_reads_test.go,
// lists the forms it names and the forms it does not see.
package lockwaittest

// Waiting is the condition, on a row of pg_stat_activity, that its backend waits on a lock:
// pg_locks holds an ungranted request of its pid.
const Waiting = "pid in (select pid from pg_locks where not granted)"

// WaitingOn is Waiting for a lock of one kind. locktype is a value of pg_locks' locktype column,
// such as advisory or transactionid.
func WaitingOn(locktype string) string {
	return "pid in (select pid from pg_locks where not granted and locktype = '" + locktype + "')"
}

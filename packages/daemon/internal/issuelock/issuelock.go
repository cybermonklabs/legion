// Package issuelock is the one coordination, inside the daemon's process, between the writers of
// an issue's Dispatch status that must not interleave: the API's sign_off and close_root, which
// write their status in Dispatch before they record it, the outbox's status rows, and the status
// repair. One daemon process serves a project (its Deployment runs one replica with the Recreate
// strategy, and `legion start` refuses a second daemon of the project on one machine), so a lock
// of the process holds every writer of the project's issues.
//
// The lock is taken before the fact lock every fact applies under (intake.SerializeFacts) and
// never while that lock is held: an acceptance holds its issue's lock while its fact waits for the
// fact lock, so a fact handler that waited for an issue's lock could wait on it for ever. A fact
// handler asks Accepting instead, which never waits.
package issuelock

import "sync"

// Locks is one mutex for each issue a writer holds or waits on, made at its first use and dropped
// once no writer holds it or waits on it, so the map keeps only the issues in use. The zero value
// is ready, and a nil *Locks serializes nothing: a test's outbox or engine built without one.
type Locks struct {
	mu    sync.Mutex
	locks map[string]*lock
}

// lock is one issue's mutex, the number of writers that hold it or wait on it, and whether the
// writer holding it is an acceptance (Accept).
type lock struct {
	sync.Mutex
	users     int
	accepting bool
}

// Lock waits for issue's mutex and answers its release, which the caller must call once.
func (l *Locks) Lock(issue string) (unlock func()) {
	return l.take(issue, false)
}

// Accept is Lock for an acceptance of issue, sign_off's or close_root's: from the moment it holds
// the mutex until its release, Accepting answers true for issue.
func (l *Locks) Accept(issue string) (unlock func()) {
	return l.take(issue, true)
}

// Accepting says whether an acceptance of issue holds its mutex now. It never waits.
func (l *Locks) Accepting(issue string) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	held, ok := l.locks[issue]
	return ok && held.accepting
}

func (l *Locks) take(issue string, accepting bool) func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*lock{}
	}
	held, ok := l.locks[issue]
	if !ok {
		held = &lock{}
		l.locks[issue] = held
	}
	held.users++
	l.mu.Unlock()
	held.Lock()
	l.mu.Lock()
	held.accepting = accepting
	l.mu.Unlock()
	return func() {
		l.mu.Lock()
		held.accepting = false
		if held.users--; held.users == 0 {
			delete(l.locks, issue)
		}
		l.mu.Unlock()
		held.Unlock()
	}
}
